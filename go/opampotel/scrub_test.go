// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
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

// vectorBody is a log body as scrub.json writes it: a string, or a map of members as attributes
// (maps nested).
type vectorBody struct {
	Key    string       `json:"key"`
	String *string      `json:"string"`
	Int    *json.Number `json:"int"`
	Bool   *bool        `json:"bool"`
	Double *float64     `json:"double"`
	Array  []string     `json:"array"`
	Map    []vectorBody `json:"map"`
}

func (b vectorBody) value(t testing.TB) attribute.Value {
	t.Helper()
	if b.Map != nil {
		var members []attribute.KeyValue
		for _, m := range b.Map {
			members = append(members, attribute.KeyValue{Key: attribute.Key(m.Key), Value: m.value(t)})
		}
		return attribute.MapValue(members...)
	}
	return vectorAttribute{Key: b.Key, String: b.String, Int: b.Int, Bool: b.Bool, Double: b.Double,
		Array: b.Array}.kv(t).Value
}

// What scrubbing leaves of attributes, as each of the four is exported (a span's, a span event's,
// a span link's through the span gate, and a log record's through the log gate), and of a log
// record's body (scrub.json).
func TestTheScrubbingVectors(t *testing.T) {
	var v struct {
		About string
		Cases []struct {
			Name               string            `json:"name"`
			Settings           json.RawMessage   `json:"settings"`
			Attributes         []vectorAttribute `json:"attributes"`
			Want               []vectorAttribute `json:"want"`
			Body               *vectorBody       `json:"body"`
			WantBody           *vectorBody       `json:"want_body"`
			DiffersFromLogfire string            `json:"differs_from_logfire"`
		}
	}
	decodeStrict(t, readVectors[json.RawMessage](t, "scrub.json"), &v)
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
			if show(span.Attributes()) != show(in) {
				t.Error("the SDK's span changed")
			}

			sink := &recordSink{}
			lp := sdklog.NewLoggerProvider(sdklog.WithAllowKeyDuplication(),
				sdklog.WithProcessor(sdklog.NewSimpleProcessor(controls.Logs(sink))))
			var r log.Record
			r.AddAttributes(in...)
			if c.Body != nil {
				r.SetBody(c.Body.value(t))
			}
			lp.Logger("app").Emit(context.Background(), r)
			got := sink.take()[0]
			var attrs []attribute.KeyValue
			got.WalkAttributes(func(kv attribute.KeyValue) bool { attrs = append(attrs, kv); return true })
			if show(attrs) != want && !(len(attrs) == 0 && want == "") {
				t.Errorf("log record attributes: %s, want %s", show(attrs), want)
			}
			if c.Body != nil {
				if g, w := got.Body().Emit(), c.WantBody.value(t).Emit(); g != w {
					t.Errorf("log body: %s, want %s", g, w)
				}
			}
		})
	}
	if len(v.Cases) < 30 {
		t.Fatalf("%d cases: scrub.json was cut short", len(v.Cases))
	}
}

// scrubbingPolicy is a policy whose scrubbing control has the defaults and the patterns; with
// neither, the control is local.
func scrubbingPolicy(defaults bool, patterns []string) string {
	if !defaults && len(patterns) == 0 {
		return every()
	}
	var fields []string
	if defaults {
		fields = append(fields, `"defaults":true`)
	}
	if len(patterns) > 0 {
		b, _ := json.Marshal(patterns)
		fields = append(fields, `"patterns":`+string(b))
	}
	return every("scrubbing", override(strings.Join(fields, ",")))
}

// Scrubbing starts, changes and stops while spans and records are exported, and other exporters
// see the application's telemetry as it was.
func TestScrubbingChangesLiveAndReverts(t *testing.T) {
	c := NewControls()
	gated, plain := tracetest.NewInMemoryExporter(), tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(c.Spans(gated)), sdktrace.WithSyncer(plain))
	span := func() string {
		_, s := tp.Tracer("t").Start(context.Background(), "s",
			trace.WithAttributes(attribute.String("api_key", "k1"), attribute.String("note", "my secret is x"),
				attribute.String("iban", "DE89")))
		s.End()
		defer gated.Reset()
		defer plain.Reset()
		if p := show(plain.GetSpans()[0].Attributes); p != "api_key=STRING:k1 note=STRING:my secret is x iban=STRING:DE89" {
			t.Fatalf("the ungated exporter saw %s", p)
		}
		return show(gated.GetSpans()[0].Attributes)
	}
	asIs := "api_key=STRING:k1 note=STRING:my secret is x iban=STRING:DE89"
	if got := span(); got != asIs {
		t.Fatalf("nothing set: %s", got)
	}
	c.Apply(settingsOf(t, []byte(scrubbingPolicy(true, nil)), 0))
	if got := span(); got != "api_key=STRING:[Scrubbed due to 'api_key'] note=STRING:[Scrubbed due to 'secret'] iban=STRING:DE89" {
		t.Fatalf("defaults: %s", got)
	}
	c.Apply(settingsOf(t, []byte(scrubbingPolicy(false, []string{"IBAN"})), 0))
	if got := span(); got != "api_key=STRING:k1 note=STRING:my secret is x iban=STRING:[Scrubbed due to 'iban']" {
		t.Fatalf("patterns: %s", got)
	}
	c.Apply(settingsOf(t, []byte(every()), 0))
	if got := span(); got != asIs {
		t.Fatalf("local: %s", got)
	}

	c.Apply(settingsOf(t, []byte(scrubbingPolicy(true, nil)), 0))
	gatedLogs, plainLogs := &recordSink{}, &recordSink{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(c.Logs(gatedLogs))),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(plainLogs)))
	var r log.Record
	r.SetBody(attribute.MapValue(attribute.String("token", "t"), attribute.String("cookie", "c"),
		attribute.Slice("lines", attribute.StringValue("wrong password"), attribute.IntValue(3))))
	lp.Logger("app").Emit(context.Background(), r)
	if got := gatedLogs.take()[0].Body().Emit(); got != `{"cookie":"[Scrubbed due to 'cookie']","lines":["[Scrubbed due to 'password']",3],"token":"t"}` {
		t.Fatalf("gated body: %s", got)
	}
	if got := plainLogs.take()[0].Body().Emit(); got != `{"cookie":"c","lines":["wrong password",3],"token":"t"}` {
		t.Fatalf("the ungated exporter saw the body %s", got)
	}
}

// The gates' scrubbing against the layer's (Evaluate), on generated keys, values and bodies made
// of the default patterns' pieces.
func TestScrubbingMatchesTheLayers(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	pieces := []string{"pass", "word", "auth", "or", "s", "api", "key", "_", "-", ".", " ", "jwt", "ssn", "pylf_v",
		"1", "x", "K", "K", "ſ", "İ", "é", "session", "safe"}
	text := func() string {
		var b strings.Builder
		for range r.IntN(6) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		return b.String()
	}
	for i := 0; i < 400; i++ {
		var patterns []string
		for range r.IntN(3) {
			if p := strings.Trim(text(), " ") + "*"; p != "*" && !strings.ContainsAny(p, ",") {
				patterns = append(patterns, p)
			}
		}
		doc := []byte(scrubbingPolicy(r.IntN(4) != 0, patterns))
		var in []attribute.KeyValue
		var wire []*protobufs.KeyValue
		for range r.IntN(5) {
			key, value := text(), text()
			if key == "" {
				key = "k"
			}
			in = append(in, attribute.String(key, value))
			wire = append(wire, &protobufs.KeyValue{Key: key,
				Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: value}}})
		}
		body := text()
		want, err := testLayer(t).Evaluate(context.Background(), &settingspb.Evaluate{Settings: doc, Host: goHost(),
			NowUnixMs: vectorNow, Input: &settingspb.Evaluate_Attributes{Attributes: &settingspb.Attributes{Attributes: wire,
				Body: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: body}}}}})
		if err != nil || want.GetRefused() != "" {
			t.Fatalf("%s: %v %v", doc, want, err)
		}
		var expect []string
		for _, kv := range want.GetRedacted() {
			expect = append(expect, fmt.Sprintf("%s=STRING:%s", kv.GetKey(), kv.GetValue().GetStringValue()))
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
		g, _ := scrubValue(&c.gates.Load().redact.scrub, nil, attribute.StringValue(body))
		if g.AsString() != want.GetBody().GetStringValue() {
			t.Fatalf("%s on the body %q: the gate made %q, the layer %q", doc, body, g.AsString(), want.GetBody().GetStringValue())
		}
	}
}
