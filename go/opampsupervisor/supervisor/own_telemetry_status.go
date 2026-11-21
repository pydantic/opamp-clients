// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"sync"

	"go.uber.org/zap"
)

// ownTelemetryRefusals logs a refusal of the server's own telemetry settings at error once for
// each reason, and at debug when the same reason comes again: a server offers the same settings
// again at each connection, and the refusal is in the status (Status.OwnTelemetry).
type ownTelemetryRefusals struct {
	mu   sync.Mutex
	last string
}

func (r *ownTelemetryRefusals) log(logger *zap.Logger, err error) {
	r.mu.Lock()
	again := r.last == err.Error()
	r.last = err.Error()
	r.mu.Unlock()
	if again {
		logger.Debug("Refusing the own telemetry settings again, for the same reason", zap.Error(err))
		return
	}
	logger.Error("Refusing the own telemetry settings", zap.Error(err))
}

func (r *ownTelemetryRefusals) applied() {
	r.mu.Lock()
	r.last = ""
	r.mu.Unlock()
}

// refuseOwnTelemetry logs why the server's own telemetry settings are refused.
func (s *Supervisor) refuseOwnTelemetry(err error) {
	s.ownTelemetryRefusals.log(s.telemetrySettings.Logger, err)
}

// noteOwnTelemetry puts the outcome of the server's own telemetry settings in the status: applied,
// or refused and why (err), so `opampsupervisor status` says it.
func (s *Supervisor) noteOwnTelemetry(err error) {
	if err == nil {
		s.ownTelemetryRefusals.applied()
	}
	if c, ok := s.opampClient.(*coreClient); ok {
		c.report.ownTelemetry(s.telemetrySettings.Logger, err)
	}
}

// ownTelemetryPolicyError is the remote config policy's refusal of the own telemetry settings.
// The server is told so in words of the Supervisor's own (errOwnTelemetryPolicy): the policy's
// error can quote the settings, and they hold a credential.
type ownTelemetryPolicyError struct{ err error }

func (e ownTelemetryPolicyError) Error() string { return e.err.Error() }
func (e ownTelemetryPolicyError) Unwrap() error { return e.err }

// errOwnTelemetryPolicy is what the server is told of such a refusal.
const errOwnTelemetryPolicy connectionSettingsError = "The host's remote config policy (agent::remote_config_policy) doesn't allow the own telemetry settings, most often their endpoint: `opampsupervisor status` on the host says why."
