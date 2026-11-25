// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// What a host the server serves, or that isn't enrolling, must never be taken for.

// rvServer is a scriptable WS server: answer(conn number, auth, msg number in conn, m).
type rvServer struct {
	*httptest.Server
	mu    sync.Mutex
	conns []string
	live  int
	pre   func(n int, r *http.Request) int // status to refuse with, 0: upgrade
	ans   func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int)
	msgs  []*protobufs.AgentToServer
}

func newRV(t *testing.T) *rvServer {
	s := &rvServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := len(s.conns)
		s.conns = append(s.conns, r.URL.Path+" "+r.Header.Get("Authorization"))
		s.mu.Unlock()
		if s.pre != nil {
			if c := s.pre(n, r); c != 0 {
				w.WriteHeader(c)
				return
			}
		}
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		s.mu.Lock()
		s.live++
		s.mu.Unlock()
		defer func() { s.mu.Lock(); s.live--; s.mu.Unlock() }()
		for k := 0; ; k++ {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			m := &protobufs.AgentToServer{}
			if proto.Unmarshal(data[1:], m) != nil {
				return
			}
			s.mu.Lock()
			s.msgs = append(s.msgs, m)
			s.mu.Unlock()
			out, code := s.ans(n, k, r.Header.Get("Authorization"), m)
			if out != nil {
				b, _ := proto.Marshal(out)
				_ = c.WriteMessage(websocket.BinaryMessage, append([]byte{0}, b...))
			}
			if code < 0 {
				return // dropped, with no Close
			}
			if code != 0 {
				_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(time.Second))
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						return
					}
				}
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}
func (s *rvServer) url() string { return "ws" + s.URL[4:] + "/v1/opamp" }
func (s *rvServer) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.conns...)
}
func (s *rvServer) liveN() int { s.mu.Lock(); defer s.mu.Unlock(); return s.live }

func rvOffer(m *protobufs.AgentToServer, hash, url, auth string) *protobufs.ServerToAgent {
	op := &protobufs.OpAMPConnectionSettings{DestinationEndpoint: url}
	if auth != "" {
		op.Headers = &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: auth}}}
	}
	return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid, ConnectionSettings: &protobufs.ConnectionSettingsOffers{Hash: []byte(hash), Opamp: op}}
}

func rvHost(t *testing.T, url, dir string, h http.Header, caps protobufs.AgentCapabilities) *coreClient {
	t.Helper()
	c := newCoreClient(zap.NewNop(), filepath.Join(dir, coreClientStateFile), nil)
	require.NoError(t, c.SetAgentDescription(&protobufs.AgentDescription{}))
	require.NoError(t, c.Start(t.Context(), types.StartSettings{OpAMPServerURL: url, Header: h,
		Capabilities: caps | protobufs.AgentCapabilities_AgentCapabilities_AcceptsOpAMPConnectionSettings | protobufs.AgentCapabilities_AgentCapabilities_ReportsConnectionSettingsStatus}))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	return c
}

var tokenHeader = http.Header{"Authorization": {"Bearer token"}}

// A server that serves the host on its configured credential, and offers settings the core
// refuses (an endpoint of the other transport): the host is served, connected.
func TestAServedHostOfferedWhatTheCoreRefusesIsAccepted(t *testing.T) {
	s := newRV(t)
	s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
		if k == 0 {
			return rvOffer(m, "h", s.URL+"/v1/opamp", "Bearer key"), 0
		}
		return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}, 0
	}
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, 0)
	for i := 0; i < 3; i++ {
		time.Sleep(time.Second)
		_ = c.SetHealth(&protobufs.ComponentHealth{Healthy: i%2 == 0})
	}
	time.Sleep(time.Second)
	st, _ := hostStatus(t, dir)
	require.True(t, st.IsAccepted())
}

// A move to an endpoint that 404s, offered again at each connection, by a server that goes on
// serving the host on its configured credential.
func TestAServedHostWhoseMoveFailsIsNotHeld(t *testing.T) {
	s := newRV(t)
	s.pre = func(n int, r *http.Request) int {
		if r.URL.Path == "/moved" {
			return 404
		}
		return 0
	}
	s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
		if k == 0 {
			return rvOffer(m, "h", "ws"+s.URL[4:]+"/moved", "Bearer token"), 0
		}
		return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}, 0
	}
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, 0)
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		_ = c.SetHealth(&protobufs.ComponentHealth{Healthy: i%2 == 0})
	}
	st, _ := hostStatus(t, dir)
	require.True(t, st.IsAccepted())
	require.False(t, c.holding(), "held for an hour")
}

// The same, the offer sent once: the host is back on its configured credential, and served.
func TestAServedHostWhoseMoveFailedOnceIsWell(t *testing.T) {
	s := newRV(t)
	s.pre = func(n int, r *http.Request) int {
		if r.URL.Path == "/moved" {
			return 502
		}
		return 0
	}
	var once sync.Once
	s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
		out := &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}
		once.Do(func() { out = rvOffer(m, "h", "ws"+s.URL[4:]+"/moved", "") })
		return out, 0
	}
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, 0)
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		_ = c.SetHealth(&protobufs.ComponentHealth{Healthy: i%2 == 0})
	}
	_ = c.SetHealth(&protobufs.ComponentHealth{Healthy: true})
	st, _ := hostStatus(t, dir)
	require.True(t, st.Server.Connected)
	require.True(t, st.OK(), "connected, served, healthy; not OK")
}

// A WS server that says nothing until it has something to say; one 503 before.
func TestAQuietServerAfterA503IsWell(t *testing.T) {
	s := newRV(t)
	s.pre = func(n int, r *http.Request) int {
		if n == 0 {
			return 503
		}
		return 0
	}
	s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) { return nil, 0 }
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, 0)
	_ = c.SetHealth(&protobufs.ComponentHealth{Healthy: true})
	require.Eventually(t, func() bool { return s.liveN() == 1 }, 40*time.Second, 50*time.Millisecond)
	time.Sleep(2 * time.Second)
	st, _ := hostStatus(t, dir)
	require.True(t, st.Server.Connected)
	require.True(t, st.OK(), "connected to a quiet server and healthy, after one 503: not OK")
}

// A configured instance_id and a UID the server assigned: kept over a hold and a restart.
func TestAnAssignedUIDIsKeptAcrossAHold(t *testing.T) {
	for _, hold := range []bool{false, true} {
		t.Run(fmt.Sprint(hold), func(t *testing.T) {
			setHolds(t, time.Second, time.Second, time.Second)
			X, Y := uuid.New(), uuid.New()
			s := newRV(t)
			refuse := hold
			var mu sync.Mutex
			i := 0
			s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
				mu.Lock()
				defer mu.Unlock()
				if auth == "Bearer token" {
					i++
					o := rvOffer(m, fmt.Sprint("h", i), s.url(), fmt.Sprint("Bearer key", i))
					o.AgentIdentification = &protobufs.AgentIdentification{NewInstanceUid: Y[:]}
					return o, 1000
				}
				if refuse {
					return nil, 1008
				}
				return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}, 0
			}
			dir := t.TempDir()
			start := func() *coreClient {
				c := newCoreClient(zap.NewNop(), filepath.Join(dir, coreClientStateFile), nil)
				require.NoError(t, c.SetAgentDescription(&protobufs.AgentDescription{}))
				require.NoError(t, c.Start(t.Context(), types.StartSettings{OpAMPServerURL: s.url(), Header: tokenHeader, InstanceUid: types.InstanceUid(X),
					Capabilities: protobufs.AgentCapabilities_AgentCapabilities_AcceptsOpAMPConnectionSettings | protobufs.AgentCapabilities_AgentCapabilities_ReportsConnectionSettingsStatus}))
				t.Cleanup(func() { _ = c.Stop(context.Background()) })
				return c
			}
			c := start()
			if hold {
				require.Eventually(t, c.holding, 15*time.Second, 10*time.Millisecond)
				mu.Lock()
				refuse = false
				mu.Unlock()
			}
			require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 15*time.Second, 20*time.Millisecond)
			time.Sleep(500 * time.Millisecond)
			require.Equal(t, [16]byte(Y), c.InstanceUID())
			require.NoError(t, c.Stop(context.Background()))
			c = start()
			require.Equal(t, [16]byte(Y), c.InstanceUID(), "the UID the server assigned")
		})
	}
}

// Renewal, after a restart, of a host with no configured headers (the enrollment token is in the URL).
func TestARenewalWithoutConfiguredHeadersLeavesTheHostAccepted(t *testing.T) {
	s := newRV(t)
	var mu sync.Mutex
	refuseK2 := false
	renew := false
	s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case auth == "":
			return rvOffer(m, "h1", s.url(), "Bearer key1"), 1000
		case auth == "Bearer key2" && refuseK2:
			return nil, 1008
		case auth == "Bearer key1" && renew:
			return rvOffer(m, "h2", s.url(), "Bearer key2"), 0
		}
		return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}, 0
	}
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, nil, 0)
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 15*time.Second, 20*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, c.Stop(context.Background()))
	mu.Lock()
	renew, refuseK2 = true, true
	mu.Unlock()
	c = rvHost(t, s.url(), dir, nil, 0)
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		_ = c.SetHealth(&protobufs.ComponentHealth{Healthy: i%2 == 0})
	}
	st, _ := hostStatus(t, dir)
	require.False(t, c.holding(), "held: a host on its own saved credential")
	require.True(t, st.IsAccepted(), "a renewal took it back")
}

// the same with configured headers, as a control.
func TestARenewalLeavesTheHostAccepted(t *testing.T) {
	s := newRV(t)
	var mu sync.Mutex
	renew := false
	s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case auth == "Bearer token":
			return rvOffer(m, "h1", s.url(), "Bearer key1"), 1000
		case auth == "Bearer key2":
			return nil, 1008
		case auth == "Bearer key1" && renew:
			return rvOffer(m, "h2", s.url(), "Bearer key2"), 0
		}
		return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}, 0
	}
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, 0)
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 15*time.Second, 20*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, c.Stop(context.Background()))
	mu.Lock()
	renew = true
	mu.Unlock()
	c = rvHost(t, s.url(), dir, tokenHeader, 0)
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		_ = c.SetHealth(&protobufs.ComponentHealth{Healthy: i%2 == 0})
	}
	st, _ := hostStatus(t, dir)
	require.True(t, st.IsAccepted(), "a renewal took it back")
}

// A moved host (endpoint only, its headers kept), after a restart, offered a renewal.
func TestARenewalOfAMovedHostLeavesItAccepted(t *testing.T) {
	s := newRV(t)
	var mu sync.Mutex
	renew := false
	moved := "ws" + s.URL[4:] + "/moved"
	s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
		mu.Lock()
		defer mu.Unlock()
		p := s.list()[n]
		switch {
		case p == "/v1/opamp Bearer token":
			return rvOffer(m, "h1", moved, ""), 1000
		case auth == "Bearer key2":
			return nil, 1008
		case renew && k == 0:
			return rvOffer(m, "h2", moved, "Bearer key2"), 0
		}
		return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}, 0
	}
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, 0)
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 15*time.Second, 20*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, c.Stop(context.Background()))
	mu.Lock()
	renew = true
	mu.Unlock()
	c = rvHost(t, s.url(), dir, tokenHeader, 0)
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		_ = c.SetHealth(&protobufs.ComponentHealth{Healthy: i%2 == 0})
	}
	st, _ := hostStatus(t, dir)
	require.False(t, c.holding(), "held")
	require.True(t, st.IsAccepted(), "a renewal took it back")
}

// The same credential offered each time, its claims lost (1011).
func TestTheSameCredentialOfferedAgainEndsTheLoop(t *testing.T) {
	for _, code := range []int{1011, 1000, 1013} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			s := newRV(t)
			s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
				if auth == "Bearer token" {
					return rvOffer(m, "h1", s.url(), "Bearer key1"), 1000
				}
				return nil, code
			}
			dir := t.TempDir()
			c := rvHost(t, s.url(), dir, tokenHeader, 0)
			require.Eventually(t, c.holding, 20*time.Second, 10*time.Millisecond)
			_, e := hostStatus(t, dir)
			require.NotEmpty(t, e["error"])
			require.EqualValues(t, 1, e["offers"], "the same one")
			require.LessOrEqual(t, len(s.list()), 6)
		})
	}
}

// Stop at a random time of holds and resumes, with setters called all along: no race, deadlock or leak.
func TestStopAtAnyTimeOfAHold(t *testing.T) {
	setHolds(t, 30*time.Millisecond, 30*time.Millisecond, 30*time.Millisecond)
	before := runtime.NumGoroutine()
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := newEnrollServer(t)
			s.set(func(s *enrollServer) { s.refuseClaim = "no"; s.maxOffers = 1 << 30 })
			dir := t.TempDir()
			c := newCoreClient(zap.NewNop(), filepath.Join(dir, coreClientStateFile), nil)
			_ = c.SetAgentDescription(&protobufs.AgentDescription{})
			if err := c.Start(context.Background(), types.StartSettings{OpAMPServerURL: s.url(), Header: tokenHeader,
				Callbacks: types.Callbacks{GetEffectiveConfig: func(context.Context) (*protobufs.EffectiveConfig, error) { return &protobufs.EffectiveConfig{}, nil },
					OnMessage: func(context.Context, *types.MessageData) { _ = c.SetHealth(&protobufs.ComponentHealth{}) }},
				Capabilities: protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig | protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth | protobufs.AgentCapabilities_AgentCapabilities_AcceptsOpAMPConnectionSettings | protobufs.AgentCapabilities_AgentCapabilities_ReportsConnectionSettingsStatus}); err != nil {
				t.Error(err)
				return
			}
			done := make(chan struct{})
			go func() {
				for j := 0; ; j++ {
					select {
					case <-done:
						return
					default:
					}
					_ = c.SetHealth(&protobufs.ComponentHealth{Healthy: j%2 == 0})
					_ = c.UpdateEffectiveConfig(context.Background())
					_, _ = c.SendCustomMessage(&protobufs.CustomMessage{Capability: "x"})
					_ = c.InstanceUID()
					time.Sleep(time.Millisecond)
				}
			}()
			time.Sleep(time.Duration(rand.Intn(6000)) * time.Millisecond)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			t0 := time.Now()
			if err := c.Stop(ctx); err != nil {
				t.Errorf("stop: %v", err)
			}
			close(done)
			if d := time.Since(t0); d > 6*time.Second {
				t.Errorf("stop took %s", d)
			}
			st, _ := hostStatus(t, dir)
			if !st.Stopped {
				t.Error("not stopped")
			}
			o, _, _ := s.seen()
			_ = o
		}()
	}
	wg.Wait()
	for i := 0; i < 100 && runtime.NumGoroutine() > before+100; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	var b strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
	for _, f := range []string{"watchStanding", ").hold(", "opamp.(*loop).run"} {
		if strings.Contains(b.String(), f) {
			t.Errorf("leaked %s", f)
		}
	}
}

// The client can't start again at the end of a hold.
func TestAHoldThatCannotEndIsTriedAgain(t *testing.T) {
	setHolds(t, 2*time.Second, time.Second, time.Second)
	s := newEnrollServer(t)
	s.set(func(s *enrollServer) { s.refuseClaim = "no" })
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, 0)
	require.Eventually(t, c.holding, 15*time.Second, 10*time.Millisecond)
	time.Sleep(time.Second)
	f := filepath.Join(dir, coreClientStateFile)
	require.NoError(t, os.Chmod(f, 0))
	if _, err := os.ReadFile(f); err == nil {
		t.Skip("this user, or system, reads it all the same")
	}
	time.Sleep(4 * time.Second)
	require.True(t, c.holding(), "held still")
	st, _ := hostStatus(t, dir)
	require.False(t, st.Stopped)
	s.set(func(s *enrollServer) { s.refuseClaim = "" })
	require.NoError(t, os.Chmod(f, 0o600))
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 30*time.Second, 20*time.Millisecond)
	err := c.Stop(context.Background())
	require.NoError(t, err)
}

// Quiet server, saved credential, one failed connection at start.
func TestAQuietServerAfterAClaimClosedIsWell(t *testing.T) {
	s := newRV(t)
	var quiet atomic.Bool
	var mu sync.Mutex
	fail := false
	s.pre = nil
	s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
		mu.Lock()
		defer mu.Unlock()
		if auth == "Bearer token" {
			return rvOffer(m, "h", s.url(), "Bearer key"), 1000
		}
		if fail {
			fail = false
			return nil, 1011
		}
		if quiet.Load() {
			return nil, 0
		}
		return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}, 0
	}
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, 0)
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 15*time.Second, 20*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, c.Stop(context.Background()))
	quiet.Store(true)
	mu.Lock()
	fail = true
	mu.Unlock()
	c = rvHost(t, s.url(), dir, tokenHeader, 0)
	_ = c.SetHealth(&protobufs.ComponentHealth{Healthy: true})
	time.Sleep(8 * time.Second)
	st, _ := hostStatus(t, dir)
	require.True(t, st.Server.Connected)
	require.True(t, st.OK())
}

func TestAnAuthExtensionRefusedIsNoEnrollment(t *testing.T) {
	s := newRV(t)
	s.pre = func(int, *http.Request) int { return 401 }
	dir := t.TempDir()
	c := newCoreClient(zap.NewNop(), filepath.Join(dir, coreClientStateFile), nil)
	_ = c.SetAgentDescription(&protobufs.AgentDescription{})
	require.NoError(t, c.Start(t.Context(), types.StartSettings{OpAMPServerURL: s.url(), Header: http.Header{"Authorization": {"Bearer configured"}}, HeaderFunc: func(h http.Header) http.Header { h.Set("Authorization", "Bearer ext"); return h }}))
	defer c.Stop(context.Background())
	time.Sleep(2 * time.Second)
	_, e := hostStatus(t, dir)
	require.Nil(t, e)
}

// The instance UID the core made is kept across a hold, then a restart.
func TestTheUIDIsKeptAcrossAHold(t *testing.T) {
	setHolds(t, time.Second, time.Second, time.Second)
	s := newEnrollServer(t)
	s.set(func(s *enrollServer) { s.refuseClaim = "no" })
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, 0)
	uid := c.InstanceUID()
	require.Eventually(t, c.holding, 15*time.Second, 10*time.Millisecond)
	require.Equal(t, uid, c.InstanceUID(), "while held")
	s.set(func(s *enrollServer) { s.refuseClaim = "" })
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 15*time.Second, 20*time.Millisecond)
	require.Equal(t, uid, c.InstanceUID())
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, c.Stop(context.Background()))
	c2 := rvHost(t, s.url(), dir, tokenHeader, 0)
	require.Equal(t, uid, c2.InstanceUID(), "UID after hold + restart")
}

// A server that is down: an enrolled host's status must not say the server answered it.
func TestAServerThatIsDownIsNoEnrollment(t *testing.T) {
	s := newEnrollServer(t)
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, 0)
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 15*time.Second, 20*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, c.Stop(context.Background()))
	url := s.url()
	s.CloseClientConnections()
	s.Close()
	c = rvHost(t, url, dir, tokenHeader, 0)
	time.Sleep(3 * time.Second)
	st, e := hostStatus(t, dir)
	t.Logf("server=%+v enrollment=%v", st.Server, e)
	require.Nil(t, e, "the server never answered, yet the status has an enrollment (the helper then says it answered and didn't take the host)")
	_ = c
}

// Plain HTTP, refused with each of these.
func TestEnrollmentOverHTTP(t *testing.T) {
	for _, code := range []int{401, 403, 409, 0 /* error response */, 429, 503, 404, 200 /* ok */} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var log []string
			i := 0
			var s *httptest.Server
			s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				m := &protobufs.AgentToServer{}
				_ = proto.Unmarshal(b, m)
				mu.Lock()
				defer mu.Unlock()
				auth := r.Header.Get("Authorization")
				log = append(log, auth)
				out := &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}
				if auth == "Bearer token" {
					i++
					out = rvOffer(m, fmt.Sprint("h", i), s.URL, fmt.Sprint("Bearer key", i))
				} else if code == 0 {
					out.ErrorResponse = &protobufs.ServerErrorResponse{Type: protobufs.ServerErrorResponseType_ServerErrorResponseType_BadRequest, ErrorMessage: "refused"}
				} else if code != 200 {
					w.Header().Set("Retry-After", "100")
					w.WriteHeader(code)
					return
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
				o, _ := proto.Marshal(out)
				_, _ = w.Write(o)
			}))
			defer s.Close()
			dir := t.TempDir()
			c := rvHost(t, s.URL, dir, tokenHeader, 0)
			if code == 200 {
				require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 10*time.Second, 20*time.Millisecond)
				return
			}
			time.Sleep(15 * time.Second)
			st, e := hostStatus(t, dir)
			mu.Lock()
			defer mu.Unlock()
			t.Logf("log=%v held=%v server=%+v enrollment=%v", log, c.holding(), st.Server, e)
			require.False(t, st.IsAccepted())
			require.False(t, st.OK())
			require.LessOrEqual(t, i, 3, "credentials offered")
			require.NotNil(t, e)
			if c.holding() {
				require.False(t, st.Server.Connected, "held, and reads connected")
			}
		})
	}
}

// A host deleted on the server comes back on its saved credential: the server may take the
// connection for a moment, until it has heard of the delete, and close it, then refuses it (401).
// Answered first or not, it isn't accepted once it was closed, nor well.
func TestADeletedHostOnItsSavedCredential(t *testing.T) {
	for _, answered := range []bool{false, true} {
		t.Run(fmt.Sprint(answered), func(t *testing.T) {
			s := newRV(t)
			var deleted atomic.Bool
			var refused atomic.Int32
			s.pre = func(_ int, r *http.Request) int {
				if deleted.Load() && refused.Add(1) > 1 {
					return http.StatusUnauthorized
				}
				return 0
			}
			s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
				if auth == "Bearer token" {
					return rvOffer(m, "h", s.url(), "Bearer key"), 1000
				}
				out, code := &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}, 0
				if deleted.Load() {
					code = websocket.ClosePolicyViolation
					if !answered {
						out = nil
					}
				}
				return out, code
			}
			dir := t.TempDir()
			c := rvHost(t, s.url(), dir, tokenHeader, 0)
			require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 15*time.Second, 20*time.Millisecond)
			time.Sleep(500 * time.Millisecond)
			require.NoError(t, c.Stop(context.Background()))
			deleted.Store(true)
			c = rvHost(t, s.url(), dir, tokenHeader, 0)
			require.NoError(t, c.SetHealth(&protobufs.ComponentHealth{Healthy: true}))
			require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.Server != nil && st.Server.HTTPStatus == 401 }, 15*time.Second, 20*time.Millisecond)
			st, _ := hostStatus(t, dir)
			t.Logf("accepted=%v server=%+v", st.IsAccepted(), st.Server)
			require.False(t, st.IsAccepted())
			require.False(t, st.OK())
		})
	}
}

// A server that served the host on its token, and then enrolls it, doesn't serve it any more: one
// it then refuses is held, and not accepted, whether it is offered the same credential each time or
// another.
func TestAHostServedAndThenRefusedIsHeld(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(fmt.Sprint(same), func(t *testing.T) {
			s := newRV(t)
			var enrolls atomic.Bool
			var i atomic.Int32
			s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
				switch {
				case auth != "Bearer token":
					return nil, 1008
				case !enrolls.Load():
					return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}, 0
				}
				j := int32(1)
				if !same {
					j = i.Add(1)
				}
				return rvOffer(m, fmt.Sprint("h", j), s.url(), fmt.Sprint("Bearer key", j)), 1000
			}
			dir := t.TempDir()
			c := rvHost(t, s.url(), dir, http.Header{"authorization": {"Bearer token"}}, protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth)
			require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 10*time.Second, 20*time.Millisecond)
			enrolls.Store(true)
			require.NoError(t, c.SetHealth(&protobufs.ComponentHealth{Healthy: true}))
			require.Eventually(t, c.holding, 20*time.Second, 10*time.Millisecond)
			st, e := hostStatus(t, dir)
			require.False(t, st.IsAccepted())
			require.False(t, st.OK())
			require.NotEmpty(t, e["error"])
			require.LessOrEqual(t, len(s.list()), 7)
		})
	}
}

// A claim closed once, and then a connection with the credential that stays open in silence: what
// failed before no longer holds.
func TestAQuietServerAfterAFailedClaimIsWell(t *testing.T) {
	setHolds(t, time.Second, time.Second, time.Second)
	s := newRV(t)
	var claims atomic.Int32
	s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
		if auth == "Bearer token" {
			return rvOffer(m, "h", s.url(), "Bearer key"), 1000
		}
		if claims.Add(1) == 1 {
			return nil, 1011
		}
		return nil, 0
	}
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, 0)
	require.NoError(t, c.SetHealth(&protobufs.ComponentHealth{Healthy: true}))
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return claims.Load() > 1 && st.Server.Connected && st.OK() }, 40*time.Second, 50*time.Millisecond)
	st, _ := hostStatus(t, dir)
	require.False(t, st.IsAccepted(), "it was never answered")
}

// A host whose connections are lost, with the server's last word an offer, isn't held for it: it
// may be served.
func TestAServedHostThatIsLostIsNotHeld(t *testing.T) {
	s := newRV(t)
	var lose atomic.Int32
	s.pre = func(n int, r *http.Request) int { return 0 }
	s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
		if k == 0 && n > 0 && lose.Add(1) <= 4 {
			go s.CloseClientConnections()
			return rvOffer(m, "h", "http://wrong.example.com", "Bearer key"), 0 // the core refuses it
		}
		return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}, 0
	}
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, 0)
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 10*time.Second, 20*time.Millisecond)
	s.CloseClientConnections()
	time.Sleep(8 * time.Second)
	require.False(t, c.holding())
	st, _ := hostStatus(t, dir)
	require.True(t, st.IsAccepted())
}

// The token's connection closed unanswered once, and then one that stays open in silence: nothing is
// known against the host any more, and the status says nothing of its enrollment.
func TestAQuietServerAfterATokenClosedIsWell(t *testing.T) {
	s := newRV(t)
	s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
		if n == 0 {
			return nil, 1011
		}
		return nil, 0
	}
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, 0)
	require.NoError(t, c.SetHealth(&protobufs.ComponentHealth{Healthy: true}))
	require.Eventually(t, func() bool { st, e := hostStatus(t, dir); return e != nil && !st.OK() }, 10*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { _, e := hostStatus(t, dir); return e == nil }, 30*time.Second, 50*time.Millisecond)
	st, _ := hostStatus(t, dir)
	require.True(t, st.OK())
}

// The loop ends however the token's connection does after the offer: closed as done, as a server
// that stops or says to come back does, or dropped (by a proxy that passes no Close on).
func TestTheLoopEndsHoweverTheTokensConnectionDoes(t *testing.T) {
	for _, same := range []bool{false, true} {
		for _, code := range []int{1000, 1001, 1011, 1012, 1013, -1} {
			t.Run(fmt.Sprintf("%d same=%v", code, same), func(t *testing.T) {
				t.Parallel()
				s := newRV(t)
				var i atomic.Int32
				s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
					if auth != "Bearer token" {
						return nil, 1008
					}
					j := int32(1)
					if !same {
						j = i.Add(1)
					}
					return rvOffer(m, fmt.Sprint("h", j), s.url(), fmt.Sprint("Bearer key", j)), code
				}
				dir := t.TempDir()
				c := rvHost(t, s.url(), dir, tokenHeader, 0)
				require.Eventually(t, c.holding, 25*time.Second, 5*time.Millisecond)
				require.LessOrEqual(t, len(s.list()), 8)
				_, e := hostStatus(t, dir)
				require.NotEmpty(t, e["error"])
			})
		}
	}
}

// A renewal the server doesn't take leaves the host accepted on the credential it has.
func TestARenewalRefusedLeavesTheHostAccepted(t *testing.T) {
	s := newRV(t)
	s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
		switch auth {
		case "Bearer token":
			return rvOffer(m, "h1", s.url(), "Bearer key1"), 1000
		case "Bearer key2":
			return nil, 1008
		}
		if k == 1 {
			return rvOffer(m, "h2", s.url(), "Bearer key2"), 0
		}
		return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}, 0
	}
	dir := t.TempDir()
	c := rvHost(t, s.url(), dir, tokenHeader, protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth)
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 15*time.Second, 5*time.Millisecond)
	for end := time.Now().Add(6 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		_ = c.SetHealth(&protobufs.ComponentHealth{Healthy: true, StatusTimeUnixNano: uint64(time.Now().UnixNano())})
		st, e := hostStatus(t, dir)
		require.True(t, st.IsAccepted(), "%v", e)
		require.Nil(t, e)
	}
	require.Contains(t, s.list(), "/v1/opamp Bearer key2", "the renewal was tried")
}

// A host the server takes and drops on every connection is never accepted for long: the status says
// since when it is taken on the connection it has.
func TestAHostTakenAndDroppedIsNotAcceptedForLong(t *testing.T) {
	for _, code := range []int{1000, 1011, -1} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			s := newRV(t)
			s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
				return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}, code
			}
			dir := t.TempDir()
			rvHost(t, s.url(), dir, tokenHeader, 0)
			require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 15*time.Second, 5*time.Millisecond)
			seen := map[time.Time]bool{}
			for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
				if st, _ := hostStatus(t, dir); st.Accepted != nil {
					seen[st.Accepted.Since] = true
				}
			}
			require.Greater(t, len(seen), 2)
		})
	}
}

// Plain HTTP, from a server that offers the same credential each time, and doesn't take the host with it: under a
// hash of its own each time, the host tries it again and again, until it is held; under the same one it tries it
// once, and asks again at its interval, as a server that serves it and only repeats itself wants.
func TestTheSameOfferOverHTTP(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(fmt.Sprintf("same hash %v", same), func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			tokens, claims := 0, 0
			var s *httptest.Server
			s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				m := &protobufs.AgentToServer{}
				_ = proto.Unmarshal(b, m)
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Authorization") != "Bearer token" {
					claims++
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				tokens++
				hash := "same"
				if !same {
					hash = fmt.Sprint(tokens)
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
				o, _ := proto.Marshal(rvOffer(m, hash, s.URL, "Bearer key"))
				_, _ = w.Write(o)
			}))
			defer s.Close()
			dir := t.TempDir()
			c := rvHost(t, s.URL, dir, tokenHeader, 0)
			seen := func() (int, int) { mu.Lock(); defer mu.Unlock(); return tokens, claims }
			if same {
				time.Sleep(12 * time.Second)
				n, k := seen()
				require.False(t, c.holding())
				require.Equal(t, 1, k)
				require.LessOrEqual(t, n, 5)
				return
			}
			require.Eventually(t, c.holding, 20*time.Second, 20*time.Millisecond)
			time.Sleep(time.Second) // what was under way
			n, k := seen()
			time.Sleep(3 * time.Second)
			m, l := seen()
			require.Equal(t, []int{n, k}, []int{m, l}, "held, and went on")
			require.LessOrEqual(t, k, 8)
			st, e := hostStatus(t, dir)
			require.False(t, st.OK())
			require.EqualValues(t, 1, e["offers"])
		})
	}
}

// A hold ends the held run's watchers: after holds and starts again, one watches the saves.
func TestAHoldEndsTheHeldRunsWatchers(t *testing.T) {
	setHolds(t, time.Second, time.Second, time.Second)
	s := newRV(t)
	var mu sync.Mutex
	refused := 0
	s.ans = func(n, k int, auth string, m *protobufs.AgentToServer) (*protobufs.ServerToAgent, int) {
		mu.Lock()
		defer mu.Unlock()
		if auth == "Bearer token" {
			return rvOffer(m, "h1", s.url(), "Bearer key1"), 1000
		}
		if refused < 2 {
			refused++
			return nil, 1008
		}
		return &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}, 0
	}
	dir := t.TempDir()
	c := newCoreClient(zap.NewNop(), filepath.Join(dir, coreClientStateFile), nil)
	require.NoError(t, c.SetAgentDescription(&protobufs.AgentDescription{}))
	require.NoError(t, c.Start(t.Context(), types.StartSettings{OpAMPServerURL: s.url(), Header: tokenHeader,
		Capabilities: protobufs.AgentCapabilities_AgentCapabilities_AcceptsOpAMPConnectionSettings | protobufs.AgentCapabilities_AgentCapabilities_ReportsConnectionSettingsStatus}))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	require.Eventually(t, func() bool { st, _ := hostStatus(t, dir); return st.IsAccepted() }, 30*time.Second, 20*time.Millisecond)
	mu.Lock()
	require.Equal(t, 2, refused, "refused twice, so held twice")
	mu.Unlock()
	watchers := func() int {
		buf := make([]byte, 1<<22)
		return strings.Count(string(buf[:runtime.Stack(buf, true)]), "(*coreClient).watchSaves(")
	}
	require.Eventually(t, func() bool { return watchers() == 1 }, 5*time.Second, 20*time.Millisecond, "watchers of the saves: %d", watchers())
	require.NoError(t, c.Stop(context.Background()))
	require.Eventually(t, func() bool { return watchers() == 0 }, 5*time.Second, 20*time.Millisecond)
}
