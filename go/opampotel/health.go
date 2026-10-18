// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"sync"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// counts is what one signal's gate counted since start, for export health: exports that
// succeeded and failed (after the exporter's own retries), the items in failed ones, and the items
// the gate dropped on purpose (send_* false, a switched-off instrumentation, a level, a cap).
type counts struct {
	mu sync.Mutex
	c  settingspb.SignalCounts
}

// filtered counts n items the gate dropped on purpose.
func (s *counts) filtered(n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.ItemsFiltered += uint64(n)
}

// exported counts an export of n items, and how it ended.
func (s *counts) exported(n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.c.ExportsFailed++
		s.c.ItemsFailed += uint64(n)
		s.c.LastFailed, s.c.LastError = true, err.Error()
		return
	}
	s.c.ExportsOk++
	s.c.LastFailed, s.c.LastError = false, ""
}

func (s *counts) snapshot() *settingspb.SignalCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &settingspb.SignalCounts{ExportsOk: s.c.ExportsOk, ExportsFailed: s.c.ExportsFailed,
		ItemsFailed: s.c.ItemsFailed, ItemsFiltered: s.c.ItemsFiltered, LastFailed: s.c.LastFailed, LastError: s.c.LastError}
}

// Counts is what the gates counted since start, for the layer's Health call.
func (c *Controls) Counts() *settingspb.ExportCounts {
	return &settingspb.ExportCounts{Traces: c.counts.traces.snapshot(), Metrics: c.counts.metrics.snapshot(),
		Logs: c.counts.logs.snapshot()}
}

// points is the number of data points in scope metrics: a metric data point is a metric's item.
func points(sms []metricdata.ScopeMetrics) int {
	n := 0
	for _, sm := range sms {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				n += len(d.DataPoints)
			case metricdata.Gauge[float64]:
				n += len(d.DataPoints)
			case metricdata.Sum[int64]:
				n += len(d.DataPoints)
			case metricdata.Sum[float64]:
				n += len(d.DataPoints)
			case metricdata.Histogram[int64]:
				n += len(d.DataPoints)
			case metricdata.Histogram[float64]:
				n += len(d.DataPoints)
			case metricdata.ExponentialHistogram[int64]:
				n += len(d.DataPoints)
			case metricdata.ExponentialHistogram[float64]:
				n += len(d.DataPoints)
			case metricdata.Summary:
				n += len(d.DataPoints)
			}
		}
	}
	return n
}
