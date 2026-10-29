// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from opentelemetry-collector-contrib's cmd/opampsupervisor. Changed here: Start waits
// for the previous run's watcher, so Stop can't take its exit for the new run's; StartOneShot
// kills a one-shot Collector that ignores the shutdown signal, and returns all of its output;
// SetExecutable switches the executable, for an installed update. The Collector doesn't inherit
// the Supervisor's OTEL_EXPORTER_OTLP_* variables (collector_env.go).

package commander

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// AgentStartedLogMsg is logged every time an Agent process is started. Each site
// tags a "start_mode" field naming the code path (normal, passthrough, one_shot)
// so the entries are self-describing. Tests count occurrences of the message to
// observe how often the Agent was (re)started, so keep the message and the log
// sites in sync.
const AgentStartedLogMsg = "Agent process started"

// Commander can start/stop/restart the Agent executable and also watch for a signal
// for the Agent process to finish.
type Commander struct {
	logger             *zap.Logger
	cfg                config.Agent
	logFilePath        string
	passthroughLogHook func(string)
	args               []string
	cmd                *exec.Cmd
	doneCh             chan struct{}
	exitCh             chan struct{}
	outputDoneCh       chan struct{}
	// watchDone is closed when the last run's watch goroutine has announced the exit on
	// exitCh and doneCh. Start waits for it, so a late token can't be taken for the new run's.
	watchDone chan struct{}
	running   *atomic.Int64
	// stopMu serializes Stop calls. The process exiting is announced once on
	// doneCh, so concurrent Stop calls would race to consume it and the loser
	// would never return.
	stopMu sync.Mutex

	minAgeForShutdownSignal time.Duration
	startedAt               time.Time
	// executable overrides cfg.Executable once set: an update the Supervisor installed.
	executable          atomic.Pointer[string]
	environmentProvider func() map[string]string
}

// SetEnvironmentProvider supplies connection credentials for each child process. Set it before Start.
func (c *Commander) SetEnvironmentProvider(provider func() map[string]string) {
	c.environmentProvider = provider
}

func (c *Commander) environment() []string {
	env := make(map[string]string, len(c.cfg.Env))
	for name, value := range c.cfg.Env {
		env[name] = value
	}
	if c.environmentProvider != nil {
		for name, value := range c.environmentProvider() {
			env[name] = value
		}
	}
	return envVarMapToEnvMapSlice(env)
}

func NewCommander(logger *zap.Logger, logFilePath string, cfg config.Agent, args ...string) (*Commander, error) {
	return &Commander{
		logger:                  logger,
		logFilePath:             logFilePath,
		cfg:                     cfg,
		args:                    args,
		outputDoneCh:            make(chan struct{}),
		running:                 &atomic.Int64{},
		minAgeForShutdownSignal: minAgentAgeForShutdownSignal,
		// Buffer channels so we can send messages without blocking on listeners.
		doneCh: make(chan struct{}, 1),
		exitCh: make(chan struct{}, 1),
	}, nil
}

// Start the Agent and begin watching the process.
// Agent's stdout and stderr are written to a file.
// Calling this method when a command is already running
// is a no-op.
func (c *Commander) Start(ctx context.Context) error {
	if c.running.Load() == 1 {
		// Already started, nothing to do
		return nil
	}
	if c.watchDone != nil {
		<-c.watchDone
	}

	// Drain channels in case there are no listeners that
	// drained messages from previous runs.
	if len(c.doneCh) > 0 {
		select {
		case <-c.doneCh:
		default:
		}
	}
	if len(c.exitCh) > 0 {
		select {
		case <-c.exitCh:
		default:
		}
	}
	c.outputDoneCh = make(chan struct{})
	c.logger.Debug("Starting agent", zap.String("agent", c.Executable()))

	args := slices.Concat(c.args, c.cfg.Arguments)

	c.cmd = exec.CommandContext(ctx, c.Executable(), args...) // #nosec G204
	c.cmd.Env = c.environment()
	c.cmd.SysProcAttr = sysProcAttrs()

	// PassthroughLogging changes how collector start up happens
	if c.cfg.PassthroughLogs {
		return c.startWithPassthroughLogging()
	}
	return c.startNormal()
}

func (c *Commander) ReloadConfigFile() error {
	if c.cmd == nil || c.cmd.Process == nil {
		return errors.New("agent process is not running")
	}

	c.logger.Debug("Sending SIGHUP to agent process to reload config", zap.Int("pid", c.cmd.Process.Pid))
	if err := c.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		return fmt.Errorf("failed to send SIGHUP to agent process: %w", err)
	}

	return nil
}

// ValidateConfig runs the collector's validate command on the specified configuration file
// to check if the configuration is valid without starting the collector.
// Returns an error if the configuration is invalid or if the validation process fails.
func (c *Commander) ValidateConfig(ctx context.Context, configPath string, additionalArgs ...string) error {
	c.logger.Debug("Validating agent config", zap.String("config", configPath))

	args := slices.Concat([]string{"validate", "--config", configPath}, additionalArgs, c.cfg.Arguments)

	cmd := exec.CommandContext(ctx, c.Executable(), args...) // #nosec G204
	cmd.Env = c.environment()

	output, err := cmd.CombinedOutput()
	if err != nil {
		c.logger.Error("Config validation failed",
			zap.String("output", string(output)),
			zap.Error(err))
		return fmt.Errorf("config validation failed: %w, output: %s", err, string(output))
	}

	c.logger.Debug("Config validation succeeded")
	return nil
}

func (c *Commander) startNormal() error {
	// Opened via openAgentLogFile (per-platform - see commander_others.go /
	// commander_windows.go) so every write lands at the file's current
	// end-of-file, and external copytruncate-style log rotation (logrotate et
	// al.) works correctly instead of the file's size reverting on the next
	// write.
	stdoutFile, err := openAgentLogFile(c.logFilePath)
	if err != nil {
		return fmt.Errorf("cannot create %s: %w", c.logFilePath, err)
	}

	// Capture standard output and standard error.
	// https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/21072
	c.cmd.Stdout = stdoutFile
	c.cmd.Stderr = stdoutFile

	if err := c.cmd.Start(); err != nil {
		stdoutFile.Close()
		return fmt.Errorf("startNormal: %w", err)
	}
	c.startedAt = time.Now()

	c.logger.Debug(AgentStartedLogMsg, zap.Int("pid", c.cmd.Process.Pid), zap.String("start_mode", "normal"))
	c.running.Store(1)
	close(c.outputDoneCh)

	watchDone := make(chan struct{})
	c.watchDone = watchDone
	go func() {
		defer stdoutFile.Close()
		c.watch(watchDone)
	}()

	return nil
}

func (c *Commander) startWithPassthroughLogging() error {
	// grab cmd pipes
	stdoutPipe, err := c.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdoutPipe: %w", err)
	}
	stderrPipe, err := c.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderrPipe: %w", err)
	}

	// start agent
	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	c.startedAt = time.Now()
	c.running.Store(1)

	colLogger := c.logger.Named("collector")
	var outputWG sync.WaitGroup

	// capture agent output
	outputWG.Go(func() {
		reader := bufio.NewReader(stdoutPipe)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					c.logger.Error("Error reading agent stdout", zap.Error(err))
				}
				// Trim and log the last line if it exists
				if line != "" {
					line = strings.TrimRight(line, "\r\n")
					colLogger.Info(line)
					c.onPassthroughLogLine(line)
				}
				break
			}
			line = strings.TrimRight(line, "\r\n")
			colLogger.Info(line)
			c.onPassthroughLogLine(line)
		}
	})
	outputWG.Go(func() {
		reader := bufio.NewReader(stderrPipe)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					c.logger.Error("Error reading agent stderr", zap.Error(err))
				}
				// Trim and log the last line if it exists
				if line != "" {
					line = strings.TrimRight(line, "\r\n")
					colLogger.Error(line)
					c.onPassthroughLogLine(line)
				}
				break
			}
			line = strings.TrimRight(line, "\r\n")
			colLogger.Error(line)
			c.onPassthroughLogLine(line)
		}
	})

	c.logger.Debug(AgentStartedLogMsg, zap.Int("pid", c.cmd.Process.Pid), zap.String("start_mode", "passthrough"))

	outputDone := c.outputDoneCh
	go func() {
		outputWG.Wait()
		close(outputDone)
	}()
	c.watchDone = make(chan struct{})
	go c.watch(c.watchDone)
	return nil
}

// watch waits for the process to exit, announces it on exitCh and then (once its output is
// drained) on doneCh, and closes watchDone.
func (c *Commander) watch(watchDone chan struct{}) {
	defer close(watchDone)

	err := c.cmd.Wait()

	// cmd.Wait returns an exec.ExitError when the Collector exits unsuccessfully or stops
	// after receiving a signal. The Commander caller will handle these cases, so we filter
	// them out here.
	var exitError *exec.ExitError
	if ok := errors.As(err, &exitError); err != nil && !ok {
		c.logger.Error("An error occurred while watching the agent process", zap.Error(err))
	}

	c.running.Store(0)
	c.exitCh <- struct{}{}
	<-c.outputDoneCh
	c.doneCh <- struct{}{}
}

// Executable is the agent's executable: the configured one, or the one SetExecutable gave.
func (c *Commander) Executable() string {
	if p := c.executable.Load(); p != nil {
		return *p
	}
	return c.cfg.Executable
}

// SetExecutable makes later starts, validations and one-shot runs use path. A running agent
// keeps running the executable it started with.
func (c *Commander) SetExecutable(path string) {
	c.executable.Store(&path)
}

// LogFilePath returns the path where the agent stdout/stderr are captured.
func (c *Commander) LogFilePath() string {
	if c.cfg.PassthroughLogs {
		return ""
	}
	return c.logFilePath
}

// SetPassthroughLogHook configures a callback invoked for each Collector log line
// when passthrough logging is enabled.
func (c *Commander) SetPassthroughLogHook(h func(string)) {
	c.passthroughLogHook = h
}

func (c *Commander) onPassthroughLogLine(line string) {
	if c.passthroughLogHook != nil {
		c.passthroughLogHook(line)
	}
}

// WaitForOutputDrain waits for passthrough log readers to finish after process exit.
func (c *Commander) WaitForOutputDrain(timeout time.Duration) bool {
	select {
	case <-c.outputDoneCh:
		return true
	case <-time.After(timeout):
		return false
	}
}

// StartOneShot starts the Collector with the expectation that it will immediately
// exit after it finishes a quick operation. This is useful for situations like reading stdout/sterr
// to e.g. check the feature gate the Collector supports.
func (c *Commander) StartOneShot() ([]byte, []byte, error) {
	stdout := []byte{}
	stderr := []byte{}
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, c.Executable(), c.args...) // #nosec G204
	cmd.Env = c.environment()
	cmd.SysProcAttr = sysProcAttrs()
	// grab cmd pipes
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdoutPipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stderrPipe: %w", err)
	}

	// start agent
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start: %w", err)
	}
	// capture agent output
	var outputWG sync.WaitGroup
	outputWG.Go(func() {
		reader := bufio.NewReader(stdoutPipe)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					c.logger.Error("Error reading agent stdout", zap.Error(err))
				}
				// Trim and append the last line if it exists
				// Normalize line endings to \n
				if line != "" {
					line = strings.TrimRight(line, "\r\n")
					stdout = append(stdout, []byte(line)...)
					stdout = append(stdout, byte('\n'))
				}
				break
			}
			// Normalize line endings to \n
			line = strings.TrimRight(line, "\r\n")
			stdout = append(stdout, []byte(line)...)
			stdout = append(stdout, byte('\n'))
		}
	})
	outputWG.Go(func() {
		reader := bufio.NewReader(stderrPipe)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					c.logger.Error("Error reading agent stderr", zap.Error(err))
				}
				// Trim and append the last line if it exists
				// Normalize line endings to \n
				if line != "" {
					line = strings.TrimRight(line, "\r\n")
					stderr = append(stderr, []byte(line)...)
					stderr = append(stderr, byte('\n'))
				}
				break
			}
			// Normalize line endings to \n
			line = strings.TrimRight(line, "\r\n")
			stderr = append(stderr, []byte(line)...)
			stderr = append(stderr, byte('\n'))
		}
	})

	c.logger.Debug(AgentStartedLogMsg, zap.Int("pid", cmd.Process.Pid), zap.String("start_mode", "one_shot"))

	// The pipes are read to the end before Wait, which closes them.
	doneCh := make(chan struct{})
	go func() {
		outputWG.Wait()
		if err := cmd.Wait(); err != nil {
			c.logger.Error("One-shot Collector encountered an error during execution", zap.Error(err))
		}
		close(doneCh)
	}()

	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	select {
	case <-doneCh:
	case <-waitCtx.Done():
		pid := cmd.Process.Pid
		c.logger.Debug("Stopping agent process", zap.Int("pid", pid))

		// Gracefully signal process to stop.
		if err := sendShutdownSignal(cmd.Process); err != nil {
			return nil, nil, err
		}

		select {
		case <-doneCh:
			c.logger.Debug("Agent process successfully stopped.", zap.Int("pid", pid))
		case <-time.After(c.cfg.StopGracePeriod):
			c.logger.Debug(
				"Agent process is not responding to SIGTERM. Sending SIGKILL to kill forcibly.",
				zap.Int("pid", pid),
			)
			if err := cmd.Process.Signal(os.Kill); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return nil, nil, fmt.Errorf("could not stop the one-shot agent process: %w", err)
			}
			<-doneCh
		}
	}

	return stdout, stderr, nil
}

// Exited returns a channel that will send a signal when the Agent process exits.
func (c *Commander) Exited() <-chan struct{} {
	return c.exitCh
}

// Pid returns Agent process PID if it is started or 0 if it is not.
func (c *Commander) Pid() int {
	if c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// ExitCode returns Agent process exit code if it exited or 0 if it is not.
func (c *Commander) ExitCode() int {
	if c.cmd == nil || c.cmd.ProcessState == nil {
		return 0
	}
	return c.cmd.ProcessState.ExitCode()
}

func (c *Commander) IsRunning() bool {
	return c.running.Load() != 0
}

// Stop the Agent process. Signals the process to stop gracefully and kills it
// forcibly if it has not exited within agent::stop_grace_period. Stop returns an
// error only when the process could not be terminated at all; a failed graceful
// shutdown alone is not an error.
//
// ctx's cancellation does not bound this call: Stop always runs to completion so
// that it terminates the process even when the caller's context is already
// cancelled, for example while the Supervisor itself is shutting down.
func (c *Commander) Stop(ctx context.Context) error {
	c.stopMu.Lock()
	defer c.stopMu.Unlock()

	if c.running.Load() == 0 {
		// Not started, nothing to do.
		return nil
	}

	pid := c.cmd.Process.Pid

	if wait := c.minAgeForShutdownSignal - time.Since(c.startedAt); wait > 0 {
		c.logger.Debug("Waiting for agent process to finish starting before sending shutdown signal",
			zap.Int("pid", pid), zap.Duration("wait", wait))
		select {
		case <-c.doneCh:
			c.running.Store(0)
			return nil
		case <-time.After(wait):
		}
	}

	c.logger.Debug("sending shutdown signal to agent process", zap.Int("pid", pid))

	// Gracefully signal process to stop. A failed send is not fatal: the process
	// still has to be terminated, so fall through to the kill deadline below
	// instead of returning with the process left running.
	if err := sendShutdownSignal(c.cmd.Process); err != nil {
		c.logger.Debug("Could not send shutdown signal to agent process",
			zap.Int("pid", pid), zap.Error(err))
	}

	// The deadline is deliberately detached from ctx's cancellation: Stop has to
	// terminate the process even when the caller's context is already cancelled,
	// for example while the Supervisor itself is shutting down.
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.StopGracePeriod)
	defer cancel()

	// Setup a goroutine to wait a while for process to finish and send kill signal
	// to the process if it doesn't finish. A kill that fails is reported over
	// killFailed so Stop can return an error instead of waiting forever for an
	// exit that will never come.
	killFailed := make(chan error, 1)
	go func() {
		<-waitCtx.Done()

		if !errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			// cancel() ran because the process exited.
			c.logger.Debug("Agent process successfully stopped.", zap.Int("pid", pid))
			return
		}

		// Time is out. Kill the process.
		c.logger.Debug(
			"Agent process is not responding to SIGTERM. Sending SIGKILL to kill forcibly.",
			zap.Int("pid", pid),
		)
		if err := c.cmd.Process.Signal(os.Kill); err != nil && !errors.Is(err, os.ErrProcessDone) {
			killFailed <- err
		}
	}()

	// Wait for process to terminate
	select {
	case <-c.doneCh:
	case err := <-killFailed:
		return fmt.Errorf("could not stop the agent process: %w", err)
	}

	c.running.Store(0)

	return nil
}

func envVarMapToEnvMapSlice(m map[string]string) []string {
	// let the command initialize the env itself
	if m == nil {
		return nil
	}
	result := inheritedEnviron() // Changed here: without OTEL_EXPORTER_OTLP_* (collector_env.go).
	for key, value := range m {
		result = append(result, key+"="+value)
	}
	return result
}
