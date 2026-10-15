# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Package downloads: every response written and hashed, whatever its status; no redirect
followed, a redirect's Location resolved by urllib.parse; nothing left when one fails or is
cancelled."""

from __future__ import annotations

import asyncio
import hashlib
import os
import stat

import pytest
from aiohttp import web

from opamp_client._executor import Executor, downloads_dir, location
from opamp_client._proto.opamp.core.v1 import core_pb2 as corepb

BODY = b"package" * 100


async def _serve() -> tuple[web.AppRunner, str, dict]:
    hits: dict[str, int] = {}
    started, release = asyncio.Event(), asyncio.Event()

    async def moved(request: web.Request) -> web.Response:
        # A body with the redirect: written and hashed like any other.
        return web.Response(status=302, headers={"Location": request.query["to"]}, body=b"moved")

    async def nowhere(_request: web.Request) -> web.Response:
        return web.Response(status=302, body=b"")

    async def file(request: web.Request) -> web.Response:
        hits["/file"] = hits.get("/file", 0) + 1
        if request.headers.get("Authorization") != "t":
            return web.Response(status=401)
        return web.Response(body=BODY)

    async def missing(_request: web.Request) -> web.Response:
        return web.Response(status=404, body=b"not here")

    async def slow(request: web.Request) -> web.StreamResponse:
        resp = web.StreamResponse()
        await resp.prepare(request)
        await resp.write(b"part" * 100)
        started.set()
        await release.wait()
        return resp

    app = web.Application()
    for path, handler in (("/moved", moved), ("/nowhere", nowhere), ("/file", file), ("/missing", missing),
                          ("/slow", slow)):
        app.router.add_get(path, handler)
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    port = site._server.sockets[0].getsockname()[1]  # type: ignore[union-attr]
    return runner, f"http://127.0.0.1:{port}", {"hits": hits, "started": started, "release": release}


async def _deleted(path: str) -> None:
    for _ in range(500):
        if not os.path.exists(path):
            return
        await asyncio.sleep(0.01)
    raise AssertionError(f"{path} is still there")


class _Downloads:
    def __init__(self, state: str, base: str) -> None:
        self.state, self.base = state, base
        self.got: list[corepb.Input] = []
        self.arrived = asyncio.Event()
        self.x = Executor(self._deliver, state)

    def _deliver(self, i: corepb.Input) -> None:
        self.got.append(i)
        self.arrived.set()

    def start(self, id_: int, path: str, max_bytes: int = 1 << 20) -> None:
        self.x.do(corepb.Effect(download=corepb.Download(
            id=id_, url=self.base + path, headers=[corepb.Header(name="Authorization", value="t")],
            max_bytes=max_bytes, timeout_ms=5000)))

    async def get(self, id_: int, path: str, max_bytes: int = 1 << 20) -> corepb.Input:
        self.arrived.clear()
        self.start(id_, path, max_bytes)
        await asyncio.wait_for(self.arrived.wait(), 10)
        return self.got[-1]


def test_downloads(tmp_path) -> None:
    async def run() -> None:
        runner, base, _ = await _serve()
        state = str(tmp_path / "state")
        d = _Downloads(state, base)
        try:
            done = await d.get(1, "/file")
            assert done.WhichOneof("kind") == "download_response"
            r = done.download_response
            assert (r.id, r.status, r.sha256) == (1, 200, hashlib.sha256(BODY).digest())
            assert not r.location and not r.HasField("location_origin")
            with open(d.x.download_path(1), "rb") as f:
                assert f.read() == BODY
            if os.name != "nt":
                assert stat.S_IMODE(os.stat(downloads_dir(state)).st_mode) == 0o700
                assert stat.S_IMODE(os.stat(d.x.download_path(1)).st_mode) == 0o600
            d.x.do(corepb.Effect(discard=corepb.Id(id=1)))
            await _deleted(d.x.download_path(1))  # beside the loop

            # Any status is a response: its body in the file, for the core to judge.
            missing = (await d.get(2, "/missing")).download_response
            assert (missing.status, missing.sha256) == (404, hashlib.sha256(b"not here").digest())
            with open(d.x.download_path(2), "rb") as f:
                assert f.read() == b"not here"

            # Too large, or no response at all: failed, and nothing left.
            too_large = await d.get(3, "/file", 10)
            assert too_large.WhichOneof("kind") == "download_failed"
            assert not os.path.exists(d.x.download_path(3))
            d.base = "http://127.0.0.1:1"
            refused = await d.get(4, "/file")
            assert refused.WhichOneof("kind") == "download_failed"
            assert not os.path.exists(d.x.download_path(4))
        finally:
            await d.x.close()
            await runner.cleanup()

    asyncio.run(run())


def test_a_redirect_is_reported_not_followed(tmp_path) -> None:
    async def run() -> None:
        runner, base, server = await _serve()
        d = _Downloads(str(tmp_path / "state"), base)
        try:
            r = (await d.get(1, "/moved?to=../a/./b/file")).download_response
            assert r.status == 302 and r.sha256 == hashlib.sha256(b"moved").digest()
            assert r.location == base + "/a/b/file"
            port = int(base.rsplit(":", 1)[1])
            assert (r.location_origin.scheme, r.location_origin.host, r.location_origin.port) == (
                "http", "127.0.0.1", port)
            assert server["hits"] == {}, "the redirect is not followed"

            # Without a Location: neither is reported.
            r = (await d.get(2, "/nowhere")).download_response
            assert r.status == 302 and not r.location and not r.HasField("location_origin")
        finally:
            await d.x.close()
            await runner.cleanup()

    asyncio.run(run())


@pytest.mark.parametrize("given, url, origin", [
    ("/file", "http://h.example:8080/file", ("http", "h.example", 8080)),
    ("file", "http://h.example:8080/dir/file", ("http", "h.example", 8080)),
    ("../../x/./y", "http://h.example:8080/x/y", ("http", "h.example", 8080)),
    ("?q=1", "http://h.example:8080/dir/page?q=1", ("http", "h.example", 8080)),
    ("//cdn.example/f", "http://cdn.example/f", ("http", "cdn.example", 0)),
    ("https://Other.Example/f", "https://Other.Example/f", ("https", "other.example", 0)),
    ("https://other.example:443/f", "https://other.example:443/f", ("https", "other.example", 0)),
    ("https://other.example:8443/f", "https://other.example:8443/f", ("https", "other.example", 8443)),
    ("http://[::1]:9/f", "http://[::1]:9/f", ("http", "::1", 9)),
    ("ftp://files.example/f", "ftp://files.example/f", ("ftp", "files.example", 0)),
])
def test_a_location_is_resolved_against_the_url_requested(given: str, url: str, origin: tuple) -> None:
    got, o = location("http://h.example:8080/dir/page", given)
    assert got == url
    assert o is not None and (o.scheme, o.host, o.port) == origin


@pytest.mark.parametrize("given", ["http://[::1/f", "http://h.example:99999/f", "http://h.example:port/f"])
def test_a_location_that_doesnt_resolve_goes_as_given_with_no_origin(given: str) -> None:
    assert location("http://h.example/", given) == (given, None)


def test_a_cancelled_download_is_dropped_its_file_deleted_and_never_reported(tmp_path) -> None:
    async def run() -> None:
        runner, base, server = await _serve()
        state = str(tmp_path / "state")
        d = _Downloads(state, base)
        try:
            d.start(1, "/slow")
            await asyncio.wait_for(server["started"].wait(), 10)
            for _ in range(500):  # the first part written
                if os.path.exists(d.x.download_path(1)) and os.path.getsize(d.x.download_path(1)):
                    break
                await asyncio.sleep(0.01)
            assert os.path.getsize(d.x.download_path(1)) > 0
            d.x.do(corepb.Effect(cancel=corepb.Id(id=1)))
            await _deleted(d.x.download_path(1))  # beside the loop
            await asyncio.sleep(0.2)
            assert d.got == []
            assert not os.path.exists(d.x.download_path(1))
        finally:
            server["release"].set()
            await d.x.close()
            await runner.cleanup()

    asyncio.run(run())
