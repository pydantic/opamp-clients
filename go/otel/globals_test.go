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

// A global provider the application sets in place of one of Setup's takes that signal's telemetry
// past this package's sampler and gates: the overrides that signal's telemetry needs are set aside
// at once, put out of force, and reported local; the others stay in force. From then on a policy
// that overrides them is refused.
func TestAGlobalProviderSetInPlaceOfSetupsSetsItsSettingsAside(t *testing.T) {
	for _, tc := range []struct {
		signal string
		swap   func(*providers)
		aside  []string
	}{
		{"traces", func(p *providers) { p.tracer = tracenoop.NewTracerProvider() }, []string{"attribute_redaction",
			"head_sampling", "instrumentations", "sampling_rules", "signal_exports.traces", "trace_rate_limit"}},
		{"metrics", func(p *providers) { p.meter = metricnoop.NewMeterProvider() }, []string{"signal_exports.metrics"}},
		{"logs", func(p *providers) { p.logger = lognoop.NewLoggerProvider() }, []string{"attribute_redaction",
			"log_levels", "log_rate_limit", "signal_exports.logs"}},
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
			if got := setAside(t, a, everySignal); got != nil || len(r.effective) != 0 {
				t.Fatalf("Setup's providers still global: set aside %v, %d reports", got, len(r.effective))
			}

			g.set(tc.swap)
			a.lookAtGlobals()
			if got := setAside(t, a, everySignal); !slices.Equal(got, tc.aside) {
				t.Fatalf("set aside %v, want %v", got, tc.aside)
			}
			if len(r.effective) != 1 || len(r.components) != 1 {
				t.Fatalf("reported %d effective configs and %d inventories, want one each", len(r.effective), len(r.components))
			}
			effective := effectiveBody(a)
			if !strings.Contains(effective, `"app_settings":{"mode":"override"`) || c.app.get("flag", "") != "on" {
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
				t.Fatalf("instrumentations.all_disabled in force: %v", c.Off("otelhttp"))
			}

			// Setup's provider set again: what took the other one's meanwhile keeps it, so the same
			// policy offered again is refused, naming a control it set aside.
			g.set(func(p *providers) { *p = ours })
			a.lookAtGlobals()
			again := remote(everySignal)
			again.ConfigHash = []byte("again")
			st := a.remoteConfig(context.Background(), again)
			if st.GetStatus() != failed_ || !strings.HasSuffix(st.GetErrorMessage(), "this SDK cannot apply the requested override") ||
				!slices.ContainsFunc(tc.aside, func(c string) bool { return strings.HasPrefix(st.GetErrorMessage(), c+":") }) {
				t.Fatalf("status %v", st)
			}
			if got := setAside(t, a, everySignal); !slices.Equal(got, tc.aside) || len(r.effective) != 1 {
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
		off := every("signal_exports", exports("off", "", "off"))
		if st := a.remoteConfig(context.Background(), remote(off)); st.GetStatus() != applied {
			t.Fatalf("status %v", st)
		}
		tp := sdktrace.NewTracerProvider()
		defer tp.Shutdown(context.Background())
		otel.SetTracerProvider(tp)
		lookedAt := func() []string { a.mu.Lock(); defer a.mu.Unlock(); return setAside(t, a, off) }
		eventually(t, "the tracer provider set in Setup's place not seen", func() bool {
			return slices.Contains(lookedAt(), "signal_exports.traces")
		})
		time.Sleep(50 * time.Millisecond) // more looks: nothing more to say
		a.mu.Lock()
		defer a.mu.Unlock()
		if slices.Contains(setAside(t, a, off), "signal_exports.logs") || a.controls.gates.Load().logs || !a.controls.gates.Load().traces {
			t.Fatalf("only the tracer provider was replaced: set aside %v", setAside(t, a, off))
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
		// Seen before the decision: a policy that overrides what they take is refused, and
		// nothing changes.
		st := a.remoteConfig(context.Background(), remote(every("signal_exports", exports("off", "off", "off"))))
		g := a.controls.gates.Load()
		if st.GetStatus() != failed_ || st.GetErrorMessage() != "signal_exports.metrics: this SDK cannot apply the requested override" ||
			!g.logs || !g.metrics || !g.traces {
			t.Fatalf("status %v; sending logs %v, metrics %v, traces %v", st, g.logs, g.metrics, g.traces)
		}
		// One that overrides only what it still carries out is applied.
		if st := a.remoteConfig(context.Background(), remote(every("signal_exports", exports("off", "", "")))); st.GetStatus() != applied {
			t.Fatalf("status %v", st)
		}
		if g := a.controls.gates.Load(); g.traces || !g.metrics || !g.logs {
			t.Fatalf("sending logs %v, metrics %v, traces %v", g.logs, g.metrics, g.traces)
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
