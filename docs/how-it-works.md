# How the clients work: one core, a client per language

Every client here runs the same compiled **core** for everything OpAMP, and is itself a thin
**client** around it, written in its language. A fix or a new behaviour lands once, in the core,
and every language has it; the client only carries out what the core asks and gives the
application an API in its language's idiom. This page is how the two meet, and what a client may
and may not do.

## The core and the client

The core is a compiled component: a WebAssembly module (`opamp_core.wasm`) that Go, Node.js and
Java run, and the same core built as a native library that Python and .NET load, held to the
module byte for byte. Inside it are two parts, both written once:

- the **protocol**: what to send and when, sequence numbers, full reports, the instance UID,
  throttling, heartbeats, stop;
- the **wire and the flows**: encoding and decoding, WebSocket framing, every HTTP request
  (URL, method, headers including `OpAMP-Instance-UID`, content type), reading every response
  (status codes, Retry-After in both forms, size limits), which connection an event belongs to,
  what is saved across restarts, and the feature flows: the remote config lifecycle
  ([remote-config.md](remote-config.md)), connection settings offers
  ([connection-settings.md](connection-settings.md), [telemetry-settings.md](telemetry-settings.md)),
  packages ([packages.md](packages.md)), available components
  ([available-components.md](available-components.md)), custom messages
  ([custom-messages.md](custom-messages.md)), the instance UID ([instance-uid.md](instance-uid.md)).

The core does no I/O, keeps no clock and makes no random choices of its own. A client calls
`step(now, input)` and carries out the effects it returns: make this HTTP request, open this
WebSocket connection, send these bytes on it, close it, save this, tell the application. When a
response comes, a connection opens or drops, the application changes something, or the returned
wake-up time comes, the client says so in the next step, naming the request or connection by the
id the effect gave it. It builds no request and reads no response itself, so the same inputs give
the same decisions and the same messages in every language.

Beside the client, each SDK integration runs a second compiled module, the **SDK settings layer**
(`opamp_sdk_settings.wasm`): it decides what an OpenTelemetry SDK does with the settings a server
sends, and the integration carries that out ([sdk-settings.md](sdk-settings.md)).

## The interface

`proto/opamp/core/v1/core.proto` (put in place by `scripts/fetch_core.py fetch`) is the contract: a
`Config` to start, then `Step{now, Input}` in and `Reply{Output{effects, wake_at}}` out. It reuses
the spec's OpAMP messages. Each language generates its types with its usual protobuf tooling.

| Effect (core → client) | Result (client → core) |
|---|---|
| `http_request{id, url, headers, body, timeout}`: POST `body` to `url` with exactly `headers`, reading at most `max_response_bytes` | `http_response{id, status, headers, body}` (any status) or `http_failed{id, detail}` |
| `ws_open{id, url, headers}` / `ws_send{id, bytes}` / `ws_close{id}` | `ws_opened` / `ws_failed{status, headers}` / `ws_message` (binary only) / `ws_closed{code}` / `ws_lost` |
| `store{id, data}`: persistent state, replaced atomically, private to the agent ([state-file.md](state-file.md)) | `stored` / `store_failed{detail}` |
| `download{id, url, headers, max_bytes}`: to a local file, hashed as it goes, following no redirect | `download_response{id, status, sha256, location}` (whatever the status) / `download_failed{detail}` |
| `cancel{id}`: give up on that effect at once | nothing more about it |
| `notify{...}`: a remote config to apply, a package to install, ... | the application's own calls |
| `wake_at` | `timer` |

An input with an id the core has moved on from (an old request, a connection it closed) is
ignored, so a client needs no bookkeeping of its own for races between a close and a late message.

The module exports four functions:

| Export | |
|---|---|
| `alloc(len) -> ptr` | a buffer for the host to write an input into |
| `init(ptr, len) -> u64` | start from an encoded `Config`; returns an encoded `Reply` as `(ptr << 32) \| len` |
| `step(ptr, len) -> u64` | run an encoded `Step`; same reply |
| `free(ptr, len)` | release a reply after reading it |

Calls take ownership of their input buffer. A `Reply` with an `error` means the client sent
something unusable; it is never a protocol condition. A trap is a bug in the core: the client
stops rather than trust the instance again. The native library has the same `init` and `step`,
with a handle per session in place of an instance; a failure inside it is caught at the boundary,
and the session then refuses every call, as a trapped instance would.

`core.proto` grows by adding fields, never renumbering them. A client built against an older one
keeps working against a newer core: new inputs are optional, and a new effect appears only for a
feature the client turns on in its config. A client that meets an effect it doesn't know stops
with an error (reported by `stop`) rather than drop what the core asked for.

## Effects end

Every effect the core waits on has a deadline, kept in `wake_at`: a request `request_timeout_ms`
(30 s), a WebSocket `connect_timeout_ms` (30 s) to open and `stop_timeout_ms` (5 s) to close, a
download `download_timeout_ms` (10 minutes). A save has none: one cancelled while it runs could
land after a newer one, so it is only ever cancelled at stop, after which nothing is saved.

- **Past its deadline an effect has failed**, exactly as if the client had reported a failure
  ("no response within 30000 ms"), and the core says `cancel{id}`: the client aborts it (a
  request, a download and its file, a connection being opened, a connection closing) and reports
  nothing more. A result that arrives anyway names an id the core is done with: it changes nothing.
- **Stop is bounded.** At stop the core cancels what the goodbye has no use for, sends the goodbye
  once the request in flight is answered, and waits for it, for the closing handshake and for a
  save under way, until `stop_timeout_ms`. Then it cancels what is left and says it has stopped
  (`Output.stopped`). Nothing it started is running any more, and it starts nothing new after the
  stop. After `ws_close` the client says when the connection has ended (`ws_closed` or `ws_lost`),
  so the core knows the handshake is over.
- **A connection being opened with settings that just changed** (a credential switch) is
  cancelled, and one opens with the new ones at once.

The client keeps no timer of its own: its only job is to abort what the core cancels.

## What a client does, and doesn't

A client carries out the core's effects exactly, feeds back what happened, and gives the
application an API that feels native. It makes no OpAMP decision of its own.

It does:

- run one execution context (a thread, an event loop or a task) that owns the core and feeds it
  inputs one at a time, in order;
- give the clocks: a monotonic `now` for every step, and the wall-clock time a response arrived;
- give random bytes from the platform's CSPRNG, for the instance UID the core makes;
- keep one timer from `wake_at`, replaced after every step;
- HTTP: send exactly the request the effect describes, and report the status, headers and body as
  received, or the failure;
- WebSocket: open with the headers given, pass on binary messages only, enforce the size limit,
  carry out sends and the close in the order given, and report each outcome by id;
- cancel: abort the effect named at once and report nothing more about it;
- storage: write the state atomically, read it back at start, and report the outcome
  ([state-file.md](state-file.md));
- downloads: stream to a file, compute the SHA-256, and report the status and digest whatever the
  status, and for a redirect its `Location` resolved and its origin, by the client's own URL
  library ([packages.md](packages.md#redirects));
- stop: carry out effects until the core says it has stopped, then release everything.

It doesn't:

- decide anything about OpAMP: when to send, retry or back off; what a message carries; what a
  status code, header or message means; which headers to send; reorder, drop or retry anything on
  its own;
- keep timeouts of its own: how long anything may take, stop included, is the core's;
- choose the transport, make up an instance UID, or decide what a download's status means;
- change a request or response in transit: **no extra headers** beyond those the HTTP stack must
  add (`Host`, `Content-Length`, the WebSocket handshake), **no following redirects** (a 3xx goes
  to the core as it is, a download's too), **no automatic compression or decompression, no
  cookies**;
- interpret a response body in any way, including checking its content type.

The conformance suite holds every client to this from outside (the checks under
`conformance/runner`), on both transports.

### Known deviations

Both are platform mode's (below): in Rust mode the core reads the response itself.

- **Decompression in Node.** `fetch` decompresses whatever it is sent, and has no way to stop.
  The core asks for `Accept-Encoding: identity`, so a server sends nothing to decompress; from one
  that compresses anyway, the Node client reads the decompressed body, and the size limit applies
  to that rather than to what was sent.
- **A caller's own HTTP client.** .NET's `HttpClient` has no per-request setting for redirects or
  decompression, so one an application passes in must be built with `AllowAutoRedirect = false`.
  Go copies the caller's client and turns redirects off; Java refuses one that follows them.

## Rust mode and platform mode

By default (**Rust mode**) the core also speaks HTTP/1.1, chunked bodies, WebSocket, CONNECT and
SOCKS5 itself: the client only opens TCP connections, starts TLS with the operator's settings,
resolves names for a SOCKS5 proxy told to, answers which proxy to use (by one rule for every
language, `tests/shell-tables`), moves bytes with backpressure, runs the timer and writes files.
Every byte on the wire is then the same in every language. In **platform mode** the language's
own HTTP and WebSocket libraries carry out the core's requests and connections, for applications
that bring their own client or need that stack. [sdks.md](sdks.md#rust-mode-and-platform-mode)
says how to choose; `spec/shell-contract.md` (put in place by `scripts/fetch_core.py fetch`) has
the precise rules of the byte streams.

## Order

The core takes one input at a time; what a client's threads or tasks decide is the order of
inputs from different sources. The core needs none between sources (the application's calls, a
connection's messages, transport results, timers), only within each: each caller's calls as it
made them, each connection's events as they came, each once, and nothing for a connection after
it ended.

One order across sources every client keeps, for the application: what the application asks from
inside the callback reaches the core before anything the client takes in next. It is the
application's answer to that step ([sdk-contract.md](sdk-contract.md)). Go's loop takes its
mailbox of calls as far as the callback's last one as the step ends. Java and .NET have one queue
for everything the client's thread or loop takes; the calls a callback asked for (with any made
before them) are fed as its step ends, ahead of their turns. Node and Python feed a call made on
the loop from the drain that is running the callback, before the event loop can deliver anything.

Saves never overlap: a save runs beside the loop, one at a time, and a client started on the same
state file waits for one still writing.

## Why effects, not host calls

The core could call the host instead (WASI or Component Model imports). It doesn't:

- a call that blocks inside the module fights every async runtime (Node's event loop, asyncio, Go
  channels, Java executors); effects are plain data, and each client runs them its own way;
- with effects the core is a function of its inputs, so it can be simulated, replayed and fuzzed
  whole;
- the Component Model isn't supported by wazero or Chicory; plain modules with bytes in and bytes
  out run everywhere.

## Runtimes

| Language | Runs the core with | Native dependency |
|---|---|---|
| Go | [wazero](https://github.com/tetratelabs/wazero) | none |
| Node.js / TypeScript | built-in `WebAssembly` | none |
| Java | [Chicory](https://github.com/dylibso/chicory), the module compiled to JVM classes at build time | none |
| Python | the native library, through `ctypes` | a core wheel per platform, `pydantic-opamp-core` (`python/core`) |
| .NET | the native library, through `LibraryImport` | a library per platform, in `Pydantic.OpAmp.Core.Native` (`runtimes/<rid>/native`) |

Python and .NET would need a native WebAssembly runtime anyway, so they load the core natively:
smaller, with no JIT, and on every platform their packages build for, Alpine's musl included.
The library is the only compiled part, in a package of its own that holds it and nothing else,
which the client's package depends on. Their tests run the module beside it, as the oracle every
reply is compared with byte for byte (seeded runs of configs and inputs, plausible and not:
`python/tests/test_native.py`, .NET's `NativeCoreTests`), and to run the hostile core.

## Where the core comes from

The core is released compiled, pinned by hash: `opamp-core.lock.json` names a core release and
the sha256 of every file in it, and `fetch_core.py fetch` (in `scripts/`) downloads it, checks every byte
against the lock, and puts each file where the clients load it: the module copies Go, Node.js and
Java embed, the native library in `target/release` (and every platform's under `dist/native` with
`--all-platforms`, for packaging), and the files the clients' tests read (`proto/`,
`spec/shell-contract.md`, `tests/shell-tables`). `fetch_core.py check` fails unless the tree
holds exactly what the lock pins. The generated types are checked in beside each client; `scripts/gen_core.py`
regenerates them from `core.proto`.

Each release also carries a hostile stand-in for the core (`testdata/core/opamp_core_adversary.wasm`),
which every SDK's harness test runs behind the same interface: a client must survive whatever the
core asks, and report a core that misbehaves rather than trust it.

## Cost

Measured on an M-series Mac with wazero 1.12:

| | |
|---|---|
| one `step` | about 0.6 µs, 12 allocations |
| starting a client (instantiating the core) | about 150 µs, 2.5 MB (the module's memory) |
| compiling the module | once per process, well under a second |

The protocol takes a handful of steps a minute, so none of this shows.
