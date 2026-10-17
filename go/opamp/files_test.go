// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

// The file operations of saves and downloads: journaled into the core tap, held by the core's
// effects checker (opamp-tap) to a disk that can crash at any point, and under injected faults (the
// fileSystem seam).
//
// OPAMP_FAULT_RUNS sets the number of fault plans per property (default 10 for saves, 4 for
// downloads), OPAMP_FAULT_SEED replays a failure.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
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
	"syscall"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// tapRec is one record of a core tap.
type tapRec struct {
	kind byte
	data []byte
}

func readTapFile(t *testing.T, path string) []tapRec {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var recs []tapRec
	for len(b) > 0 {
		if len(b) < 13 {
			t.Fatalf("%s: a torn header", path)
		}
		n := binary.LittleEndian.Uint32(b[1:])
		if uint32(len(b)-13) < n {
			t.Fatalf("%s: a torn record", path)
		}
		recs = append(recs, tapRec{kind: b[0], data: b[13 : 13+n]})
		b = b[13+n:]
	}
	return recs
}

// fileOp is a file record (kind 5), decoded.
type fileOp map[string]any

func (o fileOp) String() string { b, _ := json.Marshal(map[string]any(o)); return string(b) }
func (o fileOp) op() string     { s, _ := o["op"].(string); return s }
func (o fileOp) path() string   { s, _ := o["path"].(string); return s }

func asFileOp(t *testing.T, r tapRec) fileOp {
	t.Helper()
	var o fileOp
	if err := json.Unmarshal(r.data, &o); err != nil {
		t.Fatalf("a file record that isn't JSON: %q", r.data)
	}
	return o
}

// saveLog is what a tap says of the saves: the stores asked for, in order, and their answers.
type saveLog struct {
	start     []byte // the state the core started from
	stores    []*corepb.Store
	answers   map[uint64][]string // "stored" or "failed", in order
	cancelled map[uint64]bool
	lastStore int // the latest store answered stored (its place in stores), -1: none
}

func savesOf(t *testing.T, recs []tapRec) saveLog {
	t.Helper()
	l := saveLog{answers: map[uint64][]string{}, cancelled: map[uint64]bool{}, lastStore: -1}
	at := map[uint64]int{}
	for _, r := range recs {
		switch r.kind {
		case 1:
			cfg := &corepb.Config{}
			if err := proto.Unmarshal(r.data, cfg); err != nil {
				t.Fatal(err)
			}
			l.start = cfg.SavedState
		case 3:
			s := &corepb.Step{}
			if err := proto.Unmarshal(r.data, s); err != nil {
				t.Fatal(err)
			}
			if id := s.Input.GetStored().GetId(); s.Input.GetStored() != nil {
				l.answers[id] = append(l.answers[id], "stored")
				if i, ok := at[id]; ok && i > l.lastStore {
					l.lastStore = i
				}
			}
			if f := s.Input.GetStoreFailed(); f != nil {
				l.answers[f.Id] = append(l.answers[f.Id], "failed")
			}
		case 4:
			reply := &corepb.Reply{}
			if err := proto.Unmarshal(r.data, reply); err != nil {
				t.Fatal(err)
			}
			for _, e := range reply.Output.GetEffects() {
				if s := e.GetStore(); s != nil {
					at[s.Id] = len(l.stores)
					l.stores = append(l.stores, s)
				}
				if c := e.GetCancel(); c != nil {
					l.cancelled[c.Id] = true
				}
			}
		}
	}
	return l
}

// version is which version content is: -1 the start's, i the data of store i (the latest with
// that data), or an error.
func (l saveLog) version(content []byte) (int, error) {
	for i := len(l.stores) - 1; i >= 0; i-- {
		if bytes.Equal(l.stores[i].Data, content) {
			return i, nil
		}
	}
	if bytes.Equal(content, l.start) {
		return -1, nil
	}
	return 0, fmt.Errorf("%d bytes that are no whole save", len(content))
}

// opampTap runs the core's effects checker on a tap file, if it is there: false if it isn't.
func opampTap(t *testing.T, path string) (ran bool, out string, err error) {
	t.Helper()
	bin, _ := filepath.Abs(filepath.Join("..", "..", "target", "release", "opamp-tap"))
	if _, serr := os.Stat(bin); serr != nil {
		return false, "", nil
	}
	b, err := exec.Command(bin, path).CombinedOutput()
	return true, string(b), err
}

func checkWithOpampTap(t *testing.T, path string) {
	t.Helper()
	ran, out, err := opampTap(t, path)
	if !ran {
		t.Log("target/release/opamp-tap isn't there: the tap isn't checked")
		return
	}
	if err != nil {
		t.Fatalf("opamp-tap %s: %v\n%s", path, err, out)
	}
}

// A client with a state file, tapped: its save of the instance UID and its download of a package
// are journaled step by step, each record before what depends on it reaches the core, and
// the effects checker finds nothing wrong.
func TestTheTapJournalsSavesAndDownloads(t *testing.T) {
	dir := t.TempDir()
	tapPath := filepath.Join(dir, "taps", "core.tap")
	if err := os.Mkdir(filepath.Dir(tapPath), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPAMP_CORE_TAP", tapPath)
	file := bytes.Repeat([]byte("the package "), 1000)
	sum := sha256.Sum256(file)
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(file) }))
	defer files.Close()
	srv := &offerServer{answer: func(*protobufs.AgentToServer) *protobufs.ServerToAgent {
		return &protobufs.ServerToAgent{PackagesAvailable: &protobufs.PackagesAvailable{
			Packages: map[string]*protobufs.PackageAvailable{"p": {Type: protobufs.PackageType_PackageType_TopLevel,
				Version: "1", Hash: []byte{1}, File: &protobufs.DownloadableFile{DownloadUrl: files.URL + "/p", ContentHash: sum[:]}}},
			AllPackagesHash: []byte{9}}}
	}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	state := filepath.Join(dir, "state.bin")
	// Left by a run that crashed mid-save.
	if err := os.WriteFile(tempFile(state), []byte("torn"), 0o644); err != nil {
		t.Fatal(err)
	}
	installed := make(chan string, 1)
	c, err := Start(context.Background(), Config{Mode: ModeGo, Endpoint: ts.URL, HeartbeatInterval: time.Hour, StateFile: state,
		AcceptPackages: true}, Status{}, func(c *Client, n Notification) {
		if n.Kind == InstallPackage {
			got, err := os.ReadFile(n.Path)
			if err == nil && sha256.Sum256(got) != sum {
				err = errors.New("the file handed over isn't the package")
			}
			c.PackageInstalled(n.PackageName, n.Package.Hash, err)
			installed <- n.Path
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	var path string
	select {
	case path = <-installed:
	case <-time.After(10 * time.Second):
		t.Fatal("no InstallPackage")
	}
	// Installed: the core discards the file.
	if !eventually(5*time.Second, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) }) {
		t.Error("the download wasn't discarded")
	}
	_ = c.Stop(context.Background())

	taps, _ := filepath.Glob(tapPath + "*")
	if len(taps) != 1 {
		t.Fatalf("taps: %v", taps)
	}
	recs := readTapFile(t, taps[0])
	if len(recs) < 3 || recs[0].kind != 1 || recs[1].kind != 2 || recs[2].kind != 5 {
		t.Fatalf("the tap doesn't start with init, then a file record: %v", recs[:min(3, len(recs))])
	}
	if o := asFileOp(t, recs[2]); o.op() != "state" || o.path() != "state.bin" {
		t.Fatalf("the first file record is %v", o)
	}
	saves := savesOf(t, recs)
	if len(saves.stores) == 0 {
		t.Fatal("nothing saved")
	}
	dl := "state.bin.downloads/" + filepath.Base(path)
	// Each stored: the records of the state's files before it end in a whole save of that data;
	// each download response: its file was made, written whole and synced before it.
	var ops []fileOp
	var seen []string
	stored, responded := 0, 0
	for _, r := range recs {
		switch r.kind {
		case 5:
			ops = append(ops, asFileOp(t, r))
			seen = append(seen, ops[len(ops)-1].String())
		case 3:
			s := &corepb.Step{}
			_ = proto.Unmarshal(r.data, s)
			if id := s.Input.GetStored(); id != nil {
				stored++
				var data []byte
				for _, st := range saves.stores {
					if st.Id == id.Id {
						data = st.Data
					}
				}
				var mine []string
				for _, o := range ops {
					if p := o.path(); p == "state.bin" || p == "state.bin.tmp" || p == "." || o["from"] == "state.bin.tmp" {
						mine = append(mine, o.String())
					}
				}
				want := []string{
					fileOp{"op": "create", "path": "state.bin.tmp"}.String(),
					fileOp{"op": "write", "path": "state.bin.tmp", "data": hex.EncodeToString(data)}.String(),
					fileOp{"op": "sync", "path": "state.bin.tmp"}.String(),
					fileOp{"op": "rename", "from": "state.bin.tmp", "to": "state.bin"}.String(),
					fileOp{"op": "syncdir", "path": "."}.String(),
				}
				if len(mine) < len(want) || strings.Join(mine[len(mine)-len(want):], "\n") != strings.Join(want, "\n") {
					t.Fatalf("stored %d after the records\n%s\nwant them to end in\n%s", id.Id, strings.Join(mine, "\n"), strings.Join(want, "\n"))
				}
			}
			if d := s.Input.GetDownloadResponse(); d != nil {
				responded++
				var mine []fileOp
				for _, o := range ops {
					if o.path() == dl {
						mine = append(mine, o)
					}
				}
				var written float64
				for _, o := range mine {
					if o.op() == "write" {
						written += o["len"].(float64)
					}
				}
				if len(mine) < 3 || mine[0].op() != "create" || mine[len(mine)-1].op() != "sync" || int(written) != len(file) {
					t.Fatalf("download %d responded after %v", d.Id, mine)
				}
			}
		}
	}
	if stored == 0 || responded != 1 {
		t.Fatalf("%d stored, %d download responses", stored, responded)
	}
	// The leftover temporary file was removed before the first save made its own; the downloads'
	// directory was made, and the file discarded.
	for _, want := range []string{
		fileOp{"op": "remove", "path": "state.bin.tmp"}.String(),
		fileOp{"op": "mkdir", "path": "state.bin.downloads"}.String(),
		fileOp{"op": "remove", "path": dl}.String(),
	} {
		found := false
		for _, s := range seen {
			found = found || s == want
		}
		if !found {
			t.Errorf("no %s in\n%s", want, strings.Join(seen, "\n"))
		}
	}
	if seen[1] != (fileOp{"op": "remove", "path": "state.bin.tmp"}).String() {
		t.Errorf("the first save didn't start by removing the leftover: %v", seen[:2])
	}
	checkWithOpampTap(t, taps[0])
}

// faultKind is a fault injected into one file operation.
type faultKind int

const (
	faultEIO   faultKind = iota // the operation fails, having done nothing
	faultHalf                   // a write writes half, then fails (ENOSPC)
	faultSlow                   // the operation takes 0.5-2 s
	faultLands                  // the operation is done, then reports a failure (a rename)
)

func (k faultKind) String() string { return [...]string{"eio", "half", "slow", "lands"}[k] }

type fault struct {
	kind faultKind
	slow time.Duration
}

// faultFS is the disk, with faults injected into chosen operations: the nth operation on the
// files it watches (counted from 0) gets plan[n].
type faultFS struct {
	osFS
	watch func(path string) bool
	mu    sync.Mutex
	n     int
	plan  map[int]fault
	// injected says what was injected, in order.
	injected []string
	// slowBegan gets a token when a slow operation starts; windows are the slow operations'
	// times, start and end.
	slowBegan chan struct{}
	windows   [][2]time.Time
}

func newFaultFS(watch func(string) bool, plan map[int]fault) *faultFS {
	return &faultFS{watch: watch, plan: plan, slowBegan: make(chan struct{}, 16)}
}

var errEIO = syscall.EIO

// at runs op (named what, on path) with the fault planned for it, if any. done is whether the
// operation took place despite the fault (for faultLands).
func (f *faultFS) at(what, path string, op func() error) error {
	if !f.watch(path) {
		return op()
	}
	f.mu.Lock()
	n := f.n
	f.n++
	ft, ok := f.plan[n]
	if ok {
		f.injected = append(f.injected, fmt.Sprintf("%s %s: %s", what, filepath.Base(path), ft.kind))
	}
	f.mu.Unlock()
	if !ok {
		return op()
	}
	switch ft.kind {
	case faultSlow:
		began := time.Now()
		f.slowBegan <- struct{}{}
		time.Sleep(ft.slow)
		err := op()
		f.mu.Lock()
		f.windows = append(f.windows, [2]time.Time{began, time.Now()})
		f.mu.Unlock()
		return err
	case faultLands:
		if err := op(); err != nil {
			return err
		}
		return &os.PathError{Op: what, Path: path, Err: errEIO}
	default: // faultEIO, and faultHalf on anything but a write
		return &os.PathError{Op: what, Path: path, Err: errEIO}
	}
}

func (f *faultFS) createNew(path string) (writeFile, error) {
	var w writeFile
	err := f.at("create", path, func() (err error) { w, err = f.osFS.createNew(path); return err })
	if err != nil {
		if w != nil { // made, then said to have failed: it is there all the same
			_ = w.Close()
		}
		return nil, err
	}
	return &faultFile{f: w, fs: f, path: path}, nil
}

func (f *faultFS) remove(path string) error {
	return f.at("remove", path, func() error { return f.osFS.remove(path) })
}

func (f *faultFS) rename(from, to string) error {
	return f.at("rename", to, func() error { return f.osFS.rename(from, to) })
}

func (f *faultFS) syncDir(dir string) (bool, error) {
	var ok bool
	err := f.at("syncdir", dir, func() (err error) { ok, err = f.osFS.syncDir(dir); return err })
	return ok && err == nil, err
}

func (f *faultFS) mkdir(dir string) error {
	return f.at("mkdir", dir, func() error { return f.osFS.mkdir(dir) })
}

type faultFile struct {
	f    writeFile
	fs   *faultFS
	path string
}

func (w *faultFile) Write(b []byte) (int, error) {
	if !w.fs.watch(w.path) {
		return w.f.Write(b)
	}
	w.fs.mu.Lock()
	ft, ok := w.fs.plan[w.fs.n]
	half := ok && ft.kind == faultHalf
	if half {
		w.fs.n++
		w.fs.injected = append(w.fs.injected, fmt.Sprintf("write %s: half", filepath.Base(w.path)))
	}
	w.fs.mu.Unlock()
	if half {
		n, _ := w.f.Write(b[:len(b)/2])
		return n, &os.PathError{Op: "write", Path: w.path, Err: syscall.ENOSPC}
	}
	var n int
	err := w.fs.at("write", w.path, func() (err error) { n, err = w.f.Write(b); return err })
	return n, err
}

func (w *faultFile) Sync() error {
	return w.fs.at("sync", w.path, w.f.Sync)
}

func (w *faultFile) Close() error {
	return w.fs.at("close", w.path, w.f.Close)
}

// faultEnv is how many plans to run, and from which seed: OPAMP_FAULT_RUNS (def), and
// OPAMP_FAULT_SEED replays one.
func faultEnv(t *testing.T, def int) (runs int, seed uint64, replay bool) {
	runs = def
	if v := os.Getenv("OPAMP_FAULT_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("OPAMP_FAULT_RUNS: %v", err)
		}
		runs = n
	}
	seed = uint64(time.Now().UnixNano())
	if v := os.Getenv("OPAMP_FAULT_SEED"); v != "" {
		s, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("OPAMP_FAULT_SEED: %v", err)
		}
		return 1, s, true
	}
	return runs, seed, false
}

// configServer offers remote config i in its answer to message i, for i < n, and records when
// every message came.
type configServer struct {
	mu    sync.Mutex
	name  string
	n     int
	times []time.Time
}

func (s *configServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m := &protobufs.AgentToServer{}
	if proto.Unmarshal(body, m) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	i := len(s.times)
	s.times = append(s.times, time.Now())
	s.mu.Unlock()
	a := &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}
	if i < s.n {
		cfg := fmt.Sprintf("%s config %d", s.name, i)
		a.RemoteConfig = &protobufs.AgentRemoteConfig{ConfigHash: []byte(cfg),
			Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte(cfg)}}}}
	}
	out, _ := proto.Marshal(a)
	_, _ = w.Write(out)
}

func (s *configServer) messages() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.times...)
}

// The saves of a client whose disk fails: an error (EIO), a write cut short (ENOSPC), a slow
// operation, a rename that lands but says it failed, in random operations of random saves, with
// a temporary file left by a crashed run or not. Whatever the plan: the client keeps
// heartbeating while a save is slow; every save gets one answer at most, and exactly one unless
// cancelled at stop; stop returns within its bound; the state file is whole, no older than the
// last save answered stored; the effects checker finds nothing wrong; and the next client on the file
// (no faults) starts from it and saves without failing.
func TestSavesUnderFileFaults(t *testing.T) {
	runs, seed, _ := faultEnv(t, 10)
	for i := range runs {
		s := seed + uint64(i)
		t.Run(fmt.Sprintf("seed=%d", s), func(t *testing.T) {
			if err := saveFaultRun(t, s); err != nil {
				t.Fatalf("replay: OPAMP_FAULT_SEED=%d go test ./opamp -run TestSavesUnderFileFaults: %v", s, err)
			}
		})
	}
}

func saveFaultRun(t *testing.T, seed uint64) error {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	dir := t.TempDir()
	state := filepath.Join(dir, "state.bin")
	configs := 1 + r.IntN(3)
	// The UID save, then one per config: some 6 operations each.
	ops := 6 * (1 + configs)
	plan := map[int]fault{}
	for range 1 + r.IntN(2) {
		f := fault{kind: faultKind(r.IntN(4))}
		if f.kind == faultSlow {
			f.slow = 500*time.Millisecond + time.Duration(r.Int64N(int64(1500*time.Millisecond)))
		}
		plan[r.IntN(ops)] = f
	}
	leftover := r.IntN(3) == 0
	stopInSlow := r.IntN(3) == 0
	t.Logf("configs %d, faults %v, leftover %v, stop in a slow operation %v", configs, plan, leftover, stopInSlow)
	if leftover {
		if err := os.WriteFile(tempFile(state), []byte("left by a crashed run"), 0o644); err != nil {
			return err
		}
	}
	fsys := newFaultFS(func(p string) bool { return p == state || p == tempFile(state) || p == dir }, plan)

	srv := &configServer{name: "a", n: configs}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	const stopTimeout = time.Second
	aTap := filepath.Join(dir, "a.tap")
	cfg := Config{Mode: ModeGo, Endpoint: ts.URL, HeartbeatInterval: 100 * time.Millisecond, StopTimeout: stopTimeout, StateFile: state,
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig), tapFile: aTap, fs: fsys}
	a, err := Start(context.Background(), cfg, Status{}, nil)
	if err != nil {
		return err
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = a.Stop(context.Background())
		}
	}()
	// Every save asked for answered (or until a slow operation starts, to stop in it).
	settled := eventually(15*time.Second, func() bool {
		if stopInSlow && len(fsys.slowBegan) > 0 {
			return true
		}
		// Every config offered, and taken some heartbeats ago (the core may save several at once).
		if len(srv.messages()) < configs+3 {
			return false
		}
		l := savesOf(t, readTapFile(t, aTap))
		for _, s := range l.stores {
			if len(l.answers[s.Id]) == 0 {
				return false
			}
		}
		return len(l.stores) > 0
	})
	if !settled {
		l := savesOf(t, readTapFile(t, aTap))
		return fmt.Errorf("the saves never all ended: %d stores, answers %v, injected %v", len(l.stores), l.answers, fsys.injected)
	}
	// Alive: still heartbeating.
	before := len(srv.messages())
	if !eventually(5*time.Second, func() bool { return len(srv.messages()) >= before+2 }) {
		return fmt.Errorf("no heartbeat after the faults (%v)", fsys.injected)
	}
	stopAsked := time.Now()
	_ = a.Stop(context.Background())
	stopped = true
	took := time.Since(stopAsked)
	fsys.mu.Lock()
	var maxSlow time.Duration
	for _, f := range plan {
		maxSlow = max(maxSlow, f.slow)
	}
	injected := append([]string(nil), fsys.injected...)
	windows := append([][2]time.Time(nil), fsys.windows...)
	fsys.mu.Unlock()
	t.Logf("injected %v; stop took %v", injected, took)
	if bound := stopTimeout + maxSlow + 3*time.Second; took > bound {
		return fmt.Errorf("stop took %v, over %v (injected %v)", took, bound, injected)
	}
	// What a stopped client left finishing ends before the next reads the file.
	workOn(state).settle()
	fsys.mu.Lock()
	windows = append([][2]time.Time(nil), fsys.windows...)
	fsys.mu.Unlock()
	// Heartbeats went on while a save was slow (one that ended before stop was asked for).
	msgs := srv.messages()
	for _, w := range windows {
		if w[1].After(stopAsked) || w[1].Sub(w[0]) < 500*time.Millisecond {
			continue
		}
		n := 0
		for _, m := range msgs {
			if m.After(w[0]) && m.Before(w[1]) {
				n++
			}
		}
		if n == 0 {
			return fmt.Errorf("no heartbeat in the %v a save was slow", w[1].Sub(w[0]))
		}
	}

	recs := readTapFile(t, aTap)
	l := savesOf(t, recs)
	for _, s := range l.stores {
		got := l.answers[s.Id]
		if len(got) > 1 || (len(got) == 0 && !l.cancelled[s.Id]) {
			return fmt.Errorf("store %d answered %v (cancelled: %v; injected %v)", s.Id, got, l.cancelled[s.Id], injected)
		}
	}
	content, err := os.ReadFile(state)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	v, err := l.version(content)
	if err != nil {
		return fmt.Errorf("the state file holds %v (injected %v)", err, injected)
	}
	if v < l.lastStore {
		return fmt.Errorf("the state file holds version %d, older than %d, answered stored (injected %v)", v, l.lastStore, injected)
	}
	if ran, out, err := opampTap(t, aTap); ran && err != nil {
		return fmt.Errorf("opamp-tap on the faulty client's tap (injected %v): %v\n%s", injected, err, out)
	}

	// The next client, on a disk that works: starts from the file, and its saves succeed (a
	// temporary file left by a failure in the way or not).
	srvB := &configServer{name: "b", n: 1}
	tsB := httptest.NewServer(srvB)
	defer tsB.Close()
	bTap := filepath.Join(dir, "b.tap")
	cfg.Endpoint, cfg.tapFile, cfg.fs = tsB.URL, bTap, nil
	b, err := Start(context.Background(), cfg, Status{}, nil)
	if err != nil {
		return err
	}
	ok := eventually(10*time.Second, func() bool {
		lb := savesOf(t, readTapFile(t, bTap))
		if len(srvB.messages()) < 2 || len(lb.stores) == 0 {
			return false
		}
		for _, s := range lb.stores {
			if len(lb.answers[s.Id]) == 0 {
				return false
			}
		}
		return true
	})
	_ = b.Stop(context.Background())
	lb := savesOf(t, readTapFile(t, bTap))
	if !bytes.Equal(lb.start, content) {
		return fmt.Errorf("the next client started from %d bytes, not the %d saved", len(lb.start), len(content))
	}
	if !ok {
		return fmt.Errorf("the next client's saves never ended: %v", lb.answers)
	}
	for id, got := range lb.answers {
		if len(got) != 1 || got[0] != "stored" {
			return fmt.Errorf("the next client's store %d: %v (injected before %v)", id, got, injected)
		}
	}
	if ran, out, err := opampTap(t, bTap); ran && err != nil {
		return fmt.Errorf("opamp-tap on the next client's tap: %v\n%s", err, out)
	}
	return nil
}

// A save whose directory sync fails (the rename landed, but may not reach the disk) is not
// stored: the core hears it failed.
func TestASaveWhoseDirectorySyncFailsIsNotStored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.bin")
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))
	x.setStateFile(path)
	// The sixth operation: remove, create, write, sync, close, rename, then syncdir.
	x.fs = newFaultFS(func(p string) bool { return p == path || p == tempFile(path) || p == dir }, map[int]fault{6: {kind: faultEIO}})
	x.do(&corepb.Effect{Kind: &corepb.Effect_Store{Store: &corepb.Store{Id: 3, Data: []byte("x")}}})
	if r := nextReport(t, x); r.in.GetStoreFailed().GetId() != 3 {
		t.Fatalf("a save whose rename may not reach the disk answered %v", r.in)
	}
}

// A download's file is written only while it is the download's: never after its removal (a
// cancel removing it while a write was under way), which would write a file the client no longer
// made.
func TestACancelledDownloadNeverWritesAfterItsFileIsRemoved(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.bin")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))
	x.setStateFile(state)
	var mu sync.Mutex
	var ops []fileOp
	held, release := make(chan struct{}), make(chan struct{})
	x.fs = journaled(&holdFirstWrite{held: held, release: release}, state, func(b []byte) {
		var o fileOp
		_ = json.Unmarshal(b, &o)
		mu.Lock()
		ops = append(ops, o)
		mu.Unlock()
	})
	x.do(&corepb.Effect{Kind: &corepb.Effect_Download{Download: &corepb.Download{Id: 8, Url: ts.URL, MaxBytes: 1 << 20, TimeoutMs: 60_000}}})
	within(t, "the first write", held)
	x.do(cancelEffect(8))
	close(release)
	x.work.settle()
	if _, err := os.Stat(x.downloadPath(8)); !os.IsNotExist(err) {
		t.Fatalf("the file is left: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	live := map[string]bool{}
	for _, o := range ops {
		switch o.op() {
		case "create":
			live[o.path()] = true
		case "remove":
			live[o.path()] = false
		case "write", "sync":
			if !live[o.path()] {
				t.Fatalf("%v on a file the client no longer made, in %v", o, ops)
			}
		}
	}
}

// holdFirstWrite is the disk, but for the first write to a download's file, which waits for
// release (saying so on held).
type holdFirstWrite struct {
	osFS
	held, release chan struct{}
	once          sync.Once
}

func (h *holdFirstWrite) createNew(path string) (writeFile, error) {
	f, err := h.osFS.createNew(path)
	if err != nil {
		return nil, err
	}
	return &heldFile{writeFile: f, h: h}, nil
}

type heldFile struct {
	writeFile
	h *holdFirstWrite
}

func (f *heldFile) Write(b []byte) (int, error) {
	f.h.once.Do(func() { close(f.h.held); <-f.h.release })
	return f.writeFile.Write(b)
}

// A file already where a download would write (left by a crashed run) is never written over nor
// handed over: the download's file is made new, or the download fails.
func TestAPartialDownloadIsNeverReused(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("the package")) }))
	defer ts.Close()
	x := newExecutor(testConnection(t, TLS{}, Proxy{}))
	x.setStateFile(filepath.Join(t.TempDir(), "state.bin"))
	if err := os.MkdirAll(downloadsDir(x.stateFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(x.downloadPath(5), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := x.download(context.Background(), &corepb.Download{Id: 5, Url: ts.URL, MaxBytes: 1 << 20, TimeoutMs: 5000})
	if in.GetDownloadFailed().GetId() != 5 {
		t.Fatalf("a download onto a file already there: %v", in)
	}
	if got, err := os.ReadFile(x.downloadPath(5)); err != nil || string(got) != "partial" {
		t.Fatalf("the file already there now holds %q (%v)", got, err)
	}

	// And a client clears what a run before left in the downloads' directory, at start.
	state := filepath.Join(t.TempDir(), "state.bin")
	left := filepath.Join(downloadsDir(state), "crashed-1")
	if err := os.MkdirAll(filepath.Dir(left), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(left, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Start(context.Background(), Config{Mode: ModeGo, Endpoint: "http://127.0.0.1:1", StateFile: state, AcceptPackages: true}, Status{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop(context.Background())
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Fatalf("a file left by a run before is still there: %v", err)
	}
}

// Downloads on a disk that fails: the downloads' directory can't be made, or a discard can't
// remove the file. The client keeps going (heartbeats), stop is bounded, nothing it couldn't
// remove survives the next start, and the next client (no faults) installs the package whole.
func TestDownloadsUnderFileFaults(t *testing.T) {
	runs, seed, _ := faultEnv(t, 4)
	for i := range runs {
		s := seed + uint64(i)
		t.Run(fmt.Sprintf("seed=%d", s), func(t *testing.T) {
			if err := downloadFaultRun(t, s); err != nil {
				t.Fatalf("replay: OPAMP_FAULT_SEED=%d go test ./opamp -run TestDownloadsUnderFileFaults: %v", s, err)
			}
		})
	}
}

func downloadFaultRun(t *testing.T, seed uint64) error {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	mkdirFails := r.IntN(2) == 0
	discardFails := !mkdirFails || r.IntN(2) == 0
	t.Logf("mkdir fails %v, the discard's remove fails %v", mkdirFails, discardFails)
	dir := t.TempDir()
	state := filepath.Join(dir, "state.bin")
	downloads := downloadsDir(state)
	file := bytes.Repeat([]byte("package "), 2000)
	sum := sha256.Sum256(file)
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(file) }))
	defer files.Close()
	srv := &offerServer{answer: func(*protobufs.AgentToServer) *protobufs.ServerToAgent {
		return &protobufs.ServerToAgent{PackagesAvailable: &protobufs.PackagesAvailable{
			Packages: map[string]*protobufs.PackageAvailable{"p": {Type: protobufs.PackageType_PackageType_TopLevel,
				Version: "1", Hash: []byte{1}, File: &protobufs.DownloadableFile{DownloadUrl: files.URL + "/p", ContentHash: sum[:]}}},
			AllPackagesHash: []byte{9}}}
	}}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// The mkdir is the first operation on the downloads; the discard's remove comes after the
	// create, the writes and the sync, the last operation on the file.
	var mu sync.Mutex
	var failed []string
	fsys := &downloadFaults{mkdirFails: mkdirFails, discardFails: discardFails, downloads: downloads, record: func(s string) {
		mu.Lock()
		failed = append(failed, s)
		mu.Unlock()
	}}
	installs := make(chan string, 4)
	notify := func(c *Client, n Notification) {
		if n.Kind == InstallPackage {
			got, err := os.ReadFile(n.Path)
			if err == nil && sha256.Sum256(got) != sum {
				err = errors.New("the file handed over isn't the package")
			}
			c.PackageInstalled(n.PackageName, n.Package.Hash, err)
			installs <- n.Path
		}
	}
	const stopTimeout = time.Second
	cfg := Config{Mode: ModeGo, Endpoint: ts.URL, HeartbeatInterval: 100 * time.Millisecond, StopTimeout: stopTimeout, StateFile: state,
		AcceptPackages: true, tapFile: filepath.Join(dir, "a.tap"), fs: fsys}
	a, err := Start(context.Background(), cfg, Status{}, notify)
	if err != nil {
		return err
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = a.Stop(context.Background())
		}
	}()
	hit := func(what string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, f := range failed {
			if f == what {
				return true
			}
		}
		return false
	}
	var leftover string
	if mkdirFails {
		if !eventually(10*time.Second, func() bool { return hit("mkdir") }) {
			return errors.New("the downloads' directory was never made")
		}
	} else {
		select {
		case leftover = <-installs:
		case <-time.After(10 * time.Second):
			return errors.New("no InstallPackage")
		}
		if !eventually(5*time.Second, func() bool { return hit("remove") }) {
			return errors.New("no discard")
		}
		if _, err := os.Stat(leftover); err != nil {
			return fmt.Errorf("the file a failed discard left: %v", err)
		}
	}
	before := len(srv.messages())
	if !eventually(5*time.Second, func() bool { return len(srv.messages()) >= before+2 }) {
		return fmt.Errorf("no heartbeat after the faults (%v)", failed)
	}
	began := time.Now()
	_ = a.Stop(context.Background())
	stopped = true
	if took := time.Since(began); took > stopTimeout+3*time.Second {
		return fmt.Errorf("stop took %v", took)
	}
	checkWithOpampTap(t, cfg.tapFile)

	// The next client, on a disk that works.
	cfg.fs, cfg.tapFile = nil, filepath.Join(dir, "b.tap")
	b, err := Start(context.Background(), cfg, Status{}, notify)
	if err != nil {
		return err
	}
	defer b.Stop(context.Background())
	if leftover != "" {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			return fmt.Errorf("the file a failed discard left survives the next start: %v", err)
		}
	}
	if mkdirFails {
		// Never installed: the next client downloads it, whole.
		select {
		case <-installs:
		case <-time.After(10 * time.Second):
			return errors.New("the next client never installed the package")
		}
	}
	return nil
}

// downloadFaults fails the first mkdir of the downloads' directory, and a remove of a download's
// file after it was synced (a discard's), with EIO.
type downloadFaults struct {
	osFS
	mkdirFails, discardFails bool
	downloads                string
	record                   func(string)
	mu                       sync.Mutex
	mkdirs                   int
	synced                   map[string]bool
}

func (d *downloadFaults) mkdir(dir string) error {
	d.mu.Lock()
	d.mkdirs++
	fail := d.mkdirFails && d.mkdirs == 1 && dir == d.downloads
	d.mu.Unlock()
	if fail {
		d.record("mkdir")
		return &os.PathError{Op: "mkdir", Path: dir, Err: errEIO}
	}
	return d.osFS.mkdir(dir)
}

func (d *downloadFaults) createNew(path string) (writeFile, error) {
	f, err := d.osFS.createNew(path)
	if err != nil {
		return nil, err
	}
	return &syncNoted{writeFile: f, d: d, path: path}, nil
}

func (d *downloadFaults) remove(path string) error {
	d.mu.Lock()
	fail := d.discardFails && d.synced[path]
	d.mu.Unlock()
	if fail {
		d.record("remove")
		return &os.PathError{Op: "remove", Path: path, Err: errEIO}
	}
	return d.osFS.remove(path)
}

type syncNoted struct {
	writeFile
	d    *downloadFaults
	path string
}

func (f *syncNoted) Sync() error {
	err := f.writeFile.Sync()
	if err == nil && filepath.Dir(f.path) == f.d.downloads {
		f.d.mu.Lock()
		if f.d.synced == nil {
			f.d.synced = map[string]bool{}
		}
		f.d.synced[f.path] = true
		f.d.mu.Unlock()
	}
	return err
}
