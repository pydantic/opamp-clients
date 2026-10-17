# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Edges of the client's own layer, around the core: headers the application gives that can't be
sent as given, stop awaited from another thread's event loop, calls after the loop has closed,
and a state file whose directory isn't there. Each is held the same in both modes."""

from __future__ import annotations

import asyncio
import threading

import pytest
from _servers import MODES, UID, OpampServer, wait_for

from opamp_client import Client, Config, CoreError, Notification, Status, opamp_pb2 as pb

# Headers the core refuses in every mode, and why: it checks every header list it sends by the
# rule an offered header meets, in both modes. aiohttp sent each as given: a name that isn't a token
# (the server reads "A:B: v" as header A, "B: v"), one of the headers the HTTP stack writes itself
# (a second Host; a Content-Length of 0 before a body, which the server reads as the next request;
# a Connection in place of the upgrade's), a value with whitespace at an end (dropped by the
# server), a header given twice, a value that isn't ASCII.
_STACK = "header 1 ({}) is for the HTTP stack to set"
_VALUE = "the value of header 1 (X) holds a control or non-ASCII character, or a space at either end"
REFUSED = [
    ({"A:B": "v"}, "the name of header 1 is not a token"),
    ({"Bad Name": "v"}, "the name of header 1 is not a token"),
    ({"": "v"}, "the name of header 1 is not a token"),
    *(({name: "x"}, _STACK.format(name)) for name in
      ["Host", "Content-Length", "transfer-encoding", "Connection", "Upgrade", "Expect", "TE", "Keep-Alive",
       "Sec-WebSocket-Key"]),
    ({"X": " padded"}, _VALUE),
    ({"X": "padded\t"}, _VALUE),
    ({"X": "a\r\nInjected: 1"}, _VALUE),
    ({"X": "a\x00b"}, _VALUE),
    ({"X": "a\x7fb"}, _VALUE),
    ({"X": "caf\u00e9"}, _VALUE),
    ({"X-A": "1", "x-a": "2"}, "header 2 (x-a) is given twice"),
]


class Recorder:
    """A TCP server that records what each connection sends (for a moment), then closes it."""

    def __init__(self) -> None:
        self.got: list[bytes] = []
        self.port = 0
        self._server: asyncio.Server | None = None

    async def start(self) -> Recorder:
        self._server = await asyncio.start_server(self._serve, "127.0.0.1", 0)
        self.port = self._server.sockets[0].getsockname()[1]
        return self

    async def _serve(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        try:
            self.got.append(await asyncio.wait_for(reader.read(65536), 1))
        except (TimeoutError, ConnectionError):
            pass
        writer.close()

    async def close(self) -> None:
        assert self._server is not None
        self._server.close()
        await self._server.wait_closed()


@pytest.mark.parametrize(("headers", "why"), REFUSED, ids=[repr(h) for h, _ in REFUSED])
@pytest.mark.parametrize("transport", ["http", "ws"])
@pytest.mark.parametrize("mode", MODES)
def test_a_header_that_cant_be_sent_as_given_fails_the_request_and_nothing_is_sent(
        mode: str, transport: str, headers: dict[str, str], why: str) -> None:
    async def main() -> None:
        server = await Recorder().start()
        notes: list[Notification] = []
        client = Client.start(Config(endpoint=f"{transport}://127.0.0.1:{server.port}/v1/opamp", mode=mode,
                                     instance_uid=UID, headers=headers), Status(), notes.append)
        await wait_for(lambda: any(n.kind == "connect_failed" for n in notes), 5)
        await asyncio.wait_for(client.stop(), 10)
        await server.close()
        failed = [n for n in notes if n.kind == "connect_failed"]
        assert failed, [n.kind for n in notes]
        assert failed[0].detail == f"not sent: {why}"
        # Not a byte of it: not even the request line.
        assert server.got == []

    asyncio.run(main())


@pytest.mark.parametrize("mode", MODES)
def test_headers_that_pass_the_check_reach_the_server_in_both_modes(mode: str) -> None:
    async def main() -> None:
        server = await Recorder().start()
        client = Client.start(Config(endpoint=f"http://127.0.0.1:{server.port}/v1/opamp", mode=mode, instance_uid=UID,
                                     headers={"Authorization": "Bearer abc", "X-Tenant": "a\tb"}), Status())
        await wait_for(lambda: server.got, 5)
        await asyncio.wait_for(client.stop(), 10)
        await server.close()
        head = server.got[0].split(b"\r\n\r\n")[0].split(b"\r\n")
        assert b"Authorization: Bearer abc" in head
        assert b"X-Tenant: a\tb" in head

    asyncio.run(main())


@pytest.mark.parametrize("transport", ["http", "ws"])
@pytest.mark.parametrize("mode", MODES)
def test_stop_awaited_on_another_threads_loop_stops_the_client_on_its_own(mode: str, transport: str) -> None:
    # An application whose client runs on a loop thread of its own, stopped from its main loop:
    # the stop was fed from the wrong thread (racing the loop's steps), and the await raised (a
    # future of another loop) with the stop half done.
    async def main() -> None:
        server = await OpampServer().start()
        loop = asyncio.get_running_loop()
        notes: list[Notification] = []
        client = Client.start(Config(endpoint=server.endpoint(transport), mode=mode, instance_uid=UID,
                                     heartbeat_interval_ms=50), Status(), notes.append)
        await wait_for(lambda: len(server.arrivals) >= 2)
        outcome: list[object] = []
        thread = threading.Thread(target=lambda: outcome.append(_run(client.stop())))
        thread.start()
        await wait_for(lambda: not thread.is_alive(), 10)
        assert outcome == [None]
        # It stopped on its loop: the goodbye went, stopped was told once, and last.
        assert [n.kind for n in notes].count("stopped") == 1 and notes[-1].kind == "stopped"
        assert server.arrivals and client._finished
        # stop on its own loop returns too, at once; and again from another loop.
        await asyncio.wait_for(client.stop(), 1)
        assert asyncio.get_running_loop() is loop
        thread = threading.Thread(target=lambda: outcome.append(_run(client.stop())))
        thread.start()
        await wait_for(lambda: not thread.is_alive(), 10)
        assert outcome == [None, None]
        await server.close()

    asyncio.run(main())


@pytest.mark.parametrize("mode", MODES)
def test_stop_from_another_loop_raises_what_stopped_the_client(mode: str) -> None:
    class Boom(Exception):
        pass

    def notify(n: Notification) -> None:
        raise Boom("the callback")

    async def main() -> None:
        server = await OpampServer().start()
        client = Client.start(Config(endpoint=server.endpoint("ws"), mode=mode, instance_uid=UID), Status(), notify)
        await wait_for(lambda: client._finished)
        outcome: list[object] = []
        thread = threading.Thread(target=lambda: outcome.append(_run(client.stop())))
        thread.start()
        await wait_for(lambda: not thread.is_alive(), 10)
        assert len(outcome) == 1 and isinstance(outcome[0], Boom)
        with pytest.raises(Boom):
            await client.stop()
        await server.close()

    asyncio.run(main())


def test_stop_from_another_loop_with_the_clients_loop_gone_says_so() -> None:
    client: Client | None = None

    async def start() -> None:
        nonlocal client
        client = Client.start(Config(endpoint="http://127.0.0.1:9/v1/opamp", instance_uid=UID), Status())

    asyncio.run(start())  # the loop ends, and closes, with the client running
    assert client is not None
    with pytest.raises(RuntimeError):
        asyncio.run(asyncio.wait_for(client.stop(), 5))


def test_stop_from_another_loop_says_so_when_the_clients_loop_stops_before_running_it() -> None:
    # The client's loop is running when the stop is handed to it, and stops before it gets to it:
    # the stop waited forever.
    loop = asyncio.new_event_loop()
    started = threading.Event()
    gate = threading.Event()
    client: Client | None = None

    async def start() -> None:
        nonlocal client
        client = Client.start(Config(endpoint="http://127.0.0.1:9/v1/opamp", instance_uid=UID), Status())
        started.set()

    def hold_then_stop() -> None:
        gate.wait(5)
        loop.stop()  # what was handed over while held waits for a next round that never comes

    loop.call_soon(lambda: loop.create_task(start()))
    thread = threading.Thread(target=loop.run_forever)
    thread.start()
    try:
        assert started.wait(5) and client is not None
        loop.call_soon_threadsafe(hold_then_stop)

        async def stop() -> None:
            waiting = asyncio.ensure_future(client.stop())
            await asyncio.sleep(0.2)  # handed over while the loop is held
            gate.set()
            await asyncio.wait_for(waiting, 5)

        with pytest.raises(RuntimeError, match="stopped before the stop was done"):
            asyncio.run(stop())
    finally:
        gate.set()
        thread.join(5)
        loop.close()


@pytest.mark.parametrize("mode", MODES)
def test_calls_after_stop_do_nothing_from_any_thread_even_once_the_loop_is_closed(mode: str) -> None:
    client: Client | None = None

    async def main() -> None:
        nonlocal client
        client = Client.start(Config(endpoint="http://127.0.0.1:9/v1/opamp", mode=mode, instance_uid=UID), Status())
        await client.stop()

    asyncio.run(main())
    assert client is not None
    errors: list[BaseException] = []

    def calls() -> None:
        try:
            client.set_health(pb.ComponentHealth(healthy=True))
            client.set_description(pb.AgentDescription())
            client.send_custom_message(pb.CustomMessage(capability="c", type="t", data=b"x"))
            client.package_removed("p")
        except BaseException as e:  # noqa: BLE001
            errors.append(e)

    calls()  # the loop's thread
    thread = threading.Thread(target=calls)
    thread.start()
    thread.join(10)
    assert errors == []


@pytest.mark.parametrize("mode", MODES)
def test_a_state_file_in_a_directory_that_isnt_there_fails_start(mode: str, tmp_path) -> None:
    # Started, it ran with every save failing unseen: nothing kept, the instance UID made anew at
    # each restart. A file not there yet in a directory that is, is a first start.
    async def main() -> None:
        with pytest.raises(FileNotFoundError):
            Client.start(Config(endpoint="http://127.0.0.1:9/v1/opamp", mode=mode,
                                state_file=str(tmp_path / "missing" / "state")), Status())
        client = Client.start(Config(endpoint="http://127.0.0.1:9/v1/opamp", mode=mode,
                                     state_file=str(tmp_path / "state")), Status())
        await asyncio.wait_for(client.stop(), 10)

    asyncio.run(main())


def _run(coro) -> object:
    """Run `coro` on a new loop in this thread: what it returned or raised."""
    try:
        return asyncio.run(asyncio.wait_for(coro, 10))
    except BaseException as e:  # noqa: BLE001
        return e


# A negative time or size is refused at start by the core, naming the field and the number meant,
# as in every SDK (protobuf's range check refused it here before, in its own words).
@pytest.mark.parametrize(("field", "value"), [("stop_timeout_ms", -5000), ("heartbeat_interval_ms", -1),
                                              ("max_send_bytes", -1), ("download_timeout_ms", -(2 ** 63))])
def test_a_negative_time_or_size_is_refused_at_start(field: str, value: int) -> None:
    async def main() -> None:
        config = Config(endpoint="http://127.0.0.1:9/v1/opamp", instance_uid=UID, **{field: value})
        with pytest.raises(CoreError, match=f"{field} is {value}: negative"):
            Client.start(config, Status())

    asyncio.run(main())


# All of stop takes the stop timeout the core goes by: its start reports it, the default too.
@pytest.mark.parametrize(("given", "want"), [(0, 5.0), (1500, 1.5)])
def test_stop_takes_the_cores_stop_timeout(given: int, want: float) -> None:
    async def main() -> None:
        client = Client.start(Config(endpoint="http://127.0.0.1:9/v1/opamp", instance_uid=UID,
                                     stop_timeout_ms=given), Status())
        assert client._stop_timeout == want
        await asyncio.wait_for(client.stop(), 10)

    asyncio.run(main())
