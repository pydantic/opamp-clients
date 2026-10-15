// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Net.Http.Headers;
using Microsoft.Extensions.Logging;
using OpAmp.Proto.V1;
using OpenTelemetry.Exporter;
using OpenTelemetry.Logs;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;
using Xunit;
using static Pydantic.OpAmp.AutoInstrumentation.Tests.Otlp;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>send_logs_level_by_scope, at the logs gate: a level per exported scope name (an ILogger category).</summary>
public class ScopeLevelsTests
{
    private static HttpRequestMessage Export(byte[] body, string mediaType = "application/x-protobuf") => new(HttpMethod.Post, "http://collector/v1/logs")
    {
        Content = new ByteArrayContent(body) { Headers = { ContentType = new MediaTypeHeaderValue(mediaType) } },
    };

    /// <summary>What a gate lets through of an export: the records the collector got (scope and body).</summary>
    private static List<(string Scope, string Body)> Through(Controls controls, byte[] export)
    {
        var sink = new Sink();
        using var client = new HttpClient(new GateHandler(controls, Signal.Logs) { InnerHandler = sink });
        client.Send(Export(export)); // http/protobuf exports synchronously
        return [.. sink.Exports.SelectMany(e => ReadLogs(e.Message)).Select(r => (r.Scope, r.Body))];
    }

    // Every case of testdata/logs.json, through the gate: the settings as the layer decides them,
    // one record of the case's scope and severity in an export of its own.
    [Fact]
    public void EveryLogsVectorHoldsAtTheGate()
    {
        var failed = new List<string>();
        foreach (var c in Vectors.Cases("logs.json"))
        {
            var controls = new Controls();
            controls.Apply(Vectors.Settings(c));
            var record = c.GetProperty("record");
            var scope = record.GetProperty("scope").GetString()!;
            var severity = record.GetProperty("severity").GetInt32();
            var kept = Through(controls, Logs(ScopeLogs(scope, [Record(severity, "r")]))).Count == 1;
            if (kept != c.GetProperty("kept").GetBoolean())
            {
                failed.Add(c.GetProperty("name").GetString()!);
            }
        }

        Assert.Empty(failed);
    }

    [Fact]
    public void EachRecordIsHeldToItsOwnScopesLevelInOneExport()
    {
        var controls = new Controls();
        controls.Apply(Vectors.Settings("{\"send_logs_level\":\"warn\",\"send_logs_level_by_scope\":\"com.acme.db*=debug, noisy.lib=off\"}"));
        var export = Logs(
            ScopeLogs("com.acme.db.pool", [Record(5, "db-debug"), Record(1, "db-trace")]),
            ScopeLogs("noisy.lib", [Record(21, "noisy-fatal"), Record(null, "noisy-unspecified")]),
            ScopeLogs("app", [Record(9, "app-info"), Record(13, "app-warn"), Record(null, "app-unspecified")]));
        Assert.Equal(
            [("com.acme.db.pool", "db-debug"), ("app", "app-warn"), ("app", "app-unspecified")],
            Through(controls, export));
    }

    // Protobuf allows a ScopeLogs' scope after its records (the .NET exporter writes it first): the
    // gate finds it wherever it is.
    [Fact]
    public void AScopeWrittenAfterItsRecordsStillNamesThem()
    {
        var controls = new Controls();
        controls.Apply(Vectors.Settings("{\"send_logs_level_by_scope\":\"noisy=off\"}"));
        var export = Logs(ScopeLogs("noisy", [Record(21, "late-scope")], scopeLast: true), ScopeLogs("quiet", [Record(21, "kept")], scopeLast: true));
        Assert.Equal([("quiet", "kept")], Through(controls, export));
        // No scope at all: the empty name, which only * matches.
        controls.Apply(Vectors.Settings("{\"send_logs_level_by_scope\":\"a*=off\"}"));
        Assert.Single(Through(controls, Logs(ScopeLogs(null, [Record(9, "unnamed")]))));
        controls.Apply(Vectors.Settings("{\"send_logs_level_by_scope\":\"*=off\"}"));
        Assert.Empty(Through(controls, Logs(ScopeLogs(null, [Record(9, "unnamed")]))));
    }

    [Fact]
    public void ScopeLevelsChangeLiveAndLeftOutEveryRecordGoesAgain()
    {
        var controls = new Controls();
        var export = Logs(ScopeLogs("noisy", [Record(9, "noisy-info")]), ScopeLogs("app", [Record(5, "app-debug")]));
        Assert.Equal(2, Through(controls, export).Count);

        controls.Apply(Vectors.Settings("{\"send_logs_level_by_scope\":\"noisy=off\"}"));
        Assert.Equal([("app", "app-debug")], Through(controls, export));
        controls.Apply(Vectors.Settings("{\"send_logs_level\":\"info\",\"send_logs_level_by_scope\":\"app=trace\"}"));
        Assert.Equal([("noisy", "noisy-info"), ("app", "app-debug")], Through(controls, export));
        controls.Apply(Vectors.Settings("{\"send_logs_level\":\"error\"}"));
        Assert.Empty(Through(controls, export)); // and none left: answered without leaving
        controls.Apply(new Settings());
        Assert.Equal(2, Through(controls, export).Count);
    }

    [Theory]
    [InlineData(false, false)]
    [InlineData(true, false)]
    [InlineData(true, true)]
    public async Task OverGrpcAndGzipTheSame(bool grpc, bool gzip)
    {
        var controls = new Controls();
        controls.Apply(Vectors.Settings("{\"send_logs_level_by_scope\":\"noisy*=off\"}"));
        var sink = new Sink();
        using var client = new HttpClient(new GateHandler(controls, Signal.Logs) { InnerHandler = sink });
        var message = Logs(ScopeLogs("noisy.lib", [Record(21, "dropped")]), ScopeLogs("app", [Record(9, "kept")]));
        var request = Export(grpc ? Grpc(message, compressed: gzip) : gzip ? Gzip(message) : message, grpc ? "application/grpc" : "application/x-protobuf");
        if (gzip)
        {
            if (grpc)
            {
                request.Headers.TryAddWithoutValidation("grpc-encoding", "gzip");
            }
            else
            {
                request.Content!.Headers.ContentEncoding.Add("gzip");
            }
        }

        await client.SendAsync(request, TestContext.Current.CancellationToken);
        Assert.Equal(["kept"], ReadLogs(Assert.Single(sink.Exports).Message).Select(r => r.Body));
    }

    // The scope a .NET log record is exported under is its ILogger category, by the OTLP exporter itself.
    [Fact]
    public void TheOtlpExportersScopeIsTheILoggerCategory()
    {
        var controls = new Controls();
        controls.Apply(Vectors.Settings("{\"send_logs_level\":\"warn\",\"send_logs_level_by_scope\":\"Checkout.Db*=debug,Checkout.Noisy=off\"}"));
        var sink = new Sink();
        using (var factory = LoggerFactory.Create(b => b.SetMinimumLevel(LogLevel.Trace).AddOpenTelemetry(o => o.AddOtlpExporter(e =>
        {
            e.Protocol = OtlpExportProtocol.HttpProtobuf;
            e.Endpoint = new Uri("http://collector:4318/v1/logs");
            e.HttpClientFactory = () => new HttpClient(new GateHandler(controls) { InnerHandler = sink });
        }))))
        {
            factory.CreateLogger("Checkout.Db.Pool").LogDebug("db-debug");
            factory.CreateLogger("Checkout.Noisy").LogCritical("noisy-critical");
            factory.CreateLogger("Checkout.Api").LogInformation("api-info");
            factory.CreateLogger("Checkout.Api").LogWarning("api-warn");
        }

        var exported = sink.Exports.SelectMany(e => ReadLogs(e.Message)).Select(r => (r.Scope, r.Body)).ToList();
        Assert.Equal([("Checkout.Db.Pool", "db-debug"), ("Checkout.Api", "api-warn")], exported);
    }

    [Fact]
    public void ALevelForEveryScopeIsTheLayersOrderLongestFirst()
    {
        var levels = new LogLevels(13, Vectors.Settings("{\"send_logs_level_by_scope\":\"a*=error,a.b*=debug,a.b=info,*=fatal\"}").SendLogsScopeLevels);
        Assert.Equal(9u, levels.Threshold("a.b"));      // exact beats prefix
        Assert.Equal(5u, levels.Threshold("a.bc"));     // the longer prefix
        Assert.Equal(17u, levels.Threshold("a.c"));     // the shorter
        Assert.Equal(21u, levels.Threshold("other"));   // * before send_logs_level
        Assert.Equal(21u, levels.Threshold(string.Empty));
        Assert.Equal(13u, new LogLevels(13, []).Threshold("x"));
    }

    // Declared where the logs gate is: every logs exporter is OTLP. Where it isn't, the layer
    // ignores it, as it does send_logs_level there.
    [Fact]
    public void ThePluginDeclaresItOnlyWhereTheLogsAreGated()
    {
        const string doc = "{\"send_logs_level_by_scope\":\"noisy=off\"}";
        var otlp = Agent.Prepare(n => n == "OTEL_OPAMP_ENDPOINT" ? "http://127.0.0.1:1/v1/opamp" : null, (_, _) => { })!;
        Assert.Contains("send_logs_level_by_scope", otlp.HostNow(providersBuilt: false).Live);
        Assert.Equal(RemoteConfigStatuses.Applied, otlp.RemoteConfig(Vectors.Config(doc)).Status);
        Assert.Equal(LogLevels.Off, otlp.Controls.LogLevels.Threshold("noisy"));
        Assert.Contains("send_logs_level_by_scope", otlp.Effective.ConfigMap.ConfigMap["elastic"].Body.ToStringUtf8());

        var console = Agent.Prepare(n => n switch
        {
            "OTEL_OPAMP_ENDPOINT" => "http://127.0.0.1:1/v1/opamp",
            "OTEL_LOGS_EXPORTER" => "console",
            _ => null,
        }, (_, _) => { })!;
        Assert.DoesNotContain("send_logs_level_by_scope", console.HostNow(providersBuilt: false).Live);
        Assert.Equal(RemoteConfigStatuses.Applied, console.RemoteConfig(Vectors.Config(doc)).Status);
        Assert.Equal(0u, console.Controls.LogLevels.Threshold("noisy")); // ignored: nothing could hold it
        Assert.DoesNotContain("send_logs_level_by_scope", console.Effective.ConfigMap.ConfigMap["elastic"].Body.ToStringUtf8());
    }
}
