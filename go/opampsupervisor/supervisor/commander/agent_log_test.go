// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package commander

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// A start keeps the logs of the two runs before it, newest first, and drops the one before those.
func TestKeepPreviousAgentLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	require.NoError(t, keepPreviousAgentLog(path), "no log yet")

	for _, run := range []string{"first", "second", "third"} {
		require.NoError(t, os.WriteFile(path, []byte(run), 0o644))
		require.NoError(t, keepPreviousAgentLog(path))
		assert.NoFileExists(t, path)
	}
	assert.Equal(t, "third", readLog(t, path+".1"))
	assert.Equal(t, "second", readLog(t, path+".2"))
	assert.NoFileExists(t, path+".3")

	// An empty log (a Collector that wrote nothing) doesn't push a kept one out.
	require.NoError(t, os.WriteFile(path, nil, 0o644))
	require.NoError(t, keepPreviousAgentLog(path))
	assert.Equal(t, "third", readLog(t, path+".1"))
}

// A log over the limit is copied to agent.log.1 and emptied, and the handle the Collector
// holds goes on writing at the start of the emptied file. One within the limit is left alone.
func TestRollAgentLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	require.NoError(t, os.WriteFile(path+".1", []byte("the run before"), 0o644))
	f, err := openAgentLogFile(path)
	require.NoError(t, err)
	defer f.Close()

	_, err = f.WriteString("0123456789")
	require.NoError(t, err)
	rolled, err := rollAgentLog(path, 10)
	require.NoError(t, err)
	assert.False(t, rolled, "at the limit, not over it")
	assert.Equal(t, "the run before", readLog(t, path+".1"))

	_, err = f.WriteString("x")
	require.NoError(t, err)
	rolled, err = rollAgentLog(path, 10)
	require.NoError(t, err)
	assert.True(t, rolled)
	assert.Equal(t, "0123456789x", readLog(t, path+".1"))
	assert.Equal(t, "the run before", readLog(t, path+".2"))
	assert.Empty(t, readLog(t, path))

	_, err = f.WriteString("after")
	require.NoError(t, err)
	assert.Equal(t, "after", readLog(t, path))
}

// While a Collector runs, its log is measured at each interval and rolled once over the limit,
// with a line in the Supervisor's log; the measuring ends with the run.
func TestCapAgentLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	f, err := openAgentLogFile(path)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.WriteString(strings.Repeat("a", 64))
	require.NoError(t, err)

	core, logs := observer.New(zap.InfoLevel)
	done, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		capAgentLog(zap.New(core), path, 32, 5*time.Millisecond, done)
	}()
	require.Eventually(t, func() bool {
		return logs.FilterMessageSnippet("passed its size limit").Len() == 1
	}, 5*time.Second, 5*time.Millisecond)
	assert.Len(t, readLog(t, path+".1"), 64)
	assert.Empty(t, readLog(t, path))

	close(done)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("capAgentLog went on after the run ended")
	}
}

// Through Start, with a real child: the second run's start keeps the first run's log.
func TestStartKeepsTheLastRunsLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "agent.log")
	continueFile := filepath.Join(t.TempDir(), "continue")
	require.NoError(t, os.WriteFile(continueFile, nil, 0o600))
	cmdr, err := NewCommander(zap.NewNop(), logPath, config.Agent{
		Executable: os.Args[0],
		Env: map[string]string{
			passthroughTestModeEnv:   "log-append-after-truncate",
			logAppendContinueFileEnv: continueFile,
		},
	})
	require.NoError(t, err)

	for run := range 2 {
		require.NoError(t, cmdr.Start(t.Context()))
		select {
		case <-cmdr.Exited():
		case <-time.After(10 * time.Second):
			t.Fatalf("run %d: timed out waiting for the child to exit", run)
		}
		require.NoError(t, cmdr.Stop(t.Context()))
		assert.Contains(t, readLog(t, logPath), "after rotation")
	}
	assert.Contains(t, readLog(t, logPath+".1"), "after rotation", "the first run's log")
	assert.NoFileExists(t, logPath+".2")
}

// A check that is still rolling the log when the Collector exits is waited for by the next Start:
// its emptying of agent.log would otherwise land on the new run's log.
func TestStartWaitsForALogCheckUnderWay(t *testing.T) {
	rolling, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	roll := func(string, int64) (bool, error) {
		once.Do(func() { close(rolling) })
		<-release
		return false, nil
	}

	logPath := filepath.Join(t.TempDir(), "agent.log")
	continueFile := filepath.Join(t.TempDir(), "continue")
	cmdr, err := NewCommander(zap.NewNop(), logPath, config.Agent{
		Executable: os.Args[0],
		Env: map[string]string{
			passthroughTestModeEnv:   "log-append-after-truncate",
			logAppendContinueFileEnv: continueFile,
		},
	})
	require.NoError(t, err)
	cmdr.logCap = agentLogCap{interval: 5 * time.Millisecond, roll: roll}
	require.NoError(t, cmdr.Start(t.Context()))
	<-rolling
	// The Collector exits while the check is under way.
	require.NoError(t, os.WriteFile(continueFile, nil, 0o600))
	select {
	case <-cmdr.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the child to exit")
	}
	select {
	case <-cmdr.watchDone:
		t.Fatal("the run was done with a log check still under way")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-cmdr.watchDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never ended after the check did")
	}
}
