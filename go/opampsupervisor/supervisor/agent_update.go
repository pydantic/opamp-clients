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
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/archive"
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

// agentInstall is the core's request to install a package, answered with done (nil: installed).
type agentInstall struct {
	name string
	pkg  *protobufs.PackageAvailable
	path string
	done func(error)
}

// installedAgentPath is the installed executable; previousAgentPath holds the one it replaced
// while the new one proves itself, and stagedAgentPath the new one before it is put in place.
func (s *Supervisor) installedAgentPath() string {
	return filepath.Join(s.config.Storage.Directory, agentUpdateDir, filepath.Base(s.config.Agent.Package.AgentBinary))
}

func (s *Supervisor) previousAgentPath() string { return s.installedAgentPath() + ".previous" }
func (s *Supervisor) stagedAgentPath() string   { return s.installedAgentPath() + ".new" }

// useInstalledAgent runs the executable an earlier update installed, if any, in place of the
// configured one. An update interrupted between moving the installed executable aside and
// putting the new one in its place is undone: the one moved aside goes back.
func (s *Supervisor) useInstalledAgent() {
	if !s.config.Capabilities.AcceptsPackages {
		return
	}
	_ = os.Remove(s.stagedAgentPath())
	installed := s.installedAgentPath()
	if _, err := os.Stat(installed); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(s.previousAgentPath(), installed); err == nil {
			s.telemetrySettings.Logger.Warn("Put back the collector executable an interrupted update had moved aside")
		}
	}
	if _, err := os.Stat(installed); err == nil {
		s.telemetrySettings.Logger.Info("Running the collector executable the OpAMP server installed", zap.String("executable", installed))
		s.config.Agent.Executable = installed
	}
}

// newPackageVerifier is the configured verifier; Cosign's trusted roots are cached in the
// storage directory.
func (s *Supervisor) newPackageVerifier() (verifier.Verifier, error) {
	cfg := s.config.Agent.Package.Verifier
	if cfg.Type == "" {
		s.telemetrySettings.Logger.Warn("agent::package::verifier::type is empty: packages from the OpAMP server are installed without checking their signatures")
	}
	return verifier.NewVerifier(cfg, filepath.Join(s.config.Storage.Directory, "sigstore"), &http.Client{})
}

// onInstallPackage takes the core's request to the agent loop, which owns the agent process.
func (s *Supervisor) onInstallPackage(req agentInstall) {
	select {
	case s.agentInstalls <- req:
	case <-s.doneChan:
		req.done(errors.New("the Supervisor is shutting down"))
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
	if bytes.HasPrefix(content, []byte{0x1f, 0x8b}) {
		format = archive.FormatTarGzip
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
	if replaced {
		if err := os.Rename(installed, s.previousAgentPath()); err != nil {
			return errors.Join(fmt.Errorf("move the installed executable aside: %w", err), s.restartAgentAfterUpdate(logger))
		}
	}
	if err := os.Rename(staged, installed); err != nil {
		return errors.Join(fmt.Errorf("put the new executable in place: %w", err), s.revertAgentUpdate(running, replaced, logger))
	}
	// The commander's, not s.config's: other goroutines read the config.
	s.commander.SetExecutable(installed)

	if err := s.waitForUpdatedAgent(); err != nil {
		return errors.Join(err, s.revertAgentUpdate(running, replaced, logger))
	}
	_ = os.Remove(s.previousAgentPath())
	logger.Info("Installed the new collector executable")
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

// waitForUpdatedAgent starts the collector and waits for it to report healthy. With no config
// to run, the collector isn't started and there is nothing to wait for.
func (s *Supervisor) waitForUpdatedAgent() error {
	s.lastHealthFromClient.Store(nil)
	s.resetAgentReady()
	status, err := s.startAgent()
	if err != nil {
		return err
	}
	if status == agentNotStarting {
		return nil
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
			return errors.New("the Supervisor is shutting down")
		}
	}
}

// revertAgentUpdate puts back the executable that ran before the update and starts it.
func (s *Supervisor) revertAgentUpdate(running string, replaced bool, logger *zap.Logger) error {
	logger.Warn("Putting the previous collector executable back")
	if err := s.commander.Stop(s.runCtx); err != nil {
		logger.Error("Could not stop the new collector", zap.Error(err))
	}
	s.drainAgentExit()
	var err error
	if replaced {
		err = os.Rename(s.previousAgentPath(), s.installedAgentPath())
	} else {
		err = os.Remove(s.installedAgentPath())
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("put the previous executable back: %w", err)
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
