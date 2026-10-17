// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Buffers.Binary;
using System.IO.Compression;
using System.Net;
using System.Text.Json;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Client.Sdk;
using Xunit;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>
/// OTLP export requests, written and read field by field with protobuf's own encoder
/// (opentelemetry/proto/{trace,logs}/v1). An attribute is OpAMP's <see cref="KeyValue"/>: OTLP's
/// common.proto, field for field.
/// </summary>
internal static class Otlp
{
    public static byte[] Message(Action<CodedOutputStream> write)
    {
        using var buffer = new MemoryStream();
        var o = new CodedOutputStream(buffer);
        write(o);
        o.Flush();
        return buffer.ToArray();
    }

    public static void Nested(CodedOutputStream o, int field, byte[] message)
    {
        o.WriteTag(field, WireFormat.WireType.LengthDelimited);
        o.WriteBytes(ByteString.CopyFrom(message));
    }

    public static KeyValue Str(string key, string value) => new() { Key = key, Value = new AnyValue { StringValue = value } };

    public static KeyValue Int(string key, long value) => new() { Key = key, Value = new AnyValue { IntValue = value } };

    /// <summary>A LogRecord: its severity (left out when null), a string body, its attributes (field 6).</summary>
    public static byte[] Record(int? severity, string body, params KeyValue[] attributes) => Message(o =>
    {
        o.WriteTag(1, WireFormat.WireType.Fixed64);
        o.WriteFixed64(1_700_000_000_000_000_000);
        if (severity is { } s)
        {
            o.WriteTag(2, WireFormat.WireType.Varint);
            o.WriteEnum(s);
        }

        Nested(o, 5, new AnyValue { StringValue = body }.ToByteArray());
        foreach (var a in attributes)
        {
            Nested(o, 6, a.ToByteArray());
        }
    });

    /// <summary>A ScopeLogs: its scope (named; none when null), these records, a schema URL; the scope after the records when <paramref name="scopeLast"/>.</summary>
    public static byte[] ScopeLogs(string? scope, IEnumerable<byte[]> records, bool scopeLast = false) => Message(o =>
    {
        void Scope()
        {
            if (scope is not null)
            {
                Nested(o, 1, Message(n =>
                {
                    n.WriteTag(1, WireFormat.WireType.LengthDelimited);
                    n.WriteString(scope);
                }));
            }
        }

        if (!scopeLast)
        {
            Scope();
        }

        foreach (var r in records)
        {
            Nested(o, 2, r);
        }

        if (scopeLast)
        {
            Scope();
        }

        o.WriteTag(3, WireFormat.WireType.LengthDelimited);
        o.WriteString("https://opentelemetry.io/schemas/1.0");
    });

    /// <summary>An ExportLogsServiceRequest: one resource (service.name) and these ScopeLogs.</summary>
    public static byte[] Logs(params byte[][] scopeLogs) => Message(o => Nested(o, 1, Message(r =>
    {
        Nested(r, 1, Message(res => Nested(res, 1, Str("service.name", "checkout").ToByteArray())));
        foreach (var s in scopeLogs)
        {
            Nested(r, 2, s);
        }
    })));

    /// <summary>A span: its name, attributes (9), events (11, each with attributes 3) and links (13, each with attributes 4).</summary>
    public static byte[] Span(string name, KeyValue[] attributes, KeyValue[][]? events = null, KeyValue[][]? links = null) => Message(o =>
    {
        o.WriteTag(1, WireFormat.WireType.LengthDelimited);
        o.WriteBytes(ByteString.CopyFrom(new byte[16]));
        o.WriteTag(5, WireFormat.WireType.LengthDelimited);
        o.WriteString(name);
        foreach (var a in attributes)
        {
            Nested(o, 9, a.ToByteArray());
        }

        o.WriteTag(10, WireFormat.WireType.Varint);
        o.WriteUInt32(3); // dropped_attributes_count, left as it was
        foreach (var e in events ?? [])
        {
            Nested(o, 11, Message(ev =>
            {
                ev.WriteTag(2, WireFormat.WireType.LengthDelimited);
                ev.WriteString("event");
                foreach (var a in e)
                {
                    Nested(ev, 3, a.ToByteArray());
                }
            }));
        }

        foreach (var l in links ?? [])
        {
            Nested(o, 13, Message(link =>
            {
                link.WriteTag(1, WireFormat.WireType.LengthDelimited);
                link.WriteBytes(ByteString.CopyFrom(new byte[16]));
                foreach (var a in l)
                {
                    Nested(link, 4, a.ToByteArray());
                }

                link.WriteTag(5, WireFormat.WireType.Varint);
                link.WriteUInt32(1); // dropped_attributes_count
            }));
        }
    });

    /// <summary>An ExportTraceServiceRequest: one resource (service.name), one scope, these spans.</summary>
    public static byte[] Traces(params byte[][] spans) => Message(o => Nested(o, 1, Message(r =>
    {
        Nested(r, 1, Message(res => Nested(res, 1, Str("service.name", "checkout").ToByteArray())));
        Nested(r, 2, Message(scope =>
        {
            Nested(scope, 1, Message(n =>
            {
                n.WriteTag(1, WireFormat.WireType.LengthDelimited);
                n.WriteString("app");
            }));
            foreach (var s in spans)
            {
                Nested(scope, 2, s);
            }
        }));
    })));

    public static byte[] Grpc(byte[] message, bool compressed = false)
    {
        var body = compressed ? Gzip(message) : message;
        var framed = new byte[body.Length + 5];
        framed[0] = compressed ? (byte)1 : (byte)0;
        BinaryPrimitives.WriteUInt32BigEndian(framed.AsSpan(1), (uint)body.Length);
        body.CopyTo(framed, 5);
        return framed;
    }

    public static byte[] Ungrpc(byte[] framed) => framed[0] == 1 ? Gunzip(framed[5..]) : framed[5..];

    public static byte[] Gzip(byte[] body)
    {
        using var output = new MemoryStream();
        using (var zip = new GZipStream(output, CompressionLevel.Optimal, leaveOpen: true))
        {
            zip.Write(body);
        }

        return output.ToArray();
    }

    public static byte[] Gunzip(byte[] body)
    {
        using var unzip = new GZipStream(new MemoryStream(body), CompressionMode.Decompress);
        using var output = new MemoryStream();
        unzip.CopyTo(output);
        return output.ToArray();
    }

    /// <summary>A message's fields, in order: number and payload (a varint's value as its bytes' number, a length-delimited one's bytes).</summary>
    public static List<(int Field, ulong Varint, byte[] Bytes)> Fields(byte[] message)
    {
        var fields = new List<(int, ulong, byte[])>();
        var input = new CodedInputStream(message);
        while (!input.IsAtEnd)
        {
            var tag = input.ReadTag();
            var field = WireFormat.GetTagFieldNumber(tag);
            switch (WireFormat.GetTagWireType(tag))
            {
                case WireFormat.WireType.Varint:
                    fields.Add((field, input.ReadUInt64(), []));
                    break;
                case WireFormat.WireType.LengthDelimited:
                    fields.Add((field, 0, input.ReadBytes().ToByteArray()));
                    break;
                case WireFormat.WireType.Fixed64:
                    fields.Add((field, input.ReadFixed64(), []));
                    break;
                case WireFormat.WireType.Fixed32:
                    fields.Add((field, input.ReadFixed32(), []));
                    break;
                default:
                    throw new InvalidDataException("a group");
            }
        }

        return fields;
    }

    private static IEnumerable<byte[]> Each(byte[] message, int field) => Fields(message).Where(f => f.Field == field).Select(f => f.Bytes);

    /// <summary>A logs export's records: each one's scope, severity, body and attributes.</summary>
    public static List<(string Scope, uint Severity, string Body, List<KeyValue> Attributes)> ReadLogs(byte[] message) =>
        [.. from resource in Each(message, 1)
            from scopeLogs in Each(resource, 2)
            let scope = Each(scopeLogs, 1).Select(s => Each(s, 1).Select(n => System.Text.Encoding.UTF8.GetString(n)).LastOrDefault()).LastOrDefault() ?? string.Empty
            from record in Each(scopeLogs, 2)
            let fields = Fields(record)
            select (scope,
                (uint)fields.Where(f => f.Field == 2).Select(f => f.Varint).LastOrDefault(),
                AnyValue.Parser.ParseFrom(Each(record, 5).Single()).StringValue,
                Each(record, 6).Select(KeyValue.Parser.ParseFrom).ToList())];

    /// <summary>A traces export's spans: each one's name, attributes, events' attributes, links' attributes and dropped_attributes_count.</summary>
    public static List<(string Name, List<KeyValue> Attributes, List<List<KeyValue>> Events, List<List<KeyValue>> Links, ulong Dropped)> ReadSpans(byte[] message) =>
        [.. from resource in Each(message, 1)
            from scopeSpans in Each(resource, 2)
            from span in Each(scopeSpans, 2)
            select (System.Text.Encoding.UTF8.GetString(Each(span, 5).Single()),
                Each(span, 9).Select(KeyValue.Parser.ParseFrom).ToList(),
                Each(span, 11).Select(e => Each(e, 3).Select(KeyValue.Parser.ParseFrom).ToList()).ToList(),
                Each(span, 13).Select(l => Each(l, 4).Select(KeyValue.Parser.ParseFrom).ToList()).ToList(),
                Fields(span).Where(f => f.Field == 10).Select(f => f.Varint).SingleOrDefault())];
}

/// <summary>A collector behind a gate: keeps each export's path and body (gRPC unframed, unzipped), and answers OK.</summary>
internal sealed class Sink : HttpMessageHandler
{
    public List<(string Path, byte[] Message)> Exports { get; } = [];

    /// <summary>What every next export is answered (null: OK).</summary>
    public Func<HttpRequestMessage, HttpResponseMessage>? Answer { get; set; }

    public List<byte[]> Messages(string signal)
    {
        lock (this.Exports)
        {
            return [.. this.Exports.Where(e => e.Path.EndsWith(signal, StringComparison.Ordinal) || e.Path.Contains(signal switch
            {
                "/v1/traces" => "TraceService",
                "/v1/logs" => "LogsService",
                _ => "MetricsService",
            }, StringComparison.Ordinal)).Select(e => e.Message)];
        }
    }

    protected override HttpResponseMessage Send(HttpRequestMessage request, CancellationToken cancellationToken)
    {
        var content = request.Content!;
        var body = content.ReadAsByteArrayAsync(cancellationToken).GetAwaiter().GetResult();
        var grpc = content.Headers.ContentType?.MediaType == "application/grpc";
        var message = grpc ? Otlp.Ungrpc(body) : content.Headers.ContentEncoding.Contains("gzip") ? Otlp.Gunzip(body) : body;
        lock (this.Exports)
        {
            this.Exports.Add((request.RequestUri!.AbsolutePath, message));
        }

        if (this.Answer is { } answer)
        {
            return answer(request);
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

/// <summary>
/// The shared test vectors (testdata/sdk-settings), and the settings the layer makes of a
/// vector's policy for a host that declares every setting.
/// </summary>
internal static class Vectors
{
    /// <summary>2026-10-14T12:00:00Z: the vectors' clock.</summary>
    public const ulong Now = 1_791_979_200_000;

    /// <summary>A host that declares every setting: each live one at once, each next-start one at the next start.</summary>
    public static Host Declaring() => new()
    {
        Live =
        {
            "debug_sampling_rate", "debug_send_logs_level", "debug_until", "drop_attributes", "mask_attributes",
            "sampling_rate", "sampling_rules", "sampling_traces_per_second", "send_logs", "send_logs_level",
            "send_logs_level_by_scope", "send_logs_per_second", "send_metrics", "send_traces",
        },
        NextStart = { "deactivate_all_instrumentations", "deactivate_instrumentations" },
    };

    /// <summary>A vectors file's cases.</summary>
    public static List<JsonElement> Cases(string file)
    {
        var dir = AppContext.BaseDirectory;
        while (!Directory.Exists(Path.Combine(dir, "testdata", "sdk-settings")))
        {
            dir = Path.GetDirectoryName(dir) ?? throw new DirectoryNotFoundException("testdata/sdk-settings");
        }

        // OPAMP_SETTINGS_VECTORS: a directory of random cases, in place of testdata's where it has the file.
        var path = Path.Combine(dir, "testdata", "sdk-settings", file);
        var other = Environment.GetEnvironmentVariable("OPAMP_SETTINGS_VECTORS");
        if (!string.IsNullOrEmpty(other) && File.Exists(Path.Combine(other, file)))
        {
            path = Path.Combine(other, file);
        }

        using var doc = JsonDocument.Parse(File.ReadAllText(path));
        return [.. doc.RootElement.GetProperty("cases").EnumerateArray().Select(c => c.Clone())];
    }

    /// <summary>A case's clock: its <c>now_unix_ms</c>, else <see cref="Now"/>.</summary>
    public static ulong NowOf(JsonElement c) => c.TryGetProperty("now_unix_ms", out var n) ? n.GetUInt64() : Now;

    /// <summary>A remote config whose only file is the policy <paramref name="document"/>.</summary>
    public static AgentRemoteConfig Config(string document) => Pydantic.OpAmp.Tests.Policy.Config(document);

    /// <summary>The settings a policy puts in force, as the layer decides them at <paramref name="now"/>.</summary>
    public static Settings Settings(string document, ulong now = Now)
    {
        using var layer = new SdkSettings();
        var d = layer.Decide(Config(document), ByteString.Empty, ByteString.Empty, Declaring(), now);
        Assert.True(d.Status.Status == RemoteConfigStatuses.Applied, $"{document}: {d.Status.ErrorMessage}");
        return d.Apply;
    }

    /// <summary>A vector's policy, as the layer decides it.</summary>
    public static Settings Settings(JsonElement c) => Settings(c.GetProperty("settings").GetRawText(), NowOf(c));

    /// <summary>A vector's attribute: a key and one of string, int, bool, double or array (of strings).</summary>
    public static KeyValue Attribute(JsonElement a)
    {
        var value = new AnyValue();
        if (a.TryGetProperty("string", out var s))
        {
            value.StringValue = s.GetString();
        }
        else if (a.TryGetProperty("int", out var i))
        {
            value.IntValue = i.GetInt64();
        }
        else if (a.TryGetProperty("bool", out var b))
        {
            value.BoolValue = b.GetBoolean();
        }
        else if (a.TryGetProperty("double", out var d))
        {
            value.DoubleValue = d.GetDouble();
        }
        else
        {
            var array = new ArrayValue();
            array.Values.AddRange(a.GetProperty("array").EnumerateArray().Select(e => new AnyValue { StringValue = e.GetString() }));
            value.ArrayValue = array;
        }

        return new KeyValue { Key = a.GetProperty("key").GetString(), Value = value };
    }

    /// <summary>The same attribute as an Activity tag carries it.</summary>
    public static KeyValuePair<string, object?> Tag(JsonElement a)
    {
        var key = a.GetProperty("key").GetString()!;
        object? value = a.TryGetProperty("string", out var s) ? s.GetString()
            : a.TryGetProperty("int", out var i) ? i.GetInt64()
            : a.TryGetProperty("bool", out var b) ? b.GetBoolean()
            : a.TryGetProperty("double", out var d) ? d.GetDouble()
            : a.GetProperty("array").EnumerateArray().Select(e => e.GetString()).ToArray();
        return new(key, value);
    }
}
