# OpAMP clients

OpAMP client SDKs for Python, Go, Node.js, Java and .NET, on one shared core.

**Start here:** [docs/getting-started.md](docs/getting-started.md) connects an application, a
host or a Kubernetes cluster to an OpAMP server, such as Logfire's Fleet
([docs/guides/logfire.md](docs/guides/logfire.md)).
[docs/supported-versions.md](docs/supported-versions.md) lists the runtimes and platforms each SDK
supports, and [docs/troubleshooting.md](docs/troubleshooting.md) what to do when something
doesn't connect.

| Language | OpenTelemetry integration (start here) | Install | Client |
|---|---|---|---|
| Python | [`python/otel/`](python/otel/), [docs](docs/python-otel.md) | `pip install logfire-fleet-otel` | [`python/`](python/) (`logfire-fleet-client`), [docs](docs/python.md) |
| Go | [`go/opampotel/`](go/opampotel/), [docs](docs/go-otel.md); also a Collector extension and Supervisor ([`go/opampextension/`](go/opampextension/), [`go/opampsupervisor/`](go/opampsupervisor/), [docs](docs/collector.md)) | `go get github.com/pydantic/opamp-clients/go/opampotel` | [`go/opamp/`](go/opamp/), [docs](docs/go.md) |
| Node.js | [`node/otel/`](node/otel/), [docs](docs/node-otel.md) | `npm install @pydantic/logfire-fleet-otel` | [`node/opamp/`](node/opamp/) (`@pydantic/logfire-fleet-client`), [docs](docs/node.md) |
| Java | [`java/opamp-javaagent/`](java/opamp-javaagent/), [docs](docs/javaagent.md) | `dev.pydantic:logfire-fleet-javaagent-extension` from Maven Central | [`java/opamp/`](java/opamp/) (`dev.pydantic:logfire-fleet-client`), [docs](docs/java.md) |
| .NET | [docs](docs/dotnet-otel.md) | `dotnet add package Logfire.Fleet.AutoInstrumentation` (or `.Extensions.Hosting`) | [`dotnet/`](dotnet/) (`Logfire.Fleet.Client`), [docs](docs/dotnet.md) |

[docs/how-it-works.md](docs/how-it-works.md) explains how the clients and the core fit together,
[docs/sdks.md](docs/sdks.md) compares the SDKs, and [examples/](examples/) runs each one.
Two guides start from an application as it is: on the Logfire SDK or sending to Logfire
([docs/guides/logfire.md](docs/guides/logfire.md)), and on Elastic's OpenTelemetry distributions
([docs/guides/elastic.md](docs/guides/elastic.md)).

The core is the state machine every client wraps. It arrives here compiled, as this repository's
`core/vX.Y.Z` releases: the WebAssembly modules Go, Node.js and Java load, the native library
Python and .NET load, and the interface (`.proto`) files the clients generate their types from.
`opamp-core.lock.json` pins one core release and the sha256 of every file in it.

```bash
scripts/fetch_core.py fetch                  # download what the lock pins, verify it, put it in place
scripts/fetch_core.py fetch --all-platforms  # and every platform's native library, for packaging
scripts/fetch_core.py check                  # what is in place is what the lock pins
scripts/fetch_core.py lock <version>         # move to another core release
```

Moving to a new core is one PR: `scripts/fetch_core.py lock <version>`, then `fetch`, then
`scripts/gen_core.py` and `scripts/gen_sdk_settings.py`. It holds the lock, the module copies
checked in beside each client and the regenerated types; CI's `fetch_core.py check` and the
generators' `--check` keep them in step.

This repository is public and holds only what `PUBLIC_ALLOWLIST` names; see `AGENTS.md`.
