// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A refusal of the server's own telemetry settings is in the status, with why, until settings
// are applied; the log has it at error once for each reason, not at each offer of the same
// settings (a server offers them again at each connection).
func TestOwnTelemetryRefusalIsInTheStatusAndLoggedOnce(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	dir := t.TempDir()
	c := newCoreClient(zap.NewNop(), filepath.Join(dir, coreClientStateFile), nil)
	clock := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	c.report.now = func() time.Time { return clock }
	c.report.started(c.logger, "wss://opamp.example.com/v1/opamp", [16]byte{1})
	s := &Supervisor{opampClient: c}
	s.telemetrySettings.Logger = zap.New(core)

	refused := ownTelemetryPolicyError{errors.New(`the collector's own telemetry sends to "https://user:s3cret@other.example.com/v1/logs?api_key=k3y", which agent::remote_config_policy::allowed_exporter_endpoints doesn't allow`)}
	for range 3 {
		s.refuseOwnTelemetry(refused)
		s.noteOwnTelemetry(refused)
		clock = clock.Add(15 * time.Second)
	}
	assert.Equal(t, 1, logs.FilterMessage("Refusing the own telemetry settings").FilterLevelExact(zapcore.ErrorLevel).Len(), "once at error")
	assert.Equal(t, 2, logs.FilterLevelExact(zapcore.DebugLevel).Len(), "the same reason again at debug")

	st, err := ReadStatus(dir)
	require.NoError(t, err)
	require.NotNil(t, st.OwnTelemetry)
	assert.Equal(t, "REFUSED", st.OwnTelemetry.Status)
	assert.Equal(t, clock.Add(-45*time.Second), st.OwnTelemetry.Since, "since the first refusal")
	var out bytes.Buffer
	st.Print(&out, clock)
	assert.Contains(t, out.String(), "Own telemetry  REFUSED since")
	assert.Contains(t, out.String(), "allowed_exporter_endpoints")
	assert.Contains(t, out.String(), `"https://other.example.com/v1/logs"`, "the endpoint, without its credentials")
	b, err := os.ReadFile(filepath.Join(dir, StatusFile))
	require.NoError(t, err)
	for _, secret := range []string{"s3cret", "k3y"} {
		assert.NotContains(t, string(b), secret, "no credentials in the status file")
		assert.NotContains(t, out.String(), secret)
		for _, e := range logs.All() {
			assert.NotContains(t, fmt.Sprint(e.ContextMap()), secret, "nor in the log")
		}
	}

	// Another reason, then the first again: each is said at error once.
	other := errors.New("could not compose the collector's config")
	s.refuseOwnTelemetry(other)
	s.refuseOwnTelemetry(refused)
	assert.Equal(t, 2, logs.FilterMessage("Refusing the own telemetry settings").FilterLevelExact(zapcore.ErrorLevel).Len())
	s.noteOwnTelemetry(other)
	st, err = ReadStatus(dir)
	require.NoError(t, err)
	assert.Equal(t, "FAILED", st.OwnTelemetry.Status, "not the policy's")

	s.noteOwnTelemetry(nil)
	st, err = ReadStatus(dir)
	require.NoError(t, err)
	assert.Equal(t, "APPLIED", st.OwnTelemetry.Status)
	assert.Empty(t, st.OwnTelemetry.Error)

	// Refused again after it applied: said at error again.
	s.refuseOwnTelemetry(refused)
	assert.Equal(t, 3, logs.FilterMessage("Refusing the own telemetry settings").FilterLevelExact(zapcore.ErrorLevel).Len())
}

// statusCapture is an OpAMP client that keeps the connection settings status it is given.
type statusCapture struct {
	client.OpAMPClient
	got *protobufs.ConnectionSettingsStatus
}

func (c *statusCapture) SetConnectionSettingsStatus(st *protobufs.ConnectionSettingsStatus) error {
	c.got = st
	return nil
}

// The server hears of a policy refusal of the own telemetry settings in the Supervisor's words,
// not the policy's error, which can quote the settings and their credential.
func TestTheServerHearsAnOwnTelemetryRefusalWithoutItsSettings(t *testing.T) {
	c := &statusCapture{}
	s := &Supervisor{opampClient: c}
	s.reportConnectionSettings([]byte("h"), ownTelemetryPolicyError{errors.New(`exporter "otlphttp" header Authorization "Bearer secret-token" isn't allowed`)})
	require.NotNil(t, c.got)
	assert.Equal(t, protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_FAILED, c.got.Status)
	assert.Equal(t, []byte("h"), c.got.LastConnectionSettingsHash)
	assert.Contains(t, c.got.ErrorMessage, "agent::remote_config_policy")
	assert.NotContains(t, c.got.ErrorMessage, "secret-token")
}
