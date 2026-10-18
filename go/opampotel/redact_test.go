// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// vectorAttribute is an attribute as the vectors write it: a key and one typed value.
type vectorAttribute struct {
	Key    string       `json:"key"`
	String *string      `json:"string"`
	Int    *json.Number `json:"int"`
	Bool   *bool        `json:"bool"`
	Double *float64     `json:"double"`
	Array  []string     `json:"array"`
}

func (a vectorAttribute) kv(t testing.TB) attribute.KeyValue {
	t.Helper()
	switch {
	case a.String != nil:
		return attribute.String(a.Key, *a.String)
	case a.Int != nil:
		n, err := a.Int.Int64()
		if err != nil {
			t.Fatal(err)
		}
		return attribute.Int64(a.Key, n)
	case a.Bool != nil:
		return attribute.Bool(a.Key, *a.Bool)
	case a.Double != nil:
		return attribute.Float64(a.Key, *a.Double)
	case a.Array != nil:
		return attribute.StringSlice(a.Key, a.Array)
	}
	t.Fatalf("attribute %q has no value", a.Key)
	return attribute.KeyValue{}
}

func kvs(t testing.TB, attrs []vectorAttribute) []attribute.KeyValue {
	out := []attribute.KeyValue{}
	for _, a := range attrs {
		out = append(out, a.kv(t))
	}
	return out
}

// show is attributes as a test compares them: each key=TYPE:value, in order.
func show(attrs []attribute.KeyValue) string {
	var out []string
	for _, kv := range attrs {
		out = append(out, fmt.Sprintf("%s=%s:%s", kv.Key, kv.Value.Type(), kv.Value.Emit()))
	}
	return strings.Join(out, " ")
}

func decodeStrict(t testing.TB, b []byte, v any) {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		t.Fatal(err)
	}
}

// What redaction leaves of attributes, as each of the four is exported: a span's, a span
// event's, a span link's (through the span gate) and a log record's (through the log gate),
// with the application's own unchanged (redact.json).
func TestTheRedactionVectors(t *testing.T) {
	var v struct {
		About string
		Cases []struct {
			Name       string            `json:"name"`
			Settings   json.RawMessage   `json:"settings"`
			Attributes []vectorAttribute `json:"attributes"`
			Want       []vectorAttribute `json:"want"`
		}
	}
	decodeStrict(t, readVectors[json.RawMessage](t, "redact.json"), &v)
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			controls := NewControls()
			controls.Apply(settingsOf(t, c.Settings, 0))
			in, want := kvs(t, c.Attributes), show(kvs(t, c.Want))

			span := tracetest.SpanStub{Name: "s", Attributes: in,
				Events: []sdktrace.Event{{Name: "e", Attributes: in}},
				Links:  []sdktrace.Link{{Attributes: in}}}.Snapshot()
			mem := tracetest.NewInMemoryExporter()
			if err := controls.Spans(mem).ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{span}); err != nil {
				t.Fatal(err)
			}
			out := mem.GetSpans()[0]
			for what, got := range map[string][]attribute.KeyValue{"span": out.Attributes,
				"event": out.Events[0].Attributes, "link": out.Links[0].Attributes} {
				if show(got) != want {
					t.Errorf("%s attributes: %s, want %s", what, show(got), want)
				}
			}
			if show(span.Attributes()) != show(in) || show(span.Events()[0].Attributes) != show(in) ||
				show(span.Links()[0].Attributes) != show(in) {
				t.Error("the SDK's span changed")
			}

			sink := &recordSink{}
			lp := sdklog.NewLoggerProvider(sdklog.WithAllowKeyDuplication(),
				sdklog.WithProcessor(sdklog.NewSimpleProcessor(controls.Logs(sink))))
			var r log.Record
			r.AddAttributes(in...)
			lp.Logger("app").Emit(context.Background(), r)
			var got []attribute.KeyValue
			sink.take()[0].WalkAttributes(func(kv attribute.KeyValue) bool { got = append(got, kv); return true })
			// The log SDK takes no attribute with an empty key (attribute.KeyValue.Valid): such a
			// record never reaches an exporter with one.
			var valid []attribute.KeyValue
			for _, kv := range kvs(t, c.Want) {
				if kv.Valid() {
					valid = append(valid, kv)
				}
			}
			if show(got) != show(valid) {
				t.Errorf("log record attributes: %s, want %s", show(got), show(valid))
			}
		})
	}
	if len(v.Cases) < 20 {
		t.Fatalf("%d cases: redact.json was cut short", len(v.Cases))
	}
}

// Another exporter, one the gate doesn't cover, sees the application's span and record as they
// were: the gate changes a view and a copy.
func TestRedactionLeavesTheSDKsTelemetryAlone(t *testing.T) {
	c := NewControls()
	c.Apply(settingsOf(t, []byte(redactionPolicy([]string{"user.email"}, []string{"secret"})), 0))
	gated, plain := tracetest.NewInMemoryExporter(), tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(c.Spans(gated)), sdktrace.WithSyncer(plain))
	_, other := sdktrace.NewTracerProvider().Tracer("o").Start(context.Background(), "other")
	_, s := tp.Tracer("t").Start(context.Background(), "s",
		trace.WithAttributes(attribute.String("secret", "x"), attribute.String("user.email", "a@b.c"), attribute.Int("n", 1)),
		trace.WithLinks(trace.Link{SpanContext: other.SpanContext(), Attributes: []attribute.KeyValue{attribute.String("secret", "l")}}))
	s.AddEvent("e", trace.WithAttributes(attribute.String("user.email", "e@f.g")))
	s.End()
	g, p := gated.GetSpans()[0], plain.GetSpans()[0]
	if show(g.Attributes) != "user.email=STRING:[REDACTED] n=INT64:1" || show(g.Events[0].Attributes) != "user.email=STRING:[REDACTED]" ||
		len(g.Links[0].Attributes) != 0 {
		t.Fatalf("gated: %s / %s / %s", show(g.Attributes), show(g.Events[0].Attributes), show(g.Links[0].Attributes))
	}
	if show(p.Attributes) != "secret=STRING:x user.email=STRING:a@b.c n=INT64:1" || show(p.Events[0].Attributes) != "user.email=STRING:e@f.g" ||
		show(p.Links[0].Attributes) != "secret=STRING:l" {
		t.Fatalf("the ungated exporter saw %s / %s / %s", show(p.Attributes), show(p.Events[0].Attributes), show(p.Links[0].Attributes))
	}
	if !g.SpanContext.Equal(p.SpanContext) || g.Name != "s" || g.InstrumentationScope.Name != "t" {
		t.Fatal("the redacted span is not the same span otherwise")
	}

	gatedLogs, plainLogs := &recordSink{}, &recordSink{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(c.Logs(gatedLogs))),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(plainLogs)))
	var r log.Record
	r.AddAttributes(attribute.String("secret", "x"), attribute.String("user.email", "a@b.c"))
	r.SetBody(attribute.StringValue("the body stays"))
	lp.Logger("app").Emit(context.Background(), r)
	attrs := func(r sdklog.Record) string {
		var out []attribute.KeyValue
		r.WalkAttributes(func(kv attribute.KeyValue) bool { out = append(out, kv); return true })
		return show(out)
	}
	gl, pl := gatedLogs.take()[0], plainLogs.take()[0]
	if attrs(gl) != "user.email=STRING:[REDACTED]" || gl.Body().AsString() != "the body stays" {
		t.Fatalf("gated record: %s", attrs(gl))
	}
	if attrs(pl) != "secret=STRING:x user.email=STRING:a@b.c" {
		t.Fatalf("the ungated exporter saw %s", attrs(pl))
	}
}

// A drop doesn't change the count of attributes the SDK's limits dropped from a span.
func TestRedactionKeepsASpansDroppedCount(t *testing.T) {
	c := NewControls()
	c.Apply(settingsOf(t, []byte(redactionPolicy(nil, []string{"a"})), 0))
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(c.Spans(mem)),
		sdktrace.WithSpanLimits(sdktrace.SpanLimits{AttributeCountLimit: 2, EventCountLimit: -1, LinkCountLimit: -1,
			AttributeValueLengthLimit: -1, AttributePerEventCountLimit: -1, AttributePerLinkCountLimit: -1}))
	_, s := tp.Tracer("t").Start(context.Background(), "s",
		trace.WithAttributes(attribute.Int("a", 1), attribute.Int("b", 2), attribute.Int("c", 3)))
	s.End()
	got := mem.GetSpans()[0]
	if show(got.Attributes) != "b=INT64:2" || got.DroppedAttributes != 1 {
		t.Fatalf("%s, %d dropped; want b, 1 dropped (by the limit, not redaction)", show(got.Attributes), got.DroppedAttributes)
	}
}

// Redaction changes and stops while spans and records are exported.
func TestRedactionChangesLiveAndReverts(t *testing.T) {
	c := NewControls()
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(c.Spans(mem)))
	span := func() string {
		_, s := tp.Tracer("t").Start(context.Background(), "s",
			trace.WithAttributes(attribute.String("Authorization", "Bearer x"), attribute.String("user", "u")))
		s.End()
		defer mem.Reset()
		return show(mem.GetSpans()[0].Attributes)
	}
	asIs := "Authorization=STRING:Bearer x user=STRING:u"
	if got := span(); got != asIs {
		t.Fatalf("nothing set: %s", got)
	}
	c.Apply(settingsOf(t, []byte(redactionPolicy(nil, []string{"authorization"})), 0))
	if got := span(); got != "user=STRING:u" {
		t.Fatalf("drop: %s", got)
	}
	c.Apply(settingsOf(t, []byte(redactionPolicy([]string{"AUTHORIZATION", "user"}, nil)), 0))
	if got := span(); got != "Authorization=STRING:[REDACTED] user=STRING:[REDACTED]" {
		t.Fatalf("mask: %s", got)
	}
	c.Apply(settingsOf(t, []byte(every()), 0))
	if got := span(); got != asIs {
		t.Fatalf("local: %s", got)
	}
}

// redactionPolicy is a policy whose attribute_redaction control masks and drops the given globs; with
// neither, the control is local.
func redactionPolicy(mask, drop []string) string {
	if len(mask) == 0 && len(drop) == 0 {
		return every()
	}
	var fields []string
	for _, f := range []struct {
		name  string
		globs []string
	}{{"mask", mask}, {"drop", drop}} {
		if len(f.globs) > 0 {
			b, _ := json.Marshal(f.globs)
			fields = append(fields, `"`+f.name+`":`+string(b))
		}
	}
	return every("attribute_redaction", override(strings.Join(fields, ",")))
}

// The gates' redaction against the layer's (Evaluate), on generated keys and patterns.
func TestRedactionMatchesTheLayers(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	word := func(letters string) string {
		var b strings.Builder
		for range r.IntN(5) {
			b.WriteRune([]rune(letters)[r.IntN(len([]rune(letters)))])
		}
		return b.String()
	}
	for i := 0; i < 300; i++ {
		var drop, mask []string
		for range r.IntN(3) {
			if w := word("aB.*é"); w != "" {
				drop = append(drop, w)
			}
		}
		for range r.IntN(3) {
			if w := word("Ab.*É"); w != "" {
				mask = append(mask, w)
			}
		}
		doc := []byte(redactionPolicy(mask, drop))
		var in []attribute.KeyValue
		var wire []*protobufs.KeyValue
		for range r.IntN(5) {
			key := word("abAB.é")
			in = append(in, attribute.Int(key, 7))
			wire = append(wire, &protobufs.KeyValue{Key: key, Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_IntValue{IntValue: 7}}})
		}
		want, err := testLayer(t).Evaluate(context.Background(), &settingspb.Evaluate{Settings: doc, Host: goHost(),
			NowUnixMs: vectorNow, Input: &settingspb.Evaluate_Attributes{Attributes: &settingspb.Attributes{Attributes: wire}}})
		if err != nil || want.GetRefused() != "" {
			t.Fatalf("%s: %v %v", doc, want, err)
		}
		var expect []string
		for _, kv := range want.GetRedacted() {
			if s, ok := kv.GetValue().GetValue().(*protobufs.AnyValue_StringValue); ok {
				expect = append(expect, fmt.Sprintf("%s=STRING:%s", kv.GetKey(), s.StringValue))
			} else {
				expect = append(expect, fmt.Sprintf("%s=INT64:%d", kv.GetKey(), kv.GetValue().GetIntValue()))
			}
		}
		c := NewControls()
		c.Apply(settingsOf(t, doc, 0))
		mem := tracetest.NewInMemoryExporter()
		span := tracetest.SpanStub{Name: "s", Attributes: in}.Snapshot()
		if err := c.Spans(mem).ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{span}); err != nil {
			t.Fatal(err)
		}
		if got := show(mem.GetSpans()[0].Attributes); got != strings.Join(expect, " ") {
			t.Fatalf("%s on %s: the gate left %s, the layer %s", doc, show(in), got, strings.Join(expect, " "))
		}
	}
}
