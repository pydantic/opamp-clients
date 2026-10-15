// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Reflection;
using OpenTelemetry;
using OpenTelemetry.Exporter;
using OpenTelemetry.Logs;
using OpenTelemetry.Metrics;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Otel;

namespace Pydantic.OpAmp.Extensions.Hosting;

/// <summary>
/// Which signals' exports the gate can't stop, read from the built providers. The SDK keeps their
/// processors and readers in internal members, read by reflection as the sampler is
/// (<see cref="Samplers"/>). A signal is gated only when every exporter it has is OTLP, sends to
/// its signal's path (the gate tells signals by path) and through the gate's HTTP client; what
/// can't be read counts as not gated.
/// </summary>
internal static class Exporters
{
    private const BindingFlags Any = BindingFlags.Instance | BindingFlags.Public | BindingFlags.NonPublic | BindingFlags.DeclaredOnly;

    // Processors that hold others, nested deeper than this, aren't followed: not gated.
    private const int Deepest = 8;

    /// <summary>The signals some exporter of which goes on sending whatever the server says. A provider not built: none.</summary>
    public static HashSet<Signal> Ungated(TracerProvider? tracing, MeterProvider? metrics, LoggerProvider? logs) => Read(tracing, metrics, logs).Ungated;

    /// <summary>
    /// What the built providers export through: each exporter, by the name <c>OTEL_*_EXPORTER</c>
    /// gives it or else its type, and whether the gate is in front of it (a processor or reader it
    /// can't see into is listed by its own type, not gated); and the signals some exporter of which
    /// goes on sending whatever the server says.
    /// </summary>
    public static (HashSet<Signal> Ungated, List<(Signal Signal, string Name, bool Gated)> Built) Read(TracerProvider? tracing, MeterProvider? metrics, LoggerProvider? logs)
    {
        var ungated = new HashSet<Signal>();
        var built = new List<(Signal, string, bool)>();
        foreach (var (signal, provider, part) in new (Signal, object?, string)[]
                 {
                     (Signal.Traces, tracing, "Processor"),
                     (Signal.Metrics, metrics, "Reader"),
                     (Signal.Logs, logs, "Processor"),
                 })
        {
            if (provider is null)
            {
                continue;
            }

            var found = new List<(string Name, bool Gated)>();
            if (!TryMember(provider, part, out var p))
            {
                found.Add((TypeName(provider), false));
                ungated.Add(signal);
            }
            else if (!Gated(signal, p, 0, found))
            {
                ungated.Add(signal);
            }

            built.AddRange(found.Select(f => (signal, f.Name, f.Gated)));
        }

        return (ungated, built);
    }

    /// <summary>
    /// A processor or reader: a composite's every one in turn, an export processor's or exporting
    /// reader's exporter, and whatever exporters or processors another processor holds (one that
    /// holds none exports nothing). None at all: nothing to stop. Each exporter met goes in
    /// <paramref name="found"/>, and each part that can't be seen into, as not gated.
    /// </summary>
    private static bool Gated(Signal signal, object? part, int depth, List<(string Name, bool Gated)> found)
    {
        if (part is null)
        {
            return true;
        }

        if (depth > Deepest)
        {
            found.Add((TypeName(part), false));
            return false;
        }

        // CompositeProcessor<T>, CompositeMetricReader: a list from Head, each node's Value and Next.
        // Every one is followed, so that each exporter is listed.
        if (TryMember(part, "Head", out var node))
        {
            var all = true;
            for (; node is not null; node = TryMember(node, "Next", out var next) ? next : null)
            {
                if (!TryMember(node, "Value", out var value))
                {
                    found.Add((TypeName(node), false));
                    all = false;
                }
                else
                {
                    all &= Gated(signal, value, depth + 1, found);
                }
            }

            return all;
        }

        if (Derives(part.GetType(), typeof(BaseExportProcessor<>)) || part is BaseExportingMetricReader)
        {
            if (!TryMember(part, "exporter", out var exporter) || exporter is null)
            {
                found.Add((TypeName(part), false));
                return false;
            }

            return Exporter(signal, exporter, found);
        }

        if (part is MetricReader)
        {
            // A reader of another kind is read from (a scrape, say): nothing in it to stop.
            found.Add((TypeName(part), false));
            return false;
        }

        var gated = true;
        foreach (var held in Held(part))
        {
            gated &= Derives(held.GetType(), typeof(BaseExporter<>)) ? Exporter(signal, held, found) : Gated(signal, held, depth + 1, found);
        }

        return gated;
    }

    /// <summary>An exporter, listed: whether it is gated.</summary>
    private static bool Exporter(Signal signal, object exporter, List<(string Name, bool Gated)> found)
    {
        var gated = GatedExporter(signal, exporter);
        found.Add((Named(exporter.GetType()), gated));
        return gated;
    }

    /// <summary>An exporter's name as <c>OTEL_*_EXPORTER</c> gives it (<c>otlp</c>, <c>console</c>, <c>zipkin</c>, <c>prometheus</c>), else its type's.</summary>
    internal static string Named(Type type) =>
        type.Assembly == typeof(OtlpExporterOptions).Assembly && type.Name.StartsWith("Otlp", StringComparison.Ordinal) ? "otlp"
        : type.Namespace != "OpenTelemetry.Exporter" ? TypeName(type)
        : type.Name.StartsWith("Console", StringComparison.Ordinal) ? "console"
        : type.Name == "ZipkinExporter" ? "zipkin"
        : type.Name == "PrometheusExporter" ? "prometheus"
        : TypeName(type);

    /// <summary>A type's name without its generic arity: <c>InMemoryExporter</c> for <c>InMemoryExporter`1</c>.</summary>
    private static string TypeName(object o) => TypeName(o.GetType());

    private static string TypeName(Type type) => type.Name.Split('`')[0];

    /// <summary>An OTLP exporter whose export client sends to its signal's path, through the gate.</summary>
    private static bool GatedExporter(Signal signal, object exporter) =>
        exporter.GetType().Assembly == typeof(OtlpExporterOptions).Assembly &&
        TryMember(exporter, "transmissionHandler", out var handler) && handler is not null &&
        TryMember(handler, "ExportClient", out var client) && client is not null &&
        TryMember(client, "Endpoint", out var endpoint) && Controls.SignalOf(endpoint as Uri) == signal &&
        TryMember(client, "HttpClient", out var http) && http is HttpClient h &&
        TryMember(h, "_handler", out var first) && first is GateHandler;

    /// <summary>
    /// The exporters and processors a processor of another kind holds in its fields, alone or in a
    /// collection (a fan-out processor's list, say).
    /// </summary>
    private static IEnumerable<object> Held(object processor)
    {
        for (var type = processor.GetType(); type is not null && type != typeof(object); type = type.BaseType)
        {
            foreach (var field in type.GetFields(Any))
            {
                var value = field.GetValue(processor);
                var values = value is System.Collections.IEnumerable many and not string ? many.Cast<object?>() : [value];
                foreach (var v in values)
                {
                    if (v is not null && !ReferenceEquals(v, processor) && Telemetry(v))
                    {
                        yield return v;
                    }
                }
            }
        }
    }

    private static bool Telemetry(object value) =>
        Derives(value.GetType(), typeof(BaseExporter<>)) || Derives(value.GetType(), typeof(BaseProcessor<>)) || value is MetricReader;

    private static bool Derives(Type? type, Type generic)
    {
        for (; type is not null; type = type.BaseType)
        {
            if (type.IsGenericType && type.GetGenericTypeDefinition() == generic)
            {
                return true;
            }
        }

        return false;
    }

    /// <summary>A property or field of the object's type or a base type, by name: false when there is none.</summary>
    internal static bool TryMember(object o, string name, out object? value)
    {
        for (var type = o.GetType(); type is not null; type = type.BaseType)
        {
            if (type.GetProperty(name, Any) is { } p && p.GetIndexParameters().Length == 0)
            {
                value = p.GetValue(o);
                return true;
            }

            if (type.GetField(name, Any) is { } f)
            {
                value = f.GetValue(o);
                return true;
            }
        }

        value = null;
        return false;
    }
}
