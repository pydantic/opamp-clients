// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"time"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code of a process that runs (STILL_ACTIVE).
const stillActive = 259

// sameProcess is whether the process with the ID is the one that made a request at the time, not one
// started since.
func sameProcess(pid int, _ string, at time.Time) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	var created, exited, kernel, user windows.Filetime
	if windows.GetExitCodeProcess(h, &code) != nil || code != stillActive || windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return false
	}
	// To a second, as file times may be.
	return !time.Unix(0, created.Nanoseconds()).After(at.Add(time.Second))
}
