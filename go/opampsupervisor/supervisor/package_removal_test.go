// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// newRemovalTestSupervisor has an installed collector executable, which isn't running.
func newRemovalTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	r := newRestartTestSupervisor(t, false)
	s := r.Supervisor
	s.config.Capabilities.AcceptsPackages = true
	s.config.Agent.Package.AgentBinary = "otelcol-contrib"
	s.commander = newHelperCommander(t, "sleep")
	s.packagedAgent = s.commander.Executable()
	require.NoError(t, os.MkdirAll(filepath.Dir(s.installedAgentPath()), 0o700))
	return s
}

// An installed executable that can't be deleted (a directory stands in for one Windows keeps
// open) is moved aside and removed: the next start deletes it and runs the configured one.
func TestARemovedCollectorThatCannotBeDeletedIsMovedAside(t *testing.T) {
	s := newRemovalTestSupervisor(t)
	installed := s.installedAgentPath()
	require.NoError(t, os.MkdirAll(filepath.Join(installed, "busy"), 0o700))

	require.NoError(t, s.removeAgentPackage())
	_, err := os.Stat(installed)
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, os.Remove(filepath.Join(removedAgentPath(installed), "busy")))
	s.config.Agent.Executable = s.packagedAgent
	s.useInstalledAgent()
	require.Equal(t, s.packagedAgent, s.config.Agent.Executable, "the removed collector doesn't run")
	_, err = os.Stat(removedAgentPath(installed))
	require.ErrorIs(t, err, os.ErrNotExist, "the next start deletes it")
}

// One that can be neither deleted nor moved aside is still installed: the removal fails, and is
// reported failed (TestACollectorThatCannotBeRemovedIsReportedFailed), so the next start runs
// it. It was reported removed, and the next start ran it all the same.
func TestARemovedCollectorThatStaysIsNotReportedRemoved(t *testing.T) {
	s := newRemovalTestSupervisor(t)
	installed := s.installedAgentPath()
	require.NoError(t, os.MkdirAll(filepath.Join(installed, "busy"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(removedAgentPath(installed), "busy"), 0o700))

	require.ErrorContains(t, s.removeAgentPackage(), "could not delete the installed collector")
	_, err := os.Stat(installed)
	require.NoError(t, err, "it is still there")
}
