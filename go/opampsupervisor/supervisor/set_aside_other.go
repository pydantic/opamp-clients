// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build !darwin && !windows

package supervisor

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// processStarted is when the process started, by /proc (Linux, in hundredths of a second since the
// boot); false if that can't be read.
func processStarted(pid int) (time.Time, bool) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Time{}, false
	}
	// After the command's name, in parentheses, which may hold anything.
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	if len(fields) < 20 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	uptime, err2 := os.ReadFile("/proc/uptime")
	up, _, _ := strings.Cut(string(uptime), " ")
	since, err3 := strconv.ParseFloat(up, 64)
	if err != nil || err2 != nil || err3 != nil {
		return time.Time{}, false
	}
	return time.Now().Add(-time.Duration(since*float64(time.Second)) + time.Duration(ticks)*10*time.Millisecond), true
}
