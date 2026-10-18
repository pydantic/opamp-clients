// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"log/slog"
	"slices"
	"strings"
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
// signal's telemetry goes to a provider this package has no sampler or gate in.
//
// A signal stays replaced once it is, even if Setup's provider is set again: what took a tracer,
// meter or logger from the other provider meanwhile keeps it.
type replaced struct{ traces, metrics, logs bool }

// The settings each replaced provider takes from this package. Redaction goes with the span or
// the log provider: it must hold for every span and log record, or none is promised. The
// instrumentation switches go with the tracer provider, whose spans they no longer stop. With
// only the meter or the logger provider replaced they still stop spans, and miss that provider's
// metrics or log records, as they miss a Prometheus reader's. The layer sets the debug rate and
// level aside with the rate and the level.
var (
	tracesSettings = []string{"sampling_rate", "sampling_rules", "sampling_traces_per_second", "send_traces",
		"debug_sampling_rate"}
	metricsSettings = []string{"send_metrics"}
	logsSettings    = []string{"send_logs", "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second",
		"debug_send_logs_level"}
	redactionSettings = []string{"drop_attributes", "mask_attributes"}
	switchSettings    = []string{"deactivate_instrumentations", "deactivate_all_instrumentations"}
)

// lost are the settings r takes from this package.
func (r replaced) lost() []string {
	var out []string
	if r.traces {
		out = append(out, tracesSettings...)
	}
	if r.metrics {
		out = append(out, metricsSettings...)
	}
	if r.logs {
		out = append(out, logsSettings...)
	}
	if r.traces || r.logs {
		out = append(out, redactionSettings...)
	}
	if r.traces {
		out = append(out, switchSettings...)
	}
	return out
}

// without is h with the settings r takes out of live.
func (r replaced) without(h *settingspb.Host) *settingspb.Host {
	lost := r.lost()
	if len(lost) == 0 {
		return h
	}
	return &settingspb.Host{
		Live:      slices.DeleteFunc(slices.Clone(h.GetLive()), func(k string) bool { return slices.Contains(lost, k) }),
		NextStart: h.GetNextStart(),
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

// lookAtGlobals sets aside what a global provider the application set in place of Setup's takes
// from this package, at once: the settings in force decided again through the layer, put in
// force, and the effective config and the inventory reported.
func (a *agent) lookAtGlobals() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped || !a.noticeReplaced() {
		return
	}
	a.narrowed(a.now())
}

// narrowed puts in force and reports what the layer decides under a host that has just narrowed:
// the settings it no longer declares set aside, the rest as they were. The inventory, which lists
// the settings carried out, is sent again too. Under a.mu.
func (a *agent) narrowed(now time.Time) {
	a.decideAgain(now, unixMs(now))
	a.arm()
	if a.reporter == nil {
		a.relist = true // start sends it
		return
	}
	if c := a.components(context.Background()); c != nil {
		a.reporter.SetAvailableComponents(c)
	}
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
	var which []string
	for _, s := range []struct {
		was, is bool
		name    string
	}{{a.replaced.traces, r.traces, "tracer"}, {a.replaced.metrics, r.metrics, "meter"}, {a.replaced.logs, r.logs, "logger"}} {
		if s.is && !s.was {
			which = append(which, s.name)
		}
	}
	before := a.replaced.lost()
	a.replaced = r
	var lost []string
	for _, k := range r.lost() {
		if !slices.Contains(before, k) && slices.Contains(a.server.host().GetLive(), k) {
			lost = append(lost, k)
		}
	}
	slog.Warn("opamp: the application set global providers of its own in place of Setup's (" +
		strings.Join(which, ", ") + "); this package can't carry out " + strings.Join(lost, ", ") +
		" for the telemetry they receive, so they are set aside from now on")
	return true
}
