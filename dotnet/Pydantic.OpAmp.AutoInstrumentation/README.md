# Pydantic.OpAmp.AutoInstrumentation

A plugin for upstream's OpenTelemetry .NET automatic instrumentation that puts the SDK it sets up
under an OpAMP server. For an SDK set up in code, see
[Pydantic.OpAmp.Extensions.Hosting](../Pydantic.OpAmp.Extensions.Hosting/README.md).

## Quickstart

Reference the plugin next to the automatic instrumentation package (from the repository, until it
is published):

```xml
<PackageReference Include="OpenTelemetry.AutoInstrumentation" Version="1.17.0" />
<ProjectReference Include="path/to/dotnet/Pydantic.OpAmp.AutoInstrumentation/Pydantic.OpAmp.AutoInstrumentation.csproj" />
```

Then name it in `OTEL_DOTNET_AUTO_PLUGINS` and run the application with the server's address and
enrollment key:

```bash
OTEL_DOTNET_AUTO_PLUGINS="Pydantic.OpAmp.AutoInstrumentation.Plugin, Pydantic.OpAmp.AutoInstrumentation" \
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
OTEL_LOG_LEVEL=info \
./instrument.sh dotnet Checkout.dll
```

When it works, it says on standard error:

```
OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>
```

That line shows only with `OTEL_LOG_LEVEL=info`. When it can't connect, it warns once, with what
to check; warnings always show.

The instance keeps its UID across restarts with no configuration
([default-state-dir.md](../../docs/default-state-dir.md)).

The server can also send the application settings of its own (a feature flag, say), which it reads
with `Pydantic.OpAmp.OpAmpAppSettings.Get("checkout.new_flow", "off")` and can be told of with
`OpAmpAppSettings.OnChange` ([the application's own settings](../../docs/dotnet-otel.md#the-applications-own-settings)).

Everything else is in [docs/dotnet-otel.md](../../docs/dotnet-otel.md).
