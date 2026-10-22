// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package commander

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// The Collector doesn't inherit the Supervisor's OTEL_EXPORTER_OTLP_* variables, so headers set on
// the host can't reach an own-telemetry endpoint the server chose. agent.env and the connection
// credentials still set them.
func TestCollectorEnvironmentDropsInheritedOTLPExporterEnv(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=host-secret")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://host.example")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://host.example")
	t.Setenv("OTEL_SERVICE_NAME", "kept")
	t.Setenv("OTEL_EXPORTER_OTLPX", "kept")

	c := &Commander{cfg: config.Agent{Env: map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://agent-env.example",
	}}}
	c.SetEnvironmentProvider(func() map[string]string {
		return map[string]string{"OTEL_EXPORTER_OTLP_CERTIFICATE": "/etc/bound.pem"}
	})

	env := c.environment()
	assert.NotContains(t, env, "OTEL_EXPORTER_OTLP_HEADERS=authorization=host-secret")
	assert.NotContains(t, env, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://host.example")
	assert.NotContains(t, env, "OTEL_EXPORTER_OTLP_ENDPOINT=https://host.example")
	assert.Contains(t, env, "OTEL_EXPORTER_OTLP_ENDPOINT=https://agent-env.example")
	assert.Contains(t, env, "OTEL_EXPORTER_OTLP_CERTIFICATE=/etc/bound.pem")
	assert.Contains(t, env, "OTEL_SERVICE_NAME=kept")
	assert.Contains(t, env, "OTEL_EXPORTER_OTLPX=kept")
}
