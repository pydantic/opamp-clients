// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// slots is how many processes of one program on one host each keep their own identity by default.
const slots = 16

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

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// stateBase is the user's own state directory, "" where there's none.
func stateBase(getenv func(string) string) string {
	switch runtime.GOOS {
	case "windows":
		return getenv("LOCALAPPDATA")
	case "darwin", "ios":
		if home := getenv("HOME"); home != "" {
			return filepath.Join(home, "Library", "Application Support")
		}
	default:
		if xdg := getenv("XDG_STATE_HOME"); xdg != "" {
			return xdg
		}
		if home := getenv("HOME"); home != "" {
			return filepath.Join(home, ".local", "state")
		}
	}
	return ""
}

// defaultStateDir is, without OTEL_OPAMP_STATE_DIR, a state directory of the user's own, so an
// instance keeps its UID across restarts with no setup (docs/default-state-dir.md). One per
// program, service and working directory; a slot in it per process running at once, each held by
// a lock until the process ends. The directory and its lock's file, or "" and nil where there's
// none to be had (no home, not writable, every slot taken): then each start is a new instance, as
// before. Never an error.
func defaultStateDir(getenv func(string) string, args []string) (string, *os.File) {
	base := stateBase(getenv)
	if base == "" || !canLock {
		return "", nil
	}
	attrs := map[string]string{}
	for _, kv := range strings.Split(getenv("OTEL_RESOURCE_ATTRIBUTES"), ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			v = strings.TrimSpace(v)
			if u, err := url.PathUnescape(v); err == nil {
				v = u
			}
			attrs[strings.TrimSpace(k)] = v
		}
	}
	program := programName(args)
	service := strings.TrimSpace(getenv("OTEL_SERVICE_NAME"))
	if service == "" {
		service = attrs["service.name"]
	}
	if service == "" {
		service = program
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{service, attrs["service.namespace"], program, cwd}, "\x00")))
	name := unsafeName.ReplaceAllString(service, "_")
	if len(name) > 64 {
		name = name[:64]
	}
	if name == "" {
		name = "service"
	}
	home := filepath.Join(base, "opamp", name+"-"+hex.EncodeToString(sum[:])[:8])
	for slot := 1; slot <= slots; slot++ {
		dir := filepath.Join(home, strconv.Itoa(slot))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			slog.Debug("opamp: no state directory of its own: each start is a new instance", "error", err)
			return "", nil
		}
		// Not inherited by a process this one starts: Go opens files close-on-exec (Unix) and
		// non-inheritable (Windows).
		f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
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
	slog.Info(fmt.Sprintf("opamp: %d processes of %s already keep their identity here: this one is a new "+
		"instance each start", slots, service))
	return "", nil
}
