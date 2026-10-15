// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opamp

// Properties of the client around the core, over random plans (a seeded generator): the
// application's calls, spread over one to four goroutines calling at once and the notification
// callback (so some reach the client while it is still carrying out the step that called back),
// over either transport. Order is only defined per caller, so whatever the plan: every custom
// message reaches the server exactly once, each caller's in the order it sent them, the server
// ends with the description set last, sequence numbers run without a gap, the goodbye is the
// last message, and nothing is reported as an error. A call never blocks, even from the callback:
// a stalled plan fails.
//
// OPAMP_PROP_RUNS sets the number of plans (default 100), OPAMP_PROP_SEED replays a failure.

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

const propCapability = "io.example.tick"

// fromCallback is the caller of the steps made from inside the notification callback.
const fromCallback = -1

// propStep is one application call: a new description or a custom message.
type propStep struct {
	describe bool
	caller   int  // a goroutine (0..), or fromCallback
	pause    bool // goroutines: sleep a moment first
	more     bool // callback: carry out the callback's next step in the same notification too
}

func (s propStep) String() string {
	what := "send"
	if s.describe {
		what = "describe"
	}
	who := "cb"
	if s.caller != fromCallback {
		who = "g" + strconv.Itoa(s.caller)
	}
	flag := ""
	if s.pause {
		flag += "+pause"
	}
	if s.more {
		flag += "+more"
	}
	return what + "@" + who + flag
}

// genPlan makes a plan. Most are short; some are long, so that many calls wait while the client
// is calling back (the client's input buffer of 64 once deadlocked there).
func genPlan(r *rand.Rand) (plan []propStep, goroutines int, ws bool) {
	goroutines = 1 + r.IntN(4)
	n, describe := 1+r.IntN(16), 1.0/3
	if r.IntN(4) == 0 {
		n, describe = 1+r.IntN(150), 2.0/3
	}
	// How often the callback goes on to its next step, and a goroutine pauses: per plan, so some
	// plans call back in long bursts while the goroutines keep calling.
	more, pause := r.Float64(), r.Float64()/2
	sends := 0
	for range n {
		s := propStep{describe: r.Float64() < describe, caller: r.IntN(goroutines+1) - 1}
		if !s.describe {
			// The core refuses a custom message while 64 wait (CustomMessageNotSent): stay under.
			if sends++; sends > 60 {
				s.describe = true
			}
		}
		if s.caller == fromCallback {
			s.more = r.Float64() < more
		} else {
			s.pause = r.Float64() < pause
		}
		plan = append(plan, s)
	}
	return plan, goroutines, r.IntN(2) == 0
}

func propDescription(n int) *protobufs.AgentDescription {
	return &protobufs.AgentDescription{NonIdentifyingAttributes: []*protobufs.KeyValue{{Key: "n",
		Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_IntValue{IntValue: int64(n)}}}}}
}

// propServer records every AgentToServer, over HTTP or WebSocket, and puts a custom message in
// every answer, so the application is called back often.
type propServer struct {
	mu  sync.Mutex
	got []*protobufs.AgentToServer
}

func (s *propServer) reply(body []byte) []byte {
	m := &protobufs.AgentToServer{}
	if err := proto.Unmarshal(body, m); err != nil {
		return nil
	}
	s.mu.Lock()
	s.got = append(s.got, m)
	s.mu.Unlock()
	tick := &protobufs.CustomMessage{Capability: propCapability, Type: "tick"}
	out, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: m.InstanceUid, CustomMessage: tick})
	return out
}

func (s *propServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
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
			if err := conn.WriteMessage(websocket.BinaryMessage, append([]byte{0}, s.reply(data[1:])...)); err != nil {
				return
			}
		}
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(s.reply(body))
}

func (s *propServer) messages() []*protobufs.AgentToServer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

// delivered lists the custom messages the application sent, as the plan's step numbers.
func delivered(got []*protobufs.AgentToServer) []int {
	var out []int
	for _, m := range got {
		if cm := m.GetCustomMessage(); cm != nil && cm.Type == "t" {
			n, _ := strconv.Atoi(string(cm.Data))
			out = append(out, n)
		}
	}
	return out
}

// lastDescribed is the description the server ended with.
func lastDescribed(got []*protobufs.AgentToServer) (int, bool) {
	for i := len(got) - 1; i >= 0; i-- {
		if d := got[i].GetAgentDescription(); d != nil {
			return int(d.NonIdentifyingAttributes[0].Value.GetIntValue()), true
		}
	}
	return 0, false
}

func eventually(timeout time.Duration, ok func() bool) bool {
	for deadline := time.Now().Add(timeout); ; time.Sleep(5 * time.Millisecond) {
		if ok() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

func checkPlan(plan []propStep, goroutines int, ws bool, mode Mode) error {
	srv := &propServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// What each caller does, in plan order; a step's number is its place in the plan, from 1.
	byCaller := map[int][]int{}
	for i, s := range plan {
		byCaller[s.caller] = append(byCaller[s.caller], i+1)
	}

	var (
		descMu   sync.Mutex // held across recording and making the call, so last is the last made
		lastDesc int
		errMu    sync.Mutex
		errs     []string
	)
	fail := func(format string, args ...any) {
		errMu.Lock()
		errs = append(errs, fmt.Sprintf(format, args...))
		errMu.Unlock()
	}
	var client *Client
	carryOut := func(n int) {
		if plan[n-1].describe {
			descMu.Lock()
			lastDesc = n
			client.SetDescription(propDescription(n))
			descMu.Unlock()
		} else {
			client.SendCustomMessage(&protobufs.CustomMessage{Capability: propCapability, Type: "t",
				Data: []byte(strconv.Itoa(n))})
		}
	}

	ready := make(chan struct{}) // client is set
	cbSteps := byCaller[fromCallback]
	cbDone := 0 // only touched on the client's goroutine
	var cbCarried atomic.Int32
	cbFinished := make(chan struct{})
	if len(cbSteps) == 0 {
		close(cbFinished)
	}
	notify := func(_ *Client, n Notification) {
		switch n.Kind {
		case Stopped:
			if n.Err != nil {
				fail("stopped: %v", n.Err)
			}
		case CustomMessageNotSent:
			fail("custom message not sent: %s", n.Detail)
		}
		select {
		case <-ready:
		default:
			return
		}
		// The callback's steps due now, called from inside it.
		for cbDone < len(cbSteps) {
			step := cbSteps[cbDone]
			cbDone++
			carryOut(step)
			cbCarried.Add(1)
			if cbDone == len(cbSteps) {
				close(cbFinished)
			}
			if !plan[step-1].more {
				break
			}
		}
	}

	cfg := Config{Endpoint: ts.URL + "/v1/opamp", InstanceUID: [16]byte{0x42}, HeartbeatInterval: 20 * time.Millisecond, Mode: mode}
	if mode == ModeRust {
		cfg.Proxy = Proxy{Direct: true}
	}
	if ws {
		cfg.Endpoint = "ws" + strings.TrimPrefix(cfg.Endpoint, "http")
	}
	var err error
	client, err = Start(context.Background(), cfg, Status{Description: propDescription(0),
		CustomCapabilities: &protobufs.CustomCapabilities{Capabilities: []string{propCapability}}}, notify)
	if err != nil {
		return err
	}
	close(ready)

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for _, n := range byCaller[g] {
				if plan[n-1].pause {
					time.Sleep(time.Millisecond)
				}
				carryOut(n)
			}
		})
	}
	callersDone := make(chan struct{})
	go func() { wg.Wait(); <-cbFinished; close(callersDone) }()
	select {
	case <-callersDone:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("the plan stalled: the callback carried out %d of %d steps", cbCarried.Load(), len(cbSteps))
	}

	var sent []int
	for i, s := range plan {
		if !s.describe {
			sent = append(sent, i+1)
		}
	}
	descMu.Lock()
	want := lastDesc
	descMu.Unlock()
	eventually(10*time.Second, func() bool {
		got := srv.messages()
		d, _ := lastDescribed(got)
		return len(delivered(got)) >= len(sent) && d == want
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Stop(ctx); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	// Over WebSocket the server may still be reading the goodbye.
	eventually(2*time.Second, func() bool {
		got := srv.messages()
		return len(got) > 0 && got[len(got)-1].AgentDisconnect != nil
	})

	got := srv.messages()
	errMu.Lock()
	defer errMu.Unlock()
	if len(errs) > 0 {
		return fmt.Errorf("errors reported: %v", errs)
	}
	del := delivered(got)
	if err := checkOnceEach(del, sent); err != nil {
		return err
	}
	sends := map[int][]int{}
	for caller, steps := range byCaller {
		for _, n := range steps {
			if !plan[n-1].describe {
				sends[caller] = append(sends[caller], n)
			}
		}
	}
	if err := checkCallerOrder(del, sends); err != nil {
		return err
	}
	if d, ok := lastDescribed(got); !ok || d != want {
		return fmt.Errorf("the server ended with description %d, the last set was %d", d, want)
	}
	if err := checkSequence(got); err != nil {
		return err
	}
	return checkGoodbyeLast(got)
}

// The properties' checks over what the server got, shared with the schedule test
// (schedule_properties_test.go), which runs them inside its state machine.

// checkOnceEach: every custom message sent (by number) arrived exactly once, and nothing else did.
func checkOnceEach(del, sent []int) error {
	counts := map[int]int{}
	for _, n := range del {
		counts[n]++
	}
	for _, n := range sent {
		if counts[n] != 1 {
			return fmt.Errorf("custom message %d arrived %d times (delivered %v)", n, counts[n], del)
		}
	}
	if len(del) != len(sent) {
		return fmt.Errorf("delivered %v, sent %v", del, sent)
	}
	return nil
}

// checkCallerOrder: each caller's custom messages (sends, by number, in the order it made them)
// first arrived in that order. A message that arrived again (resent after a lost answer) counts
// at its first arrival.
func checkCallerOrder(del []int, sends map[int][]int) error {
	callerOf := map[int]int{}
	for caller, ns := range sends {
		for _, n := range ns {
			callerOf[n] = caller
		}
	}
	seen := map[int]bool{}
	arrived := map[int][]int{}
	for _, n := range del {
		caller, ok := callerOf[n]
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		arrived[caller] = append(arrived[caller], n)
	}
	for caller, mine := range sends {
		var want []int
		for _, n := range mine {
			if seen[n] {
				want = append(want, n)
			}
		}
		if !slices.Equal(arrived[caller], want) {
			return fmt.Errorf("caller %d sent %v, arrived in the order %v", caller, mine, arrived[caller])
		}
	}
	return nil
}

// checkSequence: sequence numbers are 0 until the server has answered a message (the messages
// sent before its first answer, RULE-CLIENT-20), then run from 1 without a gap.
func checkSequence(got []*protobufs.AgentToServer) error {
	var next uint64
	for i, m := range got {
		switch {
		case m.SequenceNum == next, next == 0 && m.SequenceNum == 1:
			next = m.SequenceNum
			if next > 0 {
				next++
			}
		default:
			return fmt.Errorf("message %d has sequence number %d, not %d", i, m.SequenceNum, next)
		}
	}
	return nil
}

// checkGoodbyeLast: the goodbye (agent_disconnect) came, and last.
func checkGoodbyeLast(got []*protobufs.AgentToServer) error {
	if len(got) == 0 || got[len(got)-1].AgentDisconnect == nil {
		return fmt.Errorf("the goodbye is not last")
	}
	return nil
}

func TestCallsFromAnywhereArriveOnceAndInOrder(t *testing.T) {
	runs := 100
	if v := os.Getenv("OPAMP_PROP_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("OPAMP_PROP_RUNS: %v", err)
		}
		runs = n
	}
	seed := uint64(time.Now().UnixNano())
	if v := os.Getenv("OPAMP_PROP_SEED"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("OPAMP_PROP_SEED: %v", err)
		}
		seed = n
	}
	// Half the runs in Go mode, half in Rust mode.
	for i := range runs {
		plan, goroutines, ws := genPlan(rand.New(rand.NewPCG(seed, uint64(i))))
		mode := []Mode{ModeGo, ModeRust}[i%2]
		if err := checkPlan(plan, goroutines, ws, mode); err != nil {
			t.Fatalf("run %d (replay: OPAMP_PROP_SEED=%d), %s mode, %d goroutines, websocket %v, plan %v:\n%v",
				i, seed, modeName(mode), goroutines, ws, plan, err)
		}
	}
}
