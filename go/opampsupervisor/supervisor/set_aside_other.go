// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build !darwin && !linux && !windows

package supervisor

import "time"

// sameProcess is whether the process with the ID is the one that made a request: it has to do here.
func sameProcess(int, string, time.Time) bool { return true }
