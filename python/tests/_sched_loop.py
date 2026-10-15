# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""A seeded asyncio event loop for schedule-exploring tests, and an in-memory network.

``SeededLoop`` is a selector event loop whose

- **order is drawn from a seed**: at each turn, the ready tasks' steps are shuffled among the
  plain callbacks (which keep their own order: a transport's connection_made before its data,
  a future's state before what waits on it), so which task goes first is the seed's choice;
- **clock is virtual**: ``time()`` stands still while anything can run, and moves to the next
  timer only when nothing can: nothing ready and no thread at work. A test drives it with
  ``await loop.idle(advance)``: run until nothing more can happen, letting ``advance`` seconds
  of virtual time pass (timers due meanwhile fire, in order), then return. Run freely (``auto``)
  it moves to the next timer whenever it is idle, after ``io_grace`` real seconds for real
  sockets, if there are any;
- **threads are part of the schedule**: every ``run_in_executor`` (and so ``asyncio.to_thread``,
  ``getaddrinfo`` and the client's state-file writer) is counted from submission until the loop
  has its result, and the loop waits for them at each turn: a thread's work happens between two
  turns, its results arrive in submission order, and the clock never moves past a thread at
  work. A thread may sleep in virtual time (``thread_sleep``): a slow disk, say. Executors are
  taken to have one worker each (the loop's own has; so has the client's writer): while an
  executor's job sleeps, the jobs queued behind it wait too, and don't hold the clock;
- **owns everything**: ``call_soon``, ``call_at`` and ``create_task`` from any thread but the
  loop's are recorded in ``wrong_thread`` (a call that should have been marshalled, known bug S8).

Given the same seed and the same inputs, a run is the same run: the seed is all a failure needs
to replay (with the test's own steps).

``Network`` is an in-memory TCP: with ``loop.network`` set, ``create_connection`` connects to it
(the network decides when, and whether: ``Network.accept``), ``start_tls`` is a no-op (the bytes
stay as they are) and ``getaddrinfo`` answers at once. Both ends are ``MemTransport``s, each
delivering what the other writes through the loop."""

from __future__ import annotations

import asyncio
import heapq
import random
import socket
import threading
from collections import deque
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from typing import Any

THREAD_WAIT_S = 60.0  # a thread at work this long (real time) is stuck


class Stuck(RuntimeError):
    """Nothing can happen, and the loop was asked to run on."""


class SeededLoop(asyncio.SelectorEventLoop):
    def __init__(self, seed: int, *, auto: bool = False, io_grace: float = 0.0, shuffle: bool = True) -> None:
        self._vnow = 1000.0
        super().__init__()
        self.seed = seed
        self.rng = random.Random(seed)
        self.auto, self.io_grace, self.shuffle = auto, io_grace, shuffle
        self._cond = threading.Condition()
        self._sleepers: set[threading.Event] = set()  # threads asleep in virtual time
        self._work: dict[int, list[int]] = {}  # per executor: [jobs submitted and not over, sleeping]
        self._seq = 0
        self._arrivals: list[tuple[int, int, asyncio.Handle]] = []  # (job, n, handle) from threads
        self._sleeps: list[tuple[int, float, Any]] = []  # (job, seconds, (wake, its executor's work))
        self._local = threading.local()
        self._waiter: asyncio.Future | None = None
        self._target = 0.0
        self.network: Network | None = None
        #: Calls made from a thread other than the loop's (call_soon, call_at, create_task).
        self.wrong_thread: list[str] = []
        #: How many turns ran, tasks were shuffled past a callback, and threads were waited for.
        self.turns = self.reorders = self.jobs = 0
        # One worker: two threads' work is done in submission order, whatever the OS does.
        self.set_default_executor(ThreadPoolExecutor(max_workers=1, thread_name_prefix="seeded"))

    # -- the clock

    def time(self) -> float:
        return self._vnow

    def idle(self, advance: float = 0.0) -> asyncio.Future:
        """A future done once nothing more can happen within `advance` seconds from now (virtual),
        and that time has passed."""
        assert self._waiter is None, "one idle() at a time"
        self._waiter = self.create_future()
        self._target = self._vnow + advance
        return self._waiter

    # -- threads

    def run_in_executor(self, executor, func, *args):
        self._check_closed()
        if executor is None:
            executor = self._default_executor
        with self._cond:
            self._seq += 1
            job = self._seq
            work = self._work.setdefault(id(executor), [0, 0])
            work[0] += 1
            self.jobs += 1
        local = self._local

        def run():
            local.job, local.work = job, work
            return func(*args)

        cf = executor.submit(run)
        fut = asyncio.wrap_future(cf, loop=self)
        # After wrap_future's own callback: the result is on its way to the loop before the
        # thread counts as done.
        cf.add_done_callback(lambda _cf: self._parked(job, work))
        return fut

    def _parked(self, job: int, work: list[int]) -> None:
        if getattr(self._local, "job", None) == job:
            self._local.job = None
        with self._cond:
            work[0] -= 1
            self._cond.notify_all()
        if not self.is_closed():
            self._write_to_self()

    def thread_sleep(self, seconds: float) -> None:
        """Called on a thread the loop runs work on: sleep `seconds` of virtual time."""
        wake = threading.Event()
        job = getattr(self._local, "job", None)
        assert job is not None, "thread_sleep is for work run by run_in_executor"
        work = self._local.work
        with self._cond:
            self._sleepers.add(wake)
            self._sleeps.append((job, seconds, (wake, work)))
            work[1] = 1
            self._cond.notify_all()
        self._write_to_self()
        wake.wait()

    def _wake(self, sleeper: tuple[threading.Event, list[int]]) -> None:
        wake, work = sleeper
        with self._cond:
            work[1] = 0
            self._sleepers.discard(wake)
        wake.set()

    def call_soon_threadsafe(self, callback, *args, context=None):
        job = getattr(self._local, "job", None)
        if job is not None and threading.get_ident() != self._thread_id:
            # A thread's result: it joins the ready queue in submission order, at the next turn.
            handle = asyncio.Handle(callback, args, self, context)
            with self._cond:
                self._arrivals.append((job, len(self._arrivals), handle))
            self._write_to_self()
            return handle
        return super().call_soon_threadsafe(callback, *args, context=context)

    def _busy(self) -> int:
        """Threads at work, or about to be (not asleep in virtual time, nor queued behind one)."""
        return sum(n for n, asleep in self._work.values() if not asleep)

    @property
    def busy(self) -> int:
        with self._cond:
            return self._busy()

    # -- owned by the loop's thread

    def _owned(self, what: str) -> None:
        if self._thread_id is not None and threading.get_ident() != self._thread_id:
            self.wrong_thread.append(f"{what} from thread {threading.current_thread().name}")

    def call_soon(self, callback, *args, context=None):
        self._owned(f"call_soon({getattr(callback, '__qualname__', callback)})")
        return super().call_soon(callback, *args, context=context)

    def call_at(self, when, callback, *args, context=None):
        self._owned(f"call_at({getattr(callback, '__qualname__', callback)})")
        return super().call_at(when, callback, *args, context=context)

    def create_task(self, coro, **kwargs):
        self._owned(f"create_task({getattr(coro, '__qualname__', coro)})")
        return super().create_task(coro, **kwargs)

    # -- one turn

    def _run_once(self) -> None:
        with self._cond:
            if not self._cond.wait_for(lambda: self._busy() <= 0, THREAD_WAIT_S):
                raise Stuck(f"a thread has been at work for {THREAD_WAIT_S} s")
            arrivals, self._arrivals = sorted(self._arrivals, key=lambda a: a[:2]), []
            sleeps, self._sleeps = sorted(self._sleeps, key=lambda s: s[0]), []
        for _job, _n, handle in arrivals:
            self._ready.append(handle)
        for _job, seconds, wake in sleeps:
            super().call_at(self._vnow + seconds, self._wake, wake)
        if not self._ready and not self._stopping:
            self._when_idle()
        if self.shuffle:
            self._reorder()
        if not self.auto and not self._ready and not self._stopping:
            # Nothing would wake a select: no I/O is real here.
            due = [h for h in self._scheduled if not h._cancelled and h._when <= self._vnow]
            if not due:
                raise Stuck(f"idle at {self._vnow} with nothing due: waiter {self._waiter!r}, "
                            f"next timer {min((h._when for h in self._scheduled if not h._cancelled), default=None)}")
        self.turns += 1
        super()._run_once()

    def _when_idle(self) -> None:
        """Nothing is ready and no thread is at work: let virtual time pass, or say so."""
        while self._scheduled and self._scheduled[0]._cancelled:
            self._timer_cancelled_count -= 1
            heapq.heappop(self._scheduled)._scheduled = False
        nxt = self._scheduled[0]._when if self._scheduled else None
        if nxt is not None and nxt <= self._vnow:
            return  # due now
        if self._waiter is not None:
            if nxt is not None and nxt <= self._target:
                self._vnow = nxt
                return
            self._vnow = max(self._vnow, self._target)
            waiter, self._waiter = self._waiter, None
            if not waiter.done():
                waiter.set_result(None)
            return
        if self.auto:
            if self.io_grace > 0:
                events = self._selector.select(self.io_grace)
                if events:
                    self._process_events(events)
                    return
            if nxt is not None:
                self._vnow = nxt
                return
            if self.io_grace > 0:
                return  # real sockets only: wait for them
        raise Stuck("nothing is ready, no timer is set and no thread is at work, and nothing waits for idle()")

    def _reorder(self) -> None:
        """Shuffle the ready tasks' steps among the plain callbacks (whose order stays). A task
        woken by a future moves with the future's other callbacks, in their order: they are one
        event (the future's completion), and asyncio runs a future's callbacks in the order they
        were added."""
        ready = self._ready
        if len(ready) < 2:
            return
        units: list[list[asyncio.Handle]] = []
        for h in ready:
            done = _completed(h)
            if done is not None and units and _completed(units[-1][-1]) is done:
                units[-1].append(h)
            else:
                units.append([h])
        tasks, plain = [], []
        for u in units:
            (tasks if any(isinstance(getattr(h._callback, "__self__", None), asyncio.Task) for h in u)
             else plain).append(u)
        if not tasks or (len(tasks) == 1 and not plain):
            return
        rng = self.rng
        rng.shuffle(tasks)
        out: list[asyncio.Handle] = []
        i = j = 0
        while i < len(plain) or j < len(tasks):
            if j < len(tasks) and (i == len(plain) or rng.randrange(len(plain) - i + len(tasks) - j) < len(tasks) - j):
                out += tasks[j]
                j += 1
            else:
                out += plain[i]
                i += 1
        if out != list(ready):
            self.reorders += 1
        ready.clear()
        ready.extend(out)

    # -- the network

    async def create_connection(self, protocol_factory, host=None, port=None, **kwargs):
        if self.network is None:
            return await super().create_connection(protocol_factory, host, port, **kwargs)
        return await self.network.connect(protocol_factory, host, port, kwargs)

    async def start_tls(self, transport, protocol, sslcontext, **kwargs):
        if self.network is None:
            return await super().start_tls(transport, protocol, sslcontext, **kwargs)
        return await self.network.start_tls(transport, protocol, kwargs)

    async def getaddrinfo(self, host, port, **kwargs):
        if self.network is None:
            return await super().getaddrinfo(host, port, **kwargs)
        return [(socket.AF_INET, socket.SOCK_STREAM, 6, "", ("10.0.0.1", port or 0))]

    # -- teardown

    def finish(self) -> None:
        """Cancel what is left, and close."""
        try:
            tasks = [t for t in asyncio.all_tasks(self) if not t.done()]
            for t in tasks:
                t.cancel()
            if tasks:
                self.auto = True
                self.run_until_complete(asyncio.gather(*tasks, return_exceptions=True))
        finally:
            self.close()
            # Threads still asleep in virtual time: their time won't come now.
            with self._cond:
                sleepers, self._sleepers = self._sleepers, set()
            for wake in sleepers:
                wake.set()
            if self._default_executor is not None:
                self._default_executor.shutdown(wait=True, cancel_futures=True)


def _completed(h: asyncio.Handle) -> asyncio.Future | None:
    """The future whose done callback `h` is, if it is one (called with the done future alone)."""
    args = h._args
    if args is not None and len(args) == 1 and isinstance(args[0], asyncio.Future) and args[0].done():
        return args[0]
    return None


class MemTransport(asyncio.Transport):
    """One end of an in-memory TCP connection: what is written arrives at the other end through
    the loop, in order; close is a FIN after it, abort a reset."""

    def __init__(self, loop: SeededLoop, name: str, peername: tuple[str, int]) -> None:
        super().__init__()
        self._loop = loop
        self.name = name
        self.peer: MemTransport | None = None
        self.protocol: asyncio.BaseProtocol | None = None
        self.closing = False  # close() or abort() called, or the connection is gone
        self.lost = False  # connection_lost has been called
        self.aborted = False
        self.written = bytearray()
        self._paused = False
        self._inbox: deque[bytes | None] = deque()  # None: the peer's FIN
        self._extra = {"peername": peername, "sockname": ("10.0.0.9", 40000), "socket": None}

    def get_extra_info(self, name, default=None):
        return self._extra.get(name, default)

    def set_protocol(self, protocol) -> None:
        self.protocol = protocol

    def get_protocol(self):
        return self.protocol

    def is_closing(self) -> bool:
        return self.closing

    def write(self, data) -> None:
        if self.closing or not data:
            return
        data = bytes(data)
        self.written += data
        if self.peer is not None:
            self._loop.call_soon(self.peer._arrive, data)

    def writelines(self, lines) -> None:
        self.write(b"".join(bytes(x) for x in lines))

    def write_eof(self) -> None:
        if not self.closing and self.peer is not None:
            self._loop.call_soon(self.peer._arrive, None)

    def can_write_eof(self) -> bool:
        return True

    def get_write_buffer_size(self) -> int:
        return 0

    def get_write_buffer_limits(self) -> tuple[int, int]:
        return (0, 0)

    def set_write_buffer_limits(self, high=None, low=None) -> None:
        pass

    def pause_reading(self) -> None:
        self._paused = True

    def resume_reading(self) -> None:
        if self._paused:
            self._paused = False
            self._loop.call_soon(self._drain)

    def is_reading(self) -> bool:
        return not self._paused and not self.closing

    def _arrive(self, data: bytes | None) -> None:
        if self.lost:
            return
        self._inbox.append(data)
        self._drain()

    def _drain(self) -> None:
        while self._inbox and not self._paused and not self.lost:
            data = self._inbox.popleft()
            if data is None:
                keep = self.protocol.eof_received()  # type: ignore[union-attr]
                if not keep:
                    self.close()
            else:
                self.protocol.data_received(data)  # type: ignore[union-attr]

    def close(self) -> None:
        if self.closing:
            return
        self.closing = True
        if self.peer is not None:
            self._loop.call_soon(self.peer._arrive, None)
        self._loop.call_soon(self._lose, None)

    def abort(self) -> None:
        if self.lost:
            return
        self.closing = self.aborted = True
        if self.peer is not None and not self.peer.lost:
            self._loop.call_soon(self.peer._lose, ConnectionResetError("reset by peer"))
        self._loop.call_soon(self._lose, None)

    def _lose(self, exc: BaseException | None) -> None:
        if self.lost:
            return
        self.lost = self.closing = True
        self._inbox.clear()
        if self.protocol is not None:
            self.protocol.connection_lost(exc)


class Network:
    """In-memory TCP for a SeededLoop. `serve(host, port)` makes the server's protocol for a new
    connection; `accept(attempt)` decides each connection attempt (call `attempt.result(...)`):
    by default, every attempt is parked in `attempts` for the test to settle."""

    def __init__(self, loop: SeededLoop, serve: Callable[[str, int], asyncio.BaseProtocol], *,
                 accept_all: bool = False) -> None:
        self.loop = loop
        self.serve = serve
        self.accept_all = accept_all  # every attempt connects at once
        self.late = 0  # attempts that connected after their cancel
        self.attempts: list[Attempt] = []
        #: Every transport made, client ends and server ends.
        self.transports: list[MemTransport] = []
        self.tls: list[tuple[MemTransport, str]] = []

    async def connect(self, factory, host: str, port: int, kwargs: dict[str, Any]):
        """As loop.create_connection: the protocol hears connection_made, then (a turn later) the
        caller gets the transport; a cancel meanwhile closes it. Some attempts (the seed's
        choice, a quarter) finish despite a cancel, as one already connected can: the caller
        gets a connection it gave up on, and must release it."""
        attempt = Attempt(self.loop.create_future(), host, port)
        self.attempts.append(attempt)
        late = self.loop.rng.random() < 0.25
        if self.accept_all:
            attempt.decide("accept")
        try:
            how = await asyncio.shield(attempt.decision)
        except asyncio.CancelledError:
            if not late:
                raise
            how = "accept"  # it had connected: the cancel came too late
            self.late += 1
        finally:
            attempt.settled = True
        if how != "accept":
            raise ConnectionRefusedError(111, f"connection to {host}:{port} refused")
        client = MemTransport(self.loop, "client", (host, port))
        server = MemTransport(self.loop, "server", ("10.0.0.9", 40000))
        client.peer, server.peer = server, client
        self.transports += [client, server]
        server_proto = self.serve(host, port)
        server.set_protocol(server_proto)
        proto = factory()
        client.set_protocol(proto)
        self.loop.call_soon(server_proto.connection_made, server)
        proto.connection_made(client)
        try:
            await asyncio.sleep(0)  # asyncio's waiter for connection_made
        except asyncio.CancelledError:
            if not late:
                client.close()  # as asyncio does
                raise
        return client, proto

    async def start_tls(self, transport: MemTransport, protocol, kwargs: dict[str, Any]):
        self.tls.append((transport, kwargs.get("server_hostname") or ""))
        await asyncio.sleep(0)
        if transport.closing:
            raise ConnectionResetError("the connection closed during the TLS handshake")
        transport.set_protocol(protocol)
        return transport

    def open(self) -> list[MemTransport]:
        """The client ends not yet closed."""
        return [t for t in self.transports if t.name == "client" and not t.lost]


class Attempt:
    """A connection attempt waiting for the network's decision."""

    def __init__(self, decision: asyncio.Future, host: str, port: int) -> None:
        self.decision = decision
        self.host, self.port = host, port
        self.settled = False

    @property
    def waiting(self) -> bool:
        return not self.settled and not self.decision.done()

    def decide(self, how: str) -> None:
        if not self.decision.done():
            self.decision.set_result(how)
