// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Diagnostics;
using System.Globalization;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Client.Sdk;
using SdkSpanKind = Pydantic.OpAmp.Client.Sdk.SpanKind;

namespace Pydantic.OpAmp.Otel;

/// <summary>How the rules and the rate decide a span (the layer's <c>SpanSampling.How</c>).</summary>
internal enum How
{
    /// <summary>As its parent was: rules and rates decide only new traces.</summary>
    FollowParent,

    /// <summary>By the SDK's trace-ID ratio sampler, at a rule's rate or sampling_rate.</summary>
    Rate,

    /// <summary>No rule matched and no sampling_rate: the application's own sampler.</summary>
    AppSampler,
}

/// <summary>How a span is sampled: <see cref="How"/>, the parent's flag, or the rate and the index of the rule that gave it (null: sampling_rate).</summary>
internal readonly record struct SpanDecision(How How, bool ParentSampled = false, double Rate = 0, int? Rule = null);

/// <summary>
/// What the server set for the sampler: sampling_rules, sampling_rate and sampling_traces_per_second.
/// Immutable: the sampler swaps one for another.
/// </summary>
internal sealed class SamplingPolicy
{
    public static readonly SamplingPolicy None = new([], null, null);

    private readonly Dictionary<double, Sampler> ratios = [];

    public SamplingPolicy(IReadOnlyList<SamplingRule> rules, double? rate, TokenBucket? cap)
    {
        this.Rules = rules;
        this.Rate = rate;
        this.Cap = cap;
        // The SDK's own ratio sampler, one per rate, made once.
        foreach (var r in rules.Select(r => r.Rate).Append(rate ?? 1).Distinct())
        {
            this.ratios[r] = new TraceIdRatioBasedSampler(r);
        }
    }

    public IReadOnlyList<SamplingRule> Rules { get; }

    public double? Rate { get; }

    /// <summary>sampling_traces_per_second's bucket; null: no cap.</summary>
    public TokenBucket? Cap { get; }

    /// <summary>Whether the rules or the rate decide; else the application's sampler does, every span.</summary>
    public bool Decides => this.Rules.Count > 0 || this.Rate is not null;

    /// <summary>The SDK's trace-ID ratio sampler at a rate the policy has.</summary>
    public Sampler Ratio(double rate) => this.ratios[rate];

    /// <summary>
    /// How a span is sampled, as the layer's reference matcher decides it: with neither rules nor
    /// a rate, the application's sampler, every span; else a span with a parent follows it, and a
    /// root span takes the first rule it matches, else sampling_rate, else the application's
    /// sampler.
    /// </summary>
    public SpanDecision Decide(string name, ActivityKind kind, IEnumerable<KeyValuePair<string, object?>>? tags, Parent parent)
    {
        if (!this.Decides)
        {
            return new SpanDecision(How.AppSampler);
        }

        switch (parent)
        {
            case Parent.LocalSampled or Parent.RemoteSampled:
                return new SpanDecision(How.FollowParent, ParentSampled: true);
            case Parent.LocalUnsampled or Parent.RemoteUnsampled:
                return new SpanDecision(How.FollowParent, ParentSampled: false);
        }

        var attributes = tags as IReadOnlyCollection<KeyValuePair<string, object?>> ?? tags?.ToList() ?? [];
        for (var i = 0; i < this.Rules.Count; i++)
        {
            if (Matches(this.Rules[i], name, kind, attributes))
            {
                return new SpanDecision(How.Rate, Rate: this.Rules[i].Rate, Rule: i);
            }
        }

        return this.Rate is { } rate ? new SpanDecision(How.Rate, Rate: rate) : new SpanDecision(How.AppSampler);
    }

    /// <summary>The text an attribute's value is matched by: a string's own, an integer's decimal, true or false. A double, an array or anything else has none.</summary>
    public static string? Text(object? value) => value switch
    {
        string s => s,
        bool b => b ? "true" : "false",
        long or int or short or sbyte or byte or ulong or uint or ushort => Convert.ToString(value, CultureInfo.InvariantCulture),
        _ => null,
    };

    /// <summary>The path a rule's <c>path</c> matches: <c>url.path</c>, else <c>http.target</c> up to its <c>?</c>.</summary>
    public static string? PathOf(IReadOnlyCollection<KeyValuePair<string, object?>> tags)
    {
        if (First(tags, "url.path") is string path)
        {
            return path;
        }

        return First(tags, "http.target") is string target ? target.Split('?')[0] : null;
    }

    private static bool Matches(SamplingRule rule, string name, ActivityKind kind, IReadOnlyCollection<KeyValuePair<string, object?>> tags) =>
        (!rule.HasName || Glob.Matches(rule.Name, name)) &&
        (!rule.HasKind || rule.Kind == KindOf(kind)) &&
        (!rule.HasPath || (PathOf(tags) is { } path && Glob.Matches(rule.Path, path))) &&
        rule.Attributes.All(a => Text(First(tags, a.Key)) is { } text && Glob.Matches(a.Value, text));

    // The first attribute of a key counts.
    private static object? First(IReadOnlyCollection<KeyValuePair<string, object?>> tags, string key)
    {
        foreach (var t in tags)
        {
            if (t.Key == key)
            {
                return t.Value;
            }
        }

        return null;
    }

    private static SdkSpanKind KindOf(ActivityKind kind) => kind switch
    {
        ActivityKind.Server => SdkSpanKind.Server,
        ActivityKind.Client => SdkSpanKind.Client,
        ActivityKind.Producer => SdkSpanKind.Producer,
        ActivityKind.Consumer => SdkSpanKind.Consumer,
        _ => SdkSpanKind.Internal,
    };
}

/// <summary>
/// The SDK's sampler, live: the application's own until the server sets a rate or rules, then the
/// rules (the first a new trace's root span matches), then the rate, by the SDK's trace-ID ratio
/// sampler; a span with a parent follows it. A trace cap (sampling_traces_per_second) then holds
/// what that sampled: a root span, or a span whose remote parent was sampled, takes a token, and
/// without one is dropped; a local child never takes one.
/// </summary>
internal sealed class LiveSampler : Sampler
{
    /// <summary>ASP.NET Core's server activity: on .NET 8 it reaches the sampler with no tags (no path).</summary>
    public const string AspNetCoreRequest = "Microsoft.AspNetCore.Hosting.HttpRequestIn";

    private readonly Func<long> nanos;
    private Sampler configured = new ParentBasedSampler(new AlwaysOnSampler());
    private volatile SamplingPolicy policy = SamplingPolicy.None;
    private volatile bool filterDecides;

    public LiveSampler(Func<long>? nanos = null)
    {
        this.nanos = nanos ?? TokenBucket.Monotonic;
        this.Description = "OpampLiveSampler";
    }

    /// <summary>What is in force.</summary>
    public SamplingPolicy Policy => this.policy;

    /// <summary>
    /// Whether ASP.NET Core's instrumentation runs our Filter (<see cref="AspNetCoreFilter"/>): a new
    /// trace's request activity is then decided there, where its path is known; here it is recorded
    /// until then.
    /// </summary>
    public bool FilterDecides
    {
        get => this.filterDecides;
        set => this.filterDecides = value;
    }

    /// <summary>Called for each span the trace cap refuses (export health counts them).</summary>
    public Action? Capped { get; set; }

    /// <summary>The sampler the configuration built, before the SDK starts; what was set before (at start) still holds.</summary>
    public LiveSampler Wrap(Sampler sampler)
    {
        this.configured = sampler;
        return this;
    }

    /// <summary>The sampling rate; null: none (the rules, then the configured sampler). The rules and the cap stay.</summary>
    public void Rate(double? value) => this.Apply(value, this.policy.Rules, this.policy.Cap?.PerSecond);

    /// <summary>
    /// Put the rate, the rules and the trace cap in force (null and none: the configured sampler,
    /// uncapped). A cap at the same rate keeps its bucket; a new one starts full.
    /// </summary>
    public void Apply(double? rate, IReadOnlyList<SamplingRule> rules, double? perSecond)
    {
        var cap = perSecond is not { } n ? null : this.policy.Cap is { } b && b.PerSecond == n ? b : new TokenBucket(n, this.nanos);
        this.policy = new SamplingPolicy(rules, rate, cap);
    }

    public override SamplingResult ShouldSample(in SamplingParameters parameters)
    {
        var parent = ParentOf(parameters.ParentContext);
        if (this.filterDecides && parent == Parent.None && parameters.Name == AspNetCoreRequest)
        {
            return new SamplingResult(SamplingDecision.RecordAndSample); // decided in the Filter
        }

        return this.Decide(parameters, parent);
    }

    /// <summary>
    /// A new trace's ASP.NET Core request, from the instrumentation's Filter, now that its
    /// attributes at start (<paramref name="tags"/>: its path, method, scheme, host) are known:
    /// what to do with it: drop it, record it only, or record and sample it.
    /// </summary>
    public SamplingDecision DecideRequest(Activity activity, IEnumerable<KeyValuePair<string, object?>> tags) =>
        this.Decide(new SamplingParameters(default, activity.TraceId, activity.OperationName, ActivityKind.Server, tags), Parent.None).Decision;

    /// <summary>Where a span's parent is, and whether it was sampled.</summary>
    public static Parent ParentOf(in ActivityContext parent) =>
        parent.TraceId == default ? Parent.None
        : (parent.TraceFlags & ActivityTraceFlags.Recorded) != 0 ? (parent.IsRemote ? Parent.RemoteSampled : Parent.LocalSampled)
        : parent.IsRemote ? Parent.RemoteUnsampled : Parent.LocalUnsampled;

    private SamplingResult Decide(in SamplingParameters p, Parent parent)
    {
        var policy = this.policy;
        SamplingResult result;
        if (!policy.Decides)
        {
            // Nothing set but maybe a cap: the configured sampler decides every span, as before.
            result = this.configured.ShouldSample(p);
        }
        else
        {
            var d = policy.Decide(p.Name, p.Kind, p.Tags, parent);
            result = d.How switch
            {
                How.FollowParent => new SamplingResult(d.ParentSampled ? SamplingDecision.RecordAndSample : SamplingDecision.Drop),
                How.Rate => policy.Ratio(d.Rate).ShouldSample(p),
                _ => this.configured.ShouldSample(p),
            };
        }

        if (policy.Cap is { } cap && result.Decision == SamplingDecision.RecordAndSample &&
            parent is Parent.None or Parent.RemoteSampled && cap.Take(1) == 0)
        {
            this.Capped?.Invoke();
            return new SamplingResult(SamplingDecision.Drop);
        }

        return result;
    }
}
