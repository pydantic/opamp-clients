// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampotel

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// held keeps the default state directories' lock files open for as long as their SDK runs: an
// unreachable *os.File would be closed by its finalizer, and its lock let go.
var held struct {
	sync.Mutex
	files []*os.File
}

// release lets a default state directory's lock go: a Setup that failed, or an SDK shut down, gives
// its slot back.
func release(f *os.File) {
	if f == nil {
		return
	}
	held.Lock()
	for i, h := range held.files {
		if h == f {
			held.files = append(held.files[:i], held.files[i+1:]...)
			break
		}
	}
	held.Unlock()
	_ = f.Close()
}

// programName is the program, as a name: its file's, without .exe.
func programName(args []string) string {
	if len(args) == 0 || args[0] == "" {
		return "go"
	}
	return strings.TrimSuffix(filepath.Base(args[0]), ".exe")
}

// slot is the first of the default state directory's slots (docs/default-state-dir.md, as the
// core's Setup names them) this process can make and lock, and its lock, held until the process
// ends; "" and nil where there's none to be had (no lock on this system, not writable, every slot
// taken: allTaken is the line to say then): each start is then a new instance. Never an error.
func slot(slots []string, allTaken string) (string, *os.File) {
	if len(slots) == 0 || !canLock {
		return "", nil
	}
	for _, dir := range slots {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			slog.Debug("opamp: no state directory of its own: each start is a new instance", "error", err)
			return "", nil
		}
		// Not inherited by a process this one starts: Go opens files close-on-exec (Unix) and
		// non-inheritable (Windows).
		f, err := os.OpenFile(filepath.Join(dir, opamp.SetupLockFile), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			slog.Debug("opamp: no state directory of its own: each start is a new instance", "error", err)
			return "", nil
		}
		if lock(f) {
			held.Lock()
			held.files = append(held.files, f)
			held.Unlock()
			return dir, f
		}
		_ = f.Close()
	}
	slog.Info("opamp: " + allTaken)
	return "", nil
}
