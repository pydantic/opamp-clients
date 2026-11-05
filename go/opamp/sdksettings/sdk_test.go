// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package sdksettings

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

const policyFile = "logfire-sdk-policy.json"

func config(body string) *protobufs.AgentRemoteConfig {
	return &protobufs.AgentRemoteConfig{ConfigHash: []byte("h"), Config: &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigObject{policyFile: {Body: []byte(body), ContentType: "application/json"}}}}
}

// effective is the policy that the effective config reports. The test fails if the effective
// config holds another file, or the policy is not JSON.
func effective(t *testing.T, e *protobufs.EffectiveConfig) string {
	t.Helper()
	files := e.GetConfigMap().GetConfigMap()
	f, ok := files[policyFile]
	if len(files) != 1 || !ok || f.GetContentType() != "application/json" {
		t.Fatalf("the effective config is %v, want %s alone", e, policyFile)
	}
	return string(f.GetBody())
}

// The controls of a policy in order: it must send the first two, and may leave out the others.
var controls = []string{"head_sampling", "signal_exports", "sampling_rules", "trace_rate_limit", "log_levels",
	"log_rate_limit", "attribute_redaction", "debug", "app_settings", "instrumentations", "scrubbing", "self_monitoring"}

const allLocal = `{"traces":{"mode":"local"},"metrics":{"mode":"local"},"logs":{"mode":"local"}}`

// logsOff is a signal_exports control that switches the log export off.
const logsOff = `{"traces":{"mode":"local"},"metrics":{"mode":"local"},"logs":{"mode":"override","enabled":false}}`

// policy is a policy of the schema with its first n controls, each local unless set gives it: set
// holds pairs of a control's name and its JSON value.
func policy(schema string, n int, set ...string) string {
	given := map[string]string{}
	for i := 0; i+1 < len(set); i += 2 {
		if !slices.Contains(controls[:n], set[i]) {
			panic("no control " + set[i] + " in " + schema)
		}
		given[set[i]] = set[i+1]
	}
	b := `{"schema":"` + schema + `"`
	for _, c := range controls[:n] {
		v, ok := given[c]
		if !ok {
			v = `{"mode":"local"}`
			if c == "signal_exports" {
				v = allLocal
			}
		}
		b += `,"` + c + `":` + v
	}
	return b + "}"
}

// v1 is a policy of the two controls it must send, each local unless set gives it.
func v1(set ...string) string { return policy("logfire-sdk-policy/v1", 2, set...) }

// every is a policy that sends every control, each local unless set gives it.
func every(set ...string) string { return policy("logfire-sdk-policy/v1", len(controls), set...) }

// override is a control's override with the given fields.
func override(fields string) string { return `{"mode":"override",` + fields + `}` }

// sorted is a policy that sends every control as the layer writes the effective config: compact,
// keys sorted.
func sorted(t *testing.T, doc string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// full is a policy as the layer writes the effective config: every control, those left out local,
// compact, keys sorted.
func full(t *testing.T, doc string) string {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range controls {
		if _, ok := v[c]; !ok {
			v[c] = map[string]any{"mode": "local"}
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// flow is the layer's agent flow, stepped by a test: its last output.
type flow struct {
	t   *testing.T
	l   *Layer
	out *settingspb.AgentOutput
}

// startFlow starts the flow for host at now (on both clocks), with a state directory or not and
// the client's saved state (nil: none).
func startFlow(t *testing.T, l *Layer, host *settingspb.Host, stateDir bool, coreState []byte) *flow {
	t.Helper()
	out, err := l.Start(context.Background(), &settingspb.AgentStart{Host: host, StateDir: stateDir, CoreState: coreState,
		Endpoint: "http://127.0.0.1:1", InstanceUid: make([]byte, 16), NowUnixMs: now, NowMonoMs: now})
	if err != nil {
		t.Fatal(err)
	}
	return &flow{t, l, out}
}

// step gives the flow one input at now plus ms (on both clocks).
func (f *flow) step(ms uint64, input func(*settingspb.AgentStep)) *flow {
	f.t.Helper()
	st := &settingspb.AgentStep{Agent: f.out.GetAgent(), NowUnixMs: now + ms, NowMonoMs: now + ms}
	input(st)
	out, err := f.l.Step(context.Background(), st)
	if err != nil {
		f.t.Fatal(err)
	}
	f.out = out
	return f
}

// decide sends doc as a remote config.
func (f *flow) decide(ms uint64, c *protobufs.AgentRemoteConfig) *flow {
	return f.step(ms, func(s *settingspb.AgentStep) { s.Input = &settingspb.AgentStep_RemoteConfig{RemoteConfig: c} })
}

// apply is the settings the last step put in force (nil: none).
func (f *flow) apply() *settingspb.Settings {
	for _, e := range f.out.GetEffects() {
		if e.GetApply() != nil {
			return e.GetApply()
		}
	}
	return nil
}

// status is the remote config status the last step gave (nil: none).
func (f *flow) status() *protobufs.RemoteConfigStatus {
	for _, e := range f.out.GetEffects() {
		if e.GetStatus() != nil {
			return e.GetStatus()
		}
	}
	return nil
}

// health is the health the last step sent (nil: none).
func (f *flow) health() *protobufs.ComponentHealth {
	for _, e := range f.out.GetEffects() {
		if e.GetHealth() != nil {
			return e.GetHealth()
		}
	}
	return nil
}

// warned is whether the last step logged a warning or worse.
func (f *flow) warned() bool {
	for _, e := range f.out.GetEffects() {
		if e.GetLog().GetLevel() >= settingspb.AgentLog_WARN {
			return true
		}
	}
	return false
}

// state is the flow's state as it stands.
func (f *flow) state() *settingspb.AgentState {
	f.t.Helper()
	var st settingspb.AgentState
	if err := proto.Unmarshal(f.out.GetAgent(), &st); err != nil {
		f.t.Fatal(err)
	}
	return &st
}

const (
	applied = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED
	failed  = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED
)

// The layer's agent flow through the module: a config decided and answered, a bad one refused
// with nothing applied, what the host does not carry out refused, a host with a bug an error.
func TestTheAgentFlow(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	f := startFlow(t, l, declaring, true, nil)
	if f.apply() == nil || effective(t, f.out.GetEffective()) != full(t, v1()) {
		t.Fatalf("start: %v", f.out)
	}
	doc := v1("head_sampling", override(`"ratio":0.25`))
	f.decide(1, config(doc))
	if f.status().GetStatus() != applied || f.apply().GetSamplingRate() != 0.25 || effective(t, f.out.GetEffective()) != full(t, doc) {
		t.Fatalf("decide: %v", f.out)
	}
	f.decide(2, config(v1("head_sampling", override(`"ratio":"x"`))))
	if f.status().GetStatus() != failed || !strings.HasPrefix(f.status().GetErrorMessage(), "head_sampling:") ||
		f.apply() != nil || effective(t, f.out.GetEffective()) != full(t, doc) {
		t.Fatalf("bad: %v", f.out)
	}
	// The policy is the remote config's only file: none, or another beside it, is refused.
	other := config(doc)
	other.Config.ConfigMap["other.json"] = &protobufs.AgentConfigObject{Body: []byte("{}"), ContentType: "application/json"}
	none := &protobufs.AgentRemoteConfig{ConfigHash: []byte("h"), Config: &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigObject{"other.json": {Body: []byte("{}"), ContentType: "application/json"}}}}
	for name, c := range map[string]*protobufs.AgentRemoteConfig{"another file": other, "no policy": none} {
		if f.decide(3, c); f.status().GetStatus() != failed || !strings.HasPrefix(f.status().GetErrorMessage(), "config_map:") {
			t.Fatalf("%s: %v", name, f.out)
		}
	}
	// An override that the host does not carry out is refused, saying which, and changes nothing.
	g := startFlow(t, l, &settingspb.Host{Live: []string{"sampling_rate"}}, false, nil)
	g.decide(1, config(v1("signal_exports", logsOff)))
	if g.status().GetStatus() != failed || g.apply() != nil ||
		g.status().GetErrorMessage() != "signal_exports.logs: this SDK cannot apply the requested override" {
		t.Fatalf("v1, not carried out: %v", g.out)
	}
	g.decide(2, config(every("instrumentations", override(`"disabled":["otelhttp"]`))))
	if g.status().GetErrorMessage() != "instrumentations: disabled: this SDK cannot apply the requested override" {
		t.Fatalf("every, not carried out: %v", g.out)
	}
	// A next-start change needs somewhere to keep state.
	h := startFlow(t, l, declaring, false, nil)
	h.decide(1, config(every("instrumentations", override(`"disabled":["redis"]`))))
	if h.status().GetErrorMessage() != "instrumentations: takes effect at the next start, which needs somewhere to keep state" {
		t.Fatalf("next start, nowhere: %v", h.out)
	}
	// A host naming what is no setting has a bug: an error, saying which.
	if _, err := l.Start(ctx, &settingspb.AgentStart{Host: &settingspb.Host{Live: []string{"send_metric"}},
		InstanceUid: make([]byte, 16)}); err == nil || !strings.Contains(err.Error(), "send_metric") {
		t.Fatalf("a host declaring send_metric: %v", err)
	}
	// A step without an input, or with a state that isn't one, is an error.
	if _, err := l.Step(ctx, &settingspb.AgentStep{Agent: f.out.GetAgent()}); err == nil {
		t.Fatal("a step without an input")
	}
	if _, err := l.Step(ctx, &settingspb.AgentStep{Agent: []byte{0xff}, Input: &settingspb.AgentStep_Timer{}}); err == nil {
		t.Fatal("a step on a state that isn't one")
	}
	// A log level: the lowest severity number to export; unset, every severity.
	doc = every("log_levels", override(`"level":"warn"`))
	if f.decide(4, config(doc)); f.status().GetStatus() != applied || f.apply().SendLogsMinSeverity == nil ||
		f.apply().GetSendLogsMinSeverity() != 13 || effective(t, f.out.GetEffective()) != sorted(t, doc) {
		t.Fatalf("log level: %v", f.out)
	}
	desc, err := l.Describe(ctx, []*protobufs.KeyValue{
		{Key: "service.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "checkout"}}},
		{Key: "host.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "h"}}},
	}, declaring, now)
	if err != nil || len(desc.GetIdentifyingAttributes()) != 1 || attribute(desc, "host.name") != "h" {
		t.Fatalf("describe: %v %v", desc, err)
	}
}

// attribute is a non-identifying attribute of the description: a string as itself, an array as
// its strings joined by commas; "<none>" when it is not there.
func attribute(d *protobufs.AgentDescription, key string) string {
	for _, kv := range d.GetNonIdentifyingAttributes() {
		if kv.GetKey() != key {
			continue
		}
		if a := kv.GetValue().GetArrayValue(); a != nil {
			var s []string
			for _, v := range a.GetValues() {
				s = append(s, v.GetStringValue())
			}
			return strings.Join(s, ",")
		}
		return kv.GetValue().GetStringValue()
	}
	return "<none>"
}

// The description tells a server the policy format the layer reads and the controls the host
// carries out: debug only with a clock, and none for a host that declares nothing.
func TestTheDescriptionNamesTheFormatAndTheControls(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	all := "head_sampling,signal_exports.traces,signal_exports.metrics,signal_exports.logs,sampling_rules,trace_rate_limit," +
		"log_levels,log_rate_limit,attribute_redaction,debug,app_settings,instrumentations,scrubbing,self_monitoring"
	for _, c := range []struct {
		name     string
		host     *settingspb.Host
		now      uint64
		controls string
	}{
		{"every setting, with a clock", everySetting, now, all},
		{"every setting, no clock", everySetting, 0, strings.Replace(all, ",debug", "", 1)},
		{"every setting but the application's own", declaring, now, strings.Replace(all, ",app_settings", "", 1)},
		{"the head sampling and the export switches", &settingspb.Host{Live: []string{"sampling_rate", "send_traces", "send_logs"}}, now,
			"head_sampling,signal_exports.traces,signal_exports.logs"},
		{"no host", nil, now, ""},
	} {
		d, err := l.Describe(ctx, nil, c.host, c.now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := attribute(d, "logfire.opamp.config_format"); got != "logfire-sdk-policy/v1" {
			t.Errorf("%s: config_format %q", c.name, got)
		}
		if got := attribute(d, "logfire.opamp.supported_controls"); got != c.controls {
			t.Errorf("%s: supported_controls %q, want %q", c.name, got, c.controls)
		}
	}
}

// A host that declares every setting: each live one at once, each next-start one at the next start.
var declaring = &settingspb.Host{Live: []string{"debug_sampling_rate", "debug_send_logs_level", "debug_until",
	"drop_attributes", "mask_attributes", "sampling_rate", "sampling_rules", "sampling_traces_per_second", "send_logs",
	"scrub_defaults", "scrub_patterns", "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second", "send_metrics",
	"send_self_metrics",
	"send_traces"},
	NextStart: []string{"deactivate_all_instrumentations", "deactivate_instrumentations"}}

// everySetting is declaring with the application's own settings (custom) too.
var everySetting = &settingspb.Host{Live: append(slices.Clone(declaring.Live), "custom"), NextStart: declaring.NextStart}

const now = uint64(1_791_979_200_000) // 2026-10-14T12:00:00Z

// The settings that match telemetry arrive typed where the host declares them. Where it does not,
// the policy is refused.
func TestTheNewSettingsArrive(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	doc := every("sampling_rules", override(`"rules":[{"rate":0,"kind":"server","path":"/health*"}]`),
		"log_levels", override(`"scopes":{"com.acme.db*":"debug"}`),
		"attribute_redaction", override(`"mask":["User.Email"],"drop":["*token*"]`),
		"trace_rate_limit", override(`"traces_per_second":50`),
		"log_rate_limit", override(`"records_per_second":0.5`))
	f := startFlow(t, l, declaring, false, nil).decide(1, config(doc))
	if f.status().GetStatus() != applied {
		t.Fatalf("decide: %v", f.out)
	}
	s := f.apply()
	rule := s.GetSamplingRules()[0]
	if len(s.GetSamplingRules()) != 1 || rule.GetRate() != 0 || rule.GetKind() != settingspb.SpanKind_SPAN_KIND_SERVER ||
		rule.GetPath() != "/health*" {
		t.Fatalf("rules: %v", s.GetSamplingRules())
	}
	if lv := s.GetSendLogsScopeLevels(); len(lv) != 1 || lv[0].GetPattern() != "com.acme.db*" || lv[0].GetMinSeverity() != 5 {
		t.Fatalf("scope levels: %v", lv)
	}
	if fmt.Sprint(s.GetDropAttributes(), s.GetMaskAttributes()) != "[*token*] [user.email]" {
		t.Fatalf("redaction: %v %v", s.GetDropAttributes(), s.GetMaskAttributes())
	}
	if s.GetSamplingTracesPerSecond() != 50 || s.GetSendLogsPerSecond() != 0.5 {
		t.Fatalf("caps: %v", s)
	}
	plain := startFlow(t, l, nil, false, nil).decide(1, config(doc))
	if plain.status().GetStatus() != failed || plain.apply() != nil ||
		!strings.HasSuffix(plain.status().GetErrorMessage(), ": this SDK cannot apply the requested override") {
		t.Fatalf("undeclared: %v", plain.out)
	}
}

// Debug mode arrives folded in, with a wake for its end; the timer then ends it.
func TestDebugModeEndsOnTheTimer(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	doc := every("head_sampling", override(`"ratio":0.1`), "debug", override(`"until":"2026-10-14T12:30:00Z","log_level":"info"`))
	f := startFlow(t, l, declaring, true, nil).decide(0, config(doc))
	end := uint64(30 * 60_000)
	if f.apply().GetSamplingRate() != 1 || f.apply().GetSendLogsMinSeverity() != 9 || f.state().GetRefreshAtUnixMs() != now+end ||
		f.out.GetWakeAtMonoMs() == 0 || f.out.GetWakeAtMonoMs() > now+end {
		t.Fatalf("decide: %v", f.out)
	}
	f.step(end, func(s *settingspb.AgentStep) { s.Input = &settingspb.AgentStep_Timer{Timer: &settingspb.ExportCounts{}} })
	if f.apply().GetSamplingRate() != 0.1 || f.apply().SendLogsMinSeverity != nil || f.state().GetRefreshAtUnixMs() != 0 ||
		effective(t, f.out.GetEffective()) != sorted(t, every("head_sampling", override(`"ratio":0.1`))) {
		t.Fatalf("the end: %v", f.out)
	}
}

// Export health: sent when the SDK is built and on a failure, held while nothing changes; an
// exporter the gates can't reach is listed under its signal, ungoverned and healthy.
func TestExportHealth(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	exporters := func(ms uint64, counts *settingspb.ExportCounts, x ...*settingspb.Exporter) func(*settingspb.AgentStep) {
		return func(s *settingspb.AgentStep) {
			s.Input = &settingspb.AgentStep_Exporters{Exporters: &settingspb.AgentExporters{Exporters: x, Counts: counts}}
		}
	}
	f := startFlow(t, l, declaring, true, nil)
	f.step(0, func(s *settingspb.AgentStep) { s.Input = &settingspb.AgentStep_Built{Built: &settingspb.AgentBuilt{Service: "checkout"}} })
	f.step(0, exporters(0, &settingspb.ExportCounts{}))
	first := f.health().GetComponentHealthMap()["exporters"]
	if len(first.GetComponentHealthMap()) != 3 || first.GetStatus() != "ok" {
		t.Fatalf("first: %v", f.out)
	}
	if f.step(30_000, exporters(0, &settingspb.ExportCounts{})); f.health() != nil {
		t.Fatalf("again: %v", f.out)
	}
	f.step(30_000, exporters(0, &settingspb.ExportCounts{}, &settingspb.Exporter{Signal: "traces", Name: "zipkin"}))
	child := f.health().GetComponentHealthMap()["exporters"].GetComponentHealthMap()["traces"].GetComponentHealthMap()["zipkin"]
	if child.GetStatus() != "ungoverned" || !child.GetHealthy() {
		t.Fatalf("ungoverned: %v", f.out)
	}
	f.step(31_000, func(s *settingspb.AgentStep) {
		s.Input = &settingspb.AgentStep_Timer{Timer: &settingspb.ExportCounts{Traces: &settingspb.SignalCounts{
			ExportsFailed: 1, ItemsFailed: 512, LastFailed: true, LastError: "connection refused"}}}
	})
	f.step(31_000, exporters(0, &settingspb.ExportCounts{Traces: &settingspb.SignalCounts{
		ExportsFailed: 1, ItemsFailed: 512, LastFailed: true, LastError: "connection refused"}}))
	m := f.state().GetHealth().GetExporters()
	if m.GetHealthy() || m.GetComponentHealthMap()["traces"].GetLastError() != "connection refused" {
		t.Fatalf("failed: %v", m)
	}
}

// Evaluate gives the reference matchers' answer for each kind of input, and a refusal's reason.
func TestEvaluate(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	doc := []byte(every("sampling_rules", override(`"rules":[{"rate":0,"kind":"server","path":"/health*"}]`),
		"log_levels", override(`"scopes":{"noisy":"off"}`),
		"attribute_redaction", override(`"mask":["*token*"]`),
		"log_rate_limit", override(`"records_per_second":2`)))
	str := func(s string) *protobufs.AnyValue {
		return &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: s}}
	}
	span, err := l.Evaluate(ctx, &settingspb.Evaluate{Settings: doc, Host: declaring, Input: &settingspb.Evaluate_Span{
		Span: &settingspb.SpanStart{Name: "GET", Kind: settingspb.SpanKind_SPAN_KIND_SERVER,
			Attributes: []*protobufs.KeyValue{{Key: "url.path", Value: str("/healthz")}}}}})
	if err != nil || span.GetSampling().GetHow() != settingspb.SpanSampling_RATE || span.GetSampling().GetRate() != 0 ||
		span.GetSampling().Rule == nil || span.GetSampling().GetRule() != 0 {
		t.Fatalf("span: %v %v", span, err)
	}
	logged, err := l.Evaluate(ctx, &settingspb.Evaluate{Settings: doc, Host: declaring, Input: &settingspb.Evaluate_Log{
		Log: &settingspb.LogRecord{Scope: "noisy", Severity: 21}}})
	if err != nil || logged.Kept == nil || logged.GetKept() {
		t.Fatalf("log: %v %v", logged, err)
	}
	redacted, err := l.Evaluate(ctx, &settingspb.Evaluate{Settings: doc, Host: declaring, Input: &settingspb.Evaluate_Attributes{
		Attributes: &settingspb.Attributes{Attributes: []*protobufs.KeyValue{{Key: "API_TOKEN", Value: str("x")}}}}})
	if err != nil || len(redacted.GetRedacted()) != 1 || redacted.GetRedacted()[0].GetValue().GetStringValue() != "[REDACTED]" {
		t.Fatalf("attributes: %v %v", redacted, err)
	}
	capped, err := l.Evaluate(ctx, &settingspb.Evaluate{Settings: doc, Host: declaring, Input: &settingspb.Evaluate_Cap{
		Cap: &settingspb.CapRun{Signal: settingspb.CapRun_LOGS, Events: []*settingspb.CapEvent{{Records: 3}, {Records: 1}}}}})
	if err != nil || fmt.Sprint(capped.GetPassed()) != "[2 0]" {
		t.Fatalf("cap: %v %v", capped, err)
	}
	refused, err := l.Evaluate(ctx, &settingspb.Evaluate{Settings: []byte(v1("head_sampling", override(`"ratio":7`))), Host: declaring,
		Input: &settingspb.Evaluate_Log{Log: &settingspb.LogRecord{}}})
	if err != nil || !strings.HasPrefix(refused.GetRefused(), "head_sampling:") {
		t.Fatalf("refused: %v %v", refused, err)
	}
}

// The shared exporters vectors (testdata/sdk-settings/exporters.json), each through Exporters:
// the exporters, governed and ungoverned signals and warnings, each compared whole.
func TestTheExportersVectors(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "sdk-settings", "exporters.json"))
	if err != nil {
		t.Fatal(err)
	}
	type exporter struct {
		Signal      string `json:"signal"`
		Name        string `json:"name"`
		Gated       bool   `json:"gated"`
		Destination string `json:"destination"`
	}
	type decided struct {
		Exporters  []exporter `json:"exporters"`
		Governed   []string   `json:"governed"`
		Ungoverned []string   `json:"ungoverned"`
		Warnings   []string   `json:"warnings"`
	}
	var v struct {
		Cases []struct {
			Name  string `json:"name"`
			Found []struct {
				Signal   string `json:"signal"`
				Kind     string `json:"kind"`
				Gated    bool   `json:"gated"`
				Endpoint string `json:"endpoint"`
				FromEnv  bool   `json:"from_env"`
			} `json:"found"`
			Env             map[string]string `json:"env"`
			DefaultProtocol string            `json:"default_protocol"`
			Want            decided           `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var found []*settingspb.FoundExporter
			for _, f := range c.Found {
				found = append(found, &settingspb.FoundExporter{Signal: f.Signal, Kind: f.Kind, Gated: f.Gated,
					Endpoint: f.Endpoint, FromEnv: f.FromEnv})
			}
			var env []string
			for k, val := range c.Env {
				env = append(env, k+"="+val)
			}
			got, err := l.Exporters(ctx, found, env, c.DefaultProtocol)
			if err != nil {
				t.Fatal(err)
			}
			have := decided{Exporters: []exporter{}, Governed: got.GetGoverned(), Ungoverned: got.GetUngoverned(),
				Warnings: got.GetWarnings()}
			for _, e := range got.GetExporters() {
				have.Exporters = append(have.Exporters, exporter{e.GetSignal(), e.GetName(), e.GetGated(), e.GetDestination()})
			}
			for _, s := range []*[]string{&have.Governed, &have.Ungoverned, &have.Warnings} {
				if *s == nil {
					*s = []string{}
				}
			}
			if !reflect.DeepEqual(have, c.Want) {
				t.Errorf("got %+v\nwant %+v", have, c.Want)
			}
		})
	}
}

// The shared inventory vectors (testdata/sdk-settings/components.json), each through
// Components, compared as the vectors' about says: each component the case gives, whole.
func TestTheComponentsVectors(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "sdk-settings", "components.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Cases []struct {
			Name      string `json:"name"`
			Inventory struct {
				Integration        string                        `json:"integration"`
				IntegrationVersion string                        `json:"integration_version"`
				SdkLanguage        string                        `json:"sdk_language"`
				SdkVersion         string                        `json:"sdk_version"`
				Instrumentations   []*settingspb.Instrumentation `json:"instrumentations"`
				Exporters          []*settingspb.Exporter        `json:"exporters"`
			} `json:"inventory"`
			Host *struct {
				Live      []string `json:"live"`
				NextStart []string `json:"next_start"`
			} `json:"host"`
			Now  uint64                     `json:"now_unix_ms"`
			Want map[string]json.RawMessage `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			// Left out, a host that declares every setting: each live one (the application's own
			// custom settings too) in live, each next-start one in next_start.
			host := everySetting
			if c.Host != nil {
				host = &settingspb.Host{Live: c.Host.Live, NextStart: c.Host.NextStart}
			}
			in := c.Inventory
			got, err := l.Components(ctx, &settingspb.Inventory{Integration: in.Integration, IntegrationVersion: in.IntegrationVersion,
				SdkLanguage: in.SdkLanguage, SdkVersion: in.SdkVersion, Instrumentations: in.Instrumentations,
				Exporters: in.Exporters}, host, c.Now)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.GetHash()) != 0 {
				t.Fatalf("a hash %x: the client's to compute", got.GetHash())
			}
			rendered := renderComponents(t, got)
			for key, raw := range c.Want {
				var want any
				if err := json.Unmarshal(raw, &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(rendered[key], want) {
					t.Errorf("%s: got %v, want %v", key, rendered[key], want)
				}
			}
		})
	}
	if len(v.Cases) < 15 {
		t.Fatalf("%d cases: components.json was cut short", len(v.Cases))
	}
}

// renderComponents is the components as the vectors write them: sdk (key to text),
// instrumentations (name to version, nil when none), exporters (signal to name to gated) and
// settings (key to applies), each always there.
func renderComponents(t *testing.T, a *protobufs.AvailableComponents) map[string]any {
	t.Helper()
	text := func(d *protobufs.ComponentDetails, key string) any {
		for _, kv := range d.GetMetadata() {
			if kv.GetKey() == key {
				s, ok := kv.GetValue().GetValue().(*protobufs.AnyValue_StringValue)
				if !ok {
					t.Fatalf("%s: not text: %v", key, kv.GetValue())
				}
				return s.StringValue
			}
		}
		return nil
	}
	part := func(key string) *protobufs.ComponentDetails {
		d, ok := a.GetComponents()[key]
		if !ok {
			t.Fatalf("no %s component", key)
		}
		return d
	}
	if len(a.GetComponents()) != 4 {
		t.Fatalf("components %v: want sdk, instrumentations, exporters and settings", a.GetComponents())
	}
	sdk := map[string]any{}
	for _, kv := range part("sdk").GetMetadata() {
		sdk[kv.GetKey()] = text(part("sdk"), kv.GetKey())
	}
	each := func(d *protobufs.ComponentDetails, f func(*protobufs.ComponentDetails) any) map[string]any {
		m := map[string]any{}
		for name, sub := range d.GetSubComponentMap() {
			m[name] = f(sub)
		}
		return m
	}
	return map[string]any{
		"sdk":              sdk,
		"instrumentations": each(part("instrumentations"), func(d *protobufs.ComponentDetails) any { return text(d, "version") }),
		"exporters": each(part("exporters"), func(signal *protobufs.ComponentDetails) any {
			return each(signal, func(e *protobufs.ComponentDetails) any {
				if md := e.GetMetadata(); len(md) >= 1 && md[0].GetKey() == "gated" {
					if g, ok := md[0].GetValue().GetValue().(*protobufs.AnyValue_BoolValue); ok {
						return g.BoolValue
					}
				}
				t.Fatalf("gated: %v", e.GetMetadata())
				return nil
			})
		}),
		// Where each exporter sends, for those that say: signal to identity to destination.
		"destinations": func() map[string]any {
			out := map[string]any{}
			for s, signal := range part("exporters").GetSubComponentMap() {
				m := map[string]any{}
				for name, e := range signal.GetSubComponentMap() {
					if d := text(e, "destination"); d != nil {
						m[name] = d
					}
				}
				if len(m) > 0 {
					out[s] = m
				}
			}
			return out
		}(),
		"settings": each(part("settings"), func(d *protobufs.ComponentDetails) any { return text(d, "applies") }),
	}
}

// Close waits for a call in progress; a call after it fails, and a second Close is nothing.
func TestCloseWhileCalling(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if _, err := l.Describe(ctx, nil, declaring, now); err != nil {
					return
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if _, err := l.Describe(ctx, nil, declaring, now); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("a call after Close: %v", err)
	}
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// Compiling the layer's module makes garbage (wazero's compiler) that goes back to the operating
// system at once, as the core's does (internal/core). In a process of its own: the module is
// compiled once per process.
func TestCompilingTheLayerLeavesNoGarbageResident(t *testing.T) {
	const child = "OPAMP_SDK_COMPILE_CHILD"
	if os.Getenv(child) != "" {
		l, err := New(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close(context.Background())
		var m goruntime.MemStats
		goruntime.ReadMemStats(&m)
		fmt.Printf("resident heap %d\n", m.HeapInuse+m.HeapIdle-m.HeapReleased)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCompilingTheLayerLeavesNoGarbageResident$", "-test.count=1")
	cmd.Env = append(os.Environ(), child+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var resident uint64
	if i := strings.Index(string(out), "resident heap "); i < 0 {
		t.Fatalf("no measurement:\n%s", out)
	} else if _, err := fmt.Sscan(string(out[i+len("resident heap "):]), &resident); err != nil {
		t.Fatal(err)
	}
	if resident > 4<<20 {
		t.Fatalf("%d MB of heap resident after compiling the layer", resident>>20)
	}
}

// The shared start vectors (testdata/sdk-settings/start.json): the policy to start with comes from
// the last config applied in the core's saved state, else the SDK's own settings. A config applied
// without a policy starts from the SDK's own settings, with a warning.
func TestTheStartVectors(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "sdk-settings", "start.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Cases []struct {
			Name      string  `json:"name"`
			CoreState *string `json:"core_state"`
			Want      string  `json:"want"`
			Warns     bool    `json:"warns"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Cases {
		var coreState []byte
		if c.CoreState != nil {
			if coreState, err = hex.DecodeString(*c.CoreState); err != nil {
				t.Fatal(err)
			}
		}
		f := startFlow(t, l, everySetting, true, coreState)
		if got := string(f.state().GetCurrent()); got != c.Want {
			t.Errorf("%s: started from %s, want %s", c.Name, got, c.Want)
		}
		if f.warned() != c.Warns {
			t.Errorf("%s: warned %v, want %v", c.Name, f.warned(), c.Warns)
		}
	}
}
