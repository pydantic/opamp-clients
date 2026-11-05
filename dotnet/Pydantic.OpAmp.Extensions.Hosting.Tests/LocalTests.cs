// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using OpenTelemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Trace;
using Xunit;

namespace Pydantic.OpAmp.Extensions.Hosting.Tests;

/// <summary>
/// The local report (contracts' logfire-sdk-local/v1): what the SDK the application built does
/// without the server, as the host's <c>local</c> carries it to the layer; and each
/// instrumentation's scope in the inventory.
/// </summary>
public class LocalTests
{
    private static CancellationToken Cancel => TestContext.Current.CancellationToken;

    /// <summary>A host with <paramref name="setup"/>'s SDK, started against <paramref name="server"/>: its agent.</summary>
    private static async Task<(IHost Host, OpAmpAgent Agent)> Started(OpAmpServer server, Action<HostApplicationBuilder> setup,
        Dictionary<string, string?>? more = null)
    {
        var b = Host.CreateApplicationBuilder(new HostApplicationBuilderSettings { DisableDefaults = true });
        b.Configuration.AddInMemoryCollection(new Dictionary<string, string?>
        {
            ["OpAmp:Endpoint"] = server.Endpoint,
            ["OpAmp:StateDirectory"] = "none",
            ["OpAmp:HeartbeatInterval"] = "100ms",
        });
        b.Configuration.AddInMemoryCollection(more ?? []);
        setup(b);
        var host = b.Build();
        await host.StartAsync(Cancel);
        return (host, host.Services.GetRequiredService<OpAmpAgent>());
    }

    private static void Otlp(OtlpExporterOptions o)
    {
        o.Endpoint = new Uri("http://127.0.0.1:1/v1/traces");
        o.Protocol = OtlpExportProtocol.HttpProtobuf;
    }

    [Fact]
    public async Task TheHostCarriesTheSamplersRatioAndWhichSignalsAreExported()
    {
        await using var server = new OpAmpServer();
        // The configuration is read as the variables are (OpAmpAgent's getenv).
        var (host, agent) = await Started(server, b => b.Services.AddOpenTelemetry()
            .WithTracing(t => t.AddSource("s").AddOtlpExporter(Otlp)).WithOpAmp(),
            new() { ["OTEL_TRACES_SAMPLER"] = "parentbased_traceidratio", ["OTEL_TRACES_SAMPLER_ARG"] = "0.5" });
        using (host)
        {
            var local = agent.HostNow(providersBuilt: true).Local;
            Assert.Equal("0.5", local.SamplingRate);
            // Traces have an exporter; no meter or logger provider: nothing exported of them.
            Assert.Equal([true, false, false], new[] { local.SendTraces, local.SendMetrics, local.SendLogs });
            Assert.True(local.HasSendMetrics && local.HasSendLogs);
            Assert.False(local.HasSendLogsLevel); // the SDK has none of its own
            // Before the providers are built nothing is told.
            Assert.Null(agent.HostNow(providersBuilt: false).Local);
            await host.StopAsync(Cancel);
        }
    }

    [Fact]
    public async Task TheApplicationsOwnSamplerIsNotTold()
    {
        await using var server = new OpAmpServer();
        var (host, agent) = await Started(server, b => b.Services.AddOpenTelemetry()
            .WithTracing(t => t.AddSource("s").SetSampler(new AlwaysOffSampler()).AddOtlpExporter(Otlp)).WithOpAmp());
        using (host)
        {
            Assert.False(agent.HostNow(providersBuilt: true).Local.HasSamplingRate);
            await host.StopAsync(Cancel);
        }
    }

    [Fact]
    public void AnInstrumentationsScopeIsTheTablesElseItsAssemblysName()
    {
        Assert.Equal("Microsoft.AspNetCore", Otel.Inventories.Scoped("aspnetcore", "1.15.0", "OpenTelemetry.Instrumentation.AspNetCore").Scope);
        Assert.Equal("System.Net.Http", Otel.Inventories.Scoped("httpclient", "1.15.0", "OpenTelemetry.Instrumentation.Http").Scope);
        Assert.Equal("Acme.Instrumentation", Otel.Inventories.Scoped("acmeinstrumentation", "1.0.0", "Acme.Instrumentation").Scope);
    }
}
