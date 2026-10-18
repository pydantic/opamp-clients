//go:build install

// The installed package, end to end: install it, enroll with a server that issues a token, and
// check the restarted Supervisor connects with the issued token, kept sealed on the disk. It
// changes the host (a package, a service, its files), so it runs only with -tags=install, as
// root or an administrator, on a throwaway machine:
//
//	OPAMP_REPO=/tmp/repo go test -tags=install -run Install .             # Linux, from apt
//	OPAMP_PKG=$PWD/dist/....pkg go test -tags=install -run Install .       # macOS
//	$env:OPAMP_MSI="$PWD\dist\....msi"; go test -tags=install -run Install . # Windows
//
// How each platform installs, enrolls and runs the service is in install_<os>_test.go.

package packaging

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server"
	"github.com/open-telemetry/opamp-go/server/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issuingServer accepts the enrollment token and the token it issues, issues one on an agent's
// first connection with the enrollment token, and records each connection's Authorization.
type issuingServer struct {
	endpoint string
	mu       sync.Mutex
	auths    []string
	statuses []protobufs.ConnectionSettingsStatuses
	offered  bool
}

func newIssuingServer(t *testing.T) *issuingServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	s := &issuingServer{endpoint: "ws://" + addr + "/v1/opamp"}
	srv := server.New(nil)
	require.NoError(t, srv.Start(server.StartSettings{
		ListenEndpoint: addr,
		Settings:       server.Settings{Callbacks: types.Callbacks{OnConnecting: s.connecting}},
	}))
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	return s
}

func (s *issuingServer) connecting(r *http.Request) types.ConnectionResponse {
	auth := r.Header.Get("Authorization")
	s.mu.Lock()
	s.auths = append(s.auths, auth)
	s.mu.Unlock()
	if auth != "Bearer enroll-token" && auth != "Bearer issued-token" {
		return types.ConnectionResponse{HTTPStatusCode: http.StatusUnauthorized}
	}
	return types.ConnectionResponse{Accept: true, ConnectionCallbacks: types.ConnectionCallbacks{
		OnMessage: func(_ context.Context, _ types.Connection, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
			s.mu.Lock()
			defer s.mu.Unlock()
			if st := m.GetConnectionSettingsStatus(); st != nil {
				s.statuses = append(s.statuses, st.Status)
			}
			reply := &protobufs.ServerToAgent{InstanceUid: m.InstanceUid}
			if auth == "Bearer enroll-token" && !s.offered {
				s.offered = true
				reply.ConnectionSettings = &protobufs.ConnectionSettingsOffers{
					Hash: []byte("issued"),
					Opamp: &protobufs.OpAMPConnectionSettings{
						DestinationEndpoint: s.endpoint,
						Headers:             &protobufs.Headers{Headers: []*protobufs.Header{{Key: "Authorization", Value: "Bearer issued-token"}}},
					},
				}
			}
			return reply
		},
	}}
}

func (s *issuingServer) seen() ([]string, []protobufs.ConnectionSettingsStatuses) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.auths), slices.Clone(s.statuses)
}

func (s *issuingServer) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auths, s.statuses = nil, nil
}

func run(t *testing.T, stdin string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s %v: %s", name, args, out)
}

func TestInstallEnrollRestart(t *testing.T) {
	requireAdmin(t)
	for _, dir := range installedDirs {
		_, err := os.Stat(dir)
		require.ErrorIs(t, err, os.ErrNotExist, "%s is there already: run it on a fresh machine", dir)
	}

	install(t)
	validateHostLogs(t, "")
	srv := newIssuingServer(t)
	enroll(t, srv.endpoint)

	stop := startService(t)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		auths, statuses := srv.seen()
		assert.Contains(c, auths, "Bearer issued-token")
		assert.Contains(c, statuses, protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED)
	}, 30*time.Second, 200*time.Millisecond)
	stop()

	state, err := os.ReadFile(filepath.Join(storage, "opamp-client.state"))
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(state, []byte("OPAMPSK1")), "the state file is sealed")
	assert.NotContains(t, string(state), "issued-token")

	srv.forget()
	stop = startService(t)
	defer stop()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		auths, _ := srv.seen()
		if assert.NotEmpty(c, auths) {
			assert.Equal(c, "Bearer issued-token", auths[0], "the restarted Supervisor connects with the issued token")
		}
	}, 30*time.Second, 200*time.Millisecond)
	auths, _ := srv.seen()
	assert.NotContains(t, auths, "Bearer enroll-token")
}

func TestInstallBuiltCollectorHostLogs(t *testing.T) {
	binary := "otelcol"
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	validateHostLogs(t, filepath.Join("dist", "otelcol", binary))
}

func validateHostLogs(t *testing.T, binary string) {
	t.Helper()
	collector := "/usr/lib/pydantic-opamp/otelcol"
	receivers := "filelog:\n    include: [/var/log/*.log]\n    storage: file_storage"
	logs := "filelog"
	switch runtime.GOOS {
	case "darwin":
		collector = "/opt/pydantic-opamp/bin/otelcol"
		receivers = "macos_unified_logging:\n    max_log_age: 1m\n    predicate: \"logType == 'error' OR logType == 'fault'\""
		logs = "macos_unified_logging"
	case "windows":
		collector = filepath.Join(os.Getenv("ProgramFiles"), "pydantic-opamp", "otelcol.exe")
		receivers = "windows_event_log/application:\n    channel: Application\n    storage: file_storage\n  windows_event_log/system:\n    channel: System\n    storage: file_storage"
		logs = "windows_event_log/application, windows_event_log/system"
	}
	if binary != "" {
		collector = binary
	}
	path := filepath.Join(t.TempDir(), "host.yaml")
	contents := "extensions:\n  file_storage:\n    create_directory: true\nreceivers:\n  " + receivers +
		"\n  host_metrics:\n    scrapers:\n      cpu: {}\n      memory: {}\nexporters:\n  nop: {}\nservice:\n  extensions: [file_storage]\n  pipelines:\n    logs:\n      receivers: [" + logs +
		"]\n      exporters: [nop]\n    metrics:\n      receivers: [host_metrics]\n      exporters: [nop]\n"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	run(t, "", collector, "validate", "--config", path)
}
