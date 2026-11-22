// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"time"

	"golang.org/x/sys/unix"
)

// processStarted is when the process started; false if that can't be read.
func processStarted(pid int) (time.Time, bool) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(p.Proc.P_pid) != pid {
		return time.Time{}, false
	}
	return time.Unix(p.Proc.P_starttime.Unix()), true
}
