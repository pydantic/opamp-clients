// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/internal/core"
	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// server records every AgentToServer and answers with an empty ServerToAgent.
type server struct {
	mu   sync.Mutex
	msgs []*protobufs.AgentToServer
	uids []string
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m := &protobufs.AgentToServer{}
	if err := proto.Unmarshal(body, m); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	s.uids = append(s.uids, r.Header.Get("OpAMP-Instance-UID"))
	s.mu.Unlock()
	out, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: m.InstanceUid})
	_, _ = w.Write(out)
}

func TestHTTPReportsThenSaysGoodbye(t *testing.T) {
	srv := &server{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	uid := [16]byte{0x01, 0x92, 0x8f, 0x3a, 0x7c, 0x3e, 0x7a, 0x1b, 0x8c, 0x2d, 0x3e, 0x4f, 0x5a, 0x6b, 0x7c, 0x8d}
	desc := &protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{{Key: "service.name",
		Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "test"}}}}}
	var notes []Notification
	var nmu sync.Mutex
	c, err := Start(context.Background(), Config{Mode: ModeGo, Endpoint: ts.URL, InstanceUID: uid, HeartbeatInterval: time.Hour},
		Status{Description: desc}, func(_ *Client, n Notification) { nmu.Lock(); notes = append(notes, n); nmu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Stop right away: the first report may still be in flight, and the goodbye must follow it.
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.msgs) != 2 {
		t.Fatalf("want the first report and agent_disconnect, got %d messages", len(srv.msgs))
	}
	if !proto.Equal(srv.msgs[0].AgentDescription, desc) {
		t.Fatalf("first message lacks the description: %v", srv.msgs[0])
	}
	if srv.msgs[1].AgentDisconnect == nil {
		t.Fatalf("last message is not agent_disconnect: %v", srv.msgs[1])
	}
	if srv.uids[0] != "01928f3a-7c3e-7a1b-8c2d-3e4f5a6b7c8d" {
		t.Fatalf("OpAMP-Instance-UID header %q", srv.uids[0])
	}
	nmu.Lock()
	defer nmu.Unlock()
	if len(notes) == 0 || notes[len(notes)-1].Kind != Stopped {
		t.Fatalf("want Stopped last, got %v", notes)
	}
}

func TestAClientThatStopsOnItsOwnSaysWhy(t *testing.T) {
	stopped := make(chan Notification, 1)
	c, err := Start(context.Background(), Config{Mode: ModeGo, Endpoint: "http://127.0.0.1:1", HeartbeatInterval: time.Hour},
		Status{}, func(_ *Client, n Notification) {
			if n.Kind == Stopped {
				stopped <- n
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	// A string that isn't UTF-8 can't be encoded: the client stops, and the application hears
	// why without calling Stop.
	c.SetDescription(&protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{{Key: "\xff"}}})
	var n Notification
	select {
	case n = <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("no Stopped notification")
	}
	if n.Err == nil {
		t.Fatal("Stopped without the error")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != n.Err {
		t.Fatalf("Stop returned %v, want %v", err, n.Err)
	}
}

// What a step through wazero costs: the protocol runs a few steps a minute, so anything under
// a millisecond is noise.
func BenchmarkStep(b *testing.B) {
	ctx := context.Background()
	c, _, err := core.New(ctx, core.Options{}, &corepb.Config{InstanceUid: make([]byte, 16)})
	if err != nil {
		b.Fatal(err)
	}
	start := &corepb.Input{Kind: &corepb.Input_Start{Start: &corepb.Empty{}}}
	if _, err := c.Step(ctx, 0, 0, start); err != nil {
		b.Fatal(err)
	}
	timer := &corepb.Input{Kind: &corepb.Input_Timer{Timer: &corepb.Empty{}}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Step(ctx, uint64(i), 0, timer); err != nil {
			b.Fatal(err)
		}
	}
}

// Starting a client instantiates the core (compiling it happens once per process).
func BenchmarkNewCore(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		c, _, err := core.New(ctx, core.Options{}, &corepb.Config{InstanceUid: make([]byte, 16)})
		if err != nil {
			b.Fatal(err)
		}
		_ = c.Close(ctx)
	}
}

// Calls from inside the callback never block, however many: the callback runs on the client's
// goroutine, the only one that takes calls. (With a buffer of 64, the 65th waited forever.)
func TestCallsFromTheCallbackDontBlock(t *testing.T) {
	srv := &propServer{} // its answers carry a custom message, so the callback runs
	ts := httptest.NewServer(srv)
	defer ts.Close()
	var c *Client
	ready := make(chan struct{})
	burst := false
	c, err := Start(context.Background(), Config{Mode: ModeGo, Endpoint: ts.URL, HeartbeatInterval: time.Hour},
		Status{CustomCapabilities: &protobufs.CustomCapabilities{Capabilities: []string{propCapability}}},
		func(_ *Client, n Notification) {
			<-ready
			if n.Kind == CustomMessage && !burst {
				burst = true
				for i := 1; i <= 200; i++ {
					c.SetDescription(propDescription(i))
				}
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	close(ready)
	if !eventually(5*time.Second, func() bool { d, _ := lastDescribed(srv.messages()); return d == 200 }) {
		t.Fatal("the last description never arrived: the client is stuck")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

// Any wake_at_ms is valid: one past what a time.Duration holds waits as long as one can, and is
// armed again (RULE-SHELL-08; the adversary's wake_max). It was armed negative: at once.
func TestUntilWakeNeverEarly(t *testing.T) {
	for _, c := range []struct {
		at, now uint64
		want    time.Duration
	}{
		{100, 40, 60 * time.Millisecond},
		{40, 100, 0},
		{1 << 40, 0, time.Duration(1<<40) * time.Millisecond},
		{1 << 63, 5000, time.Duration(maxMillis) * time.Millisecond},
		{^uint64(0), 5000, time.Duration(maxMillis) * time.Millisecond},
	} {
		if got := untilWake(c.at, c.now); got != c.want {
			t.Errorf("untilWake(%d, %d) = %v, want %v", c.at, c.now, got, c.want)
		}
	}
}

// Stop called from inside the callback asks the client to stop and returns at once: the client
// stops once the callback has returned, and says Stopped after it (RULE-SHELL-03). Stop waited for
// the client, which waited for the callback: it never returned (or at its context's deadline).
// covers: RULE-SHELL-03
func TestStopFromTheCallbackReturnsAtOnce(t *testing.T) {
	srv := &propServer{} // its answers carry a custom message, so the callback runs
	ts := httptest.NewServer(srv)
	defer ts.Close()
	type result struct {
		err         error
		took        time.Duration
		stoppedYet  bool
		callsBefore int
	}
	inside := make(chan result, 1)
	stopped := make(chan struct{})
	var mu sync.Mutex
	var kinds []NotificationKind
	asked := false
	c, err := Start(context.Background(), Config{Mode: ModeGo, Endpoint: ts.URL, HeartbeatInterval: time.Hour},
		Status{CustomCapabilities: &protobufs.CustomCapabilities{Capabilities: []string{propCapability}}},
		func(c *Client, n Notification) {
			mu.Lock()
			kinds = append(kinds, n.Kind)
			mu.Unlock()
			if n.Kind == Stopped {
				close(stopped)
				return
			}
			if n.Kind != CustomMessage || asked {
				return
			}
			asked = true
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			t0 := time.Now()
			err := c.Stop(ctx)
			select {
			case <-stopped:
				inside <- result{err: err, took: time.Since(t0), stoppedYet: true}
			default:
				inside <- result{err: err, took: time.Since(t0)}
			}
			// A call after the stop was asked for, from the callback still: it doesn't block.
			c.SetHealth(&protobufs.ComponentHealth{Healthy: true, Status: "after stop"})
		})
	if err != nil {
		t.Fatal(err)
	}
	var r result
	select {
	case r = <-inside:
	case <-time.After(10 * time.Second):
		t.Fatal("the callback never ran, or Stop in it never returned")
	}
	if r.err != nil || r.took > time.Second {
		t.Fatalf("Stop from the callback returned %v after %v; want nil, at once", r.err, r.took)
	}
	if r.stoppedYet {
		t.Fatal("Stopped was told while the callback that asked for the stop was still running")
	}
	// The client stops once the callback has returned.
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the stop asked for from the callback never happened")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop from outside, after: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := len(kinds); n == 0 || kinds[n-1] != Stopped {
		t.Fatalf("want Stopped last, got %v", kinds)
	}
	for _, k := range kinds[:len(kinds)-1] {
		if k == Stopped {
			t.Fatalf("Stopped told twice: %v", kinds)
		}
	}
	// The goodbye went: the stop asked for from the callback is an ordinary stop.
	msgs := srv.messages()
	if len(msgs) == 0 || msgs[len(msgs)-1].AgentDisconnect == nil {
		t.Fatal("no agent_disconnect after the stop asked for from the callback")
	}
}

// A callback that panics on the Stopped of an ordinary stop has heard Stopped: it is not told it
// again (RULE-SHELL-01: stopped exactly once, last), and Stop returns the panic (RULE-SHELL-02:
// the error is the callback's failure). The client said Stopped a second time, carrying the
// panic, after the one the callback had panicked on.
// covers: RULE-SHELL-01, RULE-SHELL-02
func TestAPanicOnStoppedIsNotToldStoppedAgain(t *testing.T) {
	for _, mode := range []Mode{ModeRust, ModeGo} {
		t.Run(string(mode), func(t *testing.T) {
			ts := httptest.NewServer(&server{})
			defer ts.Close()
			var mu sync.Mutex
			var stopped []error
			c, err := Start(context.Background(), Config{Mode: mode, Endpoint: ts.URL, HeartbeatInterval: time.Hour},
				Status{}, func(_ *Client, n Notification) {
					if n.Kind != Stopped {
						return
					}
					mu.Lock()
					stopped = append(stopped, n.Err)
					mu.Unlock()
					panic("broken on stopped")
				})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = c.Stop(ctx)
			if err == nil || !strings.Contains(err.Error(), "broken on stopped") {
				t.Fatalf("Stop returned %v, want the panic", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(stopped) != 1 || stopped[0] != nil {
				t.Fatalf("Stopped told %d times (errors %v), want once, the stop's", len(stopped), stopped)
			}
		})
	}
}

// Stop on a client that has stopped says how it stopped, every time, whatever its context: the
// error it stopped with, never the context's. With a context already done, Stop picked at random
// between the two (a select with both ready), and said context.Canceled about half the time.
func TestStopAfterTheEndIgnoresTheContext(t *testing.T) {
	stopped := make(chan struct{})
	c, err := Start(context.Background(), Config{Mode: ModeGo, Endpoint: "http://127.0.0.1:1", HeartbeatInterval: time.Hour},
		Status{}, func(_ *Client, n Notification) {
			if n.Kind == Stopped {
				close(stopped)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	// A string that isn't UTF-8 can't be encoded: the client stops on its own, with the error.
	c.SetDescription(&protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{{Key: "\xff"}}})
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("no Stopped notification")
	}
	want := c.Stop(context.Background())
	if want == nil {
		t.Fatal("Stop returned nil for a client that stopped with an error")
	}
	done, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 100 {
		if err := c.Stop(done); err != want {
			t.Fatalf("Stop with a done context, call %d: %v, want %v", i, err, want)
		}
	}
}
