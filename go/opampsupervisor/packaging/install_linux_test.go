// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

//go:build install && linux

// Linux: the package comes from the apt repository OPAMP_REPO (repo.sh's, somewhere apt's _apt
// user can read), installed the way the README says. Without systemd running, the test does
// what the unit asks of systemd itself: decrypt the two credentials into /run/credentials/<unit>/
// and start the Supervisor as the service's user.

package packaging

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	unit        = "pydantic-opamp-supervisor.service"
	credentials = "/run/credentials/" + unit
	storage     = "/var/lib/pydantic-opamp"
)

// installedDirs are what the package and enrolling make: a fresh machine has none of them.
var installedDirs = []string{"/etc/pydantic-opamp", storage}

func requireAdmin(t *testing.T) {
	require.Equal(t, 0, os.Geteuid(), "it installs a package: run it as root")
}

// install installs the package from the apt repository OPAMP_REPO.
func install(t *testing.T) {
	t.Helper()
	repo := os.Getenv("OPAMP_REPO")
	require.NotEmpty(t, repo, "OPAMP_REPO names the repository to install from")
	require.NoError(t, os.MkdirAll("/etc/apt/keyrings", 0o755))
	run(t, "", "install", "-m", "0644", filepath.Join(repo, "pydantic-opamp.gpg"), "/etc/apt/keyrings/")
	run(t, "", "install", "-m", "0644", filepath.Join(repo, "pydantic-opamp.sources"), "/etc/apt/sources.list.d/")
	run(t, "", "apt-get", "update")
	run(t, "", "apt-get", "install", "--yes", "--no-install-recommends", "pydantic-opamp-supervisor")
}

func enroll(t *testing.T, endpoint string) {
	t.Helper()
	run(t, "enroll-token\n", "pydantic-opamp-enroll", "--endpoint", endpoint, "--plaintext", "--no-start")
}

// startService does systemd's part of the unit, then starts the Supervisor; stopping is the
// returned function.
func startService(t *testing.T) func() {
	t.Helper()
	require.NoError(t, os.MkdirAll(credentials, 0o700))
	for _, name := range []string{"opamp-enroll-token", "opamp-state-key"} {
		cred := "/etc/pydantic-opamp/" + strings.TrimPrefix(name, "opamp-") + ".cred"
		run(t, "", "systemd-creds", "decrypt", "--name="+name, cred, filepath.Join(credentials, name))
	}
	require.NoError(t, os.MkdirAll(storage, 0o700))
	run(t, "", "chown", "-R", "pydantic-opamp:pydantic-opamp", credentials, storage)

	var out bytes.Buffer
	cmd := exec.Command("runuser", "-u", "pydantic-opamp", "--", "env", "CREDENTIALS_DIRECTORY="+credentials,
		"/usr/lib/pydantic-opamp/opampsupervisor", "--config", "/etc/pydantic-opamp/supervisor.yaml")
	cmd.Dir = storage
	cmd.Stdout, cmd.Stderr = &out, &out
	require.NoError(t, cmd.Start())
	return func() {
		_ = exec.Command("pkill", "-TERM", "-u", "pydantic-opamp").Run()
		_ = cmd.Wait()
		t.Logf("Supervisor output:\n%s", out.String())
	}
}
