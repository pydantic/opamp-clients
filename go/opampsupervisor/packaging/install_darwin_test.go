// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

//go:build install && darwin

// macOS: the .pkg OPAMP_PKG installed with installer(8), the Supervisor run by launchd as the
// package's LaunchDaemon, its state key in the System Keychain.

package packaging

import (
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
	run(t, "", "launchctl", "bootstrap", "system", "/Library/LaunchDaemons/"+label+".plist")
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
