// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	lognoop "go.opentelemetry.io/otel/log/noop"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// setAside is what of the policy `wanted` overrides the agent's effective config reports local,
// sorted: a control whose override is gone ("signal_exports.traces"), or a field of one whose
// override stays ("log_levels: scopes"). A policy in force reports what it set aside local.
func setAside(t *testing.T, a *agent, wanted string) []string {
	t.Helper()
	controls := func(doc string) map[string]map[string]any {
		var raw map[string]any
		if err := json.Unmarshal([]byte(doc), &raw); err != nil {
			t.Fatalf("policy %s: %v", doc, err)
		}
		out := map[string]map[string]any{}
		for k, v := range raw {
			if k == "signal_exports" {
				for signal, o := range v.(map[string]any) {
					out[k+"."+signal] = o.(map[string]any)
				}
			} else if o, ok := v.(map[string]any); ok {
				out[k] = o
			}
		}
		return out
	}
	have := controls(effectiveBody(a))
	var aside []string
	for control, o := range controls(wanted) {
		if o["mode"] != "override" {
			continue
		}
		if have[control]["mode"] != "override" {
			aside = append(aside, control)
			continue
		}
		for field := range o {
			if _, ok := have[control][field]; !ok {
				aside = append(aside, control+": "+field)
			}
		}
	}
	slices.Sort(aside)
	return aside
}

// fakeGlobals are global providers a test sets.
type fakeGlobals struct {
	mu  sync.Mutex
	now providers
}

func (g *fakeGlobals) get() providers { g.mu.Lock(); defer g.mu.Unlock(); return g.now }
func (g *fakeGlobals) set(f func(*providers)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f(&g.now)
}

// Every control this package carries out on a signal, in one policy, beside the application's own
// settings.
var everySignal = every("head_sampling", override(`"ratio":0`),
	"signal_exports", `{"traces":{"mode":"override","enabled":false},"metrics":{"mode":"override","enabled":false},`+
		`"logs":{"mode":"override","enabled":false}}`,
	"sampling_rules", override(`"rules":[{"rate":1,"name":"x"}]`),
	"trace_rate_limit", override(`"traces_per_second":5`),
	"log_levels", override(`"level":"warn","scopes":{"noisy":"off"}`),
	"log_rate_limit", override(`"records_per_second":5`),
	"attribute_redaction", override(`"drop":["secret"],"mask":["token"]`),
	"instrumentations", override(`"all_disabled":true`),
	"app_settings", override(`"values":{"flag":"on"}`))

// listedIn is each signal's exporters in components, as identity to gated.
func listedIn(t *testing.T, c *protobufs.AvailableComponents) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for signal, d := range c.GetComponents()["exporters"].GetSubComponentMap() {
		out[signal] = map[string]bool{}
		for name, e := range d.GetSubComponentMap() {
			out[signal][name] = e.GetMetadata()[0].GetValue().GetBoolValue()
		}
	}
	return out
}

// A global provider the application sets in place of one of Setup's takes that signal's telemetry
// past this package's sampler and gates: reported at once as an ungoverned exporter of that signal
// (in the inventory and export health), while every override stays in force on what Setup's
// providers export and later policies are APPLIED. Nothing is set aside.
func TestAGlobalProviderSetInPlaceOfSetupsIsReportedUngoverned(t *testing.T) {
	for _, tc := range []struct {
		signal string
		swap   func(*providers)
	}{
		{"traces", func(p *providers) { p.tracer = tracenoop.NewTracerProvider() }},
		{"metrics", func(p *providers) { p.meter = metricnoop.NewMeterProvider() }},
		{"logs", func(p *providers) { p.logger = lognoop.NewLoggerProvider() }},
	} {
		t.Run(tc.signal, func(t *testing.T) {
			a, r := startedAgent(t, &server{endpoint: "http://127.0.0.1:1"})
			c := a.controls
			ours := providers{tracer: c.TracerProvider(tracenoop.NewTracerProvider()),
				meter: c.MeterProvider(metricnoop.NewMeterProvider()), logger: c.LoggerProvider(lognoop.NewLoggerProvider())}
			g := &fakeGlobals{now: ours}
			a.mu.Lock()
			a.ours, a.globals = ours, g.get
			a.mu.Unlock()
			if st := a.remoteConfig(context.Background(), remote(everySignal)); st.GetStatus() != applied {
				t.Fatalf("status %v", st)
			}
			reported := len(r.effective) // the config's own
			a.lookAtGlobals()            // nothing replaced: nothing reported
			if len(r.components) != 0 || len(r.health) != 0 {
				t.Fatalf("Setup's providers still global: %d inventories, %d health reports", len(r.components), len(r.health))
			}

			g.set(tc.swap)
			a.lookAtGlobals()
			if len(r.components) != 1 || len(r.health) != 1 {
				t.Fatalf("reported %d inventories and %d health reports, want one each", len(r.components), len(r.health))
			}
			if listed := listedIn(t, r.components[0]); listed[tc.signal][appProvider] || len(listed) != 1 {
				t.Fatalf("listed %v: want an ungoverned %s for %s alone", listed, appProvider, tc.signal)
			}
			child := r.health[0].GetComponentHealthMap()["exporters"].GetComponentHealthMap()[tc.signal].GetComponentHealthMap()[appProvider]
			if child.GetStatus() != "ungoverned" || !child.GetHealthy() {
				t.Fatalf("export health %v: ungoverned, and healthy all the same", child)
			}
			// Nothing set aside: every override stays in force, and nothing more is reported of it.
			if got := setAside(t, a, everySignal); got != nil || len(r.effective) != reported {
				t.Fatalf("set aside %v, %d effective configs reported", got, len(r.effective))
			}
			for k, sending := range sentSignals(c) {
				if sending {
					t.Fatalf("send_%s is no longer in force", k)
				}
			}
			if c.app.get("flag", "") != "on" || !c.Off("otelhttp") {
				t.Fatalf("the application's settings and the switches stay in force")
			}

			// Setup's provider set again: what took the other one's meanwhile keeps it, so it is
			// still reported; the same policy offered again is APPLIED.
			g.set(func(p *providers) { *p = ours })
			a.lookAtGlobals()
			again := remote(everySignal)
			again.ConfigHash = []byte("again")
			if st := a.remoteConfig(context.Background(), again); st.GetStatus() != applied {
				t.Fatalf("status %v", st)
			}
			a.mu.Lock()
			listed := a.listedExporters()
			a.mu.Unlock()
			if len(listed) != 1 || listed[0].GetName() != appProvider || listed[0].GetSignal() != tc.signal {
				t.Fatalf("after Setup's provider is set again: %v", listed)
			}
		})
	}
}

// sentSignals says which signals' gates let their telemetry through.
func sentSignals(c *Controls) map[string]bool {
	g := c.gates.Load()
	return map[string]bool{"traces": g.traces, "metrics": g.metrics, "logs": g.logs}
}

// globalsKept puts the global providers back as they were when the test ends.
func globalsKept(t *testing.T) {
	t.Helper()
	before := globalProviders()
	t.Cleanup(func() {
		otel.SetTracerProvider(before.tracer)
		otel.SetMeterProvider(before.meter)
		global.SetLoggerProvider(before.logger)
	})
}

// Through Setup and the otel packages' own globals: the agent sees a provider set in its place
// on its own, without a config, and says so once; and a decision sees one set since the last look
// before it answers.
func TestSetupSeesTheApplicationSetAGlobalProviderOfItsOwn(t *testing.T) {
	globalsKept(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	vars := env(map[string]string{"OTEL_OPAMP_ENDPOINT": "http://127.0.0.1:1/v1/opamp", "OTEL_METRICS_EXPORTER": "none",
		"OTEL_OPAMP_STATE_DIR": "none"})

	t.Run("on its own", func(t *testing.T) {
		said := records(t)
		was := watchEvery
		watchEvery = 10 * time.Millisecond
		t.Cleanup(func() { watchEvery = was })
		sdk, err := setup(context.Background(), vars)
		if err != nil {
			t.Fatal(err)
		}
		defer sdk.Shutdown(context.Background())
		a := sdk.agent
		off := every("signal_exports", exports("off", "", "off"))
		if st := a.remoteConfig(context.Background(), remote(off)); st.GetStatus() != applied {
			t.Fatalf("status %v", st)
		}
		tp := sdktrace.NewTracerProvider()
		defer tp.Shutdown(context.Background())
		otel.SetTracerProvider(tp)
		listed := func() []*settingspb.Exporter { a.mu.Lock(); defer a.mu.Unlock(); return a.listedExporters() }
		eventually(t, "the tracer provider set in Setup's place not seen", func() bool {
			return slices.ContainsFunc(listed(), func(e *settingspb.Exporter) bool {
				return e.GetSignal() == "traces" && e.GetName() == appProvider && !e.GetGated()
			})
		})
		time.Sleep(50 * time.Millisecond) // more looks: nothing more to say
		a.mu.Lock()
		defer a.mu.Unlock()
		if len(a.listedExporters()) != 1 || a.controls.gates.Load().logs || a.controls.gates.Load().traces {
			t.Fatalf("only the tracer provider was replaced, and the policy stays in force: %v", a.listedExporters())
		}
		var warned []string
		for _, s := range *said {
			if strings.Contains(s, "provider of its own") {
				warned = append(warned, s)
			}
		}
		// The layer's words, as every SDK says them, once.
		if len(warned) != 1 || !strings.HasPrefix(warned[0], "WARN") ||
			!strings.Contains(warned[0], "the application set a traces provider of its own") ||
			!strings.Contains(warned[0], "ungoverned") {
			t.Fatalf("warned %q", warned)
		}
	})

	t.Run("at a decision", func(t *testing.T) {
		was := watchEvery
		watchEvery = time.Hour
		t.Cleanup(func() { watchEvery = was })
		sdk, err := setup(context.Background(), vars)
		if err != nil {
			t.Fatal(err)
		}
		defer sdk.Shutdown(context.Background())
		mp, lp := sdkmetric.NewMeterProvider(), sdklog.NewLoggerProvider()
		defer mp.Shutdown(context.Background())
		defer lp.Shutdown(context.Background())
		otel.SetMeterProvider(mp)
		global.SetLoggerProvider(lp)
		a := sdk.agent
		// Seen before the decision, and reported; the policy is APPLIED all the same.
		st := a.remoteConfig(context.Background(), remote(every("signal_exports", exports("off", "off", "off"))))
		if g := a.controls.gates.Load(); st.GetStatus() != applied || g.logs || g.metrics || g.traces {
			t.Fatalf("status %v; sending logs %v, metrics %v, traces %v", st, g.logs, g.metrics, g.traces)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		var names []string
		for _, e := range a.listedExporters() {
			names = append(names, e.GetSignal()+"/"+e.GetName())
		}
		if !slices.Equal(names, []string{"metrics/" + appProvider, "logs/" + appProvider}) {
			t.Fatalf("listed %v", names)
		}
	})
}
