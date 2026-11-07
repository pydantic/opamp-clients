// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

type healthWant struct {
	Send       *bool   `json:"send"`
	SentUnixMs *uint64 `json:"sent_unix_ms"`
	entryWant
	Signals map[string]struct {
		entryWant
		Counters  map[string]int64  `json:"counters"`
		Exporters map[string]string `json:"exporters"`
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
				Now       uint64                 `json:"now_unix_ms"`
				Settings  json.RawMessage        `json:"settings"`
				Exporters []*settingspb.Exporter `json:"exporters"`
				Counts    json.RawMessage        `json:"counts"`
				Want      healthWant             `json:"want"`
			} `json:"steps"`
		}
	}
	decodeStrict(t, readVectors[json.RawMessage](t, "health.json"), &v)
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			// A wall clock set back is the layer's case: the agent's flow never steps back in time.
			for i := 1; i < len(c.Steps); i++ {
				if c.Steps[i].Now < c.Steps[i-1].Now {
					t.Skip("the wall clock goes back")
				}
			}
			ft := newFakeTime(c.Steps[0].Now)
			a, rep := timedAgent(t, t.TempDir(), ft)
			for i, s := range c.Steps {
				doc := s.Settings
				if doc == nil {
					doc = json.RawMessage(v1()) // left out: every control local
				}
				counts := &settingspb.ExportCounts{}
				if s.Counts != nil {
					if err := protojson.Unmarshal(s.Counts, counts); err != nil {
						t.Fatal(err)
					}
				}
				// The clock first, no timer run; the settings in force sent, then the exporters and
				// their counts; then the timers due and the exporters told: health is built at once.
				ft.mu.Lock()
				ft.at = time.UnixMilli(int64(s.Now))
				ft.mu.Unlock()
				a.remoteConfig(context.Background(), remote(string(doc)))
				a.mu.Lock()
				a.exporters = s.Exporters
				setCounts(a.controls, counts)
				sent := len(rep.health)
				a.mu.Unlock()
				ft.set(s.Now)
				a.mu.Lock()
				a.built("")
				report := flowHealth(t, a)
				a.mu.Unlock()
				w := s.Want
				if w.Send != nil && (len(rep.health) > sent) != *w.Send {
					t.Fatalf("step %d: sent %v, want %v", i, len(rep.health) > sent, *w.Send)
				}
				if next := ft.next(); next != s.Now+30_000 {
					t.Fatalf("step %d: the next report at %d, want 30 s on", i, next)
				}
				if w.SentUnixMs != nil && report.GetSentUnixMs() != *w.SentUnixMs {
					t.Fatalf("step %d: sent at %d, want %d", i, report.GetSentUnixMs(), *w.SentUnixMs)
				}
				exporters := report.GetExporters()
				if len(rep.health) > sent {
					got := rep.health[len(rep.health)-1]
					if !got.GetHealthy() || !proto.Equal(got.GetComponentHealthMap()["exporters"], exporters) {
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
					if sw.Exporters != nil {
						got := map[string]string{}
						for k, c := range sig.GetComponentHealthMap() {
							got[k] = c.GetStatus()
							if !c.GetHealthy() {
								t.Fatalf("step %d: %s %s unhealthy: ungoverned is no failure", i, name, k)
							}
						}
						if !reflect.DeepEqual(got, sw.Exporters) {
							t.Fatalf("step %d: %s exporters %v, want %v", i, name, got, sw.Exporters)
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

// flowState is the agent's flow as it stands. Under a.mu.
func flowState(t testing.TB, a *agent) *settingspb.AgentState {
	t.Helper()
	var st settingspb.AgentState
	if err := proto.Unmarshal(a.flow, &st); err != nil {
		t.Fatal(err)
	}
	return &st
}

// flowHealth is the export health the agent's flow last built. Under a.mu.
func flowHealth(t testing.TB, a *agent) *settingspb.HealthReport { return flowState(t, a).GetHealth() }

// flowRefreshAt is when the agent's flow asks the layer again (debug mode's end), in Unix
// milliseconds (0: never). Under a.mu.
func flowRefreshAt(t testing.TB, a *agent) uint64 { return flowState(t, a).GetRefreshAtUnixMs() }

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
	first := a.built("")
	a.mu.Unlock()
	rep.health = nil // the client's first status carries it
	if !first.GetHealthy() || first.GetComponentHealthMap()["exporters"].GetStatus() != "ok" {
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
	if len(rep.effective) != 2 { // the config's, then the end's
		t.Fatal("debug mode didn't end on the shared timer")
	}
}
