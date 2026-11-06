// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build e2e

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server/types"
	"github.com/stretchr/testify/require"
)

func TestSupervisorRefusesARemoteConfigItsPolicyDoesNotAllow(t *testing.T) {
	var status atomic.Pointer[protobufs.RemoteConfigStatus]
	var effective atomic.Value
	var health atomic.Pointer[protobufs.ComponentHealth]
	server := newOpAMPServer(t, defaultConnectingHandler, types.ConnectionCallbacks{
		OnMessage: func(_ context.Context, _ types.Connection, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
			if m.RemoteConfigStatus != nil {
				status.Store(m.RemoteConfigStatus)
			}
			if c := m.GetEffectiveConfig().GetConfigMap().GetConfigMap()[""]; c != nil {
				effective.Store(string(c.Body))
			}
			if m.Health != nil {
				health.Store(m.Health)
			}
			return &protobufs.ServerToAgent{}
		},
	})
	storageDir := t.TempDir()
	s, _ := newSupervisor(t, "policy", map[string]string{"url": server.addr, "storage_dir": storageDir})
	require.NoError(t, s.Start(t.Context()))
	stop := sync.OnceFunc(s.Shutdown)
	defer stop()
	waitForSupervisorConnection(server.supervisorConnected, true)

	send := func(body []byte) []byte {
		hash := sha256.Sum256(body)
		server.sendToSupervisor(&protobufs.ServerToAgent{RemoteConfig: &protobufs.AgentRemoteConfig{
			Config:     &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: body}}},
			ConfigHash: hash[:],
		}})
		return hash[:]
	}
	waitStatus := func(hash []byte, want protobufs.RemoteConfigStatuses) *protobufs.RemoteConfigStatus {
		var got *protobufs.RemoteConfigStatus
		require.Eventually(t, func() bool {
			got = status.Load()
			return got != nil && bytes.Equal(got.LastRemoteConfigHash, hash) && got.Status == want
		}, 30*time.Second, 100*time.Millisecond, "never %s", want)
		return got
	}

	allowed, _, _ := createHealthCheckCollectorConf(t, true)
	waitStatus(send(allowed.Bytes()), protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED)
	require.Eventually(t, func() bool { h := health.Load(); return h != nil && h.Healthy }, 30*time.Second, 100*time.Millisecond)

	refused := waitStatus(send([]byte(strings.Replace(allowed.String(), "receivers:\n  nop:", "receivers:\n  nop:\n  filelog:\n    include: [/etc/shadow]", 1))),
		protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED)
	require.Contains(t, refused.ErrorMessage, `configures receiver "filelog"`)

	refused = waitStatus(send([]byte(strings.Replace(allowed.String(), `"localhost:13133"`, `"${file:/etc/hostname}"`, 1))),
		protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED)
	require.Contains(t, refused.ErrorMessage, "references the file provider")

	cfg, _ := effective.Load().(string)
	require.NotContains(t, cfg, "filelog", "the collector runs the config it ran")
	require.NotContains(t, cfg, "${file:")
	h := health.Load()
	require.True(t, h != nil && h.Healthy)

	// The refused config is the last one received: a restart doesn't run it, or fail on it.
	stop()
	waitForSupervisorConnection(server.supervisorConnected, false)
	restarted, _ := newSupervisor(t, "policy", map[string]string{"url": server.addr, "storage_dir": storageDir})
	require.NoError(t, restarted.Start(t.Context()))
	defer restarted.Shutdown()
	waitForSupervisorConnection(server.supervisorConnected, true)
}

// The packaged configs' settings (validate_config, automatic_config_rollback, the crash log): a
// config naming a receiver the Collector doesn't have is FAILED before it runs, with the
// Collector's reason, and the Collector goes on running the config it had, never restarted.
func TestSupervisorRefusesAConfigWithAComponentTheCollectorLacks(t *testing.T) {
	storageDir := t.TempDir()
	var status atomic.Pointer[protobufs.RemoteConfigStatus]
	server := newOpAMPServer(t, defaultConnectingHandler, types.ConnectionCallbacks{
		OnMessage: func(_ context.Context, _ types.Connection, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
			if m.RemoteConfigStatus != nil {
				status.Store(m.RemoteConfigStatus)
			}
			return &protobufs.ServerToAgent{}
		},
	})
	s, cfg := newSupervisor(t, "basic", map[string]string{
		"url":                             server.addr,
		"storage_dir":                     storageDir,
		"automatic_config_rollback":       "true",
		"validate_config":                 "true",
		"collector_crash_log_snippet_kib": "4",
	})
	require.True(t, cfg.Agent.ValidateConfig)
	require.NoError(t, s.Start(t.Context()))
	defer s.Shutdown()
	waitForSupervisorConnection(server.supervisorConnected, true)

	send := func(body []byte) []byte {
		h := sha256.Sum256(body)
		server.sendToSupervisor(&protobufs.ServerToAgent{RemoteConfig: &protobufs.AgentRemoteConfig{
			Config:     &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: body}}},
			ConfigHash: h[:],
		}})
		return h[:]
	}
	working, _, port := createHealthCheckCollectorConf(t, true)
	workingHash := send(working.Bytes())
	require.Eventually(t, func() bool {
		st := status.Load()
		return st != nil && st.Status == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED && bytes.Equal(st.LastRemoteConfigHash, workingHash)
	}, 15*time.Second, 100*time.Millisecond, "the working config was not APPLIED")
	require.True(t, healthCheckOK(port))

	// The same config with a kafka receiver, which this Collector isn't built with.
	bad := strings.Replace(working.String(), "receivers:\n  nop:\n", "receivers:\n  nop:\n  kafka:\n", 1)
	bad = strings.Replace(bad, "receivers: [nop]", "receivers: [nop, kafka]", 1)
	require.NotEqual(t, working.String(), bad)
	badHash := send([]byte(bad))
	require.Eventually(t, func() bool {
		st := status.Load()
		return st != nil && st.Status == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED && bytes.Equal(st.LastRemoteConfigHash, badHash)
	}, 15*time.Second, 100*time.Millisecond, "the config with a missing receiver was not FAILED")
	require.Contains(t, status.Load().ErrorMessage, `unknown type: "kafka"`)
	// Never stopped for it: the health endpoint answers throughout.
	require.Never(t, func() bool { return !healthCheckOK(port) }, 3*time.Second, 50*time.Millisecond, "the Collector stopped for a config it was never given")
}
