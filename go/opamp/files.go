// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// ReadState is what is saved at stateFile (nil if there is no file yet), once the work a client
// stopped before on it left under way has ended: what a client started next on the file starts
// from. Stop doesn't wait for a save already writing (it lands by itself), so a read straight
// after Stop can find the state before it. stateFile is named as it was in Config.StateFile. A
// state sealed with Config.StateSealer comes back sealed.
func ReadState(stateFile string) ([]byte, error) {
	workOn(stateFile).settle()
	b, err := os.ReadFile(stateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// fileSystem is every file operation a save and a download's file carry out: the disk itself
// (osFS), journaled into the core tap when it is on (journal), and replaced by tests that inject
// faults.
type fileSystem interface {
	// createNew makes a new file at path, readable by the agent's user only, for writing: never
	// one already there (O_EXCL).
	createNew(path string) (writeFile, error)
	remove(path string) error
	rename(from, to string) error
	// syncDir flushes directory dir, so the changes to its entries (a rename) reach the disk:
	// false where directories can't be flushed (Windows), and nothing was done.
	syncDir(dir string) (bool, error)
	// mkdir makes directory dir, the user's only; an error satisfying os.IsExist if it is there.
	mkdir(dir string) error
}

// writeFile is a file made by createNew.
type writeFile interface {
	Write(b []byte) (int, error)
	Sync() error
	Close() error
}

// osFS is the disk.
type osFS struct{}

func (osFS) createNew(path string) (writeFile, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func (osFS) remove(path string) error         { return os.Remove(path) }
func (osFS) rename(from, to string) error     { return os.Rename(from, to) }
func (osFS) mkdir(dir string) error           { return os.Mkdir(dir, 0o700) }
func (osFS) syncDir(dir string) (bool, error) { return syncDir(dir) }

func syncDir(dir string) (bool, error) {
	if runtime.GOOS == "windows" {
		return false, nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err == nil, err
}

// journal records each file operation that succeeded in the core tap (the tap checker's file records)
// before passing on what it returned, so a record is in the tap before anything that depends on
// it reaches the core. Paths are relative to the state file's directory, with '/'.
type journal struct {
	fs     fileSystem
	dir    string // the state file's directory
	tmp    string // the state file's temporary file: its writes are recorded with their bytes
	record func(op []byte)
}

// journaled wraps fsys in a journal for the state file at state, recording with rec; fsys itself
// if rec is nil (no tap).
func journaled(fsys fileSystem, state string, rec func([]byte)) fileSystem {
	if rec == nil {
		return fsys
	}
	j := &journal{fs: fsys, dir: filepath.Dir(state), tmp: tempFile(state), record: rec}
	j.op(map[string]any{"op": "state", "path": j.rel(state)})
	return j
}

func (j *journal) rel(p string) string {
	r, err := filepath.Rel(j.dir, p)
	if err != nil {
		r = p
	}
	return filepath.ToSlash(r)
}

func (j *journal) op(fields map[string]any) {
	b, _ := json.Marshal(fields)
	j.record(b)
}

func (j *journal) createNew(path string) (writeFile, error) {
	f, err := j.fs.createNew(path)
	if err != nil {
		return nil, err
	}
	j.op(map[string]any{"op": "create", "path": j.rel(path)})
	return &journaledFile{f: f, j: j, path: path}, nil
}

func (j *journal) remove(path string) error {
	err := j.fs.remove(path)
	if err == nil {
		j.op(map[string]any{"op": "remove", "path": j.rel(path)})
	}
	return err
}

func (j *journal) rename(from, to string) error {
	err := j.fs.rename(from, to)
	if err == nil {
		j.op(map[string]any{"op": "rename", "from": j.rel(from), "to": j.rel(to)})
	}
	return err
}

func (j *journal) syncDir(dir string) (bool, error) {
	ok, err := j.fs.syncDir(dir)
	if ok && err == nil {
		j.op(map[string]any{"op": "syncdir", "path": j.rel(dir)})
	}
	return ok, err
}

func (j *journal) mkdir(dir string) error {
	err := j.fs.mkdir(dir)
	if err == nil {
		j.op(map[string]any{"op": "mkdir", "path": j.rel(dir)})
	}
	return err
}

// journaledFile records a file's writes (what was written, even of a write that then failed) and
// syncs.
type journaledFile struct {
	f    writeFile
	j    *journal
	path string
}

func (f *journaledFile) Write(b []byte) (int, error) {
	n, err := f.f.Write(b)
	if n > 0 {
		if f.path == f.j.tmp {
			f.j.op(map[string]any{"op": "write", "path": f.j.rel(f.path), "data": hex.EncodeToString(b[:n])})
		} else {
			f.j.op(map[string]any{"op": "write", "path": f.j.rel(f.path), "len": n})
		}
	}
	return n, err
}

func (f *journaledFile) Sync() error {
	err := f.f.Sync()
	if err == nil {
		f.j.op(map[string]any{"op": "sync", "path": f.j.rel(f.path)})
	}
	return err
}

func (f *journaledFile) Close() error { return f.f.Close() }

// tempFile is where a save of the state file at path is written before it replaces it.
func tempFile(path string) string { return path + ".tmp" }
