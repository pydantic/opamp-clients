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
        Sampler Ratio() => new TraceIdRatioBasedSampler(SamplerArg(getenv));

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

    /// <summary>
    /// The share of new traces <see cref="SamplerFromEnv"/>'s sampler samples: always on is 1,
    /// always off 0, a ratio its argument.
    /// </summary>
    public static double SamplingRateFromEnv(Func<string, string?> getenv) =>
        getenv("OTEL_TRACES_SAMPLER")?.Trim().ToLowerInvariant() switch
        {
            "always_off" or "parentbased_always_off" => 0,
            "traceidratio" or "parentbased_traceidratio" => SamplerArg(getenv),
            _ => 1,
        };

    /// <summary>OTEL_TRACES_SAMPLER_ARG as a ratio: one out of range, or not a number, is 1.</summary>
    private static double SamplerArg(Func<string, string?> getenv) =>
        double.TryParse(getenv("OTEL_TRACES_SAMPLER_ARG"), NumberStyles.Float, CultureInfo.InvariantCulture, out var r) && r is >= 0 and <= 1 ? r : 1;

    /// <summary>
    /// A ratio as decimal text, as the local report takes it: <c>1</c>, <c>0.25</c>,
    /// <c>0.00001</c> (the shortest digits that read back as <paramref name="x"/>, never an exponent).
    /// </summary>
    public static string Decimal(double x)
    {
        var text = x.ToString("R", CultureInfo.InvariantCulture);
        var e = text.IndexOfAny(['E', 'e']);
        if (e < 0)
        {
            return text;
        }

        // Below 1e-5 the round trip format gives an exponent: the same digits, written out.
        var digits = text[..e].Replace(".", string.Empty, StringComparison.Ordinal);
        var exponent = int.Parse(text[(e + 1)..], NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture);
        return exponent < 0 ? "0." + new string('0', -exponent - 1) + digits : text;
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
