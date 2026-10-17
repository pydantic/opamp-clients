// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Diagnostics;
using System.Diagnostics.Metrics;
using System.Net;
using System.Net.Http.Headers;
using Google.Protobuf;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Diagnostics.HealthChecks;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;
using OpAmp.Proto.V1;
using OpenTelemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Logs;
using OpenTelemetry.Metrics;
using OpenTelemetry.Resources;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Otel;
using Pydantic.OpAmp.Tests;
using Xunit;

namespace Pydantic.OpAmp.Extensions.Hosting.Tests;

public class HostingTests
{
    private static CancellationToken Cancel => TestContext.Current.CancellationToken;

    private static HostApplicationBuilder Builder(Dictionary<string, string?> config)
    {
        var b = Host.CreateApplicationBuilder(new HostApplicationBuilderSettings { DisableDefaults = true });
        // The user's state directory is the test's own, never the real one.
        var home = Kit.TempDir();
        foreach (var name in new[] { "XDG_STATE_HOME", "HOME", "LOCALAPPDATA" })
        {
            config.TryAdd(name, home);
        }

        b.Configuration.AddInMemoryCollection(config);
        return b;
    }

    /// <summary>Whether the package's gate is what the exporter's HTTP client comes from.</summary>
    private static bool Gated(OtlpExporterOptions o) =>
        o.HttpClientFactory.Method.DeclaringType?.FullName?.StartsWith("Pydantic.OpAmp.Otel.", StringComparison.Ordinal) == true;

    private static OtlpExporterOptions Exporter(IHost host) =>
        host.Services.GetRequiredService<IOptionsFactory<OtlpExporterOptions>>().Create(Options.DefaultName);

    [Fact]
    public async Task WithoutAnEndpointNothingChanges()
    {
        var b = Builder([]);
        b.Services.AddOpenTelemetry().WithTracing(t => t.AddSource("s").AddOtlpExporter()).WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        Assert.Null(host.Services.GetRequiredService<OpAmpAgent>().Agent);
        Assert.False(Gated(Exporter(host)));
        await host.StopAsync(Cancel);
    }

    [Fact]
    public void SettingsComeFromTheOpAmpSectionThenTheVariablesThenCode()
    {
        var b = Builder(new()
        {
            ["OpAmp:Endpoint"] = "http://section/v1/opamp",
            ["OpAmp:HeartbeatInterval"] = "2s",
            ["OTEL_OPAMP_ENDPOINT"] = "http://variable/v1/opamp",
            ["OTEL_OPAMP_HEADERS"] = "Authorization=Bearer%20key",
            ["OTEL_OPAMP_STATE_DIR"] = "/var/lib/app/opamp",
            ["OTEL_OPAMP_HEARTBEAT_INTERVAL"] = "9000",
        });
        b.Services.AddOpenTelemetry().WithOpAmp(o => o.StateDirectory = "/srv/opamp");
        using var host = b.Build();
        var o = host.Services.GetRequiredService<IOptions<OpAmpOptions>>().Value;
        Assert.Equal(new Uri("http://section/v1/opamp"), o.Endpoint);
        Assert.Equal(TimeSpan.FromSeconds(2), o.HeartbeatInterval);
        Assert.Equal("Bearer key", o.Headers["Authorization"]);
        Assert.Equal("/srv/opamp", o.StateDirectory);

        var v = Builder(new() { ["OTEL_OPAMP_ENDPOINT"] = "http://variable/v1/opamp", ["OTEL_OPAMP_HEARTBEAT_INTERVAL"] = "9000", ["OpAmp:Headers:X-Key"] = "k" });
        v.Services.AddOpenTelemetry().WithOpAmp();
        using var other = v.Build();
        var ov = other.Services.GetRequiredService<IOptions<OpAmpOptions>>().Value;
        Assert.Equal(new Uri("http://variable/v1/opamp"), ov.Endpoint);
        Assert.Equal(TimeSpan.FromSeconds(9), ov.HeartbeatInterval);
        Assert.Equal("k", ov.Headers["X-Key"]);
        Assert.Null(ov.StateDirectory);
    }

    [Fact]
    public async Task TheServersSettingsAreCarriedOutAndAnswered()
    {
        var body = Policy.Of(("head_sampling", """{"ratio":0.25}"""), ("signal_exports.logs", """{"enabled":false}"""));
        await using var server = new OpAmpServer(Kit.Config(body));
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OpAmp:StateDirectory"] = Kit.TempDir(), ["OpAmp:HeartbeatInterval"] = "100ms" });
        b.Services.AddOpenTelemetry().WithTracing(t => t.AddSource("s")).WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>();

        // The first message may still be on its way when the host has started.
        await Kit.Until(() => Capabilities(server) != 0, "the first message");
        Assert.True((Capabilities(server) & (ulong)AgentCapabilities.AcceptsRemoteConfig) != 0);
        await Kit.Until(() => server.Messages.Any(m => m.RemoteConfigStatus is { Status: RemoteConfigStatuses.Applied } s &&
                                                       s.LastRemoteConfigHash.ToStringUtf8() == body), "the remote config answered APPLIED");
        Assert.False(agent.Controls.SendLogs);
        // In force, as reported: every control it carries out, at once.
        await Kit.Until(() => server.Messages.Any(m => Effective(m).Contains("signal_exports.logs")), "the effective config reported");
        var effective = server.Messages.Select(Effective).Last(e => e.Length > 0);
        Assert.Contains("head_sampling", effective);
        await host.StopAsync(Cancel);
    }

    private static ulong Capabilities(OpAmpServer server) => server.Messages.Select(m => m.Capabilities).FirstOrDefault(c => c != 0);

    /// <summary>The overrides of the policy a message reports in force (<see cref="Policy.Overrides(string)"/>); empty when it reports none.</summary>
    private static string Effective(AgentToServer m) =>
        m.EffectiveConfig?.ConfigMap?.ConfigMap.TryGetValue(Policy.File, out var e) == true ? Policy.Overrides(e.Body.ToStringUtf8()) : string.Empty;

    [Fact]
    public async Task StoppingTheHostTellsTheServer()
    {
        await using var server = new OpAmpServer();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint });
        b.Services.AddOpenTelemetry().WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        await Kit.Until(() => !server.Messages.IsEmpty, "the first message");
        Assert.DoesNotContain(server.Messages, m => m.AgentDisconnect != null);
        await host.StopAsync(Cancel);
        Assert.Contains(server.Messages, m => m.AgentDisconnect != null);
    }

    [Fact]
    public async Task TheApplicationsSamplerIsWrapped()
    {
        await using var server = new OpAmpServer();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint });
        b.Services.AddOpenTelemetry().WithTracing(t => t.AddSource("wrapped").SetSampler(new AlwaysOffSampler())).WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>();
        Assert.DoesNotContain("sampling_rate", agent.HostNow(providersBuilt: true).Undeclared());
        using var source = new ActivitySource("wrapped");
        using (var dropped = source.StartActivity("s"))
        {
            Assert.False(dropped!.Recorded);
        }

        agent.Controls.Sampler.Rate(1);
        using (var kept = source.StartActivity("s"))
        {
            Assert.True(kept!.Recorded);
        }

        await host.StopAsync(Cancel);
    }

    [Fact]
    public async Task ASamplerThatReplacesTheLiveOneIsReported()
    {
        await using var server = new OpAmpServer();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint });
        b.Services.AddOpenTelemetry().WithOpAmp().WithTracing(t => t.AddSource("replaced").SetSampler(new AlwaysOffSampler()));
        using var host = b.Build();
        await host.StartAsync(Cancel);
        Assert.Contains("sampling_rate", host.Services.GetRequiredService<OpAmpAgent>().HostNow(providersBuilt: true).Undeclared());
        await host.StopAsync(Cancel);

        // No tracer provider at all: nothing to sample.
        var none = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint });
        none.Services.AddOpenTelemetry().WithMetrics(m => m.AddMeter("m")).WithOpAmp();
        using var metricsOnly = none.Build();
        await metricsOnly.StartAsync(Cancel);
        Assert.Contains("sampling_rate", metricsOnly.Services.GetRequiredService<OpAmpAgent>().HostNow(providersBuilt: true).Undeclared());
        await metricsOnly.StopAsync(Cancel);
    }

    [Fact]
    public async Task EachSignalsExportsStopWhileTheServerSaysSo()
    {
        var body = Policy.Stop("traces", "logs");
        await using var server = new OpAmpServer(Kit.Config(body));
        await using var collector = new Collector();
        void Otlp(OtlpExporterOptions o, string path)
        {
            o.Endpoint = new Uri(collector.Endpoint + path);
            o.Protocol = OtlpExportProtocol.HttpProtobuf;
        }

        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OpAmp:HeartbeatInterval"] = "100ms" });
        b.Logging.AddOpenTelemetry(o => o.AddOtlpExporter(e => Otlp(e, "/v1/logs")));
        b.Services.AddOpenTelemetry()
            .WithTracing(t => t.AddSource("gated").AddOtlpExporter(e => Otlp(e, "/v1/traces")))
            .WithMetrics(m => m.AddMeter("gated").AddOtlpExporter(e => Otlp(e, "/v1/metrics")))
            .WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>();
        await Kit.Until(() => !agent.Controls.SendTraces && !agent.Controls.SendLogs, "the settings in force");

        using (new ActivitySource("gated").StartActivity("s"))
        {
        }

        using var meter = new Meter("gated");
        meter.CreateCounter<long>("c").Add(1);
        host.Services.GetRequiredService<ILogger<HostingTests>>().LogWarning("a log");
        host.Services.GetRequiredService<TracerProvider>().ForceFlush();
        host.Services.GetRequiredService<MeterProvider>().ForceFlush();
        host.Services.GetRequiredService<LoggerProvider>().ForceFlush();
        await Kit.Until(() => collector.Paths.Contains("/v1/metrics"), "the metrics exported");
        Assert.DoesNotContain("/v1/traces", collector.Paths);
        Assert.DoesNotContain("/v1/logs", collector.Paths);

        agent.Controls.Apply(new Client.Sdk.Settings());
        using (new ActivitySource("gated").StartActivity("s"))
        {
        }

        host.Services.GetRequiredService<TracerProvider>().ForceFlush();
        await Kit.Until(() => collector.Paths.Contains("/v1/traces"), "the traces exported once sent again");
        await host.StopAsync(Cancel);
    }

    [Fact]
    public async Task TheLogsBelowTheServersLevelAreNotSentAndAllAreOnceItIsLeftOut()
    {
        await using var server = new OpAmpServer(Kit.Config(Policy.Of(("log_levels", """{"level":"warn"}"""))));
        await using var collector = new Collector();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OpAmp:HeartbeatInterval"] = "100ms" });
        b.Logging.AddOpenTelemetry(o => o.AddOtlpExporter(e =>
        {
            e.Endpoint = new Uri(collector.Endpoint + "/v1/logs");
            e.Protocol = OtlpExportProtocol.HttpProtobuf;
        }));
        b.Services.AddOpenTelemetry().WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>();
        await Kit.Until(() => agent.Controls.LogsFloor == 13, "the level in force");

        var logger = host.Services.GetRequiredService<ILogger<HostingTests>>();
        var logs = host.Services.GetRequiredService<LoggerProvider>();
        string Exported() => string.Concat(collector.Exports.Where(e => e.Path == "/v1/logs").Select(e => System.Text.Encoding.UTF8.GetString(e.Body)));
        logger.LogInformation("below-the-floor");
        logger.LogWarning("at-the-floor");
        logger.LogError("above-the-floor");
        logs.ForceFlush();
        await Kit.Until(() => Exported().Contains("above-the-floor"), "the logs exported");
        Assert.Contains("at-the-floor", Exported());
        Assert.DoesNotContain("below-the-floor", Exported());

        // Below the floor only: nothing leaves.
        var before = collector.Exports.Count;
        logger.LogInformation("alone-below-the-floor");
        logs.ForceFlush();
        Assert.Equal(before, collector.Exports.Count);

        agent.Controls.Apply(new Client.Sdk.Settings());
        logger.LogInformation("sent-again");
        logs.ForceFlush();
        await Kit.Until(() => Exported().Contains("sent-again"), "every severity exported once the level is left out");
        await host.StopAsync(Cancel);
    }

    [Fact]
    public async Task EachLoggerCategoryIsHeldToItsOwnLevel()
    {
        var body = Policy.Of(("log_levels", """{"level":"warn","scopes":{"Checkout.Db*":"debug","Checkout.Noisy":"off"}}"""));
        await using var server = new OpAmpServer(Kit.Config(body));
        await using var collector = new Collector();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OpAmp:HeartbeatInterval"] = "100ms" });
        b.Logging.SetMinimumLevel(LogLevel.Trace);
        b.Logging.AddOpenTelemetry(o => o.AddOtlpExporter(e =>
        {
            e.Endpoint = new Uri(collector.Endpoint + "/v1/logs");
            e.Protocol = OtlpExportProtocol.HttpProtobuf;
        }));
        b.Services.AddOpenTelemetry().WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>();
        Assert.Contains("send_logs_level_by_scope", agent.HostNow(providersBuilt: true).Live);
        await Kit.Until(() => agent.Controls.LogLevels.Threshold("Checkout.Noisy") == LogLevels.Off, "the levels in force");

        var loggers = host.Services.GetRequiredService<ILoggerFactory>();
        loggers.CreateLogger("Checkout.Db.Pool").LogDebug("db-debug");
        loggers.CreateLogger("Checkout.Noisy").LogCritical("noisy-critical");
        loggers.CreateLogger("Checkout.Api").LogInformation("api-info");
        loggers.CreateLogger("Checkout.Api").LogError("api-error");
        host.Services.GetRequiredService<LoggerProvider>().ForceFlush();
        string Exported() => string.Concat(collector.Exports.Where(e => e.Path == "/v1/logs").Select(e => System.Text.Encoding.UTF8.GetString(e.Body)));
        await Kit.Until(() => Exported().Contains("api-error"), "the logs exported");
        Assert.Contains("db-debug", Exported());
        Assert.DoesNotContain("noisy-critical", Exported());
        Assert.DoesNotContain("api-info", Exported());
        await Kit.Until(() => server.Messages.Any(m => Effective(m).Contains("\"scopes\"")), "the levels reported in force");
        await host.StopAsync(Cancel);
    }

    [Fact]
    public async Task TheLogRateCapLetsTheFirstRecordsThrough()
    {
        await using var server = new OpAmpServer(Kit.Config(Policy.Of(("log_rate_limit", """{"records_per_second":2}"""))));
        await using var collector = new Collector();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OpAmp:HeartbeatInterval"] = "100ms" });
        // Only the test's records: the host's and the agent's own (information) would take tokens too.
        b.Logging.SetMinimumLevel(LogLevel.Warning);
        b.Logging.AddOpenTelemetry(o => o.AddOtlpExporter(e =>
        {
            e.Endpoint = new Uri(collector.Endpoint + "/v1/logs");
            e.Protocol = OtlpExportProtocol.HttpProtobuf;
        }));
        b.Services.AddOpenTelemetry().WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>();
        Assert.Contains("send_logs_per_second", agent.HostNow(providersBuilt: true).Live);
        await Kit.Until(() => agent.Controls.Policy(Signal.Logs).Cap is not null, "the cap in force");
        await Task.Delay(1100, Cancel); // a full bucket, whatever was logged before

        var logger = host.Services.GetRequiredService<ILogger<HostingTests>>();
        foreach (var i in Enumerable.Range(1, 5))
        {
            logger.LogWarning("capped-" + i);
        }

        host.Services.GetRequiredService<LoggerProvider>().ForceFlush();
        string Exported() => string.Concat(collector.Exports.Where(e => e.Path == "/v1/logs").Select(e => System.Text.Encoding.UTF8.GetString(e.Body)));
        await Kit.Until(() => Exported().Contains("capped-"), "the logs exported");
        Assert.Contains("capped-1", Exported());
        Assert.Contains("capped-2", Exported());
        Assert.DoesNotContain("capped-3", Exported());
        await host.StopAsync(Cancel);
    }

    [Fact]
    public async Task SpanAndLogAttributesAreRedactedAtExport()
    {
        var body = Policy.Of(("attribute_redaction", """{"drop":["secret.*"],"mask":["user.email"]}"""));
        await using var server = new OpAmpServer(Kit.Config(body));
        await using var collector = new Collector();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OpAmp:HeartbeatInterval"] = "100ms" });
        b.Logging.AddOpenTelemetry(o => o.AddOtlpExporter(e =>
        {
            e.Endpoint = new Uri(collector.Endpoint + "/v1/logs");
            e.Protocol = OtlpExportProtocol.HttpProtobuf;
        }));
        b.Services.AddOpenTelemetry()
            .WithTracing(t => t.AddSource("redacted").AddOtlpExporter(e =>
            {
                e.Endpoint = new Uri(collector.Endpoint + "/v1/traces");
                e.Protocol = OtlpExportProtocol.HttpProtobuf;
            }))
            .WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>();
        Assert.Contains("mask_attributes", agent.HostNow(providersBuilt: true).Live);
        await Kit.Until(() => agent.Controls.Policy(Signal.Traces).Redact is not null, "redaction in force");

        using (var source = new ActivitySource("redacted"))
        using (var a = source.StartActivity("checkout"))
        {
            a!.SetTag("secret.token", "span-secret-value");
            a.SetTag("user.email", "span-email-value");
            a.AddEvent(new ActivityEvent("e", tags: new ActivityTagsCollection { ["user.email"] = "event-email-value" }));
        }

        host.Services.GetRequiredService<ILogger<HostingTests>>().LogWarning("signed in {user.email} {secret.key}", "log-email-value", "log-secret-value");
        host.Services.GetRequiredService<TracerProvider>().ForceFlush();
        host.Services.GetRequiredService<LoggerProvider>().ForceFlush();
        string Exported(string path) => string.Concat(collector.Exports.Where(e => e.Path == path).Select(e => System.Text.Encoding.UTF8.GetString(e.Body)));
        await Kit.Until(() => Exported("/v1/traces").Contains("checkout") && Exported("/v1/logs").Contains("signed in"), "the span and the log exported");
        foreach (var secret in new[] { "span-secret-value", "span-email-value", "event-email-value", "log-email-value", "log-secret-value", "secret.token" })
        {
            Assert.DoesNotContain(secret, Exported("/v1/traces") + Exported("/v1/logs"));
        }

        Assert.Contains("[REDACTED]", Exported("/v1/traces"));
        Assert.Contains("[REDACTED]", Exported("/v1/logs"));
        Assert.Contains("{secret.key}", Exported("/v1/logs")); // the body is left alone: only attributes are redacted
        await host.StopAsync(Cancel);
    }

    [Fact]
    public async Task DebugModeEndsOnTheTimerAndTheServerIsTold()
    {
        // A window of five seconds from now, by this machine's clock.
        var until = DateTimeOffset.UtcNow.AddSeconds(5).ToString("yyyy-MM-ddTHH:mm:ss.fffZ", System.Globalization.CultureInfo.InvariantCulture);
        var body = Policy.Of(("head_sampling", """{"ratio":0.1}"""), ("debug", $$"""{"until":"{{until}}","sampling_ratio":1}"""));
        await using var server = new OpAmpServer(Kit.Config(body));
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OpAmp:HeartbeatInterval"] = "100ms" });
        b.Services.AddOpenTelemetry().WithTracing(t => t.AddSource("x")).WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>();
        foreach (var key in new[] { "debug_until", "debug_sampling_rate", "debug_send_logs_level" })
        {
            Assert.Contains(key, agent.HostNow(providersBuilt: true).Live);
        }

        await Kit.Until(() => server.Messages.Any(m => Effective(m).Contains("\"debug\"")), "debug mode reported in force");
        Assert.Equal(1, agent.Controls.Sampler.Policy.Rate);
        await Kit.Until(() => server.Messages.Any(m => Effective(m) == """{"head_sampling":{"ratio":0.1}}"""), "its end reported, the server not needed");
        Assert.Equal(0.1, agent.Controls.Sampler.Policy.Rate);
        await host.StopAsync(Cancel);
    }

    [Fact]
    public async Task WithASpanExporterTheGateIsntInFrontOfRedactionIsRefused()
    {
        await using var server = new OpAmpServer();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint });
        b.Services.AddOpenTelemetry().WithTracing(t => t.AddSource("x").AddProcessor(new SimpleActivityExportProcessor(new Kept<Activity>()))).WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>();
        Assert.DoesNotContain("drop_attributes", agent.HostNow(providersBuilt: true).Live);
        var refused = agent.Agent!.RemoteConfig(Kit.Config(Policy.Of(("attribute_redaction", """{"drop":["token"]}"""))));
        Assert.Equal(RemoteConfigStatuses.Failed, refused.Status);
        Assert.Equal("attribute_redaction: drop: this SDK cannot apply the requested override", refused.ErrorMessage);
        Assert.Null(agent.Controls.Policy(Signal.Traces).Redact);
        await host.StopAsync(Cancel);
    }

    // The application names its instrumentations in code, so the server cannot switch them off.
    [Fact]
    public async Task AnOverrideOfTheInstrumentationsIsRefused()
    {
        await using var server = new OpAmpServer();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OpAmp:StateDirectory"] = Kit.TempDir() });
        b.Services.AddOpenTelemetry().WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>();
        var refused = agent.Agent!.RemoteConfig(Kit.Config(Policy.Of(("instrumentations", """{"disabled":["httpclient"]}"""))));
        Assert.Equal(RemoteConfigStatuses.Failed, refused.Status);
        Assert.Equal("instrumentations: disabled: this SDK cannot apply the requested override", refused.ErrorMessage);
        await host.StopAsync(Cancel);
    }

    // A logs exporter the gate isn't in front of: the levels, global or per scope, can't be held.
    [Fact]
    public async Task WithALogsExporterTheGateIsntInFrontOfTheLevelsAreRefused()
    {
        await using var server = new OpAmpServer();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint });
        b.Logging.AddOpenTelemetry(o => o.AddProcessor(new SimpleLogRecordExportProcessor(new Kept<LogRecord>())));
        b.Services.AddOpenTelemetry().WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>();
        var h = agent.HostNow(providersBuilt: true);
        Assert.Contains("send_logs_level", h.Undeclared());
        Assert.DoesNotContain("send_logs_level_by_scope", h.Live);
        var refused = agent.Agent!.RemoteConfig(Kit.Config(Policy.Of(("log_levels", """{"level":"error","scopes":{"x":"off"}}"""))));
        Assert.Equal(RemoteConfigStatuses.Failed, refused.Status);
        Assert.Equal("log_levels: level: this SDK cannot apply the requested override", refused.ErrorMessage);
        Assert.False(agent.Controls.LogLevels.Active);
        await host.StopAsync(Cancel);
    }

    private sealed class Upstream : HttpMessageHandler
    {
        public int Requests { get; private set; }

        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            this.Requests++;
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.Accepted));
        }
    }

    [Fact]
    public async Task AGrpcExportIsToldApartByItsPath()
    {
        var controls = new Controls();
        controls.Apply(new Client.Sdk.Settings { SendTraces = false });
        var upstream = new Upstream();
        using var client = new HttpClient(new GateHandler(controls) { InnerHandler = upstream });
        HttpRequestMessage Export(string service) => new(HttpMethod.Post, $"http://collector:4317/opentelemetry.proto.collector.{service}/Export")
        {
            Content = new ByteArrayContent([1]) { Headers = { ContentType = new MediaTypeHeaderValue("application/grpc") } },
        };

        var dropped = await client.SendAsync(Export("trace.v1.TraceService"), Cancel);
        Assert.Equal("0", dropped.Headers.GetValues("grpc-status").Single());
        Assert.Equal(0, upstream.Requests);
        Assert.Equal(HttpStatusCode.Accepted, (await client.SendAsync(Export("logs.v1.LogsService"), Cancel)).StatusCode);
        Assert.Equal(HttpStatusCode.Accepted, (await client.SendAsync(Export("metrics.v1.MetricsService"), Cancel)).StatusCode);
        // A path that names no signal is never held back.
        Assert.Equal(HttpStatusCode.Accepted, (await client.SendAsync(new HttpRequestMessage(HttpMethod.Post, "http://collector/ingest"), Cancel)).StatusCode);
        Assert.Equal(3, upstream.Requests);
    }

    [Fact]
    public async Task AnExportersOwnHttpClientIsLeftAlone()
    {
        await using var server = new OpAmpServer();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint });
        Func<HttpClient> mine = () => new HttpClient();
        b.Services.Configure<OtlpExporterOptions>(o => o.HttpClientFactory = mine);
        b.Services.AddOpenTelemetry().WithOpAmp();
        using var host = b.Build();
        Assert.Same(mine, Exporter(host).HttpClientFactory);

        var plain = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint });
        plain.Services.AddOpenTelemetry().WithOpAmp();
        using var other = plain.Build();
        Assert.True(Gated(Exporter(other)));
    }

    /// <summary>An exporter that isn't OTLP.</summary>
    private sealed class Kept<T> : BaseExporter<T>
        where T : class
    {
        public override ExportResult Export(in Batch<T> batch) => ExportResult.Success;
    }

    /// <summary>
    /// Every signal turned off by the server, with the SDK <paramref name="setup"/> sets up. The
    /// signals in <paramref name="ungated"/> go to an exporter the gate is not in front of. Their
    /// send_* is not carried out, so the policy is refused and every signal goes on sending. With
    /// none, the policy is applied and every signal is off.
    /// </summary>
    private static async Task SendingIsRefusedFor(string[] ungated, Action<HostApplicationBuilder, string> setup)
    {
        var body = Policy.Stop("traces", "metrics", "logs");
        await using var server = new OpAmpServer(Kit.Config(body));
        await using var collector = new Collector();
        var said = new Said();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OpAmp:HeartbeatInterval"] = "100ms" });
        b.Logging.AddProvider(said);
        setup(b, collector.Endpoint);
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>();
        var signals = Enum.GetValues<Signal>().Select(s => s.ToString().ToLowerInvariant()).ToArray();
        var refusedFor = signals.FirstOrDefault(name => ungated.Contains($"send_{name}"));
        var answered = refusedFor is null ? RemoteConfigStatuses.Applied : RemoteConfigStatuses.Failed;
        await Kit.Until(() => server.Messages.Any(m => m.RemoteConfigStatus is { } s && s.Status == answered &&
                                                       s.LastRemoteConfigHash.ToStringUtf8() == body), $"the remote config answered {answered}");
        var status = server.Messages.Select(m => m.RemoteConfigStatus).Last(s => s is not null && s.LastRemoteConfigHash.ToStringUtf8() == body);
        Assert.Equal(refusedFor is null ? string.Empty : $"signal_exports.{refusedFor}: this SDK cannot apply the requested override", status.ErrorMessage);
        await Kit.Until(() => server.Messages.Any(m => Effective(m).Length > 0), "the effective config reported");
        var effective = server.Messages.Select(Effective).Last(e => e.Length > 0);
        Assert.Equal(
            refusedFor is null ? """{"signal_exports.logs":{"enabled":false},"signal_exports.metrics":{"enabled":false},"signal_exports.traces":{"enabled":false}}""" : "{}",
            effective);
        foreach (var signal in Enum.GetValues<Signal>())
        {
            Assert.Equal(refusedFor is not null, agent.Controls.Sends(signal));
        }

        // send_logs_level is held at the logs gate: it is carried out exactly where send_logs is.
        Assert.Equal(ungated.Contains("send_logs"), agent.HostNow(providersBuilt: true).Undeclared().Contains("send_logs_level"));

        // The description lists only the signals whose export the SDK can stop.
        var description = server.Messages.Select(m => m.AgentDescription).First(d => d is not null);
        var controls = description.NonIdentifyingAttributes.Single(a => a.Key == "logfire.opamp.supported_controls").Value.ArrayValue.Values.Select(v => v.StringValue);
        foreach (var name in signals)
        {
            Assert.Equal(!ungated.Contains($"send_{name}"), controls.Contains($"signal_exports.{name}"));
        }

        // The inventory says the same: an exporter of a signal whose sending is not carried out is
        // not gated, and every other one is.
        var exporters = agent.Agent!.Components!.Components["exporters"].SubComponentMap;
        foreach (var (signal, named) in exporters)
        {
            Assert.All(named.SubComponentMap.Values, e => Assert.Equal(!ungated.Contains($"send_{signal}"), e.Metadata.Single(m => m.Key == "gated").Value.BoolValue));
        }

        Assert.All(ungated, name => Assert.Contains(name["send_".Length..], exporters.Keys));

        var notCarriedOut = said.Lines.Where(l => l.EndsWith(" is not carried out", StringComparison.Ordinal) && l.Contains("exporter that can't be stopped", StringComparison.Ordinal))
            .Select(l => l.Split(' ')[^5]).ToArray();
        Assert.Equal(ungated.Order(), notCarriedOut.Order());
        await host.StopAsync(Cancel);
    }

    private static void Otlp(OtlpExporterOptions o, string endpoint)
    {
        o.Endpoint = new Uri(endpoint);
        o.Protocol = OtlpExportProtocol.HttpProtobuf;
    }

    [Fact]
    public async Task AnOverrideOfASignalWithAnExporterThatIsntOtlpIsRefused()
    {
        await SendingIsRefusedFor(["send_logs", "send_traces"], (b, collector) =>
        {
            b.Logging.AddOpenTelemetry(o => o.AddProcessor(new SimpleLogRecordExportProcessor(new Kept<LogRecord>())));
            b.Services.AddOpenTelemetry()
                .WithTracing(t => t.AddSource("kept").AddOtlpExporter(e => Otlp(e, collector + "/v1/traces")).AddProcessor(new SimpleActivityExportProcessor(new Kept<Activity>())))
                .WithMetrics(m => m.AddMeter("kept").AddOtlpExporter(e => Otlp(e, collector + "/v1/metrics")))
                .WithOpAmp();
        });

        // A metric reader of its own, likewise.
        await SendingIsRefusedFor(["send_metrics"], (b, collector) =>
            b.Services.AddOpenTelemetry()
                .WithMetrics(m => m.AddMeter("kept").AddOtlpExporter(e => Otlp(e, collector + "/v1/metrics")).AddReader(new PeriodicExportingMetricReader(new Kept<Metric>())))
                .WithOpAmp());
    }

    /// <summary>A processor of the application's own that hands each span to exporters it keeps in a list.</summary>
    private sealed class FanOut(params BaseExporter<Activity>[] to) : BaseProcessor<Activity>
    {
        private readonly List<BaseExporter<Activity>> to = [.. to];

        public override void OnEnd(Activity data)
        {
            foreach (var e in this.to)
            {
                e.Export(new Batch<Activity>([data], 1));
            }
        }
    }

    // An exporter held in a processor's collection is found too.
    [Fact]
    public async Task AnExporterInAProcessorsListIsFound() =>
        await SendingIsRefusedFor(["send_traces"], (b, collector) =>
            b.Services.AddOpenTelemetry()
                .WithTracing(t => t.AddSource("s").AddOtlpExporter(e => Otlp(e, collector + "/v1/traces")).AddProcessor(new FanOut(new Kept<Activity>())))
                .WithOpAmp());

    // The gate tells signals by path: an endpoint with a path of its own isn't gated.
    [Fact]
    public async Task AnOtlpEndpointWithAPathOfItsOwnIsNotGated() =>
        await SendingIsRefusedFor(["send_traces"], (b, collector) =>
        {
            b.Logging.AddOpenTelemetry(o => o.AddOtlpExporter(e => Otlp(e, collector + "/v1/logs")));
            b.Services.AddOpenTelemetry()
                .WithTracing(t => t.AddSource("s").AddOtlpExporter(e => Otlp(e, collector + "/ingest/spans")))
                .WithMetrics(m => m.AddMeter("m").AddOtlpExporter(e => Otlp(e, collector + "/v1/metrics")))
                .WithOpAmp();
        });

    [Fact]
    public async Task AnOtlpExporterWithAnHttpClientFactoryOfItsOwnIsNotGated() =>
        await SendingIsRefusedFor(["send_metrics"], (b, collector) =>
            b.Services.AddOpenTelemetry()
                .WithTracing(t => t.AddSource("s").AddOtlpExporter(e => Otlp(e, collector + "/v1/traces")))
                .WithMetrics(m => m.AddMeter("m").AddOtlpExporter(e =>
                {
                    Otlp(e, collector + "/v1/metrics");
                    e.HttpClientFactory = () => new HttpClient();
                }))
                .WithOpAmp());

    // Every exporter OTLP and gated, or none at all: every signal can be stopped.
    [Fact]
    public async Task GatedOrNoExportersAreCarriedOut() =>
        await SendingIsRefusedFor([], (b, collector) =>
        {
            b.Logging.AddOpenTelemetry(o => o.AddOtlpExporter(e => Otlp(e, collector + "/v1/logs")));
            b.Services.AddOpenTelemetry()
                .WithTracing(t => t.AddSource("s").AddOtlpExporter(e => Otlp(e, collector + "/v1/traces")).AddOtlpExporter(e => Otlp(e, collector + "/v1/traces")))
                .WithOpAmp();
        });

    // A processor added to a built provider once the host has started exports past the gate. Before
    // the next policy is decided, that signal's export (here, traces) is set aside in the policy in
    // force and reported local, and the next policy, which overrides it, is refused; a check between
    // policies (here, logs; the health timer's, or Recheck) reports the effective config again. The
    // inventory no longer says those signals are gated.
    [Fact]
    public async Task AProcessorAddedToABuiltProviderSetsItsSignalAside()
    {
        var first = Policy.Stop("traces", "logs");
        var next = Policy.Of(("head_sampling", """{"ratio":1}"""), ("signal_exports.traces", """{"enabled":false}"""), ("signal_exports.logs", """{"enabled":false}"""));
        await using var server = new OpAmpServer(Kit.Config(first));
        await using var collector = new Collector();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OpAmp:HeartbeatInterval"] = "100ms" });
        b.Logging.AddOpenTelemetry(o => o.AddOtlpExporter(e => Otlp(e, collector.Endpoint + "/v1/logs")));
        b.Services.AddOpenTelemetry().WithTracing(t => t.AddSource("s").AddOtlpExporter(e => Otlp(e, collector.Endpoint + "/v1/traces"))).WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var agent = host.Services.GetRequiredService<OpAmpAgent>();
        await Kit.Until(() => Applied(server, first), "the first config answered APPLIED");
        Assert.False(agent.Controls.Sends(Signal.Traces));
        Assert.False(agent.Controls.Sends(Signal.Logs));

        host.Services.GetRequiredService<TracerProvider>().AddProcessor(new SimpleActivityExportProcessor(new Kept<Activity>()));
        var refused = agent.Agent!.RemoteConfig(Kit.Config(next));
        Assert.Equal(RemoteConfigStatuses.Failed, refused.Status);
        Assert.Equal("signal_exports.traces: this SDK cannot apply the requested override", refused.ErrorMessage);
        Assert.True(agent.Controls.Sends(Signal.Traces));
        Assert.False(agent.Controls.Sends(Signal.Logs));
        Assert.Equal(Policy.Overrides(Policy.Stop("logs")), Policy.Overrides(agent.Agent.Effective));

        host.Services.GetRequiredService<LoggerProvider>().AddProcessor(new SimpleLogRecordExportProcessor(new Kept<LogRecord>()));
        agent.Agent.Recheck();
        Assert.True(agent.Controls.Sends(Signal.Logs));
        await Kit.Until(() => server.Messages.Any(m => m.EffectiveConfig?.ConfigMap?.ConfigMap.TryGetValue(Policy.File, out var e) == true &&
                                                       Policy.Overrides(e.Body.ToStringUtf8()) == "{}"), "the effective config reported again");
        var exporters = agent.Agent.Components!.Components["exporters"].SubComponentMap;
        foreach (var signal in new[] { "traces", "logs" })
        {
            Assert.All(exporters[signal].SubComponentMap.Values, e => Assert.False(e.Metadata.Single(m => m.Key == "gated").Value.BoolValue));
        }

        await host.StopAsync(Cancel);
    }

    private static bool Applied(OpAmpServer server, string body) =>
        server.Messages.Any(m => m.RemoteConfigStatus is { Status: RemoteConfigStatuses.Applied } s && s.LastRemoteConfigHash.ToStringUtf8() == body);

    // An exporter given a client certificate or a CA builds them into its own HTTP client: the
    // gate's carries the same, and every signal can still be stopped.
    [Fact]
    public void WithAClientCertificateTheExportersAreStillGated()
    {
        var dir = Directory.CreateTempSubdirectory("opamp-tls-").FullName;
        using var key = System.Security.Cryptography.RSA.Create(2048);
        var req = new System.Security.Cryptography.X509Certificates.CertificateRequest(
            "CN=client", key, System.Security.Cryptography.HashAlgorithmName.SHA256, System.Security.Cryptography.RSASignaturePadding.Pkcs1);
        using var cert = req.CreateSelfSigned(DateTimeOffset.UtcNow.AddDays(-1), DateTimeOffset.UtcNow.AddDays(1));
        File.WriteAllText(Path.Combine(dir, "client.pem"), cert.ExportCertificatePem());
        File.WriteAllText(Path.Combine(dir, "key.pem"), key.ExportPkcs8PrivateKeyPem());
        var b = Builder(new()
        {
            ["OpAmp:Endpoint"] = "http://127.0.0.1:1/v1/opamp",
            ["OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE"] = Path.Combine(dir, "client.pem"),
            ["OTEL_EXPORTER_OTLP_CLIENT_KEY"] = Path.Combine(dir, "key.pem"),
        });
        b.Services.AddOpenTelemetry().WithOpAmp();
        using var host = b.Build();
        Assert.True(Gated(Exporter(host)));
        Assert.Equal(
            new[] { "deactivate_instrumentations", "deactivate_all_instrumentations" },
            host.Services.GetRequiredService<OpAmpAgent>().HostNow(providersBuilt: false).Undeclared());
    }

    // A signal's own TLS variables the SDK doesn't read: they stop nothing.
    [Fact]
    public void ASignalsOwnTlsVariablesAreNotTheSdks()
    {
        var b = Builder(new() { ["OpAmp:Endpoint"] = "http://127.0.0.1:1/v1/opamp", ["OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE"] = "/etc/tls/client.pem" });
        b.Services.AddOpenTelemetry().WithOpAmp();
        using var host = b.Build();
        Assert.True(Gated(Exporter(host)));
    }

    // TLS files the gate can't use leave every exporter as it is: none of the signals can be stopped.
    [Fact]
    public void WithTlsFilesItCantUseNoExportIsStopped()
    {
        var b = Builder(new() { ["OpAmp:Endpoint"] = "http://127.0.0.1:1/v1/opamp", ["OTEL_EXPORTER_OTLP_CERTIFICATE"] = "/nonexistent/ca.pem" });
        b.Services.AddOpenTelemetry().WithOpAmp();
        using var host = b.Build();
        Assert.False(Gated(Exporter(host)));
        Assert.Equal(
            new[] { "deactivate_instrumentations", "deactivate_all_instrumentations", "send_traces", "send_metrics", "send_logs", "send_logs_level" },
            host.Services.GetRequiredService<OpAmpAgent>().HostNow(providersBuilt: false).Undeclared());
    }

    [Fact]
    public async Task TheHealthChecksAreTheAgentsHealth()
    {
        await using var server = new OpAmpServer();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OpAmp:HeartbeatInterval"] = "100ms" });
        b.Services.AddHealthChecks().AddCheck("db", () => HealthCheckResult.Unhealthy("down"));
        b.Services.Configure<HealthCheckPublisherOptions>(o => o.Delay = TimeSpan.Zero);
        b.Services.AddOpenTelemetry().WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        await Kit.Until(() => server.Messages.Any(m => m.Health is { Healthy: false }), "the health checks reported");
        var health = server.Messages.Last(m => m.Health is { Healthy: false }).Health;
        Assert.Equal("Unhealthy", health.Status);
        Assert.Equal("down", health.ComponentHealthMap["db"].LastError);
        Assert.Equal("db: down", health.LastError);

        // Export health goes beside the checks, under "exporters": healthy, with the app unhealthy.
        await Kit.Until(() => server.Messages.Any(m => m.Health is { Healthy: false } h && h.ComponentHealthMap.ContainsKey("db") &&
                                                       h.ComponentHealthMap.ContainsKey("exporters")), "the checks and export health together");
        var both = server.Messages.Last(m => m.Health?.ComponentHealthMap.ContainsKey("exporters") == true).Health;
        Assert.True(both.ComponentHealthMap["exporters"].Healthy);
        Assert.Equal(["logs", "metrics", "traces"], both.ComponentHealthMap["exporters"].ComponentHealthMap.Keys.Order());
        await host.StopAsync(Cancel);
    }

    [Fact]
    public void AHealthReportDiffersOnlyByMoreThanItsTime()
    {
        HealthReport Report(HealthStatus s) => new(
            new Dictionary<string, HealthReportEntry> { ["db"] = new(s, "down", TimeSpan.Zero, null, null) }, TimeSpan.Zero);
        var a = HealthPublisher.Health(Report(HealthStatus.Degraded), 1);
        Thread.Sleep(2);
        var again = HealthPublisher.Health(Report(HealthStatus.Degraded), 1);
        Assert.True(a.Healthy); // degraded still serves
        Assert.NotEqual(a.StatusTimeUnixNano, again.StatusTimeUnixNano);
        Assert.True(HealthPublisher.Same(a, again));
        Assert.False(HealthPublisher.Same(a, HealthPublisher.Health(Report(HealthStatus.Unhealthy), 1)));
    }

    [Fact]
    public async Task TheInstanceIsNamedByItsUidAcrossRestarts()
    {
        await using var server = new OpAmpServer();
        var dir = Kit.TempDir();
        async Task<string> Run()
        {
            var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OpAmp:StateDirectory"] = dir });
            b.Services.AddOpenTelemetry()
                .ConfigureResource(r => r.AddService("app", serviceInstanceId: "per-start"))
                .WithTracing(t => t.AddSource("s"))
                .WithOpAmp();
            using var host = b.Build();
            await host.StartAsync(Cancel);
            var uid = host.Services.GetRequiredService<OpAmpAgent>().Agent!.Uid;
            var resource = host.Services.GetRequiredService<TracerProvider>().GetResource();
            Assert.Equal(uid, resource.Attributes.Single(a => a.Key == "service.instance.id").Value);
            await Kit.Until(() => server.Messages.Any(m => Uuid.Format(m.InstanceUid.ToByteArray()) == uid), "a message from the UID");
            await host.StopAsync(Cancel);
            return uid;
        }

        Assert.Equal(await Run(), await Run());
    }

    [Fact]
    public async Task ByDefaultTheInstanceKeepsItsUidAcrossRestarts()
    {
        await using var server = new OpAmpServer();
        var home = Kit.TempDir();
        IHost Start(string? stateDir = null)
        {
            var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OTEL_SERVICE_NAME"] = "checkout", ["XDG_STATE_HOME"] = home, ["HOME"] = home, ["LOCALAPPDATA"] = home, ["OpAmp:StateDirectory"] = stateDir });
            b.Services.AddOpenTelemetry().WithOpAmp();
            return b.Build();
        }

        static string Uid(IHost h) => h.Services.GetRequiredService<OpAmpAgent>().Agent!.Uid;
        var first = Start();
        var other = Start();
        // Another process of it, running at once: an identity of its own.
        Assert.NotEqual(Uid(first), Uid(other));
        var uid = Uid(first);
        first.Dispose();
        other.Dispose();
        using (var again = Start())
        {
            Assert.Equal(uid, Uid(again));
        }

        // "none": each start is a new instance.
        using (var off = Start("none"))
        {
            Assert.NotEqual(uid, Uid(off));
        }
    }

    [Fact]
    public void CalledAgainItIsStillOneAgent()
    {
        var b = Builder(new() { ["OpAmp:Endpoint"] = "http://127.0.0.1:1/v1/opamp" });
        b.Services.AddOpenTelemetry().WithOpAmp().WithOpAmp(o => o.StateDirectory = "/srv/opamp");
        b.AddPydanticOpAmp();
        Assert.Single(b.Services, d => d.ImplementationType == typeof(OpAmpHostedService));
        Assert.Single(b.Services, d => d.ImplementationType == typeof(HealthPublisher));
        Assert.Single(b.Services, d => d.ServiceType == typeof(IPostConfigureOptions<OtlpExporterOptions>));
        using var host = b.Build();
        Assert.Equal("/srv/opamp", host.Services.GetRequiredService<IOptions<OpAmpOptions>>().Value.StateDirectory);
    }

    private sealed class Said : ILoggerProvider, ILogger
    {
        public System.Collections.Concurrent.ConcurrentQueue<string> Lines { get; } = new();

        public ILogger CreateLogger(string categoryName) => this;

        public IDisposable? BeginScope<TState>(TState state)
            where TState : notnull => null;

        public bool IsEnabled(LogLevel logLevel) => true;

        public void Log<TState>(LogLevel logLevel, EventId eventId, TState state, Exception? exception, Func<TState, Exception?, string> formatter) =>
            this.Lines.Enqueue($"{logLevel}: {formatter(state, exception)}");

        public void Dispose()
        {
        }
    }

    [Fact]
    public async Task WhatTheAgentSaidBeforeTheHostStartedIsLogged()
    {
        await using var server = new OpAmpServer();
        var dir = Kit.TempDir();
        File.WriteAllText(Path.Combine(dir, "sdk-settings.json"), "not a settings document");
        var said = new Said();
        var b = Builder(new() { ["OpAmp:Endpoint"] = server.Endpoint, ["OpAmp:StateDirectory"] = dir });
        b.Logging.AddProvider(said);
        b.Services.AddOpenTelemetry().WithOpAmp().WithTracing(t => t.AddSource("late").SetSampler(new AlwaysOnSampler()));
        using var host = b.Build();
        await host.StartAsync(Cancel);
        Assert.Contains(said.Lines, l => l.StartsWith("Warning: OpAMP: ", StringComparison.Ordinal) && l.Contains("saved", StringComparison.Ordinal));
        Assert.Contains(said.Lines, l => l.Contains("sampling_rate is not carried out", StringComparison.Ordinal));
        await host.StopAsync(Cancel);
    }

    [Theory]
    [InlineData("OpAmp:HeartbeatInterval", "soon", "OpAmp:HeartbeatInterval unreadable, using the default: not a duration: \"soon\"")]
    [InlineData("OpAmp:HeartbeatInterval", "1:x", "OpAmp:HeartbeatInterval unreadable, using the default: String '1:x' was not recognized as a valid TimeSpan.")]
    [InlineData("OpAmp:HeartbeatInterval", "-00:00:30", "OpAmp:HeartbeatInterval is negative, using the default: \"-00:00:30\"")]
    [InlineData("OTEL_OPAMP_HEARTBEAT_INTERVAL", "soon", "OTEL_OPAMP_HEARTBEAT_INTERVAL unreadable, using the default: not a duration: \"soon\"")]
    public async Task ABadHeartbeatIntervalIsTheDefault(string key, string value, string warning)
    {
        await using var server = new OpAmpServer();
        var said = new Said();
        var b = Builder(new() { ["OTEL_OPAMP_ENDPOINT"] = server.Endpoint, [key] = value });
        b.Logging.AddProvider(said);
        b.Services.AddOpenTelemetry().WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel); // a bad interval never fails the host
        Assert.Null(host.Services.GetRequiredService<IOptions<OpAmpOptions>>().Value.HeartbeatInterval);
        Assert.NotNull(host.Services.GetRequiredService<OpAmpAgent>().Agent); // managed, at the client's default interval
        Assert.Contains($"Warning: OpAMP: {warning}", said.Lines);
        await Kit.Until(() => !server.Messages.IsEmpty, "the first message");
        await host.StopAsync(Cancel);
    }

    [Theory]
    [InlineData("OpAmp:Endpoint", "OpAmp:Endpoint is not a URL")]
    [InlineData("OTEL_OPAMP_ENDPOINT", "OTEL_OPAMP_ENDPOINT is not a URL (Invalid URI: The format of the URI could not be determined.)")]
    public async Task AnEndpointThatIsNotAUrlLeavesTheHostRunningUnmanaged(string key, string why)
    {
        var said = new Said();
        var b = Builder(new() { [key] = "opamp server, please" });
        b.Logging.AddProvider(said);
        b.Services.AddOpenTelemetry().WithTracing(t => t.AddSource("s").AddOtlpExporter()).WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel); // the host runs
        Assert.Null(host.Services.GetRequiredService<OpAmpAgent>().Agent);
        Assert.False(Gated(Exporter(host)));
        Assert.Contains($"Error: OpAMP: the client did not start; the SDK runs unmanaged: {why}", said.Lines);
        await host.StopAsync(Cancel);
    }

    [Fact]
    public void AnEndpointTheDelegateSetsWinsOverABadOne()
    {
        var b = Builder(new() { ["OpAmp:Endpoint"] = "opamp server, please" });
        b.Services.AddOpenTelemetry().WithOpAmp(o => o.Endpoint = new Uri("http://127.0.0.1:1/v1/opamp"));
        using var host = b.Build();
        Assert.NotNull(host.Services.GetRequiredService<OpAmpAgent>().Agent);
    }

    [Fact]
    public async Task AServerThatRefusesTheAgentIsSaidOnceWithWhatToCheckThenTheConnection()
    {
        var port = Kit.FreePort();
        using var server = new HttpListener { Prefixes = { $"http://127.0.0.1:{port}/" } };
        server.Start();
        var requests = 0;
        _ = Task.Run(
            async () =>
            {
                while (server.IsListening)
                {
                    try
                    {
                        var ctx = await server.GetContextAsync();
                        using var buf = new MemoryStream();
                        await ctx.Request.InputStream.CopyToAsync(buf, Cancel);
                        if (Interlocked.Increment(ref requests) <= 3)
                        {
                            ctx.Response.StatusCode = 401;
                        }
                        else
                        {
                            var bytes = new ServerToAgent { InstanceUid = AgentToServer.Parser.ParseFrom(buf.ToArray()).InstanceUid }.ToByteArray();
                            ctx.Response.ContentType = "application/x-protobuf";
                            await ctx.Response.OutputStream.WriteAsync(bytes, Cancel);
                        }

                        ctx.Response.Close();
                    }
                    catch (Exception)
                    {
                        return;
                    }
                }
            },
            Cancel);
        var said = new Said();
        var b = Builder(new() { ["OpAmp:Endpoint"] = $"http://user:secret@127.0.0.1:{port}/v1/opamp?token=x", ["OpAmp:HeartbeatInterval"] = "100ms" });
        b.Logging.AddProvider(said);
        b.Logging.SetMinimumLevel(LogLevel.Debug);
        b.Services.AddOpenTelemetry().ConfigureResource(r => r.AddService("checkout")).WithTracing(t => t.AddSource("s")).WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        var uid = host.Services.GetRequiredService<OpAmpAgent>().Agent!.Uid;
        var where = $"http://127.0.0.1:{port}/v1/opamp";
        await Kit.Until(() => said.Lines.Any(l => l.StartsWith("Information: OpAMP: connected", StringComparison.Ordinal)), "connected, said");
        await host.StopAsync(Cancel);
        Assert.Equal(
            [
                $"Warning: OpAMP: can't reach {where} (HTTP 401): the server refused this agent's credentials: check OpAmp:Headers " +
                "(Authorization=Bearer <token>). Retrying in the background; the SDK keeps its current settings meanwhile",
                $"Information: OpAMP: connected to {where}; this SDK is managed as checkout, instance {uid}",
            ],
            said.Lines.Where(l => l.Contains("OpAMP: c", StringComparison.Ordinal)));
        Assert.Equal(2, said.Lines.Count(l => l.StartsWith($"Debug: OpAMP: still can't reach {where}", StringComparison.Ordinal)));
        Assert.DoesNotContain(said.Lines, l => l.Contains("secret", StringComparison.Ordinal) || l.Contains("token=x", StringComparison.Ordinal));
    }

    [Fact]
    public async Task AddPydanticOpAmpIsWithOpAmpOnTheApplicationsBuilder()
    {
        await using var server = new OpAmpServer();
        var b = Builder([]);
        Assert.Same(b, b.AddPydanticOpAmp(o => o.Endpoint = new Uri(server.Endpoint)));
        using var host = b.Build();
        await host.StartAsync(Cancel);
        await Kit.Until(() => !server.Messages.IsEmpty, "the first message");
        await host.StopAsync(Cancel);
        Assert.Contains(server.Messages, m => m.AgentDisconnect != null);
    }
}
