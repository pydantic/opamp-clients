// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/pydantic/opamp-clients/go/opamp"
)

// held keeps the state directories' lock files open for as long as their SDK runs: an
// unreachable *os.File would be closed by its finalizer, and its lock let go.
var held struct {
	sync.Mutex
	files []*os.File
}

// release lets a state directory's lock go: a Setup that failed, or an SDK shut down, gives
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

// stateSlots is how many processes running at once keep their own identity in one state directory.
const stateSlots = 16

// namedSlots are a named state directory's slots (docs/default-state-dir.md): itself, then its
// folders 2 to 16. The first process keeps the directory and its UID, as before slots; one sharing
// it (a process started with the same OTEL_OPAMP_STATE_DIR) takes a folder of its own instead of
// its UID.
func namedSlots(dir string) []string {
	out := []string{dir}
	for n := 2; n <= stateSlots; n++ {
		out = append(out, filepath.Join(dir, strconv.Itoa(n)))
	}
	return out
}

// slot is the first of a state directory's slots (the default one's as the core's Setup names
// them, or the one named's) this process can make and lock, and its lock, held until the process
// ends; "" and nil where there's none to be had (no lock on this system, not writable, every slot
// taken: allTaken is the line to say then): each start is then a new instance. Never an error; a
// directory named that can't be used is said as a warning, the default one's at debug.
func slot(slots []string, allTaken, named string) (string, *os.File) {
	if len(slots) == 0 || !canLock {
		return "", nil
	}
	unusable := func(err error) {
		if named != "" {
			slog.Warn("opamp: the state directory "+named+" can't be used: each start is a new instance", "error", err)
		} else {
			slog.Debug("opamp: no state directory of its own: each start is a new instance", "error", err)
		}
	}
	for _, dir := range slots {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			unusable(err)
			return "", nil
		}
		// Not inherited by a process this one starts: Go opens files close-on-exec (Unix) and
		// non-inheritable (Windows).
		f, err := os.OpenFile(filepath.Join(dir, opamp.SetupLockFile), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			unusable(err)
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
