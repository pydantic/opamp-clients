// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// Package opampotel sets up the OpenTelemetry SDK for a Go application under an OpAMP server
// (docs/go-otel.md).
//
// Setup builds the SDK the OTEL_* variables describe, as autoexport does, sets it as the global
// providers, and puts it under the server OTEL_OPAMP_ENDPOINT (or WithEndpoint) names: a sampler
// and exporters the server's settings change while the application runs, instrumentations it can
// switch off, one instance UID. Go instrumentation takes its tracers from the global provider, so
// every setting takes effect at once, the instrumentation switches too.
//
// What the settings mean, what to answer and what to report is the SDK settings layer's
// (github.com/pydantic/opamp-clients/go/opamp/sdk), the same in every language; this package
// carries it out on the Go SDK.
package opampotel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/contrib/propagators/autoprop"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/pydantic/opamp-clients/go/opamp/sdk/settingspb"
)

// SDK is the OpenTelemetry SDK Setup built, set as the global providers.
type SDK struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	LoggerProvider log.LoggerProvider
	Resource       *resource.Resource
	// Controls is what the server's settings change; nil when the package is off.
	Controls *Controls

	agent    *agent
	shutdown []func(context.Context) error
	// exporters are those it built, for the inventory.
	exporters []*settingspb.Exporter
}

// Setup builds the SDK and, when OTEL_OPAMP_ENDPOINT or WithEndpoint names a server, joins the
// fleet. Call Shutdown before the application exits: the server hears it go, then the SDK flushes.
// An option given wins over its OTEL_OPAMP_* variable; the rest still come from the variables.
//
// Without an endpoint it builds the same SDK, unmanaged. If the server's settings can't
// be loaded or the client can't start, it logs why and the SDK runs unmanaged: telemetry never
// waits on the server.
func Setup(ctx context.Context, opts ...Option) (*SDK, error) {
	return setup(ctx, os.Getenv, opts...)
}

func setup(ctx context.Context, getenv func(string) string, opts ...Option) (_ *SDK, err error) {
	s := &SDK{}
	srv := serverFromEnv(getenv, opts...)
	before := theAppSettings.snapshot() // a failed Setup leaves what AppSetting reads as it was
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Shutdown(ctx))
			theAppSettings.put(before)
			if srv != nil {
				release(srv.stateLock) // a failed Setup holds no slot
			}
		}
	}()
	if srv != nil {
		srv.metricsUngated = !metricsGated(getenv)
		controls := NewControls()
		controls.app = theAppSettings // what AppSetting reads
		if s.agent, err = newAgent(ctx, srv, controls, getenv("OTEL_RESOURCE_ATTRIBUTES")); err != nil {
			slog.Error("opamp: the SDK settings layer did not load; the SDK runs unmanaged", "error", err)
			s.agent, err = nil, nil
			release(srv.stateLock)
			srv.stateLock = nil
		} else {
			s.Controls = controls
			// The client stopped, the default state directory's slot is free again: a Setup after
			// this Shutdown, in the same process, is the same instance.
			s.shutdown = append(s.shutdown, s.agent.stop, func(context.Context) error {
				release(srv.stateLock)
				srv.stateLock = nil
				return nil
			})
		}
	}

	var attrs []attribute.KeyValue
	if s.agent != nil {
		attrs = append(attrs, attribute.String(instanceID, s.agent.uid.String()))
	}
	own, err := resource.New(ctx, resource.WithHost(), resource.WithAttributes(attrs...))
	if err != nil && own == nil {
		return nil, err
	}
	// Default reads OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES; the instance UID wins.
	if s.Resource, err = resource.Merge(resource.Default(), own); err != nil {
		return nil, err
	}
	if err := s.traces(ctx, getenv); err != nil {
		return nil, err
	}
	if err := s.metrics(ctx, getenv); err != nil {
		return nil, err
	}
	if err := s.logs(ctx); err != nil {
		return nil, err
	}
	// The globals only once all of it is built: a failed Setup leaves them as they were.
	otel.SetTextMapPropagator(autoprop.NewTextMapPropagator())
	otel.SetTracerProvider(s.TracerProvider)
	otel.SetMeterProvider(s.MeterProvider)
	global.SetLoggerProvider(s.LoggerProvider)
	if s.agent != nil {
		s.agent.exporters = s.exporters
		s.agent.ours = providers{tracer: s.TracerProvider, meter: s.MeterProvider, logger: s.LoggerProvider}
		s.agent.start(ctx, s.Resource)
	}
	return s, nil
}

// Shutdown says goodbye to the server, then flushes and stops the SDK.
func (s *SDK) Shutdown(ctx context.Context) error {
	var errs []error
	for _, f := range s.shutdown {
		errs = append(errs, f(ctx))
	}
	s.shutdown = nil
	return errors.Join(errs...)
}

func (s *SDK) traces(ctx context.Context, getenv func(string) string) error {
	exporter, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return fmt.Errorf("traces exporter: %w", err)
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(s.Resource)}
	if s.Controls != nil {
		opts = append(opts, sdktrace.WithSampler(s.Controls.Sampler.Wrap(samplerFromEnv(getenv))))
	}
	if !autoexport.IsNoneSpanExporter(exporter) {
		if s.Controls != nil {
			exporter = s.Controls.Spans(exporter)
		}
		s.built("traces", exporterName("OTEL_TRACES_EXPORTER"), s.Controls != nil)
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	s.shutdown = append(s.shutdown, tp.Shutdown)
	s.TracerProvider = tp
	if s.Controls != nil {
		s.TracerProvider = s.Controls.TracerProvider(tp)
	}
	return nil
}

func (s *SDK) metrics(ctx context.Context, getenv func(string) string) error {
	reader, err := s.metricReader(ctx, getenv)
	if err != nil {
		return fmt.Errorf("metrics exporter: %w", err)
	}
	opts := []sdkmetric.Option{sdkmetric.WithResource(s.Resource)}
	if reader != nil {
		opts = append(opts, sdkmetric.WithReader(reader))
		// Gated as the host says: an exporter that metricsGated says is not gated leaves send_metrics undeclared.
		s.built("metrics", metricsExporter(getenv), s.Controls != nil && metricsGated(getenv))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	s.shutdown = append(s.shutdown, mp.Shutdown)
	s.MeterProvider = mp
	if s.Controls != nil {
		s.MeterProvider = s.Controls.MeterProvider(mp)
	}
	return nil
}

// metricsExporter is OTEL_METRICS_EXPORTER, otlp when unset.
func metricsExporter(getenv func(string) string) string {
	if name := strings.TrimSpace(getenv("OTEL_METRICS_EXPORTER")); name != "" {
		return name
	}
	return "otlp"
}

// metricsGated says whether metricReader builds the metrics reader around a gated exporter, so
// send_metrics can stop it. Known from the variables alone, before the SDK is built.
func metricsGated(getenv func(string) string) bool {
	if strings.TrimSpace(getenv("OTEL_METRICS_PRODUCERS")) != "" {
		return false
	}
	switch metricsExporter(getenv) {
	case "otlp", "console", "none":
		return true
	}
	return false
}

// metricReader is autoexport's reader for OTEL_METRICS_EXPORTER, with the exporter gated. A
// reader can't be wrapped outside the SDK, so this builds the push exporters autoexport does
// (otlp over either protocol, console) the same way, around a gated exporter. Anything else (a
// pull exporter such as prometheus, or OTEL_METRICS_PRODUCERS) is autoexport's own, and the
// server can't stop it.
func (s *SDK) metricReader(ctx context.Context, getenv func(string) string) (sdkmetric.Reader, error) {
	name := metricsExporter(getenv)
	if s.Controls == nil || !metricsGated(getenv) {
		if s.Controls != nil {
			slog.Warn("opamp: the metrics exporter is autoexport's own: the server can't stop it", "exporter", name)
		}
		r, err := autoexport.NewMetricReader(ctx)
		if err != nil || autoexport.IsNoneMetricReader(r) {
			return nil, err
		}
		return r, nil
	}
	var exporter sdkmetric.Exporter
	var err error
	switch name {
	case "none":
		return nil, nil
	case "console":
		exporter, err = stdoutmetric.New()
	default:
		protocol := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"))
		if protocol == "" {
			protocol = strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
		}
		switch protocol {
		case "grpc":
			exporter, err = otlpmetricgrpc.New(ctx)
		case "", "http/protobuf":
			exporter, err = otlpmetrichttp.New(ctx)
		default:
			return nil, fmt.Errorf("OTLP protocol %q: want grpc or http/protobuf", protocol)
		}
	}
	if err != nil {
		return nil, err
	}
	return sdkmetric.NewPeriodicReader(s.Controls.Metrics(exporter)), nil
}

func (s *SDK) logs(ctx context.Context) error {
	exporter, err := autoexport.NewLogExporter(ctx)
	if err != nil {
		return fmt.Errorf("logs exporter: %w", err)
	}
	opts := []sdklog.LoggerProviderOption{sdklog.WithResource(s.Resource)}
	if !autoexport.IsNoneLogExporter(exporter) {
		if s.Controls != nil {
			exporter = s.Controls.Logs(exporter)
		}
		s.built("logs", exporterName("OTEL_LOGS_EXPORTER"), s.Controls != nil)
		opts = append(opts, sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)))
	}
	lp := sdklog.NewLoggerProvider(opts...)
	s.shutdown = append(s.shutdown, lp.Shutdown)
	s.LoggerProvider = lp
	if s.Controls != nil {
		s.LoggerProvider = s.Controls.LoggerProvider(lp)
	}
	return nil
}

// built keeps an exporter it built for a signal, under the name OTEL_*_EXPORTER gives it, and
// whether a gate wraps it.
func (s *SDK) built(signal, name string, gated bool) {
	s.exporters = append(s.exporters, &settingspb.Exporter{Signal: signal, Name: name, Gated: gated})
}
