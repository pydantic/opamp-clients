// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
	"github.com/stretchr/testify/require"
)

func TestBootstrapExchangePersistsBothCredentialsAcrossRestart(t *testing.T) {
	ts := newCoreTestServer(t)
	var once sync.Once
	ts.setRespond(func(auth string, _ *protobufs.AgentToServer) *protobufs.ServerToAgent {
		var offer *protobufs.ServerToAgent
		if auth == "Bearer enroll" {
			once.Do(func() {
				offer = &protobufs.ServerToAgent{ConnectionSettings: &protobufs.ConnectionSettingsOffers{
					Hash:             []byte("device-credentials-v1"),
					Opamp:            &protobufs.OpAMPConnectionSettings{DestinationEndpoint: ts.ws, Headers: &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: "Bearer device-opamp"}}}},
					OtherConnections: map[string]*protobufs.OtherConnectionSettings{"logfire/otlp": {Headers: &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: "Bearer device-ingest"}}}}},
				}}
			})
		}
		return offer
	})
	storage := config.Storage{Directory: t.TempDir(), StateKey: "file:" + filepath.Join(t.TempDir(), "key")}
	server := config.OpAMPServer{Endpoint: ts.ws, Headers: http.Header{"Authorization": {"Bearer enroll"}}}
	caps := config.Capabilities{AcceptsOpAMPConnectionSettings: true, AcceptsOtherConnectionSettings: true, ReportsOwnMetrics: true}
	start := func() *Supervisor {
		s := clientSupervisor(t, server, caps, storage)
		s.config.Agent.ConnectionEnv = map[string]string{"logfire/otlp": "LOGFIRE_TOKEN"}
		s.connectionUpdates = make(chan connectionUpdate, 1)
		require.NoError(t, s.startOpAMPClient())
		select {
		case update := <-s.connectionUpdates:
			require.Equal(t, map[string]string{"LOGFIRE_TOKEN": "device-ingest"}, *s.connectionEnv.Load())
			s.reportConnectionSettings(update.hash, nil)
		case <-time.After(5 * time.Second):
			t.Fatal("the Supervisor did not receive its exporter credential")
		}
		return s
	}
	s := start()
	ts.waitMsg("the device credential on the wire", func(*protobufs.AgentToServer) bool { return slices.Contains(ts.auths, "Bearer device-opamp") })
	ts.waitMsg("both settings applied", func(m *protobufs.AgentToServer) bool {
		return m.ConnectionSettingsStatus.GetStatus() == protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED
	})
	require.NoError(t, s.stopOpAMPClient())
	state, err := os.ReadFile(filepath.Join(storage.Directory, coreClientStateFile))
	require.NoError(t, err)
	require.False(t, bytes.Contains(state, []byte("device-opamp")))
	require.False(t, bytes.Contains(state, []byte("device-ingest")))
	ts.forget()
	s = start()
	defer func() { require.NoError(t, s.stopOpAMPClient()) }()
	ts.waitMsg("a message after restart", func(*protobufs.AgentToServer) bool { return true })
	require.NotContains(t, ts.seenAuths(), "Bearer enroll")
}

func TestConnectionCredentialBindings(t *testing.T) {
	connection := func(value string) *protobufs.OtherConnectionSettings {
		return &protobufs.OtherConnectionSettings{Headers: &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: value}}}}
	}
	bindings := map[string]string{"logfire/otlp": "LOGFIRE_TOKEN"}
	env, err := connectionCredentials(bindings, map[string]*protobufs.OtherConnectionSettings{"logfire/otlp": connection("Bearer synthetic-token")})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"LOGFIRE_TOKEN": "synthetic-token"}, env)
	for name, settings := range map[string]map[string]*protobufs.OtherConnectionSettings{
		"unconfigured destination": {"unexpected": connection("Bearer synthetic-secret")},
		"missing credential":       {"logfire/otlp": {}},
		"non-bearer":               {"logfire/otlp": connection("Basic synthetic-secret")},
		"header injection":         {"logfire/otlp": connection("Bearer synthetic-secret\r\nInjected: yes")},
		"two authorizations":       {"logfire/otlp": {Headers: &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: "Bearer synthetic-secret"}, {Key: "authorization", Value: "Bearer another-secret"}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			env, err := connectionCredentials(bindings, settings)
			require.Error(t, err)
			require.Nil(t, env)
			require.NotContains(t, err.Error(), "synthetic-secret")
		})
	}
}
