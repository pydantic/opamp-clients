// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Net.Http.Headers;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Logfire.OpAmp.Client.Sdk;
using Logfire.OpAmp.Otel;
using Logfire.OpAmp.Tests;
using Xunit;
using static Logfire.OpAmp.AutoInstrumentation.Tests.Otlp;

namespace Logfire.OpAmp.AutoInstrumentation.Tests;

/// <summary>send_logs_per_second at the logs gate, on an injected monotonic clock.</summary>
public class LogsCapTests
{
    private long now;

    private Controls Controls() => new(() => this.now);

    private static List<string> Through(Controls controls, byte[] export)
    {
        var sink = new Sink();
        using var client = new HttpClient(new GateHandler(controls, Signal.Logs) { InnerHandler = sink });
        client.Send(new HttpRequestMessage(HttpMethod.Post, "http://collector/v1/logs")
        {
            Content = new ByteArrayContent(export) { Headers = { ContentType = new MediaTypeHeaderValue("application/x-protobuf") } },
        });
        return [.. sink.Exports.SelectMany(e => ReadLogs(e.Message)).Select(r => r.Body)];
    }

    // A batch of n records, as small as a record gets: only its severity (info).
    private static byte[] Batch(long n)
    {
        var record = Message(o =>
        {
            o.WriteTag(2, WireFormat.WireType.Varint);
            o.WriteEnum(9);
        });
        return Message(o => Nested(o, 1, Message(r => Nested(r, 2, Message(s =>
        {
            for (var i = 0; i < n; i++)
            {
                Nested(s, 2, record);
            }
        })))));
    }

    private static int Records(Controls controls, long n)
    {
        var sink = new Sink();
        using var client = new HttpClient(new GateHandler(controls, Signal.Logs) { InnerHandler = sink });
        client.Send(new HttpRequestMessage(HttpMethod.Post, "http://collector/v1/logs")
        {
            Content = new ByteArrayContent(Batch(n)) { Headers = { ContentType = new MediaTypeHeaderValue("application/x-protobuf") } },
        });
        return sink.Exports.Sum(e => ReadRecords(e.Message));
    }

    private static int ReadRecords(byte[] message) =>
        Fields(message).Where(f => f.Field == 1).Sum(r => Fields(r.Bytes).Where(f => f.Field == 2).Sum(s => Fields(s.Bytes).Count(f => f.Field == 2)));

    // The logs cases of testdata/caps.json, each event a batch through the gate at its time.
    [Fact]
    public void EveryLogsCapVectorHoldsAtTheGate()
    {
        var failed = new List<string>();
        foreach (var c in Vectors.Cases("caps.json").Where(c => c.GetProperty("signal").GetString() == "logs"))
        {
            var controls = this.Controls();
            controls.Apply(Vectors.Settings(c));
            var got = new List<long>();
            foreach (var e in c.GetProperty("events").EnumerateArray())
            {
                this.now = e.GetProperty("at_ns").GetInt64();
                got.Add(Records(controls, e.GetProperty("records").GetInt64()));
            }

            if (!got.SequenceEqual(c.GetProperty("want").EnumerateArray().Select(w => w.GetInt64())))
            {
                failed.Add($"{c.GetProperty("name").GetString()}: {string.Join(",", got)}");
            }
        }

        Assert.Empty(failed);
    }

    [Fact]
    public void ABatchKeepsItsFirstRecordsAndTheLevelsDropBeforeTheCapCounts()
    {
        var controls = this.Controls();
        controls.Apply(Vectors.Settings(Policy.Of(("log_levels", """{"level":"warn"}"""), ("log_rate_limit", """{"records_per_second":3}"""))));
        var export = Logs(
            ScopeLogs("a", [Record(9, "a-info"), Record(13, "a-1"), Record(9, "a-info-2"), Record(17, "a-2")]),
            ScopeLogs("b", [Record(13, "b-3"), Record(21, "b-4"), Record(13, "b-5")]));
        // The info records take no token: the three first records past the level go, in order.
        Assert.Equal(["a-1", "a-2", "b-3"], Through(controls, export));
        Assert.Empty(Through(controls, export)); // the bucket is empty: none left, nothing leaves
        this.now = 1_000_000_000;
        Assert.Equal(["a-1", "a-2", "b-3"], Through(controls, export));
    }

    [Fact]
    public void AChangeOfRateStartsAFullBucketTheSameRateKeepsItsAndLeftOutIsUnlimited()
    {
        var controls = this.Controls();
        var two = Logs(ScopeLogs("a", [Record(9, "1"), Record(9, "2")]));
        controls.Apply(Vectors.Settings(Policy.Of(("log_rate_limit", """{"records_per_second":2}"""))));
        Assert.Equal(2, Through(controls, two).Count);
        controls.Apply(Vectors.Settings(Policy.Of(("log_rate_limit", """{"records_per_second":2}"""), ("signal_exports.logs", """{"enabled":true}"""))));
        Assert.Empty(Through(controls, two)); // the same cap: its bucket, still empty
        controls.Apply(Vectors.Settings(Policy.Of(("log_rate_limit", """{"records_per_second":1}"""))));
        Assert.Single(Through(controls, two)); // a new cap: a full bucket of 1
        controls.Apply(new Settings());
        Assert.Equal(2, Through(controls, two).Count);
        Assert.Equal(2, Through(controls, two).Count);
    }

    // Regression: the OTLP exporter's retry of a failed export (the same bytes, a new request) went
    // through the gate afresh: its records were charged to the cap again and dropped, counted filtered.
    [Fact]
    public void ARetryOfAFailedExportIsSentAsFirstDecided()
    {
        var controls = this.Controls();
        controls.Apply(Vectors.Settings(Policy.Of(("log_rate_limit", """{"records_per_second":2}"""))));
        var export = Logs(ScopeLogs("a", [Record(9, "1"), Record(9, "2"), Record(9, "3")]));
        var sink = new Sink { Answer = _ => new HttpResponseMessage(System.Net.HttpStatusCode.ServiceUnavailable) };
        using var client = new HttpClient(new GateHandler(controls, Signal.Logs) { InnerHandler = sink });
        HttpRequestMessage Attempt() => new(HttpMethod.Post, "http://collector/v1/logs")
        {
            Content = new ByteArrayContent(export) { Headers = { ContentType = new MediaTypeHeaderValue("application/x-protobuf") } },
        };
        client.Send(Attempt());
        sink.Answer = null;
        client.Send(Attempt());
        Assert.Equal(["1", "2"], ReadLogs(sink.Exports[1].Message).Select(r => r.Body));
        Assert.Equal(1UL, controls.Tally.Counts().Logs.ItemsFiltered);
        // Once sent, the same bytes again are a new export: the bucket is empty.
        Assert.Empty(Through(controls, export));
    }

    [Fact]
    public void ADroppedLogsExportTakesNoTokens()
    {
        var controls = this.Controls();
        controls.Apply(Vectors.Settings(Policy.Of(("signal_exports.logs", """{"enabled":false}"""), ("log_rate_limit", """{"records_per_second":1}"""))));
        var one = Logs(ScopeLogs("a", [Record(9, "1")]));
        Assert.Empty(Through(controls, one));
        controls.Apply(Vectors.Settings(Policy.Of(("log_rate_limit", """{"records_per_second":1}"""))));
        Assert.Single(Through(controls, one));
    }

    [Fact]
    public void ThePluginDeclaresItWhateverTheLogsExportersAre()
    {
        var both = Agent.Prepare(n => n switch { "OTEL_OPAMP_ENDPOINT" => "http://127.0.0.1:1/v1/opamp", "OTEL_LOGS_EXPORTER" => "otlp,console", _ => null }, (_, _) => { })!;
        Assert.Contains("send_logs_per_second", both.HostNow(providersBuilt: false).Live);
        Assert.Equal(RemoteConfigStatuses.Applied, both.RemoteConfig(Vectors.Config(Policy.Of(("log_rate_limit", """{"records_per_second":5}""")))).Status);
        Assert.NotNull(both.Controls.Policy(Signal.Logs).Cap);
        // The OTLP exporter is behind the gate; the console one is reported ungoverned.
        Assert.Equal(["otlp:True", "console:False"], both.Inventory().Exporters.Where(e => e.Signal == "logs").Select(e => $"{e.Name}:{e.Gated}"));
    }
}
