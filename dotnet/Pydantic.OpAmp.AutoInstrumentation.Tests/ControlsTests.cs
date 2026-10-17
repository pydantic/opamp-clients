// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Diagnostics;
using System.IO.Compression;
using System.Net;
using System.Net.Http.Headers;
using Google.Protobuf;
using Microsoft.Extensions.Logging;
using OpenTelemetry.Exporter;
using OpenTelemetry.Logs;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;
using Xunit;
using SamplingDecision = OpenTelemetry.Trace.SamplingDecision;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

public class ControlsTests
{
    // A trace ID whose ratio-sampling decision is known: the .NET SDK samples a trace when the
    // absolute value of its first 8 bytes (big-endian) is below rate * long.MaxValue.
    private static ActivityTraceId TraceId(byte first) =>
        ActivityTraceId.CreateFromBytes([first, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1]);

    private static SamplingDecision Sample(Sampler s, ActivityTraceId id, ActivityContext parent = default) =>
        s.ShouldSample(new SamplingParameters(parent, id, "s", ActivityKind.Internal)).Decision;

    [Fact]
    public void TheSamplerIsTheConfiguredOneUntilARateIsSet()
    {
        var s = new LiveSampler().Wrap(new AlwaysOffSampler());
        var low = TraceId(0x00);
        var high = TraceId(0x7f); // ~0.99 of long.MaxValue
        Assert.Equal(SamplingDecision.Drop, Sample(s, low));
        s.Rate(0.5);
        Assert.Equal(SamplingDecision.RecordAndSample, Sample(s, low));
        Assert.Equal(SamplingDecision.Drop, Sample(s, high));
        s.Rate(null);
        Assert.Equal(SamplingDecision.Drop, Sample(s, low));
    }

    [Fact]
    public void ARateSetAtStartHoldsOnceTheConfiguredSamplerIsKnown()
    {
        var c = new Controls();
        c.Apply(new Settings { SamplingRate = 0 });
        var s = c.Sampler.Wrap(new AlwaysOnSampler());
        Assert.Equal(SamplingDecision.Drop, Sample(s, TraceId(0)));
    }

    [Fact]
    public void AChildFollowsItsParentWhateverTheRate()
    {
        var s = new LiveSampler();
        s.Rate(0);
        var parent = new ActivityContext(TraceId(1), ActivitySpanId.CreateRandom(), ActivityTraceFlags.Recorded, isRemote: true);
        Assert.Equal(SamplingDecision.RecordAndSample, Sample(s, TraceId(1), parent));
    }

    [Fact]
    public void ASettingLeftOutIsTheSdksOwn()
    {
        var c = new Controls();
        c.Apply(new Settings { SendTraces = false, SendMetrics = false, SendLogs = false });
        Assert.False(c.SendTraces || c.SendMetrics || c.SendLogs);
        c.Apply(new Settings());
        Assert.True(c.SendTraces && c.SendMetrics && c.SendLogs);
    }

    private sealed class Server : HttpMessageHandler
    {
        public int Requests { get; private set; }

        protected override HttpResponseMessage Send(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            this.Requests++;
            return new HttpResponseMessage(HttpStatusCode.Accepted);
        }

        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken) =>
            Task.FromResult(this.Send(request, cancellationToken));
    }

    private static HttpRequestMessage Export(string mediaType) => new(HttpMethod.Post, "http://collector/v1/traces")
    {
        Content = new ByteArrayContent([1, 2, 3]) { Headers = { ContentType = new MediaTypeHeaderValue(mediaType) } },
    };

    [Fact]
    public async Task AGatedExportIsAnsweredWithoutLeavingWhileItsSignalIsntSent()
    {
        var controls = new Controls();
        var server = new Server();
        using var client = new HttpClient(new GateHandler(controls, Signal.Traces) { InnerHandler = server });

        Assert.Equal(HttpStatusCode.Accepted, (await client.SendAsync(Export("application/x-protobuf"))).StatusCode);
        controls.Apply(new Settings { SendTraces = false });
        var dropped = client.Send(Export("application/x-protobuf")); // http/protobuf exports synchronously
        Assert.Equal(HttpStatusCode.OK, dropped.StatusCode);
        Assert.False(dropped.Headers.Contains("grpc-status"));
        var grpc = await client.SendAsync(Export("application/grpc"));
        Assert.Equal(HttpStatusCode.OK, grpc.StatusCode);
        Assert.Equal("0", grpc.Headers.GetValues("grpc-status").Single()); // OK: nothing to retry
        Assert.Equal(1, server.Requests);
    }

    [Fact]
    public void TheLogsFloorIsTheSettingsAndLeftOutIsEverySeverity()
    {
        var c = new Controls();
        Assert.Equal(0u, c.LogsFloor);
        c.Apply(new Settings { SendLogsMinSeverity = 13 });
        Assert.Equal(13u, c.LogsFloor);
        c.Apply(new Settings());
        Assert.Equal(0u, c.LogsFloor);
    }

    /// <summary>send_logs_level alone, at the logs gate: the body without the records below it, and how many it keeps; null when it is sent as it is.</summary>
    private static (byte[] Body, int Kept)? Filter(byte[] body, bool grpc, uint floor, string? encoding = null)
    {
        var r = OtlpRewriter.Rewrite(body, grpc, encoding, Signal.Logs, new ExportPolicy(new LogLevels(floor, [])));
        return r.Body is null ? null : (r.Body, r.Kept);
    }

    // OTLP, written field by field with protobuf's own encoder (opentelemetry/proto/logs/v1).
    private static byte[] Message(Action<CodedOutputStream> write)
    {
        using var buffer = new MemoryStream();
        var o = new CodedOutputStream(buffer);
        write(o);
        o.Flush();
        return buffer.ToArray();
    }

    private static void Nested(CodedOutputStream o, int field, byte[] message)
    {
        o.WriteTag(field, WireFormat.WireType.LengthDelimited);
        o.WriteBytes(ByteString.CopyFrom(message));
    }

    /// <summary>A LogRecord: a time, its severity (left out when null), and its body (a string).</summary>
    private static byte[] Record(int? severity, string body) => Message(o =>
    {
        o.WriteTag(1, WireFormat.WireType.Fixed64);
        o.WriteFixed64(1_700_000_000_000_000_000);
        if (severity is { } s)
        {
            o.WriteTag(2, WireFormat.WireType.Varint);
            o.WriteEnum(s);
        }

        Nested(o, 5, Message(v =>
        {
            v.WriteTag(1, WireFormat.WireType.LengthDelimited);
            v.WriteString(body);
        }));
    });

    /// <summary>An ExportLogsServiceRequest: one resource, one scope (named, with a schema URL), these records.</summary>
    private static byte[] Logs(params byte[][] records) => Message(o => Nested(o, 1, Message(r =>
    {
        Nested(r, 1, Message(res => Nested(res, 1, Message(kv =>
        {
            kv.WriteTag(1, WireFormat.WireType.LengthDelimited);
            kv.WriteString("service.name");
        }))));
        Nested(r, 2, Message(scope =>
        {
            Nested(scope, 1, Message(n =>
            {
                n.WriteTag(1, WireFormat.WireType.LengthDelimited);
                n.WriteString("app");
            }));
            foreach (var rec in records)
            {
                Nested(scope, 2, rec);
            }

            scope.WriteTag(3, WireFormat.WireType.LengthDelimited);
            scope.WriteString("https://opentelemetry.io/schemas/1.0");
        }));
    })));

    private static byte[] Grpc(byte[] message)
    {
        var framed = new byte[message.Length + 5];
        System.Buffers.Binary.BinaryPrimitives.WriteUInt32BigEndian(framed.AsSpan(1), (uint)message.Length);
        message.CopyTo(framed, 5);
        return framed;
    }

    [Fact]
    public void TheLogsFloorDropsTheRecordsBelowItAndKeepsTheRestAsTheyAre()
    {
        var debug = Record(5, "debug");
        var info = Record(9, "info");
        var warn = Record(13, "warn");
        var error = Record(17, "error");
        var unspecified = Record(null, "unspecified");
        var zero = Record(0, "zero");
        var all = Logs(debug, info, warn, error, unspecified, zero);

        var filtered = Filter(all, grpc: false, floor: 13);
        Assert.NotNull(filtered);
        Assert.Equal(4, filtered.Value.Kept);
        // Everything but the dropped records, byte for byte: resource, scope, schema URL, records.
        Assert.Equal(Logs(warn, error, unspecified, zero), filtered.Value.Body);

        Assert.Equal(Grpc(Logs(warn, error, unspecified, zero)), Filter(Grpc(all), grpc: true, floor: 13)!.Value.Body);
        Assert.Equal(0, Filter(Logs(debug, info), grpc: false, floor: 21)!.Value.Kept);

        // Nothing dropped, no floor, or a body it can't read: sent as it is.
        Assert.Null(Filter(all, grpc: false, floor: 1));
        Assert.Null(Filter(all, grpc: false, floor: 0));
        Assert.Null(Filter([0xff, 0xff], grpc: false, floor: 13));
        var compressed = Grpc(all);
        compressed[0] = 1;
        Assert.Null(Filter(compressed, grpc: true, floor: 13));
    }

    private static byte[] Gzip(byte[] body)
    {
        using var output = new MemoryStream();
        using (var zip = new GZipStream(output, CompressionLevel.Optimal, leaveOpen: true))
        {
            zip.Write(body);
        }

        return output.ToArray();
    }

    private static byte[] Gunzip(byte[] body)
    {
        using var unzip = new GZipStream(new MemoryStream(body), CompressionMode.Decompress);
        using var output = new MemoryStream();
        unzip.CopyTo(output);
        return output.ToArray();
    }

    // A gRPC frame with the compressed flag set, around a gzip message.
    private static byte[] GrpcGzip(byte[] message)
    {
        var framed = Grpc(Gzip(message));
        framed[0] = 1;
        return framed;
    }

    [Fact]
    public void TheLogsFloorReadsGzipAndZipsTheResultTheSameWay()
    {
        var info = Record(9, "info");
        var warn = Record(13, "warn");
        var all = Logs(info, warn);

        // http/protobuf: Content-Encoding: gzip.
        var http = Filter(Gzip(all), grpc: false, floor: 13, encoding: "gzip");
        Assert.Equal(1, http!.Value.Kept);
        Assert.Equal(Logs(warn), Gunzip(http.Value.Body));

        // gRPC: the compressed flag, and grpc-encoding: gzip.
        var grpc = Filter(GrpcGzip(all), grpc: true, floor: 13, encoding: "gzip")!.Value.Body;
        Assert.Equal(1, grpc[0]);
        Assert.Equal((uint)grpc.Length - 5, System.Buffers.Binary.BinaryPrimitives.ReadUInt32BigEndian(grpc.AsSpan(1)));
        Assert.Equal(Logs(warn), Gunzip(grpc[5..]));

        // Uncompressed gRPC with grpc-encoding set (the flag is per message): read as it is.
        Assert.Equal(Grpc(Logs(warn)), Filter(Grpc(all), grpc: true, floor: 13, encoding: "gzip")!.Value.Body);
        Assert.Equal(Logs(warn), Filter(all, grpc: false, floor: 13, encoding: "identity")!.Value.Body);

        // Another codec, a compressed message with no codec named, or gzip that isn't: sent as it is.
        Assert.Null(Filter(Gzip(all), grpc: false, floor: 13, encoding: "br"));
        Assert.Null(Filter(GrpcGzip(all), grpc: true, floor: 13, encoding: "deflate"));
        Assert.Null(Filter(GrpcGzip(all), grpc: true, floor: 13));
        Assert.Null(Filter(all, grpc: false, floor: 13, encoding: "gzip"));
        Assert.Null(Filter(Gzip(all), grpc: false, floor: 13));
    }

    private sealed class Exports : HttpMessageHandler
    {
        public List<(byte[] Body, string? Encoding)> Sent { get; } = [];

        protected override HttpResponseMessage Send(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            var grpc = request.Content!.Headers.ContentType?.MediaType == "application/grpc";
            var encoding = grpc
                ? request.Headers.TryGetValues("grpc-encoding", out var g) ? g.Single() : null
                : request.Content.Headers.ContentEncoding.SingleOrDefault();
            lock (this.Sent)
            {
                this.Sent.Add((request.Content.ReadAsByteArrayAsync(cancellationToken).GetAwaiter().GetResult(), encoding));
            }

            var response = new HttpResponseMessage(HttpStatusCode.OK) { Content = new ByteArrayContent([]) };
            if (grpc)
            {
                response.Headers.TryAddWithoutValidation("grpc-status", "0");
            }

            return response;
        }

        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken) =>
            Task.FromResult(this.Send(request, cancellationToken));
    }

    [Theory]
    [InlineData(OtlpExportProtocol.HttpProtobuf)]
    [InlineData(OtlpExportProtocol.Grpc)]
    public void TheOtlpExportersGzipLogsAreHeldToTheFloor(OtlpExportProtocol protocol)
    {
        var controls = new Controls();
        controls.Apply(new Settings { SendLogsMinSeverity = 13 }); // warn
        var exports = new Exports();
        using (var factory = LoggerFactory.Create(b => b.AddOpenTelemetry(o => o.AddOtlpExporter(e =>
        {
            e.Protocol = protocol;
            e.Endpoint = new Uri(protocol == OtlpExportProtocol.Grpc ? "http://collector:4317" : "http://collector:4318/v1/logs");
            e.Compression = OtlpExportCompression.GZip;
            e.HttpClientFactory = () => new HttpClient(new GateHandler(controls) { InnerHandler = exports });
        }))))
        {
            var logger = factory.CreateLogger("app");
            logger.LogInformation("below-the-floor");
            logger.LogWarning("at-the-floor");
        }

        var (body, encoding) = Assert.Single(exports.Sent);
        Assert.Equal("gzip", encoding);
        byte[] message;
        if (protocol == OtlpExportProtocol.Grpc)
        {
            Assert.Equal(1, body[0]);
            Assert.Equal((uint)body.Length - 5, System.Buffers.Binary.BinaryPrimitives.ReadUInt32BigEndian(body.AsSpan(1)));
            message = Gunzip(body[5..]);
        }
        else
        {
            message = Gunzip(body);
        }

        var text = System.Text.Encoding.UTF8.GetString(message);
        Assert.Contains("at-the-floor", text);
        Assert.DoesNotContain("below-the-floor", text);
    }

    private sealed class Collector : HttpMessageHandler
    {
        public List<byte[]> Bodies { get; } = [];

        protected override HttpResponseMessage Send(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            this.Bodies.Add(request.Content!.ReadAsByteArrayAsync(cancellationToken).GetAwaiter().GetResult());
            return new HttpResponseMessage(HttpStatusCode.OK);
        }

        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken) =>
            Task.FromResult(this.Send(request, cancellationToken));
    }

    [Fact]
    public async Task ALiveLogsFloorHoldsTheNextExportsAndClearingItSendsEverything()
    {
        var controls = new Controls();
        var collector = new Collector();
        using var client = new HttpClient(new GateHandler(controls, Signal.Logs) { InnerHandler = collector });
        HttpRequestMessage Export(byte[] body, string mediaType = "application/x-protobuf") => new(HttpMethod.Post, "http://collector/v1/logs")
        {
            Content = new ByteArrayContent(body) { Headers = { ContentType = new MediaTypeHeaderValue(mediaType) } },
        };

        var info = Record(9, "info");
        var warn = Record(13, "warn");
        var unspecified = Record(null, "unspecified");
        var all = Logs(info, warn, unspecified);

        controls.Apply(new Settings { SendLogsMinSeverity = 13 }); // warn
        Assert.Equal(HttpStatusCode.OK, client.Send(Export(all)).StatusCode); // http/protobuf exports synchronously
        Assert.Equal(Logs(warn, unspecified), collector.Bodies.Last());
        var grpc = await client.SendAsync(Export(Grpc(all), "application/grpc"), TestContext.Current.CancellationToken);
        Assert.Equal(HttpStatusCode.OK, grpc.StatusCode);
        Assert.Equal(Grpc(Logs(warn, unspecified)), collector.Bodies.Last());

        // None left: answered without leaving, as a stopped signal's export is.
        controls.Apply(new Settings { SendLogsMinSeverity = 17 });
        var none = await client.SendAsync(Export(Grpc(Logs(info, warn)), "application/grpc"), TestContext.Current.CancellationToken);
        Assert.Equal("0", none.Headers.GetValues("grpc-status").Single());
        Assert.Equal(2, collector.Bodies.Count);

        // send_logs false still stops every record, those at or above the floor too.
        controls.Apply(new Settings { SendLogs = false, SendLogsMinSeverity = 1 });
        client.Send(Export(all));
        Assert.Equal(2, collector.Bodies.Count);

        // Left out: every severity again.
        controls.Apply(new Settings());
        client.Send(Export(all));
        Assert.Equal(all, collector.Bodies.Last());
    }

    [Fact]
    public async Task OnlyALogsExportIsHeldToTheFloor()
    {
        var controls = new Controls();
        controls.Apply(new Settings { SendLogsMinSeverity = 21 });
        var collector = new Collector();
        using var client = new HttpClient(new GateHandler(controls) { InnerHandler = collector });
        var body = Logs(Record(9, "info"));
        await client.PostAsync("http://collector/v1/traces", new ByteArrayContent(body), TestContext.Current.CancellationToken);
        Assert.Equal(body, collector.Bodies.Single());
        await client.PostAsync("http://collector:4317/opentelemetry.proto.collector.logs.v1.LogsService/Export", new ByteArrayContent(Grpc(body)) { Headers = { ContentType = new MediaTypeHeaderValue("application/grpc") } }, TestContext.Current.CancellationToken);
        Assert.Single(collector.Bodies);
    }

    [Fact]
    public void TheGateKeepsTheExportersTimeout()
    {
        var options = new OtlpExporterOptions { TimeoutMilliseconds = 1234 };
        Controls.Gate(options, new Controls(), Signal.Traces);
        using var client = options.HttpClientFactory();
        Assert.Equal(TimeSpan.FromMilliseconds(1234), client.Timeout);
    }
}
