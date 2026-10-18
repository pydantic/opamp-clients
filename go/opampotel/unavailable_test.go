// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"context"
	"errors"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestACoreThatCantBeLoadedIsSaidOnceAndTheSDKRunsUnmanaged(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	wasCore, wasLayer := coreUnavailable, layerUnavailable
	t.Cleanup(func() { coreUnavailable, layerUnavailable, coreWarned = wasCore, wasLayer, sync.Once{} })
	fails := func(context.Context) error { return errors.New("compile the module: invalid magic number") }
	loads := func(context.Context) error { return nil }
	want := "WARN opamp: the core can't be loaded on " + goruntime.GOOS + "/" + goruntime.GOARCH + "; the SDK runs unmanaged"
	for _, c := range []struct {
		name        string
		core, layer func(context.Context) error
		endpoint    string
		said        []string
	}{
		{"the client core's module", fails, loads, "http://127.0.0.1:1/v1/opamp", []string{want}},
		{"the SDK settings layer's module", loads, fails, "http://127.0.0.1:1/v1/opamp", []string{want}},
		// Without a server the layer is never loaded: nothing to say.
		{"the SDK settings layer's module, with no server", loads, fails, "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			coreUnavailable, layerUnavailable, coreWarned = c.core, c.layer, sync.Once{}
			said := records(t)
			for range 2 { // set up again: not said again
				sdk, err := setup(context.Background(), env(map[string]string{"OTEL_OPAMP_ENDPOINT": c.endpoint}))
				if err != nil {
					t.Fatalf("a core that can't be loaded must not hold the application up: %v", err)
				}
				if sdk.Controls != nil || sdk.agent != nil {
					t.Fatal("managed without a core")
				}
				rec := tracetest.NewSpanRecorder()
				sdk.TracerProvider.(*sdktrace.TracerProvider).RegisterSpanProcessor(rec)
				_, span := sdk.TracerProvider.Tracer("app").Start(context.Background(), "work")
				span.End()
				if len(rec.Ended()) != 1 {
					t.Fatal("the SDK did not record the application's span")
				}
				if err := sdk.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			var opamp []string
			for _, s := range *said {
				if strings.Contains(s, "opamp") {
					opamp = append(opamp, s)
				}
			}
			if strings.Join(opamp, "\n") != strings.Join(c.said, "\n") {
				t.Fatalf("said %q, want %q", opamp, c.said)
			}
		})
	}
}
