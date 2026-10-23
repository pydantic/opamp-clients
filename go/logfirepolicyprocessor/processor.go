// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package logfirepolicyprocessor

import (
	"context"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processorhelper"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/match"
	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// policy is one policy's settings, ready to judge telemetry. It never changes: a new policy is a
// new collector config, and new processors.
type policy struct {
	sendTraces, sendMetrics, sendLogs bool
	levels                            *match.Levels
	redact                            *match.Redaction
}

func newPolicy(ctx context.Context, cfg *Config) (*policy, error) {
	s, err := Settings(ctx, []byte(cfg.Policy))
	if err != nil {
		return nil, err
	}
	return policyOf(s), nil
}

func policyOf(s *settingspb.Settings) *policy {
	return &policy{
		sendTraces:  s.SendTraces == nil || *s.SendTraces,
		sendMetrics: s.SendMetrics == nil || *s.SendMetrics,
		sendLogs:    s.SendLogs == nil || *s.SendLogs,
		levels:      match.LevelsOf(s),
		redact:      match.RedactionOf(s),
	}
}

// traces carries out send_traces and attribute_redaction (on each span's attributes, its events'
// and its links'; not the resource's or the scope's, as in an SDK). It keeps every span.
func (p *policy) traces(_ context.Context, td ptrace.Traces) (ptrace.Traces, error) {
	if !p.sendTraces {
		return td, processorhelper.ErrSkipProcessingData
	}
	if p.redact.None() {
		return td, nil
	}
	for _, rs := range td.ResourceSpans().All() {
		for _, ss := range rs.ScopeSpans().All() {
			for _, s := range ss.Spans().All() {
				p.redactAttributes(s.Attributes())
				for _, e := range s.Events().All() {
					p.redactAttributes(e.Attributes())
				}
				for _, l := range s.Links().All() {
					p.redactAttributes(l.Attributes())
				}
			}
		}
	}
	return td, nil
}

// metrics carries out send_metrics. Metric attributes are never redacted, as in an SDK: masking
// would merge series.
func (p *policy) metrics(_ context.Context, md pmetric.Metrics) (pmetric.Metrics, error) {
	if !p.sendMetrics {
		return md, processorhelper.ErrSkipProcessingData
	}
	return md, nil
}

// logs carries out send_logs, log_levels (by the scope a record comes under and its
// SeverityNumber) and attribute_redaction on each record's attributes.
func (p *policy) logs(_ context.Context, ld plog.Logs) (plog.Logs, error) {
	if !p.sendLogs {
		return ld, processorhelper.ErrSkipProcessingData
	}
	ld.ResourceLogs().RemoveIf(func(rl plog.ResourceLogs) bool {
		rl.ScopeLogs().RemoveIf(func(sl plog.ScopeLogs) bool {
			scope := sl.Scope().Name()
			sl.LogRecords().RemoveIf(func(r plog.LogRecord) bool {
				if !p.levels.Keeps(scope, int(r.SeverityNumber())) {
					return true
				}
				p.redactAttributes(r.Attributes())
				return false
			})
			return sl.LogRecords().Len() == 0
		})
		return rl.ScopeLogs().Len() == 0
	})
	if ld.ResourceLogs().Len() == 0 {
		return ld, processorhelper.ErrSkipProcessingData
	}
	return ld, nil
}

// redactAttributes drops the attributes drop_attributes matches and masks those mask_attributes
// matches (their value becomes the string [REDACTED], whatever its type), in place; the rest keep
// their order. A key sent twice is redacted twice.
func (p *policy) redactAttributes(m pcommon.Map) {
	if p.redact.None() {
		return
	}
	m.RemoveIf(func(k string, v pcommon.Value) bool {
		drop, mask := p.redact.Key(k)
		if mask {
			v.SetStr(match.Mask)
		}
		return drop
	})
}
