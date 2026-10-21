// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/match"
	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// Controls is what the server changes while the SDK runs: the sampler, whether each signal is
// sent, which instrumentations are on. Each takes effect at once, instrumentations too.
// Safe for concurrent use.
type Controls struct {
	Sampler *LiveSampler
	mu      sync.Mutex // Apply: one document at a time, the SDK's settings and custom together
	gates   atomic.Pointer[gates]
	app     *appSettings                           // custom: what the application reads (AppSetting)
	scopes  seenScopes                             // those handed a tracer, meter or logger: the inventory
	counts  struct{ traces, metrics, logs counts } // for export health
	// clock is the rate caps' monotonic clock, in nanoseconds (a test's own in tests).
	clock func() int64
}

// NewControls starts with the SDK's own settings: everything sent, every instrumentation on.
func NewControls() *Controls {
	c := &Controls{Sampler: NewLiveSampler(nil), app: newAppSettings()}
	c.clock = c.Sampler.clock // one monotonic clock for the caps
	c.gates.Store(&gates{traces: true, metrics: true, logs: true,
		levels: &logLevels{}, redact: &redaction{}, switches: &switches{}})
	return c
}

// Apply carries out the settings; a setting left out is the SDK's own. The application's own
// settings (custom) are put in force last, so a function given to OnAppSettingsChange sees the
// SDK's settings already in force; it runs on the calling goroutine. Both are put in force under
// one lock, so the SDK's settings and the application's always come from the same document.
func (c *Controls) Apply(s *settingspb.Settings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.apply(s)
	c.app.put(s.GetCustom())
}

// apply carries out the SDK's own settings; c.mu is held.
func (c *Controls) apply(s *settingspb.Settings) {
	c.Sampler.apply(s.SamplingRate, s.GetSamplingRules(), s.SamplingTracesPerSecond)
	g := &gates{
		traces:  s.SendTraces == nil || *s.SendTraces,
		metrics: s.SendMetrics == nil || *s.SendMetrics,
		logs:    s.SendLogs == nil || *s.SendLogs,
		levels:  match.LevelsOf(s),
		logsCap: c.gates.Load().logsCap,
		redact:  &redaction{*match.RedactionOf(s)},
	}
	// A cap applied again (the same document re-delivered, or refreshed) keeps its bucket.
	if !sameCap(g.logsCap, s.SendLogsPerSecond) {
		g.logsCap = capOf(s.SendLogsPerSecond)
	}
	g.switches = &switches{all: s.GetDeactivateAllInstrumentations()}
	if names := s.GetDeactivateInstrumentations().GetNames(); len(names) > 0 {
		g.switches.names = map[string]bool{}
		for _, n := range names {
			g.switches.names[n] = true
		}
	}
	c.gates.Store(g)
}

// gates are what the gates hold an export to: one document's worth, never changed. Apply swaps
// them whole and a gate loads them once an export, so an export is held to one document's send_*,
// levels, cap, redaction and switches, never to the send_* of one and the redaction of the one
// before.
type gates struct {
	traces, metrics, logs bool       // send_traces, send_metrics, send_logs
	levels                *logLevels // send_logs_level and send_logs_level_by_scope
	logsCap               *bucket    // send_logs_per_second; nil: none
	redact                *redaction // drop_attributes and mask_attributes
	switches              *switches
}

// switches are the instrumentations switched off.
type switches struct {
	all   bool
	names map[string]bool
}

// Off says whether the instrumentation with this scope name is switched off. An instrumentation
// is named by its scope name or the scope name's last path element: `otelhttp` is
// go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp.
func (c *Controls) Off(scope string) bool { return c.gates.Load().switches.off(scope) }

func (sw *switches) off(scope string) bool {
	if sw.all {
		return true
	}
	if len(sw.names) == 0 {
		return false
	}
	return sw.names[scope] || sw.names[scope[strings.LastIndexByte(scope, '/')+1:]]
}

// LiveSampler is the SDK's own sampler until the server says otherwise. For a new trace, the
// first of the server's sampling rules its root span matches decides the rate; else the server's
// sampling rate; else the SDK's own sampler. With rules or a rate set, a span with a parent follows
// its parent's decision, as the default does. Then a trace cap (sampling_traces_per_second) may
// drop what was to be sampled, when it starts or joins a trace.
type LiveSampler struct {
	mu         sync.Mutex // configured, rate, rules
	configured sdktrace.Sampler
	rate       *float64
	rules      []*settingspb.SamplingRule
	current    atomic.Pointer[samplerBox]
	cap        atomic.Pointer[bucket] // nil: none
	// clock is the trace cap's monotonic clock, in nanoseconds.
	clock func() int64
}

type samplerBox struct{ sdktrace.Sampler }

// NewLiveSampler wraps the configured sampler (nil: the SDK's default, parent-based always-on).
func NewLiveSampler(configured sdktrace.Sampler) *LiveSampler {
	if configured == nil {
		configured = sdktrace.ParentBased(sdktrace.AlwaysSample())
	}
	start := time.Now()
	s := &LiveSampler{configured: configured, clock: func() int64 { return int64(time.Since(start)) }}
	s.current.Store(&samplerBox{configured})
	return s
}

// Wrap sets the sampler the configuration built, before the SDK starts; rules and a rate set
// before (at start) still hold.
func (s *LiveSampler) Wrap(configured sdktrace.Sampler) *LiveSampler {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configured = configured
	s.set(s.rate, s.rules)
	return s
}

// Rate sets the sampling rate; nil: back to the configured sampler (where no rule matches).
func (s *LiveSampler) Rate(rate *float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set(rate, s.rules)
}

// Rules sets the sampling rules, as the layer hands them over; none: the rate alone.
func (s *LiveSampler) Rules(rules []*settingspb.SamplingRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set(s.rate, rules)
}

// Cap sets the trace cap, in traces a second; nil: none. The same cap again keeps its bucket; a
// new one starts full.
func (s *LiveSampler) Cap(perSecond *float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !sameCap(s.cap.Load(), perSecond) {
		s.cap.Store(capOf(perSecond))
	}
}

func (s *LiveSampler) apply(rate *float64, rules []*settingspb.SamplingRule, perSecond *float64) {
	s.mu.Lock()
	s.set(rate, rules)
	s.mu.Unlock()
	s.Cap(perSecond)
}

func (s *LiveSampler) set(rate *float64, rules []*settingspb.SamplingRule) {
	s.rate, s.rules = rate, rules
	fallback := s.configured
	if rate != nil {
		fallback = sdktrace.TraceIDRatioBased(*rate)
	}
	switch {
	case len(rules) == 0 && rate == nil:
		s.current.Store(&samplerBox{s.configured})
	case len(rules) == 0:
		s.current.Store(&samplerBox{sdktrace.ParentBased(fallback)})
	default:
		r := &ruled{fallback: fallback}
		for _, sr := range rules {
			r.rules = append(r.rules, newRule(sr))
		}
		s.current.Store(&samplerBox{sdktrace.ParentBased(r)})
	}
}

func (s *LiveSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	res := s.current.Load().ShouldSample(p)
	if b := s.cap.Load(); b != nil && res.Decision == sdktrace.RecordAndSample && takesToken(p) {
		if b.take(s.clock(), 1) == 0 {
			return sdktrace.SamplingResult{Decision: sdktrace.Drop, Tracestate: res.Tracestate}
		}
	}
	return res
}

// takesToken says whether a sampled span counts against the trace cap: a root span, or one whose
// caller's span was sampled. A child of a span sampled in this process never does: refusing it
// would break its trace.
func takesToken(p sdktrace.SamplingParameters) bool {
	parent := trace.SpanContextFromContext(p.ParentContext)
	return !parent.IsValid() || parent.IsRemote() && parent.IsSampled()
}

func (s *LiveSampler) Description() string {
	d := s.current.Load().Description()
	if b := s.cap.Load(); b != nil {
		d += fmt.Sprintf(",TracesPerSecond{%g}", b.perSecond)
	}
	return "OpampLiveSampler{" + d + "}"
}

// ruled samples a root span by the first rule it matches, else by the fallback: the rate, or the
// configured sampler.
type ruled struct {
	rules    []rule
	fallback sdktrace.Sampler
}

func (r *ruled) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	for i := range r.rules {
		if r.rules[i].matches(p) {
			return r.rules[i].sampler.ShouldSample(p)
		}
	}
	return r.fallback.ShouldSample(p)
}

func (r *ruled) Description() string {
	return fmt.Sprintf("SamplingRules{%d rules, else %s}", len(r.rules), r.fallback.Description())
}

// TracerProvider hands out tracers that start no span while their instrumentation is switched
// off. The span they return instead carries the parent's context, so the trace goes on through
// the instrumentations still on, and propagation is unchanged.
//
// Every span they return, started or skipped, gives this provider as its TracerProvider:
// instrumentations such as otelhttp's Transport take their tracer from the span in the context,
// and must be switched the same.
func (c *Controls) TracerProvider(tp trace.TracerProvider) trace.TracerProvider {
	return &tracerProvider{tp: tp, c: c}
}

type tracerProvider struct {
	embedded.TracerProvider
	tp trace.TracerProvider
	c  *Controls
}

func (p *tracerProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	cfg := trace.NewTracerConfig(opts...)
	p.c.scopes.add(name, cfg.InstrumentationVersion())
	return &tracer{Tracer: p.tp.Tracer(name, opts...), scope: name, p: p}
}

type tracer struct {
	trace.Tracer
	scope string
	p     *tracerProvider
}

var off = noop.NewTracerProvider().Tracer("")

func (t *tracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	// The tracer below sees the parent as it made it (the SDK counts a span's children).
	if parent, ok := trace.SpanFromContext(ctx).(*span); ok {
		ctx = trace.ContextWithSpan(ctx, parent.Span)
	}
	var s trace.Span
	if t.p.c.Off(t.scope) {
		ctx, s = off.Start(ctx, name, opts...)
	} else {
		ctx, s = t.Tracer.Start(ctx, name, opts...)
	}
	s = &span{Span: s, p: t.p}
	return trace.ContextWithSpan(ctx, s), s
}

// span is the SDK's span (or the skipped one), its TracerProvider the switching one.
type span struct {
	trace.Span
	p *tracerProvider
}

func (s *span) TracerProvider() trace.TracerProvider { return s.p }

// Spans gates a span exporter: while traces aren't sent, what would be exported is dropped, and
// drop_attributes and mask_attributes redact the attributes of each span, its events and its
// links. A span redaction changes is exported as a view of the SDK's: other processors and
// exporters still see it as it was.
func (c *Controls) Spans(to sdktrace.SpanExporter) sdktrace.SpanExporter { return &spans{to, c} }

type spans struct {
	sdktrace.SpanExporter
	c *Controls
}

func (e *spans) ExportSpans(ctx context.Context, s []sdktrace.ReadOnlySpan) error {
	g := e.c.gates.Load()
	if !g.traces {
		e.c.counts.traces.filtered(len(s))
		return nil
	}
	if r := g.redact; !r.none() {
		s = redactSpans(r, s)
	}
	err := e.SpanExporter.ExportSpans(ctx, s)
	e.c.counts.traces.exported(len(s), err)
	return err
}

// Metrics gates a metric exporter: while metrics aren't sent, what would be exported is dropped,
// and a switched-off instrumentation's metrics are always dropped (its instruments were made at
// start and go on recording; only exporting can stop).
func (c *Controls) Metrics(to sdkmetric.Exporter) sdkmetric.Exporter { return &metrics{to, c} }

type metrics struct {
	sdkmetric.Exporter
	c *Controls
}

func (e *metrics) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	counts, g := &e.c.counts.metrics, e.c.gates.Load()
	if !g.metrics {
		counts.filtered(points(rm.ScopeMetrics))
		return nil
	}
	kept := rm.ScopeMetrics[:0:0]
	for _, sm := range rm.ScopeMetrics {
		if !g.switches.off(sm.Scope.Name) {
			kept = append(kept, sm)
		} else {
			counts.filtered(points([]metricdata.ScopeMetrics{sm}))
		}
	}
	if len(kept) == 0 && len(rm.ScopeMetrics) > 0 {
		return nil
	}
	export := rm
	if len(kept) != len(rm.ScopeMetrics) {
		// The reader reuses rm: export a copy.
		export = &metricdata.ResourceMetrics{Resource: rm.Resource, ScopeMetrics: kept}
	}
	err := e.Exporter.Export(ctx, export)
	counts.exported(points(kept), err)
	return err
}

// Logs gates a log exporter: while logs aren't sent, what would be exported is dropped; a
// switched-off instrumentation's (a log bridge's) records are always dropped, and so are records
// below their scope's level: send_logs_level_by_scope's for the scope name the record is exported
// under (the name given to the bridge, otelslog.NewLogger's), else send_logs_level's. A record
// with no severity (0) is kept, but under `off`. Then send_logs_per_second: of what is left of a
// batch, the first records the cap's tokens allow go on, the rest are dropped. Last,
// drop_attributes and mask_attributes redact each record's attributes, on a copy: other
// processors and exporters still see the record as it was.
func (c *Controls) Logs(to sdklog.Exporter) sdklog.Exporter { return &logs{to, c} }

type logs struct {
	sdklog.Exporter
	c *Controls
}

func (e *logs) Export(ctx context.Context, records []sdklog.Record) error {
	counts, g := &e.c.counts.logs, e.c.gates.Load()
	if !g.logs {
		counts.filtered(len(records))
		return nil
	}
	// The batch itself while every record passes; a copy of those that do from the first that
	// doesn't (the processor's batch isn't ours to change).
	kept, copied := records, false
	for i := range records {
		r := &records[i]
		scope := r.InstrumentationScope().Name
		pass := g.levels.Keeps(scope, int(r.Severity())) && !g.switches.off(scope)
		switch {
		case pass && copied:
			kept = append(kept, *r)
		case !pass && !copied:
			kept, copied = append(make([]sdklog.Record, 0, len(records)), records[:i]...), true
		}
	}
	if b := g.logsCap; b != nil {
		kept = kept[:b.take(e.c.clock(), uint64(len(kept)))]
	}
	counts.filtered(len(records) - len(kept))
	if r := g.redact; !r.none() {
		kept = redactRecords(r, kept, copied)
	}
	if len(kept) == 0 {
		return nil
	}
	err := e.Exporter.Export(ctx, kept)
	counts.exported(len(kept), err)
	return err
}
