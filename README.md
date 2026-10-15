# OpAMP clients

OpAMP client SDKs for Python, Go, Node.js, Java and .NET, on one shared core.

| Language | Client | OpenTelemetry integration |
|---|---|---|
| Python | [`python/`](python/), [docs](docs/python.md) | [`python/otel/`](python/otel/), [docs](docs/python-otel.md) |
| Go | [`go/opamp/`](go/opamp/) | [`go/otel/`](go/otel/), [docs](docs/go-otel.md); also a Collector extension and Supervisor ([`go/opampextension/`](go/opampextension/), [`go/opampsupervisor/`](go/opampsupervisor/)) |
| Node.js | [`node/opamp/`](node/opamp/) | [`node/otel/`](node/otel/), [docs](docs/node-otel.md) |
| Java | [`java/opamp/`](java/opamp/), [docs](docs/java.md) | [`java/opamp-javaagent/`](java/opamp-javaagent/), [docs](docs/javaagent.md) |
| .NET | [`dotnet/`](dotnet/), [docs](docs/dotnet.md) | [docs](docs/dotnet-otel.md) |

[docs/how-it-works.md](docs/how-it-works.md) explains how the clients and the core fit together,
[docs/sdks.md](docs/sdks.md) compares the SDKs, and [examples/](examples/) runs each one.

The core is the state machine every client wraps. It arrives here compiled, as this repository's
`core/vX.Y.Z` releases: the WebAssembly modules Go, Node.js and Java load, the native library
Python and .NET load, and the interface (`.proto`) files the clients generate their types from.
`opamp-core.lock.json` pins one core release and the sha256 of every file in it.

```bash
scripts/fetch_core.py fetch                  # download what the lock pins, verify it, put it in place
scripts/fetch_core.py fetch --all-platforms  # and every platform's native library, for packaging
scripts/fetch_core.py check                  # what is in place is what the lock pins
scripts/fetch_core.py lock 0.2.0             # move to another core release
```

Moving to a new core is a PR that changes only the lock.

This repository is public and holds only what `PUBLIC_ALLOWLIST` names; see `AGENTS.md`.
