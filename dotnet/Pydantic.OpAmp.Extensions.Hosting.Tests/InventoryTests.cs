// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Diagnostics;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using OpAmp.Proto.V1;
using OpenTelemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Logs;
using OpenTelemetry.Metrics;
using OpenTelemetry.Trace;
using Xunit;

namespace Pydantic.OpAmp.Extensions.Hosting.Tests;

/// <summary>The inventory (docs/available-components.md): what WithOpAmp() lists of the SDK the application built, and the client sending it.</summary>
public class InventoryTests
{
    private static CancellationToken Cancel => TestContext.Current.CancellationToken;

    private const string Collector = "http://127.0.0.1:1";

    /// <summary>A host with <paramref name="setup"/>'s SDK, started against <paramref name="server"/>: its agent's components.</summary>
    private static async Task<(IHost Host, AvailableComponents Components)> Started(OpAmpServer server, Action<HostApplicationBuilder> setup)
    {
        var b = Host.CreateApplicationBuilder(new HostApplicationBuilderSettings { DisableDefaults = true });
        b.Configuration.AddInMemoryCollection(new Dictionary<string, string?>
        {
            ["OpAmp:Endpoint"] = server.Endpoint,
            ["OpAmp:StateDirectory"] = "none",
            ["OpAmp:HeartbeatInterval"] = "100ms",
        });
        setup(b);
        var host = b.Build();
        await host.StartAsync(Cancel);
        return (host, host.Services.GetRequiredService<OpAmpAgent>().Agent!.Components!);
    }

    private static void Otlp(OtlpExporterOptions o, string path)
    {
        o.Endpoint = new Uri(Collector + path);
        o.Protocol = OtlpExportProtocol.HttpProtobuf;
    }

    /// <summary>Each signal's exporters, by name: whether gated.</summary>
    private static Dictionary<string, Dictionary<string, bool>> Exporters(AvailableComponents c) =>
        c.Components["exporters"].SubComponentMap.ToDictionary(
            s => s.Key, s => s.Value.SubComponentMap.ToDictionary(e => e.Key, e => e.Value.Metadata.Single(m => m.Key == "gated").Value.BoolValue));

    /// <summary>Each instrumentation, by name: its version (empty: none known).</summary>
    private static Dictionary<string, string> Instrumentations(AvailableComponents c) =>
        c.Components["instrumentations"].SubComponentMap.ToDictionary(
            i => i.Key, i => i.Value.Metadata.SingleOrDefault(m => m.Key == "version")?.Value.StringValue ?? string.Empty);

    private static string Sdk(AvailableComponents c, string key) =>
        c.Components["sdk"].Metadata.Single(m => m.Key == key).Value.StringValue;

    [Fact]
    public async Task ATypicalSetupListsItselfTheSdkItsInstrumentationsAndItsExporters()
    {
        await using var server = new OpAmpServer();
        var (host, c) = await Started(server, b =>
        {
            b.Logging.AddOpenTelemetry(o => o.AddOtlpExporter(e => Otlp(e, "/v1/logs")));
            b.Services.AddOpenTelemetry()
                .WithTracing(t => t.AddSource("s").AddAspNetCoreInstrumentation().AddOtlpExporter(e => Otlp(e, "/v1/traces")))
                .WithMetrics(m => m.AddMeter("m").AddOtlpExporter(e => Otlp(e, "/v1/metrics")).AddReader(new PeriodicExportingMetricReader(new Kept<Metric>())))
                .WithOpAmp();
        });
        using (host)
        {
            Assert.Equal("Pydantic.OpAmp.Extensions.Hosting", Sdk(c, "integration.name"));
            Assert.Equal("0.1.0", Sdk(c, "integration.version"));
            Assert.Equal("dotnet", Sdk(c, "telemetry.sdk.language"));
            // The SDK's own telemetry.sdk.version, as the resource has it.
            var resource = host.Services.GetRequiredService<TracerProvider>().GetResource().Attributes;
            Assert.Equal(resource.Single(a => a.Key == "telemetry.sdk.version").Value, Sdk(c, "telemetry.sdk.version"));

            // As automatic instrumentation's variables name it, at its package's version.
            Assert.Equal(new Dictionary<string, string> { ["aspnetcore"] = "1.15.0" }, Instrumentations(c));
            Assert.Equal(
                new Dictionary<string, Dictionary<string, bool>>
                {
                    ["traces"] = new() { ["otlp"] = true },
                    ["metrics"] = new() { ["otlp"] = false, ["Kept"] = false }, // a reader of its own: send_metrics isn't carried out
                    ["logs"] = new() { ["otlp"] = true },
                },
                Exporters(c));

            // The settings are the ones it carries out, by the host it runs by.
            var settings = c.Components["settings"].SubComponentMap;
            Assert.DoesNotContain("send_metrics", settings.Keys);
            Assert.DoesNotContain("deactivate_instrumentations", settings.Keys); // named in code
            Assert.Contains("send_traces", settings.Keys);
            Assert.Equal(
                new Client.Sdk.SdkSettings().Components(new Client.Sdk.Inventory(), host.Services.GetRequiredService<OpAmpAgent>().HostNow(providersBuilt: true), 1).Components["settings"],
                c.Components["settings"]);
            await host.StopAsync(Cancel);
        }
    }

    [Fact]
    public async Task WithNoInstrumentationOrExporterNoneIsListed()
    {
        await using var server = new OpAmpServer();
        var (host, c) = await Started(server, b => b.Services.AddOpenTelemetry().WithTracing(t => t.AddSource("s")).WithOpAmp());
        using (host)
        {
            Assert.Empty(c.Components["instrumentations"].SubComponentMap);
            Assert.Empty(c.Components["exporters"].SubComponentMap);
            Assert.Equal("dotnet", Sdk(c, "telemetry.sdk.language"));
            await host.StopAsync(Cancel);
        }
    }

    // Two OTLP exporters of a signal are one, gated; an instrumentation added twice is one.
    [Fact]
    public async Task AnExporterOrInstrumentationAddedTwiceIsListedOnce()
    {
        await using var server = new OpAmpServer();
        var (host, c) = await Started(server, b => b.Services.AddOpenTelemetry()
            .WithTracing(t => t.AddSource("s").AddAspNetCoreInstrumentation().AddInstrumentation(() => new Probe()).AddInstrumentation(() => new Probe())
                .AddOtlpExporter(e => Otlp(e, "/v1/traces")).AddOtlpExporter(e => Otlp(e, "/v1/traces")))
            .WithOpAmp());
        using (host)
        {
            Assert.Equal(new Dictionary<string, Dictionary<string, bool>> { ["traces"] = new() { ["otlp"] = true } }, Exporters(c));
            var instrumentations = Instrumentations(c);
            Assert.Equal(["aspnetcore", "pydanticopampextensionshostingtests"], instrumentations.Keys.Order(StringComparer.Ordinal));
            Assert.Equal("1.0.0", instrumentations["pydanticopampextensionshostingtests"]);
            await host.StopAsync(Cancel);
        }
    }

    // One that isn't OTLP is listed by its type, and so is a processor it can't see into; beside
    // them, the OTLP exporter isn't gated either: the signal's sending isn't carried out.
    [Fact]
    public async Task AnExporterOfATypeWithoutAVariableNameIsListedByItsType()
    {
        await using var server = new OpAmpServer();
        var (host, c) = await Started(server, b => b.Services.AddOpenTelemetry()
            .WithTracing(t => t.AddSource("s").AddOtlpExporter(e => Otlp(e, "/v1/traces")).AddProcessor(new SimpleActivityExportProcessor(new Kept<Activity>())))
            .WithMetrics(m => m.AddMeter("m").AddReader(new Pulled()))
            .WithOpAmp());
        using (host)
        {
            Assert.Equal(
                new Dictionary<string, Dictionary<string, bool>>
                {
                    ["traces"] = new() { ["otlp"] = false, ["Kept"] = false },
                    ["metrics"] = new() { ["Pulled"] = false },
                },
                Exporters(c));
            await host.StopAsync(Cancel);
        }
    }

    // An OTLP exporter the gate can't be in front of (an endpoint with a path of its own) is not gated.
    [Fact]
    public async Task AnOtlpExporterTheGateIsntInFrontOfIsNotGated()
    {
        await using var server = new OpAmpServer();
        var (host, c) = await Started(server, b => b.Services.AddOpenTelemetry()
            .WithTracing(t => t.AddSource("s").AddOtlpExporter(e => Otlp(e, "/ingest/spans")))
            .WithOpAmp());
        using (host)
        {
            Assert.Equal(new Dictionary<string, Dictionary<string, bool>> { ["traces"] = new() { ["otlp"] = false } }, Exporters(c));
            Assert.DoesNotContain("send_traces", c.Components["settings"].SubComponentMap.Keys);
            await host.StopAsync(Cancel);
        }
    }

    // The client sends the hash from the first message, and the components in full once asked.
    [Fact]
    public async Task TheClientSendsTheComponents()
    {
        await using var server = new OpAmpServer();
        var (host, c) = await Started(server, b => b.Services.AddOpenTelemetry()
            .WithTracing(t => t.AddSource("s").AddAspNetCoreInstrumentation().AddOtlpExporter(e => Otlp(e, "/v1/traces")))
            .WithOpAmp());
        using (host)
        {
            await Kit.Until(() => server.Messages.Any(m => m.AvailableComponents is { Hash.IsEmpty: false }), "the components' hash");
            var first = server.Messages.First(m => m.AvailableComponents is not null);
            Assert.True((first.Capabilities & (ulong)AgentCapabilities.ReportsAvailableComponents) != 0);
            server.AskForComponents = true;
            await Kit.Until(() => server.Messages.Any(m => m.AvailableComponents is { Components.Count: > 0 }), "the components in full");
            var full = server.Messages.First(m => m.AvailableComponents is { Components.Count: > 0 }).AvailableComponents;
            Assert.Equal(first.AvailableComponents.Hash, full.Hash);
            Assert.Equal(c.Components, full.Components);
            await host.StopAsync(Cancel);
        }
    }

    [Theory]
    [InlineData("OpenTelemetry.Instrumentation.Http", "httpclient")]
    [InlineData("OpenTelemetry.Instrumentation.AspNetCore", "aspnetcore")]
    [InlineData("OpenTelemetry.Instrumentation.SqlClient", "sqlclient")]
    [InlineData("OpenTelemetry.Instrumentation.Runtime", "netruntime")]
    [InlineData("OpenTelemetry.Instrumentation.EntityFrameworkCore", "entityframeworkcore")]
    [InlineData("OpenTelemetry.Instrumentation.ElasticsearchClient", "elasticsearch")]
    [InlineData("Acme.Telemetry-Db_Probe", "acmetelemetrydbprobe")]
    public void AnInstrumentationIsNamedAsAutomaticInstrumentationNamesIt(string assembly, string name) =>
        Assert.Equal(name, OpAmpAgent.InstrumentationName(assembly));

    /// <summary>An exporter that isn't OTLP.</summary>
    private sealed class Kept<T> : BaseExporter<T>
        where T : class
    {
        public override ExportResult Export(in Batch<T> batch) => ExportResult.Success;
    }

    /// <summary>A reader that is read from, as a scrape is: nothing in it to stop.</summary>
    private sealed class Pulled : MetricReader
    {
    }

    /// <summary>An instrumentation of the application's own.</summary>
    private sealed class Probe
    {
    }
}
