// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// oneConfigServer offers one remote config, and records the statuses it hears about it.
type oneConfigServer struct {
	cfg *protobufs.AgentRemoteConfig

	mu       sync.Mutex
	n        int
	statuses []*protobufs.RemoteConfigStatus
}

func (s *oneConfigServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m := &protobufs.AgentToServer{}
	if err := proto.Unmarshal(body, m); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.n++
	if st := m.GetRemoteConfigStatus(); st != nil {
		s.statuses = append(s.statuses, st)
	}
	s.mu.Unlock()
	out, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: m.InstanceUid, RemoteConfig: s.cfg})
	_, _ = w.Write(out)
}

func (s *oneConfigServer) heard(want protobufs.RemoteConfigStatuses) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.statuses {
		if st.GetStatus() == want && bytes.Equal(st.GetLastRemoteConfigHash(), s.cfg.GetConfigHash()) {
			return true
		}
	}
	return false
}

func (s *oneConfigServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// runOnce runs a client on the state file until the server has heard answer about its config.
func runOnce(t *testing.T, srv *oneConfigServer, url, state string, answer protobufs.RemoteConfigStatuses) [16]byte {
	t.Helper()
	c, err := Start(t.Context(), Config{Mode: ModePlatform, Endpoint: url, StateFile: state, HeartbeatInterval: 50 * time.Millisecond,
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig | protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig)},
		Status{}, func(c *Client, n Notification) {
			if n.Kind == RemoteConfig {
				c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{LastRemoteConfigHash: n.RemoteConfig.GetConfigHash(), Status: answer})
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !srv.heard(answer) {
		if time.Now().After(deadline) {
			t.Fatalf("the server never heard %v", answer)
		}
		time.Sleep(20 * time.Millisecond)
	}
	uid := c.InstanceUID()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	return uid
}

// Open says what a client starts from, without connecting: the config the application last
// applied, and the instance UID, from the state file.
func TestOpenSaysWhatTheClientResumes(t *testing.T) {
	cfg := &protobufs.AgentRemoteConfig{ConfigHash: []byte("one"), Config: &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte("receivers: {}")}}}}
	srv := &oneConfigServer{cfg: cfg}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	state := filepath.Join(t.TempDir(), "state")
	uid := runOnce(t, srv, ts.URL, state, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED)

	before := srv.requests()
	o, err := Open(t.Context(), Config{Mode: ModePlatform, Endpoint: ts.URL, StateFile: state})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if o.InstanceUID() != uid {
		t.Fatalf("instance UID %x, want %x", o.InstanceUID(), uid)
	}
	r := o.Resumed()
	if !proto.Equal(r.AppliedRemoteConfig, cfg) || !proto.Equal(r.RemoteConfig, cfg) {
		t.Fatalf("resumed %v", r)
	}
	if r.RemoteConfigRefused != nil {
		t.Fatalf("refused %v", r.RemoteConfigRefused)
	}
	time.Sleep(100 * time.Millisecond)
	if srv.requests() != before {
		t.Fatal("Open connected")
	}
}

// A config the application refused isn't handed over again; Open says it was refused.
func TestOpenSaysARefusedConfig(t *testing.T) {
	cfg := &protobufs.AgentRemoteConfig{ConfigHash: []byte("bad"), Config: &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte("nope")}}}}
	srv := &oneConfigServer{cfg: cfg}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	state := filepath.Join(t.TempDir(), "state")
	runOnce(t, srv, ts.URL, state, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED)

	o, err := Open(t.Context(), Config{Mode: ModePlatform, Endpoint: ts.URL, StateFile: state})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	r := o.Resumed()
	if r.RemoteConfig != nil || r.AppliedRemoteConfig != nil {
		t.Fatalf("resumed %v", r)
	}
	if !bytes.Equal(r.RemoteConfigRefused.GetLastRemoteConfigHash(), []byte("bad")) ||
		r.RemoteConfigRefused.GetStatus() != protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED {
		t.Fatalf("refused %v", r.RemoteConfigRefused)
	}
}

// An Opened starts once; a closed one doesn't start; without a state file nothing is resumed.
func TestAnOpenedStartsOnce(t *testing.T) {
	o, err := Open(t.Context(), Config{Mode: ModePlatform, Endpoint: "http://127.0.0.1:1", HeartbeatInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if r := o.Resumed(); r != (Resumed{}) {
		t.Fatalf("resumed %v without a state file", r)
	}
	if o.InstanceUID() == ([16]byte{}) {
		t.Fatal("no instance UID")
	}
	c, err := o.Start(Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.InstanceUID() != o.InstanceUID() {
		t.Fatal("started with another instance UID")
	}
	if _, err := o.Start(Status{}, nil); err == nil {
		t.Fatal("started twice")
	}
	o.Close() // does nothing once started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	closed, err := Open(t.Context(), Config{Mode: ModePlatform, Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if _, err := closed.Start(Status{}, nil); err == nil {
		t.Fatal("a closed client started")
	}
}
