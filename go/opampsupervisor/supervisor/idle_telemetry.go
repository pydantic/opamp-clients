// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"strings"

	"go.opentelemetry.io/collector/confmap"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// disablePlaceholderMetrics turns off the internal metrics of the Collector the Supervisor runs
// while it holds no remote config. That Collector runs only the no-op pipeline and the opamp
// extension, so nothing reads its metrics, and without a setting of its own it falls back to a
// Prometheus reader on localhost:8888 and exits when another process holds that port.
//
// It changes nothing once a config sets the Collector's metrics (the own_metrics the server
// offers, say), or when the Supervisor runs local config files of its own, which may expect the
// default reader.
func (s *Supervisor) disablePlaceholderMetrics(conf *confmap.Conf, placeholder bool) error {
	if !placeholder || conf.IsSet("service::telemetry::metrics") {
		return nil
	}
	for _, file := range s.config.Agent.ConfigFiles {
		if !strings.HasPrefix(file, "$") {
			return nil
		}
	}
	return config.MergeConfFromYAML(conf, []byte(noopTelemetry))
}
