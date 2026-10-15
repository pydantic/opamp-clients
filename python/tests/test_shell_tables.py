# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The shells' shared tables (tests/shell-tables at the repository root): the URLs the core
accepts, read and sent alike by this client's HTTP stack; the proxy the environment names for a
destination; and the TLS settings an application can give. Where this client differs from the
table (the Rust client's choices), the case is listed below with the reason, and the test checks
that it still differs that way."""

from __future__ import annotations

import asyncio
import ipaddress
import json
import ssl
import subprocess
import sys
from pathlib import Path
from urllib.parse import urlsplit

import pytest
import yarl

from opamp_client._executor import Executor
from opamp_client._proto.opamp.core.v1 import core_pb2 as corepb
from opamp_client._core import Core
from opamp_client._streams import proxy_choice
from opamp_client._transport import TLS, Proxy, Transport

TABLES = next(p / "tests" / "shell-tables" for p in Path(__file__).resolve().parents
              if (p / "tests" / "shell-tables").is_dir())


def table(name: str) -> dict:
    return json.loads((TABLES / name).read_text())


# URLs.

URLS = table("urls.json")["urls"]


def same_host(got: str | None, want: str) -> bool:
    got, want = (got or "").strip("[]").lower(), want.strip("[]").lower()
    try:
        return ipaddress.ip_address(got) == ipaddress.ip_address(want)
    except ValueError:
        return got == want


def test_python_mode_reads_every_url_alike() -> None:
    """Python mode hands each URL to aiohttp as a yarl.URL taken as it is written (encoded=True,
    the executor's _url): requests, WebSocket upgrades and downloads alike."""
    wrong = []
    for u in URLS:
        url = yarl.URL(u["url"], encoded=True)
        got = (url.raw_host, url.port, url.raw_path or "/", url.raw_query_string if "?" in u["url"] else None)
        if not (same_host(got[0], u["host"]) and got[1:] == (u["port"], u["path"], u["query"])):
            wrong.append((u["url"], got))
    assert not wrong


def test_rust_mode_picks_the_proxy_for_every_url_by_its_host_and_port() -> None:
    """Rust mode's core reads the URL itself; the shell reads only its scheme, host and port, to
    pick the proxy (Transport.proxy_for, by urllib.parse.urlsplit)."""
    wrong = []
    for u in URLS:
        s = urlsplit(u["url"])
        port = s.port or (443 if s.scheme.lower() in ("https", "wss") else 80)
        if not (same_host(s.hostname, u["host"]) and port == u["port"]):
            wrong.append((u["url"], s.hostname, port))
    assert not wrong


def sent_as(target: str) -> str:
    """The request target aiohttp sends for `target`. Known difference: a query that is there but
    empty (``/p?``) goes without its ``?`` (``/p``): yarl holds an empty query as none
    (URL.raw_path_qs), and aiohttp writes that; the two are the same resource to every server we
    know of."""
    return target[:-1] if target.endswith("?") and "?" not in target[:-1] else target


async def _raw_server(seen: list[str], status: bytes) -> tuple[asyncio.Server, int]:
    """A server that records each request line's target as it came, unparsed."""
    async def handle(r: asyncio.StreamReader, w: asyncio.StreamWriter) -> None:
        line = await r.readline()
        parts = line.decode("latin-1").split(" ")
        if len(parts) == 3:
            seen.append(parts[1])
        while (await r.readline()) not in (b"\r\n", b""):
            pass
        w.write(b"HTTP/1.1 " + status + b"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
        await w.drain()
        w.close()

    server = await asyncio.start_server(handle, "127.0.0.1", 0)
    return server, server.sockets[0].getsockname()[1]


@pytest.mark.parametrize("how", ["http_request", "download", "ws_open"])
def test_python_mode_sends_every_urls_request_target_exactly(how: str, tmp_path) -> None:
    async def run() -> None:
        seen: list[str] = []
        server, port = await _raw_server(seen, b"400 Bad Request" if how == "ws_open" else b"200 OK")
        got: list[corepb.Input] = []
        x = Executor(got.append, str(tmp_path / "state"), Transport(proxy=Proxy.DIRECT))
        wrong = []
        try:
            for n, u in enumerate(URLS, 1):
                if how == "ws_open" and not u["scheme"].lower().startswith("ws"):
                    continue
                seen.clear()
                got.clear()
                url = ("ws" if how == "ws_open" else "http") + f"://127.0.0.1:{port}" + u["target"]
                if how == "http_request":
                    x.do(corepb.Effect(http_request=corepb.HttpRequest(
                        id=n, url=url, body=b"x", timeout_ms=5000, max_response_bytes=1024)))
                elif how == "download":
                    x.do(corepb.Effect(download=corepb.Download(id=n, url=url, max_bytes=1024, timeout_ms=5000)))
                else:
                    x.do(corepb.Effect(ws_open=corepb.WsOpen(id=n, url=url, max_message_bytes=1024)))
                for _ in range(500):
                    if got:
                        break
                    await asyncio.sleep(0.01)
                kind = got[0].WhichOneof("kind") if got else None
                if kind not in ("http_response", "download_response", "ws_failed") or seen != [sent_as(u["target"])]:
                    wrong.append((u["url"], u["target"], list(seen), kind))
                if how == "download":
                    x.do(corepb.Effect(discard=corepb.Id(id=n)))
        finally:
            await x.close(5)
            server.close()
        assert not wrong

    asyncio.run(run())


# Proxies.

PROXY = table("proxy.json")["cases"]

#: Where this client's choice differs from the table's, and why: what it picks instead.
PROXY_DIFFERENCES = {
    # Python's convention (urllib.request.getproxies_environment, which requests follows too):
    # the lower-case name wins when both are set.
    "http-proxy-upper-first": "http://lower-proxy:1",
    # Python's convention (urllib.request.proxy_bypass_environment): a NO_PROXY entry with a port
    # matches that host on that port, as curl does too.
    "no-proxy-port": None,
    # urllib reads the lower-case name first, and an empty one as unset.
    "upper-empty-hides-lower": "http://lower-proxy:1",
    # urllib takes the value as it is: spaces in it make no URL, which the client refuses.
    "value-trimmed": "refused (HTTP_PROXY: Port could not be cast to integer value as '3128  ')",
    # urllib's httpoxy rule drops HTTP_PROXY alone under CGI.
    "cgi-all-proxy": "http://all-proxy:3130",
    # urllib's NO_PROXY: * is every host, addresses too.
    "no-proxy-star-address": None,
    # This client's NO_PROXY strips an entry's brackets, as curl takes [::1].
    "no-proxy-bracketed-entry": None,
}


@pytest.mark.parametrize("case", PROXY, ids=[c["id"] for c in PROXY])
def test_the_proxy_the_environment_names_python_mode(case: dict) -> None:
    """Python mode: the connector's Transport.route, by urllib's convention, with the differences
    above; the environment is given to it (no other variable: the process's own are left
    alone)."""
    try:
        route = Transport(_environ=case["env"]).proxy_for(case["url"])
        got = None if route is None else (
            f"{route.scheme}://{'[' + route.host + ']' if ':' in route.host else route.host}:{route.port}")
    except ValueError as e:
        got = f"refused ({e})"
    want = PROXY_DIFFERENCES.get(case["id"], case["proxy"])
    assert got == want, case["why"]


def _core_route(choice: corepb.ProxyChoice, url: str) -> str | None:
    """Where Rust mode's core connects first for `url` when told `choice`: the proxy as the table
    writes a route (scheme://host:port, credentials left out), or None for directly. The core
    picks it itself: it asks no proxy_query."""
    core = Core(corepb.Config(instance_uid=bytes(16), endpoint=url, streams=True, entropy=bytes(32), proxy=choice))
    try:
        out = core.step(1, corepb.Input(start=corepb.Empty()))
        kinds = [e.WhichOneof("kind") for e in out.effects]
        assert "proxy_query" not in kinds, f"{url}: the core asked which proxy"
        opens = [e.stream_open for e in out.effects if e.WhichOneof("kind") == "stream_open"]
        assert opens, f"{url}: no stream opened: {out}"
        open_ = opens[0]
        u = urlsplit(url)
        port = u.port or (443 if u.scheme.lower() in ("https", "wss") else 80)
        if open_.host.lower() == (u.hostname or "").lower() and open_.port == port:
            return None
        # Which kind of proxy: SOCKS5 greets with version 5, an https:// one is TLS first.
        nxt = core.step(2, corepb.Input(stream_opened=corepb.Id(id=open_.id)))
        scheme = "http"
        for e in nxt.effects:
            kind = e.WhichOneof("kind")
            if kind == "stream_write" and e.stream_write.data[:1] == b"\x05":
                scheme = "socks5"
            if kind == "stream_start_tls":
                scheme = "https"
        host = f"[{open_.host}]" if ":" in open_.host else open_.host
        return f"{scheme}://{host}:{open_.port}"
    finally:
        core.close()


# covers: RULE-PROXY-02
@pytest.mark.parametrize("case", PROXY, ids=[c["id"] for c in PROXY])
def test_the_proxy_the_environment_names_rust_mode(case: dict) -> None:
    """Rust mode: the client hands the core the environment's variables (Config.proxy) and the
    core picks by the shared rule, exactly as the table says (tests/shell-tables/README.md): no
    differences. Through this client's reading of the environment and the core it ships."""
    choice = proxy_choice(Transport(rust=True, _environ=case["env"]))
    assert _core_route(choice, case["url"]) == case["proxy"], case["why"]


# covers: RULE-PROXY-02
def test_rust_mode_tells_the_core_its_proxy() -> None:
    """What Rust mode tells the core: the configured proxy and no variables; none with
    Proxy.DIRECT; otherwise each variable the rule reads that is set, even empty, by its own name.
    A proxy_query (only a core that wasn't told asks) hears the configured proxy or none."""
    env = {"HTTP_PROXY": "http://env:1", "no_proxy": "", "REQUEST_METHOD": "GET", "OTHER": "x"}
    configured = Transport(proxy=Proxy("http://u:p%40ss@configured:8080", headers={"X-A": "1"}), rust=True, _environ=env)
    assert proxy_choice(configured) == corepb.ProxyChoice(url="http://u:p%40ss@configured:8080",
                                                          headers=[corepb.Header(name="X-A", value="1")])
    answer = configured.proxy_for("http://x/")
    assert answer is not None and answer.url == "http://u:p%40ss@configured:8080"
    direct = Transport(proxy=Proxy.DIRECT, rust=True, _environ=env)
    assert proxy_choice(direct) == corepb.ProxyChoice(direct=True)
    assert direct.proxy_for("http://x/") is None
    from_env = Transport(rust=True, _environ=env)
    assert proxy_choice(from_env) == corepb.ProxyChoice(environment=[
        corepb.Header(name="REQUEST_METHOD", value="GET"), corepb.Header(name="HTTP_PROXY", value="http://env:1"),
        corepb.Header(name="no_proxy", value="")])
    assert from_env.proxy_for("http://x/") is None
    # The core reads them by the rule: CGI, so none.
    assert _core_route(proxy_choice(from_env), "http://example.com/") is None
    # A value the core can't use is its to refuse (the connection fails): no error here.
    assert proxy_choice(Transport(rust=True, _environ={"HTTP_PROXY": "ftp://x"})).environment[0].value == "ftp://x"


# TLS.

TLS_CASES = table("tls.json")["cases"]
VERSIONS = {"1.0": ssl.TLSVersion.TLSv1, "1.1": ssl.TLSVersion.TLSv1_1, "1.2": ssl.TLSVersion.TLSv1_2,
            "1.3": ssl.TLSVersion.TLSv1_3}


def tls(case: dict, form: str) -> TLS:
    """The TLS settings an application would give for `case`: files, or the PEM text read from
    them."""
    kw: dict = {}
    for name, field in (("ca", "ca"), ("cert", "cert"), ("key", "key")):
        if name in case:
            path = TABLES / case[name]
            kw[f"{field}_file" if form == "file" else f"{field}_pem"] = (
                str(path) if form == "file" else path.read_bytes())
    if "min" in case:
        kw["min_version"] = VERSIONS[case["min"]]
    if "max" in case:
        kw["max_version"] = VERSIONS[case["max"]]
    return TLS(**kw)


@pytest.mark.parametrize("form", ["file", "pem"])
@pytest.mark.parametrize("case", TLS_CASES, ids=[c["id"] for c in TLS_CASES])
def test_the_tls_settings_taken(case: dict, form: str) -> None:
    """Both modes build their TLS from the Transport's context: made, or refused at once."""
    try:
        Transport(tls(case, form), Proxy.DIRECT)
        got = "ok"
    except (ValueError, OSError) as e:  # ssl.SSLError is an OSError
        got = f"refused ({type(e).__name__}: {e})"
    assert got.split(" ")[0] == case["expect"], f"{case['why']}: {got}"


def test_an_encrypted_key_without_its_password_is_refused_not_asked_for() -> None:
    """With no key_password, OpenSSL would ask for the pass phrase on the terminal, or read it from
    standard input: the client must refuse the key instead, whatever is there to read."""
    case = next(c for c in TLS_CASES if c["id"] == "client-key-encrypted")
    code = (f"from opamp_client._transport import TLS\n"
            f"TLS(cert_file={str(TABLES / case['cert'])!r}, key_file={str(TABLES / case['key'])!r}).context()\n")
    # tls/gen.sh's pass phrase, there to be read.
    r = subprocess.run([sys.executable, "-c", code], input="secret\n", capture_output=True, text=True, timeout=60)
    assert r.returncode != 0, "the key was taken with a pass phrase read from standard input"
    assert "pass phrase" not in (r.stdout + r.stderr).lower(), r.stderr
