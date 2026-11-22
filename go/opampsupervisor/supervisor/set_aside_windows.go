// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"time"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code of a process that runs (STILL_ACTIVE).
const stillActive = 259

// processStarted is when the process started; false if that can't be read, or it has ended.
func processStarted(pid int) (time.Time, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	var created, exited, kernel, user windows.Filetime
	if windows.GetExitCodeProcess(h, &code) != nil || code != stillActive || windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return time.Time{}, false
	}
	return time.Unix(0, created.Nanoseconds()), true
}
