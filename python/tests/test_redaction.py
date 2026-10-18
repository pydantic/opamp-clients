# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Noninterference of secrets, as the core's own redaction tests check it:
two configurations that differ only in their secrets' values, each as long in both, look the same
from outside. What an application logs of them, their repr, is the same text. The secrets: the
headers sent to the server, a proxy's password and its CONNECT headers, and a client key and its
password."""

from __future__ import annotations

from hypothesis import given
from hypothesis import strategies as st

from pydantic_opamp_client import TLS, Config, Proxy

# A secret's characters: printable, but none that ends a URL's user-info.
CHARS = st.characters(min_codepoint=0x21, max_codepoint=0x7E, exclude_characters="@/:%?#[]")


@st.composite
def pairs(draw: st.DrawFn) -> tuple[str, str]:
    """Two different secrets of one length."""
    a = draw(st.text(CHARS, min_size=1, max_size=40))
    b = draw(st.text(CHARS, min_size=len(a), max_size=len(a)).filter(lambda s: s != a))
    return a, b


def shown(token: str, password: str, proxy_token: str, key: str) -> list[str]:
    proxy = Proxy(f"http://agent:{password}@proxy.example:3128", headers={"Proxy-Authorization": proxy_token})
    tls = TLS(cert_pem="CERT", key_pem=key, key_password=key)
    configs = [
        Config("https://opamp.example", headers={"Authorization": token}, proxy=proxy, tls=tls),
        Config("https://opamp.example", headers={"Authorization": token},
               proxy=f"http://agent:{password}@proxy.example:3128"),
    ]
    return [repr(v) for v in (proxy, tls, *configs)]


@given(pairs(), pairs(), pairs(), pairs())
def test_secrets_change_nothing_shown(token: tuple[str, str], password: tuple[str, str],
                                      proxy_token: tuple[str, str], key: tuple[str, str]) -> None:
    one = shown(token[0], password[0], proxy_token[0], key[0])
    two = shown(token[1], password[1], proxy_token[1], key[1])
    assert one == two


def test_what_is_not_secret_is_still_shown() -> None:
    shown_ = " ".join(shown("tok", "pw", "ptok", "key"))
    for part in ("https://opamp.example", "Authorization", "Proxy-Authorization", "proxy.example:3128", "agent"):
        assert part in shown_, part
