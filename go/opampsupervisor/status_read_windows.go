// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// transientRead is whether a read of the status file failed because the Supervisor was replacing
// it: Windows refuses to open a file that a rename is replacing (a sharing violation, or access
// denied while it is pending delete), where Unix gives the old file or the new one.
func transientRead(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
