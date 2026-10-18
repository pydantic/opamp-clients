# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The shared core and the SDK settings layer, compiled (a native library), through their C
interface: protobuf bytes in and out, as their WebAssembly modules take and answer them.

The library comes from pydantic-opamp-core (pydantic_opamp_core/), the version this package pins. Each
call hands the library the input for the duration of the call and gets back bytes it allocated,
which are copied out and handed back to it to free. ctypes lets go of the GIL for the call."""

from __future__ import annotations

import ctypes
import importlib.util
import sys
from pathlib import Path

ABI_VERSION = 1  # the C interface this package speaks (the library's ABI_VERSION)


class Trap(Exception):
    """The core panicked (in the WebAssembly module, a trap): a bug in the core."""


def _lib_name() -> str:
    if sys.platform.startswith("win"):
        return "opamp_native.dll"
    return "libopamp_native.dylib" if sys.platform == "darwin" else "libopamp_native.so"


def _find() -> str:
    spec = importlib.util.find_spec("pydantic_opamp_core")
    for d in (spec.submodule_search_locations or []) if spec else []:
        if (lib := Path(d) / _lib_name()).is_file():
            return str(lib)
    raise ImportError(f"the OpAMP core library ({_lib_name()}) isn't installed: "
                      "pydantic-opamp-core has no build for this platform")


_lib = ctypes.CDLL(_find())
_u8p = ctypes.POINTER(ctypes.c_uint8)
_call = [ctypes.c_char_p, ctypes.c_size_t, ctypes.POINTER(_u8p), ctypes.POINTER(ctypes.c_size_t)]
_lib.opamp_native_abi_version.restype = ctypes.c_uint32
_lib.opamp_native_abi_version.argtypes = []
_lib.opamp_core_new.restype = ctypes.c_void_p
_lib.opamp_core_new.argtypes = []
_lib.opamp_core_free.restype = None
_lib.opamp_core_free.argtypes = [ctypes.c_void_p]
for _f in (_lib.opamp_core_init, _lib.opamp_core_step):
    _f.restype = ctypes.c_int32
    _f.argtypes = [ctypes.c_void_p, *_call]
_lib.opamp_sdk_settings_call.restype = ctypes.c_int32
_lib.opamp_sdk_settings_call.argtypes = _call
_lib.opamp_bytes_free.restype = None
_lib.opamp_bytes_free.argtypes = [_u8p, ctypes.c_size_t]

if (_version := _lib.opamp_native_abi_version()) != ABI_VERSION:
    raise ImportError(f"the OpAMP core library speaks interface {_version}, this package {ABI_VERSION}: "
                      "install the pydantic-opamp-core this package asks for")


def _run(fn, *handle: int, data: bytes) -> bytes:
    """Call fn with data; the reply, or a panic raised as Trap."""
    out, n = _u8p(), ctypes.c_size_t()
    code = fn(*handle, data, len(data), ctypes.byref(out), ctypes.byref(n))
    try:
        got = ctypes.string_at(out, n.value)
    finally:
        _lib.opamp_bytes_free(out, n)
    if code != 0:
        raise Trap(got.decode("utf-8", "replace"))
    return got


class Core:
    """One session: init with an encoded Config, then step with encoded Steps; each answers an
    encoded Reply. After a Trap every call raises one."""

    def __init__(self) -> None:
        self._handle = _lib.opamp_core_new()
        if not self._handle:
            raise MemoryError("opamp_core_new")

    def close(self) -> None:
        """Free the session now, not when the garbage collector gets to this object: a stopped
        client is in reference cycles, kept until a collection finds it, and the collector
        doesn't count the memory the library holds. Every call after raises Trap."""
        if handle := getattr(self, "_handle", None):
            self._handle = None
            _lib.opamp_core_free(handle)

    __del__ = close

    def init(self, config: bytes) -> bytes:
        return _run(_lib.opamp_core_init, self._live(), data=config)

    def step(self, step: bytes) -> bytes:
        return _run(_lib.opamp_core_step, self._live(), data=step)

    def _live(self) -> int:
        if self._handle is None:
            raise Trap("the session is closed")  # a null handle would crash the library
        return self._handle


def sdk_settings_call(request: bytes) -> bytes:
    """Run one encoded SDK settings Request; returns the encoded Reply."""
    return _run(_lib.opamp_sdk_settings_call, data=request)


def setup_call(call: bytes) -> bytes:
    """Run one encoded SetupCall (the core's Setup: no session); returns the encoded SetupReply.
    Bound at the first call: a library from before Setup still runs the client."""
    fn = _lib.opamp_core_setup
    fn.restype = ctypes.c_int32
    fn.argtypes = _call
    return _run(fn, data=call)
