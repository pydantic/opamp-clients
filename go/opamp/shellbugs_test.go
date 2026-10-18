// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

// Regressions for the shell audit (S1, S3, S4, S5): a connection opening at stop, a download
// outliving stop, a slow save, and notifications before Start returns.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// offerServer answers every AgentToServer with what answer returns, and records the messages.
type offerServer struct {
	mu     sync.Mutex
	got    []*protobufs.AgentToServer
	answer func(*protobufs.AgentToServer) *protobufs.ServerToAgent
}

func (s *offerServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m := &protobufs.AgentToServer{}
	if proto.Unmarshal(body, m) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.got = append(s.got, m)
	s.mu.Unlock()
	a := s.answer(m)
	a.InstanceUid = m.InstanceUid
	out, _ := proto.Marshal(a)
	_, _ = w.Write(out)
}

func (s *offerServer) messages() []*protobufs.AgentToServer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*protobufs.AgentToServer(nil), s.got...)
}

// S1: a WebSocket whose upgrade completes after stop is closed at once, and stop doesn't wait
// for it.
func TestAConnectionOpeningAtStopIsClosed(t *testing.T) {
	closed := make(chan struct{})
	var once sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer once.Do(func() { close(closed) })
		time.Sleep(2 * time.Second) // holds the upgrade
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer ts.Close()
	c, err := Start(context.Background(), Config{Mode: ModePlatform, Endpoint: "ws" + strings.TrimPrefix(ts.URL, "http"),
		InstanceUID: [16]byte{0x42}, HeartbeatInterval: time.Hour}, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	stopped := make(chan struct{})
	began := time.Now()
	go func() { _ = c.Stop(context.Background()); close(stopped) }()
	within(t, "stop", stopped)
	t.Logf("stop took %v", time.Since(began))
	within(t, "the connection closed", closed)
	t.Logf("closed after %v", time.Since(began))
}

// S3: a download in flight at stop is aborted, and doesn't touch the next client's file.
func TestADownloadDoesNotOutliveStop(t *testing.T) {
	file := []byte("the package, version 1")
	sum := sha256.Sum256(file)
	held, release, abortedFirst := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	first := true
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		f := first
		first = false
		mu.Unlock()
		if !f {
			_, _ = w.Write(file)
			return
		}
		close(held)
		select {
		case <-release:
			_, _ = w.Write(bytes.Repeat([]byte("OLD"), 100))
		case <-r.Context().Done():
			close(abortedFirst)
		}
	}))
	defer files.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	srv := &offerServer{answer: func(*protobufs.AgentToServer) *protobufs.ServerToAgent {
		return &protobufs.ServerToAgent{PackagesAvailable: &protobufs.PackagesAvailable{
			Packages: map[string]*protobufs.PackageAvailable{"p": {Type: protobufs.PackageType_PackageType_TopLevel,
				Version: "1", Hash: []byte{1}, File: &protobufs.DownloadableFile{DownloadUrl: files.URL + "/pkg", ContentHash: sum[:]}}},
			AllPackagesHash: []byte{9}}}
	}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	cfg := Config{Mode: ModePlatform, Endpoint: ts.URL, InstanceUID: [16]byte{0x42}, HeartbeatInterval: time.Hour,
		StateFile: filepath.Join(t.TempDir(), "state"), AcceptPackages: true}

	a, err := Start(context.Background(), cfg, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	within(t, "the first download", held)
	_ = a.Stop(context.Background())
	select {
	case <-abortedFirst:
	case <-time.After(3 * time.Second):
		t.Error("the download outlived stop")
	}

	paths := make(chan string, 1)
	b, err := Start(context.Background(), cfg, Status{}, func(_ *Client, n Notification) {
		if n.Kind == InstallPackage {
			paths <- n.Path
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background())
	var path string
	select {
	case path = <-paths:
	case <-time.After(10 * time.Second):
		t.Fatal("no InstallPackage")
	}
	close(release) // the first client's download, if it is still there, goes on now
	time.Sleep(300 * time.Millisecond)
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, file) {
		t.Errorf("the file handed over holds %q (%v), want %q", got, err, file)
	}
}

// S4: a slow save holds up neither heartbeats nor stop; the next client on the file waits for it.
func TestASlowSaveHoldsUpNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	started, release := holdSaves(t, path)
	srv := &server{}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	// No UID chosen: the core makes one and saves it at once.
	cfg := Config{Mode: ModePlatform, Endpoint: ts.URL, HeartbeatInterval: 50 * time.Millisecond, StopTimeout: 300 * time.Millisecond, StateFile: path}
	a, err := Start(context.Background(), cfg, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no save")
	}
	if !eventually(2*time.Second, func() bool { srv.mu.Lock(); defer srv.mu.Unlock(); return len(srv.msgs) >= 3 }) {
		t.Error("heartbeats wait for the save")
	}
	began := time.Now()
	stopped := make(chan struct{})
	go func() { _ = a.Stop(context.Background()); close(stopped) }()
	select {
	case <-stopped:
		if took := time.Since(began); took > 2*time.Second {
			t.Errorf("stop took %v", took)
		}
	case <-time.After(2 * time.Second):
		release()
		<-stopped
		t.Fatal("stop waits for the save")
	}

	next := make(chan *Client, 1)
	go func() {
		b, err := Start(context.Background(), cfg, Status{}, nil)
		if err != nil {
			t.Error(err)
		}
		next <- b
	}()
	select {
	case b := <-next:
		_ = b.Stop(context.Background())
		t.Fatal("the next client started while a save was still writing")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	b := <-next
	defer b.Stop(context.Background())
	if b.InstanceUID() != a.InstanceUID() {
		t.Fatalf("the next client didn't start from what was saved: %x, then %x", a.InstanceUID(), b.InstanceUID())
	}
}

// ReadState right after Stop: a save the client left writing lands first, and what it saved is
// what is read, as the next client on the file would start from it.
func TestReadStateWaitsForTheSaveAStoppedClientLeftWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	started, release := holdSaves(t, path)
	ts := httptest.NewServer(&server{})
	defer ts.Close()
	// No UID chosen: the core makes one and saves it at once.
	cfg := Config{Mode: ModePlatform, Endpoint: ts.URL, HeartbeatInterval: time.Hour, StopTimeout: 300 * time.Millisecond, StateFile: path}
	a, err := Start(context.Background(), cfg, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no save")
	}
	_ = a.Stop(context.Background())
	if before, err := os.ReadFile(path); err == nil || len(before) > 0 {
		t.Fatalf("the save landed before it was let go: %x (%v)", before, err)
	}
	type read struct {
		b   []byte
		err error
	}
	got := make(chan read, 1)
	go func() {
		b, err := ReadState(path)
		got <- read{b, err}
	}()
	select {
	case r := <-got:
		t.Fatalf("read %x (%v) while the save was still writing", r.b, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	r := <-got
	if r.err != nil || len(r.b) == 0 {
		t.Fatalf("read %x (%v), want the state saved", r.b, r.err)
	}
	if on, _ := os.ReadFile(path); !bytes.Equal(r.b, on) {
		t.Fatalf("read %x, the file holds %x", r.b, on)
	}
	if b, err := ReadState(filepath.Join(t.TempDir(), "none")); b != nil || err != nil {
		t.Fatalf("no file: %x, %v", b, err)
	}
}

// S5: a notification on the first step (a remote config saved before a restart) comes with the
// client's handle, after Start has returned, and can be answered through it.
func TestTheCallbackGetsTheClient(t *testing.T) {
	hash := []byte{7, 7, 7}
	srv := &offerServer{answer: func(*protobufs.AgentToServer) *protobufs.ServerToAgent {
		return &protobufs.ServerToAgent{RemoteConfig: &protobufs.AgentRemoteConfig{ConfigHash: hash,
			Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte("x")}}}}}
	}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	cfg := Config{Mode: ModePlatform, Endpoint: ts.URL, InstanceUID: [16]byte{0x42}, HeartbeatInterval: time.Hour,
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig), StateFile: filepath.Join(t.TempDir(), "state")}
	got := make(chan struct{}, 1)
	a, err := Start(context.Background(), cfg, Status{}, func(_ *Client, n Notification) {
		if n.Kind == RemoteConfig {
			select {
			case got <- struct{}{}:
			default:
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	within(t, "the remote config", got)
	time.Sleep(200 * time.Millisecond) // saved
	_ = a.Stop(context.Background())

	// Restarted, the server gone: the saved config comes on the first step.
	ts.Close()
	handles := make(chan *Client, 4)
	b, err := Start(context.Background(), cfg, Status{}, func(c *Client, n Notification) {
		if n.Kind == RemoteConfig {
			handles <- c
			c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{LastRemoteConfigHash: n.RemoteConfig.ConfigHash,
				Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED})
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background())
	select {
	case h := <-handles:
		if h != b {
			t.Fatalf("the callback got %p, Start returned %p", h, b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the saved remote config never came")
	}
}

// A panic in the callback ends the client, saying why, rather than the process.
func TestAPanickingCallbackStopsTheClient(t *testing.T) {
	stopped := make(chan error, 1)
	c, err := Start(context.Background(), Config{Mode: ModePlatform, Endpoint: "http://127.0.0.1:1", HeartbeatInterval: time.Hour},
		Status{}, func(_ *Client, n Notification) {
			switch n.Kind {
			case ConnectFailed:
				panic("the application's bug")
			case Stopped:
				stopped <- n.Err
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stopped:
		if err == nil || !strings.Contains(err.Error(), "the application's bug") {
			t.Fatalf("stopped with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no Stopped")
	}
	if err := c.Stop(context.Background()); err == nil {
		t.Fatal("Stop returned no error")
	}
}
