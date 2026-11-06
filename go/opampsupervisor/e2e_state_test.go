// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build e2e

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// seedAppliedRemoteConfig leaves rc in the OpAMP client's state file in dir as the config the
// collector last applied: what a Supervisor that ran it before starts from. The Supervisor
// keeps no copy of its own (it read last_recv_remote_config.dat).
func seedAppliedRemoteConfig(t *testing.T, dir string, rc *protobufs.AgentRemoteConfig) {
	t.Helper()
	var applied atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var m protobufs.AgentToServer
		if err := proto.Unmarshal(body, &m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		st := m.GetRemoteConfigStatus()
		if st.GetStatus() == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED && bytes.Equal(st.GetLastRemoteConfigHash(), rc.GetConfigHash()) {
			applied.Store(true)
		}
		out, _ := proto.Marshal(&protobufs.ServerToAgent{InstanceUid: m.InstanceUid, RemoteConfig: rc})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}))
	defer srv.Close()
	c, err := opamp.Start(t.Context(), opamp.Config{
		Endpoint: srv.URL, StateFile: filepath.Join(dir, "opamp-state"), HeartbeatInterval: 50 * time.Millisecond,
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig | protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig),
	}, opamp.Status{}, func(c *opamp.Client, n opamp.Notification) {
		if n.Kind == opamp.RemoteConfig {
			c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{
				LastRemoteConfigHash: n.RemoteConfig.GetConfigHash(), Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
			})
		}
	})
	require.NoError(t, err)
	require.Eventually(t, applied.Load, 10*time.Second, 20*time.Millisecond, "the seeded config was never applied")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, c.Stop(ctx))
}

// reportedApplied is whether the last RemoteConfigStatus held in v says hash is applied.
func reportedApplied(v *atomic.Value, hash []byte) bool {
	st, ok := v.Load().(*protobufs.RemoteConfigStatus)
	return ok && st.GetStatus() == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED && bytes.Equal(st.GetLastRemoteConfigHash(), hash)
}
