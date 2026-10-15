// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package supervisor

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/config/configtelemetry"
	"go.opentelemetry.io/collector/service/telemetry/otelconftelemetry"
	otelconf "go.opentelemetry.io/contrib/otelconf/v0.3.0"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// TestInitTelemetrySettingsIgnoresOTLPExporterEnv checks that the Supervisor's own OTLP
// exporters take their scheme and TLS settings from the Supervisor's config, not from
// OTEL_EXPORTER_OTLP_* variables in the environment (which are there for the Collector or
// set by the host, such as a CI runner).
func TestInitTelemetrySettingsIgnoresOTLPExporterEnv(t *testing.T) {
	tests := []struct {
		name string
		// tls is whether the Supervisor's config names an https endpoint.
		tls bool
		// env sets OTEL_EXPORTER_OTLP_* variables that contradict the config.
		env func(t *testing.T, otherCertFile string)
	}{
		{
			name: "https endpoint in config, http endpoint in env",
			tls:  true,
			env: func(t *testing.T, _ string) {
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
			},
		},
		{
			name: "https endpoint in config, insecure in env",
			tls:  true,
			env: func(t *testing.T, _ string) {
				t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
			},
		},
		{
			name: "http endpoint in config, certificate in env",
			tls:  false,
			env: func(t *testing.T, otherCertFile string) {
				t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", otherCertFile)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newRecordingOTLPServer(t, tt.tls)
			other := httptest.NewTLSServer(http.NotFoundHandler())
			t.Cleanup(other.Close)
			tt.env(t, writeServerCertFile(t, other))
			envBefore := os.Environ()

			var certificate *string
			if tt.tls {
				certFile := writeServerCertFile(t, srv.Server)
				certificate = &certFile
			}
			protocol := "http/protobuf"
			otlp := func(path string) *otelconf.OTLP {
				endpoint := srv.URL + path
				return &otelconf.OTLP{Protocol: &protocol, Endpoint: &endpoint, Certificate: certificate}
			}
			metricsEndpoint := srv.URL + "/v1/metrics"

			settings, err := initTelemetrySettings(t.Context(), zap.NewNop(), config.Telemetry{
				Logs: config.Logs{
					Level: zap.InfoLevel,
					Processors: []otelconf.LogRecordProcessor{{
						Simple: &otelconf.SimpleLogRecordProcessor{Exporter: otelconf.LogRecordExporter{OTLP: otlp("/v1/logs")}},
					}},
				},
				Metrics: config.Metrics{
					Level: configtelemetry.LevelBasic,
					Readers: []otelconf.MetricReader{{
						Periodic: &otelconf.PeriodicMetricReader{Exporter: otelconf.PushMetricExporter{OTLP: &otelconf.OTLPMetric{
							Protocol: &protocol, Endpoint: &metricsEndpoint, Certificate: certificate,
						}}},
					}},
				},
				Traces: otelconftelemetry.TracesConfig{
					TracerProvider: otelconf.TracerProvider{
						Processors: []otelconf.SpanProcessor{{
							Simple: &otelconf.SimpleSpanProcessor{Exporter: otelconf.SpanExporter{OTLP: otlp("/v1/traces")}},
						}},
					},
				},
			})
			require.NoError(t, err)
			assert.ElementsMatch(t, envBefore, os.Environ(), "the environment is restored")

			type flusher interface {
				ForceFlush(context.Context) error
				Shutdown(context.Context) error
			}
			providers := []any{settings.TracerProvider, settings.MeterProvider, settings.loggerProvider}
			for _, p := range providers {
				t.Cleanup(func() { _ = p.(flusher).Shutdown(context.Background()) })
			}

			ctx := t.Context()
			_, span := settings.TracerProvider.Tracer("test").Start(ctx, "span")
			span.End()
			counter, err := settings.MeterProvider.Meter("test").Int64Counter("counter")
			require.NoError(t, err)
			counter.Add(ctx, 1)
			var record otellog.Record
			record.SetBody(attribute.StringValue("log"))
			settings.loggerProvider.Logger("test").Emit(ctx, record)
			for _, p := range providers {
				require.NoError(t, p.(flusher).ForceFlush(ctx))
			}

			assert.Equal(t, map[string]bool{"/v1/traces": tt.tls, "/v1/metrics": tt.tls, "/v1/logs": tt.tls}, srv.requests())
		})
	}
}

// recordingOTLPServer accepts OTLP/HTTP exports and records, per path, whether the request
// came over TLS.
type recordingOTLPServer struct {
	*httptest.Server
	mu  sync.Mutex
	got map[string]bool
}

func newRecordingOTLPServer(t *testing.T, useTLS bool) *recordingOTLPServer {
	t.Helper()
	s := &recordingOTLPServer{got: map[string]bool{}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.got[r.URL.Path] = r.TLS != nil
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	})
	if useTLS {
		s.Server = httptest.NewTLSServer(handler)
	} else {
		s.Server = httptest.NewServer(handler)
	}
	t.Cleanup(s.Close)
	return s
}

func (s *recordingOTLPServer) requests() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	got := make(map[string]bool, len(s.got))
	for k, v := range s.got {
		got[k] = v
	}
	return got
}

// writeServerCertFile writes srv's TLS certificate as PEM and returns the file's path.
func writeServerCertFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(path, block, 0o600))
	return path
}

// NewSupervisor takes a nil logger (its tests pass one): with the variables set, the Supervisor
// still starts, and says nothing of what it ignored.
func TestWithoutOTLPExporterEnvTakesANilLogger(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	ran := false
	require.NoError(t, withoutOTLPExporterEnv(nil, func() error {
		ran = true
		_, set := os.LookupEnv("OTEL_EXPORTER_OTLP_ENDPOINT")
		assert.False(t, set)
		return nil
	}))
	assert.True(t, ran)
	assert.Equal(t, "http://127.0.0.1:4318", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
}
