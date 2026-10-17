# Connection-settings offers

How the core handles a server's offer of new OpAMP connection settings (the spec's
"OpAMP Connection Setting Offer Flow"), and what each client does for it.

## Scope

This version handles offers for the **OpAMP connection** that change its **endpoint** and
**headers** (an `Authorization` header, say). That is the Registration On First Use case: agents
installed with one shared credential each get their own after the first connection.

Refused with FAILED, never silently ignored, until each is designed:

- Offers with a client certificate, TLS settings or a proxy. These need certificate material
  passed through every client, which is a separate change.
- An endpoint of the other transport (a `wss://` offer to an HTTP client, or the reverse).
- Offers for own telemetry and other connections: the core hands these to the application
  unchanged (they configure the agent, not the OpAMP client).

A heartbeat-only offer (no endpoint, headers, certificate, TLS or proxy) changes no connection:
the core applies the interval, and reports APPLIED without reconnecting.

An offer names a new endpoint (it must be non-empty, OPAMP-CONNSET-07) and, usually, new headers.
Offered headers replace the ones in use; an offer without headers keeps them. An offer is named
by its hash (or, without one, by a fingerprint of its settings): the same offer again, whether in
use, being tried or already answered, changes nothing.

## The flow

1. **Offer.** A `ServerToAgent` carries `connection_settings.opamp` with a hash different from the
   settings in use.
2. **Check.** The core refuses (FAILED, with the reason) an offer it can't apply: out of scope
   above, an empty or malformed endpoint (OPAMP-CONNSET-07) or malformed headers ([What the core
   refuses](#what-the-core-refuses)), or no persistent storage (see below). An offer that comes
   while the core is busy with another (a candidate kept, or another offer being saved) is not
   refused: it waits ([An offer while busy](#an-offer-while-busy)).
3. **Save the candidate.** The core writes the state with the offer as *candidate* (a `Store`
   effect) and waits for the client to confirm it was written. If it wasn't, the offer is not
   FAILED (a disk error can pass, and an offer answered is not taken again): it waits, and its
   save is tried again later. A newer offer refused meanwhile (a candidate was being saved) stays
   the answer the core remembers: it isn't taken if it comes again, and a restart reports it.
4. **Switch.** The core reports APPLYING and reconnects: over WebSocket, the
   goodbye (`agent_disconnect`), a Close, and a new connection with the candidate's endpoint and
   headers; over HTTP, the next request goes to the candidate.
5. **Verify.** The candidate is proven by the server answering with it: over HTTP, a response
   that is an OpAMP message; over WebSocket, the server's first message on the connection. An
   HTTP 200 or a WebSocket upgrade alone is not proof. Something on the way that knows nothing of
   the credentials gives those too: a load balancer's default page, a wrong path, a captive
   portal. Taken as proof, they would promote settings the server never accepted, and the client
   would forget the ones it does. The candidate is disproven by a refused WebSocket upgrade, an
   HTTP error, a failed request, a reply that isn't an OpAMP message, a WebSocket connection that
   ends before the server answers on it, or no answer within the verification timeout (default
   60 s). The timeout counts from the first attempt with it, so a long backoff doesn't use it up.
   Throttling (429, 503, UNAVAILABLE) proves nothing either way, and neither does a rejected
   message (413, BAD_REQUEST): the server took the connection, and the full report owed after a
   switch is the message most likely to be too large. The wait runs on. An attempt still on its
   way when the time runs out can succeed after all: the candidate is then proven, and the client
   connects with it from then on.
6. **Promote**, if proven: the candidate becomes the *valid* settings and the old ones are
   forgotten (a `Store`), then APPLIED.
7. **Revert**, if disproven: reconnect with the valid settings, and drop the candidate (a `Store`)
   only once they have **worked again**; then FAILED with the reason. If the valid settings are
   refused instead, the server has already switched to the candidate, so the candidate is tried
   again.

   This departs from the spec's step 9 ("if step 6 fails the Client deletes the new settings"),
   and must. A candidate can connect, which lets the server drop the old settings (step 7). The
   connection can then drop before the client saves the promotion, and the retry can fail for a
   network reason. Deleting the candidate at that point locks the agent out for good.

**After a crash**, a saved candidate is verified first (steps 5 to 7), as the spec requires, so an
agent that died mid-switch neither keeps unproven settings nor loses proven ones.

### An offer while busy

FAILED says an offer can never be taken; "not now" is not FAILED (issue #213). An answered offer
is not taken again, so a FAILED for "busy" or for a failed save would leave a server that offers
the same settings again no way to move the client on. Instead, as the remote-config lifecycle
keeps the newest config:

- An offer that comes while a candidate is kept or another offer is being saved **waits**,
  answered nothing (the status stays about the offer the core is busy with). Only the newest
  waits; it is saved with the rest of the state (SavedPb tag 15), so a restart keeps it.
- **A later offer replaces it**, whatever becomes of that offer: taken, refused, left waiting,
  ignored (in use or answered), or one with nothing for the OpAMP connection. Taken later, the
  older offer could be promoted after a newer one, or its APPLYING replace the APPLIED the server
  waits for (`tla/composition`, `KeysClient.tla`, where the key store drops the key the client
  trusts or waits for good).
- It is **taken** once the core is free (the candidate promoted or dropped, or the offer's own
  save failed), no save is on its way, no failed save is owed (the core's own retry: an offer
  whose save failed is taken again once a save issued since lands; the flow keeps no retry of
  its own), and the server has heard the status as it
  is: an answer without an error came to a message that carried it (`Kernel::status_heard`),
  with the OpAMP part's status unchanged since (`Core::status_heard`). Taken sooner, its APPLYING
  could replace the APPLIED of the candidate just promoted before the server heard it. Offered
  again while the core is free, it is taken at once.
- An offer whose telemetry part is taken in at once is the latest offer of the combined status
  ([telemetry-settings.md](telemetry-settings.md)): its OpAMP part, waiting, counts as APPLYING.
  So is one that replaces an offer waiting that was the latest, or that waits while the latest
  offer's own save fails: the offer waiting is the latest, or not at all. Otherwise the status
  stays about the offer before it until it is taken, and the offer waiting holds nothing of it up.

`tla/connection-settings` has the rules (`TakeWaiting`, `Heard`, `ReceiveOfferAgain`), and
`tla/telemetry-settings` the combined status (`RetryTake`).

## What the core refuses

A server can send anything, and every client hands what the core gives it to an HTTP stack of
its own. Those stacks read URLs and headers differently (WHATWG parsers escape what RFC 3986
parsers reject, Java's `URI` finds no host in `a_b`, .NET refuses non-ASCII header values, Java
refuses to set `Host` or `Connection`), and a client that repairs a value (escaping a space,
resolving a relative URL) sends the agent's credentials somewhere the server never named. So the
core checks every endpoint and header before any client sees it, and a value that fails is refused whole: the offer is FAILED at
once, nothing is saved, and no request or connection is ever made with it (RULE-CLIENT-12). The
reason names what is wrong ("the endpoint has no host", "the offered headers: the value of header
1 (Authorization) holds a control or non-ASCII character, or a space at either end") and never
echoes a header's value.

An **endpoint** must be:

- an absolute URL of a scheme the client speaks (`ws`/`wss` over WebSocket, `http`/`https` over
  HTTP, in any case), at most **8000 octets** (what RFC 9110 section 4.1 asks everyone to
  support, and what RULE-CLIENT-12 names);
- printable ASCII only: no whitespace, control character, `\` or non-ASCII character;
- with a **host**: a DNS name of letters, digits and hyphens (labels of 1 to 63, not starting or
  ending with a hyphen, at most 253 in all) whose last label starts with a letter, a dotted-quad
  IPv4 address, or an IPv6 address in brackets (no zone). A last label starting with a digit is
  an IPv4 address to WHATWG parsers (in decimal, octal or hex) and no host at all to Java, so
  only a dotted quad passes;
- with an optional **port** from 1 to 65535 (no leading zeros, not empty);
- **without userinfo**: `user:password@host` is a credential in a URL, which the core would then
  hand to every client and log, and which stacks treat differently (some send it as Basic auth,
  some drop it). An offer carries credentials in its headers;
- with a path and query of the characters RFC 3986 allows there (%-escapes complete), no `'` in
  the query (WHATWG parsers escape it), no `.` or `..` segment (escaped or not), and no fragment.

**Headers** (up to **64** of them) must each have:

- a name that is an RFC 9110 token, given once (case-insensitively: stacks disagree whether a
  second one replaces or adds);
- a value of printable ASCII, spaces and tabs, with no space or tab at either end (a receiver
  strips them), so never CR, LF or NUL: nothing can be injected;
- name and value together at most **8000 octets**;
- a name that isn't the HTTP stack's own: `Connection`, `Content-Length`, `Expect`, `Host`,
  `Keep-Alive`, `Proxy-Connection`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade` and
  `Sec-WebSocket-*`. These carry the request or open the WebSocket; Java's `HttpClient` throws
  on several, and an offered one would change how the request is framed (a client adds no
  headers beyond those the stack must: [how-it-works.md](how-it-works.md#what-a-client-does-and-doesnt)). The core's own headers (`Content-Type`,
  `OpAMP-Instance-UID`, `User-Agent`, `Accept-Encoding`) may be offered: the core's replace them.

The same checks apply to own-telemetry endpoints (`http`/`https` only) and headers, and to the
headers of other connections and of proxies ([telemetry-settings.md](telemetry-settings.md)); an
other connection's endpoint and a proxy's URL ("a URL, host:port or some other destination
specifier") need only hold no control character and be at most 8000 octets. Package downloads
are checked the same way ([packages.md](packages.md)).

**Heartbeat intervals.** The core takes any interval the server offers, but at most a day: 2^64 - 1 seconds is a day.
So no deadline the core hands a client is more than a day away, which Node's 32-bit `setTimeout`
and .NET's `TimeSpan` can hold, and a server that meant "rarely" still hears from the agent. An
offer of 0 turns heartbeats off over WebSocket and keeps the configured polling interval over
HTTP, as before. The interval is taken from any offer, including one otherwise FAILED.

## Rules that hold

- **Never locked out.** The saved state always holds settings the server accepts: the valid ones,
  or the candidate once the server has switched to it and dropped the old ones. The client forgets
  settings only after proving their replacement.
- **Nothing unproven is kept.** Settings become valid only after the server has answered with
  them.
- **Statuses tell the truth.** APPLIED only once the new settings are saved as valid; FAILED only
  once the candidate is dropped.
- **Every candidate is resolved.** With a finite number of faults and refusals the client can see,
  a saved candidate is promoted or dropped, and the client ends up connected.

## Without persistent storage

A client with nowhere to save state refuses every offer that changes the endpoint or headers,
and doesn't declare that it accepts connection settings, so a server doesn't keep offering it keys.
The spec only requires this for certificate offers (OPAMP-CONNSET-05), but it is needed for
headers too. The server drops the old credentials once the new ones work (spec step
7), so an agent that restarts without the new ones has nothing the server accepts. This is exactly
the endless 401 loop stock supervisors fall into.

## Saving

The core saves one versioned state: the valid settings, the candidate, the last status, and a
fingerprint of the configured endpoint and headers. The same state holds the other flows', and
the latest offer's hash with the telemetry part's refusal of it, if it refused it
([telemetry-settings.md](telemetry-settings.md)). Each change (accept, promote, drop) is saved
before it takes effect in memory, one at a time; a promotion or a drop that can't be saved waits,
and nothing else happens meanwhile, until a save holding it lands. The core tries a save that
fails again itself, whichever flow's change it held, after a backoff (5 s, doubling to a minute),
until one lands; a change meanwhile is saved at once. Clients store the state as it is and pass
it back at start ([state-file.md](state-file.md)); a state the core can't read (garbage, another
version, another configuration) is ignored.

A save that lands but is reported failed is safe: the client answers the offer FAILED, and a
crash before its next save brings the candidate back from disk, APPLYING, to be tried. What each
status said was true when it said it, and nothing locks the agent out.

The format is kept readable across upgrades: a state this build can't read, or a fingerprint that
changes, would lose the offered credentials after an upgrade, so every format a release has
written reads back in later ones.

## Who does what

| | Does |
|---|---|
| The core | the flow above; the endpoint and headers in use; carries `connection_settings_status`; reconnecting (goodbye, close, connect now); the heartbeat interval; the saved state (versioned, restored from the config at start); discarding saved settings when the operator changes the configured endpoint or headers |
| The client | reads the saved state before starting and passes it in the config; carries out `store` (write atomically, keep it private: a file readable only by the agent, or the platform's secret store) and reports `stored` or `store_failed` |

## Configuration

- `persistence`: whether the client can save state. Without it, credential offers are refused.
- `accept_connection_settings`: declares `AcceptsOpAMPConnectionSettings` and
  `ReportsConnectionSettingsStatus`. Off by default: a server offers settings only to an agent
  that asks.
- `verify_timeout_ms`: how long a candidate has to prove itself (default 60 s).

Each client gives these its own way, and keeps the state in a file it writes atomically (a new
file, synced, renamed over the old) for the agent's user only ([Secrets at rest](#secrets-at-rest)):

| Client | State file | Accept offers | Timeout |
|---|---|---|---|
| Go | `Config.StateFile` | `Config.AcceptConnectionSettings` | `Config.VerifyTimeout` |
| Node | `stateFile` | `acceptConnectionSettings` | `verifyTimeoutMs` |
| Python | `Config.state_file` | `Config.accept_connection_settings` | `Config.verify_timeout_ms` |
| Java | `Config.Builder.stateFile(Path)` | `acceptConnectionSettings(true)` | `verifyTimeout(Duration)` |
| .NET | `OpAmpClientOptions.StateFile` | `AcceptConnectionSettings` | `VerifyTimeout` |

A state file that exists but can't be read stops the client starting: better than quietly
falling back to the configured credentials the server may already have dropped.

### In the SDK integrations

Each language's SDK integration ([sdk-settings.md](sdk-settings.md): Go, Node.js, Python, the
Java agent extension, .NET and Rust) sets `accept_connection_settings` always, and gives the
client its state file in the state directory (`<state dir>/opamp-state`). So whether an agent
takes a key of its own is whether it has a state directory, which every integration has by
default ([default-state-dir.md](default-state-dir.md)): with one it declares both capabilities
and takes the key, and the next start comes back with it; with none (`none`) it declares neither,
as [above](#without-persistent-storage), and a server offers it nothing. The headers given (the
enrollment key in `OTEL_OPAMP_HEADERS`, or in code) are the configured settings: changing them, or
the endpoint, discards the saved ones. Headers come from the configuration alone: no integration
has a callback that would overwrite the ones the server issued (the collector extension and the
supervisor refuse an auth extension with `accepts_opamp_connection_settings` for that reason). The offer's heartbeat interval holds
until the next start, over the SDK's own (`OTEL_OPAMP_HEARTBEAT_INTERVAL`). Each integration's unit tests take a key from a server that offers
one, and `check_setup` (`examples/integrations.py`) takes one from the reference server and comes
back with it at the next start, in every language.

### Secrets at rest

The saved state holds secrets: the headers of the connection settings in use and on trial (their
credentials, bearer tokens typically), and the telemetry and other connection settings whole,
their client certificates' private keys included ([telemetry-settings.md](telemetry-settings.md)).
So each client writes the file, and the package files it downloads beside it
(`<state file>.downloads`), for the agent's user alone:

| Client | Linux, macOS | Windows |
|---|---|---|
| Go, Node, Python | file 0600; downloads a 0700 directory of 0600 files | nothing set: the directory's permissions apply |
| Java | the same | an access-control list allowing only the user, set before anything is written (the file is opened unshared until then); the downloads directory's is inherited by its files |
| .NET | the same | an access-control list allowing only the user, protected from inheritance, set as the file is created; likewise the downloads directory |

The temporary file a save writes first (the state file's path and `.tmp`) is always created new with those permissions: one left behind is
deleted, never reused.

Keep the state file on a local disk, in a directory only the agent's user can read, not on a
shared or network path; on Windows, with the Go, Node and Python clients, that directory's
permissions are all that protect it. Don't copy it anywhere less protected (backups, images).

The core never puts a header's value in a reason it gives, so the clients' logs of its reasons
hold no credential. Endpoints, URLs and remote config bodies are logged as they are.

In .NET, what an application most likely logs is a notification: a `ConnectionSettings` or
`InstallPackage` notification prints (its `ToString`, as `$"{n}"` and structured loggers format it)
the offer with every header's value and every private key shown as `<redacted>`, and every
endpoint, proxy and download URL without its password, as the options records do.

## After a restart: a candidate that may have connected

The spec lets a server drop the old credentials once the agent has connected with the new ones
(step 7), at any time after. So a candidate back from disk after a crash may already have
connected, its promotion not yet saved, and the server may drop the old credentials whenever it
likes. The core treats such a candidate as possibly proven:

- it is tried first, as any saved candidate is;
- if its time runs out or its connections fail, the client connects with the valid settings
  meanwhile, and the candidate stays (reported APPLYING);
- it is dropped (FAILED) only once the server has refused it (401 or 403) and the valid settings
  work; otherwise it is tried again a verification timeout later, until it connects (APPLIED) or
  is refused.

A candidate that is new in this run is dropped as before: the client knows it never connected.

This holds against a server that switches when the new credentials connect and one that
switches at any time after, and against refusals the client can't see (a client certificate
refused in the TLS handshake looks like a failed connection): never locked out in every case. With
invisible refusals, a candidate back from disk that the server refuses is kept and tried again for
ever, so a client should report such a refusal as one. Without this, a candidate that had
connected was dropped after a restart, and a server that switched later locked the agent out.

## Moves and rotations

A server can move an agent to another server: it offers a new endpoint (of the same transport)
with a new key. It can also rotate keys: a second key, a third, at the same endpoint. Either is an
offer like the first one, and every SDK integration follows it the same way:

- The new endpoint and key are one candidate. The SDK never sends the new key to the old endpoint,
  nor the old key to the new one.
- The old endpoint and key stay saved until the new ones are answered. The SDK reports APPLIED,
  with the offer's hash, only after that answer.
- If nothing answers (a closed port, a refusal, the verification timeout), the SDK goes back to
  the old endpoint with the old key and reports FAILED with the offer's hash. It keeps working
  there.
- The new endpoint and key are kept across a restart. The next start goes to them first, not to
  the configured endpoint. (Changing the configured endpoint or headers discards them, as it
  discards any saved settings.)
- What the SDK logs about reaching its server (the "can't reach" warning, "connected to") names
  the endpoint the client uses now, so after a move it names the new server. Each client tells
  it (`Endpoint()` in Go, `endpoint` in Python and Node, `endpoint()` in Java, `Endpoint` in .NET):
  where its latest OpAMP request or connection went. A client in Rust mode can't tell it, so the
  SDK names the configured endpoint then.

Each integration's unit tests check a move to a second server, a second key at the same
endpoint, and a move to an endpoint nobody listens on, each across a restart where it applies. End to end, `check_rotation` (`examples/integrations.py`, the check `rotation` of each
integration's `try.py`) has the reference server's operator rotate an agent's key and then move it
to the server's second listener: the agent takes both, and its next start goes straight to the
new endpoint.

## How it is checked

- The core is checked on its own before it is released: against a model of the design, with and
  without storage, crashes anywhere, an attempt on its way when the verification time runs out, a
  server that switches late, refusals the client can't see, and saves that land but are reported
  failed, over either transport; against a simulated server, disk and crashes, checking the rules
  above after every step; and against a hostile server (malformed endpoints, headers, telemetry
  settings and download URLs, heartbeat intervals up to 2^64 - 1 seconds): no request or
  connection ever carries a value the core refuses, and no deadline is more than a day away. The
  spec's literal step 9 fails it.
- The conformance scenarios `hostile-settings` and `hostile-headers`: a server offers malformed
  endpoints and headers; every client must report each FAILED, never connect to it, and carry on
  (and `hostile-heartbeat`: intervals of 0 and huge ones).
- The conformance scenarios `credential-offer` (the server offers each agent its own token,
  then accepts only that, and the agent restarts halfway) and `credential-offer-broken` (the
  offered token never works), `credential-rotation` (a second token once the first works, and
  the agent never sends an older one again) and `endpoint-move` (a new token at the server's
  second endpoint: the new token goes only there, the old one never does, and after a restart
  the agent goes straight there), for every client over both transports.
