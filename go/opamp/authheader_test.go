// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opamp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// authServer is an OpAMP server that records each request's or upgrade's headers, and closes each
// WebSocket connection after its first message, so the client connects again.
type authServer struct {
	opamp propServer
	mu    sync.Mutex
	seen  []http.Header
}

func (s *authServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.seen = append(s.seen, r.Header.Clone())
	s.mu.Unlock()
	if !websocket.IsWebSocketUpgrade(r) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(s.opamp.reply(body))
		return
	}
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	if _, data, err := conn.ReadMessage(); err == nil && len(data) > 0 {
		// The WebSocket framing's header byte (0), then the message.
		_ = conn.WriteMessage(websocket.BinaryMessage, append([]byte{0}, s.opamp.reply(data[1:])...))
	}
}

// authorizations is the Authorization header of every request or upgrade so far.
func (s *authServer) authorizations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, h := range s.seen {
		out = append(out, h.Get("Authorization"))
	}
	return out
}

// AuthHeader's headers go with every request and upgrade, over the configured header of the same
// name and with the other configured ones, in both modes over both transports; a new token goes
// with the next request or connection. The client hands them to the core (SetAuthHeaders), which
// sends them as it sends any header.
func TestAuthHeaderGoesWithEveryAttempt(t *testing.T) {
	for _, mode := range []Mode{ModeGo, ModeRust} {
		for _, ws := range []bool{false, true} {
			name := string(mode) + map[bool]string{false: "/http", true: "/ws"}[ws]
			t.Run(name, func(t *testing.T) {
				srv := &authServer{}
				ts := httptest.NewServer(srv)
				defer ts.Close()
				var token atomic.Value
				token.Store("Bearer 1")
				cfg := Config{
					Mode:              mode,
					Endpoint:          endpoint(ts, ws),
					Header:            http.Header{"Authorization": {"enrollment"}, "X-Kept": {"yes"}},
					HeartbeatInterval: 50 * time.Millisecond,
					AuthHeader:        func() http.Header { return http.Header{"Authorization": {token.Load().(string)}} },
					authEvery:         10 * time.Millisecond,
				}
				c, err := Start(context.Background(), cfg, Status{}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Stop(context.Background()) }()
				waitFor(t, "the first token", func() bool { return slices.Contains(srv.authorizations(), "Bearer 1") })
				token.Store("Bearer 2")
				waitFor(t, "the new token", func() bool { return slices.Contains(srv.authorizations(), "Bearer 2") })
				srv.mu.Lock()
				defer srv.mu.Unlock()
				for _, h := range srv.seen {
					if len(h.Values("Authorization")) != 1 || h.Get("Authorization") == "enrollment" || h.Get("X-Kept") != "yes" {
						t.Fatalf("the server got %v: want the AuthHeader's Authorization alone, and X-Kept", h)
					}
				}
			})
		}
	}
}

// An AuthHeader that hands back the same map each time, changed in place, is still heard: what the
// client compares with is a copy, not the map itself.
func TestAuthHeaderChangedInPlaceIsHeard(t *testing.T) {
	srv := &authServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	var token atomic.Value
	token.Store("Bearer 1")
	shared := http.Header{}
	cfg := Config{
		Mode:              ModeGo,
		Endpoint:          endpoint(ts, false),
		HeartbeatInterval: 50 * time.Millisecond,
		AuthHeader: func() http.Header {
			shared.Set("Authorization", token.Load().(string))
			return shared
		},
		authEvery: 10 * time.Millisecond,
	}
	c, err := Start(context.Background(), cfg, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Stop(context.Background()) }()
	waitFor(t, "the first token", func() bool { return slices.Contains(srv.authorizations(), "Bearer 1") })
	token.Store("Bearer 2")
	waitFor(t, "the token changed in place", func() bool { return slices.Contains(srv.authorizations(), "Bearer 2") })
}

// waitFor waits up to 20 s for ok.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 20 s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
