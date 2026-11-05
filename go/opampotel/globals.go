// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// providers are global providers: the ones Setup set, or the ones set now.
type providers struct {
	tracer trace.TracerProvider
	meter  metric.MeterProvider
	logger log.LoggerProvider
}

// globalProviders are the global providers set now.
func globalProviders() providers {
	return providers{tracer: otel.GetTracerProvider(), meter: otel.GetMeterProvider(), logger: global.GetLoggerProvider()}
}

// replaced says which of Setup's global providers the application has set one of its own in
// place of. Go instrumentation takes its tracer, meter or logger from the global provider, often
// at each call (otelhttp's transport takes a tracer at each request), so from then on that
// signal's telemetry goes to a provider this package has no sampler or gate in. It is reported as
// an ungoverned exporter of that signal (appProvider); the policy stays in force on everything
// Setup's providers still export.
//
// A signal stays replaced once it is, even if Setup's provider is set again: what took a tracer,
// meter or logger from the other provider meanwhile keeps it.
type replaced struct{ traces, metrics, logs bool }

// appProvider is the identity a global provider of the application's own is reported under.
const appProvider = "app_provider"

// listedExporters are what the inventory and export health list, as decide last decided them.
// Under a.mu, or before start.
func (a *agent) listedExporters() []*settingspb.Exporter {
	return a.exporters
}

// decide has the layer decide the exporters listed, as every SDK's does, from those Setup built
// and an ungoverned app_provider for each signal whose global provider the application replaced;
// each warning it gives is said once. On an error the list stays as it was. Under a.mu, or before
// start.
func (a *agent) decide(ctx context.Context) {
	found := slices.Clone(a.found)
	for _, s := range []struct {
		is     bool
		signal string
	}{{a.replaced.traces, "traces"}, {a.replaced.metrics, "metrics"}, {a.replaced.logs, "logs"}} {
		if s.is {
			found = append(found, &settingspb.FoundExporter{Signal: s.signal, Kind: appProvider})
		}
	}
	// autoexport reads the process's environment, and so its exporters send where it says.
	d, err := a.layer.Exporters(ctx, found, os.Environ(), "http/protobuf")
	if err != nil {
		slog.Warn("opamp: the exporters could not be listed", "error", err)
		return
	}
	a.exporters = d.GetExporters()
	for _, w := range d.GetWarnings() {
		if !a.warned[w] {
			if a.warned == nil {
				a.warned = map[string]bool{}
			}
			a.warned[w] = true
			slog.Warn("opamp: " + w)
		}
	}
}

// watchEvery is how often the agent looks at the global providers. The otel packages say nothing
// when a global provider is set, so looking is the only way to see it. A look is three atomic
// loads and three comparisons. Each decision also looks first, so a config is never answered for
// a provider replaced before it.
var watchEvery = time.Second

// watch looks at the global providers every interval until stop is closed.
func (a *agent) watch(stop <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			a.lookAtGlobals()
		}
	}
}

// lookAtGlobals reports, at once, a global provider the application set in place of Setup's: the
// inventory and export health, with that signal's ungoverned app_provider.
func (a *agent) lookAtGlobals() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped || !a.noticeReplaced() {
		return
	}
	a.reportReplaced()
}

// reportReplaced sends the inventory, and tells the flow the exporters now (export health is
// asked again at once): the policy in force stays as it is. Before there is a client, start sends
// the inventory. Under a.mu.
func (a *agent) reportReplaced() {
	if a.reporter == nil {
		a.relist = true // start sends it
		return
	}
	if c := a.components(context.Background()); c != nil {
		a.reporter.SetAvailableComponents(c)
	}
	a.exportersNow()
}

// noticeReplaced compares the global providers with Setup's, and says whether a signal is
// replaced that wasn't before. Under a.mu.
func (a *agent) noticeReplaced() bool {
	if a.ours == (providers{}) {
		return false // Setup hasn't set them yet
	}
	now := a.globals()
	r := a.replaced
	// An interface comparison: Setup's providers are pointers, so it never panics.
	r.traces = r.traces || now.tracer != a.ours.tracer
	r.metrics = r.metrics || now.meter != a.ours.meter
	r.logs = r.logs || now.logger != a.ours.logger
	if r == a.replaced {
		return false
	}
	a.replaced = r
	a.decide(context.Background())
	return true
}
