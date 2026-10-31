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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commandTimeout bounds each command these tests run. A command stuck past it (a service that
// doesn't stop) fails the test with what the machine shows then (diagnose), instead of hanging
// until the CI job is cancelled with no output at all.
const commandTimeout = 3 * time.Minute

func run(t *testing.T, stdin string, name string, args ...string) {
	t.Helper()
	out, err := timed(t, stdin, name, args...)
	require.NoError(t, err, "%s %v: %s", name, args, out)
}

// timed runs a command bounded by commandTimeout, and on a timeout logs diagnose's report.
func timed(t *testing.T, stdin string, name string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	// A child the killed command left holding its output must not keep the read open.
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Logf("%s %v still running after %s; the machine now:\n%s", name, args, commandTimeout, diagnose())
		return out, fmt.Errorf("timed out after %s: %w", commandTimeout, err)
	}
	return out, err
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
	// WIP repro stages (removed before review).
	if os.Getenv("REPRO") == "noconnect" {
		enroll(t, "ws://127.0.0.1:1/v1/opamp")
		for i := 0; i < 3; i++ {
			stop := startService(t)
			time.Sleep(10 * time.Second)
			stop()
		}
		return
	}
	enroll(t, srv.endpoint)

	stop := startService(t)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		auths, statuses := srv.seen()
		assert.Contains(c, auths, "Bearer issued-token")
		assert.Contains(c, statuses, protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED)
	}, 30*time.Second, 200*time.Millisecond)
	stop()
	if os.Getenv("REPRO") == "first" {
		return
	}

	state, err := os.ReadFile(filepath.Join(storage, "opamp-state"))
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(state, []byte("OPAMPSK1")), "the state file is sealed")
	assert.NotContains(t, string(state), "issued-token")

	srv.forget()
	if os.Getenv("REPRO") == "secondslow" {
		time.Sleep(20 * time.Second)
	}
	stop = startService(t)
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		auths, _ := srv.seen()
		if assert.NotEmpty(c, auths) {
			assert.Equal(c, "Bearer issued-token", auths[0], "the restarted Supervisor connects with the issued token")
		}
	}, 30*time.Second, 200*time.Millisecond)
	auths, _ := srv.seen()
	assert.NotContains(t, auths, "Bearer enroll-token")
	if strings.HasPrefix(os.Getenv("REPRO"), "second") {
		return
	}
	// Linux's fixture runs the process itself when systemd is absent.
	if runtime.GOOS == "linux" {
		stop()
		stop = nil
	}
	retained, err := os.ReadFile(filepath.Join(storage, "opamp-state"))
	require.NoError(t, err)
	uninstall(t, false)
	stop = nil
	uninstall(t, false)
	after, err := os.ReadFile(filepath.Join(storage, "opamp-state"))
	require.NoError(t, err)
	assert.Equal(t, retained, after, "removal preserves the sealed credentials and identity")
	srv.forget()
	install(t)
	stop = startService(t)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		auths, _ := srv.seen()
		if assert.NotEmpty(c, auths) {
			assert.Equal(c, "Bearer issued-token", auths[0])
		}
	}, 30*time.Second, 200*time.Millisecond)
	if runtime.GOOS == "linux" {
		stop()
		stop = nil
	}
	uninstall(t, true)
	stop = nil
	_, err = os.Stat(storage)
	require.ErrorIs(t, err, os.ErrNotExist)
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
