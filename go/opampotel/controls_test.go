// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

const otelhttp = "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

func names(n ...string) *settingspb.Names { return &settingspb.Names{Names: n} }

// A trace ID whose ratio-sampling decision is known: the SDK samples a trace when its low 8 bytes,
// read big-endian and shifted right once, are below rate * 2^63.
func traceID(low byte) trace.TraceID { return trace.TraceID{8: low} }

func sample(s sdktrace.Sampler, id trace.TraceID) sdktrace.SamplingDecision {
	return s.ShouldSample(sdktrace.SamplingParameters{ParentContext: context.Background(), TraceID: id}).Decision
}

func TestTheSamplerIsTheConfiguredOneUntilARateIsSet(t *testing.T) {
	s := NewLiveSampler(nil).Wrap(sdktrace.NeverSample())
	low, high := traceID(0x00), traceID(0xff) // 0 and ~2^63: under and over half
	if sample(s, low) != sdktrace.Drop {
		t.Fatal("the configured sampler (never) should decide")
	}
	s.Rate(proto.Float64(0.5))
	if sample(s, low) != sdktrace.RecordAndSample || sample(s, high) != sdktrace.Drop {
		t.Fatal("rate 0.5 should keep the low half of trace IDs")
	}
	s.Rate(nil)
	if sample(s, low) != sdktrace.Drop {
		t.Fatal("left out: back to the configured sampler")
	}
}

func TestARateSetAtStartHoldsOnceTheConfiguredSamplerIsKnown(t *testing.T) {
	c := NewControls()
	c.Apply(&settingspb.Settings{SamplingRate: proto.Float64(0)})
	s := c.Sampler.Wrap(sdktrace.AlwaysSample())
	if sample(s, traceID(0)) != sdktrace.Drop {
		t.Fatal("the saved rate 0 should hold over the configured always_on")
	}
}

func TestAChildFollowsItsParentWhateverTheRate(t *testing.T) {
	s := NewLiveSampler(nil)
	s.Rate(proto.Float64(0))
	parent := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID(1), SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled}))
	d := s.ShouldSample(sdktrace.SamplingParameters{ParentContext: parent, TraceID: traceID(1)}).Decision
	if d != sdktrace.RecordAndSample {
		t.Fatal("a sampled parent's child should be sampled at rate 0")
	}
}

func TestAnInstrumentationIsNamedByItsScopeOrItsLastPathElement(t *testing.T) {
	c := NewControls()
	c.Apply(&settingspb.Settings{DeactivateInstrumentations: names("otelhttp", "checkout")})
	for scope, off := range map[string]bool{
		otelhttp:   true,
		"checkout": true,
		"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc": false,
		"example.com/otelhttpx": false,
		"otelhttp/v2":           false,
	} {
		if c.Off(scope) != off {
			t.Errorf("Off(%q) = %v, want %v", scope, !off, off)
		}
	}
	c.Apply(&settingspb.Settings{DeactivateAllInstrumentations: proto.Bool(true)})
	if !c.Off("anything") {
		t.Error("all: every scope is off")
	}
	c.Apply(&settingspb.Settings{})
	if c.Off(otelhttp) {
		t.Error("left out: every instrumentation on")
	}
}

func TestASwitchedOffInstrumentationStartsNoSpanButKeepsTheTraceGoing(t *testing.T) {
	c := NewControls()
	rec := tracetest.NewSpanRecorder()
	tp := c.TracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	http, app := tp.Tracer(otelhttp), tp.Tracer("checkout")

	ctx, root := app.Start(context.Background(), "root")
	c.Apply(&settingspb.Settings{DeactivateInstrumentations: names("otelhttp")}) // after the tracer was made
	ctx2, skipped := http.Start(ctx, "GET")
	if skipped.IsRecording() || !skipped.SpanContext().Equal(root.SpanContext()) {
		t.Fatal("the switched-off instrumentation's span should be the parent's context, not recording")
	}
	skipped.End() // must not end the parent
	_, child := app.Start(ctx2, "child")
	child.End()
	if !root.IsRecording() {
		t.Fatal("ending the skipped span ended the parent")
	}
	root.End()

	c.Apply(&settingspb.Settings{})
	_, again := http.Start(context.Background(), "GET")
	again.End()

	var got []string
	for _, s := range rec.Ended() {
		got = append(got, s.Name())
		if s.Name() == "child" && s.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Error("the application's span should parent on the root, past the skipped one")
		}
	}
	if want := []string{"child", "root", "GET"}; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("ended spans %v, want %v", got, want)
	}
}

func TestSpansAreDroppedWhileTracesAreNotSent(t *testing.T) {
	c := NewControls()
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(c.Spans(mem)))
	span := func() { _, s := tp.Tracer("t").Start(context.Background(), "s"); s.End() }
	span()
	c.Apply(&settingspb.Settings{SendTraces: proto.Bool(false)})
	span()
	c.Apply(&settingspb.Settings{SendTraces: proto.Bool(true)})
	span()
	if n := len(mem.GetSpans()); n != 2 {
		t.Fatalf("%d spans exported, want 2", n)
	}
}

type metricSink struct {
	sdkmetric.Exporter
	got []*metricdata.ResourceMetrics
}

func (m *metricSink) Export(_ context.Context, rm *metricdata.ResourceMetrics) error {
	m.got = append(m.got, rm)
	return nil
}

func TestMetricsAreDroppedWhileNotSentAndPerSwitchedOffScope(t *testing.T) {
	c := NewControls()
	sink := &metricSink{}
	e := c.Metrics(sink)
	rm := &metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{{}, {}}}
	rm.ScopeMetrics[0].Scope.Name = otelhttp
	rm.ScopeMetrics[1].Scope.Name = "checkout"
	ctx := context.Background()

	_ = e.Export(ctx, rm)
	if len(sink.got) != 1 || sink.got[0] != rm {
		t.Fatal("nothing off: exported as is")
	}
	c.Apply(&settingspb.Settings{DeactivateInstrumentations: names("otelhttp")})
	_ = e.Export(ctx, rm)
	if len(sink.got) != 2 || len(sink.got[1].ScopeMetrics) != 1 || sink.got[1].ScopeMetrics[0].Scope.Name != "checkout" {
		t.Fatal("otelhttp's metrics should be left out")
	}
	if len(rm.ScopeMetrics) != 2 || rm.ScopeMetrics[0].Scope.Name != otelhttp {
		t.Fatal("the reader's own ResourceMetrics must not change")
	}
	c.Apply(&settingspb.Settings{DeactivateAllInstrumentations: proto.Bool(true)})
	_ = e.Export(ctx, rm)
	c.Apply(&settingspb.Settings{SendMetrics: proto.Bool(false)})
	_ = e.Export(ctx, rm)
	if len(sink.got) != 2 {
		t.Fatalf("%d exports, want none while all are off or metrics aren't sent", len(sink.got)-2)
	}
}

type logSink struct {
	got               [][]string
	flushed, shutDown int
}

func (l *logSink) Export(_ context.Context, records []sdklog.Record) error {
	var scopes []string
	for _, r := range records {
		scopes = append(scopes, r.InstrumentationScope().Name)
	}
	l.got = append(l.got, scopes)
	return nil
}

func (l *logSink) ForceFlush(context.Context) error { l.flushed++; return nil }
func (l *logSink) Shutdown(context.Context) error   { l.shutDown++; return nil }

func TestLogsAreDroppedWhileNotSentAndPerSwitchedOffScope(t *testing.T) {
	c := NewControls()
	sink := &logSink{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(c.Logs(sink))))
	emit := func(scope string) {
		var r log.Record
		r.SetBody(attribute.StringValue("hello"))
		lp.Logger(scope).Emit(context.Background(), r)
	}
	emit("otelslog")
	c.Apply(&settingspb.Settings{DeactivateInstrumentations: names("otelslog")})
	emit("otelslog")
	emit("checkout")
	c.Apply(&settingspb.Settings{SendLogs: proto.Bool(false)})
	emit("checkout")
	want := [][]string{{"otelslog"}, {"checkout"}}
	if len(sink.got) != 2 || sink.got[0][0] != want[0][0] || sink.got[1][0] != want[1][0] {
		t.Fatalf("exported %v, want %v", sink.got, want)
	}
	// Flushing and shutting down reach the exporter, sent or not.
	if err := errors.Join(lp.ForceFlush(context.Background()), lp.Shutdown(context.Background())); err != nil ||
		sink.flushed == 0 || sink.shutDown != 1 {
		t.Fatalf("flushed %d, shut down %d (%v); want flushed, shut down once", sink.flushed, sink.shutDown, err)
	}
}

type severitySink struct {
	logSink
	got []log.Severity
}

func (l *severitySink) Export(_ context.Context, records []sdklog.Record) error {
	for _, r := range records {
		l.got = append(l.got, r.Severity())
	}
	return nil
}

func TestLogsBelowTheLevelAreDroppedWhileItIsSet(t *testing.T) {
	c := NewControls()
	sink := &severitySink{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(c.Logs(sink))))
	every := []log.Severity{0, log.SeverityTrace, log.SeverityDebug, log.SeverityInfo, log.SeverityWarn,
		log.SeverityWarn4, log.SeverityError, log.SeverityFatal}
	emitAll := func() []log.Severity {
		sink.got = nil
		for _, sev := range every {
			var r log.Record
			r.SetSeverity(sev)
			lp.Logger("checkout").Emit(context.Background(), r)
		}
		return sink.got
	}
	same := func(a, b []log.Severity) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	if got := emitAll(); !same(got, every) {
		t.Fatalf("no level: exported %v, want every record %v", got, every)
	}
	c.Apply(&settingspb.Settings{SendLogsMinSeverity: proto.Uint32(13)}) // warn
	want := []log.Severity{0, log.SeverityWarn, log.SeverityWarn4, log.SeverityError, log.SeverityFatal}
	if got := emitAll(); !same(got, want) {
		t.Fatalf("warn: exported %v, want %v (severity 0 kept)", got, want)
	}
	c.Apply(&settingspb.Settings{SendLogsMinSeverity: proto.Uint32(13), SendLogs: proto.Bool(false)})
	if got := emitAll(); len(got) != 0 {
		t.Fatalf("logs not sent: exported %v, want none", got)
	}
	c.Apply(&settingspb.Settings{})
	if got := emitAll(); !same(got, every) {
		t.Fatalf("level left out: exported %v, want every record %v", got, every)
	}
}

// otelhttp's Transport, and other instrumentations, take their tracer from the provider of the
// span in the request's context, not the global one: that provider must be the switching one.
func TestAnInstrumentationReachedThroughASpansProviderIsSwitchedToo(t *testing.T) {
	c := NewControls()
	rec := tracetest.NewSpanRecorder()
	tp := c.TracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	fromContext := func(ctx context.Context, scope string) trace.Tracer {
		return trace.SpanFromContext(ctx).TracerProvider().Tracer(scope)
	}

	ctx, root := tp.Tracer("checkout").Start(context.Background(), "root")
	_, direct := fromContext(ctx, "checkout").Start(ctx, "direct")
	direct.End()
	c.Apply(&settingspb.Settings{DeactivateInstrumentations: names("otelhttp")})
	ctx2, skipped := fromContext(ctx, otelhttp).Start(ctx, "GET")
	if skipped.IsRecording() {
		t.Fatal("otelhttp is off, whichever provider it took its tracer from")
	}
	// Past the skipped span, an instrumentation still on traces, parented on the root.
	_, grpc := fromContext(ctx2, "otelgrpc").Start(ctx2, "grpc")
	grpc.End()
	skipped.End()
	root.End()

	ended := rec.Ended()
	if len(ended) != 3 || ended[0].Name() != "direct" || ended[1].Name() != "grpc" || ended[2].Name() != "root" ||
		ended[1].Parent().SpanID() != root.SpanContext().SpanID() {
		var got []string
		for _, s := range ended {
			got = append(got, s.Name())
		}
		t.Fatalf("ended spans %v, want [direct grpc root], grpc under root", got)
	}
	if n := ended[2].ChildSpanCount(); n != 1 {
		t.Fatalf("the SDK counted %d children of the root, want 1 (direct)", n)
	}
}

// The server's settings are applied on the client's goroutine while the application's start
// spans, and an application may apply its own: none of it may race (go test -race).
func TestSettingsCanBeAppliedWhileSpansStart(t *testing.T) {
	c := NewControls()
	tp := c.TracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSampler(c.Sampler)))
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				c.Apply(&settingspb.Settings{SamplingRate: proto.Float64(float64(i%3) / 2),
					DeactivateInstrumentations: names("otelhttp")})
				c.Apply(&settingspb.Settings{})
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_, s := tp.Tracer(otelhttp).Start(context.Background(), "s")
				s.End()
				_ = c.Sampler.Description()
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); c.Sampler.Wrap(sdktrace.AlwaysSample()) }()
	wg.Wait()
}

// passwordSink counts the exported spans and records that carry user.password.
type passwordSink struct{ leaked atomic.Int64 }

func (p *passwordSink) attrs(attrs []attribute.KeyValue) {
	for _, kv := range attrs {
		if kv.Key == "user.password" {
			p.leaked.Add(1)
		}
	}
}

func (p *passwordSink) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, s := range spans {
		p.attrs(s.Attributes())
	}
	return nil
}

func (p *passwordSink) Export(_ context.Context, records []sdklog.Record) error {
	for _, r := range records {
		p.attrs(recordAttrs(r))
	}
	return nil
}

func (p *passwordSink) ForceFlush(context.Context) error { return nil }
func (p *passwordSink) Shutdown(context.Context) error   { return nil }

// A sends nothing; B sends, dropping *password*: neither exports user.password. An export while
// one is applied over the other is held to one of them, never to B's send_* and A's redaction.
func TestAnExportIsHeldToOneDocumentWhileTheyFlip(t *testing.T) {
	a := settingsOf(t, []byte(v1("signal_exports", exports("off", "", "off"))), 0)
	b := settingsOf(t, []byte(every("signal_exports", exports("on", "", "on"), "attribute_redaction", override(`"drop":["*password*"]`))), 0)
	password := attribute.String("user.password", "hunter2")
	span := tracetest.SpanStub{Name: "s", Attributes: []attribute.KeyValue{password}}.Snapshot()
	// A record as the SDK makes it (one made here would keep no attribute: its limits are zero).
	made := &recordSink{}
	var r log.Record
	r.AddAttributes(password)
	sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(made))).Logger("app").Emit(context.Background(), r)
	record := made.take()[0]
	for _, tc := range []struct {
		signal string
		export func(*Controls, *passwordSink) error
	}{
		{"traces", func(c *Controls, sink *passwordSink) error {
			return c.Spans(sink).ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{span})
		}},
		{"logs", func(c *Controls, sink *passwordSink) error {
			return c.Logs(sink).Export(context.Background(), []sdklog.Record{record})
		}},
	} {
		t.Run(tc.signal, func(t *testing.T) {
			c, sink := NewControls(), &passwordSink{}
			c.Apply(a)
			var stop atomic.Bool
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stop.Load() {
					c.Apply(b)
					c.Apply(a)
				}
			}()
			exports := 0
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && sink.leaked.Load() == 0; exports++ {
				_ = tc.export(c, sink)
			}
			stop.Store(true)
			wg.Wait()
			if n := sink.leaked.Load(); n > 0 {
				t.Fatalf("user.password exported %d times in %d exports: neither document exports it", n, exports)
			}
		})
	}
}
