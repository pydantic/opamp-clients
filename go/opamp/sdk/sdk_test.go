// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package sdk

import (
	"context"
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

func config(body string) *protobufs.AgentRemoteConfig {
	return &protobufs.AgentRemoteConfig{ConfigHash: []byte("h"), Config: &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigObject{"elastic": {Body: []byte(body), ContentType: "application/json"}}}}
}

func effective(e *protobufs.EffectiveConfig) string {
	return string(e.GetConfigMap().GetConfigMap()["elastic"].GetBody())
}

// What the effective config names as set aside (its elastic.set_aside entry); "" without one.
func setAside(t *testing.T, e *protobufs.EffectiveConfig) string {
	t.Helper()
	files := e.GetConfigMap().GetConfigMap()
	for name := range files {
		if name != "elastic" && name != "elastic.set_aside" {
			t.Fatalf("the effective config has %q", name)
		}
	}
	f, ok := files["elastic.set_aside"]
	if !ok {
		return ""
	}
	if f.GetContentType() != "application/json" {
		t.Fatalf("elastic.set_aside is %q", f.GetContentType())
	}
	return string(f.GetBody())
}

// The layer through the module: what the Rust tests hold it to, reached from Go.
func TestLoadDecideDescribe(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	loaded, err := l.Load(ctx, []byte(`{"send_logs":"false"}`), declaring)
	if err != nil || loaded.GetSettings().GetSendLogs() || effective(loaded.GetEffective()) != `{"send_logs":"false"}` {
		t.Fatalf("load: %v %v", loaded, err)
	}
	d, err := l.Decide(ctx, config(`{"sampling_rate":"0.25"}`), loaded.GetCurrent(), loaded.GetCurrent(), declaring, keep)
	if err != nil || d.GetStatus().GetStatus() != protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED ||
		d.GetApply().GetSamplingRate() != 0.25 || effective(d.GetEffective()) != `{"sampling_rate":"0.25"}` ||
		setAside(t, d.GetEffective()) != "" {
		t.Fatalf("decide: %v %v", d, err)
	}
	bad, err := l.Decide(ctx, config(`{"sampling_rate":"x"}`), d.GetCurrent(), loaded.GetCurrent(), declaring, keep)
	if err != nil || bad.GetStatus().GetStatus() != protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED ||
		!strings.HasPrefix(bad.GetStatus().GetErrorMessage(), "sampling_rate:") || bad.GetApply() != nil {
		t.Fatalf("bad: %v %v", bad, err)
	}
	// A host that doesn't declare the instrumentation switch: set aside, never in force; a setting
	// nothing knows is told apart from it.
	host := &settingspb.Host{Live: []string{"sampling_rate"}}
	d, err = l.Decide(ctx, config(`{"deactivate_instrumentations":"otelhttp","x":"1"}`), nil, nil, host, nil)
	if err != nil || d.GetStatus().GetStatus() != protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED ||
		effective(d.GetEffective()) != `{}` || fmt.Sprint(d.GetApply().GetUnsupported(), d.GetApply().GetIgnored()) != "[deactivate_instrumentations] [x]" {
		t.Fatalf("host: %v %v", d, err)
	}
	// Named for the server beside what is in force (#215), by Decide, Load and Refresh alike, and
	// still after a FAILED, which leaves the document in force.
	names := `{"ignored":["x"],"unsupported":["deactivate_instrumentations"]}`
	if got := setAside(t, d.GetEffective()); got != names || d.GetStatus().GetErrorMessage() != "" {
		t.Fatalf("set aside: %q", got)
	}
	if l2, err := l.Load(ctx, d.GetCurrent(), host); err != nil || setAside(t, l2.GetEffective()) != names {
		t.Fatalf("set aside at load: %v %v", l2, err)
	}
	if r, err := l.Refresh(ctx, d.GetCurrent(), nil, host, 1); err != nil || setAside(t, r.GetEffective()) != names {
		t.Fatalf("set aside at refresh: %v %v", r, err)
	}
	failed, err := l.Decide(ctx, config(`{"sampling_rate":"7"}`), d.GetCurrent(), nil, host, nil)
	if err != nil || failed.GetStatus().GetStatus() != protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED ||
		setAside(t, failed.GetEffective()) != names {
		t.Fatalf("set aside after FAILED: %v %v", failed, err)
	}
	// A host naming what is no setting has a bug: an error, saying which.
	if _, err := l.Load(ctx, nil, &settingspb.Host{Live: []string{"send_metric"}}); err == nil || !strings.Contains(err.Error(), "send_metric") {
		t.Fatalf("a host declaring send_metric: %v", err)
	}
	// A log level: the lowest severity number to export, reported by its name; unset, every severity.
	d, err = l.Decide(ctx, config(`{"send_logs_level":"Warning"}`), nil, nil, declaring, nil)
	if err != nil || d.GetStatus().GetStatus() != protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED ||
		d.GetApply().SendLogsMinSeverity == nil || d.GetApply().GetSendLogsMinSeverity() != 13 ||
		effective(d.GetEffective()) != `{"send_logs_level":"warn"}` || loaded.GetSettings().SendLogsMinSeverity != nil {
		t.Fatalf("log level: %v %v", d, err)
	}
	desc, err := l.Describe(ctx, []*protobufs.KeyValue{
		{Key: "service.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "checkout"}}},
		{Key: "host.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "h"}}},
	})
	if err != nil || len(desc.GetIdentifyingAttributes()) != 1 || len(desc.GetNonIdentifyingAttributes()) != 1 {
		t.Fatalf("describe: %v %v", desc, err)
	}
}

// A host that declares every setting: each live one at once, each next-start one at the next start.
var declaring = &settingspb.Host{Live: []string{"debug_sampling_rate", "debug_send_logs_level", "debug_until",
	"drop_attributes", "mask_attributes", "sampling_rate", "sampling_rules", "sampling_traces_per_second", "send_logs",
	"send_logs_level", "send_logs_level_by_scope", "send_logs_per_second", "send_metrics", "send_traces"},
	NextStart: []string{"deactivate_all_instrumentations", "deactivate_instrumentations", "opamp_polling_interval"}}

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
	current := []byte(`{"sampling_rate":"0.5"}`)
	full := func([]byte) error { return errors.New("no space left on device") }
	d, err := l.Decide(ctx, config(`{"sampling_rate":"0.25"}`), current, current, declaring, full)
	if err != nil || d.GetStatus().GetStatus() != protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED ||
		d.GetStatus().GetErrorMessage() != "can't save the settings: no space left on device" || d.GetApply() != nil ||
		string(d.GetCurrent()) != string(current) || effective(d.GetEffective()) != `{"sampling_rate":"0.5"}` {
		t.Fatalf("a failed save: %v %v", d, err)
	}
	// Nowhere to keep state: a live change is applied, a next-start one refused.
	d, err = l.Decide(ctx, config(`{"sampling_rate":"0.25"}`), current, current, declaring, nil)
	if err != nil || d.GetStatus().GetStatus() != protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED {
		t.Fatalf("live, nowhere: %v %v", d, err)
	}
	d, err = l.Decide(ctx, config(`{"opamp_polling_interval":"1m"}`), current, current, declaring, nil)
	if err != nil || d.GetStatus().GetErrorMessage() !=
		"opamp_polling_interval take effect at the next start, which needs somewhere to keep state" {
		t.Fatalf("next start, nowhere: %v %v", d, err)
	}
	var kept [][]byte
	d, err = l.Decide(ctx, config(`{"opamp_polling_interval":"1m"}`), current, current, declaring, func(doc []byte) error {
		kept = append(kept, doc)
		return nil
	})
	if err != nil || d.GetStatus().GetStatus() != protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED ||
		!slices.Equal(d.GetPendingRestart(), []string{"opamp_polling_interval"}) || len(kept) != 1 ||
		string(kept[0]) != `{"opamp_polling_interval":"1m"}` {
		t.Fatalf("next start, saved: %v %v %q", d, err, kept)
	}
	// A refused config asks for no save.
	d, err = l.Decide(ctx, config(`{"sampling_rate":"2"}`), current, current, declaring, full)
	if err != nil || d.GetStatus().GetErrorMessage() != "sampling_rate: 2 is not between 0 and 1" {
		t.Fatalf("refused: %v %v", d, err)
	}
}

// The settings that match telemetry arrive typed where the host declares them; undeclared, ignored.
func TestTheNewSettingsArrive(t *testing.T) {
	ctx := context.Background()
	l, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	doc := `{"sampling_rules":[{"rate":0,"kind":"server","path":"/health*"}],"send_logs_level_by_scope":"com.acme.db*=debug",` +
		`"drop_attributes":"*token*","mask_attributes":"User.Email","sampling_traces_per_second":"50","send_logs_per_second":"0.5"}`
	d, err := l.DecideAt(ctx, config(doc), nil, nil, declaring, now, nil)
	if err != nil || d.GetStatus().GetStatus() != protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED {
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
	if err != nil || len(plain.GetApply().GetSamplingRules()) != 0 || len(plain.GetApply().GetUnsupported()) != 6 ||
		effective(plain.GetEffective()) != "{}" {
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
	doc := `{"sampling_rate":"0.1","debug_until":"2026-10-14T12:30:00Z","debug_send_logs_level":"info"}`
	d, err := l.DecideAt(ctx, config(doc), nil, nil, declaring, now, keep)
	end := now + 30*60_000
	if err != nil || d.GetApply().GetSamplingRate() != 1 || d.GetApply().GetSendLogsMinSeverity() != 9 || d.GetRefreshAtUnixMs() != end {
		t.Fatalf("decide: %v %v", d, err)
	}
	r, err := l.Refresh(ctx, d.GetCurrent(), nil, declaring, end)
	if err != nil || r.GetSettings().GetSamplingRate() != 0.1 || r.GetRefreshAtUnixMs() != 0 ||
		effective(r.GetEffective()) != `{"sampling_rate":"0.1"}` {
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
	doc := []byte(`{"sampling_rules":[{"rate":0,"kind":"server","path":"/health*"}],"send_logs_level_by_scope":"noisy=off",` +
		`"mask_attributes":"*token*","send_logs_per_second":"2"}`)
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
	refused, err := l.Evaluate(ctx, &settingspb.Evaluate{Settings: []byte(`{"sampling_rate":"7"}`), Host: declaring, Input: &settingspb.Evaluate_Log{
		Log: &settingspb.LogRecord{}}})
	if err != nil || !strings.HasPrefix(refused.GetRefused(), "sampling_rate:") {
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
			host := &settingspb.Host{Live: append(slices.Clone(declaring.Live), "custom"), NextStart: declaring.NextStart}
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
				if _, err := l.Describe(ctx, nil); err != nil {
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
	if _, err := l.Describe(ctx, nil); err == nil || !strings.Contains(err.Error(), "closed") {
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
