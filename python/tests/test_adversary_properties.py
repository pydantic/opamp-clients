# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The adversary (testdata/core/opamp_core_adversary.wasm): a hostile core, loaded in place of the real
one, that hands the client effect sequences the real core never makes and checks every input the
client feeds back against spec/shell-contract.md. This is the harness and the local server the
adversary expects: one client per seed, several at once, each against this server, whose every
URL's query says what to do; the adversary reports through the client's own callback.

Each test runs in both modes (``MODES``). In Rust mode the client tells the core it speaks streams,
and each run has a raw server of its own (_adversary_raw.py: ``raw=PORT`` and ``tls=NAME`` in the
endpoint's query), which the adversary opens streams to: it hands the client the stream effects
(stream_open, stream_write, stream_start_tls, stream_splice, download_data, ...), which
src/opamp_client/_streams.py carries out, and the platform's as well (http_request, ws_open,
download), which Rust mode's client passes on to the executor, as a core's saves are: Rust mode's
wiring (Streams is offered every effect first), its stop and its shutdown, with both kinds under
way. Go's harness (go/opamp/adversary_properties_test.go) runs Rust mode the same way.

Seeds ``OPAMP_ADVERSARY_BASE`` (1) to base + ``OPAMP_ADVERSARY_SEEDS`` (24) - 1, in each mode;
``OPAMP_ADVERSARY_SEED=S`` replays one (``-k python`` or ``-k rust`` picks the mode, and
``OPAMP_ADVERSARY_FOCUS`` the production in focus in the main run, if the seed ran with one: the
replay line of a failed seed says both); ``OPAMP_ADVERSARY_WASM`` overrides the module's path."""

from __future__ import annotations

import asyncio
import os
import socket
import ssl
import struct
import tempfile
import time
from dataclasses import dataclass, field
from pathlib import Path

import pytest
import trustme
from _adversary_raw import RawServer
from _adversary_raw import body as raw_body
from aiohttp import WSMsgType, web

from opamp_client import TLS, Client, Config, Notification, Status, opamp_pb2 as pb

PREFIX = "opamp-adversary"
STOP_DEADLINE = 15.0  # stop must return within this
SETTLE = 3.0  # after stop: connections closed, nothing new, downloads unchanged
ASK_DEADLINE = 120.0  # the script asks for stop well within this
AT_ONCE = 4
# The harness's marker (_mark) is answered within this of being sent: a server that never answers
# it fails the seed, and doesn't hang it.
MARKER_DEADLINE = 30.0

# The modes every test runs in: Python mode (aiohttp), and Rust mode (the default).
MODES = ["python", "rust"]

# The productions that trigger a bug this client is known to have, in each mode: the main run
# leaves them out, and test_known_bugs_still_fail checks that each still fails (when one stops
# failing, the bug is fixed: take it off).
KNOWN: dict[str, dict[str, tuple[str, str, str]]] = {
    # None open in either mode. Taken off:
    # - stop_while_opening (S1) and ws_hold_past_timeout (S2) were the duties of a shell
    #   that doesn't carry out cancel: the adversary no longer has them. Their counterparts (a
    #   cancel while connecting, stop at any point) run in the main batch and in
    #   test_cancels_are_honoured; test_lifecycle.py and test_effects.py test them.
    # - headers_odd, limit_huge and limit_zero are fixed: test_fixed_bugs_stay_fixed, and
    #   test_effects.py's test_what_aiohttp_refuses_still_ends_the_effect.
    "python": {},
    "rust": {},
}

# The cancel productions (RULE-SHELL-06), in every run's grammar.
CANCELS = ["cancel_download", "cancel_http", "cancel_http_done", "cancel_store", "cancel_twice",
           "cancel_ws_closing", "cancel_ws_opening", "stop_cancel"]
# And those of the streams and their files, in Rust mode's.
STREAM_CANCELS = ["cancel_file", "cancel_splice", "cancel_stream_open", "cancel_stream_opening",
                  "cancel_stream_resolving"]

# The name the raw server's certificate is for, which the adversary starts TLS under (tls=NAME).
TLS_NAME = "localhost"


@dataclass
class Certs:
    """A batch's CA, as the client is given it, and the raw server's certificate under it (for
    127.0.0.1 and localhost, as test_tls.py's servers')."""

    ca: bytes
    server: ssl.SSLContext


def _certs() -> Certs:
    ca = trustme.CA()
    server = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH)
    ca.issue_cert("127.0.0.1", TLS_NAME).configure_cert(server)
    return Certs(ca.cert_pem.bytes(), server)


def _wasm() -> bytes:
    path = os.environ.get("OPAMP_ADVERSARY_WASM")
    if not path:
        root = next((p for p in Path(__file__).resolve().parents if (p / "testdata" / "core").is_dir()), None)
        assert root is not None, "the repository root (with testdata/core) is not above this test"
        path = str(root / "testdata" / "core" / "opamp_core_adversary.wasm")
    assert os.path.isfile(path), f"the adversary module is missing: {path} (scripts/fetch_core.py fetch puts it there)"
    return Path(path).read_bytes()


def _seeds() -> tuple[list[int], bool]:
    """The seeds, and whether this is a replay of one."""
    if one := os.environ.get("OPAMP_ADVERSARY_SEED"):
        return [int(one)], True
    base = int(os.environ.get("OPAMP_ADVERSARY_BASE", "1"))
    count = int(os.environ.get("OPAMP_ADVERSARY_SEEDS", "24"))
    return list(range(base, base + count)), False


def _body(size: int) -> bytes:
    return bytes(i % 251 for i in range(size))


@dataclass
class RunCounts:
    """What the server saw of one run."""

    open_ws: int = 0
    last_arrival: float = 0.0  # time.monotonic() of the last request or upgrade
    marker: float = 0.0  # and of the harness's marker (GET /marker, sent as stop returned)

    def after_stop(self, stop_returned: float) -> str | None:
        """What breaks RULE-SHELL-07 in when the run's requests and upgrades arrived, stop having
        returned at `stop_returned` (None: nothing does): one that arrived after the marker.

        The marker is a request the harness sends the moment stop returns, on a connection of its
        own. This server's handlers run on the loop every seed's client shares, in the order their
        requests came: what the client sent before stop returned is handled before the marker,
        however long a busy loop takes to get to either. Measured from the moment stop returned, as
        it was, a request already on its way then counted as the client's (Node's harness, built the
        same way, failed so on CI: 171 ms and 348 ms)."""
        if self.last_arrival <= self.marker:
            return None
        return (f"a request or upgrade arrived {(self.last_arrival - stop_returned) * 1000:.0f} ms after stop "
                f"returned, {(self.last_arrival - self.marker) * 1000:.0f} ms after a request the harness sent then")


def _mark(port: int, run: str) -> asyncio.Future[str | None]:
    """Send the harness's marker for `run` on a connection of its own: the request is written
    before this returns. The future gives what went wrong, or None if it was answered 204."""
    try:
        sock = socket.create_connection(("127.0.0.1", port), timeout=MARKER_DEADLINE)
        sock.sendall(f"GET /marker?run={run} HTTP/1.1\r\nHost: x\r\n\r\n".encode())
    except OSError as e:
        sent = asyncio.get_running_loop().create_future()
        sent.set_result(repr(e))
        return sent

    async def answered() -> str | None:
        try:
            reader, writer = await asyncio.open_connection(sock=sock)
            try:
                status = await asyncio.wait_for(reader.readline(), MARKER_DEADLINE)
            finally:
                writer.close()
        except asyncio.TimeoutError:
            return f"not answered within {MARKER_DEADLINE:.0f} s"
        except OSError as e:
            return repr(e)
        return None if status.startswith(b"HTTP/1.1 204 ") else f"answered {status.strip()!r}"

    return asyncio.ensure_future(answered())


class AdversaryServer:
    """The README's local server: POST /h, GET /d, the GET /w upgrade, each doing what its query says."""

    def __init__(self) -> None:
        self.runs: dict[str, RunCounts] = {}
        self.port = 0
        self._runner: web.AppRunner | None = None
        self._sockets: set[web.WebSocketResponse] = set()

    def counts(self, run: str) -> RunCounts:
        return self.runs.setdefault(run, RunCounts())

    def _arrived(self, req: web.Request) -> tuple[RunCounts, dict[str, int]]:
        counts = self.counts(req.query.get("run", ""))
        counts.last_arrival = time.monotonic()
        q = {k: int(v) for k, v in req.query.items() if k != "run" and v.lstrip("-").isdigit()}
        return counts, q

    async def _hold(self, delay: int) -> None:
        # -1: never answer, until the client goes (the handler is cancelled) or 30 s.
        await asyncio.sleep(30 if delay < 0 else delay / 1000)

    async def _answer(self, req: web.Request) -> web.StreamResponse:
        _, q = self._arrived(req)
        if req.method == "POST":
            await req.read()
        await self._hold(q.get("delay", 0))
        if q.get("drop", 0) == 1:
            if req.transport is not None:
                req.transport.abort()
            raise asyncio.CancelledError  # nothing more to send: the connection is gone
        return web.Response(status=q.get("status", 200), body=_body(q.get("size", 0)))

    async def _marker(self, req: web.Request) -> web.StreamResponse:
        # The harness's own, not the client's: when what was sent as stop returned got here.
        self.counts(req.query.get("run", "")).marker = time.monotonic()
        return web.Response(status=204)

    async def _upgrade(self, req: web.Request) -> web.StreamResponse:
        counts, q = self._arrived(req)
        delay = q.get("delay", 0)
        if delay:
            await self._hold(delay)
        if q.get("status", 0):
            return web.Response(status=q["status"])
        transport = req.transport
        ws = web.WebSocketResponse()
        await ws.prepare(req)
        counts.open_ws += 1
        self._sockets.add(ws)
        timers: list[asyncio.Task] = []
        try:
            for _ in range(q.get("push", 0)):
                await ws.send_bytes(bytes(range(16)))
            if close := q.get("close", 0):
                async def close_later() -> None:
                    await asyncio.sleep(close / 1000)
                    await ws.close(code=1000)
                timers.append(asyncio.ensure_future(close_later()))
            if drop := q.get("drop", 0):
                async def drop_later() -> None:
                    await asyncio.sleep(drop / 1000)
                    if transport is not None:
                        transport.abort()
                timers.append(asyncio.ensure_future(drop_later()))
            async for m in ws:
                if m.type == WSMsgType.BINARY and q.get("echo", 0) == 1:
                    await ws.send_bytes(m.data)
        except (ConnectionError, RuntimeError):
            pass
        finally:
            for t in timers:
                t.cancel()
            counts.open_ws -= 1
            self._sockets.discard(ws)
        return ws

    async def start(self) -> AdversaryServer:
        app = web.Application()
        app.router.add_post("/h", self._answer)
        app.router.add_get("/d", self._answer)
        app.router.add_get("/w", self._upgrade)
        app.router.add_get("/marker", self._marker)
        # A handler whose client went away is cancelled (a held answer ends with its client).
        self._runner = web.AppRunner(app, handler_cancellation=True, access_log=None)
        await self._runner.setup()
        site = web.TCPSite(self._runner, "127.0.0.1", 0)
        await site.start()
        self.port = site._server.sockets[0].getsockname()[1]  # type: ignore[union-attr]
        return self

    async def close(self) -> None:
        for ws in list(self._sockets):
            await ws.close()
        if self._runner is not None:
            await self._runner.cleanup()


@dataclass
class SeedResult:
    seed: int
    problems: list[str] = field(default_factory=list)
    verdict: dict[str, str] | None = None

    def used(self) -> set[str]:
        return set(filter(None, (self.verdict or {}).get("used", "").split(",")))

    def enabled(self) -> set[str]:
        return set(filter(None, (self.verdict or {}).get("enabled", "").split(",")))


def _parse_verdict(detail: str) -> dict[str, str]:
    return dict(kv.split("=", 1) for kv in detail[len(PREFIX) + len(" verdict "):].split(" ") if "=" in kv)


def _listing(path: str) -> dict[str, tuple[int, int]]:
    try:
        return {e.name: (e.stat().st_size, e.stat().st_mtime_ns) for e in os.scandir(path)}
    except FileNotFoundError:
        return {}


async def run_seed(server: AdversaryServer, wasm: bytes, seed: int, *, mode: str, off: list[str],
                   focus: str | None = None, steps: int | None = None, raw: RawServer | None = None,
                   ca: bytes | None = None) -> SeedResult:
    """One client, in `mode`, against the adversary with `seed`, checked as the README's harness says.
    `raw`: the run's raw server, which the adversary opens streams to (Rust mode), and `ca` the CA
    of its certificate. `steps`: the script's length, if not the adversary's own (30; 45 with
    streams)."""
    result = SeedResult(seed)
    problems = result.problems
    query = f"seed={seed}&off={','.join(off)}"
    if steps is not None:
        query += f"&steps={steps}"
    if focus:
        query += f"&focus={focus}"
    if raw is not None:
        query += f"&raw={raw.port}&tls={TLS_NAME}"
    loop = asyncio.get_running_loop()
    with tempfile.TemporaryDirectory(prefix=f"opamp-adversary-{seed}-") as d:
        state_file = os.path.join(d, "state")
        returned = False
        active = 0
        stopped: list[Notification] = []
        last_note = 0
        verdicts: list[dict[str, str]] = []
        asked = asyncio.Event()
        stop_called: list[float] = []
        stops: list[asyncio.Future] = []
        client: Client | None = None

        def call_outside() -> None:
            # From another thread: the client carries it out on the loop (known bug S8).
            client.set_health(pb.ComponentHealth(healthy=True, status="outside"))  # type: ignore[union-attr]

        async def stop_outside() -> None:
            await client.stop()  # type: ignore[union-attr]

        def notify(n: Notification) -> None:
            nonlocal active, last_note
            active += 1
            try:
                if active > 1:
                    problems.append(f"two callbacks at once ({n.kind})")
                if not returned:
                    problems.append(f"a callback before start returned ({n.kind})")
                if stopped:
                    problems.append(f"a callback after stopped ({n.kind} {n.detail[:200]!r})")
                if n.kind == "stopped":
                    stopped.append(n)
                    return
                if n.kind != "bad_message" or not n.detail.startswith(PREFIX + " "):
                    return
                what = n.detail[len(PREFIX) + 1:]
                if what.startswith("note "):
                    k = int(what[5:])
                    if k != last_note + 1:
                        problems.append(f"note {k} after note {last_note}")
                    last_note = max(last_note, k)
                elif what == "call inside":
                    # "inside": the adversary checks this call reaches it before anything the
                    # client takes in next (RULE-SHELL-10).
                    client.set_health(pb.ComponentHealth(healthy=True, status="inside"))  # type: ignore[union-attr]
                elif what == "call outside":
                    loop.run_in_executor(None, call_outside)
                elif what in ("stop inside", "stop outside"):
                    if not stop_called:
                        stop_called.append(time.monotonic())
                        # Inside: requested, not waited for (that would deadlock). Outside: another task.
                        stops.append(asyncio.ensure_future(client.stop() if what == "stop inside"  # type: ignore[union-attr]
                                                           else stop_outside()))
                    asked.set()
                elif what.startswith("violation"):
                    problems.append(n.detail)
                elif what.startswith("verdict"):
                    verdicts.append(_parse_verdict(n.detail))
            finally:
                active -= 1

        endpoint = f"http://127.0.0.1:{server.port}/opamp?{query}"
        # In either mode the adversary hands the client platform effects, for the executor's aiohttp;
        # with a raw server, the stream effects too, and TLS on them under the batch's CA.
        client = Client.start(Config(mode=mode, endpoint=endpoint, instance_uid=seed.to_bytes(16, "big"), state_file=state_file,
                                     tls=TLS(ca_pem=ca) if ca is not None else None),
                              Status(), notify, _core_module=wasm)
        returned = True
        # To name the seed of an unhandled exception.
        _SEEDS.update({id(client): seed, id(client._x): seed, id(client._streams): seed})
        try:
            await asyncio.wait_for(asked.wait(), ASK_DEADLINE)
        except asyncio.TimeoutError:
            if not stopped:
                problems.append(f"the script never asked for stop within {ASK_DEADLINE:.0f} s")
        if not stop_called:
            stop_called.append(time.monotonic())
            stops.append(asyncio.ensure_future(client.stop()))
        stop_returned = None
        try:
            left = STOP_DEADLINE - (time.monotonic() - stop_called[0])
            await asyncio.wait_for(asyncio.shield(stops[0]), max(left, 0.01))
            stop_returned = time.monotonic()
        except asyncio.TimeoutError:
            problems.append(f"stop didn't return within {STOP_DEADLINE:.0f} s")
        except Exception as e:  # noqa: BLE001
            stop_returned = time.monotonic()
            problems.append(f"the client stopped with an error: {e!r}")
        if stop_returned is not None:
            # Sent now, before anything else is awaited, on a connection of its own: what the client
            # sent before stop returned reaches the server's handler before this does.
            marked = _mark(server.port, str(seed))
            raw_marked = None
            if raw is not None:
                raw_marked = raw.mark()
                # From here a connection the server was told not to read is read: only that tells
                # it the connection has gone.
                raw.release()
            # Every later stop gives the same.
            try:
                await asyncio.wait_for(client.stop(), STOP_DEADLINE)
            except Exception as e:  # noqa: BLE001
                if not any("stopped with an error" in p for p in problems):
                    problems.append(f"a second stop raised {e!r}")
            downloads = state_file + ".downloads"
            before = _listing(downloads)
            counts = server.counts(str(seed))
            for _ in range(int(SETTLE * 100)):
                if counts.open_ws == 0:
                    break
                await asyncio.sleep(0.01)
            await asyncio.sleep(max(0.0, stop_returned + SETTLE - time.monotonic()))
            if counts.open_ws:
                problems.append(f"{counts.open_ws} WebSocket connection(s) still open {SETTLE:.0f} s after stop")
            # A request the client had sent before stop returned may reach the server's handler
            # after it, but not after the marker sent then has.
            if (marker_error := await marked) is not None:
                problems.append(f"the harness's marker request failed: {marker_error}")
            elif (late := counts.after_stop(stop_returned)) is not None:
                problems.append(late)
            if raw is not None and raw_marked is not None:
                # The streams: every connection gone, and none made after stop returned.
                if (raw_error := await raw_marked) is not None:
                    problems.append(f"the harness's marker on the raw server failed: {raw_error}")
                elif (late := raw.after_mark()) is not None:
                    problems.append(late)
                still, anonymous = raw.open()
                if still:
                    problems.append(f"RULE-SHELL-07: {still} stream connection(s) still open {SETTLE:.0f} s after "
                                    f"stop returned ({anonymous} never said who they were)")
            if (after := _listing(downloads)) != before:
                problems.append(f"downloads changed after stop: {before} -> {after}")
        if len(stopped) != 1:
            problems.append(f"stopped {len(stopped)} times")
        for n in stopped:
            if n.error is not None:
                problems.append(f"stopped with an error: {n.error!r}")
        if len(verdicts) != 1:
            problems.append(f"{len(verdicts)} verdicts")
        else:
            result.verdict = v = verdicts[0]
            if v.get("violations") != "0":
                problems.append(f"the verdict counts {v.get('violations')} violations")
            stored = v.get("stored", "-")
            if stored != "-":
                try:
                    with open(state_file, "rb") as f:
                        data = f.read()
                except FileNotFoundError:
                    data = None
                # RULE-SHELL-09: what was last answered stored, or the data of a later store (one
                # cancelled, or cut off by stop, while it was writing: it lands whole, unreported).
                if data != bytes.fromhex(stored) and not _later_store(data, bytes.fromhex(stored), seed):
                    problems.append(f"the state file holds {data.hex() if data is not None else 'nothing'}, the last "
                                    f"stored is {stored}")
        # Let what is still finishing raise now, while the seed is known.
        await asyncio.sleep(0)
        import gc
        gc.collect()
    return result


def _store_id(data: bytes | None, seed: int) -> int | None:
    """The adversary's store number in `data`, written as "<PREFIX> seed <seed> store <n> ..."."""
    prefix = f"{PREFIX} seed {seed} store ".encode()
    if data is None or not data.startswith(prefix):
        return None
    n = data[len(prefix):].split(b" ", 1)[0]
    return int(n) if n.isdigit() else None


def _later_store(data: bytes | None, stored: bytes, seed: int) -> bool:
    """Whether `data` is a store the adversary issued after the one last answered stored."""
    n, m = _store_id(data, seed), _store_id(stored, seed)
    return n is not None and m is not None and n > m


def _seed_of(exc: BaseException | None) -> int | None:
    """The seed whose client raised `exc` in one of its tasks (an effect left without its input)."""
    tb = exc.__traceback__ if exc is not None else None
    while tb is not None:
        if (seed := _SEEDS.get(id(tb.tb_frame.f_locals.get("self")))) is not None:
            return seed
        tb = tb.tb_next
    return None


_SEEDS: dict[int, int] = {}  # id() of a running client's parts -> its seed


async def run_batch(seeds: list[int], **kw) -> list[SeedResult]:
    wasm = _wasm()
    certs = _certs()
    server = await AdversaryServer().start()
    gate = asyncio.Semaphore(AT_ONCE)
    results = {s: SeedResult(s) for s in seeds}
    stray: list[str] = []

    def unhandled(_loop, ctx: dict) -> None:
        # An exception nothing in the client caught: whatever it was doing is never reported to the
        # core (RULE-SHELL-05: every effect ends, whatever the platform throws).
        exc = ctx.get("exception")
        text = f"unhandled in the client: {ctx.get('message')}: {exc!r}"
        seed = _seed_of(exc)
        (results[seed].problems if seed in results else stray).append(text)

    asyncio.get_running_loop().set_exception_handler(unhandled)

    async def one(seed: int) -> None:
        async with gate:
            # In Rust mode the run has a raw server of its own, for its streams.
            raw = await RawServer(certs.server).start() if kw["mode"] == "rust" else None
            try:
                r = await run_seed(server, wasm, seed, raw=raw, ca=certs.ca if raw is not None else None, **kw)
            finally:
                if raw is not None:
                    await raw.close()
            results[seed].problems[:0] = r.problems
            results[seed].verdict = r.verdict

    try:
        await asyncio.gather(*(one(s) for s in seeds))
    finally:
        await server.close()
    if stray:
        results[seeds[0]].problems.append(f"(seed unknown: replay each seed alone) {stray}")
    return [results[s] for s in seeds]


def _replay(seed: int, mode: str, focus: str | None = None) -> str:
    """How to run `seed` alone, in `mode` (every test here has its mode in its id), with `focus` in
    focus in the main run."""
    if focus:
        return (f"replay: OPAMP_ADVERSARY_SEED={seed} OPAMP_ADVERSARY_FOCUS={focus} uv run --python 3.13 pytest -q "
                f"tests/test_adversary_properties.py -k 'keeps_the_shell_contract and {mode}'")
    return (f"replay: OPAMP_ADVERSARY_SEED={seed} uv run --python 3.13 pytest -q tests/test_adversary_properties.py "
            f"-k {mode}")


# covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-04, RULE-SHELL-05, RULE-SHELL-07,
# covers: RULE-SHELL-08, RULE-SHELL-09, RULE-SHELL-10
@pytest.mark.parametrize("mode", MODES)
def test_the_client_keeps_the_shell_contract_against_the_adversary(mode: str) -> None:
    seeds, replay = _seeds()
    focus = os.environ.get("OPAMP_ADVERSARY_FOCUS") or None
    results = asyncio.run(run_batch(seeds, mode=mode, off=sorted(KNOWN[mode]), focus=focus))
    failures = [f"seed {r.seed}:\n    " + "\n    ".join(r.problems) + f"\n    {_replay(r.seed, mode, focus)}"
                for r in results if r.problems]
    assert not failures, f"{len(failures)} of {len(results)} seeds failed:\n" + "\n".join(failures)
    if replay or focus:
        return
    used = set().union(*(r.used() for r in results))
    enabled = set().union(*(r.enabled() for r in results))
    # A production is drawn at random, and runs only if the run's timing leaves it room, so one can
    # miss a batch by chance (with Rust mode's streams the grammar is half as large again): each
    # missed one runs in focus, on two seeds past the batch's, as in Java's, Node's and Go's
    # harnesses.
    last = seeds[-1]
    still = []
    for production in sorted(enabled - used):
        again = asyncio.run(run_batch([last + 1, last + 2], mode=mode, off=sorted(KNOWN[mode]), focus=production))
        failures = [f"seed {r.seed}, {production} in focus:\n    " + "\n    ".join(r.problems)
                    + f"\n    {_replay(r.seed, mode, production)}" for r in again if r.problems]
        assert not failures, "\n".join(failures)
        if not any(production in r.used() for r in again):
            still.append(production)
    assert not still, (f"productions that never ran in seeds {seeds[0]} to {last}, nor in focus in {last + 1} and "
                       f"{last + 2}: {still}")


async def _get(port: int, run: str, sock: socket.socket | None = None) -> None:
    """GET /d for `run` on a connection of its own (`sock`: one that has already sent it), answered."""
    if sock is None:
        sock = socket.create_connection(("127.0.0.1", port))
        sock.sendall(f"GET /d?run={run} HTTP/1.1\r\nHost: x\r\n\r\n".encode())
    reader, writer = await asyncio.open_connection(sock=sock)
    try:
        assert (await asyncio.wait_for(reader.readline(), 10)).startswith(b"HTTP/1.1 200 ")
    finally:
        writer.close()


# covers: RULE-SHELL-07
def test_an_arrival_is_judged_against_the_marker() -> None:
    """The harness's own measure of what arrived after stop: a request sent before stop returned
    isn't the client's to answer for however late the server takes it, and one sent after stop
    returned still is. (`pytest -s` shows what each was told.)"""

    async def main() -> None:
        server = await AdversaryServer().start()
        try:
            # Sent before stop returned, and the loop the server shares then busy for 400 ms: by
            # the moment stop returned it arrived late, by the marker it didn't.
            before = socket.create_connection(("127.0.0.1", server.port))
            before.sendall(b"GET /d?run=late HTTP/1.1\r\nHost: x\r\n\r\n")
            stop_returned = time.monotonic()
            marked = _mark(server.port, "late")
            time.sleep(0.4)
            assert await marked is None
            await _get(server.port, "late", before)
            counts = server.counts("late")
            late = counts.last_arrival - stop_returned
            assert late > 0.05, f"the server was to be late: it took the request {late * 1000:.0f} ms after stop"
            said = counts.after_stop(stop_returned)
            print(f"a request sent before stop returned, taken {late * 1000:.0f} ms after it: {said or 'passes'}")
            assert said is None

            # Sent after stop returned: 100 ms after the marker sent then was answered.
            stop_returned = time.monotonic()
            assert await _mark(server.port, "after") is None
            await asyncio.sleep(0.1)
            await _get(server.port, "after")
            said = server.counts("after").after_stop(stop_returned)
            print(f"a request sent 100 ms after stop returned: {said or 'passes'}")
            assert said is not None, "a request sent 100 ms after stop returned passed"
        finally:
            await server.close()

    asyncio.run(main())


# The raw server does what its first line says, and tells what it saw of a connection (as Go's
# TestAdversaryRawServer).
# covers: RULE-SHELL-07
def test_the_raw_server_does_what_its_first_line_says() -> None:
    async def main() -> None:
        certs = _certs()
        server = await RawServer(certs.server).start()
        opened: list[asyncio.StreamWriter] = []

        async def dial(line: str) -> tuple[asyncio.StreamReader, asyncio.StreamWriter]:
            reader, writer = await asyncio.open_connection("127.0.0.1", server.port)
            opened.append(writer)
            writer.write(line.encode() + b"\n")
            ok = await asyncio.wait_for(reader.readline(), 20)
            assert ok == b"ok\n", f"{line!r}: answered {ok!r}"
            return reader, writer

        async def line(reader: asyncio.StreamReader) -> bytes:
            return await asyncio.wait_for(reader.readline(), 20)

        async def ask(tag: int) -> bytes:
            reader, _ = await dial(f"opamp-raw tag={1000 + tag} ask={tag} wait=5000")
            return await line(reader)

        def reset(writer: asyncio.StreamWriter) -> None:
            writer.get_extra_info("socket").setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
            writer.transport.abort()

        try:
            # send, in pieces, then its side closed: the pattern, then the end.
            reader, _ = await dial("opamp-raw tag=1 send=1000 chunk=300 gap=1 end=fin")
            assert await asyncio.wait_for(reader.read(), 20) == raw_body(1000) == _body(1000)

            # echo, and what it read: all of the pattern, then the peer's end.
            reader, writer = await dial("opamp-raw tag=2 echo=1")
            writer.write(raw_body(5000))
            assert await asyncio.wait_for(reader.readexactly(5000), 20) == raw_body(5000)
            writer.write_eof()
            assert await ask(2) == b"fin 5000 0\n", "after the pattern and a close"

            # Bytes that aren't the pattern's, and a reset.
            _, writer = await dial("opamp-raw tag=3")
            writer.write(bytes([0, 1, 9]))
            await asyncio.sleep(0.1)
            reset(writer)
            assert await ask(3) == b"reset 3 1\n", "after three bytes, one wrong, and a reset"

            # One still open is said to be, once the wait is over; one never seen is none.
            await dial("opamp-raw tag=4")
            reader, _ = await dial("opamp-raw tag=1004 ask=4 wait=200")
            assert await line(reader) == b"open 0 0\n", "a connection still open"
            assert await ask(99) == b"none 0 0\n", "a connection never made"

            # A connection that never says who it is counts until it goes.
            _, anonymous = await asyncio.open_connection("127.0.0.1", server.port)
            opened.append(anonymous)
            reader, _ = await dial("opamp-raw tag=1005 ask=0 wait=300")
            assert await line(reader) == b"open 1 0\n", "with a connection that never said who it was"
            assert server.open()[1] == 1
            anonymous.close()
            assert await ask(0) == b"none 0 0\n", "once it has gone"

            # TLS after ok, under the certificate's name; then the pattern inside it, and a close.
            reader, writer = await dial("opamp-raw tag=6 tls=1 send=100 end=close")
            await writer.start_tls(TLS(ca_pem=certs.ca).context(), server_hostname=TLS_NAME)
            assert await asyncio.wait_for(reader.read(), 20) == raw_body(100)
            # And what it reads inside it, to the peer's end of it.
            reader, writer = await dial("opamp-raw tag=11 tls=1 echo=1")
            await writer.start_tls(TLS(ca_pem=certs.ca).context(), server_hostname=TLS_NAME)
            writer.write(raw_body(3000))
            assert await asyncio.wait_for(reader.readexactly(3000), 20) == raw_body(3000)
            writer.close()
            assert await ask(11) == b"fin 3000 0\n", "inside TLS: the pattern, then its close"

            # read=0 reads nothing until the run is released; delay=-1 never answers; reset is one.
            _, writer = await dial("opamp-raw tag=7 read=0")
            writer.write(raw_body(10))
            writer.write_eof()
            reader, _ = await dial("opamp-raw tag=1007 ask=7 wait=200")
            assert await line(reader) == b"open 0 0\n", "read=0 before the release"
            server.release()
            assert await ask(7) == b"fin 10 0\n", "read=0 after the release"
            reader, _ = await dial("opamp-raw tag=8 end=reset after=50")
            with pytest.raises(ConnectionError):
                await asyncio.wait_for(reader.read(), 20)
            reader, silent = await asyncio.open_connection("127.0.0.1", server.port)
            opened.append(silent)
            silent.write(b"opamp-raw tag=9 delay=-1\n")
            with pytest.raises(asyncio.TimeoutError):
                await asyncio.wait_for(reader.read(1), 0.2)

            # The marker: a connection accepted after it is late, one before it isn't; and the
            # marker is no connection of the run's.
            assert "never got" in (server.after_mark() or "")
            still = server.open()
            assert await server.mark() is None
            assert server.after_mark() is None, "nothing came after the marker"
            assert server.open() == still
            await asyncio.sleep(0.1)
            await dial("opamp-raw tag=10")
            assert "RULE-SHELL-07" in (server.after_mark() or ""), "a connection made after the marker passed"
        finally:
            for writer in opened:
                writer.transport.abort()
            await server.close()

    asyncio.run(main())


@pytest.mark.parametrize("mode, production", [(mode, p) for mode in MODES for p in sorted(KNOWN[mode])])
def test_known_bugs_still_fail(mode: str, production: str) -> None:
    bug, why, says = KNOWN[mode][production]
    others = sorted(set(KNOWN[mode]) - {production})
    results = asyncio.run(run_batch([1, 2, 3, 4], mode=mode, off=others, focus=production))
    ran = [r for r in results if production in r.used()]
    assert ran, f"{production} never ran in seeds 1-4 with focus: the check can't tell"
    failed = [r for r in ran if any(says in p for p in r.problems)]
    assert failed, (f"{production} ({bug}: {why}) no longer fails with {says!r} in seeds {[r.seed for r in ran]} "
                    f"(they say {[r.problems for r in ran]}): the bug looks fixed; take it off KNOWN[{mode!r}] in "
                    "tests/test_adversary_properties.py")


# covers: RULE-SHELL-06
@pytest.mark.parametrize("mode, production", [(mode, p) for mode in MODES
                                              for p in CANCELS + (STREAM_CANCELS if mode == "rust" else [])])
def test_cancels_are_honoured(mode: str, production: str) -> None:
    results = asyncio.run(run_batch([1, 2], mode=mode, off=sorted(KNOWN[mode]), focus=production, steps=10))
    for r in results:
        assert r.verdict, f"seed {r.seed}: no verdict: {r.problems}"
        assert production in r.enabled(), f"seed {r.seed}: {production} isn't enabled: {sorted(r.enabled())}"
    failures = [f"seed {r.seed}: {r.problems} ({_replay(r.seed, mode)})" for r in results if r.problems]
    assert not failures, "\n".join(failures)


# Fixed on sdk-shells, each with the seed that showed it. limit_zero's seed delivered messages over
# a limit of 0; headers_odd's and limit_huge's left the effect unanswered (the harness saw the task's
# exception only while nothing held the task; test_effects.py tests them directly).
@pytest.mark.parametrize("production, seed", [("limit_zero", 1), ("headers_odd", 2), ("limit_huge", 1)])
@pytest.mark.parametrize("mode", MODES)
def test_fixed_bugs_stay_fixed(mode: str, production: str, seed: int) -> None:
    (r,) = asyncio.run(run_batch([seed], mode=mode, off=[], focus=production))
    assert production in r.used(), f"{production} didn't run with seed {seed}"
    assert not r.problems, f"{r.problems} ({_replay(seed, mode)})"
