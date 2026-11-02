// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections;
using System.Globalization;
using System.Text.RegularExpressions;
using OpenTelemetry.Trace;

namespace Pydantic.OpAmp.Otel;

/// <summary>What the OpAMP and SDK variables say, the same for the plugin and the hosting package.</summary>
internal static partial class Variables
{
    /// <summary>
    /// Where the SDK's <paramref name="name"/> exporter of <paramref name="signal"/> sends, as its
    /// variables say: OTLP's signal endpoint, else the shared one, else the default for its
    /// protocol (http/protobuf, port 4318, unless gRPC); Zipkin's endpoint. Empty for any other
    /// (console, Prometheus, which is pulled). The layer keeps only the scheme, host and port.
    /// </summary>
    public static string Destination(Func<string, string?> getenv, Signal signal, string name)
    {
        string? Set(string key) => getenv(key)?.Trim() is { Length: > 0 } v ? v : null;
        var upper = signal.ToString().ToUpperInvariant();
        return name switch
        {
            "otlp" => Set($"OTEL_EXPORTER_OTLP_{upper}_ENDPOINT") ?? Set("OTEL_EXPORTER_OTLP_ENDPOINT")
                ?? ((Set($"OTEL_EXPORTER_OTLP_{upper}_PROTOCOL") ?? Set("OTEL_EXPORTER_OTLP_PROTOCOL")) == "grpc"
                    ? "http://localhost:4317" : "http://localhost:4318"),
            "zipkin" => Set("OTEL_EXPORTER_ZIPKIN_ENDPOINT") ?? "http://localhost:9411/api/v2/spans",
            _ => string.Empty,
        };
    }

    /// <summary>
    /// `1500` (milliseconds, as the SDK's variables), or `500ms`, `30s`, `2m`: the hosting package's
    /// <c>OpAmp:HeartbeatInterval</c>. (The variables themselves are the core's Setup call's to read.)
    /// </summary>
    public static TimeSpan Duration(string value)
    {
        var m = DurationPattern().Match(value);
        if (!m.Success)
        {
            throw new FormatException($"not a duration: \"{value}\"");
        }

        var n = long.Parse(m.Groups[1].Value, CultureInfo.InvariantCulture);
        return m.Groups[2].Value switch
        {
            "s" => TimeSpan.FromSeconds(n),
            "m" => TimeSpan.FromMinutes(n),
            _ => TimeSpan.FromMilliseconds(n),
        };
    }

    /// <summary>
    /// A heartbeat interval, as <see cref="Duration"/> (or, <paramref name="timeSpan"/>, with a colon,
    /// a TimeSpan: <c>00:00:30</c>); one it can't read, or a negative one (which the client refuses),
    /// is null, the client's default, with a warning: never an error.
    /// </summary>
    public static TimeSpan? Interval(string name, string value, Action<string> warn, bool timeSpan = false)
    {
        try
        {
            var interval = timeSpan && value.Contains(':', StringComparison.Ordinal) ? TimeSpan.Parse(value, CultureInfo.InvariantCulture) : Duration(value);
            if (interval < TimeSpan.Zero)
            {
                warn($"{name} is negative, using the default: \"{value}\"");
                return null;
            }

            return interval;
        }
        catch (Exception e) when (e is FormatException or OverflowException)
        {
            warn($"{name} unreadable, using the default: {e.Message}");
            return null;
        }
    }

    /// <summary>
    /// The sampler OTEL_TRACES_SAMPLER names, as the SDK builds it when given none (the SDK reads the
    /// variable only then, and a plugin's sampler replaces it): the specification's values, and an
    /// argument out of range is 1.
    /// </summary>
    public static Sampler SamplerFromEnv(Func<string, string?> getenv)
    {
        Sampler Ratio()
        {
            var ok = double.TryParse(getenv("OTEL_TRACES_SAMPLER_ARG"), NumberStyles.Float, CultureInfo.InvariantCulture, out var r);
            return new TraceIdRatioBasedSampler(ok && r is >= 0 and <= 1 ? r : 1);
        }

        return getenv("OTEL_TRACES_SAMPLER")?.Trim().ToLowerInvariant() switch
        {
            "always_on" => new AlwaysOnSampler(),
            "always_off" => new AlwaysOffSampler(),
            "traceidratio" => Ratio(),
            "parentbased_always_off" => new ParentBasedSampler(new AlwaysOffSampler()),
            "parentbased_traceidratio" => new ParentBasedSampler(Ratio()),
            _ => new ParentBasedSampler(new AlwaysOnSampler()),
        };
    }

    /// <summary>The process environment, as a lookup.</summary>
    public static Func<string, string?> Process()
    {
        var snapshot = new Dictionary<string, string?>();
        foreach (DictionaryEntry e in Environment.GetEnvironmentVariables())
        {
            snapshot[(string)e.Key] = e.Value as string;
        }

        return name => snapshot.GetValueOrDefault(name);
    }

    [GeneratedRegex(@"^\s*(\d+)\s*(ms|s|m)?\s*$")]
    private static partial Regex DurationPattern();
}
