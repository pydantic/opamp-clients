// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// Upstream's code reads and writes its files as it did; the ones the client's state holds are
// read from what it resumes, and never written.
func TestUpstreamsFilesAreTheClientsState(t *testing.T) {
	dir := t.TempDir()
	s := clientSupervisor(t, config.OpAMPServer{}, config.Capabilities{ReportsOwnMetrics: true}, config.Storage{Directory: dir})
	applied := &protobufs.AgentRemoteConfig{ConfigHash: []byte("applied")}
	telemetry := &protobufs.ConnectionSettingsOffers{Hash: []byte("t")}
	s.resumed = opamp.Resumed{AppliedRemoteConfig: applied, ConnectionSettings: telemetry}

	require.NoError(t, s.saveLastReceivedConfig(&protobufs.AgentRemoteConfig{ConfigHash: []byte("received")}))
	require.NoError(t, s.saveLastWorkingRemoteConfig(applied))
	require.NoError(t, s.saveLastReceivedOwnTelemetrySettings(telemetry, lastRecvOwnTelemetryConfigFile))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "the Supervisor wrote files of its own")

	_, err = s.readStorageFile(lastRecvRemoteConfigFile)
	require.ErrorIs(t, err, os.ErrNotExist, "the client resumes no config to work on")
	got, err := s.loadPersistedRemoteConfig(lastWorkingRemoteConfigFile)
	require.NoError(t, err)
	require.True(t, proto.Equal(applied, got))
	data, err := s.readStorageFile(lastRecvOwnTelemetryConfigFile)
	require.NoError(t, err)
	gotTelemetry := &protobufs.ConnectionSettingsOffers{}
	require.NoError(t, proto.Unmarshal(data, gotTelemetry))
	require.True(t, proto.Equal(telemetry, gotTelemetry))

	// Another file is written and read as before.
	require.NoError(t, s.writeStorageFile("other", []byte("x")))
	data, err = s.readStorageFile("other")
	require.NoError(t, err)
	require.Equal(t, []byte("x"), data)
}

// The persistent state is the client's: its instance UID (agent::instance_id when set), kept by
// the client and not in persistent_state.yaml.
func TestThePersistentStateIsTheClients(t *testing.T) {
	dir := t.TempDir()
	open := func(instanceID string) *persistentState {
		s := clientSupervisor(t, config.OpAMPServer{Endpoint: "http://127.0.0.1:9/v1/opamp"}, config.Capabilities{}, config.Storage{Directory: dir})
		s.config.Agent.InstanceID = instanceID
		p, err := s.clientPersistentState()
		require.NoError(t, err)
		s.closeOpenedClient()
		return p
	}
	configured := uuid.MustParse("018fee23-4a51-7303-a441-73faed7d9deb")
	require.Equal(t, configured, open(configured.String()).InstanceID)
	made := open("").InstanceID
	require.NotEqual(t, uuid.Nil, made)
	_, err := os.Stat(filepath.Join(dir, "persistent_state.yaml"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// A UID the client made is kept: a Supervisor started on it starts on it again. (Started with it
// as though it were configured, the client wouldn't keep it.)
func TestTheClientKeepsTheUIDItMade(t *testing.T) {
	ts := newCoreTestServer(t)
	dir := t.TempDir()
	start := func() uuid.UUID {
		ts.forget()
		s := clientSupervisor(t, config.OpAMPServer{Endpoint: ts.ws}, config.Capabilities{}, config.Storage{Directory: dir})
		var err error
		s.persistentState, err = s.clientPersistentState()
		require.NoError(t, err)
		require.NoError(t, s.startOpAMPClient())
		id := s.persistentState.InstanceID
		ts.waitMsg("the client's first message", func(m *protobufs.AgentToServer) bool {
			return uuid.UUID(m.GetInstanceUid()) == id
		})
		require.NoError(t, s.stopOpAMPClient())
		return id
	}
	made := start()
	require.Equal(t, made, start())
}
