// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"

	"github.com/pydantic/opamp-clients/go/opamp"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/verifier"
)

// An update interrupted before the new collector proved healthy (the host lost power during the
// health wait) is undone at the next start: the executable it replaced runs again, and stays the
// rollback target of the install the core asks for again. The unproven one ran, and that second
// install moved it over the good one.
func TestAnUpdateInterruptedBeforeItsHealthCheckIsUndone(t *testing.T) {
	for name, replaced := range map[string]bool{"over an installed one": true, "over the configured one": false} {
		t.Run(name, func(t *testing.T) {
			s := newRemovalTestSupervisor(t)
			installed := s.installedAgentPath()
			if replaced {
				require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o700))
			}
			require.NoError(t, os.WriteFile(s.stagedAgentPath(), []byte("v2"), 0o700))
			require.NoError(t, s.putUpdateInPlace(replaced))

			s.config.Agent.Executable = s.packagedAgent
			s.useInstalledAgent()
			if replaced {
				require.Equal(t, installed, s.config.Agent.Executable)
				got, err := os.ReadFile(installed)
				require.NoError(t, err)
				require.Equal(t, "v1", string(got), "the executable that ran before the update runs")
			} else {
				require.Equal(t, s.packagedAgent, s.config.Agent.Executable, "the configured collector runs")
				requireNoFile(t, installed)
			}
			requireNoFile(t, s.previousAgentPath())
			requireNoFile(t, s.unprovenAgentPath())
		})
	}
}

// A crash while the mark is written leaves it short: nothing had moved yet, so nothing is undone.
func TestAMarkWrittenOnlyInPartUndoesNothing(t *testing.T) {
	s := newRemovalTestSupervisor(t)
	installed := s.installedAgentPath()
	require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o700))
	require.NoError(t, os.WriteFile(s.unprovenAgentPath(), []byte("rep"), 0o600))

	s.config.Agent.Executable = s.packagedAgent
	s.useInstalledAgent()
	require.Equal(t, installed, s.config.Agent.Executable)
	requireNoFile(t, s.unprovenAgentPath())
}

// Shutting down during the health wait isn't an outcome of the install: the core isn't told, so
// its record stays uncertain and it installs again after the restart, which first undoes this
// one. The version was reported failed for good.
func TestAnUpdateCutShortByAShutdownIsNotAnswered(t *testing.T) {
	r := newRestartTestSupervisor(t, false)
	s := r.Supervisor
	s.commander = newHelperCommander(t, "sleep")
	t.Cleanup(func() { _ = s.commander.Stop(t.Context()) })
	close(s.doneChan)
	err := s.waitForUpdatedAgent()
	require.Error(t, err)
	require.True(t, unanswered(err))
}

// With no config to run, the new collector still has to prove itself, on the config the
// bootstrap runs. It was installed unchecked, and the executable it replaced was deleted.
func TestAnUpdateWithNoConfigToRunIsStillChecked(t *testing.T) {
	r := newRestartTestSupervisor(t, true)
	s := r.Supervisor
	s.commander = newHelperCommander(t, "exit-soon")
	require.ErrorContains(t, s.waitForUpdatedAgent(), "exited")
}

// A rollback that can't put the previous executable back still leaves a collector running, the
// configured one. The core is told the install failed leaving what the agent has unknown: the
// version is reported failed, and isn't recorded as in place, and the next start tries the
// rollback again before the core installs once more. The core was told nothing, so the server
// never heard the install failed.
func TestARollbackThatFailsRunsTheConfiguredCollector(t *testing.T) {
	s := newRemovalTestSupervisor(t)
	installed := s.installedAgentPath()
	require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o700))
	require.NoError(t, os.WriteFile(s.stagedAgentPath(), []byte("v2"), 0o700))
	require.NoError(t, s.putUpdateInPlace(true))
	// A directory can't be renamed over the executable.
	require.NoError(t, os.Remove(s.previousAgentPath()))
	require.NoError(t, os.MkdirAll(filepath.Join(s.previousAgentPath(), "busy"), 0o700))
	t.Cleanup(func() { _ = s.commander.Stop(t.Context()) })

	err := s.revertAgentUpdate(installed, true, s.telemetrySettings.Logger)
	require.Error(t, err)
	require.False(t, unanswered(err))
	require.True(t, opamp.IsFailedLeavingUnknown(err), "%v", err)
	require.Equal(t, s.packagedAgent, s.commander.Executable())
	require.FileExists(t, s.unprovenAgentPath(), "the next start tries again")
}

// A start that can't undo an unproven update leaves its mark, and the configured collector runs.
// The core asks for the install again; it isn't done, and the core isn't told, until a start has
// undone the update. It was done over the configured collector: the mark was rewritten "fresh",
// the new executable renamed over the installed one, and undoing it deleted that one.
func TestNoInstallWhileAnUpdateAStartCouldNotUndoIsLeft(t *testing.T) {
	s := newRemovalTestSupervisor(t)
	installed := s.installedAgentPath()
	require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o700))
	require.NoError(t, os.WriteFile(s.unprovenAgentPath(), []byte(unprovenReplaced), 0o600))
	// A directory can't be renamed over the executable: the undo fails.
	require.NoError(t, os.MkdirAll(filepath.Join(s.previousAgentPath(), "busy"), 0o700))

	s.config.Agent.Executable = s.packagedAgent
	s.useInstalledAgent()
	require.Equal(t, s.packagedAgent, s.config.Agent.Executable, "the configured collector runs")
	require.FileExists(t, s.unprovenAgentPath())

	err := s.installAgentPackage(agentInstall{
		pkg:  &protobufs.PackageAvailable{Type: protobufs.PackageType_PackageType_TopLevel, Version: "v2"},
		path: filepath.Join(t.TempDir(), "package.tar.gz"),
	})
	require.Error(t, err)
	require.True(t, unanswered(err), "the core isn't told: %v", err)
	got, err := os.ReadFile(installed)
	require.NoError(t, err)
	require.Equal(t, "v1", string(got), "the installed executable is kept")
	mark, err := os.ReadFile(s.unprovenAgentPath())
	require.NoError(t, err)
	require.Equal(t, unprovenReplaced, string(mark), "the next start tries the undo again")
}

func requireNoFile(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, path)
}

// An install whose signature can't be checked for now (Sigstore unreachable) failed for now: the
// core tries it again later, and doesn't report the version failed. It was failed for good, and
// a Sigstore outage failed every version offered during it until the server offered another. A
// signature that doesn't verify fails for good.
func TestAnInstallSigstoreCannotCheckForNowIsTriedAgain(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		forNow bool
	}{
		"Sigstore unreachable":      {fmt.Errorf("fetch Sigstore's trusted root: %w", verifier.ErrUnavailable), true},
		"a signature not verifying": {errors.New("the package's signature doesn't verify"), false},
	} {
		t.Run(name, func(t *testing.T) {
			s := newRemovalTestSupervisor(t)
			s.packageVerifier = verifierFunc(func() error { return c.err })
			path := filepath.Join(t.TempDir(), "package")
			require.NoError(t, os.WriteFile(path, []byte("the collector"), 0o600))
			err := s.installAgentPackage(agentInstall{
				pkg:  &protobufs.PackageAvailable{Type: protobufs.PackageType_PackageType_TopLevel, Version: "v2"},
				path: path,
			})
			require.ErrorIs(t, err, c.err)
			require.False(t, unanswered(err))
			require.Equal(t, c.forNow, opamp.IsFailedForNow(err), "%v", err)
			require.Equal(t, s.packagedAgent, s.commander.Executable(), "the running collector wasn't touched")
		})
	}
}

// verifierFunc is a package verifier answering with verify's error.
type verifierFunc func() error

func (f verifierFunc) Verify(context.Context, []byte, []byte) error { return f() }
func (verifierFunc) Type() string                                   { return "test" }
