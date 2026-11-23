// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// transientRead is how long to try a failed read of the status file again: Windows refuses to
// open a file that a rename is replacing. A sharing violation is only that, so it is tried
// again for as long as the caller waits (forever); access denied is that too while the old file
// is pending delete, but is also a real permission error (the status is the Supervisor's user's),
// so it is tried again for a moment only. Anything else, not at all.
func transientRead(err error) time.Duration {
	switch {
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		return forever
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return 2 * time.Second
	}
	return 0
}
