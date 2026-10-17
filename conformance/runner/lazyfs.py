# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""LazyFS (https://github.com/dsrhaslab/lazyfs), a FUSE file system that keeps what isn't flushed
in a cache of its own: the runner's real crashes of a machine (run.py --lazyfs; the container in
conformance/lazyfs/ has it).

Each driver run's state file is on a mount of its own (one LazyFS process per run: a fault drops
what isn't flushed in the whole file system, and runs go on at once). At a crash the runner kills
the driver, then has LazyFS drop every byte not flushed (`clear-cache`), or LazyFS has torn a
write of the state's temporary file and killed itself (`torn-op`); then it mounts the same root
again, a machine that has rebooted, and the next run starts there.

LazyFS keeps file data only: a rename, a create or an unlink goes to the disk under it at once, so
a directory that isn't flushed loses nothing here (the core tap's model, opamp-tap's, covers
that)."""

from __future__ import annotations

import os
import queue
import shutil
import subprocess
import threading
import time
from pathlib import Path

CONFIG = """[faults]
fifo_path="{fifo}"
fifo_path_completed="{done}"
[cache]
apply_eviction=false
[cache.simple]
custom_size="0.05gb"
blocks_per_page=1
[filesystem]
log_all_operations={all_ops}
logfile="{log}"
"""


def available() -> str | None:
    """Why LazyFS can't run here (None: it can): it needs its binary ($LAZYFS), FUSE (/dev/fuse)
    and the right to mount (CAP_SYS_ADMIN, or a setuid fusermount3)."""
    binary = os.environ.get("LAZYFS")
    if not binary or not os.access(binary, os.X_OK):
        return "no LazyFS binary ($LAZYFS): run in the container of conformance/lazyfs/ (conformance/lazyfs/run.sh)"
    if not os.path.exists("/dev/fuse"):
        return "no /dev/fuse: the container needs --device /dev/fuse and --cap-add SYS_ADMIN (docs/state-file.md)"
    if shutil.which("fusermount3") is None:
        return "no fusermount3 (the fuse3 package)"
    return None


class Mount:
    """One LazyFS file system: `mnt` shows `root` through LazyFS's cache."""

    def __init__(self, base: Path, logs: Path | None = None) -> None:
        self.base = base
        #: Where its logs are copied when it stops (the run's directory), if anywhere.
        self.logs = logs
        self.root, self.mnt = base / "root", base / "mnt"
        self.fifo, self.done = base / "faults.fifo", base / "done.fifo"
        self.log = base / "lazyfs.log"
        self.proc: subprocess.Popen | None = None
        self._out = None
        #: What LazyFS says on its completion FIFO, line by line.
        self._said: queue.Queue[str] = queue.Queue()

    def start(self) -> None:
        """Mount it (the root as it is: what the last crash left)."""
        self.root.mkdir(parents=True, exist_ok=True)
        try:
            self.mnt.mkdir(parents=True, exist_ok=True)
        except OSError:
            subprocess.run(["fusermount3", "-uz", str(self.mnt)], capture_output=True)
            self.mnt.mkdir(parents=True, exist_ok=True)
        for f in (self.fifo, self.done):
            if not f.exists():
                os.mkfifo(f)
        cfg = self.base / "lazyfs.toml"
        cfg.write_text(CONFIG.format(fifo=self.fifo, done=self.done, log=self.log,
                                     all_ops="true" if os.environ.get("LAZYFS_LOG_ALL") else "false"))
        self._out = open(self.base / "lazyfs.out", "a")
        # LazyFS's fault worker opens the completion FIFO before it reads a command, and that open
        # waits for a reader: one is there for the mount's life, or commands would pile up unread
        # (and be read as one).
        listening = threading.Event()
        said = self._said = queue.Queue()

        def listen() -> None:
            with open(self.done) as f:
                listening.set()
                for line in f:
                    said.put(line.strip())

        threading.Thread(target=listen, daemon=True).start()
        self.proc = subprocess.Popen(
            [os.environ["LAZYFS"], str(self.mnt), "--config-path", str(cfg), "-o", "allow_other",
             "-o", "modules=subdir", "-o", f"subdir={self.root}", "-f"],
            stdout=self._out, stderr=subprocess.STDOUT)
        deadline = time.monotonic() + 30
        while not os.path.ismount(self.mnt):
            if self.proc.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError(f"LazyFS did not mount {self.mnt} (exit {self.proc.poll()}); see {self.base}")
            time.sleep(0.02)
        if not listening.wait(30):
            raise RuntimeError(f"LazyFS's fault worker never started ({self.base})")

    def alive(self) -> bool:
        return self.proc is not None and self.proc.poll() is None

    def stop(self) -> None:
        """Unmount it, and end its process; a mount left dangling (it died, or something still
        held a file open) is detached lazily."""
        subprocess.run(["fusermount3", "-u" if self.alive() else "-uz", str(self.mnt)], capture_output=True)
        if self.proc is not None:
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=10)
            self.proc = None
        # Whatever is left (os.path.ismount can't tell a dead FUSE mount: its stat fails).
        subprocess.run(["fusermount3", "-uz", str(self.mnt)], capture_output=True)
        if self._out is not None:
            self._out.close()
            self._out = None
        if self.logs is not None:
            for name in ("lazyfs.log", "lazyfs.out"):
                if (self.base / name).exists():
                    with open(self.logs / name, "a") as f:
                        f.write((self.base / name).read_text(errors="replace"))
                    (self.base / name).unlink()

    def _command(self, line: str) -> None:
        """Send LazyFS a command through its FIFO."""
        fd = os.open(self.fifo, os.O_WRONLY)
        try:
            os.write(fd, (line + "\n").encode())
        finally:
            os.close(fd)

    def _logged(self, what: str, timeout: float = 5) -> str | None:
        """Wait for a line of LazyFS's log holding `what`, or "INVALID": the line (None: neither came)."""
        deadline = time.monotonic() + timeout
        seen = 0
        while time.monotonic() < deadline:
            try:
                text = self.log.read_text(errors="replace")
            except OSError:
                text = ""
            for line in text.splitlines()[seen:]:
                if what in line or "INVALID" in line:
                    return line
            seen = len(text.splitlines())
            time.sleep(0.02)
        return None

    def clear_cache(self, timeout: float = 10) -> bool:
        """Drop every byte written and not flushed, as a power loss would: whether LazyFS said it
        had, in time."""
        while not self._said.empty():
            self._said.get_nowait()
        self._command("lazyfs::clear-cache")
        deadline = time.monotonic() + timeout
        while (left := deadline - time.monotonic()) > 0:
            try:
                if self._said.get(timeout=left) == "finished::clear-cache":
                    return True
            except queue.Empty:
                break
        return False

    def tear(self, path: Path, occurrence: int, persist: int) -> None:
        """Tear the `occurrence`-th write to `path` (on the mount) in two, keep part `persist` (1
        or 2) of it on the disk, and crash: LazyFS kills itself, dropping all it hadn't flushed."""
        under = self.root / path.relative_to(self.mnt)
        self._command(f"lazyfs::torn-op::file={under}::persist={persist}::parts=2::occurrence={occurrence}")
        said = self._logged("configured successfully")
        if said is None or "INVALID" in said:
            raise RuntimeError(f"LazyFS didn't take the torn write: {said}")

    def reboot(self) -> None:
        """The machine comes back: the same root, mounted again, with an empty cache."""
        self.stop()
        self.start()
