// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// A collector the server stops offering that can't be removed is reported failed, with why, and
// keeps running until the server offers one again. It was left unanswered, so the core kept it
// uncertain and asked for the removal again at every start, telling the server nothing.
func TestACollectorThatCannotBeRemovedIsReportedFailed(t *testing.T) {
	body := []byte("collector")
	sum := sha256.Sum256(body)
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(files.Close)
	offered := &protobufs.PackagesAvailable{AllPackagesHash: []byte("one"), Packages: map[string]*protobufs.PackageAvailable{
		"": {Type: protobufs.PackageType_PackageType_TopLevel, Version: "v2", Hash: []byte("v2"),
			File: &protobufs.DownloadableFile{DownloadUrl: files.URL + "/otelcol", ContentHash: sum[:]}},
	}}
	dropped := &protobufs.PackagesAvailable{AllPackagesHash: []byte("none"), Packages: map[string]*protobufs.PackageAvailable{}}

	ts := newCoreTestServer(t)
	var installed atomic.Bool
	ts.setRespond(func(_ string, _ *protobufs.AgentToServer) *protobufs.ServerToAgent {
		if installed.Load() {
			return &protobufs.ServerToAgent{PackagesAvailable: dropped}
		}
		return &protobufs.ServerToAgent{PackagesAvailable: offered}
	})

	c := newCoreClient(zaptest.NewLogger(t), filepath.Join(t.TempDir(), coreClientStateFile), nil)
	removals := make(chan struct{}, 4)
	c.installPackage = func(req agentInstall) {
		if req.remove {
			removals <- struct{}{}
			req.done(errors.New("the installed collector is busy"))
			return
		}
		installed.Store(true)
		req.done(nil)
	}
	require.NoError(t, c.SetAgentDescription(&protobufs.AgentDescription{}))
	interval := 50 * time.Millisecond
	require.NoError(t, c.Start(t.Context(), types.StartSettings{
		OpAMPServerURL:    ts.url,
		InstanceUid:       types.InstanceUid(uuid.MustParse("018fee23-4a51-7303-a441-73faed7d9deb")),
		HeartbeatInterval: &interval,
		Capabilities: protobufs.AgentCapabilities_AgentCapabilities_AcceptsPackages |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsPackageStatuses |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat,
	}))
	defer func() { assert.NoError(t, c.Stop(context.Background())) }()

	ts.waitMsg("the failed removal reported", func(m *protobufs.AgentToServer) bool {
		return string(m.GetPackageStatuses().GetServerProvidedAllPackagesHash()) == "none" &&
			strings.Contains(m.GetPackageStatuses().GetPackages()[""].GetErrorMessage(), "the installed collector is busy")
	})
	// Not tried again while the server keeps not offering it.
	time.Sleep(500 * time.Millisecond)
	assert.Len(t, removals, 1)
}
