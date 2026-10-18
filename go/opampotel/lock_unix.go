// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

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
