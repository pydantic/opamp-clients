// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Buffers;
using System.Buffers.Binary;
using System.IO.Compression;
using System.Text;

namespace Pydantic.OpAmp.Otel;

/// <summary>What a gate holds one signal's exports to; a part left null does nothing.</summary>
/// <param name="Levels">Logs: the lowest severity exported, per scope (send_logs_level and send_logs_level_by_scope).</param>
/// <param name="Cap">Logs: send_logs_per_second, of the records past the levels.</param>
/// <param name="Redact">Traces and logs: drop_attributes and mask_attributes.</param>
internal sealed record ExportPolicy(LogLevels? Levels = null, TokenBucket? Cap = null, Redactor? Redact = null)
{
    /// <summary>Holds an export to nothing: it goes as it came.</summary>
    public static readonly ExportPolicy None = new();

    /// <summary>Whether an export of <paramref name="signal"/> can come out changed.</summary>
    public bool Changes(Signal signal) => signal switch
    {
        Signal.Logs => this.Levels is { Active: true } || this.Cap is not null || this.Redact is not null,
        Signal.Traces => this.Redact is not null,
        _ => false,
    };
}

/// <summary>
/// An export, as the gate rewrote it. <see cref="Body"/>: the body to send in its place; null:
/// send it as it is. <see cref="Items"/>: the spans, metric data points or log records it carried;
/// <see cref="Kept"/>: those still in it. <see cref="Readable"/>: false when it couldn't be read as
/// OTLP (another encoding, or not OTLP at all): nothing is known of it, and it is as it came.
/// </summary>
internal readonly record struct Rewritten(byte[]? Body, int Items, int Kept, bool Readable)
{
    public static readonly Rewritten Unreadable = new(null, 0, 0, false);
}

/// <summary>
/// The OTLP exporter's gate, inside an export: an <c>Export{Traces,Metrics,Logs}ServiceRequest</c>
/// held to the server's settings. The .NET SDK has no hook that keeps a record from one exporter
/// (every log processor sees every record, and a record carries no flag an exporter heeds), so the
/// gate, which sees exactly the exports <c>send_*</c> stops, changes what they carry. It walks the
/// protobuf wire format down to the items (resource, then scope, then span, data point or log
/// record) and reads only what it needs of each; everything else is copied byte for byte. A body in
/// gzip (http/protobuf's Content-Encoding, or gRPC's compressed flag with grpc-encoding) is read
/// unzipped and given back zipped again.
/// </summary>
internal static class OtlpRewriter
{
    // Every request: resource_* 1; Resource*: scope_* 2; Scope*: spans, metrics, log_records 2.
    private const int Resources = 1;
    private const int Scopes = 2;
    private const int Items = 2;

    // ScopeLogs.scope, InstrumentationScope.name; LogRecord.severity_number.
    private const int Scope = 1;
    private const int ScopeName = 1;
    private const int SeverityNumber = 2;

    // Where attributes are: Span.attributes 9, its events 11 (Event.attributes 3) and links 13
    // (Link.attributes 4); LogRecord.attributes 6. Each a KeyValue: key 1, value 2.
    private const int SpanAttributes = 9;
    private const int SpanEvents = 11;
    private const int SpanLinks = 13;
    private const int EventAttributes = 3;
    private const int LinkAttributes = 4;
    private const int LogAttributes = 6;
    private const int Key = 1;

    private static readonly byte[] MaskedBytes = Encoding.UTF8.GetBytes(Redactor.Masked);

    private const int Varint = 0;
    private const int Fixed64 = 1;
    private const int Length = 2;
    private const int Fixed32 = 5;

    /// <summary>
    /// The export of <paramref name="signal"/> in <paramref name="body"/> (gRPC-framed when
    /// <paramref name="grpc"/>; <paramref name="encoding"/>: Content-Encoding, or grpc-encoding),
    /// held to <paramref name="policy"/>, and what it carried.
    /// </summary>
    public static Rewritten Rewrite(byte[] body, bool grpc, string? encoding, Signal signal, ExportPolicy policy)
    {
        if (Unframe(body, grpc, encoding) is not { } unframed)
        {
            return Rewritten.Unreadable;
        }

        var walk = new Walk(signal, policy, write: policy.Changes(signal));
        if (signal == Signal.Logs && policy.Cap is { } cap)
        {
            // The cap is on the records past the levels, the batch's first ones: count them, take
            // that many tokens, then let as many through.
            var count = new Walk(signal, policy with { Cap = null }, write: false);
            if (Message(unframed.Message, 0, count, scope: null) is null)
            {
                return Rewritten.Unreadable;
            }

            walk.Budget = cap.Take(count.Kept);
        }

        var rewritten = Message(unframed.Message, 0, walk, scope: null);
        if (rewritten is null)
        {
            return Rewritten.Unreadable;
        }

        if (!walk.Changed)
        {
            return new Rewritten(null, walk.Items, walk.Kept, true);
        }

        return new Rewritten(Frame(rewritten, grpc, unframed.Gzip), walk.Items, walk.Kept, true);
    }

    /// <summary>The message in a body, unzipped; null if it can't be read (another encoding, a bad frame, gzip that isn't).</summary>
    private static (byte[] Message, bool Gzip)? Unframe(byte[] body, bool grpc, string? encoding)
    {
        var gzip = string.Equals(encoding, "gzip", StringComparison.OrdinalIgnoreCase);
        var message = body;
        if (grpc)
        {
            // gRPC's frame: a compressed flag, the length (big-endian), the message. The flag says
            // whether the message is compressed; grpc-encoding says with what.
            if (body.Length < 5 || body[0] > 1 || BinaryPrimitives.ReadUInt32BigEndian(body.AsSpan(1)) != body.Length - 5)
            {
                return null;
            }

            gzip = body[0] == 1;
            if (gzip && !string.Equals(encoding, "gzip", StringComparison.OrdinalIgnoreCase))
            {
                return null;
            }

            message = body[5..];
        }
        else if (encoding is not null && !gzip && !string.Equals(encoding, "identity", StringComparison.OrdinalIgnoreCase))
        {
            return null;
        }

        if (gzip)
        {
            if (Gunzip(message) is not { } unzipped)
            {
                return null;
            }

            message = unzipped;
        }

        return (message, gzip);
    }

    /// <summary>A message framed and encoded as the body it came in was.</summary>
    private static byte[] Frame(byte[] message, bool grpc, bool gzip)
    {
        if (gzip)
        {
            message = Gzip(message);
        }

        if (!grpc)
        {
            return message;
        }

        var framed = new byte[message.Length + 5];
        framed[0] = gzip ? (byte)1 : (byte)0;
        BinaryPrimitives.WriteUInt32BigEndian(framed.AsSpan(1), (uint)message.Length);
        message.CopyTo(framed, 5);
        return framed;
    }

    /// <summary>A gzip body unzipped; null if it isn't gzip.</summary>
    private static byte[]? Gunzip(byte[] body)
    {
        try
        {
            using var unzip = new GZipStream(new MemoryStream(body), CompressionMode.Decompress);
            using var output = new MemoryStream();
            unzip.CopyTo(output);
            return output.ToArray();
        }
        catch (InvalidDataException)
        {
            return null;
        }
    }

    // As the OTLP exporter compresses (CompressionLevel.Fastest).
    private static byte[] Gzip(byte[] body)
    {
        using var output = new MemoryStream();
        using (var zip = new GZipStream(output, CompressionLevel.Fastest, leaveOpen: true))
        {
            zip.Write(body);
        }

        return output.ToArray();
    }

    /// <summary>
    /// One message, at <paramref name="depth"/> (0: the request, 1: a Resource*, 2: a Scope*), held
    /// to the walk's policy: its bytes as rewritten (an empty array when the walk only counts).
    /// <paramref name="scope"/>: the scope's name, within a ScopeLogs. Null if it isn't well formed.
    /// </summary>
    private static byte[]? Message(ReadOnlySpan<byte> message, int depth, Walk walk, string? scope)
    {
        if (depth == 2 && walk.Signal == Signal.Logs)
        {
            // The scope's name, wherever it is among the records: protobuf allows it after them.
            if (ScopeNameOf(message) is not { } name)
            {
                return null;
            }

            scope = name;
        }

        var output = walk.Write ? new ArrayBufferWriter<byte>(Math.Max(message.Length, 1)) : null;
        var at = 0;
        while (at < message.Length)
        {
            var start = at;
            if (!Field(message, ref at, out var number, out var wire, out var payload))
            {
                return null;
            }

            var into = (depth, number, wire) switch
            {
                (0, Resources, Length) => 1,
                (1, Scopes, Length) => 2,
                (2, Items, Length) => 3,
                _ => 0,
            };
            if (into == 0)
            {
                output?.Write(message[start..at]);
                continue;
            }

            var inner = message.Slice(payload, at - payload);
            byte[]? rewritten;
            if (into == 3)
            {
                if (!Item(inner, walk, scope, out rewritten))
                {
                    return null;
                }

                if (rewritten is null)
                {
                    continue; // dropped
                }
            }
            else if ((rewritten = Message(inner, into, walk, scope)) is null)
            {
                return null;
            }

            if (output is not null)
            {
                output.Write(message.Slice(start, TagLength(message[start..])));
                WriteVarint(output, (ulong)rewritten.Length);
                output.Write(rewritten);
            }
        }

        return output is null ? [] : output.WrittenSpan.ToArray();
    }

    /// <summary>
    /// One span, metric or log record: counted, and kept (<paramref name="rewritten"/>, its bytes as
    /// they are to go) or dropped (null). False if it isn't well formed.
    /// </summary>
    private static bool Item(ReadOnlySpan<byte> item, Walk walk, string? scope, out byte[]? rewritten)
    {
        rewritten = null;
        switch (walk.Signal)
        {
            case Signal.Logs:
                if (Severity(item) is not { } severity)
                {
                    return false;
                }

                walk.Items++;
                if ((walk.Policy.Levels is { } levels && !levels.Keeps(scope ?? string.Empty, severity)) || walk.Budget-- <= 0)
                {
                    walk.Changed = true;
                    return true;
                }

                walk.Kept++;
                return Redacted(item, walk, LogAttributes, out rewritten);
            case Signal.Metrics:
                if (DataPoints(item) is not { } points)
                {
                    return false;
                }

                walk.Items += points;
                walk.Kept += points;
                rewritten = walk.Write ? item.ToArray() : [];
                return true;
            default:
                walk.Items++;
                walk.Kept++;
                return Redacted(item, walk, SpanAttributes, out rewritten);
        }
    }

    /// <summary>
    /// A span, span event, span link or log record as it is to go: its attributes (field
    /// <paramref name="attributes"/>) redacted, and a span's events' and links' too; as it is when
    /// the walk doesn't redact. False if it isn't well formed.
    /// </summary>
    private static bool Redacted(ReadOnlySpan<byte> message, Walk walk, int attributes, out byte[]? rewritten)
    {
        rewritten = null;
        if (!walk.Write || walk.Policy.Redact is not { } redact)
        {
            rewritten = walk.Write ? message.ToArray() : [];
            return true;
        }

        var output = new ArrayBufferWriter<byte>(Math.Max(message.Length, 1));
        var at = 0;
        while (at < message.Length)
        {
            var start = at;
            if (!Field(message, ref at, out var number, out var wire, out var payload))
            {
                return false;
            }

            var inner = message.Slice(payload, at - payload);
            var tag = message.Slice(start, TagLength(message[start..]));
            if (wire == Length && (int)number == attributes)
            {
                if (KeyOf(inner) is not { } key)
                {
                    return false;
                }

                switch (redact.Of(Encoding.UTF8.GetString(key)))
                {
                    case Redaction.Drop:
                        walk.Changed = true;
                        continue;
                    case Redaction.Mask:
                        walk.Changed = true;
                        var masked = MaskedKeyValue(key);
                        output.Write(tag);
                        WriteVarint(output, (ulong)masked.Length);
                        output.Write(masked);
                        continue;
                }
            }
            else if (wire == Length && attributes == SpanAttributes && number is SpanEvents or SpanLinks)
            {
                if (!Redacted(inner, walk, number == SpanEvents ? EventAttributes : LinkAttributes, out var nested))
                {
                    return false;
                }

                output.Write(tag);
                WriteVarint(output, (ulong)nested!.Length);
                output.Write(nested);
                continue;
            }

            output.Write(message[start..at]);
        }

        rewritten = output.WrittenSpan.ToArray();
        return true;
    }

    /// <summary>A KeyValue's key, as its UTF-8 bytes (the last given; empty if none). Null if it isn't well formed.</summary>
    private static byte[]? KeyOf(ReadOnlySpan<byte> keyValue)
    {
        var key = Array.Empty<byte>();
        var at = 0;
        while (at < keyValue.Length)
        {
            if (!Field(keyValue, ref at, out var number, out var wire, out var payload))
            {
                return null;
            }

            if (number == Key && wire == Length)
            {
                key = keyValue.Slice(payload, at - payload).ToArray();
            }
        }

        return key;
    }

    /// <summary>KeyValue{key, value: AnyValue{string_value: "[REDACTED]"}}.</summary>
    private static byte[] MaskedKeyValue(byte[] key)
    {
        var any = new ArrayBufferWriter<byte>();
        any.Write<byte>([0x0a]); // string_value
        WriteVarint(any, (ulong)MaskedBytes.Length);
        any.Write(MaskedBytes);
        var kv = new ArrayBufferWriter<byte>();
        kv.Write<byte>([0x0a]); // key
        WriteVarint(kv, (ulong)key.Length);
        kv.Write(key);
        kv.Write<byte>([0x12]); // value
        WriteVarint(kv, (ulong)any.WrittenCount);
        kv.Write(any.WrittenSpan);
        return kv.WrittenSpan.ToArray();
    }

    /// <summary>The name of a ScopeLogs' scope (the last given, as protobuf reads it; empty if none). Null if it isn't well formed.</summary>
    private static string? ScopeNameOf(ReadOnlySpan<byte> scopeLogs)
    {
        var name = string.Empty;
        var at = 0;
        while (at < scopeLogs.Length)
        {
            if (!Field(scopeLogs, ref at, out var number, out var wire, out var payload))
            {
                return null;
            }

            if (number != Scope || wire != Length)
            {
                continue;
            }

            var scope = scopeLogs.Slice(payload, at - payload);
            var s = 0;
            while (s < scope.Length)
            {
                if (!Field(scope, ref s, out var n, out var w, out var p))
                {
                    return null;
                }

                if (n == ScopeName && w == Length)
                {
                    name = Encoding.UTF8.GetString(scope.Slice(p, s - p));
                }
            }
        }

        return name;
    }

    /// <summary>A LogRecord's severity_number (the last one, as protobuf reads it; 0 if none). Null if it isn't well formed.</summary>
    private static uint? Severity(ReadOnlySpan<byte> record)
    {
        ulong severity = 0;
        var at = 0;
        while (at < record.Length)
        {
            var start = at;
            if (!Field(record, ref at, out var number, out var wire, out _))
            {
                return null;
            }

            if (number == SeverityNumber && wire == Varint)
            {
                var v = start + TagLength(record[start..]);
                if (!ReadVarint(record, ref v, out severity))
                {
                    return null;
                }
            }
        }

        return (uint)Math.Min(severity, uint.MaxValue);
    }

    /// <summary>
    /// A Metric's data points: those of its gauge (5), sum (7), histogram (9), exponential
    /// histogram (10) or summary (11), each a data_points (1) list. Null if it isn't well formed.
    /// </summary>
    private static int? DataPoints(ReadOnlySpan<byte> metric)
    {
        var points = 0;
        var at = 0;
        while (at < metric.Length)
        {
            if (!Field(metric, ref at, out var number, out var wire, out var payload))
            {
                return null;
            }

            if (wire != Length || number is not (5 or 7 or 9 or 10 or 11))
            {
                continue;
            }

            var data = metric.Slice(payload, at - payload);
            var d = 0;
            while (d < data.Length)
            {
                if (!Field(data, ref d, out var n, out var w, out _))
                {
                    return null;
                }

                if (n == 1 && w == Length)
                {
                    points++;
                }
            }
        }

        return points;
    }

    /// <summary>
    /// The field at <paramref name="at"/>: its number, wire type, and where a length-delimited
    /// one's payload starts; <paramref name="at"/> moves past it. False if it isn't well formed.
    /// </summary>
    private static bool Field(ReadOnlySpan<byte> m, ref int at, out ulong number, out int wire, out int payload)
    {
        payload = 0;
        number = 0;
        wire = 0;
        if (!ReadVarint(m, ref at, out var tag))
        {
            return false;
        }

        number = tag >> 3;
        wire = (int)(tag & 7);
        switch (wire)
        {
            case Varint:
                return ReadVarint(m, ref at, out _);
            case Fixed64:
                at += 8;
                return at <= m.Length;
            case Fixed32:
                at += 4;
                return at <= m.Length;
            case Length:
                if (!ReadVarint(m, ref at, out var length) || length > (ulong)(m.Length - at))
                {
                    return false;
                }

                payload = at;
                at += (int)length;
                return true;
            default:
                return false; // groups: not in OTLP
        }
    }

    private static int TagLength(ReadOnlySpan<byte> m)
    {
        var at = 0;
        ReadVarint(m, ref at, out _);
        return at;
    }

    private static bool ReadVarint(ReadOnlySpan<byte> m, ref int at, out ulong value)
    {
        value = 0;
        for (var shift = 0; shift < 64; shift += 7)
        {
            if (at >= m.Length)
            {
                return false;
            }

            var b = m[at++];
            value |= (ulong)(b & 0x7f) << shift;
            if (b < 0x80)
            {
                return true;
            }
        }

        return false;
    }

    private static void WriteVarint(ArrayBufferWriter<byte> output, ulong value)
    {
        Span<byte> bytes = stackalloc byte[10];
        var n = 0;
        for (; value >= 0x80; value >>= 7)
        {
            bytes[n++] = (byte)(value | 0x80);
        }

        bytes[n++] = (byte)value;
        output.Write(bytes[..n]);
    }

    /// <summary>One export's walk: what it holds the export to, and what it has seen so far.</summary>
    private sealed class Walk(Signal signal, ExportPolicy policy, bool write)
    {
        public Signal Signal { get; } = signal;

        public ExportPolicy Policy { get; } = policy;

        /// <summary>Whether the export is written out again; else only counted.</summary>
        public bool Write { get; } = write;

        public int Items { get; set; }

        public int Kept { get; set; }

        /// <summary>Logs: how many more records past the levels may go (the rate cap's tokens).</summary>
        public long Budget { get; set; } = long.MaxValue;

        /// <summary>Whether anything was dropped or changed.</summary>
        public bool Changed { get; set; }
    }
}
