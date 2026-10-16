// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package main

// The setups try.py's truth checks run the example under (check_truth in
// examples/integrations.py): what an application does to the SDK opampotel.Setup built, named in
// EXAMPLE_SETUP (comma-separated). Without EXAMPLE_SETUP the example does none of them.
//
//   - global-tracer: once Setup has returned, the application sets a tracer provider of its own
//     as the global one, exporting over OTLP to the same place as Setup's. otelhttp's transport
//     takes its tracer from the global provider at each request, so its spans go there.
//   - global-meter: the same with a meter provider of its own, and a gauge the application
//     observes through it.
//   - global-logger: the same with a logger provider of its own. main.go makes its otelslog
//     loggers after the setups, so its log records go there.

import (
	"context"
	"errors"
	"log"
	"runtime"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

var setups = map[string]func(context.Context) (func(context.Context) error, error){
	"global-tracer": func(ctx context.Context) (func(context.Context) error, error) {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithResource(resource.Default()), sdktrace.WithBatcher(exporter))
		otel.SetTracerProvider(tp)
		return tp.Shutdown, nil
	},
	"global-meter": func(ctx context.Context) (func(context.Context) error, error) {
		exporter, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, err
		}
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(resource.Default()),
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
		otel.SetMeterProvider(mp)
		_, err = otel.Meter("checkout").Int64ObservableGauge("checkout.goroutines",
			metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
				o.Observe(int64(runtime.NumGoroutine()))
				return nil
			}))
		return mp.Shutdown, err
	},
	"global-logger": func(ctx context.Context) (func(context.Context) error, error) {
		exporter, err := otlploghttp.New(ctx)
		if err != nil {
			return nil, err
		}
		lp := sdklog.NewLoggerProvider(sdklog.WithResource(resource.Default()),
			sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)))
		global.SetLoggerProvider(lp)
		return lp.Shutdown, nil
	},
}

// applySetups carries out each setup names names, and returns what shuts them down.
func applySetups(ctx context.Context, names string) func(context.Context) error {
	var shutdown []func(context.Context) error
	for _, name := range strings.Split(names, ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		setup, ok := setups[name]
		if !ok {
			log.Fatalf("EXAMPLE_SETUP: no setup %q", name)
		}
		stop, err := setup(ctx)
		if err != nil {
			log.Fatalf("EXAMPLE_SETUP %s: %v", name, err)
		}
		shutdown = append(shutdown, stop)
	}
	return func(ctx context.Context) error {
		var errs []error
		for _, stop := range shutdown {
			errs = append(errs, stop(ctx))
		}
		return errors.Join(errs...)
	}
}
