# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The matchers the settings that match telemetry run, once per span or log record: too often to
call into the SDK settings layer, so each language has its own, held to the layer's reference
matchers (the SDK settings core's rules) by the shared test vectors
(testdata/sdk-settings) and by the layer's Evaluate call. Only `*` globs, exact names
and prefixes: no regular expressions, whose dialects differ between languages."""

from __future__ import annotations

import math
import threading
import time
from collections.abc import Callable, Mapping

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
    """A rate cap's token bucket, the algorithm every language runs
    (testdata/sdk-settings/caps.json pins it): full at its first take; refilled at `per_second` a
    second on a monotonic clock, up to max(per_second, 1); a take of n gets the whole tokens there
    are, up to n. In floats, in the layer's order. Thread-safe."""

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
    """drop_attributes and mask_attributes, then scrubbing: what happens to an attribute, by its
    key, then to what is kept, by its value. Redaction patterns as the layer hands them over
    (lowercase), matched ignoring ASCII case; a key both match is dropped. Decisions are kept per
    key: an application has few attribute keys."""

    KEEP, DROP, MASK = 0, 1, 2
    _CACHE = 4096

    def __init__(self, drop: tuple[str, ...], mask: tuple[str, ...], scrubber: Scrubber | None = None) -> None:
        self.drop, self.mask = drop, mask
        self.scrubber = scrubber if scrubber is not None else Scrubber(False, ())
        self._seen: dict[str, int] = {}

    def __bool__(self) -> bool:
        return bool(self.drop or self.mask or self.scrubber)

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

    def apply(self, attributes) -> dict | None:
        """The attributes as exported: dropped ones gone, masked ones MASK, the rest scrubbed, in
        their order; None when none changes."""
        if not attributes or not self:
            return None
        out, changed = {}, False
        for k, v in attributes.items():
            what = self.of(k)
            if what == self.KEEP:
                r = self.scrubber.value(k, v) if self.scrubber else Scrubber._UNCHANGED
                if r is Scrubber._UNCHANGED:
                    out[k] = v
                else:
                    out[k], changed = r, True
            elif what == self.MASK:
                out[k], changed = MASK, True
            else:
                changed = True
        return out if changed else None

    def body(self, body):
        """A log record's body as exported (scrubbed), or Scrubber._UNCHANGED."""
        return self.scrubber.value(None, body) if self.scrubber and body is not None else Scrubber._UNCHANGED


#: The keys whose values scrubbing never touches, at any depth: the Logfire SDK's SAFE_KEYS
#: (logfire 5.2.0). Each is matched exactly, case and all.
SAFE_KEYS = frozenset({
    "agent_session_id", "binary_content", "code.filepath", "code.function", "code.lineno", "db.plan",
    "db.query.text", "db.statement", "do_not_scrub", "error.type", "event.name", "exception.message",
    "exception.stacktrace", "exception.type", "fastapi.route.name", "fastapi.route.operation_id",
    "gen_ai.conversation.id", "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.provider.name",
    "gen_ai.request.model", "gen_ai.response.model", "gen_ai.system", "gen_ai.system_instructions",
    "gen_ai.tool.call.id", "gen_ai.tool.definitions", "gen_ai.tool.name",
    "gen_ai.usage.cache_creation.input_tokens", "gen_ai.usage.cache_read.input_tokens",
    "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.raw", "http.method",
    "http.route", "http.scheme", "http.status_code", "http.target", "http.url",
    "langsmith.metadata.session_id", "langsmith.trace.session_name", "logfire.config",
    "logfire.json_schema", "logfire.level_name", "logfire.level_num", "logfire.logger_name", "logfire.msg",
    "logfire.msg_template", "logfire.package_versions", "logfire.pending_parent_id", "logfire.sample_rate",
    "logfire.scrubbed", "logfire.span_type", "logfire.tags", "logfire.version", "model_request_parameters",
    "pydantic_ai.all_messages", "rpc.method", "url.full", "url.path", "url.query",
})

# The kinds of the Logfire SDK's default patterns: the word as it is; `auth` unless `or` or `ors`
# and a word boundary follow (author); two words with at most one of `. _ -` and a space between
# (api_key); `pylf_v`, digits, `_` (a Logfire token's prefix); the word on its own, a word boundary
# or a `_` (part of the match) on each side.
_WORD, _AUTH, _PAIR, _TOKEN, _ACRONYM = range(5)

#: The Logfire SDK's default patterns (logfire 5.2.0's DEFAULT_PATTERNS), in its order: at a
#: position, the first that matches there is the match. Written out instead of as a regex, so that
#: every language matches alike (testdata/sdk-settings/scrub.json holds them to the layer).
_DEFAULTS = (
    (_WORD, "password", ""), (_WORD, "passwd", ""), (_WORD, "mysql_pwd", ""), (_WORD, "secret", ""),
    (_AUTH, "", ""), (_WORD, "credential", ""), (_PAIR, "private", "key"), (_PAIR, "api", "key"),
    (_WORD, "session", ""), (_WORD, "cookie", ""), (_PAIR, "social", "security"), (_PAIR, "credit", "card"),
    (_PAIR, "logfire", "token"), (_TOKEN, "", ""), (_ACRONYM, "csrf", ""), (_ACRONYM, "xsrf", ""),
    (_ACRONYM, "jwt", ""), (_ACRONYM, "ssn", ""),
)

# What else a pattern's lowercase letter matches, as Python's re.IGNORECASE has it.
_ALSO = {"i": "İı", "s": "ſ", "k": "K"}


def _same_letter(p: str, c: str) -> bool:
    """Whether the text's character `c` is the pattern's character `p`: a lowercase ASCII letter
    also matches its uppercase, and `i`, `s` and `k` what re.IGNORECASE adds; others only
    themselves."""
    if not ("a" <= p <= "z"):
        return p == c
    return c == p or c == p.upper() or c in _ALSO.get(p, "")


def _is_word(t: str, i: int) -> bool:
    """Whether t[i] is a word character (an ASCII letter, digit or `_`); outside t, none."""
    if i < 0 or i >= len(t):
        return False
    c = t[i]
    return "a" <= c <= "z" or "A" <= c <= "Z" or "0" <= c <= "9" or c == "_"


def _boundary(t: str, at: int) -> bool:
    return _is_word(t, at - 1) != _is_word(t, at)


def _word_at(t: str, at: int, w: str) -> int:
    """Where `w` ends if it is at t[at:], else -1."""
    if at + len(w) > len(t):
        return -1
    for i, p in enumerate(w):
        if not _same_letter(p, t[at + i]):
            return -1
    return at + len(w)


def _char(t: str, i: int) -> str:
    return t[i] if 0 <= i < len(t) else ""


def _default_at(t: str, at: int, kind: int, a: str, b: str) -> int:  # noqa: PLR0911
    """Where the default pattern ends if it matches at t[at:], else -1; where it could match two
    ways, the way Python's regex tries first (a separator, then none)."""
    if kind == _WORD:
        return _word_at(t, at, a)
    if kind == _AUTH:
        end = _word_at(t, at, "auth")
        if end < 0:
            return -1
        orr = _word_at(t, end, "or")
        if orr >= 0 and (_same_letter("s", _char(t, orr)) and _boundary(t, orr + 1) or _boundary(t, orr)):
            return -1
        return end
    if kind == _PAIR:
        mid = _word_at(t, at, a)
        if mid < 0:
            return -1
        if _char(t, mid) in (".", "_", " ", "-"):
            end = _word_at(t, mid + 1, b)
            if end >= 0:
                return end
        return _word_at(t, mid, b)
    if kind == _TOKEN:
        j = _word_at(t, at, "pylf_v")
        if j < 0:
            return -1
        digits = j
        while "0" <= _char(t, j) <= "9":
            j += 1
        return j + 1 if j > digits and _char(t, j) == "_" else -1

    def after(end: int) -> int:
        if end < 0:
            return -1
        if _boundary(t, end):
            return end
        return end + 1 if _char(t, end) == "_" else -1

    if _boundary(t, at):
        end = after(_word_at(t, at, a))
        if end >= 0:
            return end
    return after(_word_at(t, at + 1, a)) if _char(t, at) == "_" else -1


def find_default(t: str) -> tuple[int, int] | None:
    """The first match of a default pattern in `t`, as Python's re.search finds it: the leftmost
    start, and there the first pattern in their order. Its start and end."""
    for at in range(len(t)):
        for kind, a, b in _DEFAULTS:
            end = _default_at(t, at, kind, a, b)
            if end >= 0:
                return at, end
    return None


def scrubbed(why: str) -> str:
    """What a scrubbed value becomes, naming why: the text a default pattern matched, or the
    pattern."""
    return f"[Scrubbed due to '{why}']"


class Scrubber:
    """scrub_defaults and scrub_patterns: the Logfire SDK's default patterns, and globs as the
    layer hands them over (lowercase, sorted), matched ignoring ASCII case against a whole key or
    string value. What a key's value becomes is kept per key: an application has few."""

    KEEP, VALUES = None, ""
    _CACHE = 4096

    def __init__(self, defaults: bool, patterns: tuple[str, ...]) -> None:
        self.defaults, self.patterns = defaults, patterns
        self._keys: dict[str, str | None] = {}

    def __bool__(self) -> bool:
        return bool(self.defaults or self.patterns)

    def key(self, key: str) -> str | None:
        """What becomes of the value of the attribute (or a map's member) `key`: KEEP (None: left
        alone, nested values too), VALUES ("": its strings scrubbed), else the string replacing
        it, whatever its type."""
        if key in self._keys:
            return self._keys[key]
        if not self or key in SAFE_KEYS:
            what = self.KEEP
        else:
            found = find_default(key) if self.defaults else None
            if found is not None:
                what = scrubbed(key[found[0]:found[1]])
            else:
                p = self._pattern(key)
                what = self.VALUES if p is None else scrubbed(p)
        if len(self._keys) < self._CACHE:
            self._keys[key] = what
        return what

    def text(self, text: str) -> str | None:
        """What a string value or a log body becomes, if scrubbed (None: as it is). A default
        pattern scrubs it when it matches part of it (a value all the match, `password` alone,
        says nothing secret, as the Logfire SDK has it); a pattern when it matches it whole."""
        if self.defaults:
            found = find_default(text)
            if found is not None and found != (0, len(text)):
                return scrubbed(text[found[0]:found[1]])
        p = self._pattern(text)
        return None if p is None else scrubbed(p)

    def _pattern(self, text: str) -> str | None:
        if not self.patterns:
            return None
        t = ascii_lower(text)
        return next((p for p in self.patterns if glob(p, t)), None)

    _UNCHANGED = object()

    def value(self, key: str | None, value):
        """A value as scrubbing leaves it (an attribute's, a map member's, a sequence's item, or a
        log body), or _UNCHANGED: `key` is the attribute or member it is the value of (None: a
        body or an item). The value itself is never changed."""
        if key is not None:
            what = self.key(key)
            if what is self.KEEP:
                return self._UNCHANGED
            if what != self.VALUES:
                return what
        if isinstance(value, str):
            r = self.text(value)
            return self._UNCHANGED if r is None else r
        if isinstance(value, (list, tuple)):
            items, changed = list(value), False
            for i, item in enumerate(items):
                r = self.value(None, item)
                if r is not self._UNCHANGED:
                    items[i], changed = r, True
            return (type(value)(items) if isinstance(value, tuple) else items) if changed else self._UNCHANGED
        if isinstance(value, Mapping):
            members, changed = dict(value), False
            for k, v in value.items():
                r = self.value(k, v)
                if r is not self._UNCHANGED:
                    members[k], changed = r, True
            return members if changed else self._UNCHANGED
        return self._UNCHANGED
