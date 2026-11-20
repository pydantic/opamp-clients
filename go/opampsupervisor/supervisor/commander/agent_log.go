// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package commander

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	"go.uber.org/zap"
)

// The Collector's log file (agent.log) is held to a size, and the logs of earlier runs are kept:
//
//   - a start moves the log of the run before it to agent.log.1, and that one's to agent.log.2,
//     so the log of a Collector that failed is still there after the next one started (a
//     Supervisor start runs the Collector twice: once to ask what it is, then for good);
//   - while the Collector runs, a log that passes agentLogMaxBytes is copied to agent.log.1 and
//     emptied. The Collector keeps its handle, which appends (openAgentLogFile), so nothing is
//     restarted; the lines it writes between the copy and the emptying are lost.
//
// So the three files hold at most three times agentLogMaxBytes, plus what is written between two
// checks.
const agentLogMaxBytes = 10 << 20

// agentLogCheckInterval is how often a running Collector's log is measured.
const agentLogCheckInterval = time.Minute

// agentLogCap is how a Commander holds its Collector's log to a size: the zero value is the
// limit, the interval and the check above. Its tests set another interval and check.
type agentLogCap struct {
	interval time.Duration
	roll     func(path string, limit int64) (bool, error)
}

// agentLogKept is how many earlier logs are kept beside the current one.
const agentLogKept = 2

func keptAgentLog(path string, n int) string { return path + "." + string(rune('0'+n)) }

// shiftAgentLogs makes room for a new agent.log.1: each kept log moves up one, the oldest goes.
func shiftAgentLogs(path string) error {
	for n := agentLogKept; n > 1; n-- {
		if err := os.Rename(keptAgentLog(path, n-1), keptAgentLog(path, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// keepPreviousAgentLog moves the last run's log out of the way of a new run, which starts its own
// empty. A log that can't be kept is no reason not to start the Collector: the error is the
// caller's to log.
func keepPreviousAgentLog(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && info.Size() == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := shiftAgentLogs(path); err != nil {
		return err
	}
	return os.Rename(path, keptAgentLog(path, 1))
}

// rollAgentLog copies a log that has passed limit bytes to agent.log.1 and empties it; it says
// whether it did.
func rollAgentLog(path string, limit int64) (bool, error) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= limit {
		return false, err
	}
	if err := shiftAgentLogs(path); err != nil {
		return false, err
	}
	src, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer src.Close()
	dst, err := os.OpenFile(keptAgentLog(path, 1), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return false, err
	}
	if err := dst.Close(); err != nil {
		return false, err
	}
	return true, os.Truncate(path, 0)
}

// run holds the log of a running Collector to limit bytes, measuring it every interval until
// done is closed.
func (a agentLogCap) run(logger *zap.Logger, path string, limit int64, done <-chan struct{}) {
	interval, roll := a.interval, a.roll
	if interval <= 0 {
		interval = agentLogCheckInterval
	}
	if roll == nil {
		roll = rollAgentLog
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failed := false
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		rolled, err := roll(path, limit)
		switch {
		case err != nil && !failed:
			// Once: a disk that is full, say, would fail at every check.
			logger.Warn("Could not hold the Collector's log to its size limit", zap.String("path", path), zap.Error(err))
		case rolled:
			logger.Info("The Collector's log passed its size limit and starts again; the log so far is kept",
				zap.String("path", path), zap.String("kept", keptAgentLog(path, 1)))
		}
		failed = err != nil
	}
}

// watchAndCapLog watches a started Collector to its exit, holding its log to its size meanwhile,
// and closes watchDone when both have ended. Start waits for watchDone before it touches the log
// files: a check still copying and emptying the last run's log as the Collector exited would
// otherwise empty the next run's.
func (c *Commander) watchAndCapLog(log *os.File, watchDone chan struct{}, logCap agentLogCap) {
	defer close(watchDone)
	defer log.Close()
	exited, capped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(capped)
		logCap.run(c.logger, c.logFilePath, agentLogMaxBytes, exited)
	}()
	c.watch(exited)
	<-capped
}
