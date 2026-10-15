// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

//go:build unix

package opampotel

import (
	"os"
	"syscall"
)

// canLock: there is an operating system lock to take.
const canLock = true

// lock takes the operating system's lock on an open file without waiting: held until the file is
// closed or the process ends.
func lock(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}
