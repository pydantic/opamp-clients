// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build install && (darwin || windows)

// An upgrade in place: the released package OPAMP_PKG or OPAMP_MSI installed and enrolled, then
// this build's package OPAMP_UPGRADE installed over it. The Supervisor comes back on its own, with
// the credentials it was issued, and its config is written again with this version's settings:
//
//	OPAMP_PKG=$PWD/released.pkg OPAMP_UPGRADE=$PWD/dist/....pkg go test -tags=install -run Upgrade .

package packaging

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpgradeInPlace(t *testing.T) {
	requireAdmin(t)
	upgrade := os.Getenv("OPAMP_UPGRADE")
	require.NotEmpty(t, upgrade, "OPAMP_UPGRADE names the package to upgrade to")
	for _, dir := range installedDirs {
		_, err := os.Stat(dir)
		require.ErrorIs(t, err, os.ErrNotExist, "%s is there already: run it on a fresh machine", dir)
	}

	install(t)
	srv := newIssuingServer(t)
	enrollStarted(t, srv.endpoint)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		auths, _ := srv.seen()
		assert.Contains(c, auths, "Bearer issued-token")
	}, 30*time.Second, 200*time.Millisecond)

	srv.forget()
	if runtime.GOOS == "windows" {
		t.Setenv("OPAMP_MSI", upgrade)
	} else {
		t.Setenv("OPAMP_PKG", upgrade)
	}
	install(t)
	defer uninstall(t, true)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		auths, _ := srv.seen()
		if assert.NotEmpty(c, auths) {
			assert.Equal(c, "Bearer issued-token", auths[0], "the upgraded Supervisor connects again with its issued token")
		}
	}, 60*time.Second, 200*time.Millisecond, "the Supervisor didn't come back after the upgrade:\n%s", diagnose())
	for _, name := range []string{"enroll.options", "supervisor.yaml.sha256", "supervisor.yaml.old"} {
		_, err := os.Stat(filepath.Join(enrolledConfigDir, name))
		assert.NoError(t, err, "the upgrade writes the config again, keeping the released one")
	}
	assertStartsAtBoot(t)
}
