# Pydantic.OpAmp.Extensions.Hosting

`WithOpAmp()`: an OpenTelemetry .NET SDK set up in code (`services.AddOpenTelemetry()`), managed
by an OpAMP server. For automatic instrumentation, see
[Pydantic.OpAmp.AutoInstrumentation](../Pydantic.OpAmp.AutoInstrumentation/README.md).

## Quickstart

Reference it (from the repository, until it is published):

```xml
<ProjectReference Include="path/to/dotnet/Pydantic.OpAmp.Extensions.Hosting/Pydantic.OpAmp.Extensions.Hosting.csproj" />
```

Add `WithOpAmp()` last to the SDK's setup:

```csharp
builder.Services.AddOpenTelemetry()
    .WithTracing(t => t.AddAspNetCoreInstrumentation().AddOtlpExporter())
    .WithOpAmp();
```

Then run the application with the server's address and enrollment key (or set `OpAmp:Endpoint` and
`OpAmp:Headers:Authorization` in its configuration):

```bash
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
dotnet Checkout.dll
```

When it works, the application's logger says, at Information:

```
OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>
```

When it can't connect, it warns once, with what to check.

The server can also send the application settings of its own (a feature flag, say), which it reads
with `Pydantic.OpAmp.OpAmpAppSettings.Get("checkout.new_flow", "off")` and can be told of with
`OpAmpAppSettings.OnChange` ([the application's own settings](../../docs/dotnet-otel.md#the-applications-own-settings)).

The instance keeps its UID across restarts with no configuration
([default-state-dir.md](../../docs/default-state-dir.md)).

Everything else is in [docs/dotnet-otel.md](../../docs/dotnet-otel.md#in-code-withopamp).
