// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

// Lifecycle churn: an agent that reconfigures its client stops it and starts another, again and
// again, in one process. Whatever a start leaves behind when its client stops (a core instance, a
// goroutine, a socket, a timer) adds up there. These tests start and stop clients many times, at
// every point a stop can come (at once, mid-handshake, with the connection up, with the callback
// running, just after a remote config, with a state file, after the callback panicked), over HTTP
// and WebSocket, in both modes, against a server on loopback, and check that the process's
// goroutines, open file descriptors and heap come back to what they were after a warm-up.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// churnServer answers every message with a remote config, over HTTP or WebSocket. On /hold it
// holds what it gets (a WebSocket upgrade, or a POST that isn't the goodbye) until the client
// goes, telling held: a stop then comes mid-handshake or mid-request.
type churnServer struct {
	held chan struct{}
}

func (s *churnServer) reply(body []byte) ([]byte, *protobufs.AgentToServer) {
	m := &protobufs.AgentToServer{}
	if proto.Unmarshal(body, m) != nil {
		return nil, nil
	}
	out, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: m.InstanceUid,
		RemoteConfig: &protobufs.AgentRemoteConfig{ConfigHash: []byte("churn"),
			Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte("churn")}}}}})
	return out, m
}

// hold waits until the client has gone (or long enough that the test has failed anyway).
func (s *churnServer) hold(r *http.Request) {
	select {
	case s.held <- struct{}{}:
	default:
	}
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

func (s *churnServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	holding := r.URL.Path == "/hold"
	if websocket.IsWebSocketUpgrade(r) {
		if holding {
			s.hold(r)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.BinaryMessage || len(data) == 0 {
				continue
			}
			out, _ := s.reply(data[1:])
			if err := conn.WriteMessage(websocket.BinaryMessage, append([]byte{0}, out...)); err != nil {
				return
			}
		}
	}
	body, _ := io.ReadAll(r.Body)
	out, m := s.reply(body)
	if holding && m != nil && m.AgentDisconnect == nil {
		s.hold(r)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

// churnUsage is what the process holds that a client could leave behind.
type churnUsage struct {
	goroutines int
	fds        int // -1 where /proc/self/fd isn't there
	heap       uint64
}

func (u churnUsage) String() string {
	return fmt.Sprintf("%d goroutines, %d fds, %d KiB heap", u.goroutines, u.fds, u.heap>>10)
}

func openFDs() int {
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(fds)
}

func churnMeasure() churnUsage {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return churnUsage{goroutines: runtime.NumGoroutine(), fds: openFDs(), heap: ms.HeapAlloc}
}

// churnSettle measures until goroutines and fds are within slack of base (what a client left
// finishing ends by itself: a save, a connection's goroutines after its close), or 5 s have gone.
func churnSettle(base churnUsage, slack int) churnUsage {
	var u churnUsage
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		u = churnMeasure()
		if (u.goroutines <= base.goroutines+slack && u.fds <= base.fds+slack) || time.Now().After(deadline) {
			return u
		}
	}
}

// churnStop is where a run's client is stopped.
type churnStop int

const (
	stopAtOnce      churnStop = iota // right after Start
	stopHeld                         // the WebSocket handshake (or the request) under way
	stopConnected                    // a config has come: the connection is up
	stopInCallback                   // the callback is running when Stop is called
	stopAfterConfig                  // from the callback that took the config, Stop at once after
	stopStateFile                    // with a state file, once the config is saved
	stopPanicked                     // the callback panicked: the client stopped itself
	churnStops
)

func (s churnStop) String() string {
	return [...]string{"at once", "held", "connected", "in callback", "after config", "state file", "panicked"}[s]
}

// churnRun starts one client, stops it at where, and checks it stopped as it should.
func churnRun(t *testing.T, ts *httptest.Server, srv *churnServer, dir string, mode Mode, ws bool, where churnStop) {
	t.Helper()
	ep := ts.URL
	if ws {
		ep = "ws" + strings.TrimPrefix(ep, "http")
	}
	if where == stopHeld {
		ep += "/hold"
	} else {
		ep += "/v1/opamp"
	}
	cfg := Config{Mode: mode, Endpoint: ep, Proxy: Proxy{Direct: true}, HeartbeatInterval: time.Hour,
		StopTimeout: 2 * time.Second, Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig)}
	if where == stopHeld {
		select { // a hold left over from the run before (a request the core tried again)
		case <-srv.held:
		default:
		}
		// Over HTTP the goodbye waits for the request held: the stop gives up on both at
		// StopTimeout.
		cfg.StopTimeout = 50 * time.Millisecond
	}
	if where == stopStateFile {
		// The same file each time, as an agent that restarts its client keeps it.
		cfg.StateFile = filepath.Join(dir, "state")
	}
	got := make(chan struct{}, 1)  // a config came
	inside := make(chan struct{})  // the callback is running (stopInCallback)
	release := make(chan struct{}) // and may return
	stopped := make(chan struct{}) // Stopped was told
	var once sync.Once
	c, err := Start(context.Background(), cfg, Status{}, func(c *Client, n Notification) {
		switch n.Kind {
		case Stopped:
			close(stopped)
		case RemoteConfig:
			c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{LastRemoteConfigHash: n.RemoteConfig.ConfigHash,
				Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED})
			switch where {
			case stopInCallback:
				once.Do(func() { close(inside); <-release })
			case stopAfterConfig:
				_ = c.Stop(context.Background())
			case stopPanicked:
				panic("churn")
			}
			select {
			case got <- struct{}{}:
			default:
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	wait := func(what string, ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatalf("%v: %s never happened", where, what)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch where {
	case stopHeld:
		wait("the held request", srv.held)
	case stopConnected, stopAfterConfig, stopStateFile:
		wait("a remote config", got)
	case stopPanicked:
		wait("the stop the panic made", stopped)
	case stopInCallback:
		wait("the callback", inside)
		go func() { time.Sleep(time.Millisecond); close(release) }()
	}
	err = c.Stop(ctx)
	switch {
	case where == stopPanicked:
		if err == nil || !strings.Contains(err.Error(), "churn") {
			t.Fatalf("%v: Stop returned %v, not the panic", where, err)
		}
	case errors.Is(err, context.DeadlineExceeded):
		t.Fatalf("%v: Stop never returned", where)
	case err != nil:
		t.Fatalf("%v: Stop returned %v", where, err)
	}
	select {
	case <-stopped:
	default:
		t.Fatalf("%v: Stop returned before Stopped was told", where)
	}
}

// Starting and stopping clients again and again leaves nothing behind: after a warm-up, rounds of
// a client stopped at every point, over HTTP and WebSocket, leave the process with no more
// goroutines, open file descriptors or heap than before them (a client's core instance is MiBs of
// heap; its connections are a goroutine or two and a descriptor each).
func TestChurnLeavesNothingBehind(t *testing.T) {
	for _, mode := range []Mode{ModeCore, ModePlatform} {
		t.Run(modeName(mode), func(t *testing.T) { churn(t, mode) })
	}
}

func churn(t *testing.T, mode Mode) {
	const warmup, rounds = 2, 30
	srv := &churnServer{held: make(chan struct{}, 1)}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := t.TempDir()
	round := func() {
		for where := range churnStops {
			for _, ws := range []bool{false, true} {
				churnRun(t, ts, srv, dir, mode, ws, where)
			}
		}
	}
	for range warmup {
		round()
	}
	ts.CloseClientConnections() // the server's side of connections already gone at the client
	base := churnSettle(churnMeasure(), 0)
	began := time.Now()
	for range rounds {
		round()
	}
	took := time.Since(began)
	ts.CloseClientConnections()
	after := churnSettle(base, 2)
	runs := rounds * int(churnStops) * 2
	t.Logf("%d clients in %v: before %v; after %v", runs, took.Round(time.Millisecond), base, after)
	// A client left behind holds a goroutine, a core instance and, connected, a socket: runs of
	// them would be far over these.
	if after.goroutines > base.goroutines+2 {
		t.Errorf("goroutines grew from %d to %d over %d clients:\n%s", base.goroutines, after.goroutines, runs, allStacks())
	}
	if base.fds >= 0 && after.fds > base.fds+2 {
		t.Errorf("open fds grew from %d to %d over %d clients", base.fds, after.fds, runs)
	}
	if after.heap > base.heap+32<<20 {
		t.Errorf("heap grew from %d KiB to %d KiB over %d clients", base.heap>>10, after.heap>>10, runs)
	}
}

// Clients on state files no longer used leave nothing behind for them: after clients, each on a
// state file of its own, took a config and stopped, no file's work is counted any more.
func TestChurnStateFilesLeaveNoWork(t *testing.T) {
	for _, mode := range []Mode{ModeCore, ModePlatform} {
		t.Run(modeName(mode), func(t *testing.T) {
			srv := &churnServer{held: make(chan struct{}, 1)}
			ts := httptest.NewServer(srv)
			defer ts.Close()
			const clients = 20
			for i := range clients {
				churnRun(t, ts, srv, t.TempDir(), mode, i%2 == 1, stopStateFile)
			}
			counted := func() int {
				worksMu.Lock()
				defer worksMu.Unlock()
				return len(works)
			}
			deadline := time.Now().Add(10 * time.Second)
			for counted() > 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if n := counted(); n > 0 {
				t.Fatalf("%d state files still counted after %d clients stopped", n, clients)
			}
		})
	}
}

func allStacks() string {
	buf := make([]byte, 1<<20)
	return string(buf[:runtime.Stack(buf, true)])
}
