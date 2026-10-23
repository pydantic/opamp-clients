// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package logfirepolicyprocessor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processorhelper"
	"go.opentelemetry.io/collector/processor/processortest"

	"github.com/pydantic/opamp-clients/go/logfirepolicyprocessor/sdkpolicy"
)

// exports is a signal_exports control: each of traces, metrics and logs is "" (local), "on" or "off".
func exports(traces, metrics, logs string) string {
	one := func(s string) string {
		switch s {
		case "on":
			return `{"mode":"override","enabled":true}`
		case "off":
			return `{"mode":"override","enabled":false}`
		}
		return `{"mode":"local"}`
	}
	return `{"traces":` + one(traces) + `,"metrics":` + one(metrics) + `,"logs":` + one(logs) + `}`
}

// doc is a policy with the two controls it must send (head_sampling local unless given), and
// the others given as pairs of a control's name and its JSON value.
func doc(set ...string) string {
	given := map[string]string{"head_sampling": `{"mode":"local"}`, "signal_exports": exports("", "", "")}
	order := []string{"head_sampling", "signal_exports"}
	for i := 0; i+1 < len(set); i += 2 {
		if _, ok := given[set[i]]; !ok {
			order = append(order, set[i])
		}
		given[set[i]] = set[i+1]
	}
	b := `{"schema":"logfire-sdk-policy/v1"`
	for _, c := range order {
		b += `,"` + c + `":` + given[c]
	}
	return b + "}"
}

func policyFor(t *testing.T, d string) *policy {
	t.Helper()
	p, err := newPolicy(context.Background(), &Config{Policy: d})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func vectors[T any](t *testing.T, name string) T {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sdk-settings", name))
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// refusedHere says whether the layer refuses the policy for this processor: one that overrides a
// control it doesn't carry out (debug mode, say). Those cases aren't the processor's to judge.
func refusedHere(t *testing.T, d json.RawMessage) bool {
	t.Helper()
	_, err := Settings(context.Background(), d)
	if err != nil && !strings.Contains(err.Error(), "cannot apply the requested override") {
		t.Fatalf("the layer refused %s: %v", d, err)
	}
	return err != nil
}

// A log record's way through the processor: send_logs, send_logs_level, send_logs_level_by_scope
// (logs.json), as through the Go integration's gate.
func TestTheLogVectors(t *testing.T) {
	v := vectors[struct {
		Cases []struct {
			Name     string          `json:"name"`
			Settings json.RawMessage `json:"settings"`
			Record   struct {
				Scope    string `json:"scope"`
				Severity int    `json:"severity"`
			} `json:"record"`
			Kept bool `json:"kept"`
		}
	}](t, "logs.json")
	ran := 0
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if refusedHere(t, c.Settings) {
				t.Skip("a control this processor doesn't carry out")
			}
			ran++
			ld := plog.NewLogs()
			sl := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty()
			sl.Scope().SetName(c.Record.Scope)
			sl.LogRecords().AppendEmpty().SetSeverityNumber(plog.SeverityNumber(c.Record.Severity))
			out, err := policyFor(t, string(c.Settings)).logs(context.Background(), ld)
			kept := err == nil && out.LogRecordCount() == 1
			if kept != c.Kept {
				t.Fatalf("kept %v (%v), want %v", kept, err, c.Kept)
			}
			// A batch with nothing left isn't sent on as an empty one.
			if !kept && !errors.Is(err, processorhelper.ErrSkipProcessingData) {
				t.Fatalf("nothing kept, yet %d resources sent on", out.ResourceLogs().Len())
			}
		})
	}
	if ran < 30 {
		t.Fatalf("%d cases run: logs.json was cut short, or too many refused", ran)
	}
}

// otlpAttributes is the vector's attributes (each a key and one of string, int, bool, double or
// array) as OTLP/JSON writes them. A repeated key stays repeated, as a decoded message keeps it.
func otlpAttributes(t *testing.T, attrs []map[string]json.RawMessage) string {
	t.Helper()
	var out []map[string]any
	for _, a := range attrs {
		var key string
		if err := json.Unmarshal(a["key"], &key); err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		switch {
		case a["string"] != nil:
			value = map[string]any{"stringValue": a["string"]}
		case a["int"] != nil:
			value = map[string]any{"intValue": string(a["int"])}
		case a["bool"] != nil:
			value = map[string]any{"boolValue": a["bool"]}
		case a["double"] != nil:
			value = map[string]any{"doubleValue": a["double"]}
		case a["array"] != nil:
			var items []string
			if err := json.Unmarshal(a["array"], &items); err != nil {
				t.Fatal(err)
			}
			var values []map[string]string
			for _, it := range items {
				values = append(values, map[string]string{"stringValue": it})
			}
			value = map[string]any{"arrayValue": map[string]any{"values": values}}
		default:
			t.Fatalf("attribute %v", a)
		}
		out = append(out, map[string]any{"key": key, "value": value})
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// render is the attributes as the vectors write them.
func render(m pcommon.Map) string {
	var out []map[string]any
	m.Range(func(k string, v pcommon.Value) bool {
		e := map[string]any{"key": k}
		switch v.Type() {
		case pcommon.ValueTypeStr:
			e["string"] = v.Str()
		case pcommon.ValueTypeInt:
			e["int"] = v.Int()
		case pcommon.ValueTypeBool:
			e["bool"] = v.Bool()
		case pcommon.ValueTypeDouble:
			e["double"] = v.Double()
		case pcommon.ValueTypeSlice:
			e["array"] = v.Slice().AsRaw()
		}
		out = append(out, e)
		return true
	})
	b, _ := json.Marshal(out)
	return string(b)
}

// What attribute_redaction leaves of a span's attributes, its event's and its link's, and of a
// log record's (redact.json), as the Go integration's gates leave them.
func TestTheRedactVectors(t *testing.T) {
	v := vectors[struct {
		Cases []struct {
			Name       string                       `json:"name"`
			Settings   json.RawMessage              `json:"settings"`
			Attributes []map[string]json.RawMessage `json:"attributes"`
			Want       []map[string]json.RawMessage `json:"want"`
		}
	}](t, "redact.json")
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			p := policyFor(t, string(c.Settings))
			attrs, want := otlpAttributes(t, c.Attributes), otlpAttributes(t, c.Want)
			wantRendered := renderOTLP(t, want)

			traceID := `"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"0102030405060708"`
			td, err := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces([]byte(`{"resourceSpans":[{"resource":{"attributes":` + attrs +
				`},"scopeSpans":[{"scope":{"name":"s","attributes":` + attrs + `},"spans":[{` + traceID + `,"name":"GET","attributes":` + attrs +
				`,"events":[{"name":"e","attributes":` + attrs + `}],"links":[{` + traceID + `,"attributes":` + attrs + `}]}]}]}]}`))
			if err != nil {
				t.Fatal(err)
			}
			td, err = p.traces(context.Background(), td)
			if err != nil {
				t.Fatal(err)
			}
			rs := td.ResourceSpans().At(0)
			span := rs.ScopeSpans().At(0).Spans().At(0)
			for what, m := range map[string]pcommon.Map{"span": span.Attributes(), "event": span.Events().At(0).Attributes(),
				"link": span.Links().At(0).Attributes()} {
				if got := render(m); got != wantRendered {
					t.Errorf("%s: %s, want %s", what, got, wantRendered)
				}
			}
			// Not the resource's or the scope's, as in an SDK.
			if got, sent := render(rs.Resource().Attributes()), renderOTLP(t, attrs); got != sent {
				t.Errorf("resource: %s, want it as sent: %s", got, sent)
			}
			if got, sent := render(rs.ScopeSpans().At(0).Scope().Attributes()), renderOTLP(t, attrs); got != sent {
				t.Errorf("scope: %s, want it as sent: %s", got, sent)
			}

			ld, err := (&plog.JSONUnmarshaler{}).UnmarshalLogs([]byte(`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"attributes":` + attrs + `}]}]}]}`))
			if err != nil {
				t.Fatal(err)
			}
			ld, err = p.logs(context.Background(), ld)
			if err != nil {
				t.Fatal(err)
			}
			if got := render(ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Attributes()); got != wantRendered {
				t.Errorf("log record: %s, want %s", got, wantRendered)
			}
		})
	}
	if len(v.Cases) < 20 {
		t.Fatalf("%d cases: redact.json was cut short", len(v.Cases))
	}
}

// renderOTLP is OTLP/JSON attributes as render writes them.
func renderOTLP(t *testing.T, attrs string) string {
	t.Helper()
	ld, err := (&plog.JSONUnmarshaler{}).UnmarshalLogs([]byte(`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"attributes":` + attrs + `}]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return render(ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Attributes())
}

// idAt is a trace ID whose low 8 bytes are x.
func idAt(x uint64) pcommon.TraceID {
	var id pcommon.TraceID
	binary.BigEndian.PutUint64(id[8:], x)
	return id
}

// The processor doesn't sample: head_sampling overridden is refused (TestRefused), and with it
// local every span goes on, whatever its trace ID.
func TestEverySpanKept(t *testing.T) {
	p := policyFor(t, doc("attribute_redaction", `{"mode":"override","mask":["*token*"]}`))
	td := ptrace.NewTraces()
	spans := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans()
	for _, x := range []uint64{0, 1 << 62, 1<<64 - 1} {
		spans.AppendEmpty().SetTraceID(idAt(x))
	}
	if out, err := p.traces(context.Background(), td); err != nil || out.SpanCount() != 3 {
		t.Fatalf("%d of 3 spans, %v", out.SpanCount(), err)
	}
}

// signal_exports: a signal switched off sends nothing on; on, or local, everything.
func TestSignalExports(t *testing.T) {
	newTraces := func() ptrace.Traces {
		td := ptrace.NewTraces()
		td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty().SetTraceID(idAt(1))
		return td
	}
	newMetrics := func() pmetric.Metrics {
		md := pmetric.NewMetrics()
		md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetEmptyGauge().DataPoints().AppendEmpty()
		return md
	}
	newLogs := func() plog.Logs {
		ld := plog.NewLogs()
		ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		return ld
	}
	for _, c := range []struct{ traces, metrics, logs string }{
		{"off", "", ""}, {"", "off", ""}, {"", "", "off"}, {"on", "on", "on"}, {"", "", ""}, {"off", "off", "off"},
	} {
		p := policyFor(t, doc("signal_exports", exports(c.traces, c.metrics, c.logs)))
		_, terr := p.traces(context.Background(), newTraces())
		_, merr := p.metrics(context.Background(), newMetrics())
		_, lerr := p.logs(context.Background(), newLogs())
		for _, s := range []struct {
			name, set string
			err       error
		}{{"traces", c.traces, terr}, {"metrics", c.metrics, merr}, {"logs", c.logs, lerr}} {
			if sent := s.err == nil; sent != (s.set != "off") {
				t.Errorf("%+v: %s sent %v", c, s.name, sent)
			}
		}
	}
}

// A policy that overrides a control this processor doesn't carry out is refused, with the
// control's name first, as the layer says it to a server; so is one that isn't a policy.
func TestRefused(t *testing.T) {
	for control, value := range map[string]string{
		"head_sampling":    `{"mode":"override","ratio":0.25}`,
		"sampling_rules":   `{"mode":"override","rules":[{"rate":0}]}`,
		"trace_rate_limit": `{"mode":"override","traces_per_second":3}`,
		"log_rate_limit":   `{"mode":"override","records_per_second":3}`,
		"debug":            `{"mode":"override","until":"2026-10-21T18:00:00Z"}`,
		"app_settings":     `{"mode":"override","values":{"a":1}}`,
		"instrumentations": `{"mode":"override","all_disabled":true}`,
		"scrubbing":        `{"mode":"override","defaults":true}`,
	} {
		err := (&Config{Policy: doc(control, value)}).Validate()
		if err == nil || !strings.HasPrefix(err.Error(), control+": ") {
			t.Errorf("%s: %v", control, err)
		}
	}
	for d, prefix := range map[string]string{"": "policy: ", `{"schema":"x"}`: "schema: ", "[": "policy: "} {
		if err := (&Config{Policy: d}).Validate(); err == nil || !strings.HasPrefix(err.Error(), prefix) {
			t.Errorf("%q: %v", d, err)
		}
	}
	if err := (&Config{Policy: doc("attribute_redaction", `{"mode":"override","mask":["*token*"]}`,
		"log_levels", `{"mode":"override","level":"warn"}`)}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// What the processor adds to an agent's description: the policy format, and the controls it
// carries out, which a server then overrides, and no others.
func TestDescription(t *testing.T) {
	d, err := sdkpolicy.Description(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, kv := range d {
		if a := kv.GetValue().GetArrayValue(); a != nil {
			var list []string
			for _, v := range a.GetValues() {
				list = append(list, v.GetStringValue())
			}
			got[kv.GetKey()] = strings.Join(list, ",")
		} else {
			got[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
	}
	want := map[string]any{
		"logfire.opamp.config_format":      "logfire-sdk-policy/v1",
		"logfire.opamp.supported_controls": "signal_exports.traces,signal_exports.metrics,signal_exports.logs,log_levels,attribute_redaction",
	}
	if g, w := asJSON(got), asJSON(want); g != w {
		t.Fatalf("%s, want %s", g, w)
	}
}

func asJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// The processor as a collector makes it, from its factory and config, in a pipeline.
func TestTheFactorysProcessors(t *testing.T) {
	f := NewFactory()
	if f.Type().String() != "logfirepolicy" {
		t.Fatal(f.Type())
	}
	cfg := f.CreateDefaultConfig().(*Config)
	if err := cfg.Validate(); err == nil {
		t.Fatal("a config without a policy validated")
	}
	cfg.Policy = doc("attribute_redaction", `{"mode":"override","drop":["secret"]}`, "log_levels", `{"mode":"override","level":"warn"}`)
	ctx, set := context.Background(), processortest.NewNopSettings(Type)

	traces := new(consumertest.TracesSink)
	tp, err := f.CreateTraces(ctx, set, cfg, traces)
	if err != nil {
		t.Fatal(err)
	}
	logs := new(consumertest.LogsSink)
	lp, err := f.CreateLogs(ctx, set, cfg, logs)
	if err != nil {
		t.Fatal(err)
	}
	metrics := new(consumertest.MetricsSink)
	mp, err := f.CreateMetrics(ctx, set, cfg, metrics)
	if err != nil {
		t.Fatal(err)
	}
	host := componenttest.NewNopHost()
	if err := tp.Start(ctx, host); err != nil {
		t.Fatal(err)
	}
	if err := lp.Start(ctx, host); err != nil {
		t.Fatal(err)
	}
	if err := mp.Start(ctx, host); err != nil {
		t.Fatal(err)
	}
	if !tp.Capabilities().MutatesData {
		t.Fatal("the processor changes data, and must say so")
	}

	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.Attributes().PutStr("secret", "s")
	span.Attributes().PutStr("kept", "k")
	if err := tp.ConsumeTraces(ctx, td); err != nil {
		t.Fatal(err)
	}
	ld := plog.NewLogs()
	records := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	records.AppendEmpty().SetSeverityNumber(plog.SeverityNumberInfo)
	records.AppendEmpty().SetSeverityNumber(plog.SeverityNumberError)
	if err := lp.ConsumeLogs(ctx, ld); err != nil {
		t.Fatal(err)
	}
	md := pmetric.NewMetrics()
	md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetEmptyGauge().DataPoints().AppendEmpty()
	if err := mp.ConsumeMetrics(ctx, md); err != nil {
		t.Fatal(err)
	}

	if got := traces.AllTraces(); len(got) != 1 || render(got[0].ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Attributes()) != `[{"key":"kept","string":"k"}]` {
		t.Fatalf("traces: %v", got)
	}
	if got := logs.LogRecordCount(); got != 1 {
		t.Fatalf("%d log records, want the error alone", got)
	}
	if got := metrics.DataPointCount(); got != 1 {
		t.Fatalf("%d data points", got)
	}
	for _, c := range []interface{ Shutdown(context.Context) error }{tp, lp, mp} {
		if err := c.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// A policy the layer refuses makes no processor.
	cfg.Policy = doc("sampling_rules", `{"mode":"override","rules":[{"rate":0}]}`)
	if _, err := f.CreateTraces(ctx, set, cfg, traces); err == nil || !strings.HasPrefix(err.Error(), "sampling_rules: ") {
		t.Fatalf("a refused policy: %v", err)
	}
}
