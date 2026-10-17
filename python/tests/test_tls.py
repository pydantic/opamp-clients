# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""TLS as the application configures it: the CAs to trust, a client certificate, versions."""

import asyncio
import os
import ssl
import stat
import tempfile

import pytest
import trustme
from _servers import MODES, OpampServer, run_client

from opamp_client import TLS, Client, Config, Status

TRANSPORTS = ["http", "ws"]


def server_context(ca: trustme.CA, *, client_auth: bool = False) -> ssl.SSLContext:
    ctx = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH)
    ca.issue_cert("127.0.0.1", "localhost").configure_cert(ctx)
    if client_auth:
        ca.configure_trust(ctx)
        ctx.verify_mode = ssl.CERT_REQUIRED
    return ctx


# covers: RULE-TLS-01
@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
@pytest.mark.parametrize("ca_as", ["pem", "file"])
def test_trusts_the_ca_it_is_given(transport: str, ca_as: str, tmp_path, mode: str) -> None:
    async def run() -> None:
        ca = trustme.CA()
        server = await OpampServer(server_context(ca)).start()
        path = tmp_path / "ca.pem"
        ca.cert_pem.write_to_path(str(path))
        tls = TLS(ca_pem=ca.cert_pem.bytes()) if ca_as == "pem" else TLS(ca_file=str(path))
        try:
            notes = await run_client(server, mode=mode, endpoint=server.endpoint(transport), tls=tls)
        finally:
            await server.close()
        assert server.arrivals, [n for n in notes if n.kind == "connect_failed"]
        assert {a.tls for a in server.arrivals} <= {"TLSv1.2", "TLSv1.3"}

    asyncio.run(run())


# covers: RULE-TLS-01
@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
def test_refuses_a_server_it_cannot_verify(transport: str, mode: str) -> None:
    async def run() -> None:
        server = await OpampServer(server_context(trustme.CA())).start()
        try:
            notes = await run_client(server, mode=mode, endpoint=server.endpoint(transport), tls=TLS(ca_pem=trustme.CA().cert_pem.bytes()))
        finally:
            await server.close()
        assert not server.arrivals, "nothing is sent to a server the client cannot verify"
        failed = [n for n in notes if n.kind == "connect_failed"]
        assert failed and "certificate verify failed" in failed[0].detail, failed

    asyncio.run(run())


# covers: RULE-TLS-01
@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
def test_the_system_cas_are_the_default_and_can_be_added(transport: str, mode: str) -> None:
    """A private CA is not among the system's: the default refuses the server; system_cas with the
    CA trusts both (the system's are loaded)."""
    async def run() -> None:
        ca = trustme.CA()
        server = await OpampServer(server_context(ca)).start()
        try:
            refused = await run_client(server, mode=mode, endpoint=server.endpoint(transport))
            assert not server.arrivals and any(n.kind == "connect_failed" for n in refused)
            await run_client(server, mode=mode, endpoint=server.endpoint(transport), tls=TLS(ca_pem=ca.cert_pem.bytes(), system_cas=True))
            assert server.arrivals
        finally:
            await server.close()

    ctx = TLS(ca_pem=trustme.CA().cert_pem.bytes(), system_cas=True).context()
    default = ssl.create_default_context()
    assert ctx.cert_store_stats()["x509_ca"] == default.cert_store_stats()["x509_ca"] + 1
    asyncio.run(run())


# covers: RULE-TLS-02
@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
@pytest.mark.parametrize("cert_as", ["pem", "file"])
def test_presents_the_client_certificate(transport: str, cert_as: str, tmp_path, mode: str) -> None:
    async def run() -> None:
        ca = trustme.CA()
        agent = ca.issue_cert("agent.example", common_name="test-agent")
        server = await OpampServer(server_context(ca, client_auth=True)).start()
        cert, key = tmp_path / "cert.pem", tmp_path / "key.pem"
        agent.cert_chain_pems[0].write_to_path(str(cert))
        agent.private_key_pem.write_to_path(str(key))
        if cert_as == "pem":
            tls = TLS(ca_pem=ca.cert_pem.bytes(), cert_pem=agent.cert_chain_pems[0].bytes(),
                      key_pem=agent.private_key_pem.bytes().decode())
        else:
            tls = TLS(ca_pem=ca.cert_pem.bytes(), cert_file=str(cert), key_file=str(key))
        try:
            await run_client(server, mode=mode, endpoint=server.endpoint(transport), tls=tls)
            # Without one, the server refuses the handshake.
            before = len(server.arrivals)
            notes = await run_client(server, mode=mode, endpoint=server.endpoint(transport), messages=before + 1,
                                     tls=TLS(ca_pem=ca.cert_pem.bytes()))
        finally:
            await server.close()
        assert server.arrivals
        assert {a.client_cn for a in server.arrivals} == {"test-agent"}
        assert len(server.arrivals) == before and any(n.kind == "connect_failed" for n in notes)

    asyncio.run(run())


def test_pem_goes_through_private_temporary_files_deleted_at_once(monkeypatch, tmp_path) -> None:
    """ssl loads certificates and keys from files only: PEM given in memory is written to new
    files only the agent's user can read, deleted as soon as it is loaded, or fails to."""
    ca = trustme.CA()
    agent = ca.issue_cert("agent.example")
    seen: list[tuple[str, int]] = []
    real = ssl.SSLContext.load_cert_chain

    def spy(self, certfile, keyfile=None, password=None):
        for path in (certfile, keyfile):
            seen.append((path, stat.S_IMODE(os.stat(path).st_mode)))
        return real(self, certfile, keyfile, password)

    monkeypatch.setattr(ssl.SSLContext, "load_cert_chain", spy)
    monkeypatch.setattr(tempfile, "tempdir", str(tmp_path))
    TLS(cert_pem=agent.cert_chain_pems[0].bytes(), key_pem=agent.private_key_pem.bytes()).context()
    assert len(seen) == 2
    for path, mode in seen:
        assert os.path.dirname(path) == str(tmp_path)
        if os.name != "nt":
            assert mode == 0o600
        assert not os.path.exists(path)
    with pytest.raises(ssl.SSLError):
        TLS(cert_pem=agent.cert_chain_pems[0].bytes(), key_pem=b"not a key").context()
    assert os.listdir(tmp_path) == []


@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
def test_max_version(transport: str, mode: str) -> None:
    async def run() -> None:
        ca = trustme.CA()
        server = await OpampServer(server_context(ca)).start()
        try:
            await run_client(server, mode=mode, endpoint=server.endpoint(transport),
                             tls=TLS(ca_pem=ca.cert_pem.bytes(), max_version=ssl.TLSVersion.TLSv1_2))
        finally:
            await server.close()
        assert server.arrivals and {a.tls for a in server.arrivals} == {"TLSv1.2"}

    asyncio.run(run())


@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", TRANSPORTS)
def test_an_ssl_context_is_used_as_it_is(transport: str, mode: str) -> None:
    async def run() -> None:
        ca = trustme.CA()
        server = await OpampServer(server_context(ca)).start()
        ctx = ssl.create_default_context()
        ca.configure_trust(ctx)
        try:
            await run_client(server, mode=mode, endpoint=server.endpoint(transport), tls=ctx)
        finally:
            await server.close()
        assert server.arrivals

    asyncio.run(run())


# covers: RULE-TLS-01
@pytest.mark.parametrize("tls", [
    TLS(min_version=ssl.TLSVersion.TLSv1_1),
    TLS(min_version=ssl.TLSVersion.MINIMUM_SUPPORTED),
    TLS(cert_file="cert.pem"),
    TLS(key_pem="key"),
    TLS(cert_file="a", cert_pem="b", key_file="c"),
])
@pytest.mark.parametrize("mode", MODES)
def test_settings_that_cannot_work_fail_at_start(tls: TLS, mode: str, tmp_path) -> None:
    async def run() -> None:
        state = tmp_path / "state"
        os.makedirs(str(state) + ".downloads")
        with pytest.raises(ValueError):
            Client.start(Config(mode=mode, endpoint="https://127.0.0.1:1/v1/opamp", tls=tls, state_file=str(state)), Status())
        assert os.path.exists(str(state) + ".downloads"), "nothing was done"

    asyncio.run(run())
