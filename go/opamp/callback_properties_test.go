// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opamp

// The callback contract (spec/shell-contract.md, RULE-SHELL-01 to -03 and -07), as a model
// test over random runs (a seeded generator, as properties_test.go): each run is one to three
// sessions of the real client against one local server, on one state file, so a remote config
// handed over in one session is handed over again at the start of the next. In a session: calls
// from outside, from another goroutine and from inside the callback, offers from the server (a
// remote config, a custom message, a heartbeat interval), stop from outside or requested from
// inside the callback, and calls after stop. After each step the notifications seen so far are
// checked against the model: one callback at a time, none before Start returned, none after
// stopped, stopped exactly once (with the error when a callback panicked) and last, Stop bounded
// and returning that error, the server's offers handed over once each in the order made (a saved
// remote config first), and each caller's calls reaching the server in its order.
//
// Fixed, and still checked: S5, a callback before Start returned (counted, and none is
// seen now: notifications are held until Start returns); a panic in the callback killed the
// process (panicKillsProcess is false: the client recovers, and stops with the panic); Stop
// called from the callback deadlocked (it waited for the client, which waited for the callback):
// the model calls it there, and it must return at once, before stopped.
//
// OPAMP_MODEL_RUNS sets the number of runs (default 30), OPAMP_MODEL_SEED replays a failure.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

// panicKillsProcess: a panic in the callback takes the process down. While it does, runs
// with a panic go to a child process and are expected to fail; once the client recovers, set it
// to false and the main model test draws panics too.
const panicKillsProcess = false

const (
	modelCapability = "io.example.model"
	modelHeartbeat  = 20 * time.Millisecond
	modelReqTimeout = 2 * time.Second
	// stopBound: the core's stop timeout (5 s), with a margin.
	stopBound = modelReqTimeout + 3*time.Second
	// awaitBound: how long an offer may take to be handed over.
	awaitBound = 5 * time.Second
	// modelPanic is what the callback panics with.
	modelPanic = "opamp-model: callback panic"
)

var modelUID = [16]byte{0x01, 0x92, 0x8f, 0x3a, 0x7c, 0x3e, 0x7a, 0x1b, 0x8c, 0x2d, 0x3e, 0x4f, 0x5a, 0x6b, 0x7c, 0x8d}

type mWho int

const (
	mOutside   mWho = iota // the test's goroutine
	mGoroutine             // another goroutine (one per session: one caller, in order)
	mInside                // the callback
)

func (w mWho) String() string { return [...]string{"out", "go", "cb"}[w] }

type mKind int

const (
	mCall  mKind = iota // an application call
	mOffer              // the server offers something
	mPanic              // the callback panics on its k-th call from now (then the session ends)
)

type mOfferKind int

const (
	offerConfig mOfferKind = iota
	offerCustom
	offerHeartbeat
)

type mStep struct {
	kind     mKind
	who      mWho
	describe bool       // mCall: SetDescription (checked for order), else SetHealth
	offer    mOfferKind // mOffer
	k        int        // mPanic: on the k-th callback from now; mOffer heartbeat: which value
}

func (s mStep) String() string {
	switch s.kind {
	case mCall:
		what := "health"
		if s.describe {
			what = "describe"
		}
		return what + "@" + s.who.String()
	case mOffer:
		return [...]string{"offer-config", "offer-custom", "offer-heartbeat"}[s.offer]
	default:
		return "panic@" + strconv.Itoa(s.k)
	}
}

type mSession struct {
	stateFile  bool // on the run's state file (else none)
	steps      []mStep
	stopInside bool // stop requested from inside the callback (else from outside)
	callsAfter int  // calls after stop
}

type mPlan struct {
	ws       bool
	sessions []mSession
}

func (p mPlan) String() string {
	var b strings.Builder
	if p.ws {
		b.WriteString("websocket")
	} else {
		b.WriteString("http")
	}
	for i, s := range p.sessions {
		fmt.Fprintf(&b, " | session %d: state=%v %v stop@", i+1, s.stateFile, s.steps)
		if s.stopInside {
			b.WriteString("cb")
		} else {
			b.WriteString("out")
		}
		if s.callsAfter > 0 {
			fmt.Fprintf(&b, " +%d calls after", s.callsAfter)
		}
	}
	return b.String()
}

// genModelPlan makes a run. withPanic: the last session panics in the callback.
func genModelPlan(r *rand.Rand, withPanic bool) mPlan {
	p := mPlan{ws: r.IntN(2) == 0}
	n := 1 + r.IntN(3)
	for i := range n {
		s := mSession{stateFile: i == 0 || r.IntN(4) != 0, stopInside: r.IntN(3) == 0, callsAfter: r.IntN(3)}
		for range r.IntN(9) {
			var st mStep
			switch r.IntN(5) {
			case 0, 1:
				st = mStep{kind: mCall, who: mWho(r.IntN(3)), describe: r.IntN(3) != 0}
			case 2:
				st = mStep{kind: mOffer, offer: offerConfig}
			case 3:
				st = mStep{kind: mOffer, offer: offerCustom}
			default:
				st = mStep{kind: mOffer, offer: offerHeartbeat, k: r.IntN(3)}
			}
			s.steps = append(s.steps, st)
		}
		// Most sessions get a config, so that the next one starts with a saved one.
		if r.IntN(3) != 0 {
			s.steps = append(s.steps, mStep{kind: mOffer, offer: offerConfig})
			j := r.IntN(len(s.steps))
			s.steps[j], s.steps[len(s.steps)-1] = s.steps[len(s.steps)-1], s.steps[j]
		}
		if withPanic && i == n-1 {
			s.steps = append(s.steps, mStep{kind: mPanic, k: 1 + r.IntN(3)})
		}
		p.sessions = append(p.sessions, s)
	}
	return p
}

// modelServer answers over HTTP and WebSocket with the offers the test makes, each once, and
// records the descriptions it receives. Over WebSocket it pushes an offer at once.
type modelServer struct {
	mu      sync.Mutex
	pending []*protobufs.ServerToAgent
	descs   []int64
	ws      *websocket.Conn
	wsMu    *sync.Mutex // one writer at a time on ws
}

func (s *modelServer) offer(m *protobufs.ServerToAgent) {
	s.mu.Lock()
	s.pending = append(s.pending, m)
	conn, wmu := s.ws, s.wsMu
	s.mu.Unlock()
	if conn != nil {
		s.flush(conn, wmu)
	}
}

func (s *modelServer) record(body []byte) bool {
	m := &protobufs.AgentToServer{}
	if err := proto.Unmarshal(body, m); err != nil {
		return false
	}
	if d := m.GetAgentDescription(); d != nil && len(d.NonIdentifyingAttributes) > 0 {
		s.mu.Lock()
		s.descs = append(s.descs, d.NonIdentifyingAttributes[0].Value.GetIntValue())
		s.mu.Unlock()
	}
	return true
}

func (s *modelServer) take(all bool) []*protobufs.ServerToAgent {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.pending)
	if !all {
		n = min(n, 1)
	}
	out := s.pending[:n:n]
	s.pending = s.pending[n:]
	return out
}

func (s *modelServer) flush(conn *websocket.Conn, wmu *sync.Mutex) {
	wmu.Lock()
	defer wmu.Unlock()
	for _, m := range s.take(true) {
		m.InstanceUid = modelUID[:]
		out, _ := proto.Marshal(m)
		_ = conn.WriteMessage(websocket.BinaryMessage, append([]byte{0}, out...))
	}
}

func (s *modelServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		wmu := &sync.Mutex{}
		s.mu.Lock()
		s.ws, s.wsMu = conn, wmu
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			if s.ws == conn {
				s.ws, s.wsMu = nil, nil
			}
			s.mu.Unlock()
		}()
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.BinaryMessage || len(data) == 0 || !s.record(data[1:]) {
				continue
			}
			wmu.Lock()
			out, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: modelUID[:]})
			_ = conn.WriteMessage(websocket.BinaryMessage, append([]byte{0}, out...))
			wmu.Unlock()
			s.mu.Lock()
			current := s.ws == conn
			s.mu.Unlock()
			if current { // offers go to this session's connection only
				s.flush(conn, wmu)
			}
		}
	}
	body, _ := io.ReadAll(r.Body)
	if !s.record(body) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	m := &protobufs.ServerToAgent{}
	if got := s.take(false); len(got) > 0 {
		m = got[0]
	}
	m.InstanceUid = modelUID[:]
	out, _ := proto.Marshal(m)
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

// newSession forgets the last session: its descriptions, and its connection (which may not have
// ended yet): offers go to the next session's.
func (s *modelServer) newSession() {
	s.mu.Lock()
	s.descs = nil
	s.ws, s.wsMu = nil, nil
	s.mu.Unlock()
}

func (s *modelServer) descriptions() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.descs...)
}

// handed is a notification an offer leads to, as the model predicts it.
type handed struct {
	kind NotificationKind
	key  string
}

func (h handed) String() string { return fmt.Sprintf("%d:%s", h.kind, h.key) }

func handedOf(n Notification) (handed, bool) {
	switch n.Kind {
	case RemoteConfig:
		return handed{RemoteConfig, string(n.RemoteConfig.GetConfigHash())}, true
	case CustomMessage:
		return handed{CustomMessage, string(n.CustomMessage.GetData())}, true
	case HeartbeatInterval:
		return handed{HeartbeatInterval, n.HeartbeatInterval.String()}, true
	}
	return handed{}, false
}

// mClient is one session's client and what its callback saw.
type mClient struct {
	client     atomic.Pointer[Client]
	started    atomic.Bool
	inProgress atomic.Int32

	mu         sync.Mutex
	notes      []Notification
	violations []string
	early      int // callbacks before Start returned (S5)
	calls      int
	stopped    int
	stoppedErr error
	panicAt    int                        // the callback panics on this call (0: never)
	armed      map[string]func(c *Client) // what the callback does on the custom message with this data
	cbDesc     int64                      // the callback's descriptions, as a caller
	answered   []cbCall                   // the remote configs the callback answered itself, before it returned

	stopInside chan error // the result of a stop requested from inside

	// also, if set, is called inside every callback, after the model's own handling (and before a
	// panic it was told to make): the schedule test's calls from the callback.
	also func(n Notification)
}

func (m *mClient) violate(format string, args ...any) {
	m.violations = append(m.violations, fmt.Sprintf(format, args...))
}

func (m *mClient) notify(n Notification) {
	if m.inProgress.Add(1) > 1 {
		m.mu.Lock()
		m.violate("RULE-SHELL-01: two callbacks at once (%v)", n.Kind)
		m.mu.Unlock()
	}
	defer m.inProgress.Add(-1)
	m.mu.Lock()
	if !m.started.Load() {
		m.early++ // S5: tolerated
	}
	if m.stopped > 0 {
		m.violate("RULE-SHELL-01: a callback (%v) after stopped", n.Kind)
	}
	m.notes = append(m.notes, n)
	m.calls++
	k := m.calls
	if n.Kind == Stopped {
		m.stopped++
		m.stoppedErr = n.Err
	}
	var action func(*Client)
	if n.Kind == CustomMessage {
		action = m.armed[string(n.CustomMessage.GetData())]
		delete(m.armed, string(n.CustomMessage.GetData()))
	}
	panicNow := m.panicAt == k && n.Kind != Stopped
	m.mu.Unlock()

	c := m.client.Load()
	if n.Kind == RemoteConfig {
		answer := &protobufs.RemoteConfigStatus{LastRemoteConfigHash: n.RemoteConfig.GetConfigHash(),
			Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED}
		if c != nil {
			c.SetRemoteConfigStatus(answer)
			m.mu.Lock()
			m.answered = append(m.answered, answerCall(k, answer.LastRemoteConfigHash))
			m.mu.Unlock()
		} else {
			// S5: no handle yet. Answer once there is one, as go/otel does.
			go func() {
				for m.client.Load() == nil {
					time.Sleep(time.Millisecond)
				}
				m.client.Load().SetRemoteConfigStatus(answer)
			}()
		}
	}
	if action != nil {
		if c == nil {
			// Only an offer made after Start returned arms an action: its callback can't be early.
			m.mu.Lock()
			m.violate("an armed callback without the handle")
			m.mu.Unlock()
		} else {
			action(c)
		}
	}
	if m.also != nil {
		m.also(n)
	}
	if panicNow {
		panic(modelPanic)
	}
}

// answers is the remote configs the callback answered itself, as calls it made.
func (m *mClient) answers() []cbCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]cbCall(nil), m.answered...)
}

func (m *mClient) snapshot() (notes []Notification, stopped int, stoppedErr error, violations []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Notification(nil), m.notes...), m.stopped, m.stoppedErr, append([]string(nil), m.violations...)
}

// contract checks the notifications seen so far against RULE-SHELL-01 and -02: no violation
// recorded by notify (two callbacks at once, one after stopped), stopped at most once and last, and
// with an error only if a callback panicked.
func (m *mClient) contract(where string, panicked bool) error {
	notes, stopped, _, violations := m.snapshot()
	if len(violations) > 0 {
		return fmt.Errorf("after %s: %v", where, violations)
	}
	if stopped > 1 {
		return fmt.Errorf("after %s: RULE-SHELL-02: stopped %d times", where, stopped)
	}
	if stopped == 1 && notes[len(notes)-1].Kind != Stopped {
		return fmt.Errorf("after %s: RULE-SHELL-01: stopped is not last: %v", where, kinds(notes))
	}
	if stopped == 1 && !panicked && notes[len(notes)-1].Err != nil {
		return fmt.Errorf("after %s: stopped with %v, no callback failed", where, notes[len(notes)-1].Err)
	}
	return nil
}

func (m *mClient) handedSoFar() []handed {
	notes, _, _, _ := m.snapshot()
	var out []handed
	for _, n := range notes {
		if h, ok := handedOf(n); ok {
			out = append(out, h)
		}
	}
	return out
}

// modelDesc encodes a description by caller and number, to check each caller's order.
func modelDesc(who mWho, n int64) *protobufs.AgentDescription {
	return propDescription(int(int64(who)*1_000_000 + n))
}

// modelRun runs a plan; it returns the first way it broke the model, and how many callbacks came
// before Start returned (S5).
type modelRun struct {
	mode  Mode // the client's: Go mode, or Rust mode
	plan  mPlan
	dir   string
	srv   *modelServer
	ts    *httptest.Server
	saved string // the hash of the remote config saved in the state file ("": none)
	// savedOr: what the state file may hold instead of saved. A save takes its time beside the
	// loop; a config that arrives while one is writing is saved after it, and a stop in between
	// leaves the earlier one ("as a crash would leave it": the core saves nothing once stopping).
	savedOr map[string]bool
	hash    int // the last config hash offered
	cust    int // the last custom message offered
	stats   modelStats
}

// modelStats: what the runs exercised.
type modelStats struct {
	savedStarts int // sessions that started with a saved remote config
	early       int // callbacks before Start returned (S5)
}

func runModelPlan(plan mPlan, dir string, mode Mode) (stats modelStats, err error) {
	r := &modelRun{mode: mode, plan: plan, dir: dir, srv: &modelServer{}}
	r.ts = httptest.NewServer(r.srv)
	defer r.ts.Close()
	for i, s := range plan.sessions {
		if err := r.session(s); err != nil {
			return r.stats, fmt.Errorf("session %d: %w", i+1, err)
		}
	}
	return r.stats, nil
}

func (r *modelRun) session(s mSession) error {
	r.srv.newSession()
	cfg := Config{Endpoint: r.ts.URL + "/v1/opamp", InstanceUID: modelUID, HeartbeatInterval: modelHeartbeat,
		RequestTimeout: modelReqTimeout, Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
		Mode: r.mode}
	if r.mode == ModeRust {
		cfg.Proxy = Proxy{Direct: true} // the local server, whatever the environment says
	}
	if r.plan.ws {
		cfg.Endpoint = "ws" + strings.TrimPrefix(cfg.Endpoint, "http")
	}
	// The offers' notifications the model expects, in order.
	var want []handed
	savedFirst := false
	if s.stateFile {
		cfg.StateFile = filepath.Join(r.dir, "state.bin")
		if r.saved != "" {
			// RULE-SHELL-01: the saved config is handed over in the first step, held until Start
			// has returned.
			want = append(want, handed{RemoteConfig, r.saved})
			savedFirst = true
			r.stats.savedStarts++
		}
	}
	m := &mClient{armed: map[string]func(*Client){}, stopInside: make(chan error, 1)}
	c, err := Start(context.Background(), cfg, Status{Description: modelDesc(mOutside, 0),
		CustomCapabilities: &protobufs.CustomCapabilities{Capabilities: []string{modelCapability}}},
		func(_ *Client, n Notification) { m.notify(n) })
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	m.client.Store(c)
	m.started.Store(true)
	defer func() {
		m.mu.Lock()
		r.stats.early += m.early
		m.mu.Unlock()
	}()
	// The other goroutine: one caller, its calls in order.
	work := make(chan func(), 64)
	workDone := make(chan struct{})
	go func() {
		defer close(workDone)
		for f := range work {
			f()
		}
	}()
	defer func() { close(work); <-workDone }()

	heartbeat := modelHeartbeat
	var outDesc, goDesc int64
	panicked := false

	// check: the model holds so far.
	check := func(where string) error {
		if err := m.contract(where, panicked); err != nil {
			return err
		}
		got := m.handedSoFar()
		for i := range min(len(got), len(want)) {
			if i == 0 && savedFirst && got[0].kind == RemoteConfig && r.savedOr[got[0].key] {
				continue
			}
			if got[i] != want[i] {
				return fmt.Errorf("after %s: handed over %v, the model expects %v", where, got, want)
			}
		}
		if len(got) > len(want) {
			return fmt.Errorf("after %s: handed over %v, more than the model expects %v", where, got, want)
		}
		return nil
	}
	// await: every offer made so far has been handed over (or the client stopped).
	await := func(where string) error {
		deadline := time.Now().Add(awaitBound)
		for {
			if err := check(where); err != nil {
				return err
			}
			_, stopped, _, _ := m.snapshot()
			if len(m.handedSoFar()) >= len(want) || stopped > 0 {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("after %s: handed over %v within %v, the model expects %v", where, m.handedSoFar(), awaitBound, want)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	// offer: the server offers m; over HTTP a call makes the client poll now.
	offer := func(msg *protobufs.ServerToAgent, h handed) {
		want = append(want, h)
		r.srv.offer(msg)
		if !r.plan.ws {
			c.SetHealth(&protobufs.ComponentHealth{Healthy: true, Status: "nudge"})
		}
	}
	offerCustomMsg := func(arm func(*Client)) {
		r.cust++
		data := "m" + strconv.Itoa(r.cust)
		if arm != nil {
			m.mu.Lock()
			m.armed[data] = arm
			m.mu.Unlock()
		}
		offer(&protobufs.ServerToAgent{CustomMessage: &protobufs.CustomMessage{Capability: modelCapability, Type: "m",
			Data: []byte(data)}}, handed{CustomMessage, data})
	}
	if err := await("start"); err != nil {
		return err
	}

	for i, st := range s.steps {
		where := fmt.Sprintf("step %d (%v)", i+1, st)
		switch st.kind {
		case mCall:
			switch st.who {
			case mOutside:
				if st.describe {
					outDesc++
					c.SetDescription(modelDesc(mOutside, outDesc))
				} else {
					c.SetHealth(&protobufs.ComponentHealth{Healthy: true, Status: "out"})
				}
			case mGoroutine:
				if st.describe {
					goDesc++
					n := goDesc
					work <- func() { c.SetDescription(modelDesc(mGoroutine, n)) }
				} else {
					work <- func() { c.SetHealth(&protobufs.ComponentHealth{Healthy: true, Status: "go"}) }
				}
			case mInside:
				describe := st.describe
				offerCustomMsg(func(c *Client) {
					if describe {
						m.cbDesc++
						c.SetDescription(modelDesc(mInside, m.cbDesc))
					} else {
						c.SetHealth(&protobufs.ComponentHealth{Healthy: true, Status: "cb"})
					}
				})
			}
		case mOffer:
			switch st.offer {
			case offerConfig:
				r.hash++
				h := "c" + strconv.Itoa(r.hash)
				offer(&protobufs.ServerToAgent{RemoteConfig: &protobufs.AgentRemoteConfig{ConfigHash: []byte(h),
					Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
						"": {Body: []byte("n: " + strconv.Itoa(r.hash)), ContentType: "text/yaml"}}}}},
					handed{RemoteConfig, h})
			case offerCustom:
				offerCustomMsg(nil)
			case offerHeartbeat:
				// Always a value other than the one in force, so that it is announced. Over HTTP, an
				// offer of 0 falls back to the configured interval (the poll can't be off).
				var secs uint64
				next := time.Duration(0)
				if r.plan.ws {
					options := []uint64{}
					for _, v := range []uint64{0, 1, 2} {
						if time.Duration(v)*time.Second != heartbeat {
							options = append(options, v)
						}
					}
					secs = options[st.k%len(options)]
					next = time.Duration(secs) * time.Second
				} else if heartbeat != modelHeartbeat {
					secs, next = 0, modelHeartbeat
				} else {
					secs = 1 + uint64(st.k%2)
					next = time.Duration(secs) * time.Second
				}
				heartbeat = next
				offer(&protobufs.ServerToAgent{ConnectionSettings: &protobufs.ConnectionSettingsOffers{
					Opamp: &protobufs.OpAMPConnectionSettings{HeartbeatIntervalSeconds: secs}}},
					handed{HeartbeatInterval, next.String()})
			}
		case mPanic:
			panicked = true
			m.mu.Lock()
			m.panicAt = m.calls + st.k
			m.mu.Unlock()
			// Callbacks until the k-th: custom messages, one at a time.
			for range st.k {
				offerCustomMsg(nil)
				if err := await(where); err != nil {
					return err
				}
			}
		}
		if err := await(where); err != nil {
			return err
		}
		if panicked {
			break
		}
	}

	// Stop.
	stopErr := error(nil)
	if panicked {
		// RULE-SHELL-02: the client stopped on its own, and said so once, with the error.
		deadline := time.Now().Add(awaitBound)
		for {
			if _, stopped, _, _ := m.snapshot(); stopped > 0 {
				break
			}
			if time.Now().After(deadline) {
				return errors.New("RULE-SHELL-02: the callback panicked, and no stopped within " + awaitBound.String())
			}
			time.Sleep(2 * time.Millisecond)
		}
		_, _, stopErr, _ = m.snapshot()
		if stopErr == nil || !strings.Contains(stopErr.Error(), modelPanic) {
			return fmt.Errorf("RULE-SHELL-02: the callback panicked, stopped with %v", stopErr)
		}
	} else if s.stopInside {
		// RULE-SHELL-03: Stop called from the callback asks for the stop and returns at once
		// (the client is waiting for the callback), before stopped; the client stops after.
		offerCustomMsg(func(c *Client) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*stopBound)
			defer cancel()
			t0 := time.Now()
			err := c.Stop(ctx)
			if took := time.Since(t0); err == nil && took > awaitBound {
				err = fmt.Errorf("it took %v", took)
			}
			m.mu.Lock()
			if err == nil && m.stopped > 0 {
				err = errors.New("stopped was told before it returned")
			}
			m.mu.Unlock()
			m.stopInside <- err
		})
		// The custom message itself may be handed over after the stop began: no await here.
		select {
		case err := <-m.stopInside:
			if err != nil {
				return fmt.Errorf("RULE-SHELL-03: Stop called from the callback: %v", err)
			}
		case <-time.After(awaitBound + stopBound):
			return errors.New("RULE-SHELL-03: Stop called from the callback never returned")
		}
	}
	// From outside (again, after a stop from inside or a panic: it gives the same result).
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*stopBound)
	err = c.Stop(ctx)
	cancel()
	if took := time.Since(t0); took > stopBound {
		return fmt.Errorf("RULE-SHELL-07: Stop took %v, over %v", took, stopBound)
	}
	if err != stopErr {
		return fmt.Errorf("RULE-SHELL-02/07: Stop returned %v, want %v", err, stopErr)
	}
	notes, stopped, _, _ := m.snapshot()
	if stopped != 1 {
		return fmt.Errorf("RULE-SHELL-02: Stop returned with stopped seen %d times: %v", stopped, kinds(notes))
	}
	// Calls after stop do nothing, and nothing calls back.
	for range s.callsAfter {
		c.SetHealth(&protobufs.ComponentHealth{Healthy: false, Status: "after"})
		done := make(chan struct{})
		work <- func() { c.SetDescription(modelDesc(mGoroutine, 999_999)); close(done) }
		select {
		case <-done:
		case <-time.After(time.Second):
			return errors.New("RULE-SHELL-03: a call after stop blocked")
		}
	}
	time.Sleep(20 * time.Millisecond)
	if err := check("stop"); err != nil {
		return err
	}
	if err := c.Stop(context.Background()); err != stopErr {
		return fmt.Errorf("RULE-SHELL-02: Stop again returned %v, want %v", err, stopErr)
	}
	// Every offer was handed over before the stop (we awaited each), except the custom message
	// that carried a stop requested from inside, which may have come after it began.
	got := m.handedSoFar()
	if s.stopInside && len(got) == len(want)-1 {
		want = want[:len(want)-1]
	}
	if !panicked && len(got) != len(want) {
		return fmt.Errorf("handed over %v, the model expects %v", got, want)
	}
	// RULE-SHELL-03: each caller's descriptions reach the server in that caller's order.
	last := map[int64]int64{}
	for _, d := range r.srv.descriptions() {
		who, n := d/1_000_000, d%1_000_000
		if n < last[who] {
			return fmt.Errorf("RULE-SHELL-03: caller %v's descriptions arrived out of order: %v", mWho(who), r.srv.descriptions())
		}
		last[who] = n
	}
	// What the state file holds now: the last config handed over in a session on it.
	if s.stateFile && !panicked {
		or := map[string]bool{}
		if r.saved != "" {
			or[r.saved] = true
		}
		for _, h := range got {
			if h.kind == RemoteConfig {
				or[h.key] = true
				r.saved = h.key
			}
		}
		r.savedOr = or
	}
	return nil
}

func kinds(notes []Notification) []NotificationKind {
	out := make([]NotificationKind, len(notes))
	for i, n := range notes {
		out[i] = n.Kind
	}
	return out
}

func modelEnv(t *testing.T) (runs int, seed uint64) {
	runs = 30
	if v := os.Getenv("OPAMP_MODEL_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("OPAMP_MODEL_RUNS: %v", err)
		}
		runs = n
	}
	seed = uint64(time.Now().UnixNano())
	if v := os.Getenv("OPAMP_MODEL_SEED"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("OPAMP_MODEL_SEED: %v", err)
		}
		seed = n
	}
	return runs, seed
}

// Each plan runs in Go mode and in Rust mode (the core's own HTTP and WebSocket over Go's
// connections): the callback contract is the shell's, whoever speaks HTTP.
//
// covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-07
func TestCallbackContractModel(t *testing.T) {
	for _, mode := range []Mode{ModeGo, ModeRust} {
		t.Run(modeName(mode), func(t *testing.T) { testCallbackContractModel(t, mode) })
	}
}

func testCallbackContractModel(t *testing.T, mode Mode) {
	runs, seed := modelEnv(t)
	var total modelStats
	for i := range runs {
		plan := genModelPlan(rand.New(rand.NewPCG(seed, uint64(i))), !panicKillsProcess)
		stats, err := runModelPlan(plan, t.TempDir(), mode)
		total.savedStarts += stats.savedStarts
		total.early += stats.early
		if err != nil {
			t.Fatalf("run %d (replay: OPAMP_MODEL_SEED=%d), plan %v:\n%v", i, seed, plan, err)
		}
	}
	t.Logf("seed %d: %d runs, %d sessions started with a saved remote config", seed, runs, total.savedStarts)
	if total.early > 0 {
		t.Logf("known break S5, tolerated: %d callbacks came before Start returned (seed %d)", total.early, seed)
	}
}

// A panic in the callback must stop the client (stopped with the error, once; Stop returns it),
// not the process. Go has no recover there: the process dies. Each run goes to a child
// process (this test binary again), and is expected to fail that way; when one passes, the bug
// is fixed: set panicKillsProcess to false.
//
// covers: RULE-SHELL-02, RULE-SHELL-07
func TestCallbackModelPanicStopsTheClient(t *testing.T) {
	const childEnv = "OPAMP_MODEL_PANIC_CHILD"
	if v := os.Getenv(childEnv); v != "" {
		seed, run, _ := strings.Cut(v, ":")
		s, _ := strconv.ParseUint(seed, 10, 64)
		i, _ := strconv.ParseUint(run, 10, 64)
		plan := genModelPlan(rand.New(rand.NewPCG(s, i)), true)
		if _, err := runModelPlan(plan, t.TempDir(), ModeGo); err != nil {
			t.Fatalf("plan %v:\n%v", plan, err)
		}
		return
	}
	if !panicKillsProcess {
		t.Skip("panics run in-process in TestCallbackContractModel")
	}
	runs, seed := modelEnv(t)
	runs = max(1, min(3, runs/10))
	for i := range runs {
		plan := genModelPlan(rand.New(rand.NewPCG(seed, uint64(i))), true)
		cmd := exec.Command(os.Args[0], "-test.run=^TestCallbackModelPanicStopsTheClient$", "-test.count=1")
		cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d:%d", childEnv, seed, i))
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		switch {
		case err == nil:
			t.Fatalf("looks fixed: a panic in the callback no longer kills the process (OPAMP_MODEL_SEED=%d, run %d, plan %v). "+
				"Set panicKillsProcess to false.", seed, i, plan)
		case errors.As(err, &exit) && strings.Contains(string(out), "panic: "+modelPanic):
			t.Logf("expected failure (RULE-SHELL-02): the callback's panic killed the process (OPAMP_MODEL_SEED=%d, run %d)", seed, i)
		default:
			t.Fatalf("run %d (replay: OPAMP_MODEL_SEED=%d, %s=%d:%d), plan %v: failed otherwise: %v\n%s",
				i, seed, childEnv, seed, i, plan, err, out)
		}
	}
}
