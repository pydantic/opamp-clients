// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

//go:build !unix && !windows

package opampotel

import "os"

// canLock: none here (js, wasip1, plan9). Two processes sharing a slot would be one instance, so
// there is no default state directory, and each start is a new instance.
const canLock = false

func lock(*os.File) bool { return false }
