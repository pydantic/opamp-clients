// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

//go:build windows

package opampotel

import (
	"os"

	"golang.org/x/sys/windows"
)

// canLock: there is an operating system lock to take.
const canLock = true

// lock takes the operating system's lock on an open file without waiting: held until the file is
// closed or the process ends.
func lock(f *os.File) bool {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, new(windows.Overlapped)) == nil
}
