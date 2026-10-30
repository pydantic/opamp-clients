// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build install && darwin

// macOS: the .pkg OPAMP_PKG installed with installer(8), the Supervisor run by launchd as the
// package's LaunchDaemon, its state key in the System Keychain.

package packaging

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	label   = "dev.pydantic.opamp-supervisor"
	storage = "/opt/pydantic-opamp/var"
)

var installedDirs = []string{"/opt/pydantic-opamp"}

func requireAdmin(t *testing.T) {
	require.Equal(t, 0, os.Geteuid(), "it installs a package: run it as root")
}

func install(t *testing.T) {
	t.Helper()
	pkg := os.Getenv("OPAMP_PKG")
	require.NotEmpty(t, pkg, "OPAMP_PKG names the package to install")
	run(t, "", "installer", "-pkg", pkg, "-target", "/")
}

func enroll(t *testing.T, endpoint string) {
	t.Helper()
	run(t, "enroll-token\n", "/opt/pydantic-opamp/bin/pydantic-opamp-enroll", "--endpoint", endpoint, "--plaintext", "--no-start")
}

// startService loads the LaunchDaemon, which starts the Supervisor; unloading it (bootout waits
// for the Supervisor to exit) is the returned function.
func startService(t *testing.T) func() {
	t.Helper()
	if exec.Command("launchctl", "print", "system/"+label).Run() != nil {
		run(t, "", "launchctl", "bootstrap", "system", "/Library/LaunchDaemons/"+label+".plist")
	}
	return func() {
		out, err := exec.Command("launchctl", "bootout", "system/"+label).CombinedOutput()
		require.NoError(t, err, "launchctl bootout: %s", out)
		// bootout can return while launchd is still taking the service down, and a bootstrap
		// then fails with "5: Input/output error": unloaded once launchctl no longer knows it.
		require.Eventually(t, func() bool {
			return exec.Command("launchctl", "print", "system/"+label).Run() != nil
		}, 30*time.Second, 100*time.Millisecond, "launchd still has %s after bootout", label)
		log, _ := os.ReadFile(storage + "/log/supervisor.log")
		t.Logf("Supervisor output:\n%s", log)
	}
}

func uninstall(t *testing.T, purge bool) {
	t.Helper()
	helper := "/opt/pydantic-opamp/bin/pydantic-opamp-uninstall"
	if _, err := os.Stat(helper); os.IsNotExist(err) {
		helper = "/opt/pydantic-opamp/etc/uninstall"
	}
	args := []string{}
	if purge {
		args = append(args, "--purge")
	}
	run(t, "", helper, args...)
	key := exec.Command("security", "find-generic-password", "-s", "opamp-state-key", "-a",
		"/opt/pydantic-opamp/var/opamp-state", "/Library/Keychains/System.keychain")
	err := key.Run()
	if purge {
		exit, ok := err.(*exec.ExitError)
		require.True(t, ok, "purge must remove the installation key")
		require.Equal(t, 44, exit.ExitCode())
	} else {
		require.NoError(t, err, "normal removal must keep the installation key")
	}
}

// diagnose reports what launchd has of the service and the end of the Supervisor's log.
func diagnose() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	print, _ := exec.CommandContext(ctx, "launchctl", "print", "system/"+label).CombinedOutput()
	ps, _ := exec.CommandContext(ctx, "ps", "-axo", "pid,ppid,stat,args").CombinedOutput()
	log, _ := exec.CommandContext(ctx, "tail", "-n", "200", storage+"/log/supervisor.log").CombinedOutput()
	return string(print) + "\n" + string(ps) + "\n" + string(log)
}
