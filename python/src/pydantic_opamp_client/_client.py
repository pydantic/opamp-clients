# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The client: feeds the core, carries out its effects, runs its timer."""

from __future__ import annotations

import asyncio
import os
import ssl
import threading
import time
from collections import deque
from collections.abc import Callable
from dataclasses import dataclass, field, fields
from typing import Any

import aiohttp

from ._core import Core
from ._executor import Executor, clear_downloads, read_state
from ._files import REAL, AfterClear, Files, Journal
from ._proto.opamp.core.v1 import core_pb2 as corepb
from ._proto.opamp.v1 import opamp_pb2 as pb
from ._streams import Streams, proxy_choice
from ._transport import TLS, Proxy, Transport, redacted_headers, redacted_url


def _u64(n: int) -> int:
    """A time or size for the core: a negative one reaches it as its two's complement, which the core
    refuses at start, naming the field and the number meant (as in every SDK)."""
    return n & 0xFFFF_FFFF_FFFF_FFFF


@dataclass
class Config:
    """The client's settings."""

    #: The server URL: http(s)://... for plain HTTP, ws(s)://... for WebSocket.
    endpoint: str
    #: The instance UID to start with (16 bytes). The server may assign another. None: the core
    #: makes one (a UUIDv7, from the clock and the operating system's randomness), and with a
    #: state_file keeps it across restarts. ``Client.instance_uid`` says which is in use.
    instance_uid: bytes | None = None
    #: Capabilities the agent supports (AgentCapabilities bits). ReportsStatus and ReportsHeartbeat
    #: are always added, and the Reports* bits for status you set.
    capabilities: int = 0
    #: Heartbeat (WebSocket) or polling (HTTP) interval until the server offers one; 0: 30 s.
    heartbeat_interval_ms: int = 0
    #: Sent with every request or upgrade (Authorization, say). One that can't be sent as given (a
    #: name that isn't a token, a control character in a value or whitespace at its ends, one of
    #: the headers the HTTP stack writes) fails each request, with why, in both modes.
    headers: dict[str, str] = field(default_factory=dict)
    #: Plain HTTP request timeout; 0: 30 s.
    request_timeout_ms: int = 0
    #: The largest message sent, encoded; 0: 64 MiB, and at least 1 KiB. A status that doesn't fit
    #: is left out (``status_too_large``).
    #: covers: OPAMP-SIZE-02, OPAMP-SIZE-08
    max_send_bytes: int = 0
    #: The largest response or message accepted; 0: 64 MiB.
    #: covers: OPAMP-SIZE-02, OPAMP-SIZE-08
    max_receive_bytes: int = 0
    #: Where the client keeps state across restarts; None: nowhere. It holds secrets (credentials
    #: the server issued, private keys of telemetry settings' certificates), so it is written
    #: readable by the agent's user only (mode 0600). On Windows it takes its directory's
    #: permissions: keep it on a local path only the agent's user can read.
    state_file: str | None = None
    #: Take offers of new connection settings from the server (docs/connection-settings.md).
    #: Offers are refused without a state_file: a restart would lose them.
    accept_connection_settings: bool = False
    #: How long offered settings have to prove themselves; 0: 60 s.
    verify_timeout_ms: int = 0
    #: Take packages from the server (docs/packages.md): the client downloads them to
    #: ``state_file + ".downloads"`` and the application installs them (``install_package``,
    #: ``remove_package``). Only with a state_file: a restart would forget what the agent has.
    accept_packages: bool = False
    #: Ask the server to assign the instance UID (the RequestInstanceUid flag,
    #: docs/instance-uid.md). The UID the client starts with (made up, or instance_uid) is the
    #: temporary value until the server assigns one. With a state_file the assigned UID is kept,
    #: and a restart doesn't ask again; without one every start asks again, and the server assigns
    #: a new UID each time: the agent is a new agent after every restart.
    #: covers: OPAMP-UID-04
    request_instance_uid: bool = False
    #: The largest package file downloaded; 0: 1 GiB.
    max_package_bytes: int = 0
    #: How long a download may take; 0: 10 minutes.
    download_timeout_ms: int = 0
    #: How long stop may take (the goodbye, and any closing handshake); 0: 5 s.
    stop_timeout_ms: int = 0
    #: How long a WebSocket connection may take to open; 0: 30 s.
    connect_timeout_ms: int = 0
    #: TLS for https:// and wss:// endpoints, https:// proxies and package downloads, in both
    #: modes: a TLS (the CAs to trust, a client certificate, versions), or an ssl.SSLContext used
    #: as it is. None: the system's CAs, TLS 1.2 or later.
    #: covers: RULE-TLS-01, RULE-TLS-02
    tls: TLS | ssl.SSLContext | None = None
    #: The proxy to reach the server and download packages through: a Proxy, or just its URL
    #: (``http://``, ``https://``, ``socks5://`` or ``socks5h://``, with ``user:password@`` if
    #: it needs them; through SOCKS5 the proxy resolves the server's name, unless the Proxy has
    #: resolve_locally). None: the environment's, read at start (HTTP_PROXY for http:// and
    #: ws://, HTTPS_PROXY for https:// and wss://, ALL_PROXY for either when its own is unset,
    #: NO_PROXY: names, addresses and CIDR networks), by the rule every SDK's core mode shares; in
    #: platform mode as urllib reads them (lower-case names first, name:port entries too).
    #: Proxy.DIRECT: none at all.
    #: covers: RULE-PROXY-01
    proxy: Proxy | str | None = None
    #: Who speaks HTTP and WebSocket. ``"core"`` (the default): the shared core itself,
    #: over asyncio's TCP connections and TLS with the settings above; Python
    #: only opens connections, starts TLS, picks the proxy and writes package files. ``"platform"``:
    #: aiohttp, for an application that needs Python's own stack (urllib's reading of the
    #: environment's proxies, say).
    mode: str = "core"
    #: Core mode: package files go from the socket to the file without passing through the core
    #: (where the response says how long the body is). It is core mode's: False in platform mode is
    #: an error.
    splice_downloads: bool = True

    def __repr__(self) -> str:
        # Without the secrets: the headers' values (Authorization, say) and a proxy URL's password.
        def shown(name: str, value: object) -> object:
            if name == "headers":
                return redacted_headers(self.headers)
            if name == "proxy" and isinstance(value, str):
                return redacted_url(value)
            return value

        parts = (f"{f.name}={shown(f.name, getattr(self, f.name))!r}" for f in fields(self))
        return f"Config({', '.join(parts)})"


@dataclass
class Status:
    """The agent's status as the client starts. The first message reports it in full."""

    description: pb.AgentDescription | None = None
    health: pb.ComponentHealth | None = None
    effective_config: pb.EffectiveConfig | None = None
    remote_config_status: pb.RemoteConfigStatus | None = None
    # The components the agent has (leave `hash` empty: the core computes it). The server gets the
    # hash, and the components in full when it asks.
    available_components: pb.AvailableComponents | None = None
    # The custom capabilities the agent supports: custom messages for them reach the application,
    # and it may send them.
    custom_capabilities: pb.CustomCapabilities | None = None


@dataclass
class Notification:
    """What the application is told. `kind` says which; the other fields go with it:

    - ``connected``: a WebSocket connected, or an HTTP request succeeded (the first, or the first
      after failures)
    - ``connect_failed``: ``status`` (an HTTP status, if there was one), ``detail``
    - ``error_response``: ``error_type`` (ServerErrorResponseType), ``message``
    - ``remote_config``: ``remote_config`` (AgentRemoteConfig): answer with set_remote_config_status
    - ``custom_message``: ``custom_message``, from the server, for a declared custom capability
    - ``server_custom_capabilities``: ``custom_capabilities`` (the server's, a list of strings)
    - ``custom_message_not_sent``: ``custom_message``, ``detail`` (why it won't be sent)
    - ``connection_settings``: ``connection_settings`` (ConnectionSettingsOffers: own telemetry and
      other connection settings, all of them): answer with set_connection_settings_status
    - ``install_package``: ``package_name``, ``package`` (PackageAvailable: version, hash, type, the
      file's signature), ``path`` (the downloaded file): install it (checking the signature is the
      application's job), then answer with package_installed; the client deletes the file after
    - ``remove_package``: ``package_name``, no longer offered: remove it, then answer with
      package_removed
    - ``command``: ``command`` (ServerToAgentCommand)
    - ``instance_uid_changed``: ``instance_uid``
    - ``heartbeat_interval``: ``heartbeat_interval_ms`` (0: heartbeats off)
    - ``status_too_large``: ``field`` (the field's name in AgentToServer, as the core gives it:
      agent_description, health, effective_config, ...), ``bytes``
    - ``bad_message``: ``detail``
    - ``save_stalled``: ``stalled`` (True: the save under way has run for its stall time, and the
      client waits on it still, saving nothing else meanwhile; False: it has ended at last),
      ``waited_ms`` (how long it had run). A disk that hangs is the application's to see to.
    - ``stopped``: ``error`` (why the client stopped on its own, which stop() raises too; None
      after stop())
    """

    kind: str
    status: int | None = None
    detail: str = ""
    error_type: int = 0
    message: str = ""
    remote_config: pb.AgentRemoteConfig | None = None
    connection_settings: pb.ConnectionSettingsOffers | None = None
    custom_message: pb.CustomMessage | None = None
    custom_capabilities: list[str] | None = None
    command: pb.ServerToAgentCommand | None = None
    package_name: str = ""
    package: pb.PackageAvailable | None = None
    path: str = ""
    instance_uid: bytes = b""
    heartbeat_interval_ms: int = 0
    field: str = ""
    bytes: int = 0
    stalled: bool = False
    waited_ms: int = 0
    error: BaseException | None = None


#: Once the core has stopped, at least this for what it gave up on to unwind (a download's file
#: deleted once the write in flight is over, a cancelled request's task ended), however much of
#: the stop timeout the core took: stop returns within stop_timeout_ms and this.
STOP_GRACE_S = 1.0

# The inputs that report an effect's outcome, each naming the effect by its id.
_RESULTS = frozenset({"http_response", "http_failed", "ws_opened", "ws_failed", "ws_message", "ws_closed", "ws_lost",
                      "stored", "store_failed", "downloaded", "download_failed", "download_response"})


def _names(input_: corepb.Input, id_: int) -> bool:
    """Whether `input_` reports on effect `id_`."""
    kind = input_.WhichOneof("kind")
    return kind in _RESULTS and getattr(input_, kind).id == id_


def _notification(n: corepb.Notification, download_path: Callable[[int], str]) -> Notification | None:
    kind = n.WhichOneof("kind")
    if kind == "connected":
        return Notification("connected")
    if kind == "connect_failed":
        status = n.connect_failed.status if n.connect_failed.HasField("status") else None
        return Notification("connect_failed", status=status, detail=n.connect_failed.detail)
    if kind == "error_response":
        return Notification("error_response", error_type=n.error_response.kind, message=n.error_response.message)
    if kind == "remote_config":
        return Notification("remote_config", remote_config=n.remote_config)
    if kind == "custom_message":
        return Notification("custom_message", custom_message=n.custom_message)
    if kind == "server_custom_capabilities":
        return Notification("server_custom_capabilities",
                            custom_capabilities=list(n.server_custom_capabilities.capabilities))
    if kind == "custom_message_not_sent":
        return Notification("custom_message_not_sent", custom_message=n.custom_message_not_sent.message,
                            detail=n.custom_message_not_sent.reason)
    if kind == "connection_settings":
        return Notification("connection_settings", connection_settings=n.connection_settings)
    if kind == "install_package":
        p = n.install_package
        return Notification("install_package", package_name=p.name, package=p.package,
                            path=download_path(p.download))
    if kind == "remove_package":
        return Notification("remove_package", package_name=n.remove_package.name)
    if kind == "command":
        return Notification("command", command=n.command)
    if kind == "instance_uid_changed":
        return Notification("instance_uid_changed", instance_uid=bytes(n.instance_uid_changed))
    if kind == "heartbeat_interval_ms":
        return Notification("heartbeat_interval", heartbeat_interval_ms=n.heartbeat_interval_ms)
    if kind == "status_too_large":
        s = n.status_too_large
        return Notification("status_too_large", field=s.name, bytes=s.bytes)
    if kind == "bad_message":
        return Notification("bad_message", detail=n.bad_message.detail)
    if kind == "save_stalled":
        return Notification("save_stalled", stalled=n.save_stalled.stalled, waited_ms=n.save_stalled.waited_ms)
    if kind == "stopped":
        return Notification("stopped")
    return None


class Client:
    """A running OpAMP client, on the current asyncio event loop. Use :meth:`start`. Its methods
    may be called from any thread (a call from another thread is carried out on the loop), and
    :meth:`stop` awaited on any loop (on another, it is carried out on the client's)."""

    def __init__(self, config: Config, status: Status, notify: Callable[[Notification], None] | None,
                 _token: object, _core_engine: Any = None, _files: Files | None = None,
                 _sessions: Callable[[str], aiohttp.ClientSession] | None = None) -> None:
        if _token is not _START:
            raise TypeError("use Client.start")
        # First: settings that can't work fail here, before anything is done.
        # Nothing one mode alone honours is ignored: it is an error that says what to set.
        if config.mode not in ("core", "platform"):
            raise ValueError(f'mode is "core" or "platform", not {config.mode!r}')
        core_mode = config.mode == "core"
        if not core_mode and not config.splice_downloads:
            raise ValueError('splice_downloads in platform mode: it is core mode\'s (aiohttp writes package files itself); '
                             'leave it True, or leave mode as it is ("core")')
        transport = Transport(config.tls, config.proxy, core_mode=core_mode)
        # The core takes the transport from the endpoint's scheme, keeps every effect's time (and
        # cancels what it gives up on), decides what a download's status means, and makes the
        # instance UID if the application chose none, and the backoff's jitter seed from the
        # entropy (seed left 0: two clients started in the same tick don't retry in step).
        self._core = Core(corepb.Config(
            instance_uid=config.instance_uid or b"",
            entropy=os.urandom(32),
            unix_ms=time.time_ns() // 1_000_000,
            stop_timeout_ms=_u64(config.stop_timeout_ms),
            connect_timeout_ms=_u64(config.connect_timeout_ms),
            capabilities=config.capabilities,
            heartbeat_interval_ms=_u64(config.heartbeat_interval_ms),
            endpoint=config.endpoint,
            headers=[corepb.Header(name=k, value=v) for k, v in config.headers.items()],
            request_timeout_ms=_u64(config.request_timeout_ms),
            max_send_bytes=_u64(config.max_send_bytes),
            max_receive_bytes=_u64(config.max_receive_bytes),
            persistence=config.state_file is not None,
            saved_state=read_state(config.state_file),
            accept_connection_settings=config.accept_connection_settings,
            verify_timeout_ms=_u64(config.verify_timeout_ms),
            accept_packages=config.accept_packages,
            request_instance_uid=config.request_instance_uid,
            max_package_bytes=_u64(config.max_package_bytes),
            download_timeout_ms=_u64(config.download_timeout_ms),
            streams=core_mode,
            splice_downloads=core_mode and config.splice_downloads,
            proxy=proxy_choice(transport) if core_mode else None,
        ), _engine=_core_engine)
        # All of stop takes the core's stop timeout, as given or by default: its start reports it.
        self._stop_timeout = (self._core.limits.stop_timeout_ms if self._core.limits else 0) / 1000
        self._stop_called: float | None = None  # when stop() was first called (the loop's clock)
        # The file system saves and download files go through; with the tap on, each operation
        # is recorded there too, the state file named first.
        files = _files or REAL
        if config.state_file is not None and self._core.tapped:
            files = Journal(files, self._core.record_file, config.state_file)
        if config.state_file is not None:
            # Files downloaded before a restart: deleted beside the loop (a slow disk holds up
            # neither start nor the application), and before this client makes any of its own.
            files = AfterClear(files, clear_downloads(files, config.state_file))
        self._notify = notify or (lambda _n: None)
        # Notifications from while the client is starting: delivered once `start` has returned.
        self._held: list[Notification] | None = []
        self._loop = asyncio.get_running_loop()
        self._thread = threading.get_ident()
        # The loop's clock (time.monotonic, unless a test's loop keeps its own).
        self._start = self._loop.time()
        self._x = Executor(self._feed, config.state_file, transport, files, sessions=_sessions)
        # Core mode's streams and files; None otherwise.
        self._streams = Streams(self._feed, transport, config.state_file, client=self._x.client,
                                files=files) if core_mode else None
        self._queue: deque[corepb.Input] = deque()
        self._draining = False
        self._timer: asyncio.TimerHandle | None = None
        self._done = self._loop.create_future()
        self._finished = False
        self._said_stopped = False
        for name in ("description", "health", "effective_config", "remote_config_status", "available_components",
                     "custom_capabilities"):
            value = getattr(status, name)
            if value is not None:
                self._feed(corepb.Input(**{f"set_{name}": value}))
        self._feed(corepb.Input(start=corepb.Empty()))
        self._loop.call_soon(self._release)

    @classmethod
    def start(cls, config: Config, status: Status,
              notify: Callable[[Notification], None] | None = None, *,
              _core_engine: Any = None, _files: Files | None = None,
              _sessions: Callable[[str], aiohttp.ClientSession] | None = None) -> Client:
        """Start a client on the running event loop. `notify` is called for every notification,
        on the loop, never before `start` returns (a saved remote config is handed over at start):
        keep it quick. TLS or proxy settings that can't work raise here: ValueError, or OSError
        and ssl.SSLError for files and PEM that can't be loaded."""
        # Tests only: _core_engine, run instead of the native core (the adversary); _files, a
        # file system standing in for the real one; _sessions (Executor's).
        return cls(config, status, notify, _START, _core_engine, _files, _sessions)

    @property
    def instance_uid(self) -> bytes:
        """The instance UID in use: the application's, one saved before a restart, one the core
        made, or one the server assigned (``instance_uid_changed`` tells of a new one)."""
        return self._core.instance_uid

    @property
    def endpoint(self) -> str | None:
        """The endpoint the client uses now: where its latest OpAMP request or WebSocket connection
        went, which after the server moved the agent (an offer of connection settings) is the
        offered one, or the configured one again once an offer failed. None before any outcome,
        and in core mode, where the core makes the requests itself (the configured one is all
        there is to name then). The URL is as the core gave it: redact it before logging."""
        return None if self._streams is not None else self._x.used

    def _release(self) -> None:
        """Deliver what came while starting, now that the caller has the client."""
        held, self._held = self._held or [], None
        try:
            for n in held:
                self._notify(n)
        except BaseException as e:  # the application's callback
            self._failed(e)

    def set_description(self, value: pb.AgentDescription) -> None:
        """Change the agent description."""
        self._call(corepb.Input(set_description=value))

    def set_health(self, value: pb.ComponentHealth) -> None:
        """Change the health."""
        self._call(corepb.Input(set_health=value))

    def set_effective_config(self, value: pb.EffectiveConfig) -> None:
        """Change the effective config."""
        self._call(corepb.Input(set_effective_config=value))

    def set_custom_capabilities(self, value: pb.CustomCapabilities) -> None:
        """Change the custom capabilities the agent supports."""
        self._call(corepb.Input(set_custom_capabilities=value))

    def send_custom_message(self, value: pb.CustomMessage) -> None:
        """Send a custom message, for one of the agent's custom capabilities. It goes with the next
        message the server takes, and again if that message is lost; if it can't be sent at all, a
        custom_message_not_sent notification says why."""
        self._call(corepb.Input(send_custom_message=value))

    def set_available_components(self, value: pb.AvailableComponents) -> None:
        """Change the components the agent has (leave `hash` empty: the core computes it)."""
        self._call(corepb.Input(set_available_components=value))

    def set_remote_config_status(self, value: pb.RemoteConfigStatus) -> None:
        """Report on a remote config (the answer to a remote_config notification)."""
        self._call(corepb.Input(set_remote_config_status=value))

    def set_connection_settings_status(self, value: pb.ConnectionSettingsStatus) -> None:
        """Report on telemetry settings (the answer to a connection_settings notification)."""
        self._call(corepb.Input(set_connection_settings_status=value))

    def package_installed(self, name: str, hash_: bytes, error: str | None = None) -> None:
        """Answer an install_package notification: the package at `hash_` is installed, or (`error`)
        why it isn't."""
        self._call(corepb.Input(package_installed=corepb.PackageInstalled(
            name=name, hash=hash_, ok=error is None, detail=error or "")))

    def package_removed(self, name: str) -> None:
        """Answer a remove_package notification: the package is removed."""
        self._call(corepb.Input(package_removed=corepb.PackageRemoved(name=name)))

    async def stop(self) -> None:
        """Send agent_disconnect if a message can still be sent, then stop: this returns when the
        core says it has stopped, within ``stop_timeout_ms`` (5 s by default). Nothing the client
        started is running then. Awaited on another event loop (another thread's), it is carried
        out on the client's, and returns (or raises) as it does there."""
        if asyncio.get_running_loop() is not self._loop:
            # The core is stepped on the client's loop only, and its futures are that loop's: fed
            # from here, the stop raced the loop's own steps, and the await raised (a future of
            # another loop) with the stop half done.
            if not self._done.done():
                if not self._loop.is_running():
                    raise RuntimeError("stop: the client's event loop isn't running")
                there = asyncio.run_coroutine_threadsafe(self.stop(), self._loop)
                here = asyncio.wrap_future(there)
                # Waited on while the client's loop runs: a loop stopped before it ran the stop
                # would leave it waiting forever.
                while not (await asyncio.wait({here}, timeout=0.1))[0]:
                    if not self._loop.is_running():
                        there.cancel()
                        raise RuntimeError("stop: the client's event loop stopped before the stop was done")
                here.result()
            return self._done.result()
        if self._stop_called is None:
            self._stop_called = self._loop.time()
        self._feed(corepb.Input(stop=corepb.Empty()))
        await asyncio.shield(self._done)

    def _call(self, input_: corepb.Input) -> None:
        """An application's call: fed now on the loop's thread, from another thread on the loop."""
        if threading.get_ident() == self._thread:
            self._feed(input_)
        elif not self._finished:
            try:
                self._loop.call_soon_threadsafe(self._feed, input_)
            except RuntimeError:
                # The loop is closed: the client can never step again, and a call after stop does
                # nothing, from whichever thread it comes (on the loop's thread it already did).
                if not self._loop.is_closed():
                    raise

    def _now(self) -> int:
        return int((self._loop.time() - self._start) * 1000)

    def _feed(self, next_: corepb.Input) -> None:
        """Step the core with every queued input, in order, carrying out the effects."""
        if self._finished:
            return
        self._queue.append(next_)
        if self._draining:
            return
        self._draining = True
        try:
            while self._queue and not self._finished:
                out = self._core.step(self._now(), self._queue.popleft())
                # stopped is told last: the core may emit a notification after it in the same
                # output (install_package, in the step that answers a save after stop), and
                # nothing reaches the application after stopped (RULE-SHELL-01).
                stopped: Notification | None = None
                for effect in out.effects:
                    kind = effect.WhichOneof("kind")
                    if kind is None:
                        # A newer module than this client: fail loudly rather than skip what the
                        # core asked for.
                        raise RuntimeError("the core asked for an effect this client doesn't know: the client is older than its core")
                    if kind == "notify":
                        n = _notification(effect.notify, self._x.download_path)
                        if n is not None and n.kind == "stopped":
                            stopped = n
                        elif n is not None:
                            self._tell(n)
                    else:
                        if kind == "cancel":
                            # Nothing more about it reaches the core, not even what is queued.
                            # covers: RULE-CLIENT-15
                            id_ = effect.cancel.id
                            self._queue = deque(i for i in self._queue if not _names(i, id_))
                        if self._streams is None or not self._streams.do(effect):
                            self._x.do(effect)
                if stopped is not None:
                    self._tell(stopped)
                if self._timer is not None:
                    self._timer.cancel()
                    self._timer = None
                if out.HasField("wake_at_ms"):
                    delay = max(0, out.wake_at_ms - self._now()) / 1000
                    self._timer = self._loop.call_later(delay, self._feed, corepb.Input(timer=corepb.Empty()))
                # Core mode: the core ends its own goodbye and closing handshakes, within its stop
                # time, and has closed every stream by now.
                if out.stopped:
                    self._end()
        except BaseException as e:  # a CoreError, a bug, or the application's callback
            self._failed(e)
        finally:
            self._draining = False

    def _failed(self, e: BaseException) -> None:
        """Something raised: the client stops, and stop() raises it too. KeyboardInterrupt and the
        like go on up after."""
        if not self._finished:
            self._end(e)
        if not isinstance(e, Exception):
            raise e

    def _tell(self, n: Notification) -> None:
        """Tell the application, or hold it until start has returned."""
        self._said_stopped |= n.kind == "stopped"
        if self._held is not None:
            self._held.append(n)
        else:
            self._notify(n)

    def _end(self, err: BaseException | None = None) -> None:
        self._finished = True
        if self._timer is not None:
            self._timer.cancel()
        self._core.close()
        if err is not None and not self._said_stopped:
            # The client stopped on its own: the application hears it now, not only from stop().
            # If its callback is what failed, it may fail again: that is already the error reported.
            try:
                self._tell(Notification("stopped", error=err))
            except Exception:
                pass

        async def finish() -> None:
            # The core has already waited for the goodbye: anything left is dropped. All of stop
            # takes stop_timeout: what the core took of it (from stop()) is not given again, but
            # for STOP_GRACE_S at least for what was given up on to unwind (RULE-SHELL-07: it is
            # released, the deletion of a download's file included, by the time stop returns). A
            # core that takes all of its stop timeout and then says stopped with a download's file
            # still being written leaves the shell to delete it: given nothing, it was deleted
            # after stop had returned.
            loop = self._loop
            begun = loop.time() if self._stop_called is None else self._stop_called
            left = begun + self._stop_timeout - loop.time()
            released_by = loop.time() + max(left, STOP_GRACE_S)
            # A save still writing lands whole whenever it does: waited for within the stop
            # timeout only.
            await self._x.close(max(0.0, left), release=released_by - loop.time())
            # Core mode: files still being written are deleted once their write in flight is over.
            if self._streams is not None:
                await self._streams.shutdown(max(0.0, released_by - loop.time()))
            if not self._done.done():
                if err is None:
                    self._done.set_result(None)
                else:
                    self._done.set_exception(err)

        self._finish_task = self._loop.create_task(finish())


_START = object()
