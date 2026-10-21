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
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/match"
	"github.com/pydantic/opamp-clients/go/opamp/sdksettings/settingspb"
)

// policy is one policy's settings, ready to judge telemetry. It never changes: a new policy is a
// new collector config, and new processors.
type policy struct {
	sendTraces, sendMetrics, sendLogs bool
	// sampler is head_sampling's ratio (nil: local, every trace kept), by the OpenTelemetry SDK's
	// own trace-ID ratio sampler.
	sampler sdktrace.Sampler
	levels  *match.Levels
	redact  *match.Redaction
}

func newPolicy(ctx context.Context, cfg *Config) (*policy, error) {
	s, err := Settings(ctx, []byte(cfg.Policy))
	if err != nil {
		return nil, err
	}
	return policyOf(s), nil
}

func policyOf(s *settingspb.Settings) *policy {
	p := &policy{
		sendTraces:  s.SendTraces == nil || *s.SendTraces,
		sendMetrics: s.SendMetrics == nil || *s.SendMetrics,
		sendLogs:    s.SendLogs == nil || *s.SendLogs,
		levels:      match.LevelsOf(s),
		redact:      match.RedactionOf(s),
	}
	if s.SamplingRate != nil {
		p.sampler = sdktrace.TraceIDRatioBased(*s.SamplingRate)
	}
	return p
}

// sampled says whether head_sampling keeps the trace: by its trace ID alone, the way an SDK's
// trace-ID ratio sampler decides a new trace at the same ratio. So an SDK and a collector under
// the same ratio keep the same traces (0.5 then 0.5 keeps half, not a quarter), and every span of
// a trace is kept or dropped together, whichever collector it goes through.
func (p *policy) sampled(id pcommon.TraceID) bool {
	if p.sampler == nil {
		return true
	}
	r := p.sampler.ShouldSample(sdktrace.SamplingParameters{ParentContext: context.Background(), TraceID: trace.TraceID(id)})
	return r.Decision == sdktrace.RecordAndSample
}

// traces carries out send_traces, head_sampling and attribute_redaction (on each span's
// attributes, its events' and its links'; not the resource's or the scope's, as in an SDK).
func (p *policy) traces(_ context.Context, td ptrace.Traces) (ptrace.Traces, error) {
	if !p.sendTraces {
		return td, processorhelper.ErrSkipProcessingData
	}
	td.ResourceSpans().RemoveIf(func(rs ptrace.ResourceSpans) bool {
		rs.ScopeSpans().RemoveIf(func(ss ptrace.ScopeSpans) bool {
			ss.Spans().RemoveIf(func(s ptrace.Span) bool {
				if !p.sampled(s.TraceID()) {
					return true
				}
				p.redactAttributes(s.Attributes())
				for i := 0; i < s.Events().Len(); i++ {
					p.redactAttributes(s.Events().At(i).Attributes())
				}
				for i := 0; i < s.Links().Len(); i++ {
					p.redactAttributes(s.Links().At(i).Attributes())
				}
				return false
			})
			return ss.Spans().Len() == 0
		})
		return rs.ScopeSpans().Len() == 0
	})
	if td.ResourceSpans().Len() == 0 {
		return td, processorhelper.ErrSkipProcessingData
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
