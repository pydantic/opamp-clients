# Supported versions

Which runtimes, OpenTelemetry versions and platforms each piece of this repository supports, and
where each is tested. "Every PR" is CI ([ci.yml](../.github/workflows/ci.yml)), which every pull
request and every push to `main` runs. "Nightly" is
[nightly.yml](../.github/workflows/nightly.yml)'s `runtimes` job.

## Language runtimes

| SDK | Needs | Every PR | Nightly |
|---|---|---|---|
| Python: client, auto-instrumentation, Logfire SDK | CPython 3.11 or later, free-threaded builds included | 3.13 | 3.11 and 3.14; 3.13 on macOS and Windows |
| Go: client | Go 1.25 or later | 1.26 | 1.25 |
| Go: SDK setup (`opampotel`), Collector extension, Supervisor | Go 1.26 or later | 1.26 | |
| Node.js: client, auto-instrumentation | Node.js 22 or later | 24 | 22 and the current release |
| Java: client, agent extension | Java 17 or later | 21 (25 for the Fray tests) | 17 and 25 |
| .NET: client, hosting, automatic instrumentation plugin | .NET 8 or .NET 10 | .NET 8 and .NET 10 | both, on macOS and Windows |

The .NET packages ship an assembly for each of `net8.0` and `net10.0`. .NET 9 and later releases
take the `net8.0` one.

A runtime stays supported while its maintainers support it. One past its end of support may be
dropped in a minor release, listed under **Breaking** in the [CHANGELOG](../CHANGELOG.md). .NET 8
reaches its end of support on 2026-11-10.

## OpenTelemetry

Each integration is built and tested against these releases. The ranges are what the package
declares.

| Integration | Tested with | Declares |
|---|---|---|
| Python auto-instrumentation ([python-otel.md](python-otel.md)) | `opentelemetry-sdk` 1.45 | `opentelemetry-sdk` 1.43 or later (the SDK's OpAMP hook came in 1.43) |
| The Logfire SDK for Python (`configure_logfire()`) | `logfire` 5.x | `logfire` 5.1 or later, below 6 |
| Go SDK setup ([go-otel.md](go-otel.md)) | `go.opentelemetry.io/otel` 1.46, contrib 0.71 | the versions in its `go.mod` or later |
| Node.js auto-instrumentation ([node-otel.md](node-otel.md)) | `@opentelemetry/sdk-node` 0.222, `auto-instrumentations-node` 0.80 | those exact versions |
| Java agent extension ([javaagent.md](javaagent.md)) | the OpenTelemetry Java agent 2.31.1 (SDK 1.66) | built against agent 2.31.1; other agent versions aren't tested |
| .NET automatic instrumentation plugin ([dotnet-otel.md](dotnet-otel.md)) | automatic instrumentation 1.17.0 (OpenTelemetry .NET 1.19.1) | automatic instrumentation 1.17.x, as upstream requires of plugins |
| Collector extension and Supervisor ([collector.md](collector.md)) | the Collector v0.163.0 (v1.69.0 for the stable modules) | that release |

Elastic's distributions are checked too ([guides/elastic.md](guides/elastic.md)): EDOT Java
1.14.0, EDOT .NET 1.6.1 and EDOT Python 1.18.0, with `try.py --edot`.

## Platforms

Go, Node.js and Java run the core as WebAssembly, so they run wherever their runtime does. Python
and .NET load the core as a native library, built for these platforms:

| Platform | Python wheel tag | .NET runtime identifier | Tested |
|---|---|---|---|
| Linux x64, glibc 2.17 or later | `manylinux_2_17_x86_64` | `linux-x64` | every PR |
| Linux arm64, glibc 2.17 or later | `manylinux_2_17_aarch64` | `linux-arm64` | every PR (installed and run) |
| Linux x64, musl (Alpine) | `musllinux_1_2_x86_64` | `linux-musl-x64` | every PR (installed and run) |
| Linux arm64, musl (Alpine) | `musllinux_1_2_aarch64` | `linux-musl-arm64` | every PR (installed and run) |
| macOS arm64, 11 or later | `macosx_11_0_arm64` | `osx-arm64` | nightly |
| macOS x64, 10.12 or later | `macosx_10_12_x86_64` | `osx-x64` | built, not run |
| Windows x64 | `win_amd64` | `win-x64` | nightly |
| Windows arm64 | `win_arm64` | `win-arm64` | built, not run |

On a platform without a library, or where it can't be loaded, the SDK says so once and runs
unmanaged: the application and its telemetry carry on, on their own settings
([troubleshooting.md](troubleshooting.md#the-core-cant-be-loaded)).

The Supervisor's release archives are for Linux, macOS and Windows, on amd64 and arm64. Its
installers (deb and rpm, macOS `.pkg`, Windows `.msi`) are built, installed and enrolled in CI on
every PR, on Linux, macOS and Windows.

## OpAMP servers

The clients speak OpAMP as the [specification](https://opentelemetry.io/docs/specs/opamp/) has
it, over WebSocket (`ws://`, `wss://`) or plain HTTP polling (`http://`, `https://`), and work
with any server that does. The Logfire SDK policy, the settings an SDK takes
([sdk-settings.md](sdk-settings.md)), is a remote config file of our own: a server that doesn't
send one still sees each agent, and can send collectors their YAML.
