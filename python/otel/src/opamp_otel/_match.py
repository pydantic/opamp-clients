# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The matchers the settings that match telemetry run, once per span or log record: too often to
call into the SDK settings layer, so each language has its own, held to the layer's reference
matchers (the SDK settings core's rules) by the shared test vectors
(testdata/sdk-settings) and by the layer's Evaluate call. Only `*` globs, exact names
and prefixes: no regular expressions, whose dialects differ between languages."""

from __future__ import annotations

import math
import threading
import time
from collections.abc import Callable

#: SeverityNumber past every other: the threshold of a scope that is `off`.
SEVERITY_OFF = 0xFFFF
#: What a masked attribute's value becomes, whatever its type.
MASK = "[REDACTED]"

# ASCII letters only: a redaction pattern ignores ASCII case, so É and é stay different.
_ASCII_LOWER = str.maketrans("ABCDEFGHIJKLMNOPQRSTUVWXYZ", "abcdefghijklmnopqrstuvwxyz")


def glob(pattern: str, text: str) -> bool:
    """Whether `text` matches the glob `pattern`: `*` matches any run of characters, none
    included; every other character is itself. By Unicode code points (a Python str's own unit),
    case-sensitively. The layer's algorithm: on a mismatch, the last `*` takes one more character
    and the pattern after it starts again, so at most pattern x text steps."""
    pi = ti = 0
    star = -1  # the pattern after the last `*` seen
    taken = 0  # the text that `*` has taken up to
    while ti < len(text):
        if pi < len(pattern) and pattern[pi] == "*":
            pi += 1
            star, taken = pi, ti
        elif pi < len(pattern) and pattern[pi] == text[ti]:
            pi += 1
            ti += 1
        elif star >= 0:
            taken += 1
            pi, ti = star, taken
        else:
            return False
    return all(c == "*" for c in pattern[pi:])


def ascii_lower(text: str) -> str:
    return text.translate(_ASCII_LOWER)


def scope_matches(pattern: str, scope: str) -> bool:
    """A send_logs_level_by_scope pattern: an exact scope name, or a prefix ending in `*`."""
    return scope.startswith(pattern[:-1]) if pattern.endswith("*") else pattern == scope


def threshold(levels: tuple[tuple[str, int], ...], floor: int | None, scope: str) -> int | None:
    """The lowest SeverityNumber a record of `scope` is exported at: the first entry of `levels`
    (as the layer sorts them: the longest name first, an exact name before a prefix of it) whose
    pattern matches, else `floor` (send_logs_level); None: every record."""
    for pattern, min_severity in levels:
        if scope_matches(pattern, scope):
            return min_severity
    return floor


def level_keeps(threshold_: int | None, severity: int) -> bool:
    """At or above the threshold. A record with no severity (0) can't be judged and is kept,
    except where its scope is `off`."""
    if threshold_ is None:
        return True
    if threshold_ == SEVERITY_OFF:
        return False
    return severity == 0 or severity >= threshold_


def attribute_text(value) -> str | None:
    """The text a sampling rule's glob matches: a string's own, an integer's decimal, true or
    false. A float or a sequence has none (languages print floats differently)."""
    if isinstance(value, str):
        return value
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, int):
        return str(value)
    return None


class Bucket:
    """A rate cap's token bucket, the algorithm every language runs (testdata/caps.json pins it):
    full at its first take; refilled at `per_second` a second on a monotonic clock, up to
    max(per_second, 1); a take of n gets the whole tokens there are, up to n. In floats, in the
    layer's order. Thread-safe."""

    def __init__(self, per_second: float, clock: Callable[[], int] = time.monotonic_ns) -> None:
        self.per_second = per_second
        self._capacity = max(per_second, 1.0)
        self._tokens = self._capacity
        self._last: int | None = None
        self._clock = clock
        self._lock = threading.Lock()

    def take(self, n: int) -> int:
        """Take up to `n` tokens now; how many were taken. A clock that goes back refills nothing."""
        with self._lock:
            now = self._clock()
            last = now if self._last is None else self._last
            elapsed = max(now - last, 0) / 1e9
            self._tokens = min(self._tokens + elapsed * self.per_second, self._capacity)
            self._last = max(last, now)
            taken = min(float(n), math.floor(self._tokens))
            self._tokens -= taken
            return int(taken)


def rebucket(old: Bucket | None, per_second: float | None, clock: Callable[[], int]) -> Bucket | None:
    """The bucket for a cap of `per_second` (None: no cap): the one in force if the cap is the
    same, else a new, full one."""
    if per_second is None:
        return None
    if old is not None and old.per_second == per_second:
        return old
    return Bucket(per_second, clock)


class Redactor:
    """drop_attributes and mask_attributes: what happens to an attribute, by its key. Patterns
    as the layer hands them over (lowercase), matched ignoring ASCII case; a key both match is
    dropped. Decisions are kept per key: an application has few attribute keys."""

    KEEP, DROP, MASK = 0, 1, 2
    _CACHE = 4096

    def __init__(self, drop: tuple[str, ...], mask: tuple[str, ...]) -> None:
        self.drop, self.mask = drop, mask
        self._seen: dict[str, int] = {}

    def __bool__(self) -> bool:
        return bool(self.drop or self.mask)

    def of(self, key: str) -> int:
        what = self._seen.get(key)
        if what is None:
            k = ascii_lower(key)
            if any(glob(p, k) for p in self.drop):
                what = self.DROP
            elif any(glob(p, k) for p in self.mask):
                what = self.MASK
            else:
                what = self.KEEP
            if len(self._seen) < self._CACHE:
                self._seen[key] = what
        return what

    def touches(self, attributes) -> bool:
        """Whether any of these attributes would change."""
        return bool(attributes) and any(self.of(k) != self.KEEP for k in attributes)

    def apply(self, attributes) -> dict:
        """The attributes as exported: dropped ones gone, masked ones MASK, the rest as they were,
        in their order."""
        out = {}
        for k, v in attributes.items():
            what = self.of(k)
            if what == self.KEEP:
                out[k] = v
            elif what == self.MASK:
                out[k] = MASK
        return out
