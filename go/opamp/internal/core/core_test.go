// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

func TestStartSendsTheFirstReport(t *testing.T) {
	ctx := context.Background()
	uid := bytes.Repeat([]byte{7}, 16)
	c, _, err := New(ctx, Options{}, &corepb.Config{InstanceUid: uid, Endpoint: "http://server/v1/opamp"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	out, err := c.Step(ctx, 0, 0, &corepb.Input{Kind: &corepb.Input_Start{Start: &corepb.Empty{}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Effects) != 1 {
		t.Fatalf("want one effect, got %v", out.Effects)
	}
	req := out.Effects[0].GetHttpRequest()
	if req == nil || req.Url != "http://server/v1/opamp" {
		t.Fatalf("want the first request, got %v", out.Effects[0])
	}
	msg := &protobufs.AgentToServer{}
	if err := proto.Unmarshal(req.Body, msg); err != nil || !bytes.Equal(msg.InstanceUid, uid) || msg.Capabilities&1 == 0 {
		t.Fatalf("want the first report, got %v (%v)", msg, err)
	}
	if out.WakeAtMs == nil {
		t.Fatal("no wake-up for the next poll")
	}

	// A 503 with Retry-After as a date, 30 s after the response came: nothing before then, and the
	// next request within the client's own spread after it (a quarter of the wait: 7.5 s).
	came := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	busy := &corepb.HttpResponse{
		Id:      req.Id,
		Status:  503,
		Headers: []*corepb.Header{{Name: "Retry-After", Value: came.Add(30 * time.Second).Format(http.TimeFormat)}},
		UnixMs:  uint64(came.UnixMilli()),
	}
	if _, err := c.Step(ctx, 10, 0, &corepb.Input{Kind: &corepb.Input_HttpResponse{HttpResponse: busy}}); err != nil {
		t.Fatal(err)
	}
	timer := &corepb.Input{Kind: &corepb.Input_Timer{Timer: &corepb.Empty{}}}
	step := func(at uint64) (*corepb.Output, bool) {
		out, err := c.Step(ctx, at, 0, timer)
		if err != nil {
			t.Fatal(err)
		}
		sent := false
		for _, e := range out.Effects {
			sent = sent || e.GetHttpRequest() != nil
		}
		return out, sent
	}
	var sent bool
	for _, at := range []uint64{30_000, 30_009} {
		if out, sent = step(at); sent {
			t.Fatalf("at %d ms: sent", at)
		}
	}
	now := uint64(30_009)
	for i := 0; i < 10 && !sent; i++ {
		wake := out.GetWakeAtMs()
		if wake <= now {
			t.Fatalf("wake at %d ms, now %d", wake, now)
		}
		if _, early := step(wake - 1); early {
			t.Fatalf("at %d ms: sent", wake-1)
		}
		out, sent = step(wake)
		now = wake
	}
	if !sent || now < 30_010 || now > 30_010+7_500 {
		t.Fatalf("sent %v, at %d ms", sent, now)
	}
}

func TestBadConfigIsAnError(t *testing.T) {
	if _, _, err := New(context.Background(), Options{}, &corepb.Config{InstanceUid: []byte{1, 2}}); err == nil {
		t.Fatal("a 2-byte UID was accepted")
	}
}

func TestCoresAreIndependent(t *testing.T) {
	ctx := context.Background()
	a, _, err := New(ctx, Options{}, &corepb.Config{InstanceUid: bytes.Repeat([]byte{1}, 16), Endpoint: "http://server/v1/opamp"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := New(ctx, Options{}, &corepb.Config{InstanceUid: bytes.Repeat([]byte{2}, 16), Endpoint: "http://server/v1/opamp"})
	if err != nil {
		t.Fatal(err)
	}
	start := &corepb.Input{Kind: &corepb.Input_Start{Start: &corepb.Empty{}}}
	oa, _ := a.Step(ctx, 0, 0, start)
	ob, _ := b.Step(ctx, 0, 0, start)
	if oa.InstanceUid[0] != 1 || ob.InstanceUid[0] != 2 {
		t.Fatalf("instances share state: %x %x", oa.InstanceUid, ob.InstanceUid)
	}
}

// With no instance UID, the core makes one from the entropy and the clock (a UUIDv7) and says
// which in init's output and every step's.
// covers: OPAMP-UID-01, OPAMP-UID-03
func TestTheCoreMakesTheUID(t *testing.T) {
	ctx := context.Background()
	ms := uint64(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC).UnixMilli())
	c, first, err := New(ctx, Options{}, &corepb.Config{Endpoint: "http://server/v1/opamp",
		Entropy: bytes.Repeat([]byte{0xab}, 32), UnixMs: ms})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	uid := first.GetInstanceUid()
	if len(uid) != 16 || uid[6]>>4 != 7 || uid[8]>>6 != 2 {
		t.Fatalf("not a UUIDv7: %x", uid)
	}
	var at uint64
	for _, b := range uid[:6] {
		at = at<<8 | uint64(b)
	}
	if at != ms {
		t.Fatalf("UUIDv7 time %d, want %d", at, ms)
	}
	out, err := c.Step(ctx, 0, 0, &corepb.Input{Kind: &corepb.Input_Start{Start: &corepb.Empty{}}})
	if err != nil || !bytes.Equal(out.InstanceUid, uid) {
		t.Fatalf("step says %x (%v), init said %x", out.GetInstanceUid(), err, uid)
	}
}

// tapRecord is one record of a core tap.
type tapRecord struct {
	kind  byte
	nanos uint64
	data  []byte
}

func readTap(t *testing.T, path string) []tapRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var recs []tapRecord
	for len(b) > 0 {
		if len(b) < 13 {
			t.Fatalf("%s: a torn record header", path)
		}
		n := binary.LittleEndian.Uint32(b[1:])
		if uint32(len(b)-13) < n {
			t.Fatalf("%s: a torn record", path)
		}
		recs = append(recs, tapRecord{kind: b[0], nanos: binary.LittleEndian.Uint64(b[5:]), data: b[13 : 13+n]})
		b = b[13+n:]
	}
	return recs
}

// The core tap: every call into the core, in and out, as it crossed; a second core in the process
// writes a file of its own; unset, no file.
func TestTheTapRecordsEveryCall(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "core.tap")
	t.Setenv("OPAMP_CORE_TAP", path)
	// The first core in the process writes path itself; this test may not be the first to tap.
	name := func(n uint64) string {
		if n == 0 {
			return path
		}
		return fmt.Sprintf("%s.%d.%d", path, os.Getpid(), n)
	}
	firstName, secondName := name(tapped.Load()), name(tapped.Load()+1)

	cfg := &corepb.Config{InstanceUid: bytes.Repeat([]byte{7}, 16), Endpoint: "http://server/v1/opamp"}
	c, _, err := New(ctx, Options{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	start := &corepb.Input{Kind: &corepb.Input_Start{Start: &corepb.Empty{}}}
	if _, err := c.Step(ctx, 5, 1234, start); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	recs := readTap(t, firstName)
	if len(recs) != 4 {
		t.Fatalf("%d records, want 4", len(recs))
	}
	for i, r := range recs {
		if r.kind != byte(i+1) {
			t.Fatalf("record %d is kind %d", i, r.kind)
		}
		if i > 0 && r.nanos < recs[i-1].nanos {
			t.Fatalf("record %d goes back in time", i)
		}
	}
	if gotCfg := (&corepb.Config{}); proto.Unmarshal(recs[0].data, gotCfg) != nil || !proto.Equal(gotCfg, cfg) {
		t.Fatalf("init in: %x", recs[0].data)
	}
	step := &corepb.Step{}
	if err := proto.Unmarshal(recs[2].data, step); err != nil || !proto.Equal(step, &corepb.Step{NowMs: 5, Input: start, UnixMs: 1234}) {
		t.Fatalf("step in: %v (%v)", step, err)
	}
	for _, i := range []int{1, 3} {
		reply := &corepb.Reply{}
		if err := proto.Unmarshal(recs[i].data, reply); err != nil || reply.Output == nil {
			t.Fatalf("record %d: not a reply: %v", i, err)
		}
	}

	// A second core: a file of its own.
	c2, _, err := New(ctx, Options{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = c2.Close(ctx)
	if recs := readTap(t, secondName); len(recs) != 2 || recs[0].kind != 1 || recs[1].kind != 2 {
		t.Fatalf("the second core's tap: %v", recs)
	}
	if recs := readTap(t, firstName); len(recs) != 4 {
		t.Fatalf("the second core wrote to the first's tap: %d records", len(recs))
	}

	// Unset: no file, and nothing counted.
	t.Setenv("OPAMP_CORE_TAP", "")
	before := tapped.Load()
	off := t.TempDir()
	t.Chdir(off)
	c3, _, err := New(ctx, Options{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c3.tap != nil || tapped.Load() != before {
		t.Fatal("tapped with the variable unset")
	}
	_ = c3.Close(ctx)
	if entries, _ := os.ReadDir(off); len(entries) != 0 {
		t.Fatalf("files made with the tap off: %v", entries)
	}
}

// A tap that can't be opened: said on stderr, and the core runs without it.
func TestAnUnopenableTapIsSkipped(t *testing.T) {
	t.Setenv("OPAMP_CORE_TAP", filepath.Join(t.TempDir(), "missing", "core.tap"))
	ctx := context.Background()
	c, _, err := New(ctx, Options{}, &corepb.Config{InstanceUid: bytes.Repeat([]byte{7}, 16), Endpoint: "http://server/v1/opamp"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if c.tap != nil {
		t.Fatal("a tap with no file")
	}
	if _, err := c.Step(ctx, 0, 0, &corepb.Input{Kind: &corepb.Input_Start{Start: &corepb.Empty{}}}); err != nil {
		t.Fatal(err)
	}
}

// Compiling the core makes about 12 MB of garbage (wazero's compiler). It goes back to the
// operating system at once, not whenever the Go runtime gets round to it: a small container (the
// demo's Go apps run in 40 MB) would otherwise carry it for minutes. Run in a process of its own,
// since the core is compiled once per process.
func TestCompilingTheCoreLeavesNoGarbageResident(t *testing.T) {
	const child = "OPAMP_CORE_COMPILE_CHILD"
	if os.Getenv(child) != "" {
		compileCore()
		var m goruntime.MemStats
		goruntime.ReadMemStats(&m)
		fmt.Printf("resident heap %d\n", m.HeapInuse+m.HeapIdle-m.HeapReleased)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCompilingTheCoreLeavesNoGarbageResident$", "-test.count=1")
	cmd.Env = append(os.Environ(), child+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var resident uint64
	if i := bytes.Index(out, []byte("resident heap ")); i < 0 {
		t.Fatalf("no measurement:\n%s", out)
	} else if _, err := fmt.Sscan(string(out[i+len("resident heap "):]), &resident); err != nil {
		t.Fatal(err)
	}
	if resident > 6<<20 {
		t.Fatalf("%d MB of heap resident after compiling the core", resident>>20)
	}
}
