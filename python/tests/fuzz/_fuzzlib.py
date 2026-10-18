# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""What the fuzz targets beside this file share: a reader of the fuzzer's bytes, the server that
answers from them (any bytes, or a ServerToAgent built from them and then perhaps damaged), and
the application whose callback calls the client as they say.

The bytes are read as they are needed, in the order the run asks (the loop is seeded, its clock
virtual: the same input is the same run). Past their end every read is 0: the server answers
with an empty message, and the callback does nothing."""

from __future__ import annotations

import hashlib
import struct

from pydantic_opamp_client import anyvalue_pb2, opamp_pb2 as pb


class Reader:
    """The fuzzer's bytes, read front to back; 0 past the end."""

    def __init__(self, data: bytes) -> None:
        self.data = data
        self.i = 0

    @property
    def empty(self) -> bool:
        return self.i >= len(self.data)

    def byte(self) -> int:
        if self.i >= len(self.data):
            return 0
        self.i += 1
        return self.data[self.i - 1]

    def u16(self) -> int:
        return self.byte() | self.byte() << 8

    def u32(self) -> int:
        return self.u16() | self.u16() << 16

    def u64(self) -> int:
        return self.u32() | self.u32() << 32

    def take(self, n: int) -> bytes:
        out = self.data[self.i:self.i + n]
        self.i += len(out)
        return out

    def blob(self, most: int = 255) -> bytes:
        """A length (one byte, or two when `most` is over 255), then that many bytes."""
        n = self.byte() if most <= 255 else self.u16()
        return self.take(min(n, most))

    def text(self, most: int = 32) -> str:
        # protobuf strings are UTF-8: what isn't becomes U+FFFD.
        return self.blob(most).decode("utf-8", "replace")

    def pick(self, choices):
        return choices[self.byte() % len(choices)]


def cuts(r: Reader, size: int) -> list[int]:
    """Where a body of `size` bytes is cut into pieces as it arrives (up to three cuts)."""
    return [r.u16() % (size + 1) for _ in range(r.byte() % 4)] if size else []


def pieces(data: bytes, at: list[int]) -> list[bytes]:
    """`data` in pieces, cut at `at`."""
    if not data:
        return []
    marks = sorted({a for a in at if 0 < a < len(data)})
    return [data[a:b] for a, b in zip([0, *marks], [*marks, len(data)])]


def token(b: bytes) -> bytes:
    """`b` as a header name: no control characters, spaces or colons."""
    return bytes(c for c in b if 32 < c < 127 and c != ord(":"))


def text(b: bytes) -> bytes:
    """`b` as a header value HTTP/1.1 parsers take: no control characters but tab (bytes 128 to
    255, obs-text, are allowed: Latin-1, or broken UTF-8)."""
    return bytes(c for c in b if c == 9 or 32 <= c != 127).strip(b" \t")


# What a status other than 200 may be, and the headers that may go with it.
STATUSES = [204, 301, 302, 400, 401, 403, 404, 429, 500, 503]
FILES = "http://files.test"


class Server:
    """An OpAMP server whose every answer is the fuzzer's. Package files it offers are kept, by
    URL, for the downloads that follow."""

    def __init__(self, r: Reader) -> None:
        self.r = r
        self.files: dict[str, bytes] = {}  # what each offered package URL serves
        self.answers = 0  # requests answered
        self.raw = 0  # answered with bytes that are no ServerToAgent of ours

    def headers(self) -> list[tuple[bytes, bytes]]:
        """Headers of the server's own, perhaps none: any bytes but CR and LF (a server or a proxy
        on the way sends what it likes: Latin-1, say)."""
        r = self.r
        out = []
        for _ in range(r.byte() % 3 if r.byte() & 1 else 0):
            name = r.pick([b"Server", b"Set-Cookie", b"X-Fuzz", b"Content-Type", r.blob(8) or b"X"])
            out.append((token(name), text(r.blob(32))))
        return [(k, v) for k, v in out if k]

    def answer(self, body: bytes) -> tuple[str, int, list[tuple[bytes, bytes]], bytes, list[int]]:
        """The answer to an OpAMP request whose body is `body`: (how, status, headers, data, cuts),
        how being "ok", "reset" (the connection drops) or "garbage" (core mode: `data` is all the
        server writes, status line included). Header names and values are bytes, as they go on
        the wire."""
        r = self.r
        self.answers += 1
        try:
            uid = pb.AgentToServer.FromString(body).instance_uid
        except Exception:  # noqa: BLE001 - the server echoes what it can
            uid = b""
        kind = r.byte() % 10
        status = 200
        headers = self.headers()
        if kind == 0:
            data = pb.ServerToAgent(instance_uid=uid).SerializeToString()
        elif kind == 1:
            self.raw += 1
            data = r.blob(4096)  # any bytes a server may send
        elif kind == 2:
            return "reset", 0, [], b"", []
        elif kind == 3:
            # Not 200: an error status, a redirect, a Retry-After (seconds, a date, or nonsense).
            status = r.pick(STATUSES)
            if r.byte() & 1:
                headers.append((b"Retry-After", r.pick([b"0", b"1", b"120", b"Wed, 21 Oct 2037 07:28:00 GMT", b"soon",
                                                        str(r.u32()).encode()])))
            if status in (301, 302):
                headers.append((b"Location", r.pick([b"http://opamp.test/v2", b"/elsewhere", b"https://x.test/", b""])))
            data = r.blob()
        elif kind == 4 and not r.empty:
            self.raw += 1
            return "garbage", 0, [], r.blob(1024), []
        else:
            msg = self.message(uid)
            data = msg.SerializeToString()
            if kind == 9 and data:
                # Damaged: a few bytes flipped, or cut short.
                b = bytearray(data)
                for _ in range(1 + r.byte() % 3):
                    b[r.u16() % len(b)] ^= r.byte() or 1
                if r.byte() & 1:
                    del b[r.u16() % len(b):]
                data = bytes(b)
                self.raw += 1
        return "ok", status, headers, data, cuts(r, len(data))

    def download(self, url: str) -> tuple[str, int, list[tuple[bytes, bytes]], bytes, list[int]]:
        """The answer to a package download from `url`."""
        r = self.r
        body = self.files.get(url)
        how = r.byte() % 8
        headers = self.headers()
        if body is None or how == 1:
            return "ok", 404, headers, b"missing", []
        if how == 2:
            return "reset", 0, [], b"", []
        if how == 3:
            # A redirect, to anywhere (a Location of any bytes but CR and LF).
            where = r.pick([f"{FILES}/moved".encode(), b"/p1", b"//other.test/p", b"http://[bad",
                            text(r.blob(40))])
            return "ok", r.pick(STATUSES), [*headers, (b"Location", where)], b"", []
        if how == 4:
            body = body[:-1] + bytes([body[-1] ^ 1]) if body else b"x"  # not the file offered
        return "ok", 200, headers, body, cuts(r, len(body))

    def upgrade(self) -> tuple[str, int, list[tuple[bytes, bytes]]]:
        """The answer to a WebSocket upgrade: ("ok", 101, []), ("status", status, headers) (refused),
        or ("reset", 0, [])."""
        r = self.r
        how = r.byte() % 6
        if how == 0:
            return "reset", 0, []
        if how == 1:
            return "status", r.pick([200, 301, 400, 401, 403, 404, 426, 429, 500, 503]), self.headers()
        return "ok", 101, []

    def ws_answer(self, body: bytes) -> tuple[str, bytes | int]:
        """What the server does with a message the client sent on a WebSocket: ("message", the
        frame's payload: the OpAMP header byte and a message, or any bytes), ("raw", any bytes: as
        frames, or not, where the client reads the frames itself; elsewhere a message's payload),
        ("close", a code), or ("drop", 0)."""
        how, status, _headers, data, _cuts = self.answer(body[1:] if body[:1] == b"\0" else body)
        if how == "reset":
            return "drop", 0
        if how == "garbage":
            return "raw", data
        if status != 200:
            return "close", self.r.pick([1000, 1001, 1002, 1008, 1011, 3000, 4999, 0])
        return "message", b"\0" + data

    def message(self, uid: bytes) -> pb.ServerToAgent:
        """A ServerToAgent built from the fuzzer's bytes: each part there or not, as a bit says."""
        r = self.r
        parts = r.u16()
        m = pb.ServerToAgent(instance_uid=uid if not parts & 1 else r.blob(20))
        if parts & 2:
            m.remote_config.config_hash = r.blob(32)
            for _ in range(r.byte() % 3):
                obj = m.remote_config.config.config_map[r.text(16)]
                obj.body = r.blob(512)
                obj.content_type = r.text(16)
        if parts & 4:
            self.packages(m.packages_available)
        if parts & 8:
            cs = m.connection_settings
            cs.hash = r.blob(16)
            if r.byte() & 1:
                cs.opamp.destination_endpoint = r.pick(["http://opamp.test/v1/opamp", "http://other.test/opamp",
                                                        "ws://opamp.test/v1/opamp", r.text(40)])
                cs.opamp.heartbeat_interval_seconds = r.pick([0, 1, 30, 3600, 2**63, 2**64 - 1, r.u32()])
                for _ in range(r.byte() % 3):
                    cs.opamp.headers.headers.add(key=r.text(16), value=r.text(32))
            if r.byte() & 1:
                cs.own_metrics.destination_endpoint = r.text(40)
                cs.own_metrics.headers.headers.add(key=r.text(16), value=r.text(32))
            for _ in range(r.byte() % 3):
                cs.other_connections[r.text(16)].destination_endpoint = r.text(40)
        if parts & 16:
            m.flags = r.pick([1, 2, 3, r.u64()])
        if parts & 32:
            m.capabilities = r.u64()
        if parts & 64:
            m.agent_identification.new_instance_uid = r.pick([bytes(16), bytes(range(16)), r.blob(20)])
        if parts & 128:
            m.command.type = r.byte() % 3  # (a type this client doesn't know too)
        if parts & 256:
            m.custom_capabilities.capabilities.extend(r.text(24) for _ in range(r.byte() % 4))
        if parts & 512:
            m.custom_message.capability = r.pick([CAPABILITY, r.text(24)])
            m.custom_message.type = r.text(16)
            m.custom_message.data = r.blob(256)
        if parts & 1024:
            m.error_response.type = r.byte() % 5
            m.error_response.error_message = r.text(64)
            if r.byte() & 1:
                m.error_response.retry_info.retry_after_nanoseconds = r.u64()
        if parts & 2048 and m.packages_available.packages:
            # The same offer again (the server repeats itself).
            again = pb.PackagesAvailable()
            again.CopyFrom(m.packages_available)
            m.packages_available.CopyFrom(again)
        return m

    def packages(self, offer: pb.PackagesAvailable) -> None:
        r = self.r
        offer.all_packages_hash = r.blob(16)
        for _ in range(r.byte() % 3):
            name = r.pick(["pkg", "other", "", "../up", "a/b", r.text(16)])
            p = offer.packages[name]
            p.type = r.byte() % 3
            p.version = r.text(8)
            p.hash = r.blob(16)
            if r.byte() % 4:
                n = len(self.files) + 1
                url = r.pick([f"{FILES}/p{n}", f"{FILES}/p{n}?x=1", "", "ftp://files.test/p", r.text(40)])
                body = r.blob(2048)
                if url.startswith(FILES):
                    self.files[url] = body
                p.file.download_url = url
                p.file.content_hash = r.pick([hashlib.sha256(body).digest(), hashlib.sha256(body).digest(), b"",
                                              r.blob(32)])
                p.file.signature = r.blob(16)
                for _ in range(r.byte() % 2):
                    p.file.headers.headers.add(key=r.text(16), value=r.text(32))


CAPABILITY = "io.example.fuzz"
CAPABILITIES = (pb.AgentCapabilities_AcceptsRemoteConfig | pb.AgentCapabilities_ReportsRemoteConfig
                | pb.AgentCapabilities_AcceptsPackages | pb.AgentCapabilities_ReportsPackageStatuses
                | pb.AgentCapabilities_AcceptsOpAMPConnectionSettings | pb.AgentCapabilities_AcceptsRestartCommand
                | pb.AgentCapabilities_ReportsHealth | pb.AgentCapabilities_ReportsEffectiveConfig
                | pb.AgentCapabilities_ReportsAvailableComponents | pb.AgentCapabilities_AcceptsOtherConnectionSettings
                | pb.AgentCapabilities_ReportsConnectionSettingsStatus)

# The application's calls a callback may make (and stop).
CALLS = ["set_health", "set_description", "set_effective_config", "set_remote_config_status", "package_installed",
         "package_removed", "set_connection_settings_status", "send_custom_message", "set_custom_capabilities",
         "set_available_components", "stop", "nothing"]


def attributes(r: Reader) -> list[anyvalue_pb2.KeyValue]:
    out = []
    for _ in range(r.byte() % 3):
        v = anyvalue_pb2.AnyValue()
        k = r.byte() % 4
        if k == 0:
            v.string_value = r.text(32)
        elif k == 1:
            v.int_value = struct.unpack("<q", struct.pack("<Q", r.u64()))[0]
        elif k == 2:
            v.bytes_value = r.blob(32)
        else:
            v.kvlist_value.values.add(key=r.text(8)).value.bool_value = bool(r.byte() & 1)
        out.append(anyvalue_pb2.KeyValue(key=r.text(16), value=v))
    return out


def call(r: Reader, client, what: str, seen: dict) -> str | None:
    """Make the application's call `what` on `client`, its arguments from the fuzzer's bytes and
    what the callback has been told (`seen`: the last notification of each kind). "stop" is
    returned for the caller to carry out."""
    if what == "set_health":
        client.set_health(pb.ComponentHealth(healthy=bool(r.byte() & 1), status=r.text(), last_error=r.text(),
                                             start_time_unix_nano=r.u64(), status_time_unix_nano=r.u64()))
    elif what == "set_description":
        client.set_description(pb.AgentDescription(identifying_attributes=attributes(r),
                                                   non_identifying_attributes=attributes(r)))
    elif what == "set_effective_config":
        ec = pb.EffectiveConfig()
        ec.config_map.config_map[r.text(8)].body = r.blob(256)
        client.set_effective_config(ec)
    elif what == "set_remote_config_status":
        last = seen.get("remote_config")
        h = last.remote_config.config_hash if last is not None and r.byte() & 1 else r.blob(32)
        client.set_remote_config_status(pb.RemoteConfigStatus(last_remote_config_hash=h, status=r.byte() % 5,
                                                              error_message=r.text()))
    elif what == "package_installed":
        last = seen.get("install_package")
        if last is not None and r.byte() % 4:
            name, h = last.package_name, last.package.hash if r.byte() % 4 else r.blob(16)
        else:
            name, h = r.pick(["pkg", "other", "", r.text(16)]), r.blob(16)
        client.package_installed(name, h, None if r.byte() & 1 else r.text())
    elif what == "package_removed":
        last = seen.get("remove_package")
        client.package_removed(last.package_name if last is not None and r.byte() & 1 else r.text(16))
    elif what == "set_connection_settings_status":
        last = seen.get("connection_settings")
        h = last.connection_settings.hash if last is not None and r.byte() & 1 else r.blob(16)
        client.set_connection_settings_status(pb.ConnectionSettingsStatus(
            last_connection_settings_hash=h, status=r.byte() % 4, error_message=r.text()))
    elif what == "send_custom_message":
        client.send_custom_message(pb.CustomMessage(capability=r.pick([CAPABILITY, r.text(24)]), type=r.text(16),
                                                    data=r.blob(512)))
    elif what == "set_custom_capabilities":
        client.set_custom_capabilities(pb.CustomCapabilities(capabilities=[r.pick([CAPABILITY, r.text(24)])
                                                                           for _ in range(r.byte() % 3)]))
    elif what == "set_available_components":
        ac = pb.AvailableComponents()
        for _ in range(r.byte() % 3):
            ac.components[r.text(16)].metadata.extend(attributes(r))
        client.set_available_components(ac)
    elif what == "stop":
        return "stop"
    return None
