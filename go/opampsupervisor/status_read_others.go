// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build !windows

package main

import "time"

// transientRead is how long to try a failed read of the status file again: never on Unix, where
// a rename gives a reader the old file or the new one.
func transientRead(error) time.Duration { return 0 }
