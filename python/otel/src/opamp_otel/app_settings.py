# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The application's own settings, as the OpAMP server sends them (docs/app-settings.md).

The server sends them beside the SDK's settings, in the ``custom`` map of the remote config's
``elastic`` entry: string keys to string values, which the SDK doesn't read but hands on. The
application reads them here::

    from opamp_otel import app_settings

    if app_settings.get("checkout.new_flow", "off") == "on":
        ...
    verbose = app_settings.get_bool("example.verbose", False)

    def changed(settings, keys):
        logging.getLogger("app").setLevel(settings.get("example.log_level", "INFO"))

    unsubscribe = app_settings.on_change(changed)

- ``get(key, default)`` is the value in force, or ``default`` while there is none: before the first
  config arrives (unless one was saved before a restart), when the config in force leaves the key
  out, and without an OpAMP server at all. It never raises. Every value is a string.
- ``on_change(callback)``: ``callback(settings, changed)`` after a change is put in force, with the
  whole map in force (read-only) and the keys added, changed or removed. Not called when a config
  leaves the map as it was. Callbacks run one at a time, in the order they were added, on the
  thread that put the change in force (the OpAMP client's): keep them quick. One that raises is
  logged, and the rest still run.

Thread-safe: any thread may read, add or remove a callback at any time.
"""

from __future__ import annotations

import logging
import math
import re
import threading
from collections.abc import Callable, Mapping
from types import MappingProxyType

__all__ = ["AppSettings", "get", "get_bool", "get_float", "get_int", "on_change", "snapshot"]

_log = logging.getLogger("opamp_otel")

#: A callback: the map in force, and the keys that changed.
Callback = Callable[[Mapping[str, str], frozenset[str]], object]

_INT = re.compile(r"[+-]?[0-9]+", re.ASCII)
# A JSON number (a leading + too): what every language reads the same way.
_FLOAT = re.compile(r"[+-]?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?", re.ASCII)
_EMPTY: Mapping[str, str] = MappingProxyType({})
#: The spaces a typed read takes round a value: every language trims these, and only these.
_SPACES = " \t\r\n"


def _text(value: object) -> str | None:
    return value.strip(_SPACES) if isinstance(value, str) else None


class AppSettings:
    """The application's settings in force, and who to tell when they change. The package keeps
    one (the module's functions); a test may make its own."""

    def __init__(self) -> None:
        self._values: Mapping[str, str] = _EMPTY
        #: Guards the callbacks list.
        self._lock = threading.Lock()
        #: Held while a change is put in force and told: changes are told one at a time, in order.
        self._telling = threading.RLock()
        self._callbacks: list[Callback] = []

    def get(self, key: str, default: str | None = None) -> str | None:
        """The value of `key` in force, or `default` when there is none. Never raises."""
        try:
            return self._values.get(key, default)
        except Exception:  # noqa: BLE001 - an unhashable key, say: there is no such setting
            return default

    def get_bool(self, key: str, default: bool) -> bool:
        """`true` or `false` in any ASCII case, spaces round it allowed; `default` for anything
        else (``on``, ``1`` and ``yes`` too)."""
        text = _text(self.get(key))
        if text is not None and text.isascii():
            return {"true": True, "false": False}.get(text.lower(), default)
        return default

    def get_int(self, key: str, default: int) -> int:
        """A decimal integer (a sign allowed, spaces round it) in the 64-bit signed range;
        `default` for anything else."""
        text = _text(self.get(key))
        if text is not None and _INT.fullmatch(text):
            n = int(text)
            if -(2**63) <= n < 2**63:
                return n
        return default

    def get_float(self, key: str, default: float) -> float:
        """A number as JSON writes one (a sign allowed, spaces round it), finite; `default` for
        anything else."""
        text = _text(self.get(key))
        if text is not None and _FLOAT.fullmatch(text):
            f = float(text)
            if math.isfinite(f):
                return f
        return default

    def snapshot(self) -> Mapping[str, str]:
        """Every setting in force, read-only: one consistent map, whatever changes after."""
        return self._values

    def on_change(self, callback: Callback) -> Callable[[], None]:
        """Call `callback(settings, changed)` after each change put in force (not for the values
        in force now: read those with snapshot()). Returns a function that removes it; calling that
        again does nothing. The same callback added twice is called twice."""
        entry = _Entry(callback)
        with self._lock:
            self._callbacks.append(entry)

        def remove() -> None:
            with self._lock:
                if entry in self._callbacks:
                    self._callbacks.remove(entry)

        return remove

    def put(self, values: Mapping[str, str]) -> None:
        """Put `values` in force (the layer's Settings.custom), and tell the callbacks if anything
        changed. The package calls it; an application doesn't."""
        new = dict(values)
        with self._telling:
            old = self._values
            changed = frozenset(k for k in old.keys() | new.keys() if old.get(k) != new.get(k))
            if not changed:
                return
            self._values = MappingProxyType(new) if new else _EMPTY
            _log.info("OpAMP: application settings changed: %s", ", ".join(sorted(changed)))
            with self._lock:
                callbacks = list(self._callbacks)
            for entry in callbacks:
                try:
                    entry.callback(self._values, changed)
                except Exception:  # noqa: BLE001 - the application's bug: the client goes on
                    _log.exception("OpAMP: an application settings callback (%s) raised",
                                   getattr(entry.callback, "__qualname__", repr(entry.callback)))


class _Entry:
    """A callback as added: each addition its own, so removing one leaves the same callback added
    again."""

    __slots__ = ("callback",)

    def __init__(self, callback: Callback) -> None:
        self.callback = callback


#: The package's: what the server's config puts in force.
SETTINGS = AppSettings()

get = SETTINGS.get
get_bool = SETTINGS.get_bool
get_int = SETTINGS.get_int
get_float = SETTINGS.get_float
snapshot = SETTINGS.snapshot
on_change = SETTINGS.on_change
