// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/pydantic/opamp-clients/go/opamp/sdk/settingspb"
)

type healthWant struct {
	Send       *bool   `json:"send"`
	SentUnixMs *uint64 `json:"sent_unix_ms"`
	entryWant
	Signals map[string]struct {
		entryWant
		Counters map[string]int64 `json:"counters"`
	} `json:"signals"`
}

type entryWant struct {
	Healthy    *bool   `json:"healthy"`
	Status     *string `json:"status"`
	LastError  *string `json:"last_error"`
	StartTime  *uint64 `json:"start_time_unix_nano"`
	StatusTime *uint64 `json:"status_time_unix_nano"`
}

func (w entryWant) check(h *protobufs.ComponentHealth) error {
	switch {
	case h == nil:
		return errors.New("missing")
	case w.Healthy != nil && h.GetHealthy() != *w.Healthy:
		return fmt.Errorf("healthy %v, want %v", h.GetHealthy(), *w.Healthy)
	case w.Status != nil && h.GetStatus() != *w.Status:
		return fmt.Errorf("status %q, want %q", h.GetStatus(), *w.Status)
	case w.LastError != nil && h.GetLastError() != *w.LastError:
		return fmt.Errorf("last error %q, want %q", h.GetLastError(), *w.LastError)
	case w.StartTime != nil && h.GetStartTimeUnixNano() != *w.StartTime:
		return fmt.Errorf("start time %d, want %d", h.GetStartTimeUnixNano(), *w.StartTime)
	case w.StatusTime != nil && h.GetStatusTimeUnixNano() != *w.StatusTime:
		return fmt.Errorf("status time %d, want %d", h.GetStatusTimeUnixNano(), *w.StatusTime)
	}
	return nil
}

// setCounts puts counts in the gates' counters, as if the gates had counted them.
func setCounts(c *Controls, e *settingspb.ExportCounts) {
	for _, s := range []struct {
		to   *counts
		from *settingspb.SignalCounts
	}{{&c.counts.traces, e.GetTraces()}, {&c.counts.metrics, e.GetMetrics()}, {&c.counts.logs, e.GetLogs()}} {
		s.to.mu.Lock()
		s.to.c = settingspb.SignalCounts{ExportsOk: s.from.GetExportsOk(), ExportsFailed: s.from.GetExportsFailed(),
			ItemsFailed: s.from.GetItemsFailed(), ItemsFiltered: s.from.GetItemsFiltered(), LastFailed: s.from.GetLastFailed(),
			LastError: s.from.GetLastError()}
		s.to.mu.Unlock()
	}
}

// Export health call after call, through the agent's timer (health.json): the counts its gates
// keep, the settings in force, what it sends to the client and when.
func TestTheHealthVectors(t *testing.T) {
	var v struct {
		About string
		Cases []struct {
			Name  string `json:"name"`
			Steps []struct {
				Now      uint64          `json:"now_unix_ms"`
				Settings json.RawMessage `json:"settings"`
				Counts   json.RawMessage `json:"counts"`
				Want     healthWant      `json:"want"`
			} `json:"steps"`
		}
	}
	decodeStrict(t, readVectors[json.RawMessage](t, "health.json"), &v)
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ft := newFakeTime(c.Steps[0].Now)
			a, rep := timedAgent(t, t.TempDir(), ft)
			a.mu.Lock()
			a.nextHealth = ft.now().Add(a.healthEvery) // as start does
			a.arm()
			a.mu.Unlock()
			for i, s := range c.Steps {
				doc := s.Settings
				if doc == nil {
					doc = json.RawMessage(v1()) // left out: every control local
				}
				settings := settingsOf(t, doc, s.Now)
				counts := &settingspb.ExportCounts{}
				if s.Counts != nil {
					if err := protojson.Unmarshal(s.Counts, counts); err != nil {
						t.Fatal(err)
					}
				}
				ft.mu.Lock()
				ft.at = time.UnixMilli(int64(s.Now)) // the clock first (it may go back), then the timer
				ft.mu.Unlock()
				a.mu.Lock()
				a.controls.Apply(settings)
				a.settings = settings
				setCounts(a.controls, counts)
				a.nextHealth = time.UnixMilli(int64(s.Now)) // due now: the timer goes off
				a.arm()
				a.mu.Unlock()
				sent := len(rep.health)
				ft.set(s.Now)
				a.mu.Lock()
				report := a.health
				next := a.nextHealth
				a.mu.Unlock()
				w := s.Want
				if w.Send != nil && (len(rep.health) > sent) != *w.Send {
					t.Fatalf("step %d: sent %v, want %v", i, len(rep.health) > sent, *w.Send)
				}
				if !next.Equal(time.UnixMilli(int64(s.Now)).Add(30 * time.Second)) {
					t.Fatalf("step %d: the next report at %v, want 30 s on", i, next)
				}
				if w.SentUnixMs != nil && report.GetSentUnixMs() != *w.SentUnixMs {
					t.Fatalf("step %d: sent at %d, want %d", i, report.GetSentUnixMs(), *w.SentUnixMs)
				}
				exporters := report.GetExporters()
				if len(rep.health) > sent {
					got := rep.health[len(rep.health)-1]
					if !got.GetHealthy() || got.GetComponentHealthMap()["exporters"] != exporters {
						t.Fatalf("step %d: sent %v: the application healthy, export health under exporters", i, got)
					}
				}
				if err := w.entryWant.check(exporters); err != nil {
					t.Fatalf("step %d: exporters: %v", i, err)
				}
				for name, sw := range w.Signals {
					sig := exporters.GetComponentHealthMap()[name]
					if err := sw.entryWant.check(sig); err != nil {
						t.Fatalf("step %d: %s: %v", i, name, err)
					}
					for k, n := range sw.Counters {
						if got := sig.GetAttributes(); attributeInt(got, k) != n {
							t.Fatalf("step %d: %s %s = %d, want %d", i, name, k, attributeInt(got, k), n)
						}
					}
				}
			}
		})
	}
	if len(v.Cases) < 14 {
		t.Fatalf("%d cases: health.json was cut short", len(v.Cases))
	}
}

func attributeInt(kvs []*protobufs.KeyValue, key string) int64 {
	for _, kv := range kvs {
		if kv.GetKey() == key {
			return kv.GetValue().GetIntValue()
		}
	}
	return -1
}

// failingSpans is a span exporter that fails while err is set.
type failingSpans struct {
	*tracetest.InMemoryExporter
	err error
}

func (f *failingSpans) ExportSpans(ctx context.Context, s []sdktrace.ReadOnlySpan) error {
	if f.err != nil {
		return f.err
	}
	return f.InMemoryExporter.ExportSpans(ctx, s)
}

type failingMetrics struct {
	metricSink
	err error
}

func (f *failingMetrics) Export(context.Context, *metricdata.ResourceMetrics) error { return f.err }

// What each gate counts: exports that succeed and fail with the exporter's error and the items
// in them, and what it drops on purpose.
func TestTheGatesCount(t *testing.T) {
	c := NewControls()
	withClock(c)
	spans := &failingSpans{InMemoryExporter: tracetest.NewInMemoryExporter()}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(c.Spans(spans)))
	span := func() { _, s := tp.Tracer("t").Start(context.Background(), "s"); s.End() }
	span()
	spans.err = errors.New("connection refused")
	span()
	span()
	spans.err = nil
	c.Apply(settingsOf(t, []byte(v1("signal_exports", exports("off", "", ""))), 0))
	span()
	tr := c.Counts().GetTraces()
	if tr.GetExportsOk() != 1 || tr.GetExportsFailed() != 2 || tr.GetItemsFailed() != 2 || tr.GetItemsFiltered() != 1 ||
		!tr.GetLastFailed() || tr.GetLastError() != "connection refused" {
		t.Fatalf("traces: %v", tr)
	}
	c.Apply(settingsOf(t, []byte(v1()), 0))
	span()
	if tr := c.Counts().GetTraces(); tr.GetLastFailed() || tr.GetLastError() != "" || tr.GetExportsOk() != 2 {
		t.Fatalf("a success clears the failure: %v", tr)
	}

	logSink := &recordSink{}
	lp := gatedLogs(c, logSink)
	c.Apply(settingsOf(t, []byte(every("log_levels", override(`"level":"warn"`), "log_rate_limit", override(`"records_per_second":1`))), 0))
	emit(lp, "app", log.SeverityDebug) // the level
	emit(lp, "app", log.SeverityError) // the cap's one token
	emit(lp, "app", log.SeverityError) // the cap
	logSink.err = errors.New("HTTP 503")
	c.Apply(settingsOf(t, []byte(every("log_levels", override(`"level":"warn"`))), 0))
	emit(lp, "app", log.SeverityError)
	lg := c.Counts().GetLogs()
	if lg.GetExportsOk() != 1 || lg.GetExportsFailed() != 1 || lg.GetItemsFailed() != 1 || lg.GetItemsFiltered() != 2 ||
		lg.GetLastError() != "HTTP 503" {
		t.Fatalf("logs: %v", lg)
	}

	m := &failingMetrics{err: errors.New("timeout")}
	rm := &metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{
		{Metrics: []metricdata.Metrics{{Data: metricdata.Sum[int64]{DataPoints: make([]metricdata.DataPoint[int64], 3)}}}},
		{Metrics: []metricdata.Metrics{{Data: metricdata.Histogram[float64]{DataPoints: make([]metricdata.HistogramDataPoint[float64], 2)}}}},
	}}
	rm.ScopeMetrics[0].Scope.Name, rm.ScopeMetrics[1].Scope.Name = "app", otelhttp
	c.Apply(settingsOf(t, []byte(every("instrumentations", override(`"disabled":["otelhttp"]`))), 0))
	_ = c.Metrics(m).Export(context.Background(), rm)
	if mt := c.Counts().GetMetrics(); mt.GetExportsFailed() != 1 || mt.GetItemsFailed() != 3 || mt.GetItemsFiltered() != 2 {
		t.Fatalf("metrics: %v", mt)
	}
}

// The agent's health at start carries export health; a failing exporter is reported on the
// next timer, a recovery too, and the application's own health stays healthy.
func TestExportHealthIsReportedOnTheTimer(t *testing.T) {
	ft := newFakeTime(vectorNow)
	a, rep := timedAgent(t, t.TempDir(), ft)
	a.mu.Lock()
	first, send := a.healthReport(context.Background(), ft.now())
	a.nextHealth = ft.now().Add(a.healthEvery)
	a.arm()
	a.mu.Unlock()
	if !send || !first.GetHealthy() || first.GetComponentHealthMap()["exporters"].GetStatus() != "ok" {
		t.Fatalf("at start: %v", first)
	}
	spans := &failingSpans{InMemoryExporter: tracetest.NewInMemoryExporter(), err: errors.New("connection refused")}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(a.controls.Spans(spans)))
	_, s := tp.Tracer("t").Start(context.Background(), "s")
	s.End()
	ft.set(vectorNow + 29_000)
	if len(rep.health) != 0 {
		t.Fatal("reported before the timer")
	}
	ft.set(vectorNow + 30_000)
	if len(rep.health) != 1 {
		t.Fatalf("%d reports after a failure, want 1", len(rep.health))
	}
	h := rep.health[0]
	traces := h.GetComponentHealthMap()["exporters"].GetComponentHealthMap()["traces"]
	if !h.GetHealthy() || h.GetComponentHealthMap()["exporters"].GetHealthy() || traces.GetLastError() != "connection refused" {
		t.Fatalf("failing: %v", h)
	}
	ft.set(vectorNow + 60_000)
	if len(rep.health) != 1 {
		t.Fatal("nothing changed, yet sent")
	}
	spans.err = nil
	_, s = tp.Tracer("t").Start(context.Background(), "s")
	s.End()
	ft.set(vectorNow + 90_000)
	if len(rep.health) != 2 || !rep.health[1].GetComponentHealthMap()["exporters"].GetHealthy() {
		t.Fatalf("recovered: %d reports", len(rep.health))
	}
	// The debug timer and the health timer are one: a debug window's end is not held up by it.
	a.remoteConfig(context.Background(), remote(every("debug", override(`"until":"2026-10-14T12:02:00Z"`))))
	if ft.pending() != 1 {
		t.Fatalf("%d timers armed", ft.pending())
	}
	ft.set(vectorNow + 120_000)
	if a.refreshAt != 0 || len(rep.effective) != 1 {
		t.Fatal("debug mode didn't end on the shared timer")
	}
}
