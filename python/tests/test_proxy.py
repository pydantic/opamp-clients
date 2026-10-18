# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Proxies as the application configures them: HTTP (CONNECT for every connection, to an http://
or https:// proxy) and SOCKS5, with credentials, over both transports, for downloads too."""

import asyncio
import base64
import hashlib
import ssl
from urllib.parse import quote

import pytest
import trustme
from _servers import MODES, HttpProxy, OpampServer, Socks5Proxy, run_client
from hypothesis import given
from hypothesis import strategies as st

from pydantic_opamp_client import TLS, Client, Config, Proxy, Status
from pydantic_opamp_client._executor import Executor
from pydantic_opamp_client._proto.opamp.core.v1 import core_pb2 as corepb
from pydantic_opamp_client._transport import Transport, _route

TRANSPORTS = ["http", "ws"]


def server_context(ca: trustme.CA) -> ssl.SSLContext:
    ctx = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH)
    ca.issue_cert("127.0.0.1", "opamp.test", "localhost").configure_cert(ctx)
    return ctx


@pytest.fixture
def opamp_test(monkeypatch) -> None:
    """The name opamp.test resolves to 127.0.0.1, in this process (for the client, and for the test
    proxies)."""
    real = asyncio.base_events.BaseEventLoop.getaddrinfo

    async def getaddrinfo(self, host, *args, **kwargs):
        return await real(self, "127.0.0.1" if host == "opamp.test" else host, *args, **kwargs)

    monkeypatch.setattr(asyncio.base_events.BaseEventLoop, "getaddrinfo", getaddrinfo)


@pytest.fixture
def localhost_v4(monkeypatch) -> None:
    """The name localhost resolves to 127.0.0.1 alone, in this process (where the test servers
    listen), not ::1 first."""
    real = asyncio.base_events.BaseEventLoop.getaddrinfo

    async def getaddrinfo(self, host, *args, **kwargs):
        return await real(self, "127.0.0.1" if host == "localhost" else host, *args, **kwargs)

    monkeypatch.setattr(asyncio.base_events.BaseEventLoop, "getaddrinfo", getaddrinfo)


@pytest.fixture
def no_env_proxies(monkeypatch) -> None:
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"):
        monkeypatch.delenv(name, raising=False)
        monkeypatch.delenv(name.lower(), raising=False)


def through(proxy, server: OpampServer) -> None:
    """Every message reached the server on a connection the proxy opened."""
    assert server.arrivals
    assert {a.port for a in server.arrivals} <= proxy.upstream_ports


# covers: RULE-PROXY-01
@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
@pytest.mark.parametrize("server_tls", [False, True], ids=["plain", "tls"])
@pytest.mark.parametrize("proxy_tls", [False, True], ids=["http-proxy", "https-proxy"])
def test_http_proxy_tunnels_every_connection(transport: str, server_tls: bool, proxy_tls: bool, mode: str) -> None:
    async def run() -> None:
        ca = trustme.CA()
        server = await OpampServer(server_context(ca) if server_tls else None).start()
        proxy = await HttpProxy("agent", "p@ss:word", server_context(ca) if proxy_tls else None).start()
        scheme = "https" if proxy_tls else "http"
        try:
            await run_client(server, mode=mode, endpoint=server.endpoint(transport), tls=TLS(ca_pem=ca.cert_pem.bytes()),
                             proxy=Proxy(f"{scheme}://agent:{quote('p@ss:word', safe='')}@127.0.0.1:{proxy.port}",
                                         headers={"X-Tenant": "t1"}))
        finally:
            await proxy.close()
            await server.close()
        through(proxy, server)
        assert proxy.log
        for entry in proxy.log:
            assert entry["method"] == "CONNECT", "a tunnel, even for http:// and ws://"
            assert entry["target"] == f"127.0.0.1:{server.port}"
            assert entry["user"] == "agent" and entry["password"] == "p@ss:word"
            assert entry["headers"]["x-tenant"] == "t1"
        if server_tls:
            assert {a.tls for a in server.arrivals} <= {"TLSv1.2", "TLSv1.3"}

    asyncio.run(run())


# covers: RULE-PROXY-01
@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
def test_http_proxy_refusing_the_credentials_is_a_failed_connection(transport: str, mode: str) -> None:
    async def run() -> None:
        server = await OpampServer().start()
        proxy = await HttpProxy("agent", "secret").start()
        try:
            notes = await run_client(server, mode=mode, endpoint=server.endpoint(transport), proxy=f"http://agent:wrong@127.0.0.1:{proxy.port}")
        finally:
            await proxy.close()
            await server.close()
        assert not server.arrivals
        failed = [n for n in notes if n.kind == "connect_failed"]
        assert failed and "407" in failed[0].detail, failed
        assert "wrong" not in failed[0].detail, "no credentials in the error"

    asyncio.run(run())


# covers: RULE-PROXY-01
@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
@pytest.mark.parametrize("server_tls", [False, True], ids=["plain", "tls"])
@pytest.mark.parametrize("scheme", ["socks5", "socks5h"])
@pytest.mark.parametrize("resolve_locally", [False, True], ids=["proxy-resolves", "resolve-locally"])
def test_socks5_proxy_with_credentials(transport: str, server_tls: bool, scheme: str, resolve_locally: bool,
                                       localhost_v4, mode: str) -> None:
    """socks5:// and socks5h:// alike send the proxy the server's name (address type 3), for it to
    resolve; with resolve_locally the client resolves it and sends the address (type 1 or 4)."""
    async def run() -> None:
        ca = trustme.CA()
        server = await OpampServer(server_context(ca) if server_tls else None).start()
        proxy = await Socks5Proxy("agent", "secret").start()
        try:
            await run_client(server, mode=mode, endpoint=server.endpoint(transport, host="localhost"), tls=TLS(ca_pem=ca.cert_pem.bytes()),
                             proxy=Proxy(f"{scheme}://agent:secret@127.0.0.1:{proxy.port}",
                                         resolve_locally=resolve_locally))
        finally:
            await proxy.close()
            await server.close()
        through(proxy, server)
        assert proxy.log
        for entry in proxy.log:
            assert entry["user"] == "agent" and entry["cmd"] == 1 and entry["port"] == server.port
            assert (entry["address_type"], entry["host"]) == ((1, "127.0.0.1") if resolve_locally else (3, "localhost"))

    asyncio.run(run())


# covers: RULE-PROXY-01
@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
def test_socks5_proxy_refusing_the_credentials_is_a_failed_connection(transport: str, mode: str) -> None:
    async def run() -> None:
        server = await OpampServer().start()
        proxy = await Socks5Proxy("agent", "secret").start()
        try:
            notes = await run_client(server, mode=mode, endpoint=server.endpoint(transport), proxy=f"socks5://agent:wrong@127.0.0.1:{proxy.port}")
        finally:
            await proxy.close()
            await server.close()
        assert not server.arrivals
        failed = [n for n in notes if n.kind == "connect_failed"]
        assert failed and "refused" in failed[0].detail, failed

    asyncio.run(run())


# covers: RULE-PROXY-01
@pytest.mark.parametrize("kind", ["http", "socks5"])
def test_downloads_go_the_same_way(kind: str, tmp_path) -> None:
    """Package downloads use the same TLS settings and proxy."""
    async def run() -> None:
        ca = trustme.CA()
        server = await OpampServer(server_context(ca)).start()
        proxy = await (HttpProxy if kind == "http" else Socks5Proxy)("agent", "secret").start()
        got: list[corepb.Input] = []
        arrived = asyncio.Event()

        def deliver(i: corepb.Input) -> None:
            got.append(i)
            arrived.set()

        x = Executor(deliver, str(tmp_path / "state"),
                     Transport(TLS(ca_pem=ca.cert_pem.bytes()), f"{kind}://agent:secret@127.0.0.1:{proxy.port}"))
        try:
            x.do(corepb.Effect(download=corepb.Download(
                id=1, url=f"https://127.0.0.1:{server.port}/file", max_bytes=1 << 20, timeout_ms=5000)))
            await asyncio.wait_for(arrived.wait(), 10)
        finally:
            await x.close()
            await proxy.close()
            await server.close()
        assert got[-1].WhichOneof("kind") == "download_response", got[-1]
        assert got[-1].download_response.status == 200
        assert got[-1].download_response.sha256 == hashlib.sha256(OpampServer.BODY).digest()
        through(proxy, server)

    asyncio.run(run())


# covers: RULE-PROXY-01
@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
@pytest.mark.parametrize("server_tls", [False, True], ids=["plain", "tls"])
@pytest.mark.parametrize("form", ["upper", "lower", "bare"])
def test_the_environments_proxy_is_the_default(transport: str, server_tls: bool, form: str, monkeypatch, mode: str) -> None:
    """With no proxy configured: HTTP_PROXY for http:// and ws://, HTTPS_PROXY for https:// and
    wss://, as urllib reads them (lower-case names, and host:port without a scheme, too)."""
    async def run() -> None:
        ca = trustme.CA()
        server = await OpampServer(server_context(ca) if server_tls else None).start()
        creds = form != "bare"
        proxy = await (HttpProxy("agent", "secret") if creds else HttpProxy(None, None)).start()
        used, other = ("HTTPS_PROXY", "HTTP_PROXY") if server_tls else ("HTTP_PROXY", "HTTPS_PROXY")
        url = f"http://agent:secret@127.0.0.1:{proxy.port}" if creds else f"127.0.0.1:{proxy.port}"
        monkeypatch.setenv(used.lower() if form == "lower" else used, url)
        monkeypatch.setenv(other, "http://127.0.0.1:9")  # the wrong one for this endpoint: unused
        try:
            await run_client(server, mode=mode, endpoint=server.endpoint(transport), tls=TLS(ca_pem=ca.cert_pem.bytes()))
        finally:
            await proxy.close()
            await server.close()
        through(proxy, server)
        assert all(e["method"] == "CONNECT" and e["user"] == ("agent" if creds else None) for e in proxy.log)

    asyncio.run(run())


# covers: RULE-PROXY-01
@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
@pytest.mark.parametrize("scheme", ["socks5", "socks5h"])
def test_the_environment_may_name_a_socks5_proxy(transport: str, scheme: str, monkeypatch, no_env_proxies,
                                                 localhost_v4, mode: str) -> None:
    """A SOCKS5 proxy from the environment gets the server's name too, socks5:// or socks5h://."""
    async def run() -> None:
        server = await OpampServer().start()
        proxy = await Socks5Proxy("agent", "secret").start()
        monkeypatch.setenv("HTTP_PROXY", f"{scheme}://agent:secret@127.0.0.1:{proxy.port}")
        try:
            await run_client(server, mode=mode, endpoint=server.endpoint(transport, host="localhost"))
        finally:
            await proxy.close()
            await server.close()
        through(proxy, server)
        assert proxy.log
        assert {(e["address_type"], e["host"]) for e in proxy.log} == {(3, "localhost")}

    asyncio.run(run())


@pytest.mark.parametrize("scheme", ["socks4", "socks4a", "SOCKS4", "socks", "gopher"])
def test_the_environment_may_not_name_socks4_or_an_unknown_scheme(scheme: str, monkeypatch, no_env_proxies) -> None:
    async def run() -> None:
        with pytest.raises(ValueError, match="HTTP_PROXY"):
            Client.start(Config(mode="platform", endpoint="http://127.0.0.1:1/v1/opamp"), Status())

    monkeypatch.setenv("HTTP_PROXY", f"{scheme}://127.0.0.1:1080")
    asyncio.run(run())


@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
@pytest.mark.parametrize("no_proxy", ["NO_PROXY=127.0.0.1", "no_proxy=example.com,127.0.0.1", "NO_PROXY=*"])
def test_no_proxy_is_respected(transport: str, no_proxy: str, monkeypatch, mode: str) -> None:
    async def run() -> None:
        server = await OpampServer().start()
        proxy = await HttpProxy(None, None).start()
        monkeypatch.setenv("HTTP_PROXY", f"http://127.0.0.1:{proxy.port}")
        monkeypatch.setenv(*no_proxy.split("=", 1))
        try:
            await run_client(server, mode=mode, endpoint=server.endpoint(transport))
        finally:
            await proxy.close()
            await server.close()
        if mode == "core" and no_proxy == "NO_PROXY=*":
            # Core mode's shared rule (tests/shell-tables, no-proxy-star-address): * is a name
            # entry, and an address (the endpoint's 127.0.0.1) is matched by address entries only.
            through(proxy, server)
            return
        assert server.arrivals and not proxy.log, "straight to the server"

    asyncio.run(run())


@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
@pytest.mark.parametrize("direct", [Proxy.DIRECT, "direct://"], ids=["Proxy.DIRECT", "url"])
def test_direct_ignores_the_environment(transport: str, direct, monkeypatch, mode: str) -> None:
    async def run() -> None:
        server = await OpampServer().start()
        proxy = await HttpProxy(None, None).start()
        monkeypatch.setenv("HTTP_PROXY", f"http://127.0.0.1:{proxy.port}")
        try:
            await run_client(server, mode=mode, endpoint=server.endpoint(transport), proxy=direct)
        finally:
            await proxy.close()
            await server.close()
        assert server.arrivals and not proxy.log

    asyncio.run(run())


# covers: RULE-PROXY-01
@pytest.mark.parametrize("bypass", [False, True], ids=["proxied", "no_proxy"])
def test_downloads_use_the_environment_too(bypass: bool, tmp_path, monkeypatch, opamp_test) -> None:
    """Each download's URL is matched on its own: NO_PROXY names other hosts, and the
    download's too, or not."""
    async def run() -> None:
        ca = trustme.CA()
        server = await OpampServer(server_context(ca)).start()
        proxy = await HttpProxy(None, None).start()
        monkeypatch.setenv("HTTPS_PROXY", f"http://127.0.0.1:{proxy.port}")
        monkeypatch.setenv("NO_PROXY", "127.0.0.1" + (",opamp.test" if bypass else ""))
        got: list[corepb.Input] = []
        arrived = asyncio.Event()

        def deliver(i: corepb.Input) -> None:
            got.append(i)
            arrived.set()

        x = Executor(deliver, str(tmp_path / "state"), Transport(TLS(ca_pem=ca.cert_pem.bytes())))
        try:
            x.do(corepb.Effect(download=corepb.Download(
                id=1, url=f"https://opamp.test:{server.port}/file", max_bytes=1 << 20, timeout_ms=5000)))
            await asyncio.wait_for(arrived.wait(), 10)
        finally:
            await x.close()
            await proxy.close()
            await server.close()
        assert got[-1].download_response.status == 200, got[-1]
        if bypass:
            assert not proxy.log
        else:
            through(proxy, server)
            assert {e["target"] for e in proxy.log} == {f"opamp.test:{server.port}"}

    asyncio.run(run())


def test_a_bad_proxy_in_the_environment_fails_at_start(monkeypatch) -> None:
    async def run() -> None:
        with pytest.raises(ValueError, match="HTTPS_PROXY"):
            Client.start(Config(mode="platform", endpoint="https://127.0.0.1:1/v1/opamp"), Status())

    monkeypatch.setenv("HTTPS_PROXY", "ftp://127.0.0.1:21")
    asyncio.run(run())


@pytest.mark.parametrize("proxy", [
    "ftp://127.0.0.1:21",
    "http://",
    "http://127.0.0.1:99999",
    "http://127.0.0.1:3128/path",
    Proxy("http://127.0.0.1:3128", headers={"X-Bad": "a\r\nInjected: yes"}),
    Proxy("http://127.0.0.1:3128", headers={"Bad Name": "x"}),
    Proxy("http://u:p@127.0.0.1:3128", headers={"Proxy-Authorization": "Basic eA=="}),
    Proxy("socks5://127.0.0.1:1080", headers={"X-Tenant": "t1"}),
    "socks5://" + "u" * 256 + ":p@127.0.0.1:1080",
    "socks4://127.0.0.1:1080",
    "socks4a://127.0.0.1:1080",
    "SOCKS4A://127.0.0.1:1080",
    "socks://127.0.0.1:1080",
    "unknown://127.0.0.1:1080",
    "127.0.0.1:3128",
])
@pytest.mark.parametrize("mode", MODES)
def test_proxies_that_cannot_work_fail_at_start(proxy, mode: str) -> None:
    async def run() -> None:
        with pytest.raises(ValueError):
            Client.start(Config(mode=mode, endpoint="http://127.0.0.1:1/v1/opamp", proxy=proxy), Status())

    asyncio.run(run())


@pytest.mark.parametrize("scheme", ["socks4", "socks4a"])
def test_socks4_is_refused_by_name(scheme: str) -> None:
    with pytest.raises(ValueError, match="SOCKS4 is not supported"):
        _route(Proxy(f"{scheme}://127.0.0.1:1080"))


@pytest.mark.parametrize("url", ["http://127.0.0.1:3128", "https://127.0.0.1:3128", "HTTP://127.0.0.1:3128",
                                 "direct://", "socks4://127.0.0.1:1080", "ftp://127.0.0.1:21"])
def test_resolve_locally_is_for_socks5_proxies_only(url: str) -> None:
    with pytest.raises(ValueError, match="resolve_locally"):
        Proxy(url, resolve_locally=True)


@pytest.mark.parametrize("scheme", ["socks5", "socks5h", "SOCKS5H"])
def test_resolve_locally_is_off_unless_asked(scheme: str) -> None:
    assert _route(Proxy(f"{scheme}://127.0.0.1:1080")).resolve_locally is False
    assert _route(Proxy(f"{scheme}://127.0.0.1:1080", resolve_locally=True)).resolve_locally is True


credentials = st.text(st.characters(codec="utf-8", exclude_characters="\0"), min_size=1, max_size=40)


# covers: RULE-PROXY-01
@given(user=credentials.filter(lambda u: ":" not in u), password=credentials)
def test_credentials_reach_the_proxy_decoded(user: str, password: str) -> None:
    """Whatever the credentials, percent-encoded in the URL: an HTTP proxy gets them, and only
    them, as Basic credentials; a SOCKS5 proxy gets them as UTF-8 (when they fit)."""
    userinfo = f"{quote(user, safe='')}:{quote(password, safe='')}"
    head = _route(Proxy(f"http://{userinfo}@proxy.example:3128")).connect_headers.decode("latin-1")
    assert head.count("\r\n") == 1 and head.startswith("Proxy-Authorization: Basic ")
    assert base64.b64decode(head.split()[2]).decode() == f"{user}:{password}"
    if len(user.encode()) <= 255 and len(password.encode()) <= 255:
        route = _route(Proxy(f"socks5://{userinfo}@proxy.example:1080"))
        assert route.socks_auth == (user.encode(), password.encode())


@given(value=st.text(max_size=20), bad=st.sampled_from("\r\n\0"), at=st.integers(0, 20))
def test_proxy_header_values_cannot_break_lines(value: str, bad: str, at: int) -> None:
    value = value[:at] + bad + value[at:]
    with pytest.raises(ValueError):
        _route(Proxy("http://proxy.example:3128", headers={"X-Header": value}))


def test_reprs_leave_out_secrets() -> None:
    assert "secret" not in repr(Proxy("http://agent:secret@proxy.example:3128"))
    assert "agent" in repr(Proxy("http://agent:secret@proxy.example:3128"))
    assert "secret" not in repr(TLS(key_pem="secret", key_password="secret"))
