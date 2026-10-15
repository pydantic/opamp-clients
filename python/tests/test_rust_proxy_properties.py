# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Rust mode's proxy rule (tests/shell-tables/README.md, "Rust mode's proxy rule"), beyond the
shared table's cases: properties of rust_proxy and rust_no_proxy over environments and hosts
Hypothesis draws, each checked against what the rule says independently of how the code reads
it (networks by ``ipaddress``, names by their labels).

Runs: ``HYPOTHESIS_EXAMPLES`` (default 300)."""

from __future__ import annotations

import ipaddress
import os

from hypothesis import given, settings, strategies as st

from opamp_client._transport import Transport, rust_no_proxy, rust_proxy

EXAMPLES = int(os.environ.get("HYPOTHESIS_EXAMPLES", "300"))

LABELS = st.sampled_from(["example", "api", "com", "org", "internal", "a", "b-1", "xn--bcher-kva"])
NAMES = st.lists(LABELS, min_size=1, max_size=4).map(".".join)
V4 = st.integers(0, 2**32 - 1).map(lambda n: str(ipaddress.IPv4Address(n)))
V6 = st.integers(0, 2**128 - 1).map(lambda n: str(ipaddress.IPv6Address(n)))
ADDRESSES = st.one_of(V4, V6)
HOSTS = st.one_of(NAMES, ADDRESSES)


@st.composite
def networks(draw: st.DrawFn) -> str:
    """A network as an entry gives it: any address in it, and its prefix."""
    v6 = draw(st.booleans())
    bits = 128 if v6 else 32
    addr = draw(st.integers(0, 2**bits - 1))
    prefix = draw(st.integers(0, bits))
    return f"{ipaddress.ip_address(addr) if v6 else ipaddress.IPv4Address(addr)}/{prefix}"


def name_entry(host: str) -> st.SearchStrategy[str]:
    """Name entries related to `host`: itself, a suffix of it (with a dot or without), its case
    changed, or another name."""
    labels = host.split(".")
    suffixes = [".".join(labels[i:]) for i in range(len(labels))]
    return st.one_of(
        st.sampled_from(suffixes),
        st.sampled_from(suffixes).map(lambda s: "." + s),
        st.sampled_from(suffixes).map(str.upper),
        NAMES,
        st.just("*"),
        st.sampled_from(suffixes).map(lambda s: f"{s}:8080"),
        st.sampled_from(suffixes).map(lambda s: "*." + s),
    )


def name_matches(entry: str, host: str) -> bool:
    """The rule for a name entry and a name host, read by labels: `*`; the host itself, or a
    dotted suffix of it (a leading dot on the entry or none), ignoring case."""
    if entry == "*":
        return True
    want = entry.lower().removeprefix(".").split(".")
    have = host.lower().split(".")
    return len(want) <= len(have) and have[len(have) - len(want):] == want


@settings(max_examples=EXAMPLES, deadline=None)
@given(st.data(), HOSTS)
# covers: RULE-PROXY-02
def test_no_proxy_follows_the_rule_entry_by_entry(data: st.DataObject, host: str) -> None:
    """Each entry decides alone (the list matches when one entry does), and how it decides is the
    rule: an address host by address and network entries, never by a name (nor `*`); a name host
    by name entries, never by an address."""
    try:
        ip = ipaddress.ip_address(host)
    except ValueError:
        ip = None
    entry = data.draw(st.one_of(networks(), ADDRESSES, name_entry(host if ip is None else "example.com")))
    try:
        net = ipaddress.ip_network(entry, strict=False)
    except ValueError:
        net = None
    if ip is not None:
        want = net is not None and net.version == ip.version and ip in net
    else:
        want = net is None and name_matches(entry, host)
    assert rust_no_proxy(entry, host) == want, (entry, host)
    # Spaces, empty entries and entries that match nothing change nothing; nor does the order.
    other = data.draw(st.lists(st.one_of(networks(), NAMES), max_size=3))
    unrelated = [e for e in other if not rust_no_proxy(e, host)]
    entries = data.draw(st.permutations([f"  {entry} ", "", *unrelated]))
    assert rust_no_proxy(",".join(entries), host) == want, (entries, host)


@settings(max_examples=EXAMPLES, deadline=None)
@given(
    st.sampled_from(["http", "https", "ws", "wss"]),
    HOSTS,
    st.fixed_dictionaries({}, optional={
        name: st.sampled_from(["", "  ", "http://p1:3128", "socks5://u:pw@s:1080", " p2:8080 "])
        for name in ["HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"]
    }),
    st.one_of(st.none(), st.sampled_from(["", "*", "example.com", "10.0.0.0/8", "::1"])),
    st.booleans(),
)
# covers: RULE-PROXY-02
def test_the_proxy_is_the_schemes_variable_then_all_proxy(scheme: str, host: str, env: dict[str, str],
                                                            no_proxy: str | None, cgi: bool) -> None:
    """The scheme's own variable, else ALL_PROXY, each by its upper-case name if set at all; empty
    is unset; NO_PROXY can only send it direct; under CGI nothing. Rust mode's Transport answers
    with exactly that."""
    if no_proxy is not None:
        env = {**env, "NO_PROXY": no_proxy}
    if cgi:
        env = {**env, "REQUEST_METHOD": "GET"}
    url = f"{scheme}://{'[' + host + ']' if ':' in host else host}:4320/v1/opamp"

    def var(upper: str) -> str | None:
        value = env[upper] if upper in env else env.get(upper.lower())
        return (value or "").strip() or None

    own = var("HTTPS_PROXY" if scheme in ("https", "wss") else "HTTP_PROXY")
    named = own if own is not None else var("ALL_PROXY")
    direct = cgi or named is None or rust_no_proxy(no_proxy or "", host)
    want = None if direct else named
    assert rust_proxy(env, url) == want
    answer = Transport(rust=True, _environ=env).proxy_for(url)
    assert (None if answer is None else answer.url) == want
    assert answer is None or (dict(answer.headers) == {} and not answer.resolve_locally)
