// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// The setups check_truth runs the application under (examples/integrations.py), named in
// EXAMPLE_SETUP; none otherwise:
//
// - plugin-exporters: a plugin of the application's own (ExtraExporters below, named in
//   OTEL_DOTNET_AUTO_PLUGINS after ours) adds a span exporter and a log record processor to the SDK
//   automatic instrumentation builds, each exporting over OTLP to the same place as the SDK's own.
//   They don't go through the OpAMP plugin's gates.
// - own-sdk: with OTEL_DOTNET_AUTO_SETUP_SDK=false, automatic instrumentation loads its
//   instrumentations but leaves the tracer and meter providers to the application, which builds
//   them here (OwnSdk.Build), exporting over OTLP.

using OpenTelemetry;
using OpenTelemetry.AutoInstrumentation.PluginApi;
using OpenTelemetry.AutoInstrumentation.PluginApi.Telemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Logs;
using OpenTelemetry.Metrics;
using OpenTelemetry.Resources;
using OpenTelemetry.Trace;

namespace Example;

/// <summary>The setups EXAMPLE_SETUP names (comma-separated).</summary>
public static class Setup
{
    public static bool Has(string name) =>
        (Environment.GetEnvironmentVariable("EXAMPLE_SETUP") ?? string.Empty)
            .Split(',', StringSplitOptions.TrimEntries | StringSplitOptions.RemoveEmptyEntries).Contains(name);
}

/// <summary>
/// plugin-exporters: an application's plugin for automatic instrumentation, adding exporters of its
/// own. Automatic instrumentation calls each plugin it names in turn.
/// </summary>
public sealed class ExtraExporters : IPlugin, ITelemetryPlugin, IConfigureLogsOptions<OpenTelemetryLoggerOptions>
{
    public void Initializing()
    {
    }

    public void Initialized()
    {
    }

    public ResourceBuilder ConfigureResource(ResourceBuilder builder) => builder;

    public TracerProviderBuilder BeforeConfigureTracerProvider(TracerProviderBuilder builder) => builder;

    public void TracerProviderInitialized(TracerProvider tracerProvider)
    {
    }

    public MeterProviderBuilder BeforeConfigureMeterProvider(MeterProviderBuilder builder) => builder;

    public MeterProviderBuilder AfterConfigureMeterProvider(MeterProviderBuilder builder) => builder;

    public void MeterProviderInitialized(MeterProvider meterProvider)
    {
    }

    public TracerProviderBuilder AfterConfigureTracerProvider(TracerProviderBuilder builder) =>
        Setup.Has("plugin-exporters")
            ? builder.AddProcessor(new BatchActivityExportProcessor(new OtlpTraceExporter(new OtlpExporterOptions()), scheduledDelayMilliseconds: 500))
            : builder;

    public void ConfigureLogsOptions(OpenTelemetryLoggerOptions options)
    {
        if (Setup.Has("plugin-exporters"))
        {
            options.AddProcessor(new BatchLogRecordExportProcessor(new OtlpLogExporter(new OtlpExporterOptions()), scheduledDelayMilliseconds: 500));
        }
    }
}

/// <summary>own-sdk: the application's own tracer and meter providers, built in code.</summary>
public static class OwnSdk
{
    public static IDisposable[] Build() =>
    [
        Sdk.CreateTracerProviderBuilder().AddHttpClientInstrumentation().AddOtlpExporter().Build(),
        Sdk.CreateMeterProviderBuilder().AddHttpClientInstrumentation().AddOtlpExporter().Build(),
    ];
}
