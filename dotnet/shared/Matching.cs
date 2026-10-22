// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using Pydantic.OpAmp.Client.Sdk;

namespace Pydantic.OpAmp.Otel;

/// <summary>
/// The log levels a logs gate holds records to: <c>send_logs_level</c> (<see cref="Floor"/>) and
/// <c>send_logs_level_by_scope</c> (<see cref="Scopes"/>, as the layer sorts them: the first that
/// matches is the one). Matched by the scope name a record is exported under: for .NET, the
/// ILogger category.
/// </summary>
internal sealed class LogLevels(uint floor, IReadOnlyList<ScopeLevel> scopes)
{
    /// <summary>A scope's threshold when it is <c>off</c>: past every SeverityNumber, 0 included.</summary>
    public const uint Off = 0xFFFF;

    /// <summary>send_logs_level's SeverityNumber; 0: none.</summary>
    public uint Floor { get; } = floor;

    public IReadOnlyList<ScopeLevel> Scopes { get; } = scopes;

    /// <summary>Whether any record can be dropped.</summary>
    public bool Active => this.Floor != 0 || this.Scopes.Count > 0;

    /// <summary>
    /// The lowest SeverityNumber exported of a scope: the first entry whose pattern matches it (an
    /// exact name, or a prefix ending in <c>*</c>), else the floor; 0: every record.
    /// </summary>
    public uint Threshold(string scope)
    {
        foreach (var level in this.Scopes)
        {
            if (Matches(level.Pattern, scope))
            {
                return level.MinSeverity;
            }
        }

        return this.Floor;
    }

    /// <summary>
    /// Whether a record is exported: at or above its scope's threshold. One with no severity (0)
    /// can't be judged and is kept, except where its scope is <c>off</c>.
    /// </summary>
    public bool Keeps(string scope, uint severity) => this.Threshold(scope) switch
    {
        0 => true,
        Off => false,
        var t => severity == 0 || severity >= t,
    };

    /// <summary>An exact scope name, or a prefix ending in <c>*</c> (<c>*</c> alone: every scope, the empty one too).</summary>
    public static bool Matches(string pattern, string scope) =>
        pattern.EndsWith('*') ? scope.StartsWith(pattern[..^1], StringComparison.Ordinal) : pattern == scope;
}

/// <summary>
/// A rate cap's token bucket (sampling_traces_per_second, send_logs_per_second), the algorithm
/// every language runs (testdata/sdk-settings/caps.json pins it): full at its first take;
/// refilled at <see cref="PerSecond"/> tokens a second on a monotonic clock, up to
/// <c>max(n, 1)</c> (a burst of a second, at least one); a take of n gets the whole tokens there
/// are, up to n. In doubles, in that order. Thread-safe.
/// </summary>
internal sealed class TokenBucket(double perSecond, Func<long> nanos)
{
    private readonly object gate = new();
    private double tokens = Math.Max(perSecond, 1);
    private long? last;

    /// <summary>The monotonic clock in nanoseconds, from the process's high-resolution timer.</summary>
    public static readonly Func<long> Monotonic = () => (long)(System.Diagnostics.Stopwatch.GetTimestamp() * (1e9 / System.Diagnostics.Stopwatch.Frequency));

    public double PerSecond { get; } = perSecond;

    /// <summary>Take up to <paramref name="n"/> tokens now: how many were taken. A clock that goes back refills nothing.</summary>
    public long Take(long n) => this.Take(nanos(), n);

    /// <summary>Take up to <paramref name="n"/> tokens at <paramref name="now"/> (nanoseconds).</summary>
    public long Take(long now, long n)
    {
        lock (this.gate)
        {
            var last = this.last ?? now;
            var elapsed = Math.Max(now - last, 0) / 1e9;
            this.tokens = Math.Min(this.tokens + (elapsed * this.PerSecond), Math.Max(this.PerSecond, 1));
            this.last = Math.Max(last, now);
            var taken = Math.Min(n, Math.Floor(this.tokens));
            this.tokens -= taken;
            return (long)taken;
        }
    }
}

/// <summary>
/// The settings' globs: <c>*</c> matches any run of characters, none included; every other
/// character is itself. Over UTF-16 code units, which for well-formed text matches as the layer's
/// over Unicode scalar values does: a pattern's literal run never starts inside a surrogate pair.
/// </summary>
internal static class Glob
{
    /// <summary>Whether <paramref name="text"/> matches <paramref name="pattern"/>, case-sensitively.</summary>
    public static bool Matches(string pattern, string text) => Match(pattern, text, ignoreAsciiCase: false);

    /// <summary>The same, ignoring ASCII case only (<c>A</c> and <c>a</c>; not <c>É</c> and <c>é</c>).</summary>
    public static bool MatchesIgnoringAsciiCase(string pattern, string text) => Match(pattern, text, ignoreAsciiCase: true);

    // On a mismatch, the last * takes one more character and the pattern after it starts again:
    // at most pattern x text steps.
    private static bool Match(string p, string t, bool ignoreAsciiCase)
    {
        int pi = 0, ti = 0, after = -1, taken = 0;
        while (ti < t.Length)
        {
            if (pi < p.Length && p[pi] == '*')
            {
                after = ++pi;
                taken = ti;
            }
            else if (pi < p.Length && Same(p[pi], t[ti], ignoreAsciiCase))
            {
                pi++;
                ti++;
            }
            else if (after >= 0)
            {
                pi = after;
                ti = ++taken;
            }
            else
            {
                return false;
            }
        }

        while (pi < p.Length && p[pi] == '*')
        {
            pi++;
        }

        return pi == p.Length;
    }

    private static bool Same(char a, char b, bool ignoreAsciiCase) =>
        a == b || (ignoreAsciiCase && char.IsAscii(a) && char.IsAscii(b) && char.ToLowerInvariant(a) == char.ToLowerInvariant(b));
}

/// <summary>What redaction does to an attribute.</summary>
internal enum Redaction
{
    Keep,
    Drop,
    Mask,
}

/// <summary>
/// drop_attributes and mask_attributes: globs on attribute keys, matched ignoring ASCII case; a key
/// both match is dropped. Then <see cref="Scrub"/> (scrub_defaults and scrub_patterns), of the
/// attributes kept and of log bodies; null: none.
/// </summary>
internal sealed class Redactor(IReadOnlyList<string> drop, IReadOnlyList<string> mask, Scrubber? scrub = null)
{
    /// <summary>What a masked attribute's value becomes, whatever its type.</summary>
    public const string Masked = "[REDACTED]";

    /// <summary>Scrubbing, of what redaction keeps; null: none.</summary>
    public Scrubber? Scrub { get; } = scrub;

    /// <summary>A redactor for the settings; null when both lists are empty and nothing is scrubbed.</summary>
    public static Redactor? Of(IReadOnlyList<string> drop, IReadOnlyList<string> mask, Scrubber? scrub = null) =>
        drop.Count == 0 && mask.Count == 0 && scrub is null ? null : new Redactor(drop, mask, scrub);

    public Redaction Of(string key) =>
        drop.Any(p => Glob.MatchesIgnoringAsciiCase(p, key)) ? Redaction.Drop
        : mask.Any(p => Glob.MatchesIgnoringAsciiCase(p, key)) ? Redaction.Mask
        : Redaction.Keep;
}
