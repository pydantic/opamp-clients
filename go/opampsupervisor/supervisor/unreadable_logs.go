// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// UnreadableLog is a file a log receiver of the running Collector found and couldn't open, as
// its log says: a receiver whose files are all like that is healthy and sends nothing, so the
// status says which, and why.
type UnreadableLog struct {
	Path     string `json:"path"`
	Receiver string `json:"receiver,omitempty"`
	Error    string `json:"error"`
}

// maxUnreadableLogs is how many are kept: enough to see what is wrong.
const maxUnreadableLogs = 20

// The file receivers' line for a file they can't open: "Failed to open file - unreadable" for a
// permission error, once a file; "Failed to open file" for any other.
var failedToOpen = []byte("Failed to open file")

// unreadableLogs reads, from the log of the Collector the Supervisor in dir last started (its
// storage directory's agent.log, which each start begins anew), the files it couldn't open. It
// needs the Collector's JSON log, which the Supervisor configures; a log it can't read is no
// files. Lines are read as long as a megabyte.
func unreadableLogs(dir string) []UnreadableLog {
	f, err := os.Open(filepath.Join(dir, agentLogFileName))
	if err != nil {
		return nil
	}
	defer f.Close()
	return scanUnreadableLogs(f)
}

func scanUnreadableLogs(r io.Reader) []UnreadableLog {
	var found []UnreadableLog
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() && len(found) < maxUnreadableLogs {
		line := sc.Bytes()
		if !bytes.Contains(line, failedToOpen) {
			continue
		}
		var entry struct {
			Msg      string `json:"msg"`
			Error    string `json:"error"`
			Path     string `json:"original_path"`
			Receiver string `json:"otelcol.component.id"`
		}
		if json.Unmarshal(line, &entry) != nil || !strings.HasPrefix(entry.Msg, string(failedToOpen)) || entry.Path == "" || seen[entry.Path] {
			continue
		}
		seen[entry.Path] = true
		found = append(found, UnreadableLog{
			Path:     entry.Path,
			Receiver: entry.Receiver,
			Error:    strings.TrimPrefix(entry.Error, "open "+entry.Path+": "),
		})
	}
	return found
}

// printUnreadableLogs says which files the Collector can't open, and what to do about it.
func printUnreadableLogs(line func(label, format string, args ...any), logs []UnreadableLog) {
	if len(logs) == 0 {
		return
	}
	label := "Log files"
	denied := false
	for _, l := range logs {
		receiver := ""
		if l.Receiver != "" {
			receiver = fmt.Sprintf(" (receiver %s)", l.Receiver)
		}
		line(label, "can't read %s%s: %s", l.Path, receiver, l.Error)
		label = ""
		denied = denied || strings.Contains(strings.ToLower(l.Error), "denied")
	}
	if denied {
		line("", "the Collector's user can't read these: give it read access, or read the journal")
		line("", "with the journald receiver where the syslog files are root's alone (Fedora, RHEL)")
	}
}
