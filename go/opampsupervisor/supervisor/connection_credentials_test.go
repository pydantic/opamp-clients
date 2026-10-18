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
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/commander"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/telemetry"
	"github.com/stretchr/testify/require"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

func TestOtherConnectionCapabilityRequiresConfiguredBindings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		enabled  bool
		bindings map[string]string
		want     bool
	}{
		{"configured", true, map[string]string{"logfire/otlp": "LOGFIRE_TOKEN"}, true},
		{"no bindings", true, nil, false},
		{"disabled", false, map[string]string{"logfire/otlp": "LOGFIRE_TOKEN"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newCoreTestServer(t)
			s := clientSupervisor(t, config.OpAMPServer{Endpoint: ts.ws}, config.Capabilities{AcceptsOtherConnectionSettings: tc.enabled}, config.Storage{})
			s.config.Agent.ConnectionEnv = tc.bindings
			require.NoError(t, s.startOpAMPClient())
			defer func() { require.NoError(t, s.stopOpAMPClient()) }()
			hello := ts.waitMsg("hello", func(*protobufs.AgentToServer) bool { return true })
			require.Equal(t, tc.want, hello.Capabilities&uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsOtherConnectionSettings) != 0)
		})
	}
}

func TestConnectionCredentialsRejectDuplicateEnvironmentBindings(t *testing.T) {
	settings := map[string]*protobufs.OtherConnectionSettings{
		"first":  {Headers: &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: "Bearer first-secret"}}}},
		"second": {Headers: &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: "Bearer second-secret"}}}},
	}
	env, err := connectionCredentials(map[string]string{"first": "EXPORT_TOKEN", "second": "EXPORT_TOKEN"}, settings)
	require.ErrorContains(t, err, "same environment binding")
	require.Nil(t, env)
	require.NotContains(t, err.Error(), "first-secret")
	require.NotContains(t, err.Error(), "second-secret")
}

func TestConnectionUpdateRetriesFailedCollectorStart(t *testing.T) {
	s := newComposeMergedConfigTestSupervisor(t, filepath.Join(t.TempDir(), "missing-collector"), false)
	metrics, err := telemetry.NewMetrics(metricnoop.NewMeterProvider())
	require.NoError(t, err)
	s.metrics = metrics
	s.doneChan = make(chan struct{})
	s.connectionUpdates = make(chan connectionUpdate, 1)
	failed := make(chan struct{}, 1)
	s.opampClient = &mockOpAMPClient{setHealthFunc: func(health *protobufs.ComponentHealth) {
		if !health.Healthy {
			select {
			case failed <- struct{}{}:
			default:
			}
		}
	}}
	_, err = s.composeMergedConfig(nil)
	require.NoError(t, err)
	core, observed := observer.New(zap.DebugLevel)
	agent := s.config.Agent
	agent.Env = map[string]string{supervisorRestartHelperEnv: "sleep"}
	s.commander, err = commander.NewCommander(zap.New(core), filepath.Join(t.TempDir(), "collector.log"), agent)
	require.NoError(t, err)
	stopped := make(chan struct{})
	go func() { defer close(stopped); s.runAgentProcess() }()
	t.Cleanup(func() {
		close(s.doneChan)
		select {
		case <-stopped:
		case <-time.After(15 * time.Second):
			t.Error("the Collector loop did not stop")
		}
	})
	s.connectionUpdates <- connectionUpdate{hash: []byte("credential-update")}
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("the missing Collector did not fail to start")
	}
	s.commander.SetExecutable(os.Args[0])
	require.Eventually(t, func() bool {
		return observed.FilterMessage(commander.AgentStartedLogMsg).Len() == 1
	}, 8*time.Second, 10*time.Millisecond)
}
