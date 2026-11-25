// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// sameProcess is whether the process with the ID is the one that made a request, not one started since: it
// started when the request says, as /proc/PID/stat has it (no clock, as a container may have another's). Where
// either isn't known (processes of others hidden, say), the ID has to do.
func sameProcess(pid int, mark string, _ time.Time) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	// After the command's name, in parentheses, which may hold anything: the state, and the rest.
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	if len(fields) < 20 {
		return true
	}
	// Z and X: it ended, and its parent hasn't noticed.
	return fields[0] != "Z" && fields[0] != "X" && (mark == "" || mark == fields[19])
}
