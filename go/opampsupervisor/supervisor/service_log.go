// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// As a Windows service the Supervisor logs to the Application event log (supervisor_windows.go),
// through a logger of its own that took no notice of the config: it wrote every debug line (the
// Collector's messages to the local OpAMP server, one each heartbeat) as an Information event,
// which a config that collects the Application log then ships as telemetry. This file is what
// holds that logger to the config; it builds on every OS, so its tests run on every OS.

// serviceLogLevel holds the service's logger to the config's telemetry::logs::level: info unless
// the config sets another, debug when it asks for it.
func serviceLogLevel(logs config.Logs) zap.Option {
	return zap.IncreaseLevel(logs.Level)
}

// serviceLogEncoderConfig is how a line reads in the event log: level, caller, message and
// fields, with no time. The event has its own, and the encoder's was seconds since 1970 as a
// floating-point number at the start of every message.
func serviceLogEncoderConfig() zapcore.EncoderConfig {
	cfg := zap.NewProductionEncoderConfig()
	cfg.TimeKey = zapcore.OmitKey
	return cfg
}
