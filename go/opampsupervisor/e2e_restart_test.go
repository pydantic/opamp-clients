// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build e2e

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authSink is an OTLP/HTTP endpoint that keeps the Authorization header of every request.
type authSink struct {
	addr string
	mu   sync.Mutex
	seen []string
}

func newAuthSink(t *testing.T) *authSink {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &authSink{addr: ln.Addr().String()}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Header.Get("Authorization"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return s
}

func (s *authSink) auths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

func (s *authSink) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = nil
}

// The issued exporter credential (other_connections, bound by agent.connection_env) reaches the
// Collector's environment, and does again after the Supervisor restarts (here within the
// config's apply window) with no word from the server: the client hands its saved settings over
// at start. A Collector run without it would export with an empty token until the server offered
// it again.
func TestSupervisorKeepsTheIssuedExporterCredentialAcrossARestart(t *testing.T) {
	sink := newAuthSink(t)
	body := fmt.Sprintf(`receivers:
  hostmetrics:
    collection_interval: 1s
    scrapers:
      load:
exporters:
  otlphttp:
    endpoint: http://%s
    headers:
      Authorization: "Bearer ${env:LOGFIRE_TOKEN}"
    retry_on_failure:
      enabled: false
    sending_queue:
      enabled: false
service:
  pipelines:
    metrics:
      receivers: [hostmetrics]
      exporters: [otlphttp]
`, sink.addr)
	hash := sha256.Sum256([]byte(body))
	remote := &protobufs.AgentRemoteConfig{
		Config:     &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte(body)}}},
		ConfigHash: hash[:],
	}
	offer := &protobufs.ConnectionSettingsOffers{
		Hash: []byte("offer-1"),
		OtherConnections: map[string]*protobufs.OtherConnectionSettings{
			"logfire/otlp": {Headers: &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: "Bearer issued-1"}}}},
		},
	}

	var mu sync.Mutex
	var settingsStatuses []protobufs.ConnectionSettingsStatuses
	callbacks := types.ConnectionCallbacks{
		OnMessage: func(_ context.Context, _ types.Connection, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
			if st := m.GetConnectionSettingsStatus(); st != nil {
				mu.Lock()
				settingsStatuses = append(settingsStatuses, st.GetStatus())
				mu.Unlock()
			}
			return &protobufs.ServerToAgent{}
		},
	}
	storage := t.TempDir()
	srv := newOpAMPServer(t, defaultConnectingHandler, callbacks)
	data := map[string]string{"url": srv.addr, "storage_dir": escapePathStringForWin(storage)}
	s, _ := newSupervisor(t, "connection_env", data)
	require.NoError(t, s.Start(t.Context()))
	waitForSupervisorConnection(srv.supervisorConnected, true)
	srv.sendToSupervisor(&protobufs.ServerToAgent{RemoteConfig: remote, ConnectionSettings: offer})

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Contains(c, sink.auths(), "Bearer issued-1")
		mu.Lock()
		defer mu.Unlock()
		assert.Contains(c, settingsStatuses, protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED)
	}, 30*time.Second, 250*time.Millisecond, "the Collector never exported with the issued credential")
	s.Shutdown()
	// The test server tells of the disconnect before it takes the next connection.
	waitForSupervisorConnection(srv.supervisorConnected, false)

	// Restarted, on the same server, which sends neither again.
	sink.forget()
	s, _ = newSupervisor(t, "connection_env", data)
	require.NoError(t, s.Start(t.Context()))
	defer s.Shutdown()
	defer srv.shutdown()
	waitForSupervisorConnection(srv.supervisorConnected, true)
	require.Eventually(t, func() bool { return len(sink.auths()) >= 5 }, 30*time.Second, 250*time.Millisecond,
		"the restarted Collector exported nothing")
	for _, auth := range sink.auths() {
		assert.Equal(t, "Bearer issued-1", auth, "the restarted Collector exported without the issued credential")
	}
}

// With the packaged settings (validate_config, automatic_config_rollback, the crash log), a
// config naming a component the Collector lacks is FAILED without the Collector stopping, and
// after the Supervisor restarts the Collector runs the config that worked: the refused one isn't
// tried again (no crash loop), and its FAILED stands.
func TestSupervisorRestartsOnTheLastWorkingConfigAfterAConfigFailedValidation(t *testing.T) {
	var mu sync.Mutex
	var statuses []*protobufs.RemoteConfigStatus
	srv := newOpAMPServer(t, defaultConnectingHandler, types.ConnectionCallbacks{
		OnMessage: func(_ context.Context, _ types.Connection, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
			if st := m.GetRemoteConfigStatus(); st != nil {
				mu.Lock()
				statuses = append(statuses, st)
				mu.Unlock()
			}
			return &protobufs.ServerToAgent{}
		},
	})
	defer srv.shutdown()
	last := func() *protobufs.RemoteConfigStatus {
		mu.Lock()
		defer mu.Unlock()
		if len(statuses) == 0 {
			return nil
		}
		return statuses[len(statuses)-1]
	}
	is := func(st *protobufs.RemoteConfigStatus, want protobufs.RemoteConfigStatuses, hash []byte) bool {
		return st != nil && st.GetStatus() == want && bytes.Equal(st.GetLastRemoteConfigHash(), hash)
	}
	data := map[string]string{
		"url":                             srv.addr,
		"storage_dir":                     escapePathStringForWin(t.TempDir()),
		"automatic_config_rollback":       "true",
		"validate_config":                 "true",
		"collector_crash_log_snippet_kib": "4",
	}
	s, _ := newSupervisor(t, "basic", data)
	require.NoError(t, s.Start(t.Context()))
	waitForSupervisorConnection(srv.supervisorConnected, true)
	send := func(body string) []byte {
		h := sha256.Sum256([]byte(body))
		srv.sendToSupervisor(&protobufs.ServerToAgent{RemoteConfig: &protobufs.AgentRemoteConfig{
			Config:     &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte(body)}}},
			ConfigHash: h[:],
		}})
		return h[:]
	}
	working, _, port := createHealthCheckCollectorConf(t, true)
	workingHash := send(working.String())
	require.Eventually(t, func() bool {
		return is(last(), protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, workingHash)
	},
		15*time.Second, 100*time.Millisecond, "the working config was not APPLIED")
	bad := strings.Replace(working.String(), "receivers:\n  nop:\n", "receivers:\n  nop:\n  kafka:\n", 1)
	bad = strings.Replace(bad, "receivers: [nop]", "receivers: [nop, kafka]", 1)
	require.NotEqual(t, working.String(), bad)
	badHash := send(bad)
	require.Eventually(t, func() bool { return is(last(), protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, badHash) },
		15*time.Second, 100*time.Millisecond, "the config with a missing receiver was not FAILED")
	s.Shutdown()
	// The test server tells of the disconnect before it takes the next connection.
	waitForSupervisorConnection(srv.supervisorConnected, false)

	mu.Lock()
	statuses = nil
	mu.Unlock()
	s, _ = newSupervisor(t, "basic", data)
	require.NoError(t, s.Start(t.Context()))
	defer s.Shutdown()
	waitForSupervisorConnection(srv.supervisorConnected, true)
	require.Eventually(t, func() bool { return healthCheckOK(port) }, 15*time.Second, 100*time.Millisecond,
		"the restarted Collector doesn't run the last working config")
	require.Never(t, func() bool { return !healthCheckOK(port) }, 5*time.Second, 50*time.Millisecond,
		"the restarted Collector stopped: the refused config was tried again")
	require.True(t, is(last(), protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, badHash),
		"after the restart the server hears %v, not the refused config's FAILED", last())
}
