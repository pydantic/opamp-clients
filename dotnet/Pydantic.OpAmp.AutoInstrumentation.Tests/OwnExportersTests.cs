// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Diagnostics;
using Microsoft.Extensions.Logging;
using OpAmp.Proto.V1;
using OpenTelemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Logs;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Otel;
using Pydantic.OpAmp.Tests;
using Xunit;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>
/// Telemetry the plugin's gates don't carry: another plugin's exporters and processors, and the
/// application's own SDK under <c>OTEL_DOTNET_AUTO_SETUP_SDK=false</c>. Each is reported to the
/// server as ungoverned, in the inventory and export health; the policy still governs every
/// exporter the gates are in front of, so nothing is set aside.
/// </summary>
public class OwnExportersTests
{
    private const string Unsupported = "this SDK cannot apply the requested override";

    private static AgentRemoteConfig Config(string policy, string? hash = null) => Policy.Config(policy, hash);

    private static Agent Prepared(Dictionary<string, string?>? env = null)
    {
        env ??= [];
        env.TryAdd("OTEL_OPAMP_ENDPOINT", "http://127.0.0.1:1/v1/opamp");
        env.TryAdd("OTEL_OPAMP_STATE_DIR", "none");
        return Agent.Prepare(n => env.GetValueOrDefault(n), (_, _) => { })!;
    }

    /// <summary>The overrides the agent's effective config reports in force: what it set aside is local.</summary>
    private static string InForce(Agent agent) => Policy.Overrides(agent.Effective);

    /// <summary>An exporter of another plugin's: OTLP, but built by it, not through the plugin's options hook.</summary>
    private static OtlpExporterOptions OwnOptions() => new() { Endpoint = new Uri("http://127.0.0.1:2/v1/traces"), Protocol = OtlpExportProtocol.HttpProtobuf };

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

    /// <summary>The exporters the agent last listed, as signal/identity: whether gated.</summary>
    private static Dictionary<string, bool> Listed(Agent agent) =>
        agent.Sdk.Components!.Components["exporters"].SubComponentMap.SelectMany(s => s.Value.SubComponentMap.Select(e =>
            (Key: $"{s.Key}/{e.Key}", Gated: e.Value.Metadata.Single(m => m.Key == "gated").Value.BoolValue))).ToDictionary(e => e.Key, e => e.Gated);

    [Fact]
    public void AnotherPluginsSpanExporterIsReportedUngoverned()
    {
        var agent = Prepared();
        using var tracing = Tracing(agent, b => b.AddProcessor(new BatchActivityExportProcessor(new OtlpTraceExporter(OwnOptions()))));
        agent.Built(tracing);
        try
        {
            agent.Start([]);
            // The plugin's exporter by where it sends, gated; the other plugin's ungoverned.
            Assert.Equal(new Dictionary<string, bool> { ["traces/otlp@127.0.0.1:1"] = true, ["traces/otlp@127.0.0.1:2"] = false },
                Listed(agent).Where(e => e.Key.StartsWith("traces/", StringComparison.Ordinal)).ToDictionary());
            var policy = Policy.Of(("head_sampling", """{"ratio":0.5}"""), ("signal_exports.traces", """{"enabled":false}"""),
                ("attribute_redaction", """{"drop":["secret"]}"""));
            Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Config(policy)).Status);
            Assert.False(agent.Controls.SendTraces);
            Assert.Equal(Policy.Overrides(policy), InForce(agent));
            var health = agent.Sdk.LastHealth!.Exporters.ComponentHealthMap["traces"].ComponentHealthMap;
            Assert.Contains(health.Values, h => h.Status == "ungoverned" && h.Healthy);
        }
        finally
        {
            agent.Stop(TimeSpan.Zero);
        }
    }

    // The plugin's own gated exporter, alone, is governed: its signal is given, whatever the endpoint's path.
    [Fact]
    public void ThePluginsOwnGatedExporterAloneIsGoverned()
    {
        var agent = Prepared();
        using var tracing = Tracing(agent, _ => { });
        agent.Built(tracing);
        try
        {
            agent.Start([]);
            Assert.All(agent.Inventory().Exporters.Where(e => e.Signal == "traces"), e => Assert.True(e.Gated));
            var policy = Policy.Of(("signal_exports.traces", """{"enabled":false}"""), ("attribute_redaction", """{"drop":["secret"]}"""));
            Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Config(policy)).Status);
            Assert.False(agent.Controls.SendTraces);
            Assert.Equal(Policy.Overrides(policy), InForce(agent));
        }
        finally
        {
            agent.Stop(TimeSpan.Zero);
        }
    }

    // Automatic instrumentation builds a logger provider for each logger factory, once the
    // application creates one: after the agent started, and after a config may be in force. The
    // plugin's processor in it has the agent check again on its first record: the other plugin's
    // exporter is reported ungoverned, and the policy in force stays.
    [Fact]
    public async Task AnotherPluginsLogExporterIsReportedUngovernedOnceItsProviderIsBuilt()
    {
        var agent = Prepared();
        try
        {
            agent.Start([]);
            var policy = Policy.Of(("signal_exports.logs", """{"enabled":false}"""), ("log_levels", """{"level":"warn"}"""));
            Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Config(policy)).Status);
            Assert.False(agent.Controls.SendLogs);

            var logs = new Own<LogRecord>();
            using (var factory = LoggerFactory.Create(b => b.AddOpenTelemetry(o =>
                   {
                       agent.Watch(o); // the plugin's hook, called first
                       o.AddOtlpExporter(e => agent.Gate(e, Signal.Logs));
                       o.AddProcessor(new SimpleLogRecordExportProcessor(logs)); // another plugin's
                   })))
            {
                factory.CreateLogger("app").LogWarning("one");
                await Until(() => Listed(agent).ContainsKey("logs/Own"), "the other plugin's exporter listed");
                Assert.False(Listed(agent)["logs/Own"]);
                Assert.Equal("ungoverned", agent.Sdk.LastHealth!.Exporters.ComponentHealthMap["logs"].ComponentHealthMap["Own"].Status);
                Assert.False(agent.Controls.SendLogs);
                Assert.Equal(Policy.Overrides(policy), InForce(agent));
                var cap = Policy.Of(("log_rate_limit", """{"records_per_second":1}"""));
                Assert.Equal(RemoteConfigStatuses.Applied, agent.RemoteConfig(Config(cap)).Status);
            }
        }
        finally
        {
            agent.Stop(TimeSpan.Zero);
        }
    }

    // With OTEL_DOTNET_AUTO_SETUP_SDK=false the application builds its tracer and meter providers
    // itself: their exporters and sampler aren't the plugin's. Their exporters are reported as one
    // ungoverned app_provider each; the sampler's settings aren't declared. The logs are the plugin's.
    [Fact]
    public void WhenTheApplicationSetsUpItsOwnSdkItsProvidersAreReportedUngoverned()
    {
        var agent = Prepared(new() { ["OTEL_DOTNET_AUTO_SETUP_SDK"] = "false" });
        var host = agent.HostNow(providersBuilt: true);
        foreach (var key in new[] { "sampling_rate", "sampling_rules", "sampling_traces_per_second" })
        {
            Assert.DoesNotContain(key, host.Live);
        }

        foreach (var key in new[] { "send_traces", "send_metrics", "drop_attributes", "send_logs", "send_logs_level", "send_logs_per_second" })
        {
            Assert.Contains(key, host.Live);
        }

        Assert.Equal(["logs/otlp:True", "metrics/app_provider:False", "traces/app_provider:False"],
            agent.Inventory().Exporters.Select(e => $"{e.Signal}/{e.Name}:{e.Gated}").Order(StringComparer.Ordinal));
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
