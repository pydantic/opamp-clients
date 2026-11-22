// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// offeringServer answers the configured credential with an offer for to, and closes (WebSocket).
func offeringServer(t *testing.T, to string) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	answer := func(m *protobufs.AgentToServer) []byte {
		mu.Lock()
		defer mu.Unlock()
		n++
		b, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: m.InstanceUid, ConnectionSettings: &protobufs.ConnectionSettingsOffers{
			Hash: []byte{byte(n)}, Opamp: &protobufs.OpAMPConnectionSettings{DestinationEndpoint: to,
				Headers: &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: "Bearer key-" + string(rune('a'+n))}}}}}})
		return b
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			m := &protobufs.AgentToServer{}
			_ = proto.Unmarshal(body, m)
			w.Header().Set("Content-Type", "application/x-protobuf")
			_, _ = w.Write(answer(m))
			return
		}
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, data, err := c.ReadMessage()
		m := &protobufs.AgentToServer{}
		if err != nil || proto.Unmarshal(data[1:], m) != nil {
			return
		}
		_ = c.WriteMessage(websocket.BinaryMessage, append([]byte{0}, answer(m)...))
		_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, _, _ = c.ReadMessage()
	}))
	t.Cleanup(s.Close)
	return s, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

func startKeeping(t *testing.T, endpoint string, keep bool) *Client {
	t.Helper()
	c, err := startFull(context.Background(), Config{Mode: ModePlatform, Endpoint: endpoint, HeartbeatInterval: time.Hour, AcceptConnectionSettings: true,
		Header: http.Header{"Authorization": {"Bearer token"}}, KeepUnreached: keep, StateFile: filepath.Join(t.TempDir(), "state")}, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.Stop(ctx); err != nil {
			t.Errorf("a client that holds back a connection doesn't stop: %v", err)
		}
	})
	return c
}

// With KeepUnreached, an agent whose offered endpoint can't be reached doesn't go back to the configured
// credential for another offer, and says which endpoint; without, it does.
func TestKeepUnreached(t *testing.T) {
	for _, to := range []string{"ws://no-such-host.invalid/v1/opamp", "ws://127.0.0.1:1/v1/opamp"} {
		s, offers := offeringServer(t, to)
		c := startKeeping(t, "ws"+s.URL[4:], true)
		late := time.After(15 * time.Second)
		for c.Standing().Unreached == "" {
			select {
			case <-c.StandingChanged():
			case <-late:
				t.Fatal("not told: ", c.Standing())
			}
		}
		if st := c.Standing(); st.Unreached != strings.Split(to, "/")[2] || st.ClaimError == "" || st.ClaimClosed || st.Accepted {
			t.Fatalf("%+v", st)
		}
		time.Sleep(4 * time.Second)
		if n := offers(); n != 1 {
			t.Fatalf("%d offers", n)
		}

		s, offers = offeringServer(t, to)
		c = startKeeping(t, "ws"+s.URL[4:], false)
		for late := time.Now().Add(15 * time.Second); offers() < 3; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(late) {
				t.Fatalf("%d offers", offers())
			}
		}
		if st := c.Standing(); st.Unreached != "" {
			t.Fatalf("%+v", st)
		}
	}
}

// Over HTTP, where a second offer tells that the server enrolls agents: two, and no more.
func TestKeepUnreachedOverHTTP(t *testing.T) {
	s, offers := offeringServer(t, "http://no-such-host.invalid/v1/opamp")
	c := startKeeping(t, s.URL, true)
	for late := time.Now().Add(20 * time.Second); c.Standing().Unreached == ""; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(late) {
			t.Fatalf("not told: %+v after %d offers", c.Standing(), offers())
		}
	}
	time.Sleep(4 * time.Second)
	if n := offers(); n > 2 || c.Standing().Unreached != "no-such-host.invalid" {
		t.Fatalf("%d offers, %+v", n, c.Standing())
	}
}

func TestKeepUnreachedIsPlatformModes(t *testing.T) {
	for _, cfg := range []Config{{KeepUnreached: true}, {KeepUnreachedSaved: true}} {
		cfg.Endpoint = "ws://127.0.0.1:1"
		if _, err := startFull(context.Background(), cfg, Status{}, nil); err == nil || !strings.Contains(err.Error(), "KeepUnreached in core mode") {
			t.Fatal(err)
		}
	}
}

// What tells a try that made no connection from one that did, over HTTP too.
func TestUnreachedIsNoConnection(t *testing.T) {
	s := newStanding()
	s.configure("ws://server", http.Header{"Authorization": {"Bearer token"}}, true)
	s.keepUnreached = true
	header := http.Header{"Authorization": {"Bearer token"}}
	claim := func() *attempt { return s.on(true, "ws://other", http.Header{"Authorization": {"Bearer key"}}) }
	token := func() *attempt { return s.on(true, "ws://server", header) }
	claim().failedToConnect("no such host")
	if token().heldBack() {
		t.Fatal("held back with no round: nothing says that the server enrolls agents")
	}
	s.st.Rounds = 1
	if claim().heldBack() || !token().heldBack() || s.now().Unreached != "other" {
		t.Fatal(s.now())
	}
	s.st = Standing{Rounds: 1}
	c := claim()
	c.opened()()
	c.lost("reset")
	if token().heldBack() {
		t.Fatal("held back after a try that connected")
	}
	claim().failedToConnect("no such host")
	claim().status(http.StatusUnauthorized, nil)
	if token().heldBack() {
		t.Fatal("held back after a try that was refused")
	}
	claim().failedToConnect("no such host")
	s.st.Accepted = true
	if token().heldBack() {
		t.Fatal("held back an agent the server serves")
	}
	s.st = Standing{}
	s.keepSaved = true
	if !token().heldBack() {
		t.Fatal("not held back in a start after a hold")
	}
	s.keepUnreached = false
	if token().heldBack() || (*attempt)(nil).heldBack() {
		t.Fatal("held back unasked")
	}
	token().failedToConnect("no such host")
	if s.unreached != "other" {
		t.Fatal("a try with the configured credential counts: ", s.unreached)
	}
}
