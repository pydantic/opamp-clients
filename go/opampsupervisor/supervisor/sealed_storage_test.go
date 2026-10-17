// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

const storedSecret = "Bearer the-exporters-secret"

// The Supervisor's copies of what the server sent hold credentials (an exporter's headers, the
// own-telemetry settings): with storage.state_key they are sealed like the client's state file.
func TestSupervisorSealsWhatTheServerSent(t *testing.T) {
	dir := t.TempDir()
	s := clientSupervisor(t, config.OpAMPServer{}, config.Capabilities{ReportsOwnMetrics: true},
		config.Storage{Directory: dir, StateKey: "file:" + filepath.Join(t.TempDir(), "key")})
	require.NoError(t, s.loadStorageSealer())

	rc := &protobufs.AgentRemoteConfig{ConfigHash: []byte("h"), Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
		"": {Body: []byte("exporters: {otlphttp: {headers: {Authorization: '" + storedSecret + "'}}}")},
	}}}
	require.NoError(t, s.saveLastReceivedConfig(rc))
	require.NoError(t, s.saveLastWorkingRemoteConfig(rc))
	telemetry := &protobufs.ConnectionSettingsOffers{OwnMetrics: &protobufs.TelemetryConnectionSettings{
		Headers: &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: storedSecret}}},
	}}
	require.NoError(t, s.saveLastReceivedOwnTelemetrySettings(telemetry, lastRecvOwnTelemetryConfigFile))

	for _, name := range []string{lastRecvRemoteConfigFile, lastWorkingRemoteConfigFile, lastRecvOwnTelemetryConfigFile} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.NotContains(t, string(raw), storedSecret, "%s in the clear", name)
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	for _, name := range []string{lastRecvRemoteConfigFile, lastWorkingRemoteConfigFile} {
		got, err := s.loadPersistedRemoteConfig(name)
		require.NoError(t, err)
		require.True(t, proto.Equal(rc, got), name)
	}
	got, err := s.readStorageFile(lastRecvOwnTelemetryConfigFile)
	require.NoError(t, err)
	opened := &protobufs.ConnectionSettingsOffers{}
	require.NoError(t, proto.Unmarshal(got, opened))
	require.True(t, proto.Equal(telemetry, opened))

	// Another key doesn't open them.
	other := clientSupervisor(t, config.OpAMPServer{}, config.Capabilities{},
		config.Storage{Directory: dir, StateKey: "file:" + filepath.Join(t.TempDir(), "key")})
	require.NoError(t, other.loadStorageSealer())
	_, err = other.loadPersistedRemoteConfig(lastRecvRemoteConfigFile)
	require.Error(t, err)
}

// Files saved before state_key was set are read as they are, and sealed at the next save.
func TestSupervisorReadsFilesSavedBeforeSealing(t *testing.T) {
	dir := t.TempDir()
	plain := clientSupervisor(t, config.OpAMPServer{}, config.Capabilities{}, config.Storage{Directory: dir})
	require.NoError(t, plain.loadStorageSealer())
	require.Nil(t, plain.storageSealer)
	rc := &protobufs.AgentRemoteConfig{ConfigHash: []byte("h"), Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
		"": {Body: []byte(storedSecret)},
	}}}
	require.NoError(t, plain.saveLastReceivedConfig(rc))

	s := clientSupervisor(t, config.OpAMPServer{}, config.Capabilities{},
		config.Storage{Directory: dir, StateKey: "file:" + filepath.Join(t.TempDir(), "key")})
	require.NoError(t, s.loadStorageSealer())
	got, err := s.loadPersistedRemoteConfig(lastRecvRemoteConfigFile)
	require.NoError(t, err)
	require.True(t, proto.Equal(rc, got))
	require.NoError(t, s.saveLastReceivedConfig(got))
	raw, err := os.ReadFile(filepath.Join(dir, lastRecvRemoteConfigFile))
	require.NoError(t, err)
	require.NotContains(t, string(raw), storedSecret)

	bad := clientSupervisor(t, config.OpAMPServer{}, config.Capabilities{}, config.Storage{Directory: dir, StateKey: "nowhere"})
	require.Error(t, bad.loadStorageSealer())
}
