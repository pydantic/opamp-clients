// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build e2e

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server/types"
	"github.com/stretchr/testify/require"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/telemetry"
)

// packageTest is an OpAMP server that offers collector executables, from a file server of its own.
type packageTest struct {
	server  *testingOpAMPServer
	files   *httptest.Server
	served  sync.Map // path -> []byte
	status  atomic.Pointer[protobufs.PackageStatus]
	health  atomic.Pointer[protobufs.ComponentHealth]
	storage string
}

// newPackageTest starts a Supervisor on it, stopped by the func returned or at the test's end.
func newPackageTest(t *testing.T, verifierNone bool) (*packageTest, func()) {
	t.Helper()
	pt := &packageTest{storage: t.TempDir()}
	pt.files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pt.served.Load(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body.([]byte))
	}))
	t.Cleanup(pt.files.Close)
	pt.server = newOpAMPServer(t, defaultConnectingHandler, types.ConnectionCallbacks{
		OnMessage: func(_ context.Context, _ types.Connection, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
			if s := m.GetPackageStatuses().GetPackages()[""]; s != nil {
				pt.status.Store(s)
			}
			if m.Health != nil {
				pt.health.Store(m.Health)
			}
			return &protobufs.ServerToAgent{}
		},
	})

	data := map[string]string{"url": pt.server.addr, "storage_dir": pt.storage, "log_level": "0", "log_file": filepath.Join(pt.storage, "supervisor.log")}
	if verifierNone {
		data["verifier_none"] = "true"
	}
	cfg, err := config.Load(getSupervisorConfig(t, "packages", data).Name())
	require.NoError(t, err)
	logger, err := telemetry.NewLogger(cfg.Telemetry.Logs)
	require.NoError(t, err)
	s, err := supervisor.NewSupervisor(t.Context(), logger, cfg)
	require.NoError(t, err)
	require.NoError(t, s.Start(t.Context()))
	stop := sync.OnceFunc(s.Shutdown)
	t.Cleanup(stop)
	waitForSupervisorConnection(pt.server.supervisorConnected, true)
	return pt, stop
}

// runHealthCheckConfig sends a config the collector runs, and waits for it to report healthy.
func (pt *packageTest) runHealthCheckConfig(t *testing.T) {
	t.Helper()
	cfg, hash, _ := createHealthCheckCollectorConf(t, false)
	pt.server.sendToSupervisor(&protobufs.ServerToAgent{RemoteConfig: &protobufs.AgentRemoteConfig{
		Config:     &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: cfg.Bytes()}}},
		ConfigHash: hash,
	}})
	pt.waitHealthy(t)
}

func (pt *packageTest) waitHealthy(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		h := pt.health.Load()
		return h != nil && h.Healthy
	}, 30*time.Second, 100*time.Millisecond, "the collector never reported healthy")
}

// offer offers the collector at version, the package's file being content.
func (pt *packageTest) offer(version string, content []byte, signature string) {
	path := "/collector-" + version
	pt.served.Store(path, content)
	sum := sha256.Sum256(content)
	pt.server.sendToSupervisor(&protobufs.ServerToAgent{PackagesAvailable: &protobufs.PackagesAvailable{
		AllPackagesHash: []byte("all-" + version),
		Packages: map[string]*protobufs.PackageAvailable{"": {
			Type:    protobufs.PackageType_PackageType_TopLevel,
			Version: version,
			Hash:    []byte("package-" + version),
			File:    &protobufs.DownloadableFile{DownloadUrl: pt.files.URL + path, ContentHash: sum[:], Signature: []byte(signature)},
		}},
	}})
}

// waitFor waits for the Supervisor's answer about the version offered.
func (pt *packageTest) waitFor(t *testing.T, version string, want protobufs.PackageStatusEnum) *protobufs.PackageStatus {
	t.Helper()
	var got *protobufs.PackageStatus
	require.Eventually(t, func() bool {
		got = pt.status.Load()
		return got != nil && got.ServerOfferedVersion == version &&
			(got.Status == protobufs.PackageStatusEnum_PackageStatusEnum_Installed || got.Status == protobufs.PackageStatusEnum_PackageStatusEnum_InstallFailed)
	}, 60*time.Second, 100*time.Millisecond, "no answer about version %s", version)
	require.Equal(t, want, got.Status, got.ErrorMessage)
	return got
}

func tarGzip(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: 2}))
	_, err := tw.Write([]byte("hi"))
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}))
	_, err = tw.Write(content)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func TestSupervisorInstallsTheCollectorTheServerOffers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the broken collector is a shell script")
	}
	collector, err := os.ReadFile(collectorExecutablePath(t))
	require.NoError(t, err)
	pt, _ := newPackageTest(t, true)
	pt.runHealthCheckConfig(t)
	installed := filepath.Join(pt.storage, "agent", "otelcol-contrib")

	// A release archive: the executable is found in it by agent::package::agent_binary.
	pt.health.Store(nil)
	pt.offer("v2", tarGzip(t, "otelcol-contrib", collector), "")
	pt.waitFor(t, "v2", protobufs.PackageStatusEnum_PackageStatusEnum_Installed)
	got, err := os.ReadFile(installed)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(collector), sha256.Sum256(got), "the archive's executable is the one installed")
	pt.waitHealthy(t)

	// One that lists its feature gates but doesn't run as a collector is put back.
	pt.offer("v3", []byte("#!/bin/sh\n[ \"$1\" = featuregate ] && echo ID && exit 0\nexit 1\n"), "")
	failed := pt.waitFor(t, "v3", protobufs.PackageStatusEnum_PackageStatusEnum_InstallFailed)
	require.Contains(t, failed.ErrorMessage, "the new collector exited with code 1")
	got, err = os.ReadFile(installed)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(collector), sha256.Sum256(got), "the previous executable is back")
	require.NoFileExists(t, installed+".previous")
	pt.waitHealthy(t)

	// One that doesn't run at all is refused before the collector is stopped.
	pt.offer("v4", []byte("not an executable"), "")
	failed = pt.waitFor(t, "v4", protobufs.PackageStatusEnum_PackageStatusEnum_InstallFailed)
	require.Contains(t, failed.ErrorMessage, "the new collector doesn't run")
	require.NoFileExists(t, installed+".new")
}

func TestSupervisorRunsTheInstalledCollectorAfterARestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the broken collector is a shell script")
	}
	collector, err := os.ReadFile(collectorExecutablePath(t))
	require.NoError(t, err)
	pt, stop := newPackageTest(t, true)
	pt.runHealthCheckConfig(t)
	pt.offer("v2", collector, "")
	pt.waitFor(t, "v2", protobufs.PackageStatusEnum_PackageStatusEnum_Installed)
	stop()
	waitForSupervisorConnection(pt.server.supervisorConnected, false)

	// The configured executable no longer runs: only the installed one can.
	installed := filepath.Join(pt.storage, "agent", "otelcol-contrib")
	require.NoError(t, os.Rename(installed, installed+".previous"), "an update interrupted after moving it aside")
	cfg, err := config.Load(getSupervisorConfig(t, "packages", map[string]string{
		"url": pt.server.addr, "storage_dir": pt.storage, "verifier_none": "true",
		"log_level": "0", "log_file": filepath.Join(pt.storage, "supervisor.log"),
	}).Name())
	require.NoError(t, err)
	cfg.Agent.Executable = filepath.Join(t.TempDir(), "broken")
	require.NoError(t, os.WriteFile(cfg.Agent.Executable, []byte("#!/bin/sh\nexit 1\n"), 0o700))
	logger, err := telemetry.NewLogger(cfg.Telemetry.Logs)
	require.NoError(t, err)
	restarted, err := supervisor.NewSupervisor(t.Context(), logger, cfg)
	require.NoError(t, err)
	pt.health.Store(nil)
	require.NoError(t, restarted.Start(t.Context()))
	defer restarted.Shutdown()
	waitForSupervisorConnection(pt.server.supervisorConnected, true)
	pt.waitHealthy(t)
	require.FileExists(t, installed)
}

func TestSupervisorChecksThePackagesSignature(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the collector is killed with pkill")
	}
	collector, err := os.ReadFile(collectorExecutablePath(t))
	require.NoError(t, err)
	// Cosign, by default: the OpenTelemetry Collector Releases' signer.
	pt, _ := newPackageTest(t, false)
	pt.runHealthCheckConfig(t)

	// Offered while a crashed collector waits to be restarted, which a refused package mustn't
	// cancel.
	require.NoError(t, exec.Command("pkill", "-KILL", "-f", pt.storage).Run())
	require.Eventually(t, func() bool {
		h := pt.health.Load()
		return h != nil && !h.Healthy
	}, 30*time.Second, 50*time.Millisecond, "the crash was never reported")
	pt.offer("v2", collector, "unsigned")
	failed := pt.waitFor(t, "v2", protobufs.PackageStatusEnum_PackageStatusEnum_InstallFailed)
	require.Contains(t, failed.ErrorMessage, "base64 certificate and signature")
	require.NoFileExists(t, filepath.Join(pt.storage, "agent", "otelcol-contrib"))
	pt.waitHealthy(t)
}
