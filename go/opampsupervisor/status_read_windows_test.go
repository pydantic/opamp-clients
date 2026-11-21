// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows"
)

// A sharing violation (the Supervisor replacing the file as it is read) is tried again as long as
// the caller waits; access denied, which is also a real permission error, for a moment; a file
// that isn't there not at all.
func TestASharingViolationIsTransient(t *testing.T) {
	open := func(err error) error { return &fs.PathError{Op: "open", Path: "supervisor-status.json", Err: err} }
	assert.Equal(t, forever, transientRead(open(windows.ERROR_SHARING_VIOLATION)))
	assert.Equal(t, 2*time.Second, transientRead(open(windows.ERROR_ACCESS_DENIED)))
	assert.Zero(t, transientRead(open(windows.ERROR_FILE_NOT_FOUND)))
}
