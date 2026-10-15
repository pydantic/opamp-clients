# Conformance suite

Every client runs the same scenarios against the same reference server, and the recorded
traces are checked against the spec. Where the core's sources are (a Cargo.toml at the root),
the same clients also run against the Rust server built from them (`--server rust`), which checks
the server as well. `OPAMP_CORE_ROOT` names a checkout with the core's sources elsewhere: the
runner then builds what comes from them there and runs it from its `target/`.

- `server/`: the reference server, in Go. It speaks plain HTTP and WebSocket directly
  (not through opamp-go's server) so that it can send a Close frame and wait for the answer,
  drop the TCP connection, reject one message, throttle for a window of time, or forget the
  agent as if it had restarted. It logs every frame and connection event as JSON lines. A
  scenario's times (`at_s`, `window_s`, a hold's `from_s`) count from the driver's first request
  (`first_contact` in the log), not from the server's start: a driver in a container that starts
  seconds late meets the faults at the same point of its run as one in process.
- The Rust reference server, built from the core's sources where they are. It takes the Go
  server's flags and writes the same log, plus a `core` event for each decision it
  makes. It injects no faults: it is here to be tested, not to test.
- `drivers/`: one small program per client, all following [DRIVERS.md](DRIVERS.md).
- `scenarios/`: one JSON file per scenario: which transports, how long, what the driver does
  (including restarts: `runs`), what the server does (including requiring and offering
  credentials: `credentials`), which client features it `needs`, and which checks apply.
- `runner/`: `run.py` plays the scenarios; `checks.py` holds the checks; `test_checks.py`
  tests the checks themselves.
- `clients.toml`: how to build and run each driver. A client marked `source` (the Rust client)
  is built from the core's sources, and the runner leaves it out where they aren't.

## Running it

```
python3 conformance/runner/run.py                         # everything
python3 conformance/runner/run.py --client go,java        # some clients
python3 conformance/runner/run.py --scenario close-frame  # some scenarios
python3 conformance/runner/run.py --list                  # clients, scenarios and checks
python3 conformance/runner/run.py --server rust           # every client against the Rust server (where it is built)
OPAMP_CORE_ROOT=../core python3 conformance/runner/run.py --server rust   # the same, the core's sources in ../core
python3 conformance/runner/run.py --jobs 4                # four runs at once (CI's setting)
python3 -m unittest conformance/runner/test_checks.py     # the checks' own tests
```

Needs Go, Python 3.13 and `uv`; the Java driver builds and runs in Docker. Results land in
`conformance/out/`: per run the server log, the driver log and `result.json`, and for the whole
invocation `results.json` and a table with one row per check and one column per client and
transport. A cell is the worst result across the scenarios that ran the check: `ok`, `FAIL` (a
MUST check failed), `warn` (a SHOULD check failed) or `-` (did not apply).

## Checks

Each check names the requirements it covers (the spec's, `OPAMP-*`, and the core's rules,
`RULE-*`) and its level. Checks that apply one of our own design rules rather than the spec,
such as sending the full status after a reconnect, are SHOULD-level and say so in their
docstring.

A check that cannot fail is useless, so `test_checks.py` requires, for every check, at least
one trace it passes and one it fails. It already caught a bug: `full_report_after_reconnect`
looked at the wrong connection and let a bare first message through.

## TLS and proxies

A scenario's `server` can ask the Go server for TLS or a proxy; the runner hands the driver what
it needs through `DRIVER_CA_FILE`, `DRIVER_CLIENT_CERT`, `DRIVER_CLIENT_KEY` and `DRIVER_PROXY`
(DRIVERS.md), and the scenario `needs` the matching client feature.

- `"tls": {"client_auth": "none" | "request" | "require"}`: the server listens with TLS only, with
  a certificate from an ECDSA P-256 CA made in memory at start, valid for `127.0.0.1`, `localhost`
  and `host.docker.internal`. The CA goes to the run's directory as `ca.pem`; with client auth, a
  client certificate and key the CA signed (`client.pem`, `client-key.pem`, subject
  `CN=conformance-agent`), which the server then verifies. The endpoint becomes `https://` or
  `wss://`. `"untrusted": true` hands the driver an unrelated CA instead. The server logs each
  handshake (`tls_handshake`: version, cipher, whether a client certificate came and its subject)
  and each failure on its own (`tls_handshake_failed`: the error, and the alert the client sent,
  such as `bad certificate`); every `a2s` then carries `tls`, `client_subject` and `remote`.
  Each connection whose handshake succeeded logs `tls_closed` when it closes (`app_bytes`: the
  application bytes the client sent on it; `closed_by`: `client`, `server` or `shutdown`, for
  connections still open when the server stops). In `tls-untrusted`, `rejects_untrusted_server`
  (MUST) asks that the client send nothing: no message, and no application byte on a completed
  handshake (one never logged as closed counts as unknown, and fails). `refuses_in_handshake`
  (SHOULD) asks that it refuse the handshake itself; a client that checks the certificate only
  after the handshake (.NET does) passes the first and gets a warning from the second.
- `"proxy": {"kind": "connect" | "socks5", "auth": {"user": …, "pass": …}}`: an in-process HTTP
  proxy (CONNECT tunnels, and requests in absolute form, which is how clients send `http://`
  through a proxy) or SOCKS5 proxy, which refuses clients without the credentials. It forwards
  to this server only (`host.docker.internal` included) and logs each tunnel (`proxy_tunnel`)
  with the address the server sees it come from; `went_through_proxy` matches every message to
  one. Refusals are logged as `proxy_refused`.

The upstream Go driver (opamp-go, which takes a `tls.Config` and a proxy URL) and all six of our
clients have these features (`clients.toml` lists them), except that Java has no SOCKS5 in
Java mode (`java-core`): the JDK's HTTP client has none. In Rust mode (`java-core-rust`) the core
speaks SOCKS5 itself, and every SDK's Rust mode has them all.

Each of the five WebAssembly SDKs is two clients here, one per mode: `go-core`, `node`,
`python-core`, `java-core` and `dotnet-core` are platform mode, and the same names with `-rust`
are Rust mode, the SDKs' default. Each driver asks for its mode by name (the `-rust` clients set
`OPAMP_<LANGUAGE>_MODE=rust`), so neither depends on what the SDK's default is.

Both are the Go server's: those scenarios don't list `rust` in `servers`, so `--server rust`
skips them (the runner refuses a scenario that asks for both).

## A hostile server

A scenario's `server` can have the Go server send malformed values that look plausible
(`server/hostile.go`). A client must refuse them, report them failed, and carry on
(RULE-CLIENT-12).

- `"hostile": {"settings": [...]}`: connection-settings offers, one at a time. Each is offered in
  every response until the agent answers it (APPLIED or FAILED), or `answer_within_s` (15 s)
  passes; `hold_s` keeps an answered one a while longer. The kinds: `endpoint_space` (whitespace
  in the path), `endpoint_bad_host` (`bad host`, `bad_host!`, ...), `endpoint_no_host`,
  `endpoint_scheme` (`ftp`, `file`, `gopher`), `endpoint_huge` (past the 8000 octets RFC 9110
  asks everyone to support), `header_ctl` (CR LF, LF, CR or NUL in a value), `header_name` (a
  name that isn't a token), and two that are not malformed but absurd: `heartbeat_zero` and
  `heartbeat_huge` (2^64-1 s, or values whose milliseconds overflow 64 bits, a double's integers
  or a 32-bit timer).
- `"hostile": {"packages": [...]}`: packages offered together, then a good one (`zz-good`, last by
  name, which is the order a client works through them). The kinds: `url_scheme`, `url_relative`,
  `url_space`, `url_no_host`, `header_ctl`, `header_name`.
- `"seed"`: picks each kind's variant (which control character, which scheme), and the kind of an
  entry `"any"`. The same scenario offers the same values every run.

Every malformed endpoint or URL that a lenient client could still reach points at a port of its
own, which answers nothing. A connection to it is logged (`hostile_connect`, with the first line
sent), and so is a request for `/packages/h0st1le/` (a relative URL resolved against the server)
or one with a header a malformed value smuggled in (`hostile_request`). `no_request_to_malformed`
fails on either.

## Redirected downloads

A package in a scenario's `server.packages` can have a `"redirect"`: its download is answered with
a chain of redirects (`server/redirect.go`), and the file comes at the chain's end. The file is
offered at `/packages/<name>/<version>` on its own origin (`"from"`: the main listener, or `tls`),
and hop n of the chain is at `/redirect/<name>/<version>/<n>`. The statuses go 302, 301, 307, 308,
303 in turn. What each hop in `"hops"` points at:

- followed: `same` (an absolute URL on the same listener), `relative` (a path-absolute Location),
  `relative_dots` (a relative path with `../`), `main`, `other` (plain HTTP on a port of its own:
  another origin, which serves anyone) and `tls` (HTTPS with a CA made at start, handed to the
  driver as `DRIVER_CA_FILE`);
- refused, and the last hop: `trap` (a sixth redirect), `downgrade` (from `tls` to http),
  `bad_host` (a space in the host), `bad_scheme` (`ftp`) and `userinfo`. Each points at a port of
  its own that answers nothing; a connection to it is logged (`redirect_trap_connect`, with the
  first line sent).

The server refuses a chain the checks couldn't judge (a refused hop that isn't the last, a
`downgrade` not from `tls`, a hop past the fifth that isn't `trap`). Every request is a
`package_download` with its package, its hop, the listener it came to (`listener`) and the file's
(`file_listener`), the `Authorization` it carried, and the status and `Location` it was answered.
The file's own origin serves only requests with the offer's `Authorization`.

- `redirects_followed`: every package behind allowed redirects is followed hop by hop, installed
  with the file at the chain's end (its SHA-256), and reported INSTALLED.
- `refused_redirects_failed`: every package whose chain ends in a redirect to refuse gets that
  redirect, never requests its target, never reaches the application, and is reported
  INSTALL_FAILED without a second attempt.
- `redirect_headers_same_origin`: no request at another origin carries the offer's headers; every
  one at the file's origin does, back there after another origin too.
- `redirect_hops_bounded`: nothing past the 5th redirect is requested, and a chain of 5 or more is
  followed as far as the 5th.

All four apply RULE-CLIENT-17, and need the `core` feature: every client of ours reports each
download's response and the core decides; the upstream clients follow their HTTP library's
redirects. The Rust server serves no packages, so these scenarios run on the Go server only.

## Requests exactly as given

The charter says a client sends the request the core describes, and nothing else. The Go server
keeps each request head as sent (plain-text connections only), so every `a2s` over HTTP,
`ws_open`, `request_rejected` and `package_download` carries `headers`: each name as spelled, in
order, with its value.

- `headers_as_given`: only the headers the core names (for a download, those the offer gives),
  each once, plus what HTTP needs to carry the request: `Host`, and `Content-Length` or
  `Transfer-Encoding` for a POST. An upgrade may add the handshake RFC 6455 (section 4.1) requires:
  `Upgrade`, `Connection`, `Sec-WebSocket-Key`, `Sec-WebSocket-Version`. Not `Origin`,
  `Sec-WebSocket-Protocol` or `Sec-WebSocket-Extensions`: they are optional, and the core asks for
  none of them. A download is a GET with no body, so only `Host`.
- `header_case_kept` (SHOULD): the core's spelling, `OpAMP-Instance-UID`. Header names are
  case-insensitive, so no conforming server can tell; hence SHOULD.
- `no_cookies`: with `"cookie": true` the server sets a cookie on every response, and no request
  may send it back. Some cookie jars (aiohttp's) ignore cookies from an IP address, so these
  scenarios set `"host": "localhost"`: the runner gives the driver `localhost`, and the server also
  listens on `::1`. A client in a container reaches `host.docker.internal`, a name already.
- `no_decompression`: a package with `"gzip": true` is served gzipped with
  `Content-Encoding: gzip`, and the offer's content hash is that of the bytes sent. A client that
  decompresses hands the core other bytes, and the package fails.

No scenario runs these checks yet. The scenarios that do (`requests-as-given` and
`download-as-sent`) come with the clients' fixes, since most HTTP stacks add headers, keep cookies
or decompress today.

## The Rust server

`--server rust` runs the scenarios whose `servers` list includes `rust`: those without faults,
and the ones written for it (`server-*`). A check says what it judges (`subject`). A check of
the server (`server_*`) must pass with every client, upstream or ours. A check of an upstream
client is compared with its own baseline, `baseline-rust-server.toml`. A check listed there as
`flaky` races inside the client (the Python client's stop sometimes reuses a sequence number),
so either result is expected.

Found this way: the .NET client over plain HTTP ignores a connection-settings offer in the reply
to its first message. The Go server repeats its offer in every reply and hides this. A server
that offers once per connection never gets a heartbeat interval to .NET agents over HTTP. The
Rust server offers once per connection, because every offer makes a supervisor reconnect.

## Scenarios

| Scenario | Transports | What happens |
|---|---|---|
| `basic` | http, ws | connect, report, poll, stop |
| `heartbeat-offer` | http, ws | the server offers a 3 s heartbeat; the client's own interval is 8 s |
| `status-change` | http, ws | the agent's description changes twice |
| `server-restart` | http, ws | the server forgets the agent 3 s in |
| `report-full-state` | http, ws | the server sets ReportFullState once |
| `description-too-large` | http, ws | the description is over the client's `max_send_bytes` (1 KiB), so the server never gets it and asks for the full state in every answer: about one message per heartbeat, not one per round trip (`asks_wait_for_the_heartbeat`, RULE-CLIENT-21; clients with `max_send_bytes`) |
| `assign-uid` | http, ws | the server assigns a UUID v7 |
| `assign-uid-not-uuid` | http, ws | the server assigns 16 bytes that aren't an RFC 4122 UUID |
| `request-uid` | http, ws | the agent asks for a UID (RequestInstanceUid); the server assigns one per message asking (Go and Rust servers) |
| `request-uid-ignored` | http, ws | the agent asks for a UID; the server ignores the flag |
| `request-uid-restart` | http, ws | the agent asks for a UID, is assigned one, and is restarted: it goes on with it without asking |
| `close-frame` | ws | the server closes with a Close frame |
| `tcp-drop` | ws | the server drops the TCP connection |
| `first-reply-lost` | http, ws | the server takes the first two messages and drops the connection before answering each: every message is number 0 until one is answered, then 1, 2, ... (`number_zero_until_answered`) |
| `accept-then-drop` | ws | for 32 s the server accepts every upgrade and drops the TCP connection before answering: the client backs off (`reconnect_backs_off`), no reconnect storm |
| `throttle-429` | http | 429 with Retry-After: 4 for five seconds |
| `unavailable` | http, ws | UNAVAILABLE (WebSocket) or 503 (HTTP), retry after 4 s |
| `reject-413` | http | the first message is rejected with 413 |
| `reject-bad-request` | http, ws | the first message is answered with BAD_REQUEST |
| `remote-config` | http, ws | the server offers a remote config |
| `remote-config-resent` | http, ws | the server puts its remote config in every response: the application gets it once (`remote_config_lifecycle`) |
| `remote-config-restart` | http, ws | the agent restarts after applying the remote config: it gets the config back at start, and the server has no need to send it again (`remote_config_lifecycle`) |
| `credential-offer` | http, ws | the server takes a shared bootstrap token, offers the agent its own and then accepts only that; the agent restarts halfway and must still get in (`connection_settings`) |
| `credential-offer-broken` | http, ws | the server offers a token it will not accept: the agent reports FAILED and carries on with the bootstrap token (`connection_settings`) |
| `own-metrics-restart` | http, ws | the server says where the agent sends its own metrics; the agent restarts after applying that, and gets it back at start (`telemetry_settings`) |
| `available-components` | http, ws | the agent reports its components as a hash; the server asks for them in full once (`available_components`) |
| `custom-messages` | http, ws | a custom message for a capability the agent declared, which it answers, and one for a capability it never declared, which the application must not see (`custom_messages`) |
| `uid-assigned-restart` | http, ws | the server assigns a new instance UID; after a restart with the UID it chose, the agent goes on with the assigned one (`instance_uid_kept`) |
| `uid-made-up-restart` | http, ws | the application chooses no instance UID: the client makes one up, and keeps it across a restart (`instance_uid_kept`) |
| `server-capabilities` | http, ws | the server says it doesn't accept the effective config, then asks for the full state: the full report leaves the effective config out |
| `server-rollout` | http, ws | Rust server only: every agent should run a config; it is offered once and recorded applied |
| `server-unavailable` | http, ws | Rust server only: overloaded for 3 s: 503 with Retry-After over HTTP, UNAVAILABLE with retry_info over WebSocket; the client waits before trying again and reports in full after |
| `server-zero-uid` | http, ws | Rust server only: the agent starts with an all-zero UID and is assigned a UUID v7 |
| `tls-basic` | http, ws | TLS; the client trusts the CA it is given (`tls`) |
| `tls-untrusted` | http, ws | TLS with a certificate the given CA did not sign: the client must send it nothing, and should refuse the handshake (`tls`) |
| `mtls-configured` | http, ws | TLS requiring a client certificate; the client presents the one it is given (`tls`, `mtls_configured`) |
| `proxy-connect` | http, ws | through an HTTP proxy with credentials (`proxy_configured`) |
| `proxy-connect-tls` | http, ws | TLS through an HTTP proxy: CONNECT tunnels on both transports (`tls`, `proxy_configured`) |
| `proxy-socks5` | http, ws | through a SOCKS5 proxy with a username and password (`proxy_configured`, `proxy_socks5`) |
| `redirect` | http, ws | 307 for the first 15 s: the client doesn't follow, and is back by 31 s at the latest (its backoff); the run is 40 s (`core`) |
| `hostile-settings` | http, ws | malformed endpoints offered: whitespace, a bad host, no host, another scheme (`connection_settings`) |
| `hostile-headers` | http, ws | malformed headers offered (a control character, a bad name), a huge endpoint, one more (`connection_settings`) |
| `hostile-heartbeat` | http, ws | heartbeat intervals of 0 and two huge ones |
| `packages` | http, ws | three packages offered, one with the wrong content hash: the good ones are downloaded and installed, the bad one refused; then one is upgraded and another removed (`packages`) |
| `hostile-packages` | http, ws | packages with another scheme, a relative URL, a control character in a header; then a good one (`packages`) |
| `hostile-packages-mix` | http, ws | packages with whitespace, no host, a bad header name; then a good one (`packages`) |
| `packages-redirects` | http, ws | package files behind redirects to follow: same origin, relative, another origin and back, http to https, a chain of 5, an https file (`packages`, `core`) |
| `packages-redirects-refused` | http, ws | package files behind redirects to refuse: a sixth, https to http, a bad host, `ftp`, userinfo; then a good one (`packages`, `core`) |
| `stop-mid-connect` | http, ws | every request and upgrade held; stop while waiting: bounded, and nothing left open (`core`) |
| `stop-mid-download` | http, ws | half a package file sent, then held; stop while downloading (`packages`, `core`) |
| `restart-mid-download` | http, ws | stop mid-download, and a client started on the same state file at once while the first lingers: it installs the whole file, never the first one's (`packages`, `core`) |
| `crash-mid-save` | http, ws | the driver killed three times in its first 2.5 s, while it saves an assigned UID and a remote config, and restarted on what a crash would have left (`file_journal`) |
| `crash-mid-download` | http, ws | the driver killed mid-download (half the file written) and restarted on what a crash would have left: it downloads again into a file of its own (`packages`, `file_journal`) |
| `callback-throws` | http, ws | the callback throws on the first remote config: the client stops, says so once with the error, says no goodbye and sends nothing more (`callback_failure`) |

## Crashes

Every client of ours records in its core tap the file operations it carries out for its core (a
save's temporary file made, written, flushed, renamed over the state file, the directory flushed; a
download's file made, written, flushed, deleted), after each succeeded (the tap checker's file records).
`core_tap` replays them on a disk that can crash at any point, losing what wasn't flushed (the data
written to a file since its last flush, all or part, a write cut short; any prefix of a directory's
changes since its last flush), as LazyFS's `clear-cache` does without FUSE, and as ext4 can after a
power loss. On every run, at every point, the state file must be whole (what it held at start, or
the whole data of one of the core's saves), never older than the last save the client answered
`stored` (so a client that says stored before the save is on the disk fails at the step that says
it, crash or no crash), and every file written must be one the client made new.

A scenario can also crash the driver for real (`"crash": {"runs": [0], "from_s": 0.1, "to_s": 2.5}`):
the runner kills run 0 (SIGKILL, or `docker kill`) at a moment picked from the scenario's `seed`,
`from_s` to `to_s` after the driver said it started; `opamp-tap crash` then picks, from the same
seed, what a crash at that moment could have left of the state file's directory and writes it
there, and the next run starts on it, beside whatever the killed process left in the downloads
directory. `crash_state_consistent` (MUST, RULE-SHELL-09) asks that what was left be whole and no
older than the last save answered stored, and that the next run start from exactly it (its core's
`Config.saved_state`). `--crash-seed N` replays a run with another seed; each result names the seed
it ran with. With `"overlap": true` the next run starts as soon as the one before says it stopped,
while it lingers: a restart in process on the same state file (`restart-mid-download`).

The checker's mutants (in the tap checker's own tests): no file sync (torn), no directory sync
(stored before it is durable), a write in place (torn, and a file not made new), an older save
landing last.

`--crash-seeds N` runs each crash scenario on N seeds, from `--crash-seed` or the scenario's. With
`--lazyfs DIR` the crashes are real: every run's state file is on a LazyFS mount of its own, a crash
kills the driver and has LazyFS drop what wasn't flushed (or tear a write of the state's temporary
file and die), the file system is mounted again, and `opamp-tap judge` says what the state file
holds for `crash_state_consistent`. It needs FUSE: `conformance/lazyfs/run.sh` runs it in a
container, nightly.

## Differential runs of the SDKs

Every SDK runs the same core, so on one scenario what each tells its core, and what the core
decides, should differ only in timing. `--differential` (or `conformance/differential/shells.py`
over an existing `conformance/out/`, as the `Conformance (differential)` CI job does with every
client's uploaded taps) compares each client's taps of each scenario and transport with the Rust
client's, or go-core's where the Rust client isn't built (`opamp-tap diff`). Each run is reduced to what doesn't depend on timing, per category
(the application's calls, the notifications, the saves and their answers, the messages sent and
received, the connections' lifecycle, the downloads), dropping the clock, effect ids, instance UIDs
(named by order of appearance), hosts and ports, identifying attributes' values, errors' texts
(their class stays), cancels, and a block of up to four lines repeated back to back (one more
heartbeat), but not an answer repeated alone (a save answered twice). Rust mode's wire categories
are its core's own streams and aren't compared. A difference fails `--strict` unless
`conformance/differential/shells.toml` allows it, with its reason; the report shows the scenario,
the two clients and the first lines where they part, with context. So does a run the Rust client
has none of, which was compared with nothing: when a change runs one SDK's family alone, the CI job
runs the Rust client itself first.
