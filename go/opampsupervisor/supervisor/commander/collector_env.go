// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package commander

import (
	"os"
	"strings"
)

// otlpExporterEnvPrefix names the variables the Collector's OTLP exporters, its own telemetry's
// among them, read when they are created: the endpoint, headers, certificates and the rest, and
// their per-signal forms.
const otlpExporterEnvPrefix = "OTEL_EXPORTER_OTLP_"

// inheritedEnviron is the Supervisor's environment as the Collector inherits it: without the
// OTEL_EXPORTER_OTLP_* variables. The server can choose where the Collector sends its own
// telemetry, and the exporter it makes for that reads these variables, so headers set on the
// host (OTEL_EXPORTER_OTLP_HEADERS, a credential for some other backend) would go to the
// server's endpoint. A variable set in agent.env is passed as before: it is added after these.
func inheritedEnviron() []string {
	return withoutOTLPExporterEnv(os.Environ())
}

func withoutOTLPExporterEnv(environ []string) []string {
	result := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, otlpExporterEnvPrefix) {
			continue
		}
		result = append(result, kv)
	}
	return result
}
