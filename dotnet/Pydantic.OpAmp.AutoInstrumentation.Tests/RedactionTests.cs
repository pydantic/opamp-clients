// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Diagnostics;
using System.Net.Http.Headers;
using Microsoft.Extensions.Logging;
using OpAmp.Proto.V1;
using OpenTelemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Logs;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;
using Pydantic.OpAmp.Tests;
using Xunit;
using static Pydantic.OpAmp.AutoInstrumentation.Tests.Otlp;
using LogRecord = OpenTelemetry.Logs.LogRecord;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>drop_attributes and mask_attributes, at the traces and logs gates.</summary>
public class RedactionTests
{
    private static CancellationToken Cancel => TestContext.Current.CancellationToken;

    private static HttpRequestMessage Export(string path, byte[] body, string mediaType = "application/x-protobuf") => new(HttpMethod.Post, "http://collector" + path)
    {
        Content = new ByteArrayContent(body) { Headers = { ContentType = new MediaTypeHeaderValue(mediaType) } },
    };

    private static Sink Send(Controls controls, string path, byte[] body)
    {
        var sink = new Sink();
        using var client = new HttpClient(new GateHandler(controls) { InnerHandler = sink });
        client.Send(Export(path, body));
        return sink;
    }

    // Every case of testdata/redact.json, through the gates: a span's attributes, its event's and its
    // link's, and a log record's, each redacted as the case wants.
    [Fact]
    public void EveryRedactVectorHoldsForSpansEventsLinksAndLogs()
    {
        var failed = new List<string>();
        foreach (var c in Vectors.Cases("redact.json"))
        {
            var controls = new Controls();
            controls.Apply(Vectors.Settings(c));
            var attributes = c.GetProperty("attributes").EnumerateArray().Select(Vectors.Attribute).ToArray();
            var want = c.GetProperty("want").EnumerateArray().Select(Vectors.Attribute).ToList();

            var span = Assert.Single(ReadSpans(Assert.Single(Send(controls, "/v1/traces", Traces(Span("s", attributes, [attributes], [attributes]))).Exports).Message));
            var record = Assert.Single(ReadLogs(Assert.Single(Send(controls, "/v1/logs", Logs(ScopeLogs("app", [Record(9, "r", attributes)]))).Exports).Message));
            foreach (var (where, got) in new[] { ("span", span.Attributes), ("event", span.Events.Single()), ("link", span.Links.Single()), ("log", record.Attributes) })
            {
                if (!got.SequenceEqual(want))
                {
                    failed.Add($"{c.GetProperty("name").GetString()} ({where}): {string.Join(", ", got)}");
                }
            }
        }

        Assert.Empty(failed);
    }

    [Fact]
    public void ADropLeavesDroppedAttributesCountAndEverythingElseAsItWas()
    {
        var controls = new Controls();
        controls.Apply(Vectors.Settings(Policy.Of(("attribute_redaction", """{"drop":["secret"]}"""))));
        var export = Traces(Span("checkout", [Str("secret", "s"), Str("kept", "k")]));
        var span = Assert.Single(ReadSpans(Assert.Single(Send(controls, "/v1/traces", export).Exports).Message));
        Assert.Equal("checkout", span.Name);
        Assert.Equal(3ul, span.Dropped); // as the SDK wrote it
        Assert.Equal([Str("kept", "k")], span.Attributes);
        // Nothing to redact: byte for byte as it came.
        Assert.Equal(Traces(Span("checkout", [Str("kept", "k")])), Assert.Single(Send(controls, "/v1/traces", Traces(Span("checkout", [Str("kept", "k")]))).Exports).Message);
    }

    [Fact]
    public void RedactionChangesLiveAndLeftOutEveryAttributeGoesAgain()
    {
        var controls = new Controls();
        var export = Logs(ScopeLogs("app", [Record(9, "r", Str("user.email", "a@b.c"), Int("card.number", 4111))]));
        List<KeyValue> Exported() => Assert.Single(ReadLogs(Assert.Single(Send(controls, "/v1/logs", export).Exports).Message)).Attributes;

        Assert.Equal([Str("user.email", "a@b.c"), Int("card.number", 4111)], Exported());
        controls.Apply(Vectors.Settings(Policy.Of(("attribute_redaction", """{"mask":["user.*","card.*"]}"""))));
        Assert.Equal([Str("user.email", "[REDACTED]"), Str("card.number", "[REDACTED]")], Exported());
        controls.Apply(Vectors.Settings(Policy.Of(("attribute_redaction", """{"drop":["card.number"],"mask":["*"]}"""))));
        Assert.Equal([Str("user.email", "[REDACTED]")], Exported());
        controls.Apply(new Settings());
        Assert.Equal([Str("user.email", "a@b.c"), Int("card.number", 4111)], Exported());
    }

    [Theory]
    [InlineData(false, false)]
    [InlineData(false, true)]
    [InlineData(true, false)]
    [InlineData(true, true)]
    public async Task OverGrpcAndGzipTheSame(bool grpc, bool gzip)
    {
        var controls = new Controls();
        controls.Apply(Vectors.Settings(Policy.Of(("attribute_redaction", """{"mask":["authorization"]}"""))));
        var sink = new Sink();
        using var client = new HttpClient(new GateHandler(controls) { InnerHandler = sink });
        var message = Traces(Span("s", [Str("authorization", "Bearer x")]));
        var path = grpc ? "/opentelemetry.proto.collector.trace.v1.TraceService/Export" : "/v1/traces";
        var request = Export(path, grpc ? Grpc(message, compressed: gzip) : gzip ? Gzip(message) : message, grpc ? "application/grpc" : "application/x-protobuf");
        if (gzip && grpc)
        {
            request.Headers.TryAddWithoutValidation("grpc-encoding", "gzip");
        }
        else if (gzip)
        {
            request.Content!.Headers.ContentEncoding.Add("gzip");
        }

        await client.SendAsync(request, Cancel);
        Assert.Equal([Str("authorization", "[REDACTED]")], Assert.Single(ReadSpans(Assert.Single(sink.Exports).Message)).Attributes);
    }

    // An export the gate can't read (another codec, or not OTLP) can't be redacted: it isn't sent,
    // and is answered as if it were (nothing to retry). Without redaction, it goes as it is.
    [Fact]
    public void WhatCantBeReadForRedactionIsntSent()
    {
        var controls = new Controls();
        var message = Traces(Span("s", [Str("authorization", "Bearer x")]));
        HttpRequestMessage Brotli()
        {
            var r = Export("/v1/traces", message);
            r.Content!.Headers.ContentEncoding.Add("br");
            return r;
        }

        var sink = new Sink();
        using var client = new HttpClient(new GateHandler(controls) { InnerHandler = sink });
        client.Send(Brotli());
        client.Send(Export("/v1/logs", [0xff, 0xff]));
        Assert.Equal(2, sink.Exports.Count);

        controls.Apply(Vectors.Settings(Policy.Of(("attribute_redaction", """{"drop":["nothing.matches"]}"""))));
        Assert.Equal(System.Net.HttpStatusCode.OK, client.Send(Brotli()).StatusCode);
        client.Send(Export("/v1/logs", [0xff, 0xff]));
        var grpc = Grpc(message, compressed: true); // compressed, with no codec named
        client.Send(Export("/opentelemetry.proto.collector.trace.v1.TraceService/Export", grpc, "application/grpc"));
        Assert.Equal(2, sink.Exports.Count);
    }

    // A log record is pooled: what it carried is kept as it is exported.
    private sealed class Kept<T>(Action<T> keep) : BaseExporter<T>
        where T : class
    {
        public override ExportResult Export(in Batch<T> batch)
        {
            foreach (var item in batch)
            {
                keep(item);
            }

            return ExportResult.Success;
        }
    }

    // The gate redacts what it sends: the application's span and log record, which another
    // exporter sees, stay as they were.
    [Fact]
    public void AnotherExporterSeesTheOriginal()
    {
        var controls = new Controls();
        controls.Apply(Vectors.Settings(Policy.Of(("attribute_redaction", """{"drop":["token"],"mask":["user.email"]}"""))));
        var sink = new Sink();
        var spans = new List<Activity>();
        var records = new List<List<KeyValuePair<string, object?>>>();
        HttpClient Gated() => new(new GateHandler(controls) { InnerHandler = sink });
        void Otlp(OtlpExporterOptions e, string path)
        {
            e.Protocol = OtlpExportProtocol.HttpProtobuf;
            e.Endpoint = new Uri("http://collector:4318" + path);
            e.HttpClientFactory = Gated;
            e.ExportProcessorType = ExportProcessorType.Simple;
        }

        using (var tracing = Sdk.CreateTracerProviderBuilder().AddSource("redaction")
                   .AddOtlpExporter(e => Otlp(e, "/v1/traces"))
                   .AddProcessor(new SimpleActivityExportProcessor(new Kept<Activity>(spans.Add))).Build())
        using (var source = new ActivitySource("redaction"))
        using (var activity = source.StartActivity("checkout", ActivityKind.Internal, default(ActivityContext),
                   [new("token", "t"), new("user.email", "a@b.c")],
                   [new ActivityLink(new ActivityContext(ActivityTraceId.CreateRandom(), ActivitySpanId.CreateRandom(), ActivityTraceFlags.Recorded), new ActivityTagsCollection { ["token"] = "linked" })]))
        {
            activity!.AddEvent(new ActivityEvent("e", tags: new ActivityTagsCollection { ["user.email"] = "event@b.c", ["token"] = "e" }));
        }

        using (var factory = LoggerFactory.Create(b => b.AddOpenTelemetry(o =>
               {
                   o.AddOtlpExporter(e => Otlp(e, "/v1/logs"));
                   o.AddProcessor(new SimpleLogRecordExportProcessor(new Kept<LogRecord>(r => records.Add([.. r.Attributes ?? []]))));
               })))
        {
            factory.CreateLogger("app").LogInformation("signed in {user.email} with {token}", "a@b.c", "t");
        }

        var span = Assert.Single(ReadSpans(Assert.Single(sink.Messages("/v1/traces"))));
        Assert.Equal([Str("user.email", "[REDACTED]")], span.Attributes);
        Assert.Equal([Str("user.email", "[REDACTED]")], span.Events.Single());
        Assert.Empty(span.Links.Single());
        var record = Assert.Single(ReadLogs(Assert.Single(sink.Messages("/v1/logs"))));
        Assert.Contains(Str("user.email", "[REDACTED]"), record.Attributes);
        Assert.DoesNotContain(record.Attributes, a => a.Key == "token");

        // What the other exporter saw: the application's own, unchanged.
        var original = Assert.Single(spans);
        Assert.Equal("t", original.GetTagItem("token"));
        Assert.Equal("a@b.c", original.GetTagItem("user.email"));
        Assert.Equal("event@b.c", original.Events.Single().Tags.Single(t => t.Key == "user.email").Value);
        Assert.Equal("linked", original.Links.Single().Tags!.Single().Value);
        Assert.Contains(Assert.Single(records), a => a.Key == "token" && (string?)a.Value == "t");
    }

    [Fact]
    public void ThePluginDeclaresItWhereEverySpanAndLogRecordGoesThroughAGate()
    {
        Agent Prepare(params (string Name, string Value)[] env) => Agent.Prepare(
            n => n == "OTEL_OPAMP_ENDPOINT" ? "http://127.0.0.1:1/v1/opamp" : env.FirstOrDefault(e => e.Name == n).Value, (_, _) => { })!;

        Assert.Contains("drop_attributes", Prepare().HostNow(providersBuilt: false).Live);
        Assert.Contains("mask_attributes", Prepare(("OTEL_LOGS_EXPORTER", "none")).HostNow(providersBuilt: false).Live);
        var console = Prepare(("OTEL_LOGS_EXPORTER", "console"));
        Assert.DoesNotContain("drop_attributes", console.HostNow(providersBuilt: false).Live);
        Assert.DoesNotContain("mask_attributes", Prepare(("OTEL_TRACES_EXPORTER", "zipkin")).HostNow(providersBuilt: false).Live);
        var refused = console.RemoteConfig(Vectors.Config(Policy.Of(("attribute_redaction", """{"drop":["token"]}"""))));
        Assert.Equal(RemoteConfigStatuses.Failed, refused.Status);
        Assert.Equal("attribute_redaction: drop: this SDK cannot apply the requested override", refused.ErrorMessage);
        Assert.Null(console.Controls.Policy(Signal.Traces).Redact); // refused, not carried out by half
    }
}
