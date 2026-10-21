// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package logfirepolicyprocessor

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"

	"github.com/pydantic/opamp-clients/go/logfirepolicyprocessor/sdkpolicy"
)

// Type is the processor's component type.
var Type = component.MustNewType(sdkpolicy.Processor)

const stability = component.StabilityLevelAlpha

// NewFactory makes the processor's factory: traces, metrics and logs.
func NewFactory() processor.Factory {
	return processor.NewFactory(Type, func() component.Config { return &Config{} },
		processor.WithTraces(createTraces, stability),
		processor.WithMetrics(createMetrics, stability),
		processor.WithLogs(createLogs, stability))
}

// It changes what goes through: it redacts in place and drops spans and records.
var mutates = processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: true})

func createTraces(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Traces) (processor.Traces, error) {
	p, err := newPolicy(ctx, cfg.(*Config))
	if err != nil {
		return nil, err
	}
	return processorhelper.NewTraces(ctx, set, cfg, next, p.traces, mutates)
}

func createMetrics(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Metrics) (processor.Metrics, error) {
	p, err := newPolicy(ctx, cfg.(*Config))
	if err != nil {
		return nil, err
	}
	return processorhelper.NewMetrics(ctx, set, cfg, next, p.metrics, mutates)
}

func createLogs(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Logs) (processor.Logs, error) {
	p, err := newPolicy(ctx, cfg.(*Config))
	if err != nil {
		return nil, err
	}
	return processorhelper.NewLogs(ctx, set, cfg, next, p.logs, mutates)
}
