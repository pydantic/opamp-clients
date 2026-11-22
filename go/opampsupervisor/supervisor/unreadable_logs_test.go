// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Collector's lines for a file it can't open, as its file receiver writes them.
const (
	deniedLine = `{"level":"error","ts":"2026-11-21T10:00:00.000Z","msg":"Failed to open file - unreadable","resource":{"service.name":"otelcol-contrib"},"otelcol.component.id":"file_log/system","otelcol.component.kind":"receiver","otelcol.signal":"logs","component":"fileconsumer","error":"open /var/log/messages: permission denied","original_path":"/var/log/messages","normalized_path":"/var/log/messages"}`
	otherLine  = `{"level":"error","ts":"2026-11-21T10:00:00.000Z","msg":"Failed to open file","otelcol.component.id":"filelog","component":"fileconsumer","error":"open /var/log/app/a.log: too many open files","original_path":"/var/log/app/a.log"}`
	startLine  = `{"level":"info","ts":"2026-11-21T10:00:00.000Z","msg":"Everything is ready. Begin running and processing data."}`
)

// A file receiver whose files it can't open is healthy and sends nothing: the status says which
// files, from which receiver, and why, once a file.
func TestTheStatusSaysWhichLogFilesTheCollectorCantRead(t *testing.T) {
	log := strings.Join([]string{
		startLine, deniedLine, otherLine, deniedLine,
		"Failed to open file /x (not JSON: the Supervisor's Collector logs JSON)",
		`{"level":"info","msg":"Started watching file","original_path":"/var/log/Failed to open file"}`,
	}, "\n")
	got := scanUnreadableLogs(strings.NewReader(log))
	assert.Equal(t, []UnreadableLog{
		{Path: "/var/log/messages", Receiver: "file_log/system", Error: "permission denied"},
		{Path: "/var/log/app/a.log", Receiver: "filelog", Error: "too many open files"},
	}, got)

	var out bytes.Buffer
	st := &Status{PID: os.Getpid(), Started: time.Now(), UnreadableLogs: got}
	st.Print(&out, time.Now())
	assert.Contains(t, out.String(), "Log files      can't read /var/log/messages (receiver file_log/system): permission denied\n")
	assert.Contains(t, out.String(), "               can't read /var/log/app/a.log (receiver filelog): too many open files\n")
	assert.Contains(t, out.String(), "journald receiver", "what to do")

	// No hint for a file that isn't a permission matter.
	out.Reset()
	st.UnreadableLogs = got[1:]
	st.Print(&out, time.Now())
	assert.NotContains(t, out.String(), "journald")

	// At most maxUnreadableLogs.
	var many []string
	for i := range maxUnreadableLogs + 5 {
		many = append(many, strings.ReplaceAll(deniedLine, "/var/log/messages", fmt.Sprintf("/var/log/f%d", i)))
	}
	assert.Len(t, scanUnreadableLogs(strings.NewReader(strings.Join(many, "\n"))), maxUnreadableLogs)
}

// ReadStatus reads them from the running Supervisor's Collector log, and not from a log older
// than the Supervisor, nor when it doesn't run.
func TestReadStatusReadsTheCollectorsUnreadableLogs(t *testing.T) {
	dir := t.TempDir()
	write := func(st Status) {
		b, err := json.Marshal(st)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, StatusFile), b, 0o600))
	}
	agentLog := filepath.Join(dir, agentLogFileName)
	require.NoError(t, os.WriteFile(agentLog, []byte(startLine+"\n"+deniedLine+"\n"), 0o644))
	started := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(agentLog, started.Add(time.Second), started.Add(time.Second)))

	write(Status{PID: os.Getpid(), Started: started})
	st, err := ReadStatus(dir)
	require.NoError(t, err)
	require.Len(t, st.UnreadableLogs, 1)
	assert.Equal(t, "/var/log/messages", st.UnreadableLogs[0].Path)

	write(Status{PID: os.Getpid(), Started: started.Add(time.Hour)})
	st, err = ReadStatus(dir)
	require.NoError(t, err)
	assert.Empty(t, st.UnreadableLogs, "the log is older than the Supervisor")

	write(Status{PID: os.Getpid(), Started: started, Stopped: true, UnreadableLogs: []UnreadableLog{{Path: "/stale"}}})
	st, err = ReadStatus(dir)
	require.NoError(t, err)
	assert.Empty(t, st.UnreadableLogs, "the Supervisor stopped; and the file's own are never read")

	require.NoError(t, os.Remove(agentLog))
	write(Status{PID: os.Getpid(), Started: started})
	st, err = ReadStatus(dir)
	require.NoError(t, err)
	assert.Empty(t, st.UnreadableLogs)
}
