// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampotel

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/pydantic/opamp-clients/go/opamp/sdk/settingspb"
)

// appSampler stands for the application's own sampler: it decides RecordOnly, which neither a
// rule nor a rate ever does, so a test can tell it decided. Wrapped parent-based, as the SDK's
// default is, where a test needs a parent followed; on its own where a test needs to see that it
// decides a child too (nothing set: the application's sampler decides every span).
type appSampler struct{}

func (appSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if v, ok := first(p.Attributes, "test.sampled"); ok {
		if v.AsBool() {
			return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
		}
		return sdktrace.SamplingResult{Decision: sdktrace.Drop}
	}
	return sdktrace.SamplingResult{Decision: sdktrace.RecordOnly}
}
func (appSampler) Description() string { return "app" }

var kinds = map[string]trace.SpanKind{"server": trace.SpanKindServer, "client": trace.SpanKindClient,
	"internal": trace.SpanKindInternal, "producer": trace.SpanKindProducer, "consumer": trace.SpanKindConsumer}

// parentContext is a context holding a parent of the vectors' kind ("none": no parent).
func parentContext(t testing.TB, parent string) context.Context {
	t.Helper()
	sc := trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}}
	switch parent {
	case "", "none":
		return context.Background()
	case "local_sampled", "remote_sampled":
		sc.TraceFlags = trace.FlagsSampled
	case "local_unsampled", "remote_unsampled":
	default:
		t.Fatalf("parent %q", parent)
	}
	sc.Remote = strings.HasPrefix(parent, "remote")
	return trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(sc))
}

// idAt is a trace ID whose ratio-sampling value (its low 8 bytes, big-endian, shifted right
// once) is x: the SDK samples it at rate r when x < uint64(r * 2^63).
func idAt(x uint64) trace.TraceID {
	var id trace.TraceID
	binary.BigEndian.PutUint64(id[8:], x<<1)
	return id
}

// checkRate says whether s, for spans like p, samples by the trace-ID ratio at exactly rate:
// the trace ID just under the bound is sampled, the one at it isn't.
func checkRate(s sdktrace.Sampler, p sdktrace.SamplingParameters, rate float64) error {
	bound := uint64(rate * (1 << 63))
	if bound > 0 {
		p.TraceID = idAt(bound - 1)
		if d := s.ShouldSample(p).Decision; d != sdktrace.RecordAndSample {
			return fmt.Errorf("rate %v: the trace ID under the bound was %v", rate, d)
		}
	}
	if bound < 1<<63 {
		p.TraceID = idAt(bound)
		if d := s.ShouldSample(p).Decision; d != sdktrace.Drop {
			return fmt.Errorf("rate %v: the trace ID at the bound was %v", rate, d)
		}
	}
	return nil
}

type samplingWant struct {
	How     string   `json:"how"`
	Sampled bool     `json:"sampled"`
	Rate    float64  `json:"rate"`
	Rule    *float64 `json:"rule"`
}

// judge checks the sampler's decision for p against what the layer says (how, rate, the parent).
func judge(s sdktrace.Sampler, p sdktrace.SamplingParameters, want samplingWant) error {
	switch want.How {
	case "app_sampler":
		if d := s.ShouldSample(p).Decision; d != sdktrace.RecordOnly {
			return fmt.Errorf("decided %v, want the application's sampler", d)
		}
	case "follow_parent":
		wantD := sdktrace.Drop
		if want.Sampled {
			wantD = sdktrace.RecordAndSample
		}
		for _, x := range []uint64{0, 1 << 62, 1<<63 - 1} { // whatever the trace ID
			p.TraceID = idAt(x)
			if d := s.ShouldSample(p).Decision; d != wantD {
				return fmt.Errorf("decided %v, want the parent's %v", d, wantD)
			}
		}
	case "rate":
		return checkRate(s, p, want.Rate)
	default:
		return fmt.Errorf("how %q", want.How)
	}
	return nil
}

// How the sampler decides a span at its start (sampling.json): the rules, sampling_rate, the
// application's sampler, a parent's decision, debug mode's rate.
func TestTheSamplingVectors(t *testing.T) {
	var v struct {
		About string
		Cases []struct {
			Name     string          `json:"name"`
			Settings json.RawMessage `json:"settings"`
			Now      uint64          `json:"now_unix_ms"`
			Span     struct {
				Name       string            `json:"name"`
				Kind       string            `json:"kind"`
				Attributes []vectorAttribute `json:"attributes"`
				Parent     string            `json:"parent"`
			} `json:"span"`
			Want samplingWant `json:"want"`
		}
	}
	decodeStrict(t, readVectors[json.RawMessage](t, "sampling.json"), &v)
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			controls := NewControls()
			controls.Sampler.Wrap(appSampler{})
			controls.Apply(settingsOf(t, c.Settings, c.Now))
			kind, ok := kinds[c.Span.Kind]
			if !ok {
				t.Fatalf("kind %q", c.Span.Kind)
			}
			p := sdktrace.SamplingParameters{ParentContext: parentContext(t, c.Span.Parent), Name: c.Span.Name,
				Kind: kind, Attributes: kvs(t, c.Span.Attributes)}
			if err := judge(controls.Sampler, p, c.Want); err != nil {
				t.Fatal(err)
			}
		})
	}
	if len(v.Cases) < 50 {
		t.Fatalf("%d cases: sampling.json was cut short", len(v.Cases))
	}
}

// A trace cap's run, span by span at the vectors' times (caps.json, the traces cases). A span's
// "sampled" is what the rules, the rate or the parent decided, or with neither set what the
// application's own sampler decided: it is told it (test.sampled), under any parent, as a sampler
// that isn't parent-based (always_on, traceidratio) samples under an unsampled one.
func TestTheTraceCapVectors(t *testing.T) {
	v := readVectors[struct{ Cases []capCase }](t, "caps.json")
	ran := 0
	for _, c := range v.Cases {
		if c.Signal != "traces" {
			continue
		}
		ran++
		t.Run(c.Name, func(t *testing.T) {
			controls := NewControls()
			clock := withClock(controls)
			controls.Sampler.Wrap(appSampler{})
			controls.Apply(settingsOf(t, c.Settings, c.Now))
			var got []uint64
			for _, e := range c.Events {
				clock.ns.Store(e.AtNs)
				p := sdktrace.SamplingParameters{ParentContext: parentContext(t, e.Parent), Name: "s",
					Attributes: []attribute.KeyValue{attribute.Bool("test.sampled", e.Sampled)}}
				p.TraceID = idAt(0) // sampled at any rate above 0
				if !e.Sampled {
					p.TraceID = idAt(1<<63 - 1) // sampled only at rate 1
				}
				sampled := controls.Sampler.ShouldSample(p).Decision == sdktrace.RecordAndSample
				got = append(got, map[bool]uint64{true: 1, false: 0}[sampled])
			}
			if fmt.Sprint(got) != fmt.Sprint(c.Want) {
				t.Fatalf("sampled %v, want %v", got, c.Want)
			}
		})
	}
	if ran < 10 {
		t.Fatalf("%d trace cases: caps.json was cut short", ran)
	}
}

// The spec's edge cases, on the real SDK: the first rule wins, a rule of rate 0 over
// sampling_rate 1, parents followed whatever the rules say, children of a sampled root kept.
func TestSamplingRulesOnTheSDK(t *testing.T) {
	c := NewControls()
	rec := tracetest.NewSpanRecorder()
	tp := c.TracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSampler(c.Sampler.Wrap(sdktrace.ParentBased(sdktrace.AlwaysSample()))),
		sdktrace.WithSpanProcessor(rec)))
	tracer := tp.Tracer("t")
	c.Apply(settingsOf(t, []byte(`{"sampling_rate":"1","sampling_rules":[{"rate":0,"kind":"server","path":"/health*"},`+
		`{"rate":1,"name":"checkout *"},{"rate":0,"name":"checkout*"}]}`), 0))
	start := func(ctx context.Context, name string, kind trace.SpanKind, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
		return tracer.Start(ctx, name, trace.WithSpanKind(kind), trace.WithAttributes(attrs...))
	}
	sampled := func(s trace.Span) bool { s.End(); return s.SpanContext().IsSampled() }

	_, health := start(context.Background(), "GET", trace.SpanKindServer, attribute.String("url.path", "/healthz"))
	_, target := start(context.Background(), "GET", trace.SpanKindServer, attribute.String("http.target", "/health?full=1"))
	_, client := start(context.Background(), "GET", trace.SpanKindClient, attribute.String("url.path", "/healthz"))
	_, other := start(context.Background(), "GET", trace.SpanKindServer, attribute.String("url.path", "/checkout"))
	ctx, cart := start(context.Background(), "checkout cart", trace.SpanKindInternal)
	_, child := start(ctx, "GET", trace.SpanKindServer, attribute.String("url.path", "/healthz"))
	_, alone := start(context.Background(), "checkoutx", trace.SpanKindInternal)
	got := []bool{sampled(health), sampled(target), sampled(client), sampled(other), sampled(child), sampled(cart), sampled(alone)}
	if fmt.Sprint(got) != "[false false true true true true false]" {
		t.Fatalf("health, http.target, client, other, child of cart, cart, checkoutx sampled %v", got)
	}

	// A caller's decision is followed: sampled under a rule of rate 0, unsampled under rate 1.
	for _, flags := range []trace.TraceFlags{trace.FlagsSampled, 0} {
		remote := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: trace.TraceID{9}, SpanID: trace.SpanID{9}, TraceFlags: flags, Remote: true}))
		c.Apply(settingsOf(t, []byte(fmt.Sprintf(`{"sampling_rules":[{"rate":%d}]}`, 1-int(flags))), 0))
		if _, s := start(remote, "GET", trace.SpanKindServer); sampled(s) != flags.IsSampled() {
			t.Fatalf("a caller sampled %v was not followed", flags.IsSampled())
		}
	}
}

// Rules change while spans start, and leaving them out restores what came before them: the
// rate, then the application's own sampler.
func TestSamplingRulesChangeLiveAndRevert(t *testing.T) {
	c := NewControls()
	c.Sampler.Wrap(sdktrace.ParentBased(appSampler{}))
	p := sdktrace.SamplingParameters{ParentContext: context.Background(), Name: "GET", Kind: trace.SpanKindServer,
		Attributes: []attribute.KeyValue{attribute.String("url.path", "/health")}}
	apply := func(doc string) { c.Apply(settingsOf(t, []byte(doc), 0)) }
	steps := []struct {
		doc  string
		want samplingWant
	}{
		{`{}`, samplingWant{How: "app_sampler"}},
		{`{"sampling_rules":[{"rate":0,"path":"/health"}]}`, samplingWant{How: "rate", Rate: 0}},
		{`{"sampling_rules":[{"rate":0.25,"path":"/health"}],"sampling_rate":"1"}`, samplingWant{How: "rate", Rate: 0.25}},
		{`{"sampling_rate":"1"}`, samplingWant{How: "rate", Rate: 1}},
		{`{"sampling_rules":"[{\"rate\":0.5}]"}`, samplingWant{How: "rate", Rate: 0.5}},
		{`{}`, samplingWant{How: "app_sampler"}},
	}
	for _, s := range steps {
		apply(s.doc)
		if err := judge(c.Sampler, p, s.want); err != nil {
			t.Fatalf("%s: %v", s.doc, err)
		}
	}
	// The configured sampler, set after the rules (as Setup does), keeps them.
	apply(`{"sampling_rules":[{"rate":0}]}`)
	c.Sampler.Wrap(sdktrace.AlwaysSample())
	if err := judge(c.Sampler, p, samplingWant{How: "rate", Rate: 0}); err != nil {
		t.Fatal(err)
	}
}

// A rule of 0.5 samples half of 10,000 random trace IDs, within 3%.
func TestARuleOfAHalfSamplesHalf(t *testing.T) {
	c := NewControls()
	c.Apply(settingsOf(t, []byte(`{"sampling_rules":[{"rate":0.5,"name":"GET"}]}`), 0))
	r := rand.New(rand.NewPCG(7, 8))
	n := 0
	for range 10_000 {
		var id trace.TraceID
		binary.BigEndian.PutUint64(id[:8], r.Uint64())
		binary.BigEndian.PutUint64(id[8:], r.Uint64())
		p := sdktrace.SamplingParameters{ParentContext: context.Background(), TraceID: id, Name: "GET"}
		if c.Sampler.ShouldSample(p).Decision == sdktrace.RecordAndSample {
			n++
		}
	}
	if n < 4850 || n > 5150 {
		t.Fatalf("%d of 10000 sampled at 0.5", n)
	}
}

// The trace cap on the real SDK: roots and spans joining a caller's sampled trace take tokens,
// children of a sampled local span don't and stay sampled; a dropped root's children are dropped
// with it; the cap's removal is at once.
func TestTheTraceCapOnTheSDK(t *testing.T) {
	c := NewControls()
	clock := withClock(c)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(c.Sampler))
	tracer := tp.Tracer("t")
	c.Apply(settingsOf(t, []byte(`{"sampling_traces_per_second":"2"}`), 0))
	root := func(ctx context.Context) (context.Context, bool) {
		ctx, s := tracer.Start(ctx, "root")
		defer s.End()
		return ctx, s.SpanContext().IsSampled()
	}
	ctx, first := root(context.Background())
	for range 10 {
		if _, child := root(ctx); !child {
			t.Fatal("a local child of a sampled span was capped")
		}
	}
	_, second := root(context.Background())
	_, third := root(context.Background())
	if !first || !second || third {
		t.Fatalf("roots sampled %v %v %v, want the first 2 of a cap of 2", first, second, third)
	}
	remote := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{9}, SpanID: trace.SpanID{9}, TraceFlags: trace.FlagsSampled, Remote: true}))
	if _, joined := root(remote); joined {
		t.Fatal("a span joining a sampled caller's trace took no token")
	}
	dropped, _ := root(context.Background())
	if _, child := root(dropped); child {
		t.Fatal("the child of a capped root was sampled")
	}
	clock.ns.Add(500_000_000)
	if _, again := root(context.Background()); !again {
		t.Fatal("half a second at 2 a second refills one")
	}
	c.Apply(settingsOf(t, []byte(`{"sampling_traces_per_second":"2"}`), 0)) // the same: the same bucket
	if _, again := root(context.Background()); again {
		t.Fatal("the same cap applied again refilled the bucket")
	}
	c.Apply(settingsOf(t, []byte(`{}`), 0))
	for range 5 {
		if _, s := root(context.Background()); !s {
			t.Fatal("the cap left out still capped")
		}
	}
	if !strings.Contains(c.Sampler.Description(), "OpampLiveSampler") {
		t.Fatal(c.Sampler.Description())
	}
}

// The rules on the sampler against the layer's (Evaluate), on generated spans and rules.
func TestSamplingRulesMatchTheLayers(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	word := func() string {
		var b strings.Builder
		for range r.IntN(4) {
			b.WriteRune([]rune("ab*/é")[r.IntN(5)])
		}
		return b.String()
	}
	kindNames := []string{"server", "client", "internal", "producer", "consumer"}
	parents := []string{"none", "none", "none", "local_sampled", "remote_unsampled"}
	pv := map[string]settingspb.Parent{"none": settingspb.Parent_PARENT_NONE, "local_sampled": settingspb.Parent_PARENT_LOCAL_SAMPLED,
		"remote_unsampled": settingspb.Parent_PARENT_REMOTE_UNSAMPLED}
	for i := 0; i < 300; i++ {
		var rules []map[string]any
		for range r.IntN(4) {
			rule := map[string]any{"rate": []float64{0, 0.25, 1}[r.IntN(3)]}
			if r.IntN(2) == 0 {
				rule["name"] = word()
			}
			if r.IntN(3) == 0 {
				rule["kind"] = kindNames[r.IntN(5)]
			}
			if r.IntN(3) == 0 {
				rule["path"] = "/" + word()
			}
			if r.IntN(3) == 0 {
				rule["attributes"] = map[string]string{"k": word()}
			}
			rules = append(rules, rule)
		}
		settings := map[string]any{"sampling_rules": rules}
		if r.IntN(2) == 0 {
			settings["sampling_rate"] = "0.5"
		}
		doc, _ := json.Marshal(settings)
		name, kind, parent := word(), kindNames[r.IntN(5)], parents[r.IntN(5)]
		var attrs []attribute.KeyValue
		var wire []*protobufs.KeyValue
		if r.IntN(2) == 0 {
			p := "/" + word()
			key := []string{"url.path", "http.target"}[r.IntN(2)]
			if key == "http.target" && r.IntN(2) == 0 {
				p += "?q=" + word()
			}
			attrs = append(attrs, attribute.String(key, p))
			wire = append(wire, &protobufs.KeyValue{Key: key, Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: p}}})
		}
		if r.IntN(2) == 0 {
			n := int64(r.IntN(300)) - 100
			attrs = append(attrs, attribute.Int64("k", n))
			wire = append(wire, &protobufs.KeyValue{Key: "k", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_IntValue{IntValue: n}}})
		}
		want, err := testLayer(t).Evaluate(context.Background(), &settingspb.Evaluate{Settings: doc, Host: goHost(),
			NowUnixMs: vectorNow, Input: &settingspb.Evaluate_Span{Span: &settingspb.SpanStart{Name: name,
				Kind: settingspb.SpanKind(kinds[kind]), Attributes: wire, Parent: pv[parent]}}})
		if err != nil || want.GetRefused() != "" {
			t.Fatalf("%s: %v %v", doc, want, err)
		}
		w := samplingWant{Rate: want.GetSampling().GetRate(), Sampled: want.GetSampling().GetParentSampled()}
		switch want.GetSampling().GetHow() {
		case settingspb.SpanSampling_FOLLOW_PARENT:
			w.How = "follow_parent"
		case settingspb.SpanSampling_RATE:
			w.How = "rate"
		case settingspb.SpanSampling_APP_SAMPLER:
			w.How = "app_sampler"
		}
		c := NewControls()
		c.Sampler.Wrap(appSampler{})
		c.Apply(settingsOf(t, doc, 0))
		p := sdktrace.SamplingParameters{ParentContext: parentContext(t, parent), Name: name, Kind: kinds[kind], Attributes: attrs}
		if err := judge(c.Sampler, p, w); err != nil {
			t.Fatalf("%s, span %q %s %v %s: %v (the layer: %v)", doc, name, kind, attrs, parent, err, want.GetSampling())
		}
	}
}
