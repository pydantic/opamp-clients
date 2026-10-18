# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The shared OpAMP client core, run natively: the native library
(pydantic-opamp-core's, through pydantic_opamp_client._native), which takes and answers the same bytes as
the WebAssembly module the other languages run. Each Core is one session, holding one client's
state."""

from __future__ import annotations

import os
import struct
import sys
import threading
import time
from typing import Any

from . import _native
from ._proto.opamp.core.v1 import core_pb2 as corepb


class CoreError(Exception):
    """The core refused an input (a bug in this client) or trapped (a bug in the core)."""


# Core tap: with OPAMP_CORE_TAP=<path> set, every call into the core is appended to a file for the
# effects checker (opamp-tap). Each record is a kind byte (1 init in, 2 init out, 3 step in,
# 4 step out), a u32 LE length, a u64 LE monotonic time in nanoseconds, then the bytes exactly as
# they crossed (Config, Reply, Step, Reply). A process's first core writes <path>, each later one
# <path>.<pid>.<n>. Unset, it costs nothing. With a state file, the file operations of saves and
# downloads go to the tap too (kind 5, the checker's file records: one JSON object each), from
# whatever thread carries them out (_files.Journal): the records are written one at a time.
_INIT_IN, _INIT_OUT, _STEP_IN, _STEP_OUT, _FILE = 1, 2, 3, 4, 5
_taps = 0  # cores tapped in this process
_taps_lock = threading.Lock()


def _open_tap() -> int | None:
    """The tap file for a new core (a file descriptor), or None: no tap."""
    global _taps
    path = os.environ.get("OPAMP_CORE_TAP")
    if not path:
        return None
    with _taps_lock:
        n = _taps
        _taps += 1
    name = path if n == 0 else f"{path}.{os.getpid()}.{n}"
    try:
        # Readable by this user only: a Config holds the saved state, secrets included.
        return os.open(name, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
    except OSError as e:
        print(f"opamp: OPAMP_CORE_TAP: can't open {name}: {e}; running without the tap", file=sys.stderr)
        return None


class Core:
    """One instance of the core: one client's state."""

    def __init__(self, config: corepb.Config, *, _engine: Any = None) -> None:
        # _engine: tests only, run instead of the native core: a WebAssembly module with the
        # core's ABI (the adversary) in wasmtime (tests/_wasm.py's WasmCore).
        engine = _native.Core() if _engine is None else _engine
        self._init, self._step = engine.init, engine.step
        self._close_engine = getattr(engine, "close", None)  # the native core's session
        #: The instance UID in use, as the core last said (the application's, a saved one, or
        #: one the core made).
        self.instance_uid = b""
        # The limits the core goes by, as given or by default: its start reports them.
        self.limits: corepb.Limits | None = None
        self._tap = _open_tap()
        self._tap_lock = threading.Lock()  # records come from worker threads too (file records)
        try:
            self._call(self._init, config, _INIT_IN)
        except BaseException:
            self.close()
            raise

    def step(self, now_ms: int, input_: corepb.Input) -> corepb.Output:
        """Feed the core one input at `now_ms` (milliseconds on a monotonic clock)."""
        # The wall clock too: core mode reads Retry-After dates against it.
        step = corepb.Step(now_ms=now_ms, input=input_, unix_ms=time.time_ns() // 1_000_000)
        return self._call(self._step, step, _STEP_IN)

    def close(self) -> None:
        """Release what the core holds outside the module (the native session, the tap file):
        nothing is stepped after."""
        if self._close_engine is not None:
            self._close_engine()
        with self._tap_lock:
            tap, self._tap = self._tap, None
        if tap is not None:
            os.close(tap)

    @property
    def tapped(self) -> bool:
        """Whether this core's calls go to a tap (OPAMP_CORE_TAP)."""
        return self._tap is not None

    def record_file(self, op: bytes) -> None:
        """Record a file operation (one JSON object) in the tap, if it is still open: from any
        thread, after the operation succeeded, before anything that depends on it is fed."""
        self._record(_FILE, op)

    def _record(self, kind: int, data: bytes) -> None:
        with self._tap_lock:
            # (Closed: a save or a download still finishing after stop goes unrecorded.)
            if self._tap is not None:
                # One write per record: a killed process leaves whole records. The time is taken
                # under the lock: the records' times go up in the file's order.
                os.write(self._tap, struct.pack("<BIQ", kind, len(data), time.monotonic_ns()) + data)

    def _call(self, fn, msg, tap_kind: int) -> corepb.Output:
        data = msg.SerializeToString()
        if self._tap is not None:
            self._record(tap_kind, data)
        try:
            raw = fn(data)
        except _native.Trap as e:
            raise CoreError(f"trap: {e}") from e
        if self._tap is not None:
            self._record(tap_kind + 1, raw)
        reply = corepb.Reply.FromString(raw)
        if reply.error:
            raise CoreError(reply.error)
        if reply.output.instance_uid:
            self.instance_uid = bytes(reply.output.instance_uid)
        if reply.output.HasField("limits"):
            self.limits = reply.output.limits
        return reply.output
