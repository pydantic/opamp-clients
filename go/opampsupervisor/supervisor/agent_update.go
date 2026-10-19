// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/archive"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/verifier"
)

// agentUpdateDir is where installed collector executables live, under the storage directory:
// the Supervisor never writes over the configured executable, which a package manager owns
// and a hardened service can't write.
const agentUpdateDir = "agent"

// defaultUpdateHealthTimeout is agent.package.health_timeout when it isn't set.
const defaultUpdateHealthTimeout = 30 * time.Second

// runCheckTimeout bounds running a new executable's featuregate command before it is installed.
const runCheckTimeout = 30 * time.Second

// agentInstall is the core's request to install a package, answered with done (nil: installed),
// or with remove to remove the collector the server installed.
type agentInstall struct {
	name   string
	pkg    *protobufs.PackageAvailable
	path   string
	remove bool
	done   func(error)
}

// installedAgentPath is the installed executable; previousAgentPath holds the one it replaced
// while the new one proves itself, and stagedAgentPath the new one before it is put in place.
func (s *Supervisor) installedAgentPath() string {
	return filepath.Join(s.config.Storage.Directory, agentUpdateDir, filepath.Base(s.config.Agent.Package.AgentBinary))
}

func (s *Supervisor) previousAgentPath() string { return s.installedAgentPath() + ".previous" }
func (s *Supervisor) stagedAgentPath() string   { return s.installedAgentPath() + ".new" }

// unprovenAgentPath marks an update under way, from just before the new executable is put in
// place until it has proved healthy: a start that finds it undoes the update. It holds
// unprovenReplaced when the installed executable was moved to previousAgentPath, and is empty
// when the configured one ran before.
func (s *Supervisor) unprovenAgentPath() string { return s.installedAgentPath() + ".unproven" }

const unprovenReplaced = "replaced"

// unansweredError is an install or rollback whose outcome the core isn't told: its record of
// the package stays uncertain, and it asks again at the next start, which first undoes an
// update left unproven.
type unansweredError struct{ error }

func (e unansweredError) Unwrap() error { return e.error }

// unanswered says whether the core is to be told nothing of err.
func unanswered(err error) bool {
	var u unansweredError
	return errors.As(err, &u)
}

var errShuttingDown = unansweredError{errors.New("the Supervisor is shutting down")}

// packagedStampPath records the packaged collector as it was when the installed one went in, so
// that a start can tell its operator the package manager changed it since.
func (s *Supervisor) packagedStampPath() string { return s.installedAgentPath() + ".packaged" }

// packagedStamp is the packaged collector's size and modification time; "" if it can't be read.
func (s *Supervisor) packagedStamp() string {
	info, err := os.Stat(s.packagedAgent)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d %d", info.Size(), info.ModTime().UnixNano())
}

// useInstalledAgent runs the executable an earlier update installed, if any, in place of the
// configured one: while the server offers a collector, it decides which one runs, and the
// configured one is the fallback. Changed here: an update interrupted before the new executable
// proved healthy is undone, and the executable it replaced runs; if it can't be undone, the
// configured one runs. Only an update interrupted before the new executable was in place was
// undone, so the unproven one ran.
func (s *Supervisor) useInstalledAgent() {
	if !s.config.Capabilities.AcceptsPackages {
		return
	}
	s.packagedAgent = s.config.Agent.Executable
	_ = os.Remove(s.stagedAgentPath())
	installed := s.installedAgentPath()
	if err := os.Remove(removedAgentPath(installed)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.telemetrySettings.Logger.Warn("Could not delete the collector executable the OpAMP server removed", zap.Error(err))
	}
	if undone, err := s.undoUnprovenUpdate(); err != nil {
		s.telemetrySettings.Logger.Error("Could not undo an update interrupted before the new collector proved healthy: the configured collector runs", zap.Error(err))
		return
	} else if undone {
		s.telemetrySettings.Logger.Warn("Undid an update interrupted before the new collector proved healthy")
	}
	if _, err := os.Stat(installed); err == nil {
		s.telemetrySettings.Logger.Info("Running the collector executable the OpAMP server installed", zap.String("executable", installed))
		if stamp, err := os.ReadFile(s.packagedStampPath()); err == nil && string(stamp) != s.packagedStamp() {
			s.telemetrySettings.Logger.Warn("The configured collector executable changed since the OpAMP server installed one; "+
				"the server's runs until it stops offering it",
				zap.String("configured", s.packagedAgent), zap.String("installed", installed))
		}
		s.config.Agent.Executable = installed
	}
}

// undoUnprovenUpdate puts back what an update left unproven had replaced: the executable it moved
// aside, or none, so that the configured one runs. A previous executable without the mark is one
// a proven update hadn't deleted yet.
func (s *Supervisor) undoUnprovenUpdate() (bool, error) {
	mark, err := os.ReadFile(s.unprovenAgentPath())
	if errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(s.previousAgentPath())
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(mark) == unprovenReplaced {
		// Not moved aside yet if it isn't there: the installed one is still the one it was.
		if err := os.Rename(s.previousAgentPath(), s.installedAgentPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	} else if err := removeExecutable(s.installedAgentPath()); err != nil {
		return false, err
	}
	return true, os.Remove(s.unprovenAgentPath())
}

// putUpdateInPlace puts the staged executable in place, after marking the update unproven:
// replaced says whether the installed one runs, and is moved aside.
func (s *Supervisor) putUpdateInPlace(replaced bool) error {
	mark := ""
	if replaced {
		mark = unprovenReplaced
	}
	if err := writeFileSynced(s.unprovenAgentPath(), []byte(mark)); err != nil {
		return fmt.Errorf("record the update: %w", err)
	}
	installed := s.installedAgentPath()
	if replaced {
		if err := os.Rename(installed, s.previousAgentPath()); err != nil {
			_ = os.Remove(s.unprovenAgentPath())
			return fmt.Errorf("move the installed executable aside: %w", err)
		}
	}
	if err := os.Rename(s.stagedAgentPath(), installed); err != nil {
		return fmt.Errorf("put the new executable in place: %w", err)
	}
	return nil
}

// writeFileSynced writes path and its directory entry to disk before it returns.
func writeFileSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Windows can't open a directory to sync it, and its renames and creates are journaled.
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// newPackageVerifier is the configured verifier; Cosign's trusted roots are cached in the
// storage directory.
func (s *Supervisor) newPackageVerifier() (verifier.Verifier, error) {
	cfg := s.config.Agent.Package.Verifier
	if cfg.Type == config.VerifierTypeNone {
		s.telemetrySettings.Logger.Warn("agent::package::verifier::type is none: packages from the OpAMP server are installed without checking their signatures")
	}
	return verifier.NewVerifier(cfg, filepath.Join(s.config.Storage.Directory, "sigstore"), &http.Client{})
}

// onInstallPackage takes the core's request to the agent loop, which owns the agent process.
func (s *Supervisor) onInstallPackage(req agentInstall) {
	select {
	case s.agentInstalls <- req:
	case <-s.doneChan:
		req.done(errShuttingDown)
	}
}

// installAgentPackage installs a new collector executable from the agent loop: it checks the
// signature, unpacks it and runs its featuregate command, then stops the collector, puts the new
// executable in place and starts it. If it doesn't report healthy within the health timeout,
// or exits first, the previous executable goes back and is started again.
func (s *Supervisor) installAgentPackage(req agentInstall) error {
	if req.name != "" || req.pkg.GetType() != protobufs.PackageType_PackageType_TopLevel {
		return fmt.Errorf("the Supervisor installs only the collector itself: a top-level package named \"\", not %q", req.name)
	}
	logger := s.telemetrySettings.Logger.With(zap.String("version", req.pkg.GetVersion()))
	logger.Info("Installing a new collector executable")

	content, err := os.ReadFile(req.path)
	if err != nil {
		return fmt.Errorf("read the downloaded package: %w", err)
	}
	if err := s.packageVerifier.Verify(s.runCtx, content, []byte(req.pkg.GetFile().GetSignature())); err != nil {
		return err
	}

	format := archive.FormatNone
	switch {
	case bytes.HasPrefix(content, []byte{0x1f, 0x8b}):
		format = archive.FormatTarGzip
	case bytes.HasPrefix(content, []byte("PK\x03\x04")):
		format = archive.FormatZip
	}
	extractor, err := archive.NewExtractor(format)
	if err != nil {
		return err
	}
	staged := s.stagedAgentPath()
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		return err
	}
	if err := extractor.Extract(s.runCtx, content, s.config.Agent.Package.AgentBinary, staged); err != nil {
		return fmt.Errorf("unpack the package: %w", err)
	}
	defer os.Remove(staged)
	if err := s.checkAgentRuns(staged); err != nil {
		return err
	}

	running := s.commander.Executable()
	installed := s.installedAgentPath()
	if err := s.commander.Stop(s.runCtx); err != nil {
		return fmt.Errorf("stop the collector: %w", err)
	}
	s.drainAgentExit()
	// Moved aside, not copied: the previous executable is installed's own, or the configured one.
	replaced := running == installed
	if err := s.putUpdateInPlace(replaced); err != nil {
		if _, statErr := os.Stat(s.unprovenAgentPath()); statErr != nil {
			return errors.Join(err, s.restartAgentAfterUpdate(logger))
		}
		return errors.Join(err, s.revertAgentUpdate(running, replaced, logger))
	}
	// The commander's, not s.config's: other goroutines read the config.
	s.commander.SetExecutable(installed)

	if err := s.waitForUpdatedAgent(); err != nil {
		if errors.Is(err, errShuttingDown) {
			// The next start undoes it, and the core asks for it again.
			return err
		}
		return errors.Join(err, s.revertAgentUpdate(running, replaced, logger))
	}
	// Proven: the mark goes first, so that a start never undoes it.
	// If it can't go, the next start undoes the update: the core isn't told it is installed,
	// and the executable it replaced stays the rollback target.
	if err := os.Remove(s.unprovenAgentPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return unansweredError{fmt.Errorf("record the update as proven: %w", err)}
	}
	_ = os.Remove(s.previousAgentPath())
	if err := os.WriteFile(s.packagedStampPath(), []byte(s.packagedStamp()), 0o600); err != nil {
		logger.Warn("Could not record the configured collector executable", zap.Error(err))
	}
	logger.Info("Installed the new collector executable")
	return nil
}

// removeAgentPackage goes back to the configured collector when the server stops offering one:
// the installed executable is deleted, and if it was running, the configured one is started in
// its place. A configured collector that won't start is logged: the installed one is gone
// either way, so the core is told it is removed. Changed here: an installed executable that
// can't be deleted or moved aside is an error, and the core isn't told it is removed: it stays
// reported, and runs, until a start can delete it. It was reported removed, and the next start
// ran it.
func (s *Supervisor) removeAgentPackage() error {
	installed := s.installedAgentPath()
	logger := s.telemetrySettings.Logger.With(zap.String("executable", s.packagedAgent))
	running := s.commander.Executable() == installed
	if running {
		logger.Info("The OpAMP server no longer offers a collector: going back to the configured one")
		if err := s.commander.Stop(s.runCtx); err != nil {
			logger.Error("Could not stop the installed collector", zap.Error(err))
		}
		s.drainAgentExit()
	}
	if err := removeExecutable(installed); err != nil {
		logger.Error("Could not delete the installed collector: it stays installed", zap.Error(err))
		if running {
			_ = s.restartAgentAfterUpdate(logger)
		}
		return fmt.Errorf("could not delete the installed collector: %w", err)
	}
	for _, p := range []string{s.previousAgentPath(), s.unprovenAgentPath(), s.packagedStampPath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Error("Could not delete the installed collector", zap.String("path", p), zap.Error(err))
		}
	}
	if running {
		s.commander.SetExecutable(s.packagedAgent)
		_ = s.restartAgentAfterUpdate(logger)
	}
	return nil
}

// removedAgentPath is where an installed executable that couldn't be deleted is moved, to be
// deleted at the next start. Nothing runs it.
func removedAgentPath(installed string) string { return installed + ".removed" }

// removeExecutable deletes path, or moves it aside to be deleted at the next start. Windows
// refuses to delete an executable for a little while after its process exits: that is tried
// again.
func removeExecutable(path string) error {
	err := os.Remove(path)
	for i := 0; err != nil && !errors.Is(err, os.ErrNotExist) && runtime.GOOS == "windows" && i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		err = os.Remove(path)
	}
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if renameErr := os.Rename(path, removedAgentPath(path)); renameErr != nil {
		return errors.Join(err, renameErr)
	}
	return nil
}

// checkAgentRuns runs a new executable's featuregate command, which the Supervisor runs at each
// start too: one that can't run here (another platform's, or not a collector) is refused
// before the running collector is stopped.
func (s *Supervisor) checkAgentRuns(executable string) error {
	ctx, cancel := context.WithTimeout(s.runCtx, runCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "featuregate") // #nosec G204 -- the verified package's executable
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("the new collector doesn't run (%s featuregate): %w: %s", filepath.Base(executable), err, bytes.TrimSpace(out))
	}
	return nil
}

// waitForUpdatedAgent starts the collector and waits for it to report healthy. Changed here:
// with no config to run, it proves itself on the config the bootstrap runs, and is stopped
// again. It was installed unchecked.
func (s *Supervisor) waitForUpdatedAgent() error {
	s.lastHealthFromClient.Store(nil)
	s.resetAgentReady()
	status, err := s.startAgent()
	if err != nil {
		return err
	}
	if status == agentNotStarting {
		if err := s.startAgentOnNoopConfig(); err != nil {
			return err
		}
		defer func() {
			if err := s.commander.Stop(s.runCtx); err != nil {
				s.telemetrySettings.Logger.Error("Could not stop the new collector after its check", zap.Error(err))
			}
			s.drainAgentExit()
		}()
	}
	timeout := s.config.Agent.Package.HealthTimeout
	if timeout <= 0 {
		timeout = defaultUpdateHealthTimeout
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-poll.C:
			if h := s.lastHealthFromClient.Load(); h != nil && h.Healthy {
				return nil
			}
		case <-s.commander.Exited():
			return fmt.Errorf("the new collector exited with code %d", s.commander.ExitCode())
		case <-deadline.C:
			return fmt.Errorf("the new collector didn't report healthy within %s", timeout)
		case <-s.doneChan:
			return errShuttingDown
		}
	}
}

// startAgentOnNoopConfig starts the collector on the config the bootstrap runs.
func (s *Supervisor) startAgentOnNoopConfig() error {
	cfg, err := s.composeNoopConfig()
	if err != nil {
		return fmt.Errorf("compose a config to check the new collector on: %w", err)
	}
	if err := os.WriteFile(s.agentConfigFilePath(), cfg, 0o600); err != nil {
		return fmt.Errorf("write a config to check the new collector on: %w", err)
	}
	s.resetPassthroughLogBuffer()
	return s.commander.Start(s.runCtx)
}

// revertAgentUpdate puts back the executable that ran before the update and starts it. Changed
// here: if it can't be put back, the configured one is started, the core isn't told (it would
// record the previous version as in place), and the next start tries again. No collector ran.
func (s *Supervisor) revertAgentUpdate(running string, replaced bool, logger *zap.Logger) error {
	logger.Warn("Putting the previous collector executable back")
	if err := s.commander.Stop(s.runCtx); err != nil {
		logger.Error("Could not stop the new collector", zap.Error(err))
	}
	s.drainAgentExit()
	if _, err := s.undoUnprovenUpdate(); err != nil {
		logger.Error("Could not put the previous collector executable back: the configured one runs", zap.Error(err))
		s.commander.SetExecutable(s.packagedAgent)
		return unansweredError{errors.Join(fmt.Errorf("put the previous executable back: %w", err), s.restartAgentAfterUpdate(logger))}
	}
	s.commander.SetExecutable(running)
	return s.restartAgentAfterUpdate(logger)
}

// restartAgentAfterUpdate starts the collector after an update that didn't go ahead.
func (s *Supervisor) restartAgentAfterUpdate(logger *zap.Logger) error {
	if _, err := s.startAgent(); err != nil {
		logger.Error("Could not start the collector after the update", zap.Error(err))
		return err
	}
	return nil
}

// drainAgentExit takes the exit a stop announced: the agent loop would take it for a crash
// when the collector isn't started again (no config to run). Starting it drains it too.
func (s *Supervisor) drainAgentExit() {
	select {
	case <-s.commander.Exited():
	default:
	}
}
