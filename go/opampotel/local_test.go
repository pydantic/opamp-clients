// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"strconv"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// The sampling rate is the ratio of the sampler OTEL_TRACES_SAMPLER names: what samplerFromEnv
// builds.
func TestTheSamplingRateIsTheRatioOfTheSamplerTheVariablesName(t *testing.T) {
	for vars, want := range map[[2]string]string{
		{"", ""}:                            "1",
		{"always_on", ""}:                   "1",
		{"always_off", ""}:                  "0",
		{"parentbased_always_off", ""}:      "0",
		{"traceidratio", "0.25"}:            "0.25",
		{"parentbased_traceidratio", "0.1"}: "0.1",
		{"traceidratio", "7"}:               "1", // out of range: 1, as the sampler built
		{"jaeger_remote", ""}:               "1", // not one this package builds: the default it builds
	} {
		getenv := env(map[string]string{"OTEL_TRACES_SAMPLER": vars[0], "OTEL_TRACES_SAMPLER_ARG": vars[1]})
		if got := localSettings(getenv, nil).GetSamplingRate(); got != want {
			t.Errorf("%v: %q, want %q", vars, got, want)
		}
	}
}

func TestARatioIsDecimalTextWithoutAnExponent(t *testing.T) {
	for x, want := range map[float64]string{1: "1", 0: "0", 0.25: "0.25", 0.00001: "0.00001", 1e-7: "0.0000001",
		1.0 / 3: "0.3333333333333333"} {
		got := decimal(x)
		if got != want {
			t.Errorf("%v: %q, want %q", x, got, want)
		}
		if back, err := strconv.ParseFloat(got, 64); err != nil || back != x {
			t.Errorf("%q reads back as %v, %v", got, back, err)
		}
	}
}

// Each signal is exported where Setup built an exporter for it; the SDK has no level or scrubbing
// of its own to tell.
func TestEachSignalIsExportedWhereSetupBuiltAnExporterForIt(t *testing.T) {
	got := localSettings(env(nil), []*settingspb.FoundExporter{{Signal: "traces", Kind: "otlp"}, {Signal: "logs", Kind: "console"}})
	want := &settingspb.Local{SamplingRate: ptr("1"), SendTraces: ptr(true), SendMetrics: ptr(false), SendLogs: ptr(true)}
	if !proto.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}

// Setup puts what it built in the host before the client starts: Load, Describe, Decide, Refresh
// and Components all see it.
func TestSetupsHostCarriesWhatTheSDKItBuiltDoes(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "console") // autoexport reads the process environment
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	srv, _ := componentsServer(t)
	getenv := env(map[string]string{"OTEL_TRACES_SAMPLER": "parentbased_traceidratio", "OTEL_TRACES_SAMPLER_ARG": "0.5",
		"OTEL_TRACES_EXPORTER": "console", "OTEL_METRICS_EXPORTER": "none", "OTEL_LOGS_EXPORTER": "none"})
	sdk, err := setup(context.Background(), getenv, WithEndpoint(srv.URL+"/v1/opamp"), WithStateDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer sdk.Shutdown(context.Background())
	want := &settingspb.Local{SamplingRate: ptr("0.5"), SendTraces: ptr(true), SendMetrics: ptr(false), SendLogs: ptr(false)}
	if got := sdk.agent.host().GetLocal(); !proto.Equal(got, want) {
		t.Fatalf("local %v, want %v", got, want)
	}
}

// Each instrumentation is listed with its whole scope name, the name it is switched off by being
// its last path element.
func TestEachInstrumentationIsListedWithItsWholeScope(t *testing.T) {
	s := &seenScopes{seen: map[string]map[string]struct{}{}}
	s.seen[otelhttp] = map[string]struct{}{"0.71.0": {}}
	got, _ := s.instrumentations()
	want := &settingspb.Instrumentation{Name: "otelhttp", Version: "0.71.0", Scope: otelhttp}
	if len(got) != 1 || !proto.Equal(got[0], want) {
		t.Fatalf("%v, want %v", got, want)
	}
}
