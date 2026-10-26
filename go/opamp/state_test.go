// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

func TestSaveStateReplacesPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.bin")
	for _, data := range []string{"one", "two"} {
		if err := saveState(osFS{}, path, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "two" {
		t.Fatalf("read %q, %v", got, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left: %v", err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", fi.Mode().Perm())
		}
	}
}

func TestALeftoverTemporaryFileIsReplacedNotReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.bin")
	if err := os.WriteFile(path+".tmp", []byte("stale, readable by all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveState(osFS{}, path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("read %q, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", fi.Mode().Perm())
		}
	}
}

func TestStoreEffectAnswers(t *testing.T) {
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))
	x.setStateFile(filepath.Join(t.TempDir(), "state.bin"))
	x.do(&corepb.Effect{Kind: &corepb.Effect_Store{Store: &corepb.Store{Id: 7, Data: []byte("x")}}})
	if r := nextReport(t, x); r.in.GetStored().GetId() != 7 || !r.last {
		t.Fatalf("got %v", r.in)
	}
	x.setStateFile(filepath.Join(t.TempDir(), "missing", "state.bin"))
	x.do(&corepb.Effect{Kind: &corepb.Effect_Store{Store: &corepb.Store{Id: 8, Data: []byte("x")}}})
	if r := nextReport(t, x); r.in.GetStoreFailed().GetId() != 8 || r.in.GetStoreFailed().GetDetail() == "" {
		t.Fatalf("got %v", r.in)
	}
	x.setStateFile("")
	if r := x.do(&corepb.Effect{Kind: &corepb.Effect_Store{Store: &corepb.Store{Id: 9}}}); r == nil || r.in.GetStoreFailed() == nil {
		t.Fatalf("got %v", r)
	}
}

func TestStartReadsTheStateFile(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be: unreadable, so Start says so.
	if _, err := Start(t.Context(), Config{Mode: ModePlatform, Endpoint: "http://x", StateFile: dir}, Status{}, nil); err == nil {
		t.Fatal("started with an unreadable state file")
	}
	// No file yet is fine.
	c, err := Start(t.Context(), Config{Mode: ModePlatform, Endpoint: "http://127.0.0.1:1", StateFile: filepath.Join(dir, "s")}, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Stop(t.Context())
}

func TestAnUnknownEffectIsAnError(t *testing.T) {
	if err := known(&corepb.Effect{}); err != errUnknownEffect {
		t.Fatalf("got %v", err)
	}
	if err := known(&corepb.Effect{Kind: &corepb.Effect_WsClose{WsClose: &corepb.Id{}}}); err != nil {
		t.Fatalf("got %v", err)
	}
}

// prefixSealer marks what it seals, so a test can see a save went through it.
type prefixSealer struct{ openErr error }

func (prefixSealer) Seal(b []byte) ([]byte, error) { return append([]byte("sealed:"), b...), nil }

func (s prefixSealer) Open(b []byte) ([]byte, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	return bytes.TrimPrefix(b, []byte("sealed:")), nil
}

type failingSealer struct{ prefixSealer }

func (failingSealer) Seal([]byte) ([]byte, error) { return nil, errors.New("no key") }

// With a sealer, what a save writes is the sealed state; one that can't be sealed fails the save
// (the core keeps it to try again) and writes nothing.
func TestStoreSeals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.bin")
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))
	x.setStateFile(path)
	x.sealer = prefixSealer{}
	x.do(&corepb.Effect{Kind: &corepb.Effect_Store{Store: &corepb.Store{Id: 7, Data: []byte("x")}}})
	if r := nextReport(t, x); r.in.GetStored().GetId() != 7 {
		t.Fatalf("got %v", r.in)
	}
	if got, _ := os.ReadFile(path); string(got) != "sealed:x" {
		t.Fatalf("the file holds %q", got)
	}
	x.sealer = failingSealer{}
	x.do(&corepb.Effect{Kind: &corepb.Effect_Store{Store: &corepb.Store{Id: 8, Data: []byte("y")}}})
	if r := nextReport(t, x); r.in.GetStoreFailed().GetId() != 8 || !strings.Contains(r.in.GetStoreFailed().GetDetail(), "no key") {
		t.Fatalf("got %v", r.in)
	}
	if got, _ := os.ReadFile(path); string(got) != "sealed:x" {
		t.Fatalf("a failed seal changed the file: %q", got)
	}
}

// A state file the sealer won't open stops Start, rather than starting without its credentials.
func TestStartRefusesAStateItCantOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(path, []byte("sealed:junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Start(t.Context(), Config{Mode: ModePlatform, Endpoint: "http://127.0.0.1:1", StateFile: path,
		StateSealer: prefixSealer{openErr: errors.New("wrong key")}}, Status{}, nil)
	if err == nil || !strings.Contains(err.Error(), "wrong key") {
		t.Fatalf("Start = %v", err)
	}
}

// A state a newer core saved (format 2): the client is told once, starts from its configuration,
// and saves nothing, so the newer release finds its identity again. The server's new UID, which
// would be saved, isn't.
func TestAStateFromANewerVersionIsKeptNotSavedOver(t *testing.T) {
	newer := []byte{0x08, 0x02}
	state := filepath.Join(t.TempDir(), "state.bin")
	if err := os.WriteFile(state, newer, 0o600); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := proto.Marshal(&protobufs.ServerToAgent{AgentIdentification: &protobufs.AgentIdentification{
			NewInstanceUid: bytes.Repeat([]byte{7}, 16)}})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(b)
	}))
	defer ts.Close()
	told := make(chan Notification, 16)
	c, err := Start(t.Context(), Config{Mode: ModePlatform, Endpoint: ts.URL, HeartbeatInterval: time.Hour, StateFile: state},
		Status{}, func(_ *Client, n Notification) { told <- n })
	if err != nil {
		t.Fatal(err)
	}
	var newerTold int
	for uid := false; !uid; {
		select {
		case n := <-told:
			switch n.Kind {
			case StateFromNewerVersion:
				if n.StateVersion != 2 {
					t.Fatalf("told format %d, want 2", n.StateVersion)
				}
				newerTold++
			case InstanceUIDChanged:
				uid = true
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the server's UID never came")
		}
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if newerTold != 1 {
		t.Fatalf("told %d times, want once", newerTold)
	}
	if got, _ := os.ReadFile(state); !bytes.Equal(got, newer) {
		t.Fatalf("the newer state was saved over: %x", got)
	}
}
