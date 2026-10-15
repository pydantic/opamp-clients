// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package supervisor

import (
	"errors"
	"os"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// otlpExporterEnvPrefix names the variables the OTLP exporters read when they are created
// (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_INSECURE, OTEL_EXPORTER_OTLP_CERTIFICATE,
// their per-signal forms, and so on).
const otlpExporterEnvPrefix = "OTEL_EXPORTER_OTLP_"

// otlpExporterEnvMu serialises withoutOTLPExporterEnv, which changes the process environment.
var otlpExporterEnvMu sync.Mutex

// withoutOTLPExporterEnv runs build with every OTEL_EXPORTER_OTLP_* variable unset, and puts
// them back afterwards.
//
// The Supervisor's own telemetry is configured by its config file, but the OTLP exporters that
// otelconf creates from it read these variables first and let otelconf's options override only
// what otelconf sets. otelconf never says "secure" for an https endpoint, so an http:// endpoint
// or OTEL_EXPORTER_OTLP_INSECURE=true in the environment leaves an https exporter insecure as
// well as TLS, and it fails to start; a certificate in the environment does the same to an
// http endpoint. These variables are usually meant for the Collector, or set by the host, so the
// Supervisor ignores them for its own telemetry and leaves them in place for the Collector.
func withoutOTLPExporterEnv(logger *zap.Logger, build func() error) error {
	otlpExporterEnvMu.Lock()
	defer otlpExporterEnvMu.Unlock()

	saved := map[string]string{}
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(name, otlpExporterEnvPrefix) {
			saved[name] = value
		}
	}
	if len(saved) == 0 {
		return build()
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	names := make([]string, 0, len(saved))
	for name := range saved {
		names = append(names, name)
	}
	sort.Strings(names)
	// Only the names: the values can hold credentials (OTEL_EXPORTER_OTLP_HEADERS).
	logger.Info("Environment variables are ignored for the Supervisor's own telemetry, which its telemetry config sets; they still reach the Collector",
		zap.Strings("env_vars", names))

	defer func() {
		for _, name := range names {
			_ = os.Setenv(name, saved[name])
		}
	}()
	var errs []error
	for _, name := range names {
		if err := os.Unsetenv(name); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return build()
}
