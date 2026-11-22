// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"time"

	"golang.org/x/sys/unix"
)

// szomb is the state of a process that ended and wasn't waited for (sys/proc.h).
const szomb = 5

// processStarted is when the process started (never, if it ended and its parent hasn't noticed); false if
// that can't be read.
func processStarted(pid int) (time.Time, bool) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(p.Proc.P_pid) != pid {
		return time.Time{}, false
	}
	if p.Proc.P_stat == szomb {
		return time.Now().Add(time.Hour), true
	}
	return time.Unix(p.Proc.P_starttime.Unix()), true
}
