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
	"runtime"
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
// files. A line longer than a megabyte is skipped.
//
// It is what that log says: a file receiver says so once a file for a permission error, so the
// line is gone once the log passes its size and is emptied, and a file made readable since is
// still named until the Collector starts again.
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
	br := bufio.NewReaderSize(r, 64<<10)
	for len(found) < maxUnreadableLogs {
		line, err := readLine(br, 1<<20)
		if err != nil {
			break
		}
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

// readLine reads a line, without its newline; a line longer than limit is read whole and returned
// empty.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	tooLong := false
	for {
		part, err := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(part) > limit {
				tooLong, line = true, nil
			} else {
				line = append(line, part...)
			}
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue
		case err != nil && (len(line) == 0 || err != io.EOF):
			return nil, err
		}
		return bytes.TrimRight(line, "\r\n"), nil
	}
}

// unreadableLogsHintGOOS is the system whose hint is given: the journal is Linux's.
var unreadableLogsHintGOOS = runtime.GOOS

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
		denied = denied || strings.Contains(l.Error, "permission denied")
	}
	if denied && unreadableLogsHintGOOS == "linux" {
		line("", "the Collector's user can't read these: give it read access, or read the journal")
		line("", "with the journald receiver where the syslog files are root's alone (Fedora, RHEL)")
	}
}
