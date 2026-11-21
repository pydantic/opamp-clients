// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// sinkCore stands in for the event log's core: like it, it leaves Enabled to the core it wraps.
type sinkCore struct {
	zapcore.Core
	wrote *[]zapcore.Level
}

func (s sinkCore) With([]zapcore.Field) zapcore.Core { return s }

func (s sinkCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if s.Enabled(ent.Level) {
		return ce.AddCore(ent, s)
	}
	return ce
}

func (s sinkCore) Write(ent zapcore.Entry, _ []zapcore.Field) error {
	*s.wrote = append(*s.wrote, ent.Level)
	return nil
}

// The service's logger, built as supervisor_windows.go builds it, writes what the config's level
// lets through: no debug line at the default (info), all of them at debug.
func TestServiceLogLevel(t *testing.T) {
	levels := func(logs config.Logs) []zapcore.Level {
		var wrote []zapcore.Level
		logger, err := zap.NewDevelopment(serviceLogLevel(logs), zap.WrapCore(func(core zapcore.Core) zapcore.Core {
			return sinkCore{core, &wrote}
		}))
		require.NoError(t, err)
		logger.Debug("Received OpAMP message from the agent")
		logger.Info("Connected to the OpAMP server.")
		logger.Warn("The remote config is unrestricted")
		return wrote
	}

	assert.Equal(t, []zapcore.Level{zapcore.InfoLevel, zapcore.WarnLevel}, levels(config.DefaultSupervisor().Telemetry.Logs))
	assert.Equal(t, []zapcore.Level{zapcore.DebugLevel, zapcore.InfoLevel, zapcore.WarnLevel}, levels(config.Logs{Level: zapcore.DebugLevel}))
	assert.Equal(t, []zapcore.Level{zapcore.WarnLevel}, levels(config.Logs{Level: zapcore.WarnLevel}))
}

// A line in the event log starts with its level, not with a time.
func TestServiceLogEncoderHasNoTime(t *testing.T) {
	buf, err := zapcore.NewConsoleEncoder(serviceLogEncoderConfig()).EncodeEntry(
		zapcore.Entry{Level: zapcore.InfoLevel, Time: time.Unix(1795194600, 0), Message: "Supervisor starting"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "info\tSupervisor starting\n", buf.String())
}
