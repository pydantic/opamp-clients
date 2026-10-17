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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	lognoop "go.opentelemetry.io/otel/log/noop"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// setAside is what the agent's effective config names as set aside, sorted.
func setAside(t *testing.T, a *agent) []string {
	t.Helper()
	body := setAsideBody(a)
	if body == "null" {
		return nil
	}
	var e struct{ Unsupported []string }
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("elastic.set_aside %s: %v", body, err)
	}
	slices.Sort(e.Unsupported)
	return e.Unsupported
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

// Every setting this package carries out on a signal, in one document, beside a custom value.
const everySignal = `{"sampling_rate":"0","sampling_rules":[{"rate":1,"name":"x"}],"sampling_traces_per_second":"5",
"send_traces":"false","send_metrics":"false","send_logs":"false","send_logs_level":"warn",
"send_logs_level_by_scope":"noisy=off","send_logs_per_second":"5","drop_attributes":"secret",
"mask_attributes":"token","deactivate_all_instrumentations":"true","custom":{"flag":"on"}}`

// A global provider the application sets in place of one of Setup's takes that signal's telemetry
// past this package's sampler and gates: the settings that signal's telemetry needs are set aside
// at once, put out of force, and reported so; the others stay in force. From then on a config
// that sends them has them set aside too, and is still APPLIED.
func TestAGlobalProviderSetInPlaceOfSetupsSetsItsSettingsAside(t *testing.T) {
	both := []string{"deactivate_all_instrumentations", "drop_attributes", "mask_attributes"}
	for _, tc := range []struct {
		signal string
		swap   func(*providers)
		aside  []string
	}{
		{"traces", func(p *providers) { p.tracer = tracenoop.NewTracerProvider() }, append([]string{
			"sampling_rate", "sampling_rules", "sampling_traces_per_second", "send_traces"}, both...)},
		{"metrics", func(p *providers) { p.meter = metricnoop.NewMeterProvider() }, []string{"send_metrics"}},
		{"logs", func(p *providers) { p.logger = lognoop.NewLoggerProvider() }, []string{"drop_attributes",
			"mask_attributes", "send_logs", "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second"}},
	} {
		t.Run(tc.signal, func(t *testing.T) {
			slices.Sort(tc.aside)
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
			a.lookAtGlobals() // nothing replaced: nothing changes
			if got := setAside(t, a); got != nil || len(r.effective) != 0 {
				t.Fatalf("Setup's providers still global: set aside %v, %d reports", got, len(r.effective))
			}

			g.set(tc.swap)
			a.lookAtGlobals()
			if got := setAside(t, a); !slices.Equal(got, tc.aside) {
				t.Fatalf("set aside %v, want %v", got, tc.aside)
			}
			if len(r.effective) != 1 || len(r.components) != 1 {
				t.Fatalf("reported %d effective configs and %d inventories, want one each", len(r.effective), len(r.components))
			}
			effective := effectiveBody(a)
			for _, k := range tc.aside {
				if strings.Contains(effective, `"`+k+`"`) {
					t.Fatalf("%s reported in force: %s", k, effective)
				}
			}
			if !strings.Contains(effective, `"custom"`) || c.app.get("flag", "") != "on" {
				t.Fatalf("the settings still carried out stay in force: %s", effective)
			}
			if inForce := sentSignals(c); inForce[tc.signal] != true {
				t.Fatalf("%s still stopped by a setting set aside", tc.signal)
			}
			for _, k := range []string{"traces", "metrics", "logs"} {
				if k != tc.signal && sentSignals(c)[k] {
					t.Fatalf("send_%s, still carried out, is no longer in force", k)
				}
			}
			// The switches stop spans: set aside with the tracer provider alone.
			if c.Off("otelhttp") != (tc.signal != "traces") {
				t.Fatalf("deactivate_all_instrumentations in force: %v", c.Off("otelhttp"))
			}

			// Setup's provider set again: what took the other one's meanwhile keeps it.
			g.set(func(p *providers) { *p = ours })
			a.lookAtGlobals()
			if st := a.remoteConfig(context.Background(), remote(everySignal)); st.GetStatus() != applied {
				t.Fatalf("status %v", st)
			}
			if got := setAside(t, a); !slices.Equal(got, tc.aside) || len(r.effective) != 1 {
				t.Fatalf("after Setup's provider is set again: set aside %v, %d reports", got, len(r.effective))
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
		if st := a.remoteConfig(context.Background(), remote(`{"send_traces":"false","send_logs":"false"}`)); st.GetStatus() != applied {
			t.Fatalf("status %v", st)
		}
		tp := sdktrace.NewTracerProvider()
		defer tp.Shutdown(context.Background())
		otel.SetTracerProvider(tp)
		lookedAt := func() []string { a.mu.Lock(); defer a.mu.Unlock(); return setAside(t, a) }
		eventually(t, "the tracer provider set in Setup's place not seen", func() bool { return slices.Contains(lookedAt(), "send_traces") })
		time.Sleep(50 * time.Millisecond) // more looks: nothing more to say
		a.mu.Lock()
		defer a.mu.Unlock()
		if slices.Contains(setAside(t, a), "send_logs") || a.controls.gates.Load().logs || !a.controls.gates.Load().traces {
			t.Fatalf("only the tracer provider was replaced: set aside %v", setAside(t, a))
		}
		var warned []string
		for _, s := range *said {
			if strings.Contains(s, "global providers of its own") {
				warned = append(warned, s)
			}
		}
		if len(warned) != 1 || !strings.HasPrefix(warned[0], "WARN") || !strings.Contains(warned[0], "(tracer)") ||
			!strings.Contains(warned[0], "send_traces") || strings.Contains(warned[0], "send_logs") {
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
		st := a.remoteConfig(context.Background(), remote(`{"send_metrics":"false","send_logs":"false","send_traces":"false"}`))
		if st.GetStatus() != applied {
			t.Fatalf("status %v", st)
		}
		got, g := setAside(t, a), a.controls.gates.Load()
		want := []string{"send_logs", "send_metrics"}
		if !slices.Contains(got, "send_logs") || !slices.Contains(got, "send_metrics") || slices.Contains(got, "send_traces") ||
			!g.logs || !g.metrics || g.traces {
			t.Fatalf("set aside %v (want %v among them, not send_traces); sending logs %v, metrics %v, traces %v",
				got, want, g.logs, g.metrics, g.traces)
		}
	})
}

// The host each set of replaced providers leaves: what the layer accepts, only settings
// this package declared, and nothing taken that no replaced signal needs.
func TestTheHostLessWhatReplacedProvidersTake(t *testing.T) {
	full := goHost()
	for _, r := range []replaced{{}, {traces: true}, {metrics: true}, {logs: true}, {true, true, true}} {
		h := r.without(full)
		if _, err := testLayer(t).LoadAt(context.Background(), nil, h, vectorNow); err != nil {
			t.Fatalf("%+v: %v", r, err)
		}
		for _, k := range full.GetLive() {
			if slices.Contains(h.GetLive(), k) == slices.Contains(r.lost(), k) {
				t.Fatalf("%+v: %s declared %v", r, k, slices.Contains(h.GetLive(), k))
			}
		}
		if !slices.Equal(h.GetNextStart(), full.GetNextStart()) || !slices.Contains(h.GetLive(), "custom") {
			t.Fatalf("%+v: %v", r, h)
		}
	}
	if !slices.Equal((replaced{}).without(full).GetLive(), full.GetLive()) {
		t.Fatal("nothing replaced: the host as it was")
	}
}
