// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Diagnostics;
using Google.Protobuf;
using Microsoft.Extensions.Logging;
using OpAmp.Proto.V1;
using OpenTelemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Logs;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Otel;
using Xunit;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>
/// Telemetry the plugin's gates don't carry: another plugin's exporters and processors, and the
/// application's own SDK under <c>OTEL_DOTNET_AUTO_SETUP_SDK=false</c>. The settings they take
/// from the gates are set aside, never answered APPLIED and left undone.
/// </summary>
public class OwnExportersTests
{
    private static AgentRemoteConfig Config(string body) => new()
    {
        ConfigHash = ByteString.CopyFromUtf8(body),
        Config = new AgentConfigMap
        {
            ConfigMap = { ["elastic"] = new AgentConfigObject { Body = ByteString.CopyFromUtf8(body), ContentType = "application/json" } },
        },
    };

    private static Agent Prepared(Dictionary<string, string?>? env = null)
    {
        env ??= [];
        env.TryAdd("OTEL_OPAMP_ENDPOINT", "http://127.0.0.1:1/v1/opamp");
        env.TryAdd("OTEL_OPAMP_STATE_DIR", "none");
        return Agent.Prepare(n => env.GetValueOrDefault(n), (_, _) => { })!;
    }

    private static string Effective(Agent agent) => agent.Effective.ConfigMap.ConfigMap["elastic"].Body.ToStringUtf8();

    private static string[] SetAside(Agent agent) =>
        agent.Effective.ConfigMap.ConfigMap.TryGetValue("elastic.set_aside", out var e)
            ? System.Text.Json.JsonDocument.Parse(e.Body.ToStringUtf8()).RootElement.GetProperty("unsupported").EnumerateArray().Select(u => u.GetString()!).ToArray()
            : [];

    /// <summary>An exporter of another plugin's: OTLP, but built by it, not through the plugin's options hook.</summary>
    private static OtlpExporterOptions OwnOptions() => new() { Endpoint = new Uri("http://127.0.0.1:1/v1/traces"), Protocol = OtlpExportProtocol.HttpProtobuf };

    /// <summary>
    /// The tracer provider as automatic instrumentation builds it, the plugin's hooks called on it,
    /// and with <paramref name="more"/> from another plugin.
    /// </summary>
    private static TracerProvider Tracing(Agent agent, Action<TracerProviderBuilder> more) =>
        Sdk.CreateTracerProviderBuilder().AddSource("own-exporters")
            .AddOtlpExporter(o =>
            {
                // An endpoint with a path of its own: the plugin's gate is given its signal.
                o.Endpoint = new Uri("http://127.0.0.1:1/ingest");
                o.Protocol = OtlpExportProtocol.HttpProtobuf;
                agent.Gate(o, Signal.Traces);
            })
            .Let(agent.WrapSampler).Let(more).Build();

    [Fact]
    public void AnotherPluginsSpanExporterSetsTheTracesGatesSettingsAside()
    {
        var agent = Prepared();
        using var tracing = Tracing(agent, b => b.AddProcessor(new BatchActivityExportProcessor(new OtlpTraceExporter(OwnOptions()))));
        agent.Built(tracing);
        try
        {
            agent.Start([]);
            var status = agent.RemoteConfig(Config("{\"send_traces\":\"false\",\"drop_attributes\":\"secret\",\"sampling_rate\":\"0.5\"}"));
            Assert.Equal(RemoteConfigStatuses.Applied, status.Status);
            Assert.True(agent.Controls.SendTraces); // not carried out: the other exporter sends
            Assert.Equal(["drop_attributes", "send_traces"], SetAside(agent).Order());
            Assert.DoesNotContain("send_traces", Effective(agent));
            Assert.Contains("sampling_rate", Effective(agent)); // the sampler is the plugin's still
            Assert.All(agent.Sdk.Components!.Components["exporters"].SubComponentMap["traces"].SubComponentMap.Values,
                e => Assert.False(e.Metadata.Single(m => m.Key == "gated").Value.BoolValue));
        }
        finally
        {
            agent.Stop(TimeSpan.Zero);
        }
    }

    // The plugin's own gated exporter, alone, is no reason: its signal is given, whatever the endpoint's path.
    [Fact]
    public void ThePluginsOwnGatedExporterAloneSetsNothingAside()
    {
        var agent = Prepared();
        using var tracing = Tracing(agent, _ => { });
        agent.Built(tracing);
        try
        {
            agent.Start([]);
            Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Config("{\"send_traces\":\"false\",\"drop_attributes\":\"secret\"}")).Status);
            Assert.False(agent.Controls.SendTraces);
            Assert.Empty(SetAside(agent));
        }
        finally
        {
            agent.Stop(TimeSpan.Zero);
        }
    }

    // Automatic instrumentation builds a logger provider for each logger factory, once the
    // application creates one: after the agent started, and after a config may be in force. The
    // plugin's processor in it has the agent check again on its first record, and the settings the
    // other processor takes are set aside, the effective config with them.
    [Fact]
    public async Task AnotherPluginsLogProcessorSetsTheLogsGatesSettingsAsideOnceItsProviderIsBuilt()
    {
        var agent = Prepared();
        try
        {
            agent.Start([]);
            Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Config("{\"send_logs\":\"false\",\"send_logs_level\":\"warn\"}")).Status);
            Assert.False(agent.Controls.SendLogs);
            Assert.Empty(SetAside(agent));

            var logs = new Own<LogRecord>();
            using (var factory = LoggerFactory.Create(b => b.AddOpenTelemetry(o =>
                   {
                       agent.Watch(o); // the plugin's hook, called first
                       o.AddOtlpExporter(e => agent.Gate(e, Signal.Logs));
                       o.AddProcessor(new SimpleLogRecordExportProcessor(logs)); // another plugin's
                   })))
            {
                factory.CreateLogger("app").LogWarning("one");
                await Until(() => agent.Controls.SendLogs, "send_logs set aside");
                Assert.Equal(["send_logs", "send_logs_level"], SetAside(agent).Order());
                Assert.DoesNotContain("send_logs", Effective(agent));
                Assert.Equal(0u, agent.Controls.LogsFloor);
                // And the next config is decided under that host.
                Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Config("{\"send_logs_per_second\":\"1\"}")).Status);
                Assert.Equal(["send_logs_per_second"], SetAside(agent));
            }

            // A provider disposed exports nothing more: the logs gate holds every log record exported again.
            Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Config("{\"send_logs_per_second\":\"1\"}")).Status);
            Assert.Empty(SetAside(agent));
        }
        finally
        {
            agent.Stop(TimeSpan.Zero);
        }
    }

    // With OTEL_DOTNET_AUTO_SETUP_SDK=false the application builds its tracer and meter providers
    // itself: their exporters and sampler aren't the plugin's. The logs still are.
    [Fact]
    public void WhenTheApplicationSetsUpItsOwnSdkTracesAndMetricsAreSetAside()
    {
        var agent = Prepared(new() { ["OTEL_DOTNET_AUTO_SETUP_SDK"] = "false" });
        var host = agent.HostNow(providersBuilt: true);
        foreach (var key in new[] { "send_traces", "send_metrics", "sampling_rate", "sampling_rules", "sampling_traces_per_second", "drop_attributes", "mask_attributes" })
        {
            Assert.DoesNotContain(key, host.Live);
        }

        foreach (var key in new[] { "send_logs", "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second" })
        {
            Assert.Contains(key, host.Live);
        }

        Assert.Equal(["logs"], agent.Inventory().Exporters.Select(e => e.Signal).Distinct());
        // Traces exported nowhere by automatic instrumentation say nothing of the application's own.
        var none = Prepared(new() { ["OTEL_DOTNET_AUTO_SETUP_SDK"] = "false", ["OTEL_TRACES_EXPORTER"] = "none" });
        Assert.DoesNotContain("drop_attributes", none.HostNow(providersBuilt: true).Live);
        Assert.Contains("drop_attributes", Prepared(new() { ["OTEL_TRACES_EXPORTER"] = "none" }).HostNow(providersBuilt: true).Live);
    }

    private static async Task Until(Func<bool> done, string what)
    {
        var until = DateTime.UtcNow + TimeSpan.FromSeconds(10);
        while (!done())
        {
            Assert.True(DateTime.UtcNow < until, $"timed out waiting for: {what}");
            await Task.Delay(20, TestContext.Current.CancellationToken);
        }
    }

    /// <summary>An exporter of another plugin's that isn't OTLP.</summary>
    private sealed class Own<T> : BaseExporter<T>
        where T : class
    {
        public override ExportResult Export(in Batch<T> batch) => ExportResult.Success;
    }
}
