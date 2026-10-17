# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Servers for the TLS and proxy tests: an OpAMP server (plain or TLS, over HTTP and WebSocket)
that records how each message arrived, an HTTP proxy and a SOCKS5 proxy, both logging what they
were asked for."""

from __future__ import annotations

import asyncio
import base64
import ipaddress
import ssl
from collections.abc import Coroutine
from dataclasses import dataclass
from typing import Any

from aiohttp import WSMsgType, web

from opamp_client import Client, Config, Notification, Status, anyvalue_pb2
from opamp_client import opamp_pb2 as pb

#: Who speaks HTTP and WebSocket (Config.mode): aiohttp, or the core over asyncio's streams.
MODES = ["python", "rust"]

UID = bytes([0x42] * 16)
DESCRIPTION = pb.AgentDescription(identifying_attributes=[
    anyvalue_pb2.KeyValue(key="service.name", value=anyvalue_pb2.AnyValue(string_value="test"))])


@dataclass
class Arrival:
    """How a message reached the server."""

    port: int  # the client side's port: the proxy's, through a proxy
    tls: str | None  # "TLSv1.3", say
    client_cn: str | None  # the client certificate's common name


class OpampServer:
    """Answers every message, over HTTP and WebSocket, and records how each arrived."""

    def __init__(self, ssl_context: ssl.SSLContext | None = None, *, remote_config: pb.AgentRemoteConfig | None = None,
                 hold_upgrade: float = 0) -> None:
        self.ssl = ssl_context
        # Offered in every answer (the client hears it once: the hash doesn't change).
        self.remote_config = remote_config
        # How long a WebSocket upgrade waits before it is accepted, to stop a client mid-handshake.
        self.hold_upgrade = hold_upgrade
        self.arrivals: list[Arrival] = []
        self.port = 0
        self._runner: web.AppRunner | None = None

    def _arrival(self, req: web.Request) -> Arrival:
        assert req.transport is not None
        tls = req.transport.get_extra_info("ssl_object")
        cn = None
        if tls is not None and (cert := tls.getpeercert()):
            cn = next((v for rdn in cert["subject"] for k, v in rdn if k == "commonName"), None)
        return Arrival(req.transport.get_extra_info("peername")[1], tls.version() if tls else None, cn)

    async def _post(self, req: web.Request) -> web.Response:
        arrival = self._arrival(req)
        msg = pb.AgentToServer.FromString(await req.read())
        self.arrivals.append(arrival)
        return web.Response(body=self._answer(msg).SerializeToString(), content_type="application/x-protobuf")

    def _answer(self, msg: pb.AgentToServer) -> pb.ServerToAgent:
        return pb.ServerToAgent(instance_uid=msg.instance_uid, remote_config=self.remote_config)

    async def _ws(self, req: web.Request) -> web.WebSocketResponse:
        arrival = self._arrival(req)
        ws = web.WebSocketResponse()
        if self.hold_upgrade:
            await asyncio.sleep(self.hold_upgrade)
            if req.transport is None or req.transport.is_closing():
                return ws  # the client gave up on it
        await ws.prepare(req)
        async for m in ws:
            if m.type == WSMsgType.BINARY:
                msg = pb.AgentToServer.FromString(m.data[1:])
                self.arrivals.append(arrival)
                await ws.send_bytes(b"\0" + self._answer(msg).SerializeToString())
        return ws

    async def start(self) -> OpampServer:
        app = web.Application()
        app.router.add_post("/v1/opamp", self._post)
        app.router.add_get("/v1/opamp", self._ws)
        app.router.add_get("/file", self._file)
        self._runner = web.AppRunner(app)
        await self._runner.setup()
        site = web.TCPSite(self._runner, "127.0.0.1", 0, ssl_context=self.ssl)
        await site.start()
        self.port = site._server.sockets[0].getsockname()[1]  # type: ignore[union-attr]
        return self

    def endpoint(self, transport: str, host: str = "127.0.0.1") -> str:
        scheme = transport + ("s" if self.ssl else "")
        return f"{scheme}://{host}:{self.port}/v1/opamp"

    # A package file.
    BODY = b"package" * 100

    async def _file(self, req: web.Request) -> web.Response:
        self.arrivals.append(self._arrival(req))
        return web.Response(body=self.BODY)

    async def close(self) -> None:
        if self._runner is not None:
            await self._runner.cleanup()


class _Proxy:
    """What both proxies share: a listener, and tunnels whose upstream ports are recorded."""

    def __init__(self, user: str | None, password: str | None, ssl_context: ssl.SSLContext | None = None) -> None:
        self.user, self.password = user, password
        self.ssl = ssl_context
        self.log: list[dict] = []  # one entry per request: what was asked, and the answer
        self.upstream_ports: set[int] = set()
        self.port = 0
        self._server: asyncio.Server | None = None
        self._writers: set[asyncio.StreamWriter] = set()

    async def start(self):
        self._server = await asyncio.start_server(self._serve, "127.0.0.1", 0, ssl=self.ssl)
        self.port = self._server.sockets[0].getsockname()[1]
        return self

    async def _serve(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        self._writers.add(writer)
        try:
            await self.handle(reader, writer)
        except (ConnectionError, OSError, asyncio.IncompleteReadError):
            pass
        finally:
            writer.close()
            self._writers.discard(writer)

    async def handle(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        raise NotImplementedError

    async def tunnel(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter, host: str,
                     port: int) -> list[Coroutine[Any, Any, None]]:
        """Connect to host:port; the two copies to run once the client has been answered."""
        up_reader, up_writer = await asyncio.open_connection(host, port)
        self._writers.add(up_writer)
        self.upstream_ports.add(up_writer.get_extra_info("sockname")[1])

        async def copy(r: asyncio.StreamReader, w: asyncio.StreamWriter) -> None:
            try:
                while data := await r.read(65536):
                    w.write(data)
                    await w.drain()
            except (ConnectionError, OSError):
                pass
            finally:
                w.close()

        return [copy(reader, up_writer), copy(up_reader, writer)]

    async def close(self) -> None:
        if self._server is not None:
            self._server.close()
            for w in list(self._writers):
                w.close()
            await self._server.wait_closed()


class HttpProxy(_Proxy):
    """CONNECT only (the client tunnels every connection); requests in absolute form are logged
    and refused. With credentials set, Basic ones are required."""

    async def handle(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        head = (await reader.readuntil(b"\r\n\r\n")).decode("latin-1").split("\r\n")
        method, target, _ = head[0].split(" ", 2)
        headers = {k.lower(): v.strip() for k, _, v in (line.partition(":") for line in head[1:] if line)}
        entry = {"method": method, "target": target, "headers": headers, "user": None}
        self.log.append(entry)
        auth = headers.get("proxy-authorization", "")
        if auth.startswith("Basic "):
            entry["user"], _, entry["password"] = base64.b64decode(auth[6:]).decode().partition(":")
        if method != "CONNECT":
            entry["answer"] = 400
            writer.write(b"HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            return
        if self.user is not None and (entry["user"], entry.get("password")) != (self.user, self.password):
            entry["answer"] = 407
            writer.write(b"HTTP/1.1 407 Proxy Authentication Required\r\n"
                         b"Proxy-Authenticate: Basic realm=\"test\"\r\nContent-Length: 0\r\n\r\n")
            return
        host, _, port = target.rpartition(":")
        copies = await self.tunnel(reader, writer, host.strip("[]"), int(port))
        entry["answer"] = 200
        writer.write(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        await asyncio.gather(*copies)


class Socks5Proxy(_Proxy):
    """SOCKS5 CONNECT, requiring the username and password when they are set (RFC 1929)."""

    async def handle(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        version, n = await reader.readexactly(2)
        assert version == 5
        methods = await reader.readexactly(n)
        entry: dict = {"methods": list(methods), "user": None}
        self.log.append(entry)
        if self.user is not None:
            if 2 not in methods:
                writer.write(b"\x05\xff")
                return
            writer.write(b"\x05\x02")
            _, ulen = await reader.readexactly(2)
            user = (await reader.readexactly(ulen)).decode()
            (plen,) = await reader.readexactly(1)
            password = (await reader.readexactly(plen)).decode()
            entry["user"] = user
            if (user, password) != (self.user, self.password):
                writer.write(b"\x01\x01")
                entry["answer"] = "refused"
                return
            writer.write(b"\x01\x00")
        else:
            writer.write(b"\x05\x00")
        _, cmd, _, kind = await reader.readexactly(4)
        if kind == 1:
            host = str(ipaddress.IPv4Address(await reader.readexactly(4)))
        elif kind == 4:
            host = str(ipaddress.IPv6Address(await reader.readexactly(16)))
        else:
            (n,) = await reader.readexactly(1)
            host = (await reader.readexactly(n)).decode()
        port = int.from_bytes(await reader.readexactly(2), "big")
        entry.update(cmd=cmd, address_type=kind, host=host, port=port)
        copies = await self.tunnel(reader, writer, host, port)
        entry["answer"] = "ok"
        writer.write(b"\x05\x00\x00\x01" + bytes(4) + bytes(2))
        await asyncio.gather(*copies)


async def wait_for(cond, seconds: float = 10) -> None:
    for _ in range(int(seconds * 100)):
        if cond():
            return
        await asyncio.sleep(0.01)


async def run_client(server: OpampServer, endpoint: str, *, messages: int = 3, **config) -> list[Notification]:
    """Run a client until the server has `messages` messages from it, or it fails to connect;
    then stop it. The notifications."""
    notes: list[Notification] = []
    client = Client.start(Config(endpoint=endpoint, instance_uid=UID, heartbeat_interval_ms=50, **config),
                          Status(description=DESCRIPTION), notes.append)
    await wait_for(lambda: len(server.arrivals) >= messages or any(n.kind == "connect_failed" for n in notes))
    await client.stop()
    return notes
