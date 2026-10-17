// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;
using Xunit;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>The inventory (docs/available-components.md): what the plugin lists of what it runs, and the client sending it.</summary>
public class InventoryTests
{
    private static Agent Prepared(Dictionary<string, string?> env)
    {
        env.TryAdd("OTEL_OPAMP_ENDPOINT", "http://127.0.0.1:1/v1/opamp");
        env.TryAdd("OTEL_OPAMP_STATE_DIR", "none");
        return Agent.Prepare(n => env.GetValueOrDefault(n), (_, _) => { })!;
    }

    /// <summary>Each signal's exporters, by name: whether gated.</summary>
    private static Dictionary<string, Dictionary<string, bool>> Exporters(Inventory inventory) =>
        inventory.Exporters.GroupBy(e => e.Signal).ToDictionary(g => g.Key, g => g.GroupBy(e => e.Name).ToDictionary(e => e.Key, e => e.All(x => x.Gated)));

    [Fact]
    public void ATypicalSetupListsItselfTheSdkEachInstrumentationAndEachExporter()
    {
        var agent = Prepared(new() { ["OTEL_METRICS_EXPORTER"] = "otlp,prometheus" });
        var inventory = agent.Inventory("1.17.0");
        Assert.Equal("Pydantic.OpAmp.AutoInstrumentation", inventory.Integration);
        Assert.Equal("0.1.0", inventory.IntegrationVersion);
        Assert.Equal("dotnet", inventory.SdkLanguage);
        // The SDK's own telemetry.sdk.version, as the resource has it.
        var resource = OpenTelemetry.Resources.ResourceBuilder.CreateDefault().Build().Attributes;
        Assert.Equal(resource.Single(a => a.Key == "telemetry.sdk.version").Value, inventory.SdkVersion);
        Assert.Equal(resource.Single(a => a.Key == "telemetry.sdk.language").Value, inventory.SdkLanguage);

        var names = inventory.Instrumentations.Select(i => i.Name).ToList();
        Assert.Contains("httpclient", names);
        Assert.Contains("aspnetcore", names);
        Assert.Contains("netruntime", names); // metrics only
        Assert.Contains("ilogger", names); // logs only
        Assert.DoesNotContain("aspnet", names); // .NET Framework's
        Assert.DoesNotContain("wcfservice", names);
        Assert.Equal(names.Distinct().Count(), names.Count); // httpclient is traces' and metrics': once
        Assert.All(inventory.Instrumentations, i => Assert.Equal("1.17.0", i.Version));

        Assert.Equal(
            new Dictionary<string, Dictionary<string, bool>>
            {
                ["traces"] = new() { ["otlp"] = true },
                ["metrics"] = new() { ["otlp"] = false, ["prometheus"] = false }, // send_metrics isn't carried out
                ["logs"] = new() { ["otlp"] = true },
            },
            Exporters(inventory));
    }

    // Each name is one deactivate_instrumentations takes: it switches that instrumentation off,
    // for every signal it has, and every instrumentation is listed.
    [Fact]
    public void EachInstrumentationIsListedByTheNameThatSwitchesItOff()
    {
        var listed = Prepared([]).Inventory().Instrumentations.Select(i => i.Name).ToList();
        foreach (var name in listed)
        {
            var vars = Env.Switches(new Settings { DeactivateInstrumentations = new Names { Names_ = { name } } });
            foreach (var signal in Env.Instrumentations.Where(kv => kv.Value.Contains(name.ToUpperInvariant())).Select(kv => kv.Key))
            {
                Assert.Equal("false", vars[$"OTEL_DOTNET_AUTO_{signal}_{name.ToUpperInvariant()}_INSTRUMENTATION_ENABLED"]);
            }

            Assert.Contains(Env.Instrumentations.Values, upstream => upstream.Contains(name.ToUpperInvariant()));
        }

        var all = Env.Instrumentations.Values.SelectMany(n => n).Select(n => n.ToLowerInvariant()).Distinct().Except(["aspnet", "wcfservice"]);
        Assert.Equal(all.Order(StringComparer.Ordinal), listed);
    }

    [Fact]
    public void WithoutAutomaticInstrumentationLoadedNoVersionIsKnown()
    {
        // The tests run without it: its version can't be told, and none is made up.
        Assert.All(Prepared([]).Inventory().Instrumentations, i => Assert.Empty(i.Version));
    }

    [Theory]
    [InlineData("none", "")] // none built
    [InlineData("otlp,otlp", "otlp:True")] // one exporter, gated
    [InlineData("otlp,none", "otlp:False")] // none beside it: the setting isn't carried out
    [InlineData("otlp,jaeger", "otlp:False")] // one it doesn't know builds nothing, but isn't gated
    [InlineData("console", "console:False")]
    [InlineData("zipkin,otlp", "otlp:False,zipkin:False")]
    [InlineData(" otlp , console ", "console:False,otlp:False")]
    public void EachTracesExporterIsListedAsTheVariableNamesIt(string variable, string listed)
    {
        var traces = Prepared(new() { ["OTEL_TRACES_EXPORTER"] = variable }).Inventory().Exporters.Where(e => e.Signal == "traces")
            .GroupBy(e => e.Name).Select(g => $"{g.Key}:{g.All(e => e.Gated)}").Order(StringComparer.Ordinal);
        Assert.Equal(listed, string.Join(",", traces));
    }

    // What isn't a signal's own exporter isn't listed: prometheus is metrics' alone.
    [Fact]
    public void AnExporterOfAnotherSignalIsNotListed()
    {
        var inventory = Prepared(new() { ["OTEL_TRACES_EXPORTER"] = "prometheus", ["OTEL_LOGS_EXPORTER"] = "zipkin" }).Inventory();
        Assert.Equal(new[] { "metrics" }, Exporters(inventory).Keys);
    }

    [Fact]
    public void ASignalSwitchedOffHasNoExporters()
    {
        var inventory = Prepared(new() { ["OTEL_DOTNET_AUTO_TRACES_ENABLED"] = "false", ["OTEL_DOTNET_AUTO_LOGS_ENABLED"] = "FALSE" }).Inventory();
        Assert.Equal(new[] { "metrics" }, Exporters(inventory).Keys);
        Assert.NotEmpty(inventory.Instrumentations); // switched off or not
    }

    // The exporters are the file's, which the plugin can't read: it lists none, rather than the
    // variables', which the SDK doesn't read then.
    [Fact]
    public void WithFileBasedConfigurationNoExporterIsListed()
    {
        var agent = Prepared(new() { ["OTEL_EXPERIMENTAL_FILE_BASED_CONFIGURATION_ENABLED"] = "true", ["OTEL_TRACES_EXPORTER"] = "console" });
        var inventory = agent.Inventory();
        Assert.Empty(inventory.Exporters);
        Assert.NotEmpty(inventory.Instrumentations);
        var c = new SdkSettings().Components(inventory, agent.HostNow(providersBuilt: true), Vectors.Now);
        Assert.DoesNotContain("deactivate_instrumentations", c.Components["settings"].SubComponentMap.Keys);
        Assert.DoesNotContain("send_traces", c.Components["settings"].SubComponentMap.Keys);
    }

    // An exporter is gated exactly where its signal's send_* is carried out.
    [Theory]
    [InlineData("otlp", "otlp", "otlp")]
    [InlineData("console", "otlp,prometheus", "otlp")]
    [InlineData("otlp,zipkin", "prometheus", "console")]
    [InlineData("none", "none", "otlp,otlp")]
    public void AnExporterIsGatedWhereItsSignalsSendingIsCarriedOut(string traces, string metrics, string logs)
    {
        var agent = Prepared(new() { ["OTEL_TRACES_EXPORTER"] = traces, ["OTEL_METRICS_EXPORTER"] = metrics, ["OTEL_LOGS_EXPORTER"] = logs });
        var live = agent.HostNow(providersBuilt: true).Live;
        Assert.All(agent.Inventory().Exporters, e => Assert.Equal(live.Contains($"send_{e.Signal}"), e.Gated));
    }

    // Started, the agent's components are the layer's of its inventory under the host it runs by:
    // the settings it lists are the ones it carries out. The client sends their hash first, and
    // them in full when the server asks.
    [Fact]
    public async Task TheClientSendsTheComponentsTheSettingsTheHostCarriesOut()
    {
        await using var server = new Server();
        var agent = Prepared(new()
        {
            ["OTEL_OPAMP_ENDPOINT"] = server.Endpoint,
            ["OTEL_OPAMP_HEARTBEAT_INTERVAL"] = "100ms",
            ["OTEL_TRACES_EXPORTER"] = "console",
        });
        try
        {
            agent.Start([new("service.name", "inventory-test")]);
            var components = agent.Sdk.Components!;
            var settings = components.Components["settings"].SubComponentMap;
            Assert.DoesNotContain("send_traces", settings.Keys); // console: not carried out
            Assert.Equal("at once", settings["send_logs"].Metadata.Single(m => m.Key == "applies").Value.StringValue);
            Assert.Equal("next start", settings["deactivate_instrumentations"].Metadata.Single(m => m.Key == "applies").Value.StringValue);
            Assert.Equal(new SdkSettings().Components(agent.Inventory(), agent.HostNow(providersBuilt: true), Vectors.Now).Components["settings"], components.Components["settings"]);
            Assert.False(components.Components["exporters"].SubComponentMap["traces"].SubComponentMap["console"].Metadata.Single().Value.BoolValue);
            Assert.Contains("httpclient", components.Components["instrumentations"].SubComponentMap.Keys);

            await Until(() => server.Messages.Any(m => m.AvailableComponents is { Hash.IsEmpty: false }), "the components' hash");
            var first = server.Messages.First(m => m.AvailableComponents is not null);
            Assert.True((first.Capabilities & (ulong)AgentCapabilities.ReportsAvailableComponents) != 0);
            server.Ask = true;
            await Until(() => server.Messages.Any(m => m.AvailableComponents is { Components.Count: > 0 }), "the components in full");
            var full = server.Messages.First(m => m.AvailableComponents is { Components.Count: > 0 }).AvailableComponents;
            Assert.Equal(first.AvailableComponents.Hash, full.Hash);
            Assert.Equal(components.Components, full.Components);
        }
        finally
        {
            agent.Stop(TimeSpan.FromSeconds(10));
        }
    }

    private static async Task Until(Func<bool> done, string what)
    {
        var until = DateTime.UtcNow + TimeSpan.FromSeconds(20);
        while (!done())
        {
            Assert.True(DateTime.UtcNow < until, $"never: {what}");
            await Task.Delay(20, TestContext.Current.CancellationToken);
        }
    }

    /// <summary>An OpAMP server over plain HTTP: it keeps every message, and asks for the available components once told to.</summary>
    private sealed class Server : IAsyncDisposable
    {
        private readonly System.Net.HttpListener listener = new();
        private readonly Task serving;

        public Server()
        {
            var l = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, 0);
            l.Start();
            var port = ((System.Net.IPEndPoint)l.LocalEndpoint).Port;
            l.Stop();
            this.Endpoint = $"http://127.0.0.1:{port}/v1/opamp";
            this.listener.Prefixes.Add($"http://127.0.0.1:{port}/");
            this.listener.Start();
            this.serving = Task.Run(this.Serve);
        }

        public string Endpoint { get; }

        public volatile bool Ask;

        public ConcurrentQueue<AgentToServer> Messages { get; } = new();

        public async ValueTask DisposeAsync()
        {
            this.listener.Close();
            await this.serving;
        }

        private async Task Serve()
        {
            while (this.listener.IsListening)
            {
                System.Net.HttpListenerContext ctx;
                try
                {
                    ctx = await this.listener.GetContextAsync();
                }
                catch (Exception)
                {
                    return;
                }

                using var buf = new MemoryStream();
                await ctx.Request.InputStream.CopyToAsync(buf);
                var m = AgentToServer.Parser.ParseFrom(buf.ToArray());
                this.Messages.Enqueue(m);
                var reply = new ServerToAgent { InstanceUid = m.InstanceUid };
                if (this.Ask)
                {
                    reply.Flags = (ulong)ServerToAgentFlags.ReportAvailableComponents;
                }

                var bytes = reply.ToByteArray();
                ctx.Response.ContentType = "application/x-protobuf";
                ctx.Response.ContentLength64 = bytes.Length;
                await ctx.Response.OutputStream.WriteAsync(bytes);
                ctx.Response.Close();
            }
        }
    }
}
