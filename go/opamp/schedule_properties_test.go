// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

// Schedules: rapid state machines over the real client, each case in a testing/synctest
// bubble on the in-memory network (pipenet_test.go). Time is virtual, so a heartbeat, a timeout, a
// Retry-After of an hour or the stop bound costs nothing; and the bubble fails a case that leaves
// a goroutine behind. A case starts a client (HTTP or WebSocket, timeouts drawn) on a state file
// with packages on, against a server that either answers everything at once ("auto") or holds
// every request, upgrade and download until an action decides it ("manual"). Then rapid draws
// actions:
//   - the application's calls, from outside, from another goroutine, and from inside the callback
//     (one call, or a burst of up to 100 while the client waits for the callback);
//   - the server answers, holds for a while (to just before, at, or just after the effect's
//     deadline), drops the connection, refuses (503), throttles (429 with Retry-After) or answers a
//     download with the wrong bytes; closes or drops an open WebSocket; offers a remote config, a
//     custom message, a heartbeat interval or a package;
//   - time advances (1 ms to an hour), a save is slow, the callback panics;
//   - stop, from outside, from another goroutine while the actions go on, or from the callback;
//   - a race: two or three of those back to back, without letting the client settle between.
//
// After every action, settled (synctest.Wait): the callback contract (the callback model's
// mClient, RULE-SHELL-01/02), and the properties test's checks over what the server got, as far as
// they hold so far (each caller's custom messages in its order, the goodbye last, nothing after
// Stop returned; with the auto server, each at most once and sequence numbers without a gap).
// At the end, unless stopped: every request answered from then on, every custom message sent
// arrives and the server ends with the last description (with the auto server, exactly once and
// without a gap: the properties test's checks in full). Then Stop, if no action did:
//   - Stop returns within StopTimeout plus a margin (virtual time), with nil, or with the panic;
//   - when it returns, no connection the server is still waiting on is open at the client's end;
//   - with the application's idle connections closed: no pipe the client dialled is open, and no
//     goroutine of the client is left (but a save already writing, which finishes by itself);
//   - nothing reaches the server after, no dial; a WebSocket that said goodbye did the closing
//     handshake; calls after stop do nothing; Stop again says the same;
//   - the core tap's record of the case (OPAMP_CORE_TAP) holds to RULE-SHELL-04/06/07: every
//     input about an effect names one the core issued, of its kind, at most one end each, nothing
//     after the core cancelled it, and nothing after the output that said stopped;
//   - and to RULE-SHELL-10: every call the callback made (the test's numbered ones, and its
//     answers to remote configs) is the next thing the core was fed after the step that called the
//     callback, but for the application's calls queued ahead of it (checkCallbackCalls).
//
// Per PR: 200 cases (the -rapid.checks default here). Nightly: -rapid.checks=10000. The base seed
// is logged; a failure prints rapid's shrunk action list and its seed: replay with
// go test -run 'TestSchedules$' ./opamp/ -rapid.seed=SEED (add -rapid.v for every action).
// Goroutines inside a bubble still run in parallel, so a seed replays the actions, not every
// interleaving between them.
//
// covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-04, RULE-SHELL-06, RULE-SHELL-07, RULE-SHELL-10

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"pgregory.net/rapid"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

const (
	schedChecks = 200 // cases per run, unless -rapid.checks or RAPID_CHECKS says
	// schedStopMargin: Stop returns within StopTimeout plus this (RULE-SHELL-07's margin).
	schedStopMargin = time.Second
	schedHeal       = 3 * time.Hour // the most the end of a case waits for everything to arrive
)

// schedStats counts what the cases exercised, for the log.
var schedStats = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

func schedCount(what string) {
	schedStats.Lock()
	schedStats.n[what]++
	schedStats.Unlock()
}

// Callers of the application's calls.
const (
	cOutside = iota
	cGoroutine
	cCallback
)

// The server.

type exKind int

const (
	exPost exKind = iota
	exUpgrade
	exDownload
)

func (k exKind) String() string { return [...]string{"post", "upgrade", "download"}[k] }

type verdict int

const (
	vOK       verdict = iota // answer: the offers; accept the upgrade; the file
	vWrong                   // a download: other bytes
	vNotFound                // 404
	v503                     // refuse
	v429                     // throttle, with Retry-After
	vDrop                    // drop the connection
)

func (v verdict) String() string {
	return [...]string{"ok", "wrong-bytes", "404", "503", "429", "drop"}[v]
}

type decision struct {
	v          verdict
	delay      time.Duration
	retryAfter int // seconds (v429)
}

// exchange is a request, upgrade or download the manual server holds until an action decides it.
type exchange struct {
	kind     exKind
	path     string
	end      *pipeEnd // the server's end of its connection
	arrived  time.Time
	decide   chan decision
	answered atomic.Bool // the server is writing its answer (or has gone)
}

// arrival is an AgentToServer as the server got it.
type arrival struct {
	m  *protobufs.AgentToServer
	at time.Time
	ws int // the WebSocket connection's number, or 0 over HTTP
}

// serverWS is the server's side of a WebSocket connection.
type serverWS struct {
	n   int
	c   *websocket.Conn
	wmu chan struct{} // one writer at a time (a channel: durably blocking in a bubble)
	end *pipeEnd
	// Under the server's mu:
	faulted bool   // the server closed or dropped it
	goodbye bool   // the client's goodbye came on it
	ended   string // "": open; "close": the client's Close frame came; "abrupt": gone without one
}

func (sw *serverWS) write(b []byte) {
	sw.wmu <- struct{}{}
	defer func() { <-sw.wmu }()
	_ = sw.c.WriteMessage(websocket.BinaryMessage, b)
}

type schedServer struct {
	mu      sync.Mutex
	manual  bool
	pending []*exchange // held, oldest first
	active  map[*exchange]bool
	offers  []*protobufs.ServerToAgent
	got     []arrival
	conns   []*serverWS
	files   map[string][]byte // download path: the package file
	quit    chan struct{}
	healed  chan struct{} // closed at heal: what is answered late is answered now
	once    sync.Once
}

func newSchedServer(manual bool) *schedServer {
	return &schedServer{manual: manual, active: map[*exchange]bool{}, files: map[string][]byte{}, quit: make(chan struct{}),
		healed: make(chan struct{})}
}

func (s *schedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/d/"):
		s.download(w, r)
	case websocket.IsWebSocketUpgrade(r):
		s.upgrade(w, r)
	default:
		s.post(w, r)
	}
}

// await is the decision on r: at once (auto), or when an action makes it (manual), after its
// delay. false: the client went (or the test ended) first.
func (s *schedServer) await(kind exKind, r *http.Request) (*exchange, decision, bool) {
	ex := &exchange{kind: kind, path: r.URL.Path, end: pipeOf(r), arrived: time.Now(), decide: make(chan decision, 1)}
	s.mu.Lock()
	if !s.manual {
		s.mu.Unlock()
		ex.answered.Store(true)
		return ex, decision{}, true
	}
	s.pending = append(s.pending, ex)
	s.active[ex] = true
	s.mu.Unlock()
	defer func() {
		ex.answered.Store(true)
		s.mu.Lock()
		delete(s.active, ex)
		s.mu.Unlock()
	}()
	var d decision
	select {
	case d = <-ex.decide:
	case <-r.Context().Done():
		s.forget(ex)
		return ex, d, false
	case <-s.quit:
		s.forget(ex)
		return ex, d, false
	}
	if d.delay > 0 {
		t := time.NewTimer(d.delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-s.healed:
		case <-r.Context().Done():
			return ex, d, false
		case <-s.quit:
			return ex, d, false
		}
	}
	return ex, d, true
}

func (s *schedServer) forget(ex *exchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.pending {
		if p == ex {
			s.pending = append(s.pending[:i:i], s.pending[i+1:]...)
			return
		}
	}
}

// claim takes ex off the held exchanges, if it is still held.
func (s *schedServer) claim(ex *exchange) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.pending {
		if p == ex {
			s.pending = append(s.pending[:i:i], s.pending[i+1:]...)
			return true
		}
	}
	return false
}

func (s *schedServer) held() []*exchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*exchange(nil), s.pending...)
}

// heal answers everything from now on, and what is held now.
func (s *schedServer) heal() {
	s.mu.Lock()
	s.manual = false
	held := s.pending
	s.pending = nil
	s.mu.Unlock()
	s.once.Do(func() { close(s.healed) })
	for _, ex := range held {
		ex.decide <- decision{}
	}
}

// refuse answers with d's failure.
func (s *schedServer) refuse(w http.ResponseWriter, d decision) {
	switch d.v {
	case v503:
		w.WriteHeader(http.StatusServiceUnavailable)
	case v429:
		w.Header().Set("Retry-After", strconv.Itoa(d.retryAfter))
		w.WriteHeader(http.StatusTooManyRequests)
	case vNotFound:
		w.WriteHeader(http.StatusNotFound)
	case vDrop:
		if c, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = c.Close()
		}
	default:
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (s *schedServer) record(m *protobufs.AgentToServer, ws int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, arrival{m: m, at: time.Now(), ws: ws})
}

func (s *schedServer) arrivals() []arrival {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]arrival(nil), s.got...)
}

func (s *schedServer) messages() []*protobufs.AgentToServer {
	var out []*protobufs.AgentToServer
	for _, a := range s.arrivals() {
		out = append(out, a.m)
	}
	return out
}

// reply is the answer to m, with every offer waiting.
func (s *schedServer) reply(m *protobufs.AgentToServer) []byte {
	s.mu.Lock()
	offers := s.offers
	s.offers = nil
	s.mu.Unlock()
	out := &protobufs.ServerToAgent{}
	for _, o := range offers {
		proto.Merge(out, o)
	}
	out.InstanceUid = m.InstanceUid
	b, _ := proto.Marshal(out)
	return b
}

func (s *schedServer) post(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	m := &protobufs.AgentToServer{}
	if err != nil || proto.Unmarshal(body, m) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.record(m, 0)
	_, d, ok := s.await(exPost, r)
	if !ok {
		return
	}
	if d.v != vOK {
		s.refuse(w, d)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(s.reply(m))
}

func (s *schedServer) download(w http.ResponseWriter, r *http.Request) {
	_, d, ok := s.await(exDownload, r)
	if !ok {
		return
	}
	schedCount("server: download " + d.v.String())
	switch d.v {
	case vOK:
		s.mu.Lock()
		f := s.files[r.URL.Path]
		s.mu.Unlock()
		_, _ = w.Write(f)
	case vWrong:
		_, _ = w.Write([]byte("not the package you are looking for"))
	default:
		s.refuse(w, d)
	}
}

func (s *schedServer) upgrade(w http.ResponseWriter, r *http.Request) {
	ex, d, ok := s.await(exUpgrade, r)
	if !ok {
		return
	}
	if d.v != vOK {
		s.refuse(w, d)
		return
	}
	c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	schedCount("server: websocket accepted")
	sw := &serverWS{c: c, wmu: make(chan struct{}, 1), end: ex.end}
	s.mu.Lock()
	sw.n = len(s.conns) + 1
	s.conns = append(s.conns, sw)
	pushNow := len(s.offers) > 0
	s.mu.Unlock()
	defer c.Close()
	if pushNow {
		go sw.write(append([]byte{0}, s.reply(&protobufs.AgentToServer{InstanceUid: modelUID[:]})...))
	}
	for {
		kind, data, err := c.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			s.mu.Lock()
			// gorilla reports a connection gone without a Close frame as a CloseError too, with
			// 1006 (abnormal closure).
			if errors.As(err, &ce) && ce.Code != websocket.CloseAbnormalClosure {
				sw.ended = "close"
			} else {
				sw.ended = "abrupt"
			}
			s.mu.Unlock()
			return
		}
		if kind != websocket.BinaryMessage || len(data) == 0 {
			continue
		}
		m := &protobufs.AgentToServer{}
		if proto.Unmarshal(data[1:], m) != nil {
			continue
		}
		s.record(m, sw.n)
		if m.AgentDisconnect != nil {
			s.mu.Lock()
			sw.goodbye = true
			s.mu.Unlock()
		}
		sw.write(append([]byte{0}, s.reply(m)...))
	}
}

// openConn is the newest WebSocket connection still open, or nil.
func (s *schedServer) openConn() *serverWS {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.conns) - 1; i >= 0; i-- {
		if s.conns[i].ended == "" && !s.conns[i].end.closed.Load() {
			return s.conns[i]
		}
	}
	return nil
}

// offer queues m for the next answer; over WebSocket it goes now, on the open connection.
func (s *schedServer) offer(m *protobufs.ServerToAgent) {
	s.mu.Lock()
	s.offers = append(s.offers, m)
	s.mu.Unlock()
	if sw := s.openConn(); sw != nil {
		go sw.write(append([]byte{0}, s.reply(&protobufs.AgentToServer{InstanceUid: modelUID[:]})...))
	}
}

// The case.

type schedCall struct {
	describe bool
}

type stopResult struct {
	err  error
	took time.Duration
	// open: what the server was still waiting on, with the client's end still open, the moment
	// Stop returned; and then, the client's goroutines (what holds it open).
	open       []string
	goroutines []string
}

type schedCase struct {
	base   string
	dir    string
	tap    string
	net    *pipeNet
	srv    *schedServer
	hs     *http.Server
	tr     *http.Transport
	cfg    Config
	ws     bool
	manual bool
	m      *mClient
	c      *Client
	work   chan func()

	callTurn chan struct{} // one call at a time
	mu       sync.Mutex
	calls    int           // the application's calls made, numbered from 1
	sends    map[int][]int // caller: the custom messages it made, in order (before any stop)
	lastDesc int           // the last description made (before any stop)
	notSent  map[int]bool  // custom messages the client said it won't send
	cbQueue  []schedCall   // calls the callback makes on its next notification
	cbStop   bool          // the callback requests stop on its next notification
	told     int           // notifications the callback has been called with
	cbCalls  []cbCall      // the calls the callback made
	problems []string      // what the callback found wrong
	nOffers  int
	maxSave  time.Duration

	stopAsked bool
	stopRes   chan stopResult
	stopped   bool // Stop returned, and was checked
	panicked  bool // the callback was told to panic
	faults    int  // what the server did that may lose an answer (manual server)
	healed    bool // stopped by finish, everything answered and arrived
	log       []string
}

// saveDelays: the slow saves each case asks for, by state file (beforeSave is global).
var saveDelays sync.Map // state file -> time.Duration

func schedBeforeSave(path string) {
	if d, ok := saveDelays.Load(path); ok && d.(time.Duration) > 0 {
		time.Sleep(d.(time.Duration))
	}
}

func (sc *schedCase) note(format string, args ...any) {
	sc.mu.Lock()
	sc.log = append(sc.log, fmt.Sprintf(format, args...))
	sc.mu.Unlock()
	if !strings.HasPrefix(format, "%") {
		what, _, _ := strings.Cut(format, "(")
		what, _, _ = strings.Cut(what, " %")
		what, _, _ = strings.Cut(what, ":")
		schedCount(what)
	}
}

// call makes one application call, on the caller's goroutine. Calls take turns (callTurn), so that
// lastDesc is the last description made; the turn is a channel, not a mutex, because a call that
// blocks (a bug: a call never blocks) must leave the bubble durably blocked, which the bubble
// reports, where a goroutine waiting on a mutex would hang it.
func (sc *schedCase) call(who int, describe bool) {
	sc.callTurn <- struct{}{}
	defer func() { <-sc.callTurn }()
	sc.mu.Lock()
	sc.calls++
	n := sc.calls
	if !sc.stopAsked {
		if describe {
			sc.lastDesc = n
		} else {
			sc.sends[who] = append(sc.sends[who], n)
		}
	}
	if who == cCallback {
		sc.cbCalls = append(sc.cbCalls, numberedCall(sc.told, n, describe))
	}
	sc.mu.Unlock()
	if describe {
		sc.c.SetDescription(propDescription(n))
	} else {
		sc.c.SendCustomMessage(&protobufs.CustomMessage{Capability: propCapability, Type: "t", Data: []byte(strconv.Itoa(n))})
	}
}

// onNotify is the application's part of the callback (mClient.also): it answers packages, notes
// what the client won't send, makes the calls queued for it, and requests stop if asked.
func (sc *schedCase) onNotify(n Notification) {
	schedCount(fmt.Sprintf("notified %d", n.Kind))
	sc.mu.Lock()
	sc.told++
	sc.mu.Unlock()
	c := sc.m.client.Load()
	switch n.Kind {
	case InstallPackage:
		want := sc.srv.fileFor(n.Package)
		if got, err := os.ReadFile(n.Path); err != nil || string(got) != string(want) {
			sc.mu.Lock()
			sc.problems = append(sc.problems, fmt.Sprintf("the file handed over (%s) holds %q (%v), the server sent %q", n.Path, got, err, want))
			sc.mu.Unlock()
		}
		if c != nil {
			c.PackageInstalled(n.PackageName, n.Package.GetHash(), nil)
		}
	case RemovePackage:
		if c != nil {
			c.PackageRemoved(n.PackageName, nil)
		}
	case CustomMessageNotSent:
		if k, err := strconv.Atoi(string(n.CustomMessage.GetData())); err == nil {
			sc.mu.Lock()
			sc.notSent[k] = true
			sc.mu.Unlock()
		}
	}
	if n.Kind == Stopped || c == nil {
		return
	}
	sc.mu.Lock()
	queue := sc.cbQueue
	sc.cbQueue = nil
	stop := sc.cbStop
	sc.cbStop = false
	sc.mu.Unlock()
	for _, q := range queue {
		sc.call(cCallback, q.describe)
	}
	if stop {
		// Stop from the callback asks for the stop and returns at once, before stopped (the
		// client waits for the callback); then wait for the client on a goroutine, as from
		// outside.
		if err := c.Stop(context.Background()); err != nil {
			sc.mu.Lock()
			sc.problems = append(sc.problems, fmt.Sprintf("RULE-SHELL-03: Stop from the callback returned %v", err))
			sc.mu.Unlock()
		}
		if _, stopped, _, _ := sc.m.snapshot(); stopped > 0 {
			sc.mu.Lock()
			sc.problems = append(sc.problems, "RULE-SHELL-03: Stop from the callback returned after stopped was told")
			sc.mu.Unlock()
		}
		go sc.runStop(c)
	}
}

func (s *schedServer) fileFor(p *protobufs.PackageAvailable) []byte {
	u := p.GetFile().GetDownloadUrl()
	i := strings.Index(u, "/d/")
	if i < 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files[u[i:]]
}

// runStop calls Stop and reports how it went.
func (sc *schedCase) runStop(c *Client) {
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	err := c.Stop(ctx)
	res := stopResult{err: err, took: time.Since(t0), open: sc.stillOpen()}
	if len(res.open) > 0 {
		res.goroutines = bubbleClientGoroutines(true)
	}
	sc.stopRes <- res
}

// stillOpen: what the server is still waiting on (a held request, upgrade or download it hasn't
// answered, an open WebSocket) whose client end is open. Nothing is, the moment Stop returns
// (RULE-SHELL-07).
func (sc *schedCase) stillOpen() []string {
	sc.srv.mu.Lock()
	defer sc.srv.mu.Unlock()
	var out []string
	for ex := range sc.srv.active {
		if !ex.answered.Load() && ex.end != nil && !ex.end.peer.closed.Load() {
			out = append(out, fmt.Sprintf("a held %v of %s", ex.kind, ex.path))
		}
	}
	for _, sw := range sc.srv.conns {
		if sw.end != nil && !sw.end.peer.closed.Load() {
			out = append(out, fmt.Sprintf("WebSocket %d", sw.n))
		}
	}
	return out
}

// askStop begins a stop, as how says.
func (sc *schedCase) askStop(how string) {
	sc.mu.Lock()
	if sc.stopAsked {
		sc.mu.Unlock()
		return
	}
	sc.stopAsked = true
	if how == "callback" {
		sc.cbStop = true
	}
	sc.mu.Unlock()
	sc.note("stop (%s)", how)
	switch how {
	case "outside":
		sc.runStop(sc.c)
	case "goroutine":
		go sc.runStop(sc.c)
	}
}

func (sc *schedCase) fail(rt *rapid.T, format string, args ...any) {
	rt.Helper()
	sc.mu.Lock()
	log := strings.Join(sc.log, "; ")
	sc.mu.Unlock()
	rt.Fatalf("%s\n(case: %s mode, %s, manual server %v, timeouts request %v connect %v stop %v download %v, heartbeat %v; did: %s)\nthe core's last steps:\n%s",
		fmt.Sprintf(format, args...), modeName(sc.cfg.Mode), map[bool]string{true: "websocket", false: "http"}[sc.ws], sc.manual,
		sc.cfg.RequestTimeout, sc.cfg.ConnectTimeout, sc.cfg.StopTimeout, sc.cfg.DownloadTimeout, sc.cfg.HeartbeatInterval, log,
		tapTail(sc.dir, 12))
}

// check runs after every action, once the client has settled.
func (sc *schedCase) check(rt *rapid.T) {
	rt.Helper()
	synctest.Wait()
	// A callback that panicked stopped the client on its own: Stop says so.
	if _, stopped, _, _ := sc.m.snapshot(); stopped > 0 && !sc.stopAsked {
		if !sc.panicked {
			sc.fail(rt, "the client stopped on its own: %v", kinds(sc.notes()))
		}
		sc.askStop("outside")
	}
	select {
	case res := <-sc.stopRes:
		sc.afterStop(rt, res)
	default:
	}
	sc.contract(rt, "an action")
	sc.mu.Lock()
	problems := append([]string(nil), sc.problems...)
	sc.mu.Unlock()
	if len(problems) > 0 {
		sc.fail(rt, "%v", problems)
	}
	if err := sc.safety(); err != nil {
		sc.fail(rt, "%v", err)
	}
}

// contract: the callback model's checks (mClient.contract), and the core's own promise that
// nothing is notified after stopped in the output that says it (the shell also tells stopped
// last within each output, TestScheduleStoppedIsTheLastNotification).
func (sc *schedCase) contract(rt *rapid.T, where string) {
	rt.Helper()
	if late := tapNotifiesAfterStopped(sc.dir); late > 0 {
		sc.fail(rt, "%s: the core put %d notification(s) after notify(stopped) in one output", where, late)
	}
	if err := sc.m.contract(where, sc.panicked); err != nil {
		sc.fail(rt, "%v", err)
	}
}

func (sc *schedCase) notes() []Notification {
	notes, _, _, _ := sc.m.snapshot()
	return notes
}

func (sc *schedCase) sendsCopy() map[int][]int {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	out := map[int][]int{}
	for k, v := range sc.sends {
		out[k] = append([]int(nil), v...)
	}
	return out
}

// safety: the properties test's checks, as far as they hold at any moment.
func (sc *schedCase) safety() error {
	got := sc.srv.messages()
	del := delivered(got)
	if err := checkCallerOrder(del, sc.sendsCopy()); err != nil {
		return fmt.Errorf("RULE-SHELL-03: %w", err)
	}
	for i, m := range got {
		if m.AgentDisconnect != nil && i != len(got)-1 {
			return fmt.Errorf("the goodbye is message %d of %d", i, len(got))
		}
	}
	if !sc.lossy() {
		seen := map[int]bool{}
		for _, n := range del {
			if seen[n] {
				return fmt.Errorf("custom message %d arrived twice, and the server lost nothing (%d delivered)", n, len(del))
			}
			seen[n] = true
		}
		if err := checkSequence(got); err != nil {
			return fmt.Errorf("the server lost nothing: %w", err)
		}
	}
	return nil
}

// afterStop: Stop returned: everything checked that holds from then on.
func (sc *schedCase) afterStop(rt *rapid.T, res stopResult) {
	rt.Helper()
	sc.stopped = true
	schedCount(fmt.Sprintf("stop took <= %v", (res.took + time.Second - 1).Truncate(time.Second)))
	if res.err != nil {
		schedCount("stopped with the panic")
	}
	bound := sc.cfg.StopTimeout + schedStopMargin
	if res.took > bound {
		sc.fail(rt, "RULE-SHELL-07: Stop took %v (virtual), over %v", res.took, bound)
	}
	// Stop returns what stopped said: the panic, if the callback panicked before the client
	// stopped (RULE-SHELL-02), else nothing.
	if _, _, stoppedErr, _ := sc.m.snapshot(); stoppedErr != nil {
		if !sc.panicked || !strings.Contains(stoppedErr.Error(), modelPanic) {
			sc.fail(rt, "stopped with %v", stoppedErr)
		}
		if res.err == nil || res.err.Error() != stoppedErr.Error() {
			sc.fail(rt, "RULE-SHELL-02: stopped with %v, Stop returned %v", stoppedErr, res.err)
		}
	} else if res.err != nil {
		sc.fail(rt, "RULE-SHELL-07: Stop returned %v, and stopped said nothing", res.err)
	}
	if len(res.open) > 0 {
		sc.fail(rt, "RULE-SHELL-07: when Stop returned (after %v, with %v), still open at the client: %v; the client's goroutines then:\n%s",
			res.took, res.err, res.open, strings.Join(res.goroutines, "\n\n"))
	}
	synctest.Wait()
	sc.contract(rt, "stop")
	if _, stopped, _, _ := sc.m.snapshot(); stopped != 1 {
		sc.fail(rt, "RULE-SHELL-02: Stop returned with stopped seen %d times: %v", stopped, kinds(sc.notes()))
	}
	// The application's transport (platform mode): its idle connections are its own to close.
	closeIdle(sc.tr)
	synctest.Wait()
	if n := sc.net.openClientEnds(); n > 0 {
		sc.fail(rt, "RULE-SHELL-07: %d connections the client dialled still open after Stop (idle ones closed); goroutines:\n%s",
			n, strings.Join(bubbleClientGoroutines(true), "\n\n"))
	}
	if gs := bubbleClientGoroutines(true); len(gs) > 0 {
		sc.fail(rt, "RULE-SHELL-07: the client's goroutines left after Stop:\n%s", strings.Join(gs, "\n\n"))
	}
	arrived, dials, notes := len(sc.srv.arrivals()), sc.net.dials(), len(sc.notes())
	// Calls after stop do nothing (and don't block).
	sc.c.SetHealth(&protobufs.ComponentHealth{Healthy: false, Status: "after"})
	sc.c.SendCustomMessage(&protobufs.CustomMessage{Capability: propCapability, Type: "after"})
	sc.work <- func() { sc.c.SetDescription(propDescription(999_999)) }
	// A save already writing finishes by itself; nothing else happens.
	time.Sleep(sc.maxSave + time.Minute)
	synctest.Wait()
	if n := len(sc.srv.arrivals()); n != arrived {
		sc.fail(rt, "RULE-SHELL-07: %d messages reached the server after Stop returned", n-arrived)
	}
	if n := sc.net.dials(); n != dials {
		sc.fail(rt, "RULE-SHELL-07: %d connections dialled after Stop returned", n-dials)
	}
	if n := len(sc.notes()); n != notes {
		sc.fail(rt, "RULE-SHELL-01: %d callbacks after Stop returned: %v", n-notes, kinds(sc.notes()))
	}
	if gs := bubbleClientGoroutines(false); len(gs) > 0 {
		sc.fail(rt, "RULE-SHELL-07: the client's goroutines still there a minute after Stop:\n%s", strings.Join(gs, "\n\n"))
	}
	sc.srv.mu.Lock()
	var noHandshake []int
	for _, sw := range sc.srv.conns {
		if sw.goodbye {
			schedCount(fmt.Sprintf("websocket goodbye: faulted=%v ended=%q", sw.faulted, sw.ended))
		}
		if sw.goodbye && !sw.faulted && sw.ended != "close" {
			noHandshake = append(noHandshake, sw.n)
		}
	}
	sc.srv.mu.Unlock()
	if len(noHandshake) > 0 {
		sc.fail(rt, "the WebSocket connections %v said goodbye and went without the closing handshake", noHandshake)
	}
	if err := sc.c.Stop(context.Background()); (err == nil) != (res.err == nil) {
		sc.fail(rt, "RULE-SHELL-02: Stop again returned %v, the first time %v", err, res.err)
	}
	if err := checkTap(sc.dir); err != nil {
		sc.fail(rt, "%v", err)
	}
	sc.mu.Lock()
	calls := append(sc.m.answers(), sc.cbCalls...)
	sc.mu.Unlock()
	if len(calls) > 0 {
		schedCount("cases with calls from the callback checked in the tap")
	}
	if err := checkCallbackCalls(sc.dir, calls); err != nil {
		sc.fail(rt, "%v", err)
	}
}

// finish ends a case: unless it stopped, everything is answered from now on and every custom
// message sent must arrive; then Stop, if nothing has asked for it.
func (sc *schedCase) finish(rt *rapid.T) {
	rt.Helper()
	synctest.Wait()
	if sc.panicked && !sc.stopAsked {
		// The callback is to panic on its next call: everything answered, it comes.
		sc.srv.heal()
		for waited := time.Duration(0); waited < schedHeal; waited += time.Minute {
			if _, stopped, _, _ := sc.m.snapshot(); stopped > 0 {
				break
			}
			time.Sleep(time.Minute)
			synctest.Wait()
		}
		if _, stopped, _, _ := sc.m.snapshot(); stopped > 0 {
			sc.askStop("outside")
		} else {
			// Nothing called back: no panic, then.
			sc.m.mu.Lock()
			sc.m.panicAt = 0
			sc.m.mu.Unlock()
			sc.panicked = false
		}
	}
	if !sc.stopAsked {
		sc.srv.heal()
		sc.note("heal")
		before := len(sc.srv.arrivals())
		arrivedAll := func() bool {
			sends := sc.sendsCopy()
			got := sc.srv.messages()
			seen := map[int]bool{}
			for _, n := range delivered(got) {
				seen[n] = true
			}
			sc.mu.Lock()
			defer sc.mu.Unlock()
			for _, ns := range sends {
				for _, n := range ns {
					if !seen[n] && !sc.notSent[n] {
						return false
					}
				}
			}
			// The last description arrived (the first message, at least), and a message since the
			// heal, answered at once: the client is connected (else the core sends no goodbye),
			// over WebSocket on a connection still up.
			d, ok := lastDescribed(got)
			return ok && d == sc.lastDesc && len(got) > before && (!sc.ws || sc.srv.openConn() != nil)
		}
		for waited := time.Duration(0); !arrivedAll() && waited < schedHeal; waited += time.Minute {
			time.Sleep(time.Minute)
			synctest.Wait()
		}
		got := sc.srv.messages()
		del := delivered(got)
		sends := sc.sendsCopy()
		sc.mu.Lock()
		lastDesc, notSent := sc.lastDesc, len(sc.notSent)
		sc.mu.Unlock()
		if !arrivedAll() {
			sc.fail(rt, "after %v of answering everything: delivered %v, sent %v; the server ended with description %v, the last set was %d",
				schedHeal, del, sends, func() int { d, _ := lastDescribed(got); return d }(), lastDesc)
		}
		if !sc.lossy() {
			// The properties test's checks, in full.
			var sent []int
			for _, ns := range sends {
				sent = append(sent, ns...)
			}
			if notSent > 0 {
				sc.fail(rt, "%d custom messages not sent, and the server lost nothing", notSent)
			}
			if err := checkOnceEach(del, sent); err != nil {
				sc.fail(rt, "%v", err)
			}
			if err := checkSequence(got); err != nil {
				sc.fail(rt, "%v", err)
			}
		}
		sc.healed = true
		sc.askStop("outside")
	}
	if sc.cbStopPending() {
		// The callback was to stop it, and wasn't called since: stop from outside.
		sc.mu.Lock()
		sc.cbStop = false
		sc.mu.Unlock()
		go sc.runStop(sc.c)
	}
	if !sc.stopped {
		sc.afterStop(rt, <-sc.stopRes) // virtual time runs on until Stop returns, or the bubble says it never will
	}
	if sc.healed {
		// Stopped while everything was answered and had arrived: the goodbye went, and last.
		if err := checkGoodbyeLast(sc.srv.messages()); err != nil {
			sc.fail(rt, "%v", err)
		}
	}
}

// lossy: the server may have lost an answer (it held one past its deadline, or failed one), so a
// message may arrive again.
func (sc *schedCase) lossy() bool { return sc.manual || sc.faults > 0 }

func (sc *schedCase) cbStopPending() bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.cbStop
}

// teardown releases everything, whether the case passed or not, so that the bubble ends.
func (sc *schedCase) teardown() {
	if !inBubble() {
		// The bubble deadlocked (the case failed with that) and rapid runs what is left of its
		// cleanups outside it: touching the bubble's channels from here is fatal. Leave it.
		return
	}
	if sc.c != nil && !sc.stopped {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		_ = sc.c.Stop(ctx)
		cancel()
	}
	if sc.work != nil {
		close(sc.work)
	}
	closeIdle(sc.tr)
	if sc.srv != nil {
		close(sc.srv.quit)
	}
	if sc.net != nil {
		sc.net.shutdown()
	}
	if sc.hs != nil {
		_ = sc.hs.Close()
	}
	if sc.dir != "" {
		saveDelays.Delete(sc.cfg.StateFile)
		_ = os.RemoveAll(sc.dir)
	}
	// A save already writing finishes by itself (time stops when the bubble's main goroutine
	// ends). The bubble fails if anything is left after, saying only that: say what.
	time.Sleep(sc.maxSave + time.Minute)
	synctest.Wait()
	if left := bubbleGoroutines(func(g string) bool { return !strings.Contains(g, "testing/synctest.") }); len(left) > 0 {
		fmt.Fprintf(os.Stderr, "schedule test: %d goroutines left in the bubble after teardown:\n%s\n", len(left), strings.Join(left, "\n\n"))
	}
}

// onPipes points cfg at the pipe network n: in platform mode through an *http.Transport over it, as
// an application passes in Config.HTTPClient (returned: its idle connections are the application's
// to close); in core mode through the streams' dialer (Config.streamNet), the core's own HTTP/1.1
// and WebSocket meeting the server's net/http and gorilla over the pipes.
func onPipes(cfg *Config, mode Mode, n *pipeNet) *http.Transport {
	cfg.Mode = mode
	if mode == ModeCore {
		cfg.Proxy = Proxy{Direct: true}
		cfg.streamNet = &streamPlatform{dial: n.DialContext}
		return nil
	}
	tr := n.transport()
	cfg.HTTPClient = &http.Client{Transport: tr}
	return tr
}

// closeIdle closes the idle connections of the application's transport, if there is one.
func closeIdle(tr *http.Transport) {
	if tr != nil {
		tr.CloseIdleConnections()
	}
}

func runSchedCase(rt *rapid.T, base string, mode Mode) {
	sc := &schedCase{base: base, sends: map[int][]int{}, notSent: map[int]bool{}, stopRes: make(chan stopResult, 2),
		callTurn: make(chan struct{}, 1)}
	rt.Cleanup(sc.teardown)
	sc.ws = rapid.Bool().Draw(rt, "websocket")
	sc.manual = rapid.IntRange(0, 2).Draw(rt, "manual") > 0
	schedCount(fmt.Sprintf("case websocket=%v manual=%v", sc.ws, sc.manual))
	pick := func(label string, ds ...time.Duration) time.Duration { return rapid.SampledFrom(ds).Draw(rt, label) }
	dir, err := os.MkdirTemp(base, "case")
	if err != nil {
		rt.Fatal(err)
	}
	sc.dir = dir
	// The core tap, for this case's core alone.
	sc.tap = filepath.Join(dir, "core.tap")
	os.Setenv("OPAMP_CORE_TAP", sc.tap)

	sc.net = newPipeNet()
	sc.srv = newSchedServer(sc.manual)
	sc.hs = &http.Server{Handler: sc.srv, ConnContext: pipeConnContext}
	go func() { _ = sc.hs.Serve(sc.net) }()
	scheme := "http"
	if sc.ws {
		scheme = "ws"
	}
	sc.cfg = Config{
		Endpoint:          scheme + "://opamp.test/v1/opamp",
		InstanceUID:       modelUID,
		Capabilities:      uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig | protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat),
		HeartbeatInterval: pick("heartbeat", 30*time.Second, 5*time.Second),
		RequestTimeout:    pick("request timeout", 30*time.Second, 2*time.Second),
		ConnectTimeout:    pick("connect timeout", 30*time.Second, 2*time.Second),
		StopTimeout:       pick("stop timeout", 5*time.Second, time.Second),
		DownloadTimeout:   pick("download timeout", 10*time.Minute, 5*time.Second),
		StateFile:         filepath.Join(dir, "state"),
		AcceptPackages:    true,
	}
	sc.tr = onPipes(&sc.cfg, mode, sc.net)
	sc.m = &mClient{armed: map[string]func(*Client){}, stopInside: make(chan error, 1)}
	sc.m.also = sc.onNotify
	sc.work = make(chan func(), 1024)
	go func() {
		for f := range sc.work {
			f()
		}
	}()
	c, err := Start(context.Background(), sc.cfg, Status{Description: propDescription(0),
		CustomCapabilities: &protobufs.CustomCapabilities{Capabilities: []string{propCapability}}},
		func(_ *Client, n Notification) { sc.m.notify(n) })
	if err != nil {
		rt.Fatalf("start: %v", err)
	}
	sc.c = c
	sc.m.client.Store(c)
	sc.m.started.Store(true)

	// rapid picks among the keys evenly: the server's answers and time count three and two.
	rt.Repeat(map[string]func(*rapid.T){
		"":         sc.check,
		"call":     sc.actCall,
		"burst":    sc.actCallbackBurst,
		"offer":    sc.actOffer,
		"answer":   sc.actAnswer,
		"answer 2": sc.actAnswer,
		"answer 3": sc.actAnswer,
		"ws":       sc.actWS,
		"time":     sc.actTime,
		"time 2":   sc.actTime,
		"save":     sc.actSlowSave,
		"stop":     sc.actStop,
		"panic":    sc.actPanic,
		"race":     sc.actRace,
	})
	sc.finish(rt)
}

func (sc *schedCase) actCall(rt *rapid.T) {
	who := rapid.IntRange(cOutside, cCallback).Draw(rt, "caller")
	describe := rapid.Bool().Draw(rt, "describe")
	sc.note("call(%d,%v)", who, describe)
	switch who {
	case cOutside:
		sc.call(cOutside, describe)
	case cGoroutine:
		sc.work <- func() { sc.call(cGoroutine, describe) }
	default:
		sc.mu.Lock()
		sc.cbQueue = append(sc.cbQueue, schedCall{describe})
		sc.mu.Unlock()
	}
}

// actCallbackBurst: many calls from the callback at once, while the client waits for it (the
// client's input buffer of 64 once deadlocked there).
func (sc *schedCase) actCallbackBurst(rt *rapid.T) {
	n := rapid.IntRange(1, 100).Draw(rt, "calls")
	sc.note("burst(%d)", n)
	sc.mu.Lock()
	for range n {
		sc.cbQueue = append(sc.cbQueue, schedCall{describe: true})
	}
	sc.mu.Unlock()
	// Something to call back with.
	sc.offerOf(1)
}

func (sc *schedCase) actOffer(rt *rapid.T) {
	sc.offerOf(rapid.IntRange(0, 3).Draw(rt, "offer"))
}

// offerOf offers a remote config (0), a custom message (1), a package (2) or a heartbeat
// interval (3).
func (sc *schedCase) offerOf(kind int) {
	sc.mu.Lock()
	sc.nOffers++
	k := sc.nOffers
	sc.mu.Unlock()
	var m *protobufs.ServerToAgent
	switch kind {
	case 0:
		m = &protobufs.ServerToAgent{RemoteConfig: &protobufs.AgentRemoteConfig{ConfigHash: []byte("c" + strconv.Itoa(k)),
			Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
				"": {Body: []byte("n: " + strconv.Itoa(k)), ContentType: "text/yaml"}}}}}
	case 1:
		m = &protobufs.ServerToAgent{CustomMessage: &protobufs.CustomMessage{Capability: propCapability, Type: "m",
			Data: []byte("m" + strconv.Itoa(k))}}
	case 2:
		path := "/d/" + strconv.Itoa(k)
		file := []byte("package version " + strconv.Itoa(k))
		sum := sha256.Sum256(file)
		sc.srv.mu.Lock()
		sc.srv.files[path] = file
		sc.srv.mu.Unlock()
		m = &protobufs.ServerToAgent{PackagesAvailable: &protobufs.PackagesAvailable{
			Packages: map[string]*protobufs.PackageAvailable{"p": {Type: protobufs.PackageType_PackageType_TopLevel,
				Version: strconv.Itoa(k), Hash: []byte{byte(k)},
				File: &protobufs.DownloadableFile{DownloadUrl: "http://opamp.test" + path, ContentHash: sum[:]}}},
			AllPackagesHash: []byte{byte(k), 1}}}
	default:
		secs := []uint64{1, 30, 60}[k%3]
		m = &protobufs.ServerToAgent{ConnectionSettings: &protobufs.ConnectionSettingsOffers{
			Opamp: &protobufs.OpAMPConnectionSettings{HeartbeatIntervalSeconds: secs}}}
	}
	sc.note("offer(%d)", kind)
	sc.srv.offer(m)
}

// deadline is the timeout that applies to an exchange of kind.
func (sc *schedCase) deadline(kind exKind) time.Duration {
	switch kind {
	case exUpgrade:
		return sc.cfg.ConnectTimeout
	case exDownload:
		return sc.cfg.DownloadTimeout
	}
	return sc.cfg.RequestTimeout
}

// drawDecision draws what the server does with ex.
func (sc *schedCase) drawDecision(rt *rapid.T, ex *exchange) decision {
	var vs []verdict
	switch ex.kind {
	case exPost:
		vs = []verdict{vOK, vOK, v503, v429, vDrop}
	case exUpgrade:
		vs = []verdict{vOK, vOK, v503, v429, vDrop}
	default:
		vs = []verdict{vOK, vOK, vWrong, vNotFound, vDrop}
	}
	d := decision{v: rapid.SampledFrom(vs).Draw(rt, "verdict")}
	if d.v == v429 {
		d.retryAfter = rapid.SampledFrom([]int{1, 60, 3600}).Draw(rt, "retry-after")
	}
	// Late: a moment, or to just before, at or just after the effect's deadline (a result racing
	// the core's cancel), or an hour.
	left := sc.deadline(ex.kind) - time.Since(ex.arrived)
	switch rapid.IntRange(0, 6).Draw(rt, "delay") {
	case 1:
		d.delay = time.Millisecond
	case 2:
		d.delay = left - time.Millisecond
	case 3:
		d.delay = left
	case 4:
		d.delay = left + time.Millisecond
	case 5:
		d.delay = time.Hour
	}
	d.delay = max(d.delay, 0)
	return d
}

func (sc *schedCase) actAnswer(rt *rapid.T) {
	held := sc.srv.held()
	if len(held) == 0 {
		return
	}
	i := rapid.IntRange(0, len(held)-1).Draw(rt, "which")
	ex := held[i]
	d := sc.drawDecision(rt, ex)
	sc.decide(ex, d)
}

func (sc *schedCase) decide(ex *exchange, d decision) {
	if !sc.srv.claim(ex) {
		return
	}
	if d.v != vOK || d.delay > 0 {
		sc.faults++
	}
	sc.note("%v %s: %v after %v", ex.kind, ex.path, d.v, d.delay)
	schedCount(fmt.Sprintf("answer %v %v", ex.kind, d.v))
	ex.decide <- d
}

// actWS: the server closes (a Close frame) or drops its newest open WebSocket connection.
func (sc *schedCase) actWS(rt *rapid.T) {
	sw := sc.srv.openConn()
	if sw == nil {
		return
	}
	drop := rapid.Bool().Draw(rt, "drop")
	sc.wsFault(sw, drop)
}

func (sc *schedCase) wsFault(sw *serverWS, drop bool) {
	sc.srv.mu.Lock()
	sw.faulted = true
	sc.srv.mu.Unlock()
	sc.faults++
	if drop {
		sc.note("ws %d: drop", sw.n)
		_ = sw.end.Close()
		return
	}
	sc.note("ws %d: close", sw.n)
	go func() {
		sw.wmu <- struct{}{}
		defer func() { <-sw.wmu }()
		_ = sw.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, ""), time.Time{})
	}()
}

func (sc *schedCase) actTime(rt *rapid.T) {
	d := rapid.SampledFrom([]time.Duration{time.Millisecond, 100 * time.Millisecond, time.Second, 5 * time.Second,
		30 * time.Second, 5 * time.Minute, time.Hour}).Draw(rt, "advance")
	sc.note("time +%v", d)
	time.Sleep(d)
}

// actSlowSave: the saves from now on take a while.
func (sc *schedCase) actSlowSave(rt *rapid.T) {
	d := rapid.SampledFrom([]time.Duration{0, 100 * time.Millisecond, 3 * time.Second, 10 * time.Second}).Draw(rt, "save")
	sc.note("saves take %v", d)
	sc.maxSave = max(sc.maxSave, d)
	saveDelays.Store(sc.cfg.StateFile, d)
}

// actStop stops, a third of the times it is drawn (most cases go on to the end, where everything
// sent must arrive).
func (sc *schedCase) actStop(rt *rapid.T) {
	how := rapid.SampledFrom([]string{"not yet", "not yet ", "outside", "goroutine", "callback", "race"}).Draw(rt, "stop")
	if strings.HasPrefix(how, "not") {
		return
	}
	if how == "race" {
		// Stop while the server answers what it holds, at once.
		sc.askStop("goroutine")
		for _, ex := range sc.srv.held() {
			sc.decide(ex, decision{})
		}
		return
	}
	sc.askStop(how)
}

// actPanic: the callback panics on its next call: the client stops, with the panic.
func (sc *schedCase) actPanic(rt *rapid.T) {
	sc.mu.Lock()
	asked := sc.stopAsked
	sc.mu.Unlock()
	if asked || sc.panicked || rapid.IntRange(0, 2).Draw(rt, "panic now") > 0 {
		return
	}
	sc.panicked = true
	sc.m.mu.Lock()
	sc.m.panicAt = sc.m.calls + 1
	sc.m.mu.Unlock()
	sc.note("panic")
	sc.offerOf(1) // something to call back with
}

// actRace: two or three things back to back, the client not settling in between.
func (sc *schedCase) actRace(rt *rapid.T) {
	n := rapid.IntRange(2, 3).Draw(rt, "race")
	sc.note("race:")
	for range n {
		switch rapid.IntRange(0, 5).Draw(rt, "what") {
		case 0:
			if held := sc.srv.held(); len(held) > 0 {
				sc.decide(held[0], decision{v: rapid.SampledFrom([]verdict{vOK, vDrop}).Draw(rt, "verdict")})
			}
		case 1:
			sc.call(cOutside, rapid.Bool().Draw(rt, "describe"))
		case 2:
			sc.askStop("goroutine")
		case 3:
			sc.note("time +1ms")
			time.Sleep(time.Millisecond)
		case 4:
			if sw := sc.srv.openConn(); sw != nil {
				sc.wsFault(sw, rapid.Bool().Draw(rt, "drop"))
			}
		default:
			sc.offerOf(rapid.IntRange(0, 3).Draw(rt, "offer"))
		}
	}
}

// bubbleClientGoroutines lists the goroutines of this bubble running the client's code (its
// non-test files, or net/http's client connections). allowSaves: but a save already writing, and
// the register's wait for it (bounded, given up on).
func bubbleClientGoroutines(allowSaves bool) []string {
	return bubbleGoroutines(func(g string) bool {
		saving := strings.Contains(g, "opamp.saveState(") || strings.Contains(g, "opamp.(*inFlight).close.func")
		return clientStack(g) && !(allowSaves && saving)
	})
}

// bubbleGoroutines lists the other goroutines of this bubble that keep says to.
func bubbleGoroutines(keep func(string) bool) []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	gs := strings.Split(string(buf), "\n\n")
	me := bubbleRE.FindStringSubmatch(strings.SplitN(gs[0], "\n", 2)[0])
	if me == nil {
		return nil
	}
	var out []string
	for _, g := range gs[1:] {
		b := bubbleRE.FindStringSubmatch(strings.SplitN(g, "\n", 2)[0])
		if b != nil && b[1] == me[1] && keep(g) {
			out = append(out, g)
		}
	}
	return out
}

// inBubble says whether the calling goroutine is in a synctest bubble.
func inBubble() bool {
	buf := make([]byte, 256)
	buf = buf[:runtime.Stack(buf, false)]
	head, _, _ := strings.Cut(string(buf), "\n")
	return bubbleRE.MatchString(head)
}

var bubbleRE = regexp.MustCompile(`synctest bubble (\d+)`)

func clientStack(g string) bool {
	for _, line := range strings.Split(g, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "/go/opamp/") && strings.Contains(line, ".go:") && !strings.Contains(line, "_test.go:") {
			return true
		}
		if strings.HasPrefix(line, "net/http.(*persistConn)") {
			return true
		}
	}
	return false
}

// The core tap's record, checked against RULE-SHELL-04, -06 and -07 (as the core's effects checker and
// the adversary's monitor do, for what this test can reach).

type tapEffect struct {
	what         string // request, connection, download, store
	state        int    // tapLive, tapOpen, tapEnded, tapCancelled
	serverClosed bool   // ws_closed came for the server's Close; the core's ws_close answers it
	closeAsked   bool   // ws_close was issued
}

const (
	tapLive = iota
	tapOpen
	tapEnded
	tapCancelled
)

// checkTap checks the case's tap file (the one core of the case wrote it, in dir).
func checkTap(dir string) error {
	files, _ := filepath.Glob(filepath.Join(dir, "core.tap*"))
	if len(files) != 1 {
		return fmt.Errorf("the core tap: %d files in %s", len(files), dir)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		return err
	}
	effects := map[uint64]*tapEffect{}
	strs := map[uint64]*tapStream{} // Core mode's streams, by the shell's ids
	stopped := false
	step := 0
	for len(b) > 0 {
		if len(b) < 13 || uint32(len(b)-13) < binary.LittleEndian.Uint32(b[1:]) {
			return errors.New("the core tap: a torn record")
		}
		kind, n := b[0], binary.LittleEndian.Uint32(b[1:])
		data := b[13 : 13+n]
		b = b[13+n:]
		switch kind {
		case 2, 4:
			reply := &corepb.Reply{}
			if err := proto.Unmarshal(data, reply); err != nil {
				return fmt.Errorf("the core tap: a reply: %w", err)
			}
			out := reply.Output
			for _, e := range out.GetEffects() {
				switch k := e.Kind.(type) {
				case *corepb.Effect_HttpRequest:
					effects[k.HttpRequest.Id] = &tapEffect{what: "request"}
				case *corepb.Effect_WsOpen:
					effects[k.WsOpen.Id] = &tapEffect{what: "connection"}
				case *corepb.Effect_Download:
					effects[k.Download.Id] = &tapEffect{what: "download"}
				case *corepb.Effect_Store:
					effects[k.Store.Id] = &tapEffect{what: "store"}
				case *corepb.Effect_WsClose:
					if t := effects[k.WsClose.Id]; t != nil {
						t.closeAsked = true
					}
				case *corepb.Effect_Cancel:
					if t := effects[k.Cancel.Id]; t != nil && t.state != tapEnded {
						t.state = tapCancelled
					}
				// Core mode: a download is the shell's (its file the client's), named first by its
				// data, a splice into it, or its end.
				case *corepb.Effect_DownloadData:
					tapDownload(effects, k.DownloadData.Id)
				case *corepb.Effect_StreamSplice:
					tapDownload(effects, k.StreamSplice.Download)
					if st := strs[k.StreamSplice.Id]; st != nil {
						st.splices++
					}
				case *corepb.Effect_DownloadDone:
					tapDownload(effects, k.DownloadDone.Id)
				case *corepb.Effect_ProxyQuery:
					strs[k.ProxyQuery.Id] = tapStreamOf(strs, k.ProxyQuery.Id)
					strs[k.ProxyQuery.Id].queried++
				case *corepb.Effect_StreamResolve:
					tapStreamOf(strs, k.StreamResolve.Id).resolving = true
				case *corepb.Effect_StreamOpen:
					st := tapStreamOf(strs, k.StreamOpen.Id)
					st.opening, st.closed, st.ended = true, false, false
				case *corepb.Effect_StreamStartTls:
					tapStreamOf(strs, k.StreamStartTls.Id).tls++
				case *corepb.Effect_StreamClose:
					st := tapStreamOf(strs, k.StreamClose.Id)
					st.closed, st.resolving = true, false
				}
			}
			if out.GetStopped() {
				stopped = true
			}
		case 3:
			step++
			st := &corepb.Step{}
			if err := proto.Unmarshal(data, st); err != nil {
				return fmt.Errorf("the core tap: a step: %w", err)
			}
			if stopped {
				return fmt.Errorf("RULE-SHELL-07: step %d (%v) after the output that said stopped", step, st.Input)
			}
			if err := tapInput(effects, st.Input); err != nil {
				return fmt.Errorf("step %d: %w", step, err)
			}
			if err := tapStreamInput(strs, st.Input); err != nil {
				return fmt.Errorf("step %d: %w", step, err)
			}
		}
	}
	return nil
}

// cbCall is a call the callback made: what it was, and which notification the callback was being
// told (the told-th of the client's, from 1).
type cbCall struct {
	told int
	what string
	is   func(*corepb.Input) bool
}

// numberedCall is the schedule test's n-th call (schedCase.call), made from the callback.
func numberedCall(told, n int, describe bool) cbCall {
	if describe {
		return cbCall{told, fmt.Sprintf("SetDescription(%d)", n), func(in *corepb.Input) bool {
			d := in.GetSetDescription()
			return d != nil && proto.Equal(d, propDescription(n))
		}}
	}
	return cbCall{told, fmt.Sprintf("SendCustomMessage(%d)", n), func(in *corepb.Input) bool {
		m := in.GetSendCustomMessage()
		return m != nil && m.Type == "t" && string(m.Data) == strconv.Itoa(n)
	}}
}

// answerCall is the callback's answer to the remote config hash.
func answerCall(told int, hash []byte) cbCall {
	return cbCall{told, fmt.Sprintf("SetRemoteConfigStatus(%q)", hash), func(in *corepb.Input) bool {
		st := in.GetSetRemoteConfigStatus()
		return st != nil && string(st.LastRemoteConfigHash) == string(hash)
	}}
}

// tapSteps reads the case's tap file (in dir): the inputs the core was fed, in order, and for each
// notification the core emitted, in order, the step (an index into the inputs) that emitted it.
func tapSteps(dir string) (inputs []*corepb.Input, notified []int, err error) {
	files, _ := filepath.Glob(filepath.Join(dir, "core.tap*"))
	if len(files) != 1 {
		return nil, nil, fmt.Errorf("the core tap: %d files in %s", len(files), dir)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		return nil, nil, err
	}
	for len(b) > 0 {
		if len(b) < 13 || uint32(len(b)-13) < binary.LittleEndian.Uint32(b[1:]) {
			return nil, nil, errors.New("the core tap: a torn record")
		}
		kind, n := b[0], binary.LittleEndian.Uint32(b[1:])
		data := b[13 : 13+n]
		b = b[13+n:]
		switch kind {
		case 3:
			st := &corepb.Step{}
			if err := proto.Unmarshal(data, st); err != nil {
				return nil, nil, fmt.Errorf("the core tap: a step: %w", err)
			}
			inputs = append(inputs, st.Input)
		case 4:
			reply := &corepb.Reply{}
			if err := proto.Unmarshal(data, reply); err != nil {
				return nil, nil, fmt.Errorf("the core tap: a reply: %w", err)
			}
			for _, e := range reply.Output.GetEffects() {
				if e.GetNotify() != nil {
					notified = append(notified, len(inputs)-1)
				}
			}
		}
	}
	return inputs, notified, nil
}

// checkCallbackCalls checks the case's tap file (in dir) against RULE-SHELL-10: each of calls, made
// from inside the callback, reached the core before anything the client took in after the step
// that called the callback. Between that step and the call the core is fed only the application's
// calls (those queued ahead of it), and what the shell knew at once about an effect of that step
// (core mode's answer to a proxy query). A call that never reached the core (the client stopped
// first) still lets nothing else in.
func checkCallbackCalls(dir string, calls []cbCall) error {
	inputs, notified, err := tapSteps(dir)
	if err != nil {
		return err
	}
	for _, call := range calls {
		if call.told > len(notified) {
			return fmt.Errorf("RULE-SHELL-10: %s was made in callback %d, and the core notified %d times", call.what, call.told, len(notified))
		}
		at := notified[call.told-1]
		for i := at + 1; i < len(inputs) && !call.is(inputs[i]); i++ {
			switch inputs[i].Kind.(type) {
			case *corepb.Input_SetDescription, *corepb.Input_SetHealth, *corepb.Input_SetEffectiveConfig,
				*corepb.Input_SetCustomCapabilities, *corepb.Input_SendCustomMessage, *corepb.Input_SetAvailableComponents,
				*corepb.Input_SetRemoteConfigStatus, *corepb.Input_SetConnectionSettingsStatus,
				*corepb.Input_PackageInstalled, *corepb.Input_PackageRemoved, *corepb.Input_Stop:
			case *corepb.Input_ProxyAnswer:
			default:
				var fed []string
				for j := at; j <= i; j++ {
					fed = append(fed, fmt.Sprintf("  step %d: %s", j+1, short(inputs[j])))
				}
				return fmt.Errorf("RULE-SHELL-10: the callback's call %s, made in callback %d (of step %d), reached the core after step %d:\n%s",
					call.what, call.told, at+1, i+1, strings.Join(fed, "\n"))
			}
		}
	}
	return nil
}

// tapDownload notes download id, named by core mode's file effects, if it is new.
func tapDownload(effects map[uint64]*tapEffect, id uint64) {
	if effects[id] == nil {
		effects[id] = &tapEffect{what: "download"}
	}
}

// tapStream is one of core mode's streams, as the tap shows it.
type tapStream struct {
	queried, tls, splices int  // proxy_query, stream_start_tls and stream_splice asked, not answered
	resolving             bool // stream_resolve asked, not answered
	opening, open         bool // stream_open asked; stream_opened came
	closed                bool // stream_close came: nothing more about it
	ended                 bool // stream_failed came
}

func tapStreamOf(strs map[uint64]*tapStream, id uint64) *tapStream {
	if strs[id] == nil {
		strs[id] = &tapStream{}
	}
	return strs[id]
}

// tapStreamInput applies one input to core mode's streams: an error if the client may not feed it
// (an answer to nothing asked, anything about a stream after its close or its failure).
func tapStreamInput(strs map[uint64]*tapStream, in *corepb.Input) error {
	var id uint64
	var verb string
	switch k := in.GetKind().(type) {
	case *corepb.Input_ProxyAnswer:
		id, verb = k.ProxyAnswer.Id, "proxy_answer"
	case *corepb.Input_StreamResolved:
		id, verb = k.StreamResolved.Id, "stream_resolved"
	case *corepb.Input_StreamOpened:
		id, verb = k.StreamOpened.Id, "stream_opened"
	case *corepb.Input_StreamTlsReady:
		id, verb = k.StreamTlsReady.Id, "stream_tls_ready"
	case *corepb.Input_StreamData:
		id, verb = k.StreamData.Id, "stream_data"
	case *corepb.Input_StreamEof:
		id, verb = k.StreamEof.Id, "stream_eof"
	case *corepb.Input_StreamFailed:
		id, verb = k.StreamFailed.Id, "stream_failed"
	case *corepb.Input_StreamSpliced:
		id, verb = k.StreamSpliced.Id, "stream_spliced"
	default:
		return nil
	}
	st := strs[id]
	switch {
	case st == nil:
		return fmt.Errorf("RULE-SHELL-04: %s %d: no stream of that id was asked for", verb, id)
	case verb == "proxy_answer":
		if st.queried == 0 {
			return fmt.Errorf("RULE-SHELL-04: proxy_answer %d: not asked (or answered already)", id)
		}
		st.queried--
		return nil
	case st.closed:
		return fmt.Errorf("RULE-SHELL-04: %s %d: after the core closed the stream", verb, id)
	case st.ended:
		return fmt.Errorf("RULE-SHELL-04: %s %d: after the stream failed", verb, id)
	}
	switch verb {
	case "stream_resolved":
		if !st.resolving {
			return fmt.Errorf("RULE-SHELL-04: stream_resolved %d: no resolution asked", id)
		}
		st.resolving = false
	case "stream_failed":
		if !st.resolving && !st.opening {
			return fmt.Errorf("RULE-SHELL-04: stream_failed %d: nothing under way", id)
		}
		st.resolving, st.ended = false, true
	case "stream_opened":
		if !st.opening || st.open {
			return fmt.Errorf("RULE-SHELL-04: stream_opened %d: not opening", id)
		}
		st.open = true
	case "stream_tls_ready":
		if !st.open || st.tls == 0 {
			return fmt.Errorf("RULE-SHELL-04: stream_tls_ready %d: no TLS asked", id)
		}
		st.tls--
	case "stream_spliced":
		if !st.open || st.splices == 0 {
			return fmt.Errorf("RULE-SHELL-04: stream_spliced %d: no splice asked", id)
		}
		st.splices--
	default:
		if !st.open {
			return fmt.Errorf("RULE-SHELL-04: %s %d: the stream hasn't opened", verb, id)
		}
	}
	return nil
}

// tapNotifiesAfterStopped counts the notifications the core put after notify(stopped) in the same
// output (none, since the core's fix).
func tapNotifiesAfterStopped(dir string) int {
	files, _ := filepath.Glob(filepath.Join(dir, "core.tap*"))
	if len(files) != 1 {
		return 0
	}
	b, _ := os.ReadFile(files[0])
	late := 0
	for len(b) >= 13 {
		kind, size := b[0], binary.LittleEndian.Uint32(b[1:])
		if uint32(len(b)-13) < size {
			break
		}
		data := b[13 : 13+size]
		b = b[13+size:]
		if kind != 2 && kind != 4 {
			continue
		}
		reply := &corepb.Reply{}
		_ = proto.Unmarshal(data, reply)
		after := false
		for _, e := range reply.Output.GetEffects() {
			if n := e.GetNotify(); n != nil {
				if after {
					late++
				}
				if n.GetStopped() != nil {
					after = true
				}
			}
		}
	}
	return late
}

// tapTail describes the last n steps in the case's tap: each input, and its output's effects.
func tapTail(dir string, n int) string {
	files, _ := filepath.Glob(filepath.Join(dir, "core.tap*"))
	if len(files) != 1 {
		return "(no tap)"
	}
	b, _ := os.ReadFile(files[0])
	var lines []string
	for len(b) >= 13 {
		kind, size := b[0], binary.LittleEndian.Uint32(b[1:])
		if uint32(len(b)-13) < size {
			break
		}
		data := b[13 : 13+size]
		b = b[13+size:]
		switch kind {
		case 3:
			st := &corepb.Step{}
			_ = proto.Unmarshal(data, st)
			lines = append(lines, fmt.Sprintf("  now_ms=%d in: %s", st.NowMs, short(st.Input)))
		case 2, 4:
			reply := &corepb.Reply{}
			_ = proto.Unmarshal(data, reply)
			var effs []string
			for _, e := range reply.Output.GetEffects() {
				effs = append(effs, short(e))
			}
			if reply.Output.GetStopped() {
				effs = append(effs, "STOPPED")
			}
			lines = append(lines, "      out: "+strings.Join(effs, ", "))
		}
	}
	if len(lines) > 2*n {
		lines = lines[len(lines)-2*n:]
	}
	return strings.Join(lines, "\n")
}

// short is a message in one line, cut.
func short(m proto.Message) string {
	s := strings.Join(strings.Fields(prototext.Format(m)), " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// tapInput applies one input to the effects under way: an error if the shell may not feed it.
func tapInput(effects map[uint64]*tapEffect, in *corepb.Input) error {
	var (
		id   uint64
		what string
		verb string
	)
	switch k := in.GetKind().(type) {
	case *corepb.Input_HttpResponse:
		id, what, verb = k.HttpResponse.Id, "request", "http_response"
	case *corepb.Input_HttpFailed:
		id, what, verb = k.HttpFailed.Id, "request", "http_failed"
	case *corepb.Input_WsOpened:
		id, what, verb = k.WsOpened.Id, "connection", "ws_opened"
	case *corepb.Input_WsFailed:
		id, what, verb = k.WsFailed.Id, "connection", "ws_failed"
	case *corepb.Input_WsMessage:
		id, what, verb = k.WsMessage.Id, "connection", "ws_message"
	case *corepb.Input_WsClosed:
		id, what, verb = k.WsClosed.Id, "connection", "ws_closed"
	case *corepb.Input_WsLost:
		id, what, verb = k.WsLost.Id, "connection", "ws_lost"
	case *corepb.Input_Stored:
		id, what, verb = k.Stored.Id, "store", "stored"
	case *corepb.Input_StoreFailed:
		id, what, verb = k.StoreFailed.Id, "store", "store_failed"
	case *corepb.Input_Downloaded:
		id, what, verb = k.Downloaded.Id, "download", "downloaded"
	case *corepb.Input_DownloadFailed:
		id, what, verb = k.DownloadFailed.Id, "download", "download_failed"
	case *corepb.Input_DownloadResponse:
		id, what, verb = k.DownloadResponse.Id, "download", "download_response"
	default:
		return nil
	}
	t := effects[id]
	switch {
	case t == nil || t.what != what:
		return fmt.Errorf("RULE-SHELL-04: %s %d: no %s of that id was issued", verb, id, what)
	case t.state == tapCancelled:
		return fmt.Errorf("RULE-SHELL-06: %s %d: the core had cancelled it", verb, id)
	case t.state == tapEnded:
		return fmt.Errorf("RULE-SHELL-04: %s %d: it had ended (answered twice)", verb, id)
	}
	switch verb {
	case "ws_opened":
		if t.state != tapLive {
			return fmt.Errorf("RULE-SHELL-04: ws_opened %d: open already", id)
		}
		t.state = tapOpen
	case "ws_failed":
		if t.state != tapLive {
			return fmt.Errorf("RULE-SHELL-04: ws_failed %d: it had opened", id)
		}
		t.state = tapEnded
	case "ws_message":
		if t.state != tapOpen || t.serverClosed {
			return fmt.Errorf("RULE-SHELL-04: ws_message %d: not open, or after the server's Close", id)
		}
	case "ws_closed":
		switch {
		case t.state != tapOpen:
			return fmt.Errorf("RULE-SHELL-04: ws_closed %d: the connection hasn't opened", id)
		case t.closeAsked:
			t.state = tapEnded
		case t.serverClosed:
			return fmt.Errorf("RULE-SHELL-04: ws_closed %d twice, and no ws_close between", id)
		default:
			t.serverClosed = true
		}
	case "ws_lost":
		if t.state != tapOpen {
			return fmt.Errorf("RULE-SHELL-04: ws_lost %d: the connection hasn't opened", id)
		}
		t.state = tapEnded
	default:
		t.state = tapEnded
	}
	return nil
}

// schedFlags sets rapid's defaults for this test: schedChecks cases, a seed chosen here (so
// that it can be logged before the run), and no fail files in the source tree.
func schedFlags(t *testing.T) uint64 {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	restore := func(name string) {
		old := flag.Lookup(name).Value.String()
		t.Cleanup(func() { _ = flag.Set(name, old) })
	}
	if !set["rapid.checks"] && os.Getenv("RAPID_CHECKS") == "" {
		restore("rapid.checks")
		_ = flag.Set("rapid.checks", strconv.Itoa(schedChecks))
	}
	if !set["rapid.nofailfile"] && !set["rapid.failfile"] {
		restore("rapid.nofailfile")
		_ = flag.Set("rapid.nofailfile", "true")
	}
	seed, _ := strconv.ParseUint(flag.Lookup("rapid.seed").Value.String(), 10, 64)
	if seed == 0 {
		var b [8]byte
		_, _ = rand.Read(b[:])
		seed = binary.LittleEndian.Uint64(b[:]) >> 1
		restore("rapid.seed")
		_ = flag.Set("rapid.seed", strconv.FormatUint(seed, 10))
	}
	return seed
}

func TestSchedules(t *testing.T) { testSchedules(t, ModePlatform, "TestSchedules") }

// TestSchedulesCoreMode: the same, in core mode. The core speaks HTTP/1.1 and WebSocket itself
// over the pipes (its streams' dialer is the pipe network's), so its own HTTP and WebSocket meet
// the server's net/http and gorilla under the schedules rapid picks; the same rules are checked
// after every step, and nothing is left when Stop returns. The tap check covers the stream
// effects too (checkTap: each stream input names a stream the core opened and hasn't closed).
func TestSchedulesCoreMode(t *testing.T) { testSchedules(t, ModeCore, "TestSchedulesCoreMode") }

func modeName(m Mode) string {
	if m == "" {
		return string(ModeCore)
	}
	return string(m)
}

// covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-04, RULE-SHELL-06, RULE-SHELL-07, RULE-SHELL-10
func testSchedules(t *testing.T, mode Mode, name string) {
	seed := schedFlags(t)
	checks := flag.Lookup("rapid.checks").Value.String()
	t.Logf("base seed %d, %s cases (replay: go test -run '%s$' ./opamp/ -rapid.seed=%d -rapid.checks=%s)", seed, checks, name, seed, checks)
	old := beforeSave
	beforeSave = schedBeforeSave
	t.Cleanup(func() { beforeSave = old })
	tap, had := os.LookupEnv("OPAMP_CORE_TAP")
	t.Cleanup(func() {
		if had {
			os.Setenv("OPAMP_CORE_TAP", tap)
		} else {
			os.Unsetenv("OPAMP_CORE_TAP")
		}
	})
	base := t.TempDir()
	began := time.Now()
	rapid.Check(t, func(rt *rapid.T) {
		rapid.SyncTest(rt, func(rt *rapid.T) { runSchedCase(rt, base, mode) })
	})
	t.Logf("%s cases in %v", checks, time.Since(began))
	schedStats.Lock()
	var whats []string
	for w, n := range schedStats.n {
		whats = append(whats, fmt.Sprintf("%s: %d", w, n))
	}
	schedStats.Unlock()
	slices.Sort(whats)
	t.Logf("exercised (shrinking included):\n  %s", strings.Join(whats, "\n  "))
}

// A 30 s heartbeat and a 1 h Retry-After, in virtual time: two hours of the client's life, in
// two bubbles in a row (the first may compile the core: nothing it keeps may be tied to that
// bubble). The second takes milliseconds of real time.
//
// covers: RULE-SHELL-07
func TestScheduleVirtualTime(t *testing.T) {
	for run := range 2 {
		began := time.Now()
		var at []time.Duration
		synctest.Test(t, func(t *testing.T) {
			n := newPipeNet()
			srv := newSchedServer(false)
			throttled := false
			var mu sync.Mutex
			start := time.Now()
			hs := &http.Server{ConnContext: pipeConnContext, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				at = append(at, time.Since(start))
				first := !throttled
				throttled = true
				mu.Unlock()
				if first {
					_, _ = io.ReadAll(r.Body)
					srv.refuse(w, decision{v: v429, retryAfter: 3600})
					return
				}
				srv.post(w, r)
			})}
			go func() { _ = hs.Serve(n) }()
			tr := n.transport()
			c, err := Start(context.Background(), Config{Mode: ModePlatform, Endpoint: "http://opamp.test/v1/opamp", InstanceUID: modelUID,
				HeartbeatInterval: 30 * time.Second, HTTPClient: &http.Client{Transport: tr}}, Status{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Hour)
			stopAt := time.Now()
			if err := c.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if took := time.Since(stopAt); took > 5*time.Second+schedStopMargin {
				t.Errorf("Stop took %v", took)
			}
			tr.CloseIdleConnections()
			synctest.Wait()
			if open := n.openClientEnds(); open != 0 {
				t.Errorf("%d connections open after Stop", open)
			}
			close(srv.quit)
			n.shutdown()
			_ = hs.Close()
		})
		real := time.Since(began)
		if len(at) < 2 || at[1] < time.Hour || at[1] > time.Hour+time.Minute {
			t.Fatalf("run %d: requests at %v: the second not an hour after the 429", run, at)
		}
		// Every 30 s from then on: 120 heartbeats in the hour that is left.
		if len(at) < 100 {
			t.Fatalf("run %d: %d requests in two hours: %v", run, len(at), at)
		}
		t.Logf("run %d: %d requests over two virtual hours (the second at %v, after Retry-After: 3600), in %v of real time",
			run, len(at), at[1], real)
		if run == 1 && real > 5*time.Second {
			t.Errorf("two virtual hours took %v of real time", real)
		}
	}
}

// TestScheduleStoppedIsTheLastNotification: the server answers the goodbye with a remote config.
// The core's output once said notify(stopped) before it (found by TestSchedules, -rapid.seed=
// 8540131567590800061). The core now drops an offer once stop is asked, and the shell tells
// stopped last in its output besides: the application hears stopped last, once, and no config
// after it (RULE-SHELL-01).
//
// covers: RULE-SHELL-01
func TestScheduleStoppedIsTheLastNotification(t *testing.T) {
	for _, mode := range []Mode{ModePlatform, ModeCore} {
		t.Run(modeName(mode), func(t *testing.T) { stoppedIsTheLastNotification(t, mode) })
	}
}

func stoppedIsTheLastNotification(t *testing.T, mode Mode) {
	synctest.Test(t, func(t *testing.T) {
		n := newPipeNet()
		srv := newSchedServer(false)
		hs := &http.Server{Handler: srv, ConnContext: pipeConnContext}
		go func() { _ = hs.Serve(n) }()
		cfg := Config{Endpoint: "http://opamp.test/v1/opamp", InstanceUID: modelUID, HeartbeatInterval: 30 * time.Second,
			Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig | protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat)}
		tr := onPipes(&cfg, mode, n)
		var mu sync.Mutex
		var heard []NotificationKind
		c, err := Start(context.Background(), cfg, Status{}, func(_ *Client, n Notification) {
			mu.Lock()
			defer mu.Unlock()
			heard = append(heard, n.Kind)
		})
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		// The goodbye's answer carries it.
		srv.offer(&protobufs.ServerToAgent{RemoteConfig: &protobufs.AgentRemoteConfig{ConfigHash: []byte("c"),
			Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte("x")}}}}})
		if err := c.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		got := slices.Clone(heard)
		mu.Unlock()
		if len(got) == 0 || got[len(got)-1] != Stopped || slices.Index(got, Stopped) != len(got)-1 {
			t.Errorf("heard %v: want stopped last, once", got)
		}
		closeIdle(tr)
		close(srv.quit)
		n.shutdown()
		_ = hs.Close()
	})
}

// TestScheduleTheCallbacksAnswerComesFirst: after a restart the core hands the saved remote config
// to the application again, in the step that starts connecting, and reports it APPLYING until the
// application answers. Here something else is waiting for the client's goroutine by the time the
// callback has answered, so both are there when the callback returns:
//   - the connection: the callback waits for the bubble to settle first, so in platform mode the
//     connection is open and its report queued (in core mode the core has not asked for its stream
//     yet: nothing of it can be waiting, and this passed before the rule was kept);
//   - a timer: the callback takes an hour (of the bubble's time), so the client's timer is due.
//
// The answer goes first (RULE-SHELL-10): in the core's tap it is the input after the step that
// handed the config over, the first message says APPLIED and none says APPLYING. Left to the
// loop's select, which picks at random among what is ready, the other came first half the time:
// twenty restarts each, so that it shows.
//
// covers: RULE-SHELL-10
func TestScheduleTheCallbacksAnswerComesFirst(t *testing.T) {
	for _, mode := range []Mode{ModePlatform, ModeCore} {
		t.Run(modeName(mode)+"/the connection", func(t *testing.T) { theCallbacksAnswerComesFirst(t, mode, synctest.Wait) })
		t.Run(modeName(mode)+"/a timer", func(t *testing.T) {
			theCallbacksAnswerComesFirst(t, mode, func() { time.Sleep(time.Hour) })
		})
	}
}

// theCallbacksAnswerComesFirst: wait is what the callback does, at each restart, before it answers.
func theCallbacksAnswerComesFirst(t *testing.T, mode Mode, wait func()) {
	const restarts = 20
	applying, applied := protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED
	hash := []byte("c1")
	base := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		n := newPipeNet()
		srv := newSchedServer(false)
		hs := &http.Server{Handler: srv, ConnContext: pipeConnContext}
		go func() { _ = hs.Serve(n) }()
		// run is one life of the client on the state file: its callback answers every remote
		// config APPLIED before it returns, after wait if this is a restart. It returns the
		// messages the server got in that life, and which callback answered.
		run := func(name string, restart bool) ([]*protobufs.AgentToServer, []cbCall) {
			dir := filepath.Join(base, name)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Endpoint: "ws://opamp.test/v1/opamp", InstanceUID: modelUID, HeartbeatInterval: 30 * time.Second,
				StateFile: filepath.Join(base, "state"), tapFile: filepath.Join(dir, "core.tap"),
				Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig | protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat)}
			tr := onPipes(&cfg, mode, n)
			before := len(srv.messages())
			var mu sync.Mutex
			var answers []cbCall
			told := 0
			answered := make(chan struct{}, 16)
			c, err := Start(context.Background(), cfg, Status{}, func(c *Client, n Notification) {
				mu.Lock()
				told++
				k := told
				mu.Unlock()
				if n.Kind != RemoteConfig {
					return
				}
				if restart {
					wait()
				}
				c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{LastRemoteConfigHash: n.RemoteConfig.GetConfigHash(), Status: applied})
				mu.Lock()
				answers = append(answers, answerCall(k, n.RemoteConfig.GetConfigHash()))
				mu.Unlock()
				answered <- struct{}{}
			})
			if err != nil {
				t.Fatal(err)
			}
			if !restart {
				synctest.Wait()
				srv.offer(&protobufs.ServerToAgent{RemoteConfig: &protobufs.AgentRemoteConfig{ConfigHash: hash,
					Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte("x")}}}}})
			}
			select {
			case <-answered:
			case <-time.After(2 * time.Hour):
				t.Fatalf("%s: the callback was not handed the remote config", name)
			}
			// (The callback has answered, and no longer waits for the bubble: this goroutine may.)
			time.Sleep(time.Minute)
			synctest.Wait()
			if err := c.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			closeIdle(tr)
			synctest.Wait()
			mu.Lock()
			defer mu.Unlock()
			if err := checkCallbackCalls(dir, answers); err != nil {
				t.Errorf("%s: %v\n%s", name, err, tapTail(dir, 6))
			}
			return srv.messages()[before:], answers
		}
		statuses := func(got []*protobufs.AgentToServer) (out []protobufs.RemoteConfigStatuses) {
			for _, m := range got {
				if st := m.RemoteConfigStatus; st != nil && string(st.LastRemoteConfigHash) == string(hash) {
					out = append(out, st.Status)
				}
			}
			return out
		}
		// The first life: the server offers the config, the application applies it, and it is
		// saved. (Offered by the server, a config is APPLYING in the message that goes before the
		// application is told.)
		if got, _ := run("first", false); !slices.Equal(statuses(got), []protobufs.RemoteConfigStatuses{applying, applied}) {
			t.Fatalf("the first life reported %v for the config: want APPLYING, then APPLIED", statuses(got))
		}
		for i := range restarts {
			got, answers := run(fmt.Sprintf("restart%d", i), true)
			if len(answers) != 1 {
				t.Fatalf("restart %d: the callback answered %d remote configs, want the saved one", i, len(answers))
			}
			if len(got) == 0 {
				t.Fatalf("restart %d: the server got no message", i)
			}
			if st := got[0].RemoteConfigStatus; st == nil || string(st.LastRemoteConfigHash) != string(hash) || st.Status != applied {
				t.Errorf("restart %d: the first message reports %v on the saved config, answered APPLIED in the callback: want APPLIED", i, st)
			}
			if st := statuses(got); slices.Contains(st, applying) {
				t.Errorf("restart %d: APPLYING reported for a config answered APPLIED in the callback: %v", i, st)
			}
		}
		close(srv.quit)
		n.shutdown()
		_ = hs.Close()
	})
}

// Stop returns within StopTimeout (and a margin) even with a save writing slowly at stop: the
// core waits for the store until the stop deadline and cancels it, and the client must not then
// wait a whole StopTimeout more for the save (found by TestSchedules, -rapid.seed=7844315713415082487:
// Stop took 10 s with a 5 s StopTimeout).
//
// covers: RULE-SHELL-07
func TestScheduleStopBoundWithASlowSave(t *testing.T) {
	old := beforeSave
	beforeSave = schedBeforeSave
	t.Cleanup(func() { beforeSave = old })
	for _, mode := range []Mode{ModePlatform, ModeCore} {
		t.Run(modeName(mode), func(t *testing.T) { stopBoundWithASlowSave(t, mode) })
	}
}

func stopBoundWithASlowSave(t *testing.T, mode Mode) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		n := newPipeNet()
		srv := newSchedServer(false)
		hs := &http.Server{Handler: srv, ConnContext: pipeConnContext}
		go func() { _ = hs.Serve(n) }()
		cfg := Config{Endpoint: "http://opamp.test/v1/opamp", InstanceUID: modelUID, HeartbeatInterval: 30 * time.Second,
			StopTimeout: 5 * time.Second, StateFile: filepath.Join(dir, "state"),
			Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig | protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat)}
		tr := onPipes(&cfg, mode, n)
		saveDelays.Store(cfg.StateFile, 10*time.Second)
		defer saveDelays.Delete(cfg.StateFile)
		c, err := Start(context.Background(), cfg, Status{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		// A remote config: the client saves it, slowly.
		srv.offer(&protobufs.ServerToAgent{RemoteConfig: &protobufs.AgentRemoteConfig{ConfigHash: []byte("c"),
			Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte("x")}}}}})
		time.Sleep(30 * time.Second) // the next poll takes it
		synctest.Wait()
		began := time.Now()
		if err := c.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if took, bound := time.Since(began), cfg.StopTimeout+schedStopMargin; took > bound {
			t.Errorf("Stop took %v with a save writing, over %v", took, bound)
		}
		time.Sleep(time.Minute) // the save ends by itself
		closeIdle(tr)
		close(srv.quit)
		n.shutdown()
		_ = hs.Close()
	})
}

// A panic in the callback stops the client in the middle of an output (the rest of it isn't
// carried out), and its shutdown still closes every connection before Stop returns: the callback,
// on the second WebSocket connection's connected, asks for the stop, offers the server's next
// config and panics, a save of the last one still writing (TestSchedules, -rapid.seed=
// 7705626846506181755, once said WebSocket 2 was still open when Stop returned: the pipe's own
// measure, TestPipeEndIsClosedOnceAnyCloseReturns).
//
// covers: RULE-SHELL-02, RULE-SHELL-07
func TestSchedulePanicClosesEveryConnection(t *testing.T) {
	old := beforeSave
	beforeSave = schedBeforeSave
	t.Cleanup(func() { beforeSave = old })
	for _, mode := range []Mode{ModePlatform, ModeCore} {
		t.Run(modeName(mode), func(t *testing.T) { panicClosesEveryConnection(t, mode) })
	}
}

func panicClosesEveryConnection(t *testing.T, mode Mode) {
	dir := t.TempDir()
	config := func(k string) *protobufs.ServerToAgent {
		return &protobufs.ServerToAgent{RemoteConfig: &protobufs.AgentRemoteConfig{ConfigHash: []byte(k),
			Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte(k)}}}}}
	}
	synctest.Test(t, func(t *testing.T) {
		n := newPipeNet()
		srv := newSchedServer(false)
		hs := &http.Server{Handler: srv, ConnContext: pipeConnContext}
		go func() { _ = hs.Serve(n) }()
		cfg := Config{Endpoint: "ws://opamp.test/v1/opamp", InstanceUID: modelUID, HeartbeatInterval: 5 * time.Second,
			StopTimeout: 5 * time.Second, StateFile: filepath.Join(dir, "state"),
			Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig | protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat)}
		tr := onPipes(&cfg, mode, n)
		saveDelays.Store(cfg.StateFile, 3*time.Second)
		defer saveDelays.Delete(cfg.StateFile)
		var connected atomic.Int32
		stopped := make(chan stopResult, 1)
		_, err := Start(context.Background(), cfg, Status{}, func(c *Client, nt Notification) {
			if nt.Kind != Connected || connected.Add(1) != 2 {
				return
			}
			for k := range 17 {
				c.SetDescription(propDescription(k))
			}
			if err := c.Stop(context.Background()); err != nil {
				t.Errorf("Stop from the callback returned %v", err)
			}
			go func() {
				began := time.Now()
				err := c.Stop(context.Background())
				res := stopResult{err: err, took: time.Since(began)}
				srv.mu.Lock()
				for _, sw := range srv.conns {
					if !sw.end.peer.closed.Load() {
						res.open = append(res.open, fmt.Sprintf("WebSocket %d", sw.n))
					}
				}
				srv.mu.Unlock()
				if len(res.open) > 0 {
					res.goroutines = bubbleClientGoroutines(true)
				}
				stopped <- res
			}()
			srv.offer(config("next"))
			panic(modelPanic)
		})
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		// A config to save (slowly), and the first connection closed by the server: the client
		// opens another.
		srv.offer(config("saved"))
		synctest.Wait()
		sw := srv.openConn()
		if sw == nil {
			t.Fatal("no connection open")
		}
		go func() {
			sw.wmu <- struct{}{}
			defer func() { <-sw.wmu }()
			_ = sw.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, ""), time.Time{})
		}()
		res := <-stopped
		if res.err == nil || !strings.Contains(res.err.Error(), modelPanic) {
			t.Errorf("Stop returned %v, not the panic", res.err)
		}
		if bound := cfg.StopTimeout + schedStopMargin; res.took > bound {
			t.Errorf("Stop took %v, over %v", res.took, bound)
		}
		srv.mu.Lock()
		conns := len(srv.conns)
		srv.mu.Unlock()
		if conns != 2 {
			t.Errorf("%d WebSocket connections, want 2", conns)
		}
		if len(res.open) > 0 {
			t.Errorf("when Stop returned, still open at the client: %v; the client's goroutines then:\n%s",
				res.open, strings.Join(res.goroutines, "\n\n"))
		}
		time.Sleep(time.Minute) // the save ends by itself
		closeIdle(tr)
		synctest.Wait()
		if open := n.openClientEnds(); open != 0 {
			t.Errorf("%d connections the client dialled still open after Stop", open)
		}
		close(srv.quit)
		n.shutdown()
		_ = hs.Close()
	})
}
