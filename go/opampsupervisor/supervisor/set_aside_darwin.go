// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"time"

	"golang.org/x/sys/unix"
)

// szomb is the state of a process that ended and wasn't waited for (sys/proc.h).
const szomb = 5

// sameProcess is whether the process with the ID is the one that made a request at the time, not one
// started since.
func sameProcess(pid int, _ string, at time.Time) bool {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	// To a second, as file times may be.
	return err == nil && int(p.Proc.P_pid) == pid && p.Proc.P_stat != szomb && !time.Unix(p.Proc.P_starttime.Unix()).After(at.Add(time.Second))
}
