// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// processStarted is when the process started (never, if it ended and its parent hasn't noticed), by /proc,
// in hundredths of a second since the boot; false if that can't be read.
func processStarted(pid int) (time.Time, bool) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Time{}, false
	}
	// After the command's name, in parentheses, which may hold anything: the state, and the rest.
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	if len(fields) < 20 {
		return time.Time{}, false
	}
	now := time.Now()
	if fields[0] == "Z" || fields[0] == "X" {
		return now.Add(time.Hour), true
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	var up unix.Timespec // not /proc/uptime, which a container may show its own in
	if err != nil || unix.ClockGettime(unix.CLOCK_BOOTTIME, &up) != nil {
		return time.Time{}, false
	}
	return now.Add(time.Duration(ticks)*10*time.Millisecond - time.Duration(up.Nano())), true
}
