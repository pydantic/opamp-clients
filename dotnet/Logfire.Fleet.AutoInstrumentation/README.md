# Logfire.Fleet.AutoInstrumentation

A plugin for upstream's OpenTelemetry .NET automatic instrumentation that puts the SDK it sets up
under an OpAMP server. For an SDK set up in code, see
[Logfire.Fleet.Extensions.Hosting](https://github.com/pydantic/opamp-clients/blob/main/dotnet/Logfire.Fleet.Extensions.Hosting/README.md).

## Quickstart

Add the plugin next to the automatic instrumentation package:

```bash
dotnet add package OpenTelemetry.AutoInstrumentation --version 1.17.0
dotnet add package Logfire.Fleet.AutoInstrumentation
```

Then name it in `OTEL_DOTNET_AUTO_PLUGINS` and run the application with the server's address and
key:

```bash
OTEL_DOTNET_AUTO_PLUGINS="Logfire.Fleet.AutoInstrumentation.Plugin, Logfire.Fleet.AutoInstrumentation" \
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<key>" \
./instrument.sh dotnet Checkout.dll
```

When it works, it says on standard error:

```
OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>
```

It shows with no logging setup (an `OTEL_LOG_LEVEL` other than `info` or `debug` hides it). In Logfire, **Fleet →
Connect an application** gives the endpoint and the key. When it can't connect, it warns once,
with what to check; warnings always show.

The instance keeps its UID across restarts with no configuration
([default-state-dir.md](https://github.com/pydantic/opamp-clients/blob/main/docs/default-state-dir.md)).

The server can also send the application settings of its own (a feature flag, say), which it reads
with `Logfire.Fleet.OpAmpAppSettings.Get("checkout.new_flow", "off")` and can be told of with
`OpAmpAppSettings.OnChange` ([the application's own settings](https://github.com/pydantic/opamp-clients/blob/main/docs/dotnet-otel.md#the-applications-own-settings)).

Everything else is in [docs/dotnet-otel.md](https://github.com/pydantic/opamp-clients/blob/main/docs/dotnet-otel.md).
