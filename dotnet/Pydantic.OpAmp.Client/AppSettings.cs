// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections;
using System.Collections.ObjectModel;
using System.Diagnostics.CodeAnalysis;
using System.Globalization;
using System.Text.RegularExpressions;

namespace Pydantic.OpAmp;

/// <summary>
/// The application's own settings, as the OpAMP server sends them (docs/app-settings.md): the
/// <c>custom</c> map of the remote config's <c>elastic</c> entry, keys to text values, which the
/// SDK doesn't read but hands on for the application to read here.
/// </summary>
/// <remarks>
/// The integration fills it: the automatic instrumentation plugin
/// (<c>Pydantic.OpAmp.AutoInstrumentation</c>) or <c>WithOpAmp()</c>
/// (<c>Pydantic.OpAmp.Extensions.Hosting</c>), both of which bring this assembly with them. With
/// neither attached, or no OpAMP server configured, every read is its default and no handler is
/// ever called. Every member is safe from any thread, and no read ever throws.
/// </remarks>
public static class OpAmpAppSettings
{
    /// <summary>What the integration puts in force, and what these read.</summary>
    internal static AppSettingsStore Store { get; } = new();

    /// <summary>
    /// The value of the setting <paramref name="key"/> in force, exactly as sent, or
    /// <paramref name="defaultValue"/> while there is none: before the first config arrives (unless
    /// one was saved before a restart), when the config in force leaves the key out, and without an
    /// OpAMP server at all. Keys are case-sensitive.
    /// </summary>
    [return: NotNullIfNotNull(nameof(defaultValue))]
    public static string? Get(string key, string? defaultValue = null) => Store.Get(key, defaultValue);

    /// <summary>
    /// The setting <paramref name="key"/> read as <c>true</c> or <c>false</c> in any ASCII case,
    /// spaces round it (space, tab, CR, LF) allowed; <paramref name="defaultValue"/> for anything
    /// else (<c>on</c>, <c>1</c> and <c>yes</c> too) and while there is none.
    /// </summary>
    public static bool GetBool(string key, bool defaultValue) => Store.GetBool(key, defaultValue);

    /// <summary>
    /// The setting <paramref name="key"/> read as a decimal integer (a sign and leading zeros
    /// allowed, spaces round it; no <c>_</c>, no hex) in the 64-bit range;
    /// <paramref name="defaultValue"/> for anything else and while there is none.
    /// </summary>
    public static long GetInt64(string key, long defaultValue) => Store.GetInt64(key, defaultValue);

    /// <summary>
    /// The setting <paramref name="key"/> read as a number as JSON writes one (a leading <c>+</c>
    /// allowed, spaces round it), finite as a double; <paramref name="defaultValue"/> for anything
    /// else (<c>NaN</c>, <c>1e400</c>, <c>.5</c>) and while there is none.
    /// </summary>
    public static double GetDouble(string key, double defaultValue) => Store.GetDouble(key, defaultValue);

    /// <summary>
    /// Every setting in force, in key order (ordinal): a map of its own that no later change
    /// alters. Empty while there are none.
    /// </summary>
    public static IReadOnlyDictionary<string, string> Snapshot() => Store.Snapshot();

    /// <summary>
    /// Call <paramref name="handler"/> after each change to the settings is put in force, with the
    /// settings now in force (as <see cref="Snapshot"/> gives them) and the keys added, changed or
    /// removed (enumerated in ordinal order). Not called for the values in force when it is added
    /// (read those with <see cref="Snapshot"/>), nor when a config leaves them as they were (the same
    /// map again, another setting changed, a FAILED config).
    /// </summary>
    /// <remarks>
    /// Changes are told one at a time, in the order they happened, to each handler in the order it
    /// was added; <see cref="Get"/> already returns the new values inside the handler. It runs on
    /// the integration's thread (the OpAMP client's), so it should be quick: hand longer work to the
    /// application's own threads. An exception it throws is caught and logged as an error; the other
    /// handlers are still called and the config is still APPLIED. A handler may add or remove
    /// handlers, itself included, while it runs: that takes effect from the next change.
    /// </remarks>
    /// <returns>Disposing it removes the handler; disposing it again does nothing.</returns>
    public static IDisposable OnChange(Action<IReadOnlyDictionary<string, string>, IReadOnlySet<string>> handler) =>
        Store.OnChange(handler);
}

/// <summary>
/// Application settings in force, and who to tell when they change: <see cref="OpAmpAppSettings"/>'s
/// one, or a test's own. The integration puts each map in force with <see cref="AppSettingsStore.Put"/>.
/// </summary>
internal sealed partial class AppSettingsStore
{
    private static readonly IReadOnlyDictionary<string, string> Empty = Freeze(new SortedDictionary<string, string>(StringComparer.Ordinal));

    // The spaces a typed read takes round a value: every language trims these, and only these.
    private static readonly char[] Spaces = [' ', '\t', '\r', '\n'];

    private readonly object telling = new(); // held while a change is put in force and told
    private readonly object subscribing = new();
    private volatile IReadOnlyDictionary<string, string> values = Empty; // never changed once stored
    private volatile Subscription[] subscriptions = []; // replaced, never changed: a change being told keeps the one it took

    /// <summary>Where a change is said (information).</summary>
    public Action<string> Info { get; set; } = _ => { };

    /// <summary>Where a handler that threw is said (an error).</summary>
    public Action<string> Error { get; set; } = m => Console.Error.WriteLine($"OpAMP: {m}");

    [return: NotNullIfNotNull(nameof(defaultValue))]
    public string? Get(string? key, string? defaultValue) =>
        key is not null && this.values.TryGetValue(key, out var v) ? v : defaultValue;

    public bool GetBool(string? key, bool defaultValue) =>
        this.Text(key) is { } v && ParseBool(v) is { } b ? b : defaultValue;

    public long GetInt64(string? key, long defaultValue) =>
        this.Text(key) is { } v && ParseInt64(v) is { } n ? n : defaultValue;

    public double GetDouble(string? key, double defaultValue) =>
        this.Text(key) is { } v && ParseDouble(v) is { } d ? d : defaultValue;

    public IReadOnlyDictionary<string, string> Snapshot() => this.values;

    public IDisposable OnChange(Action<IReadOnlyDictionary<string, string>, IReadOnlySet<string>> handler)
    {
        ArgumentNullException.ThrowIfNull(handler);
        var sub = new Subscription(this, handler);
        lock (this.subscribing)
        {
            this.subscriptions = [.. this.subscriptions, sub];
        }

        return sub;
    }

    /// <summary>
    /// Put <paramref name="next"/> in force (the layer's <c>Settings.custom</c>; null: none), and,
    /// if anything changed, tell each handler on the calling thread.
    /// </summary>
    public void Put(IEnumerable<KeyValuePair<string, string>>? next)
    {
        lock (this.telling)
        {
            // Who is told of this change: those subscribed when it began, before its values can be
            // read. One added or removed from here on (on another thread, or by a handler of this
            // change) takes effect from the next change.
            Subscription[] told;
            lock (this.subscribing)
            {
                told = this.subscriptions;
            }

            var map = new SortedDictionary<string, string>(StringComparer.Ordinal);
            if (next is not null)
            {
                foreach (var (k, v) in next)
                {
                    map[k] = v;
                }
            }

            var changed = Changed(this.values, map);
            if (changed.Count == 0)
            {
                return;
            }

            var now = Freeze(map);
            this.values = now;
            this.Info($"application settings changed: {changed}");
            foreach (var sub in told)
            {
                try
                {
                    sub.Handler(now, changed);
                }
                catch (Exception e)
                {
                    // The application's bug: the others are still told, and the client goes on.
                    this.Error($"an application settings handler threw (changed: {changed}): {e}");
                }
            }
        }
    }

    /// <summary>`true` or `false` in any ASCII case (only ASCII: "falſe" is no "false"); else null.</summary>
    internal static bool? ParseBool(string v)
    {
        v = v.Trim(Spaces);
        if (AsciiIgnoreCase(v, "true"))
        {
            return true;
        }

        if (AsciiIgnoreCase(v, "false"))
        {
            return false;
        }

        return null;
    }

    /// <summary>Decimal digits with an optional sign, in the 64-bit range; else null.</summary>
    internal static long? ParseInt64(string v)
    {
        v = v.Trim(Spaces);
        return IntPattern().IsMatch(v) && long.TryParse(v, NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture, out var n)
            ? n
            : null;
    }

    /// <summary>A JSON number with an optional leading <c>+</c>, finite as a double; else null.</summary>
    internal static double? ParseDouble(string v)
    {
        v = v.Trim(Spaces);
        const NumberStyles json = NumberStyles.AllowLeadingSign | NumberStyles.AllowDecimalPoint | NumberStyles.AllowExponent;
        return FloatPattern().IsMatch(v) && double.TryParse(v, json, CultureInfo.InvariantCulture, out var d) && double.IsFinite(d)
            ? d
            : null;
    }

    private static bool AsciiIgnoreCase(string v, string lower)
    {
        if (v.Length != lower.Length)
        {
            return false;
        }

        for (var i = 0; i < v.Length; i++)
        {
            var c = v[i];
            if ((c is >= 'A' and <= 'Z' ? (char)(c + ('a' - 'A')) : c) != lower[i])
            {
                return false;
            }
        }

        return true;
    }

    private static IReadOnlyDictionary<string, string> Freeze(SortedDictionary<string, string> map) =>
        new ReadOnlyDictionary<string, string>(map);

    /// <summary>The keys added, changed or removed between <paramref name="old"/> and <paramref name="now"/>.</summary>
    private static ChangedKeys Changed(IReadOnlyDictionary<string, string> old, IReadOnlyDictionary<string, string> now)
    {
        var changed = new SortedSet<string>(StringComparer.Ordinal);
        foreach (var (k, v) in old)
        {
            if (!now.TryGetValue(k, out var w) || !string.Equals(v, w, StringComparison.Ordinal))
            {
                changed.Add(k);
            }
        }

        foreach (var k in now.Keys)
        {
            if (!old.ContainsKey(k))
            {
                changed.Add(k);
            }
        }

        return new ChangedKeys(changed);
    }

    private string? Text(string? key) => this.Get(key, null);

    private void Remove(Subscription sub)
    {
        lock (this.subscribing)
        {
            var i = Array.IndexOf(this.subscriptions, sub);
            if (i >= 0)
            {
                this.subscriptions = [.. this.subscriptions[..i], .. this.subscriptions[(i + 1)..]];
            }
        }
    }

    [GeneratedRegex(@"^[+-]?[0-9]+\z", RegexOptions.CultureInvariant)]
    private static partial Regex IntPattern();

    // A JSON number, a leading + too: what every language reads the same way.
    [GeneratedRegex(@"^[+-]?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?\z", RegexOptions.CultureInvariant)]
    private static partial Regex FloatPattern();

    /// <summary>A handler as added: each addition its own, so removing one leaves the same handler added again.</summary>
    private sealed class Subscription(AppSettingsStore store, Action<IReadOnlyDictionary<string, string>, IReadOnlySet<string>> handler) : IDisposable
    {
        public Action<IReadOnlyDictionary<string, string>, IReadOnlySet<string>> Handler => handler;

        public void Dispose() => store.Remove(this);
    }
}

/// <summary>The keys a change added, changed or removed: read-only, enumerated in ordinal order.</summary>
internal sealed class ChangedKeys(SortedSet<string> keys) : IReadOnlySet<string>
{
    public int Count => keys.Count;

    public bool Contains(string item) => keys.Contains(item);

    public IEnumerator<string> GetEnumerator() => keys.GetEnumerator();

    IEnumerator IEnumerable.GetEnumerator() => this.GetEnumerator();

    public bool IsProperSubsetOf(IEnumerable<string> other) => keys.IsProperSubsetOf(other);

    public bool IsProperSupersetOf(IEnumerable<string> other) => keys.IsProperSupersetOf(other);

    public bool IsSubsetOf(IEnumerable<string> other) => keys.IsSubsetOf(other);

    public bool IsSupersetOf(IEnumerable<string> other) => keys.IsSupersetOf(other);

    public bool Overlaps(IEnumerable<string> other) => keys.Overlaps(other);

    public bool SetEquals(IEnumerable<string> other) => keys.SetEquals(other);

    /// <summary>The keys, comma-separated, in order.</summary>
    public override string ToString() => string.Join(", ", keys);
}
