// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

const storedSecret = "Bearer the-exporters-secret"

// What the server sent holds credentials (an exporter's headers, the own-telemetry settings):
// with storage.state_key nothing in the storage directory holds them in the clear. The client's
// state file keeps them, sealed; the Supervisor keeps no copies of its own (it sealed the ones
// it kept, last_recv_remote_config.dat and the rest).
func TestNothingTheServerSentIsInTheClear(t *testing.T) {
	dir := t.TempDir()
	s := clientSupervisor(t, config.OpAMPServer{}, config.Capabilities{},
		config.Storage{Directory: dir, StateKey: "file:" + filepath.Join(t.TempDir(), "key")})
	require.NoError(t, s.loadStorageSealer())

	rc := &protobufs.AgentRemoteConfig{ConfigHash: []byte("h"), Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
		"": {Body: []byte("exporters: {otlphttp: {headers: {Authorization: '" + storedSecret + "'}}}")},
	}}}
	telemetry := &protobufs.ConnectionSettingsOffers{Hash: []byte("t"), OwnMetrics: &protobufs.TelemetryConnectionSettings{
		DestinationEndpoint: "https://ingest.example/v1/metrics",
		Headers:             &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: storedSecret}}},
	}}
	ts := newCoreTestServer(t)
	ts.setRespond(func(string, *protobufs.AgentToServer) *protobufs.ServerToAgent {
		return &protobufs.ServerToAgent{RemoteConfig: rc, ConnectionSettings: telemetry}
	})

	c := newCoreClient(zaptest.NewLogger(t), filepath.Join(dir, coreClientStateFile), s.storageSealer)
	require.NoError(t, c.SetAgentDescription(&protobufs.AgentDescription{}))
	interval := 50 * time.Millisecond
	require.NoError(t, c.Start(t.Context(), types.StartSettings{
		OpAMPServerURL:    ts.url,
		InstanceUid:       types.InstanceUid(uuid.MustParse("018fee23-4a51-7303-a441-73faed7d9deb")),
		HeartbeatInterval: &interval,
		Capabilities: protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsOwnMetrics |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat,
		Callbacks: types.Callbacks{OnMessage: func(_ context.Context, m *types.MessageData) {
			if m.RemoteConfig != nil {
				assert.NoError(t, c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{
					LastRemoteConfigHash: m.RemoteConfig.ConfigHash, Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
				}))
			}
		}},
	}))
	ts.waitMsg("the config applied", func(m *protobufs.AgentToServer) bool {
		return m.GetRemoteConfigStatus().GetStatus() == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED
	})
	require.NoError(t, c.Stop(context.Background()))

	// The next start resumes them.
	again := newCoreClient(zaptest.NewLogger(t), filepath.Join(dir, coreClientStateFile), s.storageSealer)
	require.NoError(t, again.SetCapabilities(&[]protobufs.AgentCapabilities{
		protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig | protobufs.AgentCapabilities_AgentCapabilities_ReportsOwnMetrics,
	}[0]))
	require.NoError(t, again.Open(t.Context(), types.StartSettings{OpAMPServerURL: ts.url}))
	r := again.Resumed()
	require.NoError(t, again.Stop(context.Background()))
	require.Equal(t, rc.GetConfigHash(), r.AppliedRemoteConfig.GetConfigHash())
	require.Equal(t, storedSecret, r.ConnectionSettings.GetOwnMetrics().GetHeaders().GetHeaders()[0].GetValue())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		require.NotContains(t, string(raw), "the-exporters-secret", "%s in the clear", e.Name())
	}
}

// A state key that can't be found stops the Supervisor's client before it reads anything.
func TestAStateKeyThatIsNowhereIsAnError(t *testing.T) {
	bad := clientSupervisor(t, config.OpAMPServer{}, config.Capabilities{}, config.Storage{Directory: t.TempDir(), StateKey: "nowhere"})
	require.Error(t, bad.loadStorageSealer())
}
