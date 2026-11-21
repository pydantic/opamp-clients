// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build !windows

package main

// transientRead is whether a read of the status file failed because the Supervisor was replacing
// it: never on Unix, where a rename gives a reader the old file or the new one.
func transientRead(error) bool { return false }
