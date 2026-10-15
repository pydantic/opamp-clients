// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Diagnostics;
using System.Text.Json;
using Microsoft.AspNetCore.Http;
using OpenTelemetry;
using OpenTelemetry.AutoInstrumentation.PluginApi.Telemetry;
using OpenTelemetry.Instrumentation.AspNetCore;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;
using Pydantic.OpAmp.Tests;
using Xunit;
using SamplingDecision = OpenTelemetry.Trace.SamplingDecision;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

/// <summary>sampling_rules and sampling_traces_per_second, in the live sampler and ASP.NET Core's Filter.</summary>
public class SamplingTests
{
    private long now;

    /// <summary>The application's sampler, told apart from the ratio sampler: it records only.</summary>
    private static readonly Sampler App = new Fixed(SamplingDecision.RecordOnly);

    private Controls Controls() => new(() => this.now);

    private static ActivityKind KindOf(string kind) => kind switch
    {
        "server" => ActivityKind.Server,
        "client" => ActivityKind.Client,
        "producer" => ActivityKind.Producer,
        "consumer" => ActivityKind.Consumer,
        _ => ActivityKind.Internal,
    };

    private static ActivityContext ParentContext(string parent, ActivityTraceId trace) => parent switch
    {
        "none" => default,
        _ => new ActivityContext(
            trace,
            ActivitySpanId.CreateRandom(),
            parent.EndsWith("_sampled", StringComparison.Ordinal) ? ActivityTraceFlags.Recorded : ActivityTraceFlags.None,
            isRemote: parent.StartsWith("remote", StringComparison.Ordinal)),
    };

    private static Parent ParentOf(string parent) => parent switch
    {
        "local_sampled" => Parent.LocalSampled,
        "local_unsampled" => Parent.LocalUnsampled,
        "remote_sampled" => Parent.RemoteSampled,
        "remote_unsampled" => Parent.RemoteUnsampled,
        _ => Parent.None,
    };

    // Every case of testdata/sampling.json: the settings as the layer decides them, put in force by
    // Controls, then the span through the policy's decision and through the sampler itself.
    [Fact]
    public void EverySamplingVectorDecidesAsTheLayerSays()
    {
        var failed = new List<string>();
        foreach (var c in Vectors.Cases("sampling.json"))
        {
            var name = c.GetProperty("name").GetString()!;
            var controls = this.Controls();
            controls.Sampler.Wrap(App);
            controls.Apply(Vectors.Settings(c));
            var span = c.GetProperty("span");
            var tags = span.GetProperty("attributes").EnumerateArray().Select(Vectors.Tag).ToList();
            var kind = KindOf(span.GetProperty("kind").GetString()!);
            var parent = span.GetProperty("parent").GetString()!;
            var want = c.GetProperty("want");
            var got = controls.Sampler.Policy.Decide(span.GetProperty("name").GetString()!, kind, tags, ParentOf(parent));
            var expected = Expected(want);
            if (got != expected)
            {
                failed.Add($"{name}: {got}, not {expected}");
                continue;
            }

            // The sampler, on a few trace ids: what the decision says it does.
            for (var i = 0; i < 16; i++)
            {
                var trace = ActivityTraceId.CreateRandom();
                var p = new SamplingParameters(ParentContext(parent, trace), trace, span.GetProperty("name").GetString()!, kind, tags);
                var decision = controls.Sampler.ShouldSample(p).Decision;
                var should = got.How switch
                {
                    How.FollowParent => got.ParentSampled ? SamplingDecision.RecordAndSample : SamplingDecision.Drop,
                    How.Rate => new TraceIdRatioBasedSampler(got.Rate).ShouldSample(p).Decision,
                    _ => SamplingDecision.RecordOnly,
                };
                if (decision != should)
                {
                    failed.Add($"{name}: the sampler said {decision}, not {should}");
                    break;
                }
            }
        }

        Assert.Empty(failed);
    }

    private static SpanDecision Expected(JsonElement want) => want.GetProperty("how").GetString() switch
    {
        "follow_parent" => new SpanDecision(How.FollowParent, ParentSampled: want.GetProperty("sampled").GetBoolean()),
        "rate" => new SpanDecision(
            How.Rate,
            Rate: want.GetProperty("rate").GetDouble(),
            Rule: want.GetProperty("rule").ValueKind == JsonValueKind.Null ? null : want.GetProperty("rule").GetInt32()),
        _ => new SpanDecision(How.AppSampler),
    };

    // The traces cases of testdata/caps.json: each event a span the application's sampler decides
    // (as "sampled" says), through the live sampler on an injected monotonic clock.
    [Fact]
    public void EveryTracesCapVectorHoldsInTheSampler()
    {
        var failed = new List<string>();
        foreach (var c in Vectors.Cases("caps.json").Where(c => c.GetProperty("signal").GetString() == "traces"))
        {
            var controls = this.Controls();
            var sampled = false;
            controls.Sampler.Wrap(new Fixed(() => sampled ? SamplingDecision.RecordAndSample : SamplingDecision.Drop));
            controls.Apply(Vectors.Settings(c));
            if (controls.Sampler.Policy.Decides)
            {
                // A debug rate of 1 decides instead: new traces are sampled, parents followed. The
                // vectors' events say the same.
                Assert.Equal(1, controls.Sampler.Policy.Rate);
            }

            var got = new List<long>();
            foreach (var e in c.GetProperty("events").EnumerateArray())
            {
                this.now = e.GetProperty("at_ns").GetInt64();
                sampled = e.GetProperty("sampled").GetBoolean();
                var trace = ActivityTraceId.CreateRandom();
                var p = new SamplingParameters(ParentContext(e.GetProperty("parent").GetString()!, trace), trace, "span", ActivityKind.Server);
                got.Add(controls.Sampler.ShouldSample(p).Decision == SamplingDecision.RecordAndSample ? 1 : 0);
            }

            if (!got.SequenceEqual(c.GetProperty("want").EnumerateArray().Select(w => w.GetInt64())))
            {
                failed.Add($"{c.GetProperty("name").GetString()}: {string.Join(",", got)}");
            }
        }

        Assert.Empty(failed);
    }

    [Fact]
    public void ARateOfAHalfKeepsHalfTheTracesByTheirIds()
    {
        var controls = this.Controls();
        controls.Apply(Vectors.Settings("{\"sampling_rules\":[{\"rate\":0.5,\"name\":\"GET\"}]}"));
        var kept = 0;
        for (var i = 0; i < 10_000; i++)
        {
            var trace = ActivityTraceId.CreateRandom();
            var p = new SamplingParameters(default, trace, "GET", ActivityKind.Server);
            var decision = controls.Sampler.ShouldSample(p).Decision;
            Assert.Equal(new TraceIdRatioBasedSampler(0.5).ShouldSample(p).Decision, decision);
            kept += decision == SamplingDecision.RecordAndSample ? 1 : 0;
        }

        Assert.InRange(kept, 4_700, 5_300);
    }

    [Fact]
    public void RulesComeAndGoWhileTheApplicationRuns()
    {
        var controls = this.Controls();
        controls.Sampler.Wrap(App);
        var health = new SamplingParameters(default, ActivityTraceId.CreateRandom(), "GET", ActivityKind.Server, [new("url.path", "/health")]);
        var other = new SamplingParameters(default, ActivityTraceId.CreateRandom(), "GET", ActivityKind.Server, [new("url.path", "/checkout")]);
        Assert.Equal(SamplingDecision.RecordOnly, controls.Sampler.ShouldSample(health).Decision);

        controls.Apply(Vectors.Settings("{\"sampling_rules\":[{\"rate\":0,\"path\":\"/health\"}]}"));
        Assert.Equal(SamplingDecision.Drop, controls.Sampler.ShouldSample(health).Decision);
        Assert.Equal(SamplingDecision.RecordOnly, controls.Sampler.ShouldSample(other).Decision); // no rule, no rate: the app's

        controls.Apply(Vectors.Settings("{\"sampling_rules\":[{\"rate\":1,\"path\":\"/health\"}],\"sampling_rate\":\"0\"}"));
        Assert.Equal(SamplingDecision.RecordAndSample, controls.Sampler.ShouldSample(health).Decision);
        Assert.Equal(SamplingDecision.Drop, controls.Sampler.ShouldSample(other).Decision);

        // The rules left out: the application's sampler again, for every span.
        controls.Apply(new Settings());
        Assert.Equal(SamplingDecision.RecordOnly, controls.Sampler.ShouldSample(health).Decision);
        Assert.Equal(SamplingDecision.RecordOnly, controls.Sampler.ShouldSample(other).Decision);
    }

    [Fact]
    public void TheCapCountsOnlyWhatWasSampledAndItsLocalChildrenGoFree()
    {
        var controls = this.Controls();
        var refused = 0;
        controls.Sampler.Capped = () => refused++;
        controls.Apply(Vectors.Settings("{\"sampling_rules\":[{\"rate\":0,\"name\":\"health\"}],\"sampling_rate\":\"1\",\"sampling_traces_per_second\":\"2\"}"));
        SamplingResult Root(string name) => controls.Sampler.ShouldSample(new SamplingParameters(default, ActivityTraceId.CreateRandom(), name, ActivityKind.Server));
        SamplingResult Child() => controls.Sampler.ShouldSample(new SamplingParameters(
            new ActivityContext(ActivityTraceId.CreateRandom(), ActivitySpanId.CreateRandom(), ActivityTraceFlags.Recorded), default, "child", ActivityKind.Internal));

        for (var i = 0; i < 5; i++)
        {
            Assert.Equal(SamplingDecision.Drop, Root("health").Decision); // not sampled: no token
        }

        Assert.Equal(SamplingDecision.RecordAndSample, Root("a").Decision);
        Assert.Equal(SamplingDecision.RecordAndSample, Child().Decision);
        Assert.Equal(SamplingDecision.RecordAndSample, Root("b").Decision);
        Assert.Equal(SamplingDecision.RecordAndSample, Child().Decision); // no token taken
        Assert.Equal(SamplingDecision.Drop, Root("c").Decision);
        Assert.Equal(1, refused);
        this.now = 500_000_000;
        Assert.Equal(SamplingDecision.RecordAndSample, Root("d").Decision);

        // The cap left out: unlimited again; a cap again: a full bucket.
        controls.Apply(Vectors.Settings("{\"sampling_rate\":\"1\"}"));
        Assert.All(Enumerable.Range(0, 10), _ => Assert.Equal(SamplingDecision.RecordAndSample, Root("e").Decision));
        controls.Apply(Vectors.Settings("{\"sampling_traces_per_second\":\"1\"}"));
        Assert.Equal(SamplingDecision.RecordAndSample, Root("f").Decision);
        Assert.Equal(SamplingDecision.Drop, Root("g").Decision);
    }

    [Fact]
    public void ACapAloneHoldsTheApplicationsSampler()
    {
        var controls = this.Controls();
        controls.Sampler.Wrap(new ParentBasedSampler(new AlwaysOnSampler()));
        controls.Apply(Vectors.Settings("{\"sampling_traces_per_second\":\"1\"}"));
        Assert.False(controls.Sampler.Policy.Decides);
        var first = new SamplingParameters(default, ActivityTraceId.CreateRandom(), "a", ActivityKind.Server);
        Assert.Equal(SamplingDecision.RecordAndSample, controls.Sampler.ShouldSample(first).Decision);
        Assert.Equal(SamplingDecision.Drop, controls.Sampler.ShouldSample(first).Decision);
    }

    [Fact]
    public void ThePluginDeclaresTheRulesAndTheCapWhereItsSamplerIsInForce()
    {
        var agent = Agent.Prepare(n => n == "OTEL_OPAMP_ENDPOINT" ? "http://127.0.0.1:1/v1/opamp" : null, (_, _) => { })!;
        var before = agent.HostNow(providersBuilt: false);
        Assert.Contains("sampling_rules", before.Live);
        Assert.Contains("sampling_traces_per_second", before.Live);
        // The SDK built without the plugin's sampler: none of it is carried out.
        var after = agent.HostNow(providersBuilt: true);
        Assert.DoesNotContain("sampling_rules", after.Live);
        Assert.DoesNotContain("sampling_traces_per_second", after.Live);
        Assert.True(typeof(IConfigureTracesOptions<AspNetCoreTraceInstrumentationOptions>).IsAssignableFrom(typeof(Plugin)));
    }

    // ASP.NET Core's requests reach the sampler with no path; the Filter decides them.
    [Fact]
    public async Task ARuleOnARequestPathDropsTheWholeTraceInTheFilter()
    {
        await using var rig = await Rig.Start(this, "{\"sampling_rules\":[{\"rate\":0,\"path\":\"/health*\"},{\"rate\":1,\"kind\":\"server\",\"path\":\"/api/*\"}]}");
        var health = new List<Answer>();
        var api = new List<Answer>();
        for (var i = 0; i < 5; i++)
        {
            health.Add(await rig.App.Get("/health"));
            api.Add(await rig.App.Get($"/api/orders/{i}"));
        }

        rig.Flush();
        foreach (var r in health)
        {
            Assert.Equal(0, rig.Spans.Of(r.TraceId)); // the request, its child and the call it made
            Assert.NotEqual(true, r.ChildRecorded);
            Assert.EndsWith("-00", r.Downstream);
        }

        foreach (var r in api)
        {
            Assert.Equal(1, rig.Spans.RootsOf(r.TraceId));
            Assert.Equal(3, rig.Spans.Of(r.TraceId)); // the request, "work" and the /echo it called
            Assert.True(r.ChildRecorded);
            Assert.EndsWith("-01", r.Downstream);
        }

        Assert.Contains(rig.Spans.Exported, s => s.Path == "/api/orders/0" && s.Root);

        // The rules changed while the application runs: /health is sampled now.
        rig.Agent.RemoteConfig(Vectors.Config("{\"sampling_rate\":\"1\"}"));
        var now = await rig.App.Get("/health");
        rig.Flush();
        Assert.Equal(3, rig.Spans.Of(now.TraceId));
    }

    [Fact]
    public async Task ARequestPathRuleAtAHalfFollowsTheTraceIdRatio()
    {
        await using var rig = await Rig.Start(this, "{\"sampling_rules\":[{\"rate\":0.5,\"path\":\"/api/*\"}]}");
        var kept = 0;
        for (var i = 0; i < 60; i++)
        {
            var r = await rig.App.Get($"/api/{i}");
            var p = new SamplingParameters(default, ActivityTraceId.CreateFromString(r.TraceId), "x", ActivityKind.Server);
            var expected = new TraceIdRatioBasedSampler(0.5).ShouldSample(p).Decision == SamplingDecision.RecordAndSample;
            rig.Flush();
            Assert.Equal(expected ? 3 : 0, await rig.Spans.OfAtLeast(r.TraceId, expected ? 3 : 0));
            kept += expected ? 1 : 0;
        }

        Assert.InRange(kept, 10, 50);
    }

    [Fact]
    public async Task ARemoteParentDecidesARequestWhateverItsPath()
    {
        await using var rig = await Rig.Start(this, "{\"sampling_rules\":[{\"rate\":0,\"path\":\"/zero\"},{\"rate\":1,\"path\":\"/one\"}]}");
        var sampled = ActivityTraceId.CreateRandom();
        var unsampled = ActivityTraceId.CreateRandom();
        var a = await rig.App.Get("/zero", $"00-{sampled}-{ActivitySpanId.CreateRandom()}-01");
        var b = await rig.App.Get("/one", $"00-{unsampled}-{ActivitySpanId.CreateRandom()}-00");
        rig.Flush();
        Assert.Equal(3, rig.Spans.Of(sampled.ToString()));
        Assert.True(a.ChildRecorded);
        Assert.Equal(0, rig.Spans.Of(unsampled.ToString()));
        Assert.EndsWith("-00", b.Downstream);
    }

    [Fact]
    public async Task TheApplicationsFilterRunsFirstAndTheTraceCapCountsRequests()
    {
        await using var rig = await Rig.Start(
            this,
            "{\"sampling_rate\":\"1\",\"sampling_traces_per_second\":\"4\"}",
            ctx => !ctx.Request.Path.StartsWithSegments("/private"));
        var refused = 0;
        rig.Agent.Controls.Sampler.Capped = () => refused++;
        var hidden = await rig.App.Get("/private/x");
        rig.Flush();
        Assert.Equal(0, rig.Spans.Of(hidden.TraceId));
        Assert.NotEqual(true, hidden.ChildRecorded);

        // The application's false took no token. Each request takes two: its own, and its call to
        // /echo, whose sampled remote parent takes one too. Two requests go, the third is capped,
        // and its call says not sampled, so takes none.
        var first = await rig.App.Get("/a");
        var second = await rig.App.Get("/b");
        var third = await rig.App.Get("/c");
        rig.Flush();
        Assert.Equal(3, rig.Spans.Of(first.TraceId));
        Assert.Equal(3, rig.Spans.Of(second.TraceId));
        Assert.Equal(0, rig.Spans.Of(third.TraceId));
        Assert.EndsWith("-00", third.Downstream);
        Assert.Equal(1, refused);
    }

    [Fact]
    public async Task ARequestTheApplicationsSamplerOnlyRecordsIsRecordedNotExported()
    {
        // No rule matches /a, so the application's sampler decides it: RecordOnly.
        await using var rig = await Rig.Start(
            this, "{\"sampling_rules\":[{\"rate\":1,\"path\":\"/health\"}]}", appSampler: new Fixed(SamplingDecision.RecordOnly));
        var r = await rig.App.Get("/a");
        rig.Flush();
        Assert.Equal(1, rig.Spans.EndedRootsOf(r.TraceId)); // the processors saw the request
        Assert.Equal(0, rig.Spans.Of(r.TraceId)); // nothing exported
        Assert.Equal("None", r.ServerFlags);
        Assert.EndsWith("-00", r.Downstream);
    }

    /// <summary>
    /// The plugin's way in, as automatic instrumentation 1.17's AspNetCoreInitializer does it: new
    /// options, each plugin's <c>ConfigureTracesOptions</c>, then the instrumentation's listener.
    /// </summary>
    private sealed class Rig : IAsyncDisposable
    {
        private readonly TracerProvider provider;
        private readonly IDisposable instrumentation;

        private Rig(Agent agent, TracerProvider provider, IDisposable instrumentation, WebApp app, Spans spans)
        {
            (this.Agent, this.provider, this.instrumentation, this.App, this.Spans) = (agent, provider, instrumentation, app, spans);
        }

        public Agent Agent { get; }

        public WebApp App { get; }

        public Spans Spans { get; }

        public static async Task<Rig> Start(SamplingTests test, string settings, Func<HttpContext, bool>? appFilter = null, Sampler? appSampler = null)
        {
            // The monotonic clock stands still: the trace cap refills nothing however slow the requests.
            var agent = Agent.Prepare(
                n => n == "OTEL_OPAMP_ENDPOINT" ? "http://127.0.0.1:1/v1/opamp" : null, (_, _) => { }, new Clock(() => Vectors.Now, () => 0))!;
            agent.RemoteConfig(Vectors.Config(settings));
            var spans = new Spans();
            var builder = Sdk.CreateTracerProviderBuilder()
                .AddSource("Microsoft.AspNetCore", WebApp.Source.Name)
                .AddProcessor(spans.Ends)
                .AddProcessor(new SimpleActivityExportProcessor(spans));
            if (appSampler is not null)
            {
                builder.SetSampler(appSampler);
            }

            var provider = builder.Let(agent.WrapSampler).Build()!;

            var options = new AspNetCoreTraceInstrumentationOptions { Filter = appFilter };
            agent.Filter(options); // Plugin.ConfigureTracesOptions(AspNetCoreTraceInstrumentationOptions)
            var listener = Activator.CreateInstance(
                Type.GetType("OpenTelemetry.Instrumentation.AspNetCore.Implementation.HttpInListener, OpenTelemetry.Instrumentation.AspNetCore", true)!, options)!;
            var instrumentation = (IDisposable)Activator.CreateInstance(
                Type.GetType("OpenTelemetry.Instrumentation.AspNetCore.AspNetCoreInstrumentation, OpenTelemetry.Instrumentation.AspNetCore", true)!, listener)!;
            return new Rig(agent, provider, instrumentation, await WebApp.Start(), spans);
        }

        public void Flush() => this.provider.ForceFlush();

        public async ValueTask DisposeAsync()
        {
            await this.App.DisposeAsync();
            this.instrumentation.Dispose();
            this.provider.Dispose();
            this.Agent.Stop(TimeSpan.FromSeconds(1));
        }
    }

    private sealed class Fixed : Sampler
    {
        private readonly Func<SamplingDecision> decision;

        public Fixed(SamplingDecision decision) => this.decision = () => decision;

        public Fixed(Func<SamplingDecision> decision) => this.decision = decision;

        public override SamplingResult ShouldSample(in SamplingParameters samplingParameters) => new(this.decision());
    }
}
