# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""How the client reaches the server: TLS and a proxy, as the application configures them. The
core knows nothing of either. Every request, WebSocket and package download goes through a
connector made here from one `Transport` (an SSL context and a proxy route)."""

from __future__ import annotations

import asyncio
import base64
import ipaddress
import os
import re
import socket
import ssl
import tempfile
from collections.abc import Iterator, Mapping
from contextlib import contextmanager
from dataclasses import dataclass, field
from typing import Any, ClassVar
from urllib.parse import unquote, urlsplit

import aiohttp
from aiohttp.abc import AbstractResolver, ResolveResult


@dataclass(frozen=True)
class TLS:
    """TLS for https:// and wss:// endpoints, https:// proxies and package downloads. The
    defaults: the system's CAs, TLS 1.2 or later, no client certificate."""

    #: CAs to trust for the server's certificate, instead of the system's: a PEM file.
    ca_file: str | None = None
    #: CAs to trust, as PEM text. With ca_file too, both are trusted.
    ca_pem: str | bytes | None = None
    #: Trust the system's CAs as well as ca_file and ca_pem. Without either they are always trusted.
    include_system_cas: bool = False
    #: The client certificate presented in every handshake (mutual TLS): a PEM file of the
    #: certificate (and any intermediates), and one of its private key (they may be the same file).
    cert_file: str | None = None
    key_file: str | None = None
    #: Or the certificate and key as PEM text. Python's ssl module loads them from files only: the
    #: client writes each to a new temporary file only the agent's user can read (mode 0600),
    #: loads it and deletes it at once.
    cert_pem: str | bytes | None = None
    key_pem: str | bytes | None = field(default=None, repr=False)
    #: The password of an encrypted private key.
    key_password: str | bytes | None = field(default=None, repr=False)
    #: The lowest TLS version; below TLS 1.2 is refused.
    min_version: ssl.TLSVersion = ssl.TLSVersion.TLSv1_2
    #: The highest TLS version.
    max_version: ssl.TLSVersion = ssl.TLSVersion.MAXIMUM_SUPPORTED

    def context(self) -> ssl.SSLContext:
        """The SSL context these settings make. Raises ValueError for settings that don't fit
        together, and OSError or ssl.SSLError for files or PEM that can't be loaded."""
        low, high = self.min_version, self.max_version
        if _below_1_2(low):
            raise ValueError(f"TLS below 1.2 is not allowed (min_version {low.name})")
        if _below_1_2(high):
            raise ValueError(f"TLS below 1.2 is not allowed (max_version {high.name}): no version is left")
        if high != ssl.TLSVersion.MAXIMUM_SUPPORTED and low not in (ssl.TLSVersion.MINIMUM_SUPPORTED,
                                                                     ssl.TLSVersion.MAXIMUM_SUPPORTED) and low > high:
            raise ValueError(f"min_version {low.name} is above max_version {high.name}: no version is left")
        if self.cert_file is not None and self.cert_pem is not None:
            raise ValueError("give cert_file or cert_pem, not both")
        if self.key_file is not None and self.key_pem is not None:
            raise ValueError("give key_file or key_pem, not both")
        has_cert = self.cert_file is not None or self.cert_pem is not None
        has_key = self.key_file is not None or self.key_pem is not None
        if has_cert != has_key:
            raise ValueError("a client certificate needs its private key, and a key its certificate")
        ca_pem = self.ca_pem.decode("ascii") if isinstance(self.ca_pem, bytes) else self.ca_pem
        if ca_pem is not None and not ca_pem.strip():
            # (create_default_context would take an empty one as none, and trust the system's CAs.)
            raise ValueError("ca_pem holds no certificate")
        # create_default_context loads the system's CAs only when given none of its own.
        ctx = ssl.create_default_context(ssl.Purpose.SERVER_AUTH, cafile=self.ca_file, cadata=ca_pem)
        if self.include_system_cas and (self.ca_file is not None or ca_pem is not None):
            ctx.load_default_certs(ssl.Purpose.SERVER_AUTH)
        ctx.minimum_version = low
        ctx.maximum_version = high
        if has_cert:
            with _pem_file(self.cert_pem) as cert, _pem_file(self.key_pem) as key:
                # Without key_password, an encrypted key is refused: OpenSSL would ask for its pass
                # phrase on the terminal (or read standard input) otherwise.
                ctx.load_cert_chain(cert or self.cert_file or "", key or self.key_file,
                                    password=self.key_password if self.key_password is not None else _no_password)
        return ctx


def _below_1_2(v: ssl.TLSVersion) -> bool:
    """Whether `v` (MINIMUM_SUPPORTED included) is below TLS 1.2."""
    if v == ssl.TLSVersion.MINIMUM_SUPPORTED:
        return True
    return v != ssl.TLSVersion.MAXIMUM_SUPPORTED and v < ssl.TLSVersion.TLSv1_2


def _no_password() -> str:
    raise ValueError("the private key is encrypted: give key_password")


@contextmanager
def _pem_file(pem: str | bytes | None) -> Iterator[str | None]:
    """A new temporary file holding `pem`, readable by this user only, deleted on leaving (None
    for no PEM). mkstemp creates it with mode 0600, never over an existing file."""
    if pem is None:
        yield None
        return
    fd, path = tempfile.mkstemp(prefix="opamp-", suffix=".pem")
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(pem.encode("ascii") if isinstance(pem, str) else pem)
        yield path
    finally:
        os.remove(path)


@dataclass(frozen=True, repr=False)
class Proxy:
    """A proxy to reach the server through, and to download packages through.

    ``url`` is ``http://host:port`` or ``https://host:port`` (an HTTP proxy; https: TLS to the
    proxy too), or ``socks5://host:port`` or ``socks5h://host:port`` (a SOCKS5 proxy; both leave
    the server's name to the proxy to resolve, unless ``resolve_locally``). Unlike curl and requests, where ``socks5://``
    resolves the name locally, ``socks5://`` here sends the name, as browsers, opamp-go and the
    OpenTelemetry Collector do. SOCKS4 and SOCKS4a, and any other scheme, are refused.
    Credentials go in it as ``user:password@``, percent-encoded. Through an HTTP proxy every
    connection is a CONNECT tunnel, plain http:// and ws:// endpoints too.

    ``Proxy.DIRECT`` (its URL is ``direct://``) is no proxy at all: every connection goes straight
    to the server, whatever the environment says."""

    #: No proxy, not even the environment's.
    DIRECT: ClassVar[Proxy]

    url: str
    #: Headers sent with every CONNECT (HTTP proxies only).
    headers: Mapping[str, str] = field(default_factory=dict)
    #: Resolve the server's name on the client and send the proxy its address instead (SOCKS5
    #: proxies only, socks5:// and socks5h:// alike): for proxies that can't resolve names.
    resolve_locally: bool = False

    def __post_init__(self) -> None:
        if self.resolve_locally and urlsplit(self.url).scheme.lower() not in ("socks5", "socks5h"):
            raise ValueError("resolve_locally is for SOCKS5 proxies (socks5:// or socks5h://) only")

    def __repr__(self) -> str:
        # Without the password, or the headers' values (Proxy-Authorization, say).
        extra = ", resolve_locally=True" if self.resolve_locally else ""
        return f"Proxy(url={redacted_url(self.url)!r}, headers={redacted_headers(self.headers)!r}{extra})"


Proxy.DIRECT = Proxy("direct://")


def redacted_url(url: str) -> str:
    """`url` without the password in it, for a repr."""
    return re.sub(r"(//[^/@:]*):[^/@]*@", r"\1:***@", url)


def redacted_headers(headers: Mapping[str, str]) -> dict[str, str]:
    """`headers` with every value left out, for a repr: any may hold a credential."""
    return dict.fromkeys(headers, "<redacted>")


_TOKEN = re.compile(r"^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$")
_DEFAULT_PORTS = {"http": 80, "https": 443, "socks5": 1080, "socks5h": 1080}


@dataclass(frozen=True)
class _Route:
    """A proxy, parsed: where it is and how to ask it for a tunnel."""

    scheme: str  # http, https, socks5 or socks5h
    host: str
    port: int
    # As given (credentials included), and the headers for a CONNECT as given: core mode hands
    # both to the core, which speaks to the proxy itself.
    url: str = field(default="", repr=False, compare=False)
    headers: Mapping[str, str] = field(default_factory=dict, repr=False, compare=False)
    # HTTP proxies: the CONNECT request's header lines (Proxy-Authorization among them).
    connect_headers: bytes = b""
    # SOCKS5 proxies: the username and password (RFC 1929), if any.
    socks_auth: tuple[bytes, bytes] | None = None
    # SOCKS5 proxies: the client resolves the server's name and sends its address. Otherwise
    # (the default, socks5:// and socks5h:// alike) the proxy gets the name.
    resolve_locally: bool = False

    def __str__(self) -> str:
        host = f"[{self.host}]" if ":" in self.host else self.host
        return f"proxy {self.scheme}://{host}:{self.port}"


def _route(proxy: Proxy, what: str = "proxy URL") -> _Route | None:
    """Parse and check `proxy` (None: direct). Errors are the application's (or its
    environment's, named by `what`), so they come from start."""
    try:
        u = urlsplit(proxy.url)
        port = u.port
    except ValueError as e:
        raise ValueError(f"{what}: {e}") from None
    scheme = u.scheme.lower()
    if scheme == "direct":
        return None
    if scheme in ("socks4", "socks4a"):
        raise ValueError(f"{what}: SOCKS4 is not supported, only SOCKS5 (socks5:// or socks5h://)")
    if scheme not in _DEFAULT_PORTS:
        raise ValueError(f"{what}: the scheme must be http, https, socks5 or socks5h, not {u.scheme!r}")
    if not u.hostname:
        raise ValueError(f"{what}: no host")
    if u.path not in ("", "/") or u.query or u.fragment:
        raise ValueError(f"{what}: only scheme, credentials, host and port")
    user = unquote(u.username) if u.username is not None else None
    password = unquote(u.password) if u.password is not None else ""
    port = port or _DEFAULT_PORTS[scheme]
    if scheme.startswith("socks"):
        if proxy.headers:
            raise ValueError("proxy headers go with CONNECT: an HTTP proxy's only")
        auth = None
        if user is not None:
            u8, p8 = user.encode(), password.encode()
            if not 1 <= len(u8) <= 255 or not 1 <= len(p8) <= 255:
                raise ValueError("SOCKS5 username and password must be 1 to 255 bytes each")
            auth = (u8, p8)
        return _Route(scheme, u.hostname, port, proxy.url, dict(proxy.headers), socks_auth=auth,
                      resolve_locally=proxy.resolve_locally)
    if proxy.resolve_locally:  # a Proxy checks this itself; a subclass or replace() might not
        raise ValueError("resolve_locally is for SOCKS5 proxies (socks5:// or socks5h://) only")
    lines = []
    for name, value in proxy.headers.items():
        if not _TOKEN.match(name):
            raise ValueError(f"proxy header name {name!r}")
        if any(c in value for c in "\r\n\0"):
            raise ValueError(f"proxy header {name}: line breaks in its value")
        if name.lower() == "proxy-authorization" and user is not None:
            raise ValueError("proxy credentials in both the URL and a Proxy-Authorization header")
        if name.lower() == "host":
            raise ValueError("proxy header Host: the client sets it")
        lines.append(f"{name}: {value}\r\n")
    if user is not None:
        token = base64.b64encode(f"{user}:{password}".encode()).decode("ascii")
        lines.append(f"Proxy-Authorization: Basic {token}\r\n")
    try:
        head = "".join(lines).encode("latin-1")
    except UnicodeEncodeError:
        raise ValueError("proxy headers must be Latin-1") from None
    return _Route(scheme, u.hostname, port, proxy.url, dict(proxy.headers), connect_headers=head)


def _proxies_from(environ: Mapping[str, str]) -> dict[str, str]:
    """The proxies `environ` names, by scheme (and "no": NO_PROXY), as
    urllib.request.getproxies_environment reads os.environ: any *_proxy variable, the lower-case
    name winning, set but empty as unset; HTTP_PROXY ignored in a CGI script (REQUEST_METHOD set:
    httpoxy, CVE-2016-1000110)."""
    proxies: dict[str, str] = {}
    for name, value in environ.items():
        name = name.lower()
        if value and name[-6:] == "_proxy":
            proxies[name[:-6]] = value
    if "REQUEST_METHOD" in environ:
        proxies.pop("http", None)
    for name, value in environ.items():
        if name[-6:] == "_proxy":
            name = name.lower()
            if value:
                proxies[name[:-6]] = value
            else:
                proxies.pop(name[:-6], None)
    return proxies


def _bypass(no_proxy: str, host: str, port: int) -> bool:
    """Whether NO_PROXY's entries (comma-separated) say to reach host:port directly. As
    urllib.request.proxy_bypass_environment: ``*`` is every host; a name is that host and the
    names under it (a leading dot or none, ignoring case); ``name:port`` is that host on that port
    only. And beyond it: an IP address or a network in CIDR notation (``10.0.0.0/8``) matches the
    addresses in it, IPv6 ones too (with or without brackets), as curl and Go take them."""
    host = host.strip("[]").lower()
    try:
        ip: ipaddress.IPv4Address | ipaddress.IPv6Address | None = ipaddress.ip_address(host)
    except ValueError:
        ip = None
    for entry in no_proxy.split(","):
        entry = entry.strip().lower()
        if not entry:
            continue
        if entry == "*":
            return True
        if ip is not None:
            try:
                if ip in ipaddress.ip_network(entry.strip("[]"), strict=False):
                    return True
                continue
            except ValueError:
                pass  # an address with a port, or a name
        name, want = entry, None
        if (m := re.fullmatch(r"(.+):(\d+)", entry)) is not None and not (":" in m[1] and not m[1].endswith("]")):
            name, want = m[1], int(m[2])
        name = name.strip("[]").lstrip(".")
        if (host == name or host.endswith("." + name)) and want in (None, port):
            return True
    return False


#: The variables core mode's proxy rule reads (tests/shell-tables/README.md, "core mode's proxy
#: rule"). The core applies the rule; the client only reads them, once.
CORE_PROXY_VARIABLES = ("REQUEST_METHOD", "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
                        "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy")


class Transport:
    """The SSL context and the proxies every connection and download uses; the executor makes its
    sessions' connectors from it.

    With no proxy configured, the environment's apply, read once, here, as urllib reads them:
    HTTP_PROXY for http:// and ws://, HTTPS_PROXY for https:// and wss://, ALL_PROXY for either
    when its own is unset (as curl and requests take it), NO_PROXY for hosts to reach directly
    (lower-case names too, and taking precedence; see _bypass).

    In core mode (``core_mode``) the core is told once (Streams' proxy_choice): the configured proxy,
    none with ``Proxy.DIRECT``, or else the variables of the environment, read once, here
    (core_environment), from which the core picks by the rule every SDK's core mode shares, not
    urllib's convention."""

    def __init__(self, tls: TLS | ssl.SSLContext | None = None, proxy: Proxy | str | None = None, *,
                 core_mode: bool = False, _environ: Mapping[str, str] | None = None) -> None:
        # _environ: tests only, the environment to read in place of the process's.
        self.ssl = tls if isinstance(tls, ssl.SSLContext) else (tls or TLS()).context()
        # The configured proxy (None: direct), or the environment's by scheme.
        self._fixed: _Route | None = None
        self._env: dict[str, str] | None = None
        self._by_scheme: dict[str, _Route] = {}
        #: Core mode with no proxy configured: the variables the shared rule reads that are set.
        self.core_environment: dict[str, str] = {}
        #: A proxy (or Proxy.DIRECT) was configured.
        self.configured = proxy is not None
        if proxy is not None:
            self._fixed = _route(Proxy(proxy) if isinstance(proxy, str) else proxy)
            return
        if core_mode:
            environ = os.environ if _environ is None else _environ
            self.core_environment = {k: environ[k] for k in CORE_PROXY_VARIABLES if k in environ}
            return
        self._env = _proxies_from(os.environ if _environ is None else _environ)
        for scheme in ("http", "https", "all"):
            url = self._env.get(scheme)
            if url:
                # A bare host:port is an HTTP proxy, as urllib takes it.
                route = _route(Proxy(url if "://" in url else "http://" + url), f"{scheme.upper()}_PROXY")
                if route is not None:
                    self._by_scheme[scheme] = route

    def route(self, scheme: str, host: str, port: int) -> _Route | None:
        """The proxy for a connection to host:port for a URL of `scheme`; None: direct."""
        if self._env is None:
            return self._fixed
        route = self._by_scheme.get("https" if scheme in ("https", "wss") else "http") or self._by_scheme.get("all")
        if route is None or _bypass(self._env.get("no", ""), host, port):
            return None
        return route

    @property
    def fixed(self) -> _Route | None:
        """The configured proxy, if any (None with Proxy.DIRECT, or none configured)."""
        return self._fixed

    def proxy_for(self, url: str) -> _Route | None:
        """The proxy for `url` (http(s):// or ws(s)://), if any. In core mode, the answer to a
        proxy question, which the core asks only when it wasn't told (Streams' proxy_choice): the
        configured proxy, or none (the environment's rule is the core's). Its ``url`` and
        ``headers`` go to the core as given, with ``resolve_locally``."""
        u = urlsplit(url)
        scheme = u.scheme.lower()
        port = u.port or (443 if scheme in ("https", "wss") else 80)
        return self.route(scheme, u.hostname or "", port)

    def connector(self) -> aiohttp.TCPConnector:
        """A new connector (for a new session) making connections this way."""
        if self._fixed is None and not self._by_scheme:
            return aiohttp.TCPConnector(ssl=self.ssl)
        return _ProxyConnector(self, self.ssl)


class _ProxyError(Exception):
    """The proxy refused, or said something it shouldn't."""


class _NoResolver(AbstractResolver):
    """Leaves the server's name to the proxy connector: it may resolve on the proxy's side only."""

    async def resolve(self, host: str, port: int = 0, family: socket.AddressFamily = socket.AF_INET) -> list[ResolveResult]:
        return [{"hostname": host, "host": host, "port": port, "family": family, "proto": 0, "flags": 0}]

    async def close(self) -> None:
        pass


class _Handshake(asyncio.Protocol):
    """Reads the proxy's answers while the tunnel is set up, before aiohttp gets the connection."""

    def __init__(self) -> None:
        self.transport: asyncio.Transport | None = None
        self._buf = bytearray()
        self._waiter: asyncio.Future[None] | None = None
        self._end: BaseException | None = None

    def connection_made(self, transport: asyncio.BaseTransport) -> None:
        self.transport = transport  # type: ignore[assignment]

    def data_received(self, data: bytes) -> None:
        self._buf += data
        self._wake()

    def eof_received(self) -> bool | None:
        self._end = self._end or ConnectionError("the proxy closed the connection")
        self._wake()
        return None

    def connection_lost(self, exc: BaseException | None) -> None:
        self._end = self._end or exc or ConnectionError("the proxy closed the connection")
        self._wake()

    def _wake(self) -> None:
        if self._waiter is not None and not self._waiter.done():
            self._waiter.set_result(None)

    async def _more(self) -> None:
        if self._end is not None:
            raise ConnectionError(str(self._end) or "the proxy closed the connection")
        self._waiter = asyncio.get_running_loop().create_future()
        await self._waiter

    async def read(self, n: int) -> bytes:
        while len(self._buf) < n:
            await self._more()
        out = bytes(self._buf[:n])
        del self._buf[:n]
        return out

    async def read_head(self, limit: int = 16 * 1024) -> bytes:
        """An HTTP response head, up to its blank line."""
        while (i := self._buf.find(b"\r\n\r\n")) < 0:
            if len(self._buf) > limit:
                raise _ProxyError("the proxy's answer to CONNECT is too long")
            await self._more()
        out = bytes(self._buf[:i])
        del self._buf[:i + 4]
        return out

    def leftover(self) -> bool:
        return bool(self._buf)


# RFC 1928's replies.
_SOCKS_REPLIES = {1: "general failure", 2: "not allowed by the ruleset", 3: "network unreachable",
                  4: "host unreachable", 5: "connection refused", 6: "TTL expired",
                  7: "command not supported", 8: "address type not supported"}


class _ProxyConnector(aiohttp.TCPConnector):
    """A connector whose connections go through the proxy the transport picks for each: a
    tunnel, by HTTP CONNECT or SOCKS5 CONNECT, or none (direct). TLS to the server (and to an
    https:// proxy) runs over the tunnel with the same context. It replaces the one step of
    TCPConnector that opens a socket, as aiohttp-socks does. Names are resolved here only for
    direct connections: through a proxy they may resolve on its side only."""

    def __init__(self, transport: Transport, ssl_context: ssl.SSLContext) -> None:
        super().__init__(ssl=ssl_context, resolver=_NoResolver())
        self._transport = transport
        self._proxy_ssl = ssl_context
        self._direct_resolver = aiohttp.ThreadedResolver()

    async def _wrap_create_connection(self, *args: Any, addr_infos: list[Any], req: aiohttp.ClientRequest,
                                      timeout: aiohttp.ClientTimeout,
                                      client_error: type[Exception] = aiohttp.ClientConnectorError,
                                      **kwargs: Any) -> tuple[asyncio.Transport, Any]:
        factory = args[0]
        host, port = addr_infos[0][4][0], addr_infos[0][4][1]
        route = self._transport.route(req.url.scheme, host, port)
        if route is None:
            try:
                hosts = await self._direct_resolver.resolve(host, port, family=self._family)
            except OSError as e:
                raise aiohttp.ClientConnectorDNSError(req.connection_key, e) from e
            return await super()._wrap_create_connection(
                *args, addr_infos=self._convert_hosts_to_addr_infos(hosts), req=req, timeout=timeout,
                client_error=client_error, **kwargs)
        server_ssl: ssl.SSLContext | None = kwargs.get("ssl")
        transport: asyncio.Transport | None = None
        try:
            async with asyncio.timeout(timeout.sock_connect):
                transport, hs = await self._tunnel(route, host, port)
                transport.pause_reading()
                if hs.leftover():
                    raise _ProxyError("the proxy sent data before the server did")
                proto = factory()
                if server_ssl is None:
                    transport.set_protocol(proto)
                    proto.connection_made(transport)
                    transport.resume_reading()
                    return transport, proto
                tls = await asyncio.get_running_loop().start_tls(
                    transport, proto, server_ssl, server_hostname=kwargs.get("server_hostname") or host,
                    ssl_handshake_timeout=timeout.total or None)
                if tls is None:
                    raise ConnectionError("the connection closed during the TLS handshake")
                proto.connection_made(tls)
                return tls, proto  # type: ignore[return-value]
        except BaseException as e:
            if transport is not None:
                transport.abort()
            if isinstance(e, _ProxyError):
                raise aiohttp.ClientProxyConnectionError(req.connection_key, OSError(None, f"{route}: {e}")) from e
            if isinstance(e, ssl.SSLCertVerificationError):
                raise aiohttp.ClientConnectorCertificateError(req.connection_key, e) from e
            if isinstance(e, ssl.SSLError):
                raise aiohttp.ClientConnectorSSLError(req.connection_key, e) from e
            if isinstance(e, OSError) and not isinstance(e, TimeoutError):
                raise aiohttp.ClientProxyConnectionError(
                    req.connection_key, OSError(e.errno, f"{route}: {e.strerror or e}")) from e
            raise

    async def _tunnel(self, r: _Route, host: str, port: int) -> tuple[asyncio.Transport, _Handshake]:
        """A connection to the proxy, tunnelled on to host:port."""
        tls = r.scheme == "https"
        transport, hs = await asyncio.get_running_loop().create_connection(
            _Handshake, r.host, r.port, ssl=self._proxy_ssl if tls else None, server_hostname=r.host if tls else None)
        try:
            if r.scheme.startswith("socks"):
                await self._socks5(r, hs, host, port)
            else:
                await self._connect(r, hs, host, port)
        except BaseException:
            transport.abort()
            raise
        return transport, hs

    async def _connect(self, r: _Route, hs: _Handshake, host: str, port: int) -> None:
        target = f"[{host}]:{port}" if ":" in host else f"{host}:{port}"
        assert hs.transport is not None
        hs.transport.write(f"CONNECT {target} HTTP/1.1\r\nHost: {target}\r\n".encode("ascii")
                           + r.connect_headers + b"\r\n")
        head = (await hs.read_head()).decode("latin-1")
        status = head.split("\r\n", 1)[0]
        parts = status.split(" ", 2)
        # ASCII digits only: str.isdigit takes "²00" too, which int() then refuses.
        if len(parts) < 2 or not parts[0].startswith("HTTP/1.") or not (parts[1].isascii() and parts[1].isdigit()):
            raise _ProxyError(f"not an HTTP answer to CONNECT: {status[:80]!r}")
        if not 200 <= int(parts[1]) < 300:
            raise _ProxyError(f"CONNECT answered {' '.join(parts[1:])}")

    async def _socks5(self, r: _Route, hs: _Handshake, host: str, port: int) -> None:
        assert hs.transport is not None
        # Offer no authentication, and username and password when there are some.
        hs.transport.write(b"\x05\x02\x00\x02" if r.socks_auth else b"\x05\x01\x00")
        version, method = await hs.read(2)
        if version != 5:
            raise _ProxyError("not a SOCKS5 proxy")
        if method == 2 and r.socks_auth:
            user, password = r.socks_auth
            hs.transport.write(b"\x01" + bytes([len(user)]) + user + bytes([len(password)]) + password)
            _, status = await hs.read(2)
            if status != 0:
                raise _ProxyError("the username and password were refused")
        elif method != 0:
            raise _ProxyError("no authentication method in common" if method == 0xFF
                              else f"unsupported authentication method {method}")
        try:
            ip: ipaddress.IPv4Address | ipaddress.IPv6Address | None = ipaddress.ip_address(host)
        except ValueError:
            ip = None
        if ip is None and r.resolve_locally:  # otherwise the proxy resolves the name
            infos = await asyncio.get_running_loop().getaddrinfo(host, port, type=socket.SOCK_STREAM)
            ip = ipaddress.ip_address(str(infos[0][4][0]).split("%", 1)[0])
        if ip is None:
            name = host.encode("ascii")
            if len(name) > 255:
                raise _ProxyError("host name too long for SOCKS5")
            address = b"\x03" + bytes([len(name)]) + name
        else:
            address = (b"\x01" if ip.version == 4 else b"\x04") + ip.packed
        hs.transport.write(b"\x05\x01\x00" + address + port.to_bytes(2, "big"))
        version, reply, _, kind = await hs.read(4)
        if version != 5:
            raise _ProxyError("not a SOCKS5 answer")
        if reply != 0:
            raise _ProxyError(f"CONNECT refused: {_SOCKS_REPLIES.get(reply, f'reply {reply}')}")
        # The address the proxy bound: not needed.
        if kind == 1:
            await hs.read(4 + 2)
        elif kind == 4:
            await hs.read(16 + 2)
        elif kind == 3:
            (n,) = await hs.read(1)
            await hs.read(n + 2)
        else:
            raise _ProxyError(f"address type {kind} in the SOCKS5 answer")
