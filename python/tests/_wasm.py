# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""A core run as a WebAssembly module in wasmtime (a dev dependency, not one of the package's),
behind the interface the client runs the native core through. The modules beside the package's
sources (src/opamp_client/opamp_core.wasm, opamp_sdk_settings.wasm, not in the wheel) are the
oracle the native extension is held to; another module with the core's ABI is a hostile core
(the adversary, testdata/core/opamp_core_adversary.wasm)."""

from __future__ import annotations

import threading
from pathlib import Path

import wasmtime

from opamp_client._native import Trap

_HERE = Path(__file__).resolve().parent.parent / "src" / "opamp_client"
_lock = threading.Lock()
_engine = wasmtime.Engine()
_modules: dict[bytes, wasmtime.Module] = {}


def shipped(name: str = "opamp_core.wasm") -> bytes:
    """A module checked in beside the package's sources (scripts/gen_core.py, gen_sdk_settings.py)."""
    return (_HERE / name).read_bytes()


def _compiled(wasm: bytes) -> wasmtime.Module:
    with _lock:
        if wasm not in _modules:
            _modules[wasm] = wasmtime.Module(_engine, wasm)
        return _modules[wasm]


class _Instance:
    def __init__(self, wasm: bytes) -> None:
        self._store = wasmtime.Store(_engine)
        self._exports = wasmtime.Instance(self._store, _compiled(wasm), []).exports(self._store)
        self._memory = self._exports["memory"]
        self._lock = threading.Lock()

    def call(self, name: str, data: bytes) -> bytes:
        with self._lock:
            ptr = self._exports["alloc"](self._store, len(data))
            self._memory.write(self._store, data, ptr)
            try:
                packed = self._exports[name](self._store, ptr, len(data))
            except wasmtime.Trap as e:
                raise Trap(str(e)) from e
            out_ptr, out_len = packed >> 32, packed & 0xFFFFFFFF
            raw = bytes(self._memory.read(self._store, out_ptr, out_ptr + out_len))
            self._exports["free"](self._store, out_ptr, out_len)
            return raw


class WasmCore:
    """One session of a core module: as _native.Core."""

    def __init__(self, wasm: bytes | None = None) -> None:
        self._instance = _Instance(shipped() if wasm is None else wasm)

    def init(self, config: bytes) -> bytes:
        return self._instance.call("init", config)

    def step(self, step: bytes) -> bytes:
        return self._instance.call("step", step)


class WasmSdkSettings:
    """The SDK settings module: as _native.sdk_settings_call."""

    def __init__(self) -> None:
        self._instance = _Instance(shipped("opamp_sdk_settings.wasm"))

    def call(self, request: bytes) -> bytes:
        return self._instance.call("call", request)
