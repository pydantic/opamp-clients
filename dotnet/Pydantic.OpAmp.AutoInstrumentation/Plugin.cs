// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using OpenTelemetry;
using OpenTelemetry.AutoInstrumentation.PluginApi;
using OpenTelemetry.AutoInstrumentation.PluginApi.Telemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Instrumentation.AspNetCore;
using OpenTelemetry.Logs;
using OpenTelemetry.Metrics;
using OpenTelemetry.Resources;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Otel;

namespace Pydantic.OpAmp.AutoInstrumentation;

/// <summary>
/// OpenTelemetry .NET automatic instrumentation under an OpAMP server (docs/dotnet-otel.md): with
/// <c>OTEL_DOTNET_AUTO_PLUGINS="Pydantic.OpAmp.AutoInstrumentation.Plugin, Pydantic.OpAmp.AutoInstrumentation"</c>
/// and <c>OTEL_OPAMP_ENDPOINT</c>, the SDK joins the server's fleet and takes its settings: a
/// sampler and exporters the settings change while the application runs, the instrumentation
/// switches at the next start, one instance UID. What the settings mean, what to answer and what
/// to report is the SDK settings layer's (<c>Pydantic.OpAmp.Client.Sdk</c>), the same in every
/// language; the plugin carries it out on the .NET SDK. Upstream's own OpAMP client stays off.
/// </summary>
public sealed class Plugin : IPlugin, ITelemetryPlugin, IConfigureTracesOptions<OtlpExporterOptions>,
    IConfigureMetricsOptions<OtlpExporterOptions>, IConfigureLogsOptions<OtlpExporterOptions>,
    IConfigureTracesOptions<AspNetCoreTraceInstrumentationOptions>, IConfigureLogsOptions<OpenTelemetryLoggerOptions>
{
    private Agent? agent;
    private Resource? resource;

    /// <summary>Why the core can't be loaded here, or null when it can (a seam for tests).</summary>
    internal Func<string?> CoreUnavailable { get; init; } = global::Pydantic.OpAmp.Client.OpAmpSetup.Unavailable;

    /// <summary>
    /// Before automatic instrumentation reads its settings: load the saved settings, put them in
    /// force, and switch off the instrumentations they name. A plugin that can't start leaves the
    /// SDK unmanaged: telemetry never waits on the server.
    /// </summary>
    public void Initializing()
    {
        if (this.CoreUnavailable() is { } unavailable)
        {
            Log.Warn($"the core can't be loaded on {System.Runtime.InteropServices.RuntimeInformation.RuntimeIdentifier}; the SDK runs unmanaged: {unavailable}");
            return;
        }

        try
        {
            this.agent = Agent.Prepare(Variables.Process(), Environment.SetEnvironmentVariable);
        }
        catch (Exception e)
        {
            Log.Error($"the plugin did not start; the SDK runs unmanaged: {e.Message}");
            return;
        }

        if (this.agent is { } a)
        {
            // Stop never throws; nothing here may fail the exit either. It takes as long as the
            // core says (its stop timeout, 5 s): no race of the plugin's own.
            AppDomain.CurrentDomain.ProcessExit += (_, _) =>
            {
                try
                {
                    a.Stop(Timeout.InfiniteTimeSpan);
                }
                catch (Exception)
                {
                }
            };
        }
    }

    /// <summary>
    /// Once the SDK is built: join the fleet. Every plugin has had the providers by now, another
    /// plugin's processors and exporters among them: what they export through is read as the agent
    /// starts.
    /// </summary>
    public void Initialized()
    {
        if (this.agent is not { } a)
        {
            return;
        }

        try
        {
            var r = this.resource ?? ResourceBuilder.CreateDefault().AddAttributes([new("service.instance.id", a.Uid)]).Build();
            a.Start(r.Attributes);
        }
        catch (Exception e)
        {
            Log.Error($"the client did not start; the SDK runs unmanaged: {e.Message}");
        }
    }

    /// <inheritdoc/>
    public ResourceBuilder ConfigureResource(ResourceBuilder builder) =>
        this.agent is { } a ? builder.AddAttributes([new("service.instance.id", a.Uid)]) : builder;

    /// <inheritdoc/>
    public TracerProviderBuilder BeforeConfigureTracerProvider(TracerProviderBuilder builder) => builder;

    /// <inheritdoc/>
    public TracerProviderBuilder AfterConfigureTracerProvider(TracerProviderBuilder builder)
    {
        this.agent?.WrapSampler(builder);
        return builder;
    }

    /// <inheritdoc/>
    public void TracerProviderInitialized(TracerProvider tracerProvider)
    {
        this.resource ??= tracerProvider.GetResource();
        this.agent?.Built(tracerProvider);
    }

    /// <inheritdoc/>
    public MeterProviderBuilder BeforeConfigureMeterProvider(MeterProviderBuilder builder) => builder;

    /// <inheritdoc/>
    public MeterProviderBuilder AfterConfigureMeterProvider(MeterProviderBuilder builder) => builder;

    /// <inheritdoc/>
    public void MeterProviderInitialized(MeterProvider meterProvider)
    {
        this.resource ??= meterProvider.GetResource();
        this.agent?.Built(meterProvider);
    }

    /// <inheritdoc/>
    public void ConfigureTracesOptions(OtlpExporterOptions options) => this.agent?.Gate(options, Signal.Traces);

    /// <summary>ASP.NET Core's requests: sampling rules see their path in the instrumentation's Filter (<see cref="AspNetCoreFilter"/>).</summary>
    public void ConfigureTracesOptions(AspNetCoreTraceInstrumentationOptions options) => this.agent?.Filter(options);

    /// <inheritdoc/>
    public void ConfigureMetricsOptions(OtlpExporterOptions options) => this.agent?.Gate(options, Signal.Metrics);

    /// <inheritdoc/>
    public void ConfigureLogsOptions(OtlpExporterOptions options) => this.agent?.Gate(options, Signal.Logs);

    /// <summary>Each logger provider's options: what it exports through is read once it is built (<see cref="Agent.Watch"/>).</summary>
    public void ConfigureLogsOptions(OpenTelemetryLoggerOptions options) => this.agent?.Watch(options);
}
