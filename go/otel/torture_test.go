// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/pydantic/opamp-clients/go/opamp/sdk/settingspb"
)

// The settings flipped while the application runs: one applier goes through a seeded sequence
// of settings documents as fast as it can, while workers start spans (roots and children, with
// attributes, events and links) and emit log records through the real SDK, its batch processors
// and Controls' gates. What must hold whatever the interleaving is checked on everything the
// gates export; then, the flipping over, a last document is applied and everything after it is
// held to what the layer (Evaluate) says of that document alone.
//
// OPAMP_PROP_RUNS sets the number of rounds (default 3, each half a second of flipping),
// OPAMP_PROP_SEED replays a failure's seed.
//
// What is checked during the flipping, and what can't be:
//   - no panic in the gates or the sampler, and no deadlock: each round is bounded, and a stuck
//     one fails with every goroutine's stack;
//   - secret.token, when every document of the round that sends traces drops it, is on no
//     exported span, event or link; likewise log records, of the documents that send logs. A
//     document that doesn't send a signal is held to nothing on it, whatever it drops: a gate
//     that reads one document's send_* and another's redaction leaks what neither would;
//   - user.email, which no document drops, is on every exported list, and as "[REDACTED]" alone
//     when every document of the round masks it;
//   - a child of a span sampled in this process is sampled, whatever the trace cap;
//   - with send_logs false in every document of the round, no log record is exported;
//   - flip.a and flip.b, which every document redacts alike, are exported alike in each list:
//     both gone, both masked or both as they were (the gates load one document's redaction a
//     batch, so a list is never redacted half under one and half under another).
//
// Not checkable while flipping: which document a given span or record was judged under (the
// sampler decides at the span's start, the gates at export, a batch later, and the applier may
// have moved on in between), so rates, levels and caps are only checked against the last
// document, where nothing moves. Nor can the cap's counts be, for the same reason: a bucket
// kept across a re-applied cap and refilled on the test's clock is right whichever document's
// spans took its tokens. Metrics aren't emitted (send_metrics flips all the same): their gate
// redacts nothing and holds no state across an export. A debug window is decided by the layer
// at the vectors' clock when a document is loaded; its end, the agent's timer, isn't Controls'.
func TestSettingsFlippedWhileTelemetryFlows(t *testing.T) {
	runs := 3
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
	t.Logf("seed %d (replay: OPAMP_PROP_SEED=%d)", seed, seed)
	for i := range runs {
		if err := tortureRound(t, seed, uint64(i)); err != nil {
			t.Fatalf("round %d (replay: OPAMP_PROP_SEED=%d): %v", i, seed, err)
		}
	}
}

// tortureDoc is the settings of a round, by the names the layer hands them over with, and what the
// round's invariants need of them. json gives the policy that sends them.
type tortureDoc struct {
	doc         map[string]any
	settings    *settingspb.Settings
	dropsSecret bool // drop_attributes matches secret.token
	masksEmail  bool // mask_attributes matches user.email (no drop pattern ever does)
	tracesOff   bool // send_traces false
	logsOff     bool // send_logs false
}

// json is the policy that sends doc's settings: each setting is the override of its control, and
// a control none of doc's settings names is local.
func (d tortureDoc) json() []byte {
	num := func(key string) any {
		f, err := strconv.ParseFloat(d.doc[key].(string), 64)
		if err != nil {
			panic(err)
		}
		return f
	}
	list := func(key string) []string { return strings.Split(d.doc[key].(string), ",") }
	controls := map[string]map[string]any{}
	set := func(control, setting, field string, value func(string) any) {
		if _, ok := d.doc[setting]; !ok {
			return
		}
		if controls[control] == nil {
			controls[control] = map[string]any{"mode": "override"}
		}
		controls[control][field] = value(setting)
	}
	as := func(key string) any { return d.doc[key] }
	set("head_sampling", "sampling_rate", "ratio", num)
	set("sampling_rules", "sampling_rules", "rules", as)
	set("trace_rate_limit", "sampling_traces_per_second", "traces_per_second", num)
	set("log_rate_limit", "send_logs_per_second", "records_per_second", num)
	set("log_levels", "send_logs_level", "level", as)
	set("log_levels", "send_logs_level_by_scope", "scopes", func(key string) any {
		scopes := map[string]string{}
		for _, e := range list(key) {
			pattern, level, _ := strings.Cut(e, "=")
			scopes[pattern] = level
		}
		return scopes
	})
	set("attribute_redaction", "drop_attributes", "drop", func(key string) any { return list(key) })
	set("attribute_redaction", "mask_attributes", "mask", func(key string) any { return list(key) })
	set("debug", "debug_until", "until", as)
	set("debug", "debug_sampling_rate", "sampling_ratio", num)
	set("debug", "debug_send_logs_level", "log_level", as)
	var pairs []string
	for control, fields := range controls {
		b, _ := json.Marshal(fields)
		pairs = append(pairs, control, string(b))
	}
	exportsSet := false
	signal := func(key string) string {
		v, ok := d.doc[key]
		if !ok {
			return ""
		}
		exportsSet = true
		if v == true {
			return "on"
		}
		return "off"
	}
	if e := exports(signal("send_traces"), signal("send_metrics"), signal("send_logs")); exportsSet {
		pairs = append(pairs, "signal_exports", e)
	}
	return []byte(every(pairs...))
}

// pick is one of the choices, at random.
func pick[T any](r *rand.Rand, choices ...T) T { return choices[r.IntN(len(choices))] }

// Sampling rules that overlap (a server span named GET at /health matches four) and that the
// generator puts in any order.
var tortureRules = []map[string]any{
	{"rate": 0, "name": "checkout*"},
	{"rate": 1, "name": "checkout cart"},
	{"rate": 0.5, "kind": "server"},
	{"rate": 0, "kind": "server", "path": "/health*"},
	{"rate": 1, "attributes": map[string]string{"tier": "gold"}},
	{"rate": 1, "name": "GET"},
	{"rate": 0},
}

var tortureScopeLevels = []string{"app.db*=debug", "app.db=error", "noisy=off", "app*=warn", "*=info"}

var tortureLevels = []string{"trace", "debug", "info", "warn", "error", "fatal"}

// genDoc is the settings of a policy; strict says the round's invariants hold in it (secret.token
// dropped where a signal is sent, user.email masked), logsOff that it sends no logs. traceCaps and
// logCaps are the caps it may set: the last document's are ones the flipping never set, so that it
// starts full buckets.
func genDoc(r *rand.Rand, strict, logsOff bool, traceCaps, logCaps []string) tortureDoc {
	d := tortureDoc{doc: map[string]any{}}
	// A quiet document sends neither traces nor logs, and so may leave secret.token alone, even in
	// a strict round: applied over it, a document that sends and drops it must not leak it.
	quiet := strict && r.IntN(4) == 0
	if r.IntN(3) > 0 {
		d.doc["sampling_rate"] = pick(r, "0", "0.25", "1")
	}
	if n := r.IntN(5); n > 0 {
		var rules []map[string]any
		for _, i := range r.Perm(len(tortureRules))[:n] {
			rules = append(rules, tortureRules[i])
		}
		d.doc["sampling_rules"] = rules
	}
	if r.IntN(2) == 0 {
		d.doc["sampling_traces_per_second"] = pick(r, traceCaps...)
	}
	if r.IntN(2) == 0 {
		d.doc["send_logs_per_second"] = pick(r, logCaps...)
	}
	for _, signal := range []string{"send_traces", "send_metrics"} {
		if r.IntN(3) == 0 {
			d.doc[signal] = r.IntN(4) > 0
		}
	}
	if quiet {
		d.doc["send_traces"] = false
	}
	d.tracesOff = d.doc["send_traces"] == false
	switch {
	case logsOff || quiet:
		d.doc["send_logs"] = false
	case r.IntN(3) == 0:
		d.doc["send_logs"] = r.IntN(4) > 0
	}
	d.logsOff = d.doc["send_logs"] == false
	if r.IntN(2) == 0 {
		d.doc["send_logs_level"] = pick(r, tortureLevels...)
	}
	var scopes []string
	for _, i := range r.Perm(len(tortureScopeLevels))[:r.IntN(len(tortureScopeLevels)+1)] {
		scopes = append(scopes, tortureScopeLevels[i])
	}
	if len(scopes) > 0 {
		d.doc["send_logs_level_by_scope"] = strings.Join(scopes, ",")
	}
	// The redaction lists grow and shrink; no drop pattern matches user.email, and flip.a and
	// flip.b are always matched alike.
	var drop, mask []string
	if strict && !quiet || r.IntN(2) == 0 {
		drop, d.dropsSecret = append(drop, pick(r, "secret.*", "SECRET.TOKEN", "*.token", "secret.token")), true
	}
	if strict || r.IntN(2) == 0 {
		mask, d.masksEmail = append(mask, pick(r, "user.email", "USER.*", "*email", "user.e*l")), true
	}
	switch r.IntN(4) {
	case 1:
		drop = append(drop, "flip.*")
	case 2:
		mask = append(mask, "FLIP.*")
	case 3:
		drop, mask = append(drop, "flip.*"), append(mask, "flip.*")
	}
	for _, p := range []string{"k.1", "K.*2", "*.3"} {
		if r.IntN(3) == 0 {
			drop = append(drop, p)
		}
		if r.IntN(3) == 0 {
			mask = append(mask, p)
		}
	}
	if len(drop) > 0 {
		d.doc["drop_attributes"] = strings.Join(drop, ",")
	}
	if len(mask) > 0 {
		d.doc["mask_attributes"] = strings.Join(mask, ",")
	}
	// Debug windows: in force (an hour ahead of the vectors' clock) or over (an hour before).
	if r.IntN(4) == 0 {
		d.doc["debug_until"] = pick(r, "2026-10-14T13:00:00Z", "2026-10-14T11:00:00Z")
		if r.IntN(2) == 0 {
			d.doc["debug_sampling_rate"] = pick(r, "0", "1")
		}
		if r.IntN(2) == 0 {
			d.doc["debug_send_logs_level"] = pick(r, "trace", "error")
		}
	}
	return d
}

// tortureAttrs are the attributes every span, event, link and record carries: one to drop, one
// to mask, a pair redacted alike, others redacted now and then, and seq, which nothing redacts.
func tortureAttrs(r *rand.Rand, seq int64) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("secret.token", "s3cr3t"),
		attribute.String("user.email", "a@b.c"),
		attribute.Int64("flip.a", seq),
		attribute.Int64("flip.b", seq),
		attribute.String("k.1", "one"),
		attribute.Int64("k.2", 2),
		attribute.String("k.3", pick(r, "x", "y")),
		attribute.Int64("seq", seq),
	}
}

// tortureSpan is what a span starts with: the rules match on its name, kind and attributes.
type tortureSpan struct {
	name  string
	kind  trace.SpanKind
	attrs []attribute.KeyValue
}

func genSpan(r *rand.Rand, seq int64) tortureSpan {
	attrs := append(tortureAttrs(r, seq), attribute.String("url.path", pick(r, "/health", "/api")))
	if r.IntN(2) == 0 {
		attrs = append(attrs, attribute.String("tier", pick(r, "gold", "silver")))
	}
	return tortureSpan{name: pick(r, "checkout cart", "checkout", "GET", "work"),
		kind: pick(r, trace.SpanKindServer, trace.SpanKindInternal, trace.SpanKindClient), attrs: attrs}
}

var tortureLogScopes = []string{"app", "app.db", "app.dbx", "noisy", "other"}

// remoteParent is a context holding a caller's sampled span.
func remoteParent(r *rand.Rand) context.Context {
	var tid trace.TraceID
	var sid trace.SpanID
	binary.BigEndian.PutUint64(tid[:8], r.Uint64()|1)
	binary.BigEndian.PutUint64(tid[8:], r.Uint64())
	binary.BigEndian.PutUint64(sid[:], r.Uint64()|1)
	return trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true}))
}

// tortureCheck is what the round's documents all say, and the first violation seen.
type tortureCheck struct {
	// tracesDropSecret and logsDropSecret: every document sending the signal drops secret.token.
	tracesDropSecret, logsDropSecret bool
	alwaysMasksEmail, alwaysLogsOff  bool
	spans, records                   atomic.Int64 // exported and checked
	mu                               sync.Mutex
	errs                             []string
}

func (c *tortureCheck) fail(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.errs) < 5 {
		c.errs = append(c.errs, fmt.Sprintf(format, args...))
	}
}

// reset forgets what the round's documents all say, keeping what failed.
func (c *tortureCheck) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tracesDropSecret, c.logsDropSecret, c.alwaysMasksEmail, c.alwaysLogsOff = false, false, false, false
}

func (c *tortureCheck) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(c.errs, "\n"))
}

// list checks one exported attribute list against the invariants; dropsSecret is whether every
// document sending its signal drops secret.token.
func (c *tortureCheck) list(what string, dropsSecret bool, attrs []attribute.KeyValue) {
	values := map[attribute.Key]attribute.Value{}
	for _, kv := range attrs {
		values[kv.Key] = kv.Value
	}
	if _, ok := values["secret.token"]; ok && dropsSecret {
		c.fail("%s: secret.token exported, every document sending it drops it: %s", what, show(attrs))
	}
	if email, ok := values["user.email"]; !ok {
		c.fail("%s: user.email missing, no document drops it: %s", what, show(attrs))
	} else if c.alwaysMasksEmail && email.Emit() != mask {
		c.fail("%s: user.email exported as %q, every document masks it", what, email.Emit())
	}
	a, aok := values["flip.a"]
	b, bok := values["flip.b"]
	if aok != bok || aok && a.Type() != b.Type() {
		c.fail("%s: flip.a and flip.b redacted under different documents: %s", what, show(attrs))
	}
}

// checkSpans is a span exporter after the gate: it checks every span against the invariants
// and, while keep is set, keeps them.
type checkSpans struct {
	check *tortureCheck
	keep  atomic.Bool
	mu    sync.Mutex
	got   []sdktrace.ReadOnlySpan
}

func (e *checkSpans) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.check.spans.Add(int64(len(spans)))
	for _, s := range spans {
		drops := e.check.tracesDropSecret
		e.check.list("span "+s.Name(), drops, s.Attributes())
		for _, ev := range s.Events() {
			e.check.list("event of "+s.Name(), drops, ev.Attributes)
		}
		for _, l := range s.Links() {
			e.check.list("link of "+s.Name(), drops, l.Attributes)
		}
	}
	if e.keep.Load() {
		e.mu.Lock()
		e.got = append(e.got, spans...)
		e.mu.Unlock()
	}
	return nil
}

func (e *checkSpans) Shutdown(context.Context) error { return nil }

// checkRecords is checkSpans' log exporter.
type checkRecords struct {
	check *tortureCheck
	keep  atomic.Bool
	mu    sync.Mutex
	got   []sdklog.Record
}

func (e *checkRecords) Export(_ context.Context, records []sdklog.Record) error {
	if len(records) > 0 && e.check.alwaysLogsOff {
		e.check.fail("%d log records exported, every document has send_logs false", len(records))
	}
	e.check.records.Add(int64(len(records)))
	for _, r := range records {
		e.check.list(fmt.Sprintf("log record of %s", r.InstrumentationScope().Name), e.check.logsDropSecret, recordAttrs(r))
	}
	if e.keep.Load() {
		e.mu.Lock()
		for _, r := range records {
			e.got = append(e.got, r.Clone())
		}
		e.mu.Unlock()
	}
	return nil
}

func (e *checkRecords) ForceFlush(context.Context) error { return nil }
func (e *checkRecords) Shutdown(context.Context) error   { return nil }

func recordAttrs(r sdklog.Record) []attribute.KeyValue {
	var out []attribute.KeyValue
	r.WalkAttributes(func(kv attribute.KeyValue) bool { out = append(out, kv); return true })
	return out
}

// noPanicSpans and noPanicLogs turn a panic in the gate, which runs on a batch processor's
// goroutine, into a failure of the round (with its seed) in place of the test binary's crash.
type noPanicSpans struct {
	sdktrace.SpanExporter
	check *tortureCheck
}

func (e noPanicSpans) ExportSpans(ctx context.Context, s []sdktrace.ReadOnlySpan) (err error) {
	defer func() {
		if p := recover(); p != nil {
			e.check.fail("the span gate panicked: %v", p)
		}
	}()
	return e.SpanExporter.ExportSpans(ctx, s)
}

type noPanicLogs struct {
	sdklog.Exporter
	check *tortureCheck
}

func (e noPanicLogs) Export(ctx context.Context, r []sdklog.Record) (err error) {
	defer func() {
		if p := recover(); p != nil {
			e.check.fail("the log gate panicked: %v", p)
		}
	}()
	return e.Exporter.Export(ctx, r)
}

// tortureRound is one round: its documents, the flipping, then the last document.
func tortureRound(t *testing.T, seed, round uint64) error {
	r := rand.New(rand.NewPCG(seed, round))
	// A round of each kind in turn: every document redacting, every document redacting and
	// sending no logs, and anything goes (every control local included).
	strict, logsOff := round%3 != 2, round%3 == 1
	var pool []tortureDoc
	for range 24 {
		pool = append(pool, genDoc(r, strict, logsOff, []string{"1", "2", "5", "1000"}, []string{"1", "4", "1000"}))
	}
	if !strict {
		pool = append(pool, tortureDoc{doc: map[string]any{}})
	}
	for i := range pool {
		pool[i].settings = settingsOf(t, pool[i].json(), 0)
	}
	sequence := make([]int, 400)
	for i := range sequence {
		sequence[i] = r.IntN(len(pool))
	}
	check := &tortureCheck{tracesDropSecret: true, logsDropSecret: true, alwaysMasksEmail: true, alwaysLogsOff: true}
	for _, d := range pool {
		check.tracesDropSecret = check.tracesDropSecret && (d.dropsSecret || d.tracesOff)
		check.logsDropSecret = check.logsDropSecret && (d.dropsSecret || d.logsOff)
		check.alwaysMasksEmail = check.alwaysMasksEmail && d.masksEmail
		check.alwaysLogsOff = check.alwaysLogsOff && d.logsOff
	}

	c := NewControls()
	clock := withClock(c)
	spans, records := &checkSpans{check: check}, &checkRecords{check: check}
	bsp := sdktrace.NewBatchSpanProcessor(noPanicSpans{c.Spans(spans), check},
		sdktrace.WithBatchTimeout(2*time.Millisecond), sdktrace.WithMaxExportBatchSize(32))
	sdkTP := sdktrace.NewTracerProvider(sdktrace.WithSampler(c.Sampler), sdktrace.WithSpanProcessor(bsp))
	blp := sdklog.NewBatchProcessor(noPanicLogs{c.Logs(records), check},
		sdklog.WithExportInterval(2*time.Millisecond), sdklog.WithExportMaxBatchSize(32))
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(blp))
	defer func() { _ = sdkTP.Shutdown(context.Background()); _ = lp.Shutdown(context.Background()) }()
	tp := c.TracerProvider(sdkTP)
	c.Apply(pool[sequence[0]].settings) // before any telemetry: every span is under a round's document

	flipping := 500 * time.Millisecond
	done, flips := make(chan struct{}), 0
	go func() {
		defer close(done)
		var stop atomic.Bool
		var wg sync.WaitGroup
		for w := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if p := recover(); p != nil {
						check.fail("worker %d panicked: %v", w, p)
					}
				}()
				tortureWorker(rand.New(rand.NewPCG(seed, round<<8|uint64(w+1))), tp, lp, check, &stop)
			}()
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					check.fail("Apply panicked: %v", p)
				}
			}()
			deadline := time.Now().Add(flipping)
			for ; time.Now().Before(deadline); flips++ {
				clock.ns.Add(int64(r.IntN(300)) * int64(time.Millisecond))
				c.Apply(pool[sequence[(flips+1)%len(sequence)]].settings)
				if r.IntN(4) == 0 {
					time.Sleep(time.Duration(r.IntN(500)) * time.Microsecond)
				}
			}
		}()
		stop.Store(true)
		wg.Wait()
		_ = sdkTP.ForceFlush(context.Background())
		_ = lp.ForceFlush(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(flipping + 60*time.Second):
		_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		return fmt.Errorf("stuck: the flipping didn't end (every goroutine's stack is above)")
	}
	if err := check.err(); err != nil {
		return fmt.Errorf("while flipping: %w", err)
	}
	t.Logf("round %d: %d documents applied, %d spans and %d log records exported and checked (secret.token dropped "+
		"where traces are sent %v, where logs are %v; user.email always masked %v, logs always off %v)", round, flips+1,
		check.spans.Load(), check.records.Load(), check.tracesDropSecret, check.logsDropSecret, check.alwaysMasksEmail,
		check.alwaysLogsOff)

	// The last document, with caps the flipping never set: everything after it follows it.
	last := genDoc(r, r.IntN(2) == 0, false, []string{"3", "7"}, []string{"6", "9"})
	if r.IntN(8) == 0 {
		last = tortureDoc{doc: map[string]any{}}
	}
	last.settings = settingsOf(t, last.json(), 0)
	check.reset() // nothing is promised of the last document but itself
	spans.keep.Store(true)
	records.keep.Store(true)
	c.Apply(last.settings)
	if err := followsTheLastDocument(t, r, last, tp, sdkTP, lp, spans, records); err != nil {
		return fmt.Errorf("after %s: %w", last.json(), err)
	}
	return check.err()
}

// tortureWorker starts spans and emits records until stop: a root with an event and a link, its
// child (sampled when the root is, whatever the cap), now and then a span joining a caller's
// sampled trace, and a log record.
func tortureWorker(r *rand.Rand, tp trace.TracerProvider, lp *sdklog.LoggerProvider, check *tortureCheck, stop *atomic.Bool) {
	tracer := tp.Tracer("torture")
	for seq := int64(0); !stop.Load(); seq++ {
		s := genSpan(r, seq)
		other := trace.SpanContextFromContext(remoteParent(r))
		ctx, root := tracer.Start(context.Background(), s.name, trace.WithSpanKind(s.kind), trace.WithAttributes(s.attrs...),
			trace.WithLinks(trace.Link{SpanContext: other, Attributes: tortureAttrs(r, seq)}))
		root.AddEvent("e", trace.WithAttributes(tortureAttrs(r, seq)...))
		_, child := tracer.Start(ctx, "child", trace.WithAttributes(tortureAttrs(r, seq)...),
			trace.WithLinks(trace.Link{SpanContext: other, Attributes: tortureAttrs(r, seq)}))
		if root.SpanContext().IsSampled() && !child.SpanContext().IsSampled() {
			check.fail("the child of a sampled local span %q was dropped", s.name)
		}
		child.AddEvent("e", trace.WithAttributes(tortureAttrs(r, seq)...))
		child.End()
		root.End()
		if r.IntN(4) == 0 {
			_, joined := tracer.Start(remoteParent(r), s.name, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(s.attrs...))
			joined.End()
		}
		var rec log.Record
		rec.SetSeverity(log.Severity(r.IntN(25)))
		rec.SetBody(attribute.Int64Value(seq))
		rec.AddAttributes(tortureAttrs(r, seq)...)
		lp.Logger(pick(r, tortureLogScopes...)).Emit(context.Background(), rec)
	}
}

// wire is attributes as the layer takes them (strings and integers: all the test makes).
func wire(attrs []attribute.KeyValue) []*protobufs.KeyValue {
	var out []*protobufs.KeyValue
	for _, kv := range attrs {
		v := &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: kv.Value.AsString()}}
		if kv.Value.Type() == attribute.INT64 {
			v = &protobufs.AnyValue{Value: &protobufs.AnyValue_IntValue{IntValue: kv.Value.AsInt64()}}
		}
		out = append(out, &protobufs.KeyValue{Key: string(kv.Key), Value: v})
	}
	return out
}

// tortureOracle is what the layer says of the last document.
type tortureOracle struct {
	t   *testing.T
	doc []byte
}

func (o tortureOracle) evaluate(e *settingspb.Evaluate) (*settingspb.Evaluated, error) {
	e.Settings, e.Host, e.NowUnixMs = o.doc, goHost(), vectorNow
	got, err := testLayer(o.t).Evaluate(context.Background(), e)
	if err == nil && got.GetRefused() != "" {
		err = fmt.Errorf("the layer refused it: %s", got.GetRefused())
	}
	return got, err
}

// redacted is the attributes as the layer redacts them, as show writes them.
func (o tortureOracle) redacted(attrs []attribute.KeyValue) (string, error) {
	got, err := o.evaluate(&settingspb.Evaluate{Input: &settingspb.Evaluate_Attributes{
		Attributes: &settingspb.Attributes{Attributes: wire(attrs)}}})
	if err != nil {
		return "", err
	}
	var out []string
	for _, kv := range got.GetRedacted() {
		if s, ok := kv.GetValue().GetValue().(*protobufs.AnyValue_StringValue); ok {
			out = append(out, fmt.Sprintf("%s=STRING:%s", kv.GetKey(), s.StringValue))
		} else {
			out = append(out, fmt.Sprintf("%s=INT64:%d", kv.GetKey(), kv.GetValue().GetIntValue()))
		}
	}
	return strings.Join(out, " "), nil
}

// sampled is whether the rules, the rate or the parent sample the span, before the cap.
func (o tortureOracle) sampled(s tortureSpan, parent settingspb.Parent, id trace.TraceID) (bool, error) {
	got, err := o.evaluate(&settingspb.Evaluate{Input: &settingspb.Evaluate_Span{Span: &settingspb.SpanStart{
		Name: s.name, Kind: settingspb.SpanKind(s.kind), Attributes: wire(s.attrs), Parent: parent}}})
	if err != nil {
		return false, err
	}
	switch how := got.GetSampling(); how.GetHow() {
	case settingspb.SpanSampling_RATE:
		return ratioSamples(how.GetRate(), binary.BigEndian.Uint64(id[8:])), nil
	case settingspb.SpanSampling_FOLLOW_PARENT:
		return how.GetParentSampled(), nil
	default: // the application's sampler: the SDK's default, parent-based always-on
		return parent == settingspb.Parent_PARENT_NONE || parent == settingspb.Parent_PARENT_REMOTE_SAMPLED ||
			parent == settingspb.Parent_PARENT_LOCAL_SAMPLED, nil
	}
}

// followsTheLastDocument emits spans and records one after another, the clock still, and holds
// each decision and each exported attribute list to what the layer says of the last document:
// sampling, the trace cap (a new one: its first n roots and joined traces), send_traces, the
// levels, the log cap (its first n records kept), send_logs, redaction.
func followsTheLastDocument(t *testing.T, r *rand.Rand, last tortureDoc, tp trace.TracerProvider, sdkTP *sdktrace.TracerProvider,
	lp *sdklog.LoggerProvider, spans *checkSpans, records *checkRecords) error {
	o := tortureOracle{t: t, doc: last.json()}
	tracer := tp.Tracer("torture")
	traceTokens, logTokens := -1, -1 // -1: no cap
	if n := last.settings.SamplingTracesPerSecond; n != nil {
		traceTokens = int(max(*n, 1))
	}
	if n := last.settings.SendLogsPerSecond; n != nil {
		logTokens = int(max(*n, 1))
	}
	// capped is whether a span the sampler would sample is dropped by the trace cap.
	capped := func(takesToken bool) bool {
		if !takesToken || traceTokens < 0 {
			return false
		}
		if traceTokens == 0 {
			return true
		}
		traceTokens--
		return false
	}
	type started struct {
		sc    trace.SpanContext
		attrs []attribute.KeyValue // its own, its event's and its link's
	}
	var want []started
	for seq := int64(0); seq < 40; seq++ {
		s := genSpan(r, seq)
		other := trace.SpanContextFromContext(remoteParent(r))
		ctx, root := tracer.Start(context.Background(), s.name, trace.WithSpanKind(s.kind), trace.WithAttributes(s.attrs...),
			trace.WithLinks(trace.Link{SpanContext: other, Attributes: s.attrs}))
		root.AddEvent("e", trace.WithAttributes(s.attrs...))
		_, child := tracer.Start(ctx, "child", trace.WithAttributes(s.attrs...),
			trace.WithLinks(trace.Link{SpanContext: other, Attributes: s.attrs}))
		child.AddEvent("e", trace.WithAttributes(s.attrs...))
		child.End()
		root.End()
		rootWant, err := o.sampled(s, settingspb.Parent_PARENT_NONE, root.SpanContext().TraceID())
		if err != nil {
			return err
		}
		rootWant = rootWant && !capped(true)
		if root.SpanContext().IsSampled() != rootWant {
			return fmt.Errorf("root %d %q %v %s: sampled %v, want %v", seq, s.name, s.kind, show(s.attrs), !rootWant, rootWant)
		}
		parent := settingspb.Parent_PARENT_LOCAL_UNSAMPLED
		if rootWant {
			parent = settingspb.Parent_PARENT_LOCAL_SAMPLED
		}
		childWant, err := o.sampled(tortureSpan{name: "child", kind: trace.SpanKindInternal, attrs: s.attrs}, parent,
			root.SpanContext().TraceID())
		if err != nil {
			return err
		}
		if child.SpanContext().IsSampled() != childWant {
			return fmt.Errorf("child %d of a root sampled %v: sampled %v, want %v", seq, rootWant, !childWant, childWant)
		}
		for _, sp := range []struct {
			s    trace.Span
			want bool
		}{{root, rootWant}, {child, childWant}} {
			if sp.want {
				want = append(want, started{sp.s.SpanContext(), s.attrs})
			}
		}
		if seq%3 == 0 {
			_, joined := tracer.Start(remoteParent(r), s.name, trace.WithSpanKind(s.kind), trace.WithAttributes(s.attrs...))
			joined.End()
			joinedWant, err := o.sampled(s, settingspb.Parent_PARENT_REMOTE_SAMPLED, joined.SpanContext().TraceID())
			if err != nil {
				return err
			}
			joinedWant = joinedWant && !capped(true)
			if joined.SpanContext().IsSampled() != joinedWant {
				return fmt.Errorf("span %d joining a sampled caller: sampled %v, want %v", seq, !joinedWant, joinedWant)
			}
			if joinedWant {
				want = append(want, started{joined.SpanContext(), s.attrs})
			}
		}
	}

	type emitted struct {
		seq   int64
		attrs []attribute.KeyValue
	}
	var wantRecords []emitted
	for seq := int64(0); seq < 60; seq++ {
		scope, sev, attrs := pick(r, tortureLogScopes...), r.IntN(25), tortureAttrs(r, seq)
		var rec log.Record
		rec.SetSeverity(log.Severity(sev))
		rec.SetBody(attribute.Int64Value(seq))
		rec.AddAttributes(attrs...)
		lp.Logger(scope).Emit(context.Background(), rec)
		got, err := o.evaluate(&settingspb.Evaluate{Input: &settingspb.Evaluate_Log{Log: &settingspb.LogRecord{
			Scope: scope, Severity: uint32(sev)}}})
		if err != nil {
			return err
		}
		if !got.GetKept() {
			continue
		}
		if logTokens == 0 {
			continue
		}
		if logTokens > 0 {
			logTokens--
		}
		wantRecords = append(wantRecords, emitted{seq, attrs})
	}

	if err := sdkTP.ForceFlush(context.Background()); err != nil {
		return err
	}
	if err := lp.ForceFlush(context.Background()); err != nil {
		return err
	}

	spans.mu.Lock()
	gotSpans := spans.got
	spans.mu.Unlock()
	if last.settings.SendTraces != nil && !*last.settings.SendTraces {
		want = nil
	}
	if len(gotSpans) != len(want) {
		return fmt.Errorf("%d spans exported, want %d", len(gotSpans), len(want))
	}
	byID := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	for _, s := range gotSpans {
		byID[s.SpanContext().SpanID()] = s
	}
	for _, w := range want {
		s, ok := byID[w.sc.SpanID()]
		if !ok {
			return fmt.Errorf("sampled span %v wasn't exported", w.sc.SpanID())
		}
		redacted, err := o.redacted(w.attrs)
		if err != nil {
			return err
		}
		lists := [][]attribute.KeyValue{s.Attributes()}
		for _, e := range s.Events() {
			lists = append(lists, e.Attributes)
		}
		for _, l := range s.Links() {
			lists = append(lists, l.Attributes)
		}
		for _, got := range lists {
			if show(got) != redacted {
				return fmt.Errorf("span %q exported with %s, the layer redacts it to %s", s.Name(), show(got), redacted)
			}
		}
	}

	records.mu.Lock()
	gotRecords := records.got
	records.mu.Unlock()
	if len(gotRecords) != len(wantRecords) {
		var seqs []int64
		for _, g := range gotRecords {
			seqs = append(seqs, g.Body().AsInt64())
		}
		return fmt.Errorf("log records %v exported, want %d", seqs, len(wantRecords))
	}
	for i, w := range wantRecords {
		got := gotRecords[i]
		if got.Body().AsInt64() != w.seq {
			return fmt.Errorf("log record %d exported in place of record %d", got.Body().AsInt64(), w.seq)
		}
		redacted, err := o.redacted(w.attrs)
		if err != nil {
			return err
		}
		if show(recordAttrs(got)) != redacted {
			return fmt.Errorf("log record %d exported with %s, the layer redacts it to %s", w.seq, show(recordAttrs(got)), redacted)
		}
	}
	t.Logf("the last document: %d spans and %d log records exported, as it says", len(want), len(wantRecords))
	return nil
}
