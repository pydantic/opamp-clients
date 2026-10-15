# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The file operations the client carries out for its core: a save's recipe and a download's
file go through a `Files`, the real file system by default; with the core tap on, a `Journal`
wraps it and records each operation that succeeded in the tap (kind 5, the checker's
file records), for the checker to replay on a disk that can crash. Tests stand in for it to inject
faults (``Client.start(_files=...)``)."""

from __future__ import annotations

import concurrent.futures
import json
import os
import shutil
import sys
from collections.abc import Callable, Sequence
from typing import Any

Buffer = Any  # bytes, bytearray or memoryview


def _flush(fd: int) -> None:
    """Flush `fd` to the disk. On macOS fsync(2) only hands the data to the drive, which may keep
    it in its cache: fcntl(F_FULLFSYNC) asks the drive to write it out. Where the file system
    doesn't support that (some network mounts), plain fsync, as libuv and OpenJDK do."""
    if sys.platform == "darwin":
        import fcntl

        try:
            fcntl.fcntl(fd, fcntl.F_FULLFSYNC)
            return
        except OSError:
            pass
    os.fsync(fd)


class Files:
    """The real file system. Each method does one operation, or raises OSError."""

    def create(self, path: str) -> int:
        """A new file at `path`, made new (never one already there), readable by this user only:
        its descriptor, open for writing."""
        return os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_BINARY", 0), 0o600)

    def write(self, fd: int, bufs: Sequence[Buffer]) -> int:
        """Write some of `bufs` to `fd`, in order: how many bytes (perhaps fewer than all, as
        write(2) may)."""
        if hasattr(os, "writev"):
            return os.writev(fd, bufs)
        return os.write(fd, bufs[0])

    def sync(self, fd: int) -> None:
        """Flush `fd`'s data to the disk."""
        _flush(fd)

    def close(self, fd: int) -> None:
        os.close(fd)

    def rename(self, src: str, dst: str) -> None:
        """Rename `src` over `dst`."""
        os.replace(src, dst)

    def sync_dir(self, path: str) -> bool:
        """Flush directory `path`'s entries to the disk; False where that can't be done (Windows
        can't open a directory)."""
        if os.name == "nt":
            return False
        fd = os.open(path, os.O_RDONLY)
        try:
            _flush(fd)
        finally:
            os.close(fd)
        return True

    def remove(self, path: str) -> None:
        os.remove(path)

    def mkdir(self, path: str) -> bool:
        """Make directory `path` (and its parents), readable by this user only, unless it is
        there: whether it was made."""
        if os.path.isdir(path):
            return False
        os.makedirs(path, mode=0o700, exist_ok=True)
        return True

    def clear(self, path: str) -> bool:
        """Delete directory `path` and everything under it: whether there was one."""
        if not os.path.lexists(path):
            return False
        shutil.rmtree(path)
        return True


#: The real file system.
REAL = Files()


class AfterClear(Files):
    """`inner`, whose files and directories are made only once `cleared` (the deletion of the
    downloads a run before left, started beside the loop at start) is over: a download never
    makes its file in a directory that is still being deleted. The rest goes straight through."""

    def __init__(self, inner: Files, cleared: concurrent.futures.Future) -> None:
        self._inner = inner
        self._cleared = cleared

    def _wait(self) -> None:
        # In a worker thread (a save's, a download's): never the loop. Its failure is its own.
        concurrent.futures.wait([self._cleared])

    def create(self, path: str) -> int:
        self._wait()
        return self._inner.create(path)

    def write(self, fd: int, bufs: Sequence[Buffer]) -> int:
        return self._inner.write(fd, bufs)

    def sync(self, fd: int) -> None:
        self._inner.sync(fd)

    def close(self, fd: int) -> None:
        self._inner.close(fd)

    def rename(self, src: str, dst: str) -> None:
        self._inner.rename(src, dst)

    def sync_dir(self, path: str) -> bool:
        return self._inner.sync_dir(path)

    def remove(self, path: str) -> None:
        self._inner.remove(path)

    def mkdir(self, path: str) -> bool:
        self._wait()
        return self._inner.mkdir(path)

    def clear(self, path: str) -> bool:
        return self._inner.clear(path)


def write_all(files: Files, fd: int, data: Buffer) -> None:
    """Write all of `data` to `fd`: a write that takes only part of it is followed by another for
    the rest."""
    view = memoryview(data).cast("B")
    while view:
        n = files.write(fd, [view])
        if n <= 0:
            raise OSError("the file took none of the bytes written to it")
        view = view[n:]


class Journal(Files):
    """`inner`, each operation that succeeded recorded (`record`: the tap's kind-5 records) after
    it, with its paths relative to the state file's directory. Thread-safe as `record` is: saves
    and downloads write from worker threads."""

    def __init__(self, inner: Files, record: Callable[[bytes], None], state: str) -> None:
        self._inner = inner
        self._record = record
        self._dir = os.path.dirname(state) or "."
        self._tmp = state + ".tmp"
        #: The files open, by descriptor: their paths.
        self._open: dict[int, str] = {}
        # The state file is named first.
        self._op(op="state", path=self._rel(state))

    def _rel(self, path: str) -> str:
        rel = os.path.relpath(path, self._dir)
        return "." if rel == os.curdir else rel.replace(os.sep, "/")

    def _op(self, **fields: Any) -> None:
        self._record(json.dumps(fields, separators=(",", ":")).encode())

    def create(self, path: str) -> int:
        fd = self._inner.create(path)
        self._open[fd] = path
        self._op(op="create", path=self._rel(path))
        return fd

    def write(self, fd: int, bufs: Sequence[Buffer]) -> int:
        n = self._inner.write(fd, bufs)
        path = self._open.get(fd)
        if path is not None and n > 0:
            if path == self._tmp:
                # A save: the bytes themselves, which the checker compares with the core's.
                data = b"".join(bytes(b) for b in bufs)[:n]
                self._op(op="write", path=self._rel(path), data=data.hex())
            else:
                self._op(op="write", path=self._rel(path), len=n)
        return n

    def sync(self, fd: int) -> None:
        self._inner.sync(fd)
        if (path := self._open.get(fd)) is not None:
            self._op(op="sync", path=self._rel(path))

    def close(self, fd: int) -> None:
        self._open.pop(fd, None)
        self._inner.close(fd)

    def rename(self, src: str, dst: str) -> None:
        self._inner.rename(src, dst)
        # (Its name changes: the descriptor still names the file, under its old name.)
        self._op(**{"op": "rename", "from": self._rel(src), "to": self._rel(dst)})

    def sync_dir(self, path: str) -> bool:
        done = self._inner.sync_dir(path)
        if done:
            self._op(op="syncdir", path=self._rel(path))
        return done

    def remove(self, path: str) -> None:
        self._inner.remove(path)
        self._op(op="remove", path=self._rel(path))

    def mkdir(self, path: str) -> bool:
        made = self._inner.mkdir(path)
        if made:
            self._op(op="mkdir", path=self._rel(path))
        return made

    def clear(self, path: str) -> bool:
        cleared = self._inner.clear(path)
        if cleared:
            self._op(op="clear", path=self._rel(path))
        return cleared
