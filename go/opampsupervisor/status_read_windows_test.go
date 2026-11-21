// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows"
)

// A sharing violation (the Supervisor replacing the file as it is read) is tried again; a file
// that isn't there is not this.
func TestASharingViolationIsTransient(t *testing.T) {
	assert.True(t, transientRead(&fs.PathError{Op: "open", Path: "supervisor-status.json", Err: windows.ERROR_SHARING_VIOLATION}))
	assert.True(t, transientRead(&fs.PathError{Op: "open", Path: "supervisor-status.json", Err: windows.ERROR_ACCESS_DENIED}))
	assert.False(t, transientRead(&fs.PathError{Op: "open", Path: "supervisor-status.json", Err: windows.ERROR_FILE_NOT_FOUND}))
}
