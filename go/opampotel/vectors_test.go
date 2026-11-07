// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings"
	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// The shared vectors (testdata/sdk-settings), run through this package's own code:
// each case's policy goes through the layer as this package's host declares it, the
// settings it hands over are applied to Controls, and the sampler or the gate decides.

const vectorNow = uint64(1_791_979_200_000) // 2026-10-14T12:00:00Z, the vectors' clock

var (
	layerOnce sync.Once
	theLayer  *sdksettings.Layer
	layerErr  error
)

// testLayer is one layer for the package's tests (the layer keeps nothing between calls).
func testLayer(t testing.TB) *sdksettings.Layer {
	t.Helper()
	layerOnce.Do(func() { theLayer, layerErr = sdksettings.New(context.Background()) })
	if layerErr != nil {
		t.Fatal(layerErr)
	}
	return theLayer
}

// goHost is the host this package declares (agent.go).
func goHost() *settingspb.Host { return (&server{}).host() }

// settingsOf is what the layer hands this package for a policy at now (0: the vectors' clock),
// as at a start with the policy saved. The test fails if the layer refuses the policy: this
// package's host carries out every control.
func settingsOf(t testing.TB, doc []byte, now uint64) *settingspb.Settings {
	t.Helper()
	if now == 0 {
		now = vectorNow
	}
	loaded := startWith(t, goHost(), doc, now)
	if loaded.warning != "" {
		t.Fatalf("the layer refused %s: %s", doc, loaded.warning)
	}
	return loaded.settings
}

// started is what the layer's agent flow starts with, from a policy the client kept applied.
type started struct {
	settings  *settingspb.Settings
	effective *protobufs.EffectiveConfig
	warning   string
}

// startWith starts the layer's agent flow for host at now, with doc kept applied (nil: none).
func startWith(t testing.TB, host *settingspb.Host, doc []byte, now uint64) started {
	t.Helper()
	var state []byte
	if doc != nil {
		state = coreState(t, string(doc))
	}
	out, err := testLayer(t).Start(context.Background(), &settingspb.AgentStart{Host: host, StateDir: true, CoreState: state,
		Endpoint: "http://127.0.0.1:1", InstanceUid: make([]byte, 16), NowUnixMs: now, NowMonoMs: now})
	if err != nil {
		t.Fatal(err)
	}
	r := started{effective: out.GetEffective()}
	for _, e := range out.GetEffects() {
		if e.GetApply() != nil {
			r.settings = e.GetApply()
		}
		if l := e.GetLog(); l.GetLevel() >= settingspb.AgentLog_WARN {
			r.warning = l.GetText()
		}
	}
	return r
}

// decideAt is the layer's answer to doc sent as a remote config to host at now, with no policy
// kept: its status, the settings it hands over (nil: none), and the effective config after.
func decideAt(t testing.TB, host *settingspb.Host, doc string, now uint64) (*protobufs.RemoteConfigStatus,
	*settingspb.Settings, *protobufs.EffectiveConfig) {
	t.Helper()
	ctx := context.Background()
	out, err := testLayer(t).Start(ctx, &settingspb.AgentStart{Host: host, StateDir: true,
		Endpoint: "http://127.0.0.1:1", InstanceUid: make([]byte, 16), NowUnixMs: now, NowMonoMs: now})
	if err == nil {
		out, err = testLayer(t).Step(ctx, &settingspb.AgentStep{Agent: out.GetAgent(), NowUnixMs: now, NowMonoMs: now,
			Input: &settingspb.AgentStep_RemoteConfig{RemoteConfig: remote(doc)}})
	}
	if err != nil {
		t.Fatal(err)
	}
	var status *protobufs.RemoteConfigStatus
	var apply *settingspb.Settings
	for _, e := range out.GetEffects() {
		if e.GetStatus() != nil {
			status = e.GetStatus()
		}
		if e.GetApply() != nil {
			apply = e.GetApply()
		}
	}
	return status, apply, out.GetEffective()
}

const policyFile = "logfire-sdk-policy.json"

// The controls of a policy in order: it must send the first two, and may leave out the others.
var policyControls = []string{"head_sampling", "signal_exports", "sampling_rules", "trace_rate_limit", "log_levels",
	"log_rate_limit", "attribute_redaction", "debug", "app_settings", "instrumentations", "scrubbing", "self_monitoring"}

// exportsLocal is a signal_exports control that leaves every export as the SDK has it.
const exportsLocal = `{"traces":{"mode":"local"},"metrics":{"mode":"local"},"logs":{"mode":"local"}}`

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

// policy is a policy of the schema with its first n controls, each local unless set gives it: set
// holds pairs of a control's name and its JSON value.
func policy(schema string, n int, set ...string) string {
	given := map[string]string{}
	for i := 0; i+1 < len(set); i += 2 {
		if !slices.Contains(policyControls[:n], set[i]) {
			panic("no control " + set[i] + " in " + schema)
		}
		given[set[i]] = set[i+1]
	}
	b := `{"schema":"` + schema + `"`
	for _, c := range policyControls[:n] {
		v, ok := given[c]
		if !ok {
			v = `{"mode":"local"}`
			if c == "signal_exports" {
				v = exportsLocal
			}
		}
		b += `,"` + c + `":` + v
	}
	return b + "}"
}

// v1 is a policy of the two controls it must send, each local unless set gives it.
func v1(set ...string) string { return policy("logfire-sdk-policy/v1", 2, set...) }

// every is a policy that sends every control, each local unless set gives it.
func every(set ...string) string { return policy("logfire-sdk-policy/v1", len(policyControls), set...) }

// override is a control's override with the given fields.
func override(fields string) string { return `{"mode":"override",` + fields + `}` }

// sorted is a policy that sends every control as the layer writes the effective config: compact,
// keys sorted.
func sorted(t testing.TB, doc string) string {
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
func full(t testing.TB, doc string) string {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range policyControls {
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

func readVectors[T any](t testing.TB, name string) T {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "sdk-settings", name)
	// OPAMP_SETTINGS_VECTORS: a directory of generated cases in place of testdata's, where it has the file.
	if other := os.Getenv("OPAMP_SETTINGS_VECTORS"); other != "" {
		if _, err := os.Stat(filepath.Join(other, name)); err == nil {
			path = filepath.Join(other, name)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

type logCase struct {
	Name     string          `json:"name"`
	Settings json.RawMessage `json:"settings"`
	Now      uint64          `json:"now_unix_ms"`
	Record   struct {
		Scope    string `json:"scope"`
		Severity int    `json:"severity"`
	} `json:"record"`
	Kept bool `json:"kept"`
}

// recordSink keeps what the gate exported.
type recordSink struct {
	mu  sync.Mutex
	got []sdklog.Record
	err error // what Export answers
}

func (s *recordSink) Export(_ context.Context, records []sdklog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range records {
		s.got = append(s.got, r.Clone())
	}
	return s.err
}

func (s *recordSink) ForceFlush(context.Context) error { return nil }
func (s *recordSink) Shutdown(context.Context) error   { return nil }

func (s *recordSink) take() []sdklog.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	got := s.got
	s.got = nil
	return got
}

// A log record's way through the gate: send_logs, send_logs_level, send_logs_level_by_scope,
// debug mode's level (logs.json).
func TestTheLogVectors(t *testing.T) {
	v := readVectors[struct{ Cases []logCase }](t, "logs.json")
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			controls := NewControls()
			controls.Apply(settingsOf(t, c.Settings, c.Now))
			sink := &recordSink{}
			lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(controls.Logs(sink))))
			var r log.Record
			r.SetSeverity(log.Severity(c.Record.Severity))
			lp.Logger(c.Record.Scope).Emit(context.Background(), r)
			if kept := len(sink.take()) == 1; kept != c.Kept {
				t.Fatalf("kept %v, want %v", kept, c.Kept)
			}
		})
	}
	if len(v.Cases) < 40 {
		t.Fatalf("%d cases: logs.json was cut short", len(v.Cases))
	}
}
