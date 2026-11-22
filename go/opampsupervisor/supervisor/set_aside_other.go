// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build !darwin && !linux && !windows

package supervisor

import "time"

// processStarted is when the process started; false: it can't be read here.
func processStarted(int) (time.Time, bool) { return time.Time{}, false }
