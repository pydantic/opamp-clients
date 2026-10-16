// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package sdk

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	"github.com/pydantic/opamp-clients/go/opamp/sdk/settingspb"
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

// The controls of a policy in order: version 1 has the first two.
var controls = []string{"head_sampling", "signal_exports", "sampling_rules", "trace_rate_limit", "log_levels",
	"log_rate_limit", "attribute_redaction", "debug", "app_settings", "instrumentations", "polling_interval"}

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

// v1 is a version 1 policy, every control local unless set gives it.
func v1(set ...string) string { return policy("logfire-sdk-policy/v1", 2, set...) }

// v2 is a version 2 draft policy, every control local unless set gives it.
func v2(set ...string) string { return policy("logfire-sdk-policy/v2-draft", len(controls), set...) }

// override is a control's override with the given fields.
func override(fields string) string { return `{"mode":"override",` + fields + `}` }

// sorted is a policy as the layer writes a version 2 effective config: compact, keys sorted.
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

func status(d *settingspb.Decision) protobufs.RemoteConfigStatuses { return d.GetStatus().GetStatus() }

const (
	applied = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED
	failed  = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED
)

// The layer through the module: what the Rust tests hold it to, reached from Go.
func TestLoadDecideDescribe(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	saved := v1("signal_exports", logsOff)
	loaded, err := l.Load(ctx, []byte(saved), declaring)
	if err != nil || loaded.GetSettings().SendLogs == nil || loaded.GetSettings().GetSendLogs() || effective(t, loaded.GetEffective()) != saved {
		t.Fatalf("load: %v %v", loaded, err)
	}
	doc := v1("head_sampling", override(`"ratio":0.25`))
	d, err := l.Decide(ctx, config(doc), loaded.GetCurrent(), loaded.GetCurrent(), declaring, keep)
	if err != nil || status(d) != applied || d.GetApply().GetSamplingRate() != 0.25 || effective(t, d.GetEffective()) != doc {
		t.Fatalf("decide: %v %v", d, err)
	}
	bad, err := l.Decide(ctx, config(v1("head_sampling", override(`"ratio":"x"`))), d.GetCurrent(), loaded.GetCurrent(), declaring, keep)
	if err != nil || status(bad) != failed || !strings.HasPrefix(bad.GetStatus().GetErrorMessage(), "head_sampling:") ||
		bad.GetApply() != nil || effective(t, bad.GetEffective()) != doc {
		t.Fatalf("bad: %v %v", bad, err)
	}
	// The policy is the remote config's only file: none, or another beside it, is refused.
	other := config(doc)
	other.Config.ConfigMap["other.json"] = &protobufs.AgentConfigObject{Body: []byte("{}"), ContentType: "application/json"}
	none := &protobufs.AgentRemoteConfig{ConfigHash: []byte("h"), Config: &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigObject{"other.json": {Body: []byte("{}"), ContentType: "application/json"}}}}
	for name, c := range map[string]*protobufs.AgentRemoteConfig{"another file": other, "no policy": none} {
		r, err := l.Decide(ctx, c, d.GetCurrent(), loaded.GetCurrent(), declaring, keep)
		if err != nil || status(r) != failed || !strings.HasPrefix(r.GetStatus().GetErrorMessage(), "config_map:") {
			t.Fatalf("%s: %v %v", name, r, err)
		}
	}
	// An override that the host does not carry out is refused, saying which, and changes nothing.
	host := &settingspb.Host{Live: []string{"sampling_rate"}}
	d2, err := l.Decide(ctx, config(v1("signal_exports", logsOff)), d.GetCurrent(), nil, host, nil)
	if err != nil || status(d2) != failed || d2.GetApply() != nil ||
		d2.GetStatus().GetErrorMessage() != "signal_exports.logs: this SDK cannot apply the requested override" ||
		string(d2.GetCurrent()) != string(d.GetCurrent()) {
		t.Fatalf("v1, not carried out: %v %v", d2, err)
	}
	d2, err = l.Decide(ctx, config(v2("instrumentations", override(`"disabled":["otelhttp"]`))), nil, nil, host, keep)
	if err != nil || status(d2) != failed ||
		d2.GetStatus().GetErrorMessage() != "instrumentations: disabled: this SDK cannot apply the requested override" {
		t.Fatalf("v2, not carried out: %v %v", d2, err)
	}
	// A host naming what is no setting has a bug: an error, saying which.
	if _, err := l.Load(ctx, nil, &settingspb.Host{Live: []string{"send_metric"}}); err == nil || !strings.Contains(err.Error(), "send_metric") {
		t.Fatalf("a host declaring send_metric: %v", err)
	}
	// A log level: the lowest severity number to export, reported by its name; unset, every severity.
	doc = v2("log_levels", override(`"level":"warn"`))
	d, err = l.Decide(ctx, config(doc), nil, nil, declaring, nil)
	if err != nil || status(d) != applied || d.GetApply().SendLogsMinSeverity == nil || d.GetApply().GetSendLogsMinSeverity() != 13 ||
		effective(t, d.GetEffective()) != sorted(t, doc) || loaded.GetSettings().SendLogsMinSeverity != nil {
		t.Fatalf("log level: %v %v", d, err)
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

// The description tells a server the policy formats the layer reads and the controls the host
// carries out: debug only with a clock, and none for a host that declares nothing.
func TestTheDescriptionNamesTheFormatsAndTheControls(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	every := "head_sampling,signal_exports.traces,signal_exports.metrics,signal_exports.logs,sampling_rules,trace_rate_limit," +
		"log_levels,log_rate_limit,attribute_redaction,debug,app_settings,instrumentations,polling_interval"
	for _, c := range []struct {
		name     string
		host     *settingspb.Host
		now      uint64
		controls string
	}{
		{"every setting, with a clock", everySetting, now, every},
		{"every setting, no clock", everySetting, 0, strings.Replace(every, ",debug", "", 1)},
		{"every setting but the application's own", declaring, now, strings.Replace(every, ",app_settings", "", 1)},
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
		if got := attribute(d, "logfire.opamp.config_formats"); got != "logfire-sdk-policy/v1,logfire-sdk-policy/v2-draft" {
			t.Errorf("%s: config_formats %q", c.name, got)
		}
		if got := attribute(d, "logfire.opamp.supported_controls"); got != c.controls {
			t.Errorf("%s: supported_controls %q, want %q", c.name, got, c.controls)
		}
	}
}

// A host that declares every setting: each live one at once, each next-start one at the next start.
var declaring = &settingspb.Host{Live: []string{"debug_sampling_rate", "debug_send_logs_level", "debug_until",
	"drop_attributes", "mask_attributes", "sampling_rate", "sampling_rules", "sampling_traces_per_second", "send_logs",
	"send_logs_level", "send_logs_level_by_scope", "send_logs_per_second", "send_metrics", "send_traces"},
	NextStart: []string{"deactivate_all_instrumentations", "deactivate_instrumentations", "opamp_polling_interval"}}

// everySetting is declaring with the application's own settings (custom) too.
var everySetting = &settingspb.Host{Live: append(slices.Clone(declaring.Live), "custom"), NextStart: declaring.NextStart}

const now = uint64(1_791_979_200_000) // 2026-10-14T12:00:00Z

// keep is a save that keeps nothing, for a host that has somewhere to keep state.
func keep([]byte) error { return nil }

// Decide, then commit (#215): the integration saves what the layer asks it to keep, and the layer
// answers from how that went, the same in every language.
func TestTheSaveTheLayerAsksForDecidesTheAnswer(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	current := []byte(v1("head_sampling", override(`"ratio":0.5`)))
	full := func([]byte) error { return errors.New("no space left on device") }
	quarter := v1("head_sampling", override(`"ratio":0.25`))
	d, err := l.Decide(ctx, config(quarter), current, current, declaring, full)
	if err != nil || status(d) != failed ||
		d.GetStatus().GetErrorMessage() != "can't save the settings: no space left on device" || d.GetApply() != nil ||
		string(d.GetCurrent()) != string(current) || effective(t, d.GetEffective()) != string(current) {
		t.Fatalf("a failed save: %v %v", d, err)
	}
	// Nowhere to keep state: a live change is applied, a next-start one refused.
	d, err = l.Decide(ctx, config(quarter), current, current, declaring, nil)
	if err != nil || status(d) != applied {
		t.Fatalf("live, nowhere: %v %v", d, err)
	}
	minute := v2("polling_interval", override(`"milliseconds":60000`))
	d, err = l.Decide(ctx, config(minute), current, current, declaring, nil)
	if err != nil || d.GetStatus().GetErrorMessage() !=
		"polling_interval: takes effect at the next start, which needs somewhere to keep state" {
		t.Fatalf("next start, nowhere: %v %v", d, err)
	}
	var kept [][]byte
	d, err = l.Decide(ctx, config(minute), current, current, declaring, func(doc []byte) error {
		kept = append(kept, doc)
		return nil
	})
	if err != nil || status(d) != applied ||
		!slices.Equal(d.GetPendingRestart(), []string{"opamp_polling_interval"}) || len(kept) != 1 ||
		string(kept[0]) != minute {
		t.Fatalf("next start, saved: %v %v %q", d, err, kept)
	}
	// A refused config asks for no save.
	d, err = l.Decide(ctx, config(v1("head_sampling", override(`"ratio":2`))), current, current, declaring, full)
	if err != nil || d.GetStatus().GetErrorMessage() != "head_sampling: ratio must be a number from 0 to 1" {
		t.Fatalf("refused: %v %v", d, err)
	}
}

// The settings that match telemetry arrive typed where the host declares them. Where it does not,
// the policy is refused.
func TestTheNewSettingsArrive(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	doc := v2("sampling_rules", override(`"rules":[{"rate":0,"kind":"server","path":"/health*"}]`),
		"log_levels", override(`"scopes":{"com.acme.db*":"debug"}`),
		"attribute_redaction", override(`"mask":["User.Email"],"drop":["*token*"]`),
		"trace_rate_limit", override(`"traces_per_second":50`),
		"log_rate_limit", override(`"records_per_second":0.5`))
	d, err := l.DecideAt(ctx, config(doc), nil, nil, declaring, now, nil)
	if err != nil || status(d) != applied {
		t.Fatalf("decide: %v %v", d, err)
	}
	s := d.GetApply()
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
	plain, err := l.Decide(ctx, config(doc), nil, nil, nil, nil)
	if err != nil || status(plain) != failed || plain.GetApply() != nil ||
		!strings.HasSuffix(plain.GetStatus().GetErrorMessage(), ": this SDK cannot apply the requested override") {
		t.Fatalf("undeclared: %v %v", plain, err)
	}
}

// Debug mode arrives folded in, with the time to ask again; a refresh then ends it.
func TestDebugModeAndRefresh(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	doc := v2("head_sampling", override(`"ratio":0.1`), "debug", override(`"until":"2026-10-14T12:30:00Z","log_level":"info"`))
	d, err := l.DecideAt(ctx, config(doc), nil, nil, declaring, now, keep)
	end := now + 30*60_000
	if err != nil || d.GetApply().GetSamplingRate() != 1 || d.GetApply().GetSendLogsMinSeverity() != 9 || d.GetRefreshAtUnixMs() != end {
		t.Fatalf("decide: %v %v", d, err)
	}
	r, err := l.Refresh(ctx, d.GetCurrent(), nil, declaring, end)
	if err != nil || r.GetSettings().GetSamplingRate() != 0.1 || r.GetSettings().SendLogsMinSeverity != nil || r.GetRefreshAtUnixMs() != 0 ||
		effective(t, r.GetEffective()) != sorted(t, v2("head_sampling", override(`"ratio":0.1`))) {
		t.Fatalf("refresh: %v %v", r, err)
	}
	loaded, err := l.LoadAt(ctx, d.GetCurrent(), declaring, now)
	if err != nil || loaded.GetRefreshAtUnixMs() != end {
		t.Fatalf("load: %v %v", loaded, err)
	}
}

// Export health: sent at the first call and on a failure, held while nothing changes.
func TestExportHealth(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	first, err := l.Health(ctx, &settingspb.ExportCounts{}, nil, now, nil)
	if err != nil || !first.GetSend() || len(first.GetExporters().GetComponentHealthMap()) != 3 || first.GetExporters().GetStatus() != "ok" {
		t.Fatalf("first: %v %v", first, err)
	}
	if again, err := l.Health(ctx, &settingspb.ExportCounts{}, first, now+30_000, nil); err != nil || again.GetSend() {
		t.Fatalf("again: %v %v", again, err)
	}
	off := false
	failed, err := l.Health(ctx, &settingspb.ExportCounts{Traces: &settingspb.SignalCounts{
		ExportsFailed: 1, ItemsFailed: 512, LastFailed: true, LastError: "connection refused"}},
		first, now+30_000, &settingspb.Settings{SendLogs: &off})
	m := failed.GetExporters().GetComponentHealthMap()
	if err != nil || !failed.GetSend() || failed.GetExporters().GetHealthy() ||
		m["traces"].GetLastError() != "connection refused" || m["logs"].GetStatus() != "off" {
		t.Fatalf("failed: %v %v", failed, err)
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
	doc := []byte(v2("sampling_rules", override(`"rules":[{"rate":0,"kind":"server","path":"/health*"}]`),
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
				if md := e.GetMetadata(); len(md) == 1 && md[0].GetKey() == "gated" {
					if g, ok := md[0].GetValue().GetValue().(*protobufs.AnyValue_BoolValue); ok {
						return g.BoolValue
					}
				}
				t.Fatalf("gated: %v", e.GetMetadata())
				return nil
			})
		}),
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
// the last config applied in the core's saved state, else from the policy kept before. A config
// applied without a policy starts from the SDK's own settings, with a warning.
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
			Saved     *string `json:"saved"`
			Want      string  `json:"want"`
			Warns     bool    `json:"warns"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Cases {
		var coreState, saved []byte
		if c.CoreState != nil {
			if coreState, err = hex.DecodeString(*c.CoreState); err != nil {
				t.Fatal(err)
			}
		}
		if c.Saved != nil {
			saved = []byte(*c.Saved)
		}
		loaded, err := l.LoadFrom(ctx, coreState, saved, everySetting, 0)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if string(loaded.GetCurrent()) != c.Want {
			t.Errorf("%s: started from %s, want %s", c.Name, loaded.GetCurrent(), c.Want)
		}
		if (loaded.GetWarning() != "") != c.Warns {
			t.Errorf("%s: warning %q, want one: %v", c.Name, loaded.GetWarning(), c.Warns)
		}
	}
}
