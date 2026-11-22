// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// The client never falls back from the credentials a server issued to the configured ones, so a host the
// server deleted is refused for good. An enrollment helper that a person runs with a token asks for the
// saved state to be set aside: it makes the directory setAsideRequest plus its process ID (and, on Linux, when
// that process started, by /proc) in the storage directory, restarts the Supervisor, and waits. The Supervisor, as it starts and before its client opens
// the state, renames it (setAsidePrefix, the time and the helper's process ID), and the client enrolls the
// host anew with the configured credentials. Nothing puts a state set aside back, replaces it or removes it: a person does
// (the helper says how), or a purge.
const (
	setAsideRequest = coreClientStateFile + ".set-aside-"
	setAsidePrefix  = coreClientStateFile + ".refused-"
)

// setAsideFor is how long a request stands.
const setAsideFor = 2 * time.Minute

// setStateAside sets the state in the storage directory aside, if it is asked to (above) and the server
// refused what the last run connected with as not valid. One it can't rename stays as it is.
func setStateAside(logger *zap.Logger, dir string, now time.Time) {
	state := filepath.Join(dir, coreClientStateFile)
	by, ok := asked(logger, dir, now)
	if !ok {
		return
	}
	if !lastRunRefused(dir) || !isFile(state) {
		logger.Warn("Not setting the saved credentials aside, as asked: the server didn't refuse them in the last run")
		return
	}
	// The first name that nothing has (a second set aside in the second, or the clock went back).
	at := filepath.Join(dir, setAsidePrefix+now.UTC().Format("20060102T150405Z"))
	aside := fmt.Sprintf("%s-%d", at, by)
	for n := 2; ; n++ {
		if _, err := os.Lstat(aside); errors.Is(err, fs.ErrNotExist) {
			break
		} else if err != nil || n > 1000 {
			logger.Error("The saved credentials can't be set aside: no name for them", zap.String("path", aside), zap.Error(err))
			return
		}
		aside = fmt.Sprintf("%s.%d-%d", at, n, by)
	}
	err := os.Rename(state, aside)
	// On Windows a reader (a virus scanner, say) may have it open.
	for i := 0; err != nil && runtime.GOOS == "windows" && i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		err = os.Rename(state, aside)
	}
	if err != nil {
		logger.Error("The saved credentials can't be set aside", zap.Error(err))
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // not on Windows, where a directory can't be flushed
		_ = d.Close()
	}
	logger.Info("The server refused this host's saved credentials: they are set aside, and it enrolls as a new host", zap.String("path", aside))
}

// asked is whether an enrollment helper that still runs asked, just now, for the state to be set aside, and
// its process ID. A request is taken away, and counts once.
func asked(logger *zap.Logger, dir string, now time.Time) (by int, ok bool) {
	for _, name := range namesWith(dir, setAsideRequest) {
		path := filepath.Join(dir, name)
		st, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if err := os.Remove(path); err != nil {
			// Taken at every start, it would never end.
			logger.Error("A request to set the saved credentials aside can't be removed, and isn't taken", zap.Error(err))
			continue
		}
		id, mark, _ := strings.Cut(strings.TrimPrefix(name, setAsideRequest), ".")
		pid, err := strconv.Atoi(id)
		age := now.Sub(st.ModTime())
		if err == nil && st.IsDir() && age < setAsideFor && age > -setAsideFor && processAlive(pid) && sameProcess(pid, mark, st.ModTime()) {
			by, ok = pid, true
		} else {
			logger.Warn("A request to set the saved credentials aside isn't taken: nobody waits for it", zap.String("request", name), zap.Duration("age", age))
		}
	}
	return by, ok
}

// refusedTries is how many tries in a row the server refuses, in a run, before its answer is taken for it.
const refusedTries = 3

// lastRunRefused is whether the server refused the credentials it issued this host all along the last tries
// of the Supervisor's last run, by the status it left: not those of a token just changed, or a try or two.
func lastRunRefused(dir string) bool {
	st, err := ReadStatus(dir)
	return err == nil && st.Accepted == nil && st.Server != nil && st.Server.IssuedRefused >= refusedTries
}

// asideName is what setStateAside names one: only such a file is a state set aside.
var asideName = regexp.MustCompile("^" + regexp.QuoteMeta(setAsidePrefix) + `\d{8}T\d{6}Z(\.\d+)?-\d+$`)

// statesSetAside are the states set aside in dir, the oldest first.
func statesSetAside(dir string) []string {
	var out []string
	for _, name := range namesWith(dir, setAsidePrefix) {
		if path := filepath.Join(dir, name); asideName.MatchString(name) && isFile(path) {
			out = append(out, path)
		}
	}
	return out
}

func namesWith(dir, prefix string) []string {
	entries, _ := os.ReadDir(dir) // sorted
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func isFile(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode().IsRegular()
}
