// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Diagnostics;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using OpenTelemetry;
using OpenTelemetry.Trace;
using Pydantic.OpAmp.Otel;
using Pydantic.OpAmp.Tests;
using Xunit;

namespace Pydantic.OpAmp.Extensions.Hosting.Tests;

/// <summary>sampling_rules in an ASP.NET Core application under <c>WithOpAmp()</c>: the sampler's Filter, after the application's.</summary>
public class SamplingTests
{
    private static CancellationToken Cancel => TestContext.Current.CancellationToken;

    private static async Task<(WebApp App, OpAmpAgent Agent, Spans Spans)> Start(OpAmpServer server, Func<HttpContext, bool>? appFilter = null)
    {
        var spans = new Spans();
        var home = Kit.TempDir();
        var app = await WebApp.Start(b =>
        {
            b.Configuration.AddInMemoryCollection(new Dictionary<string, string?>
            {
                ["OpAmp:Endpoint"] = server.Endpoint,
                ["OpAmp:HeartbeatInterval"] = "100ms",
                ["XDG_STATE_HOME"] = home,
                ["HOME"] = home,
                ["LOCALAPPDATA"] = home,
            });
            b.Services.AddOpenTelemetry()
                .WithTracing(t => t
                    .AddAspNetCoreInstrumentation(o => o.Filter = appFilter)
                    .AddSource(WebApp.Source.Name)
                    .AddProcessor(new SimpleActivityExportProcessor(spans)))
                .WithOpAmp();
        });
        return (app, app.Services.GetRequiredService<OpAmpAgent>(), spans);
    }

    [Fact]
    public async Task ARuleOnARequestPathDropsTheWholeTrace()
    {
        var body = Policy.Of(("sampling_rules", """{"rules":[{"rate":0,"path":"/health*"}]}"""), ("head_sampling", """{"ratio":1}"""));
        await using var server = new OpAmpServer(Kit.Config(body));
        var (app, agent, spans) = await Start(server);
        await using var _ = app;
        Assert.Contains("sampling_rules", agent.HostNow(providersBuilt: true).Live);
        Assert.Contains("sampling_traces_per_second", agent.HostNow(providersBuilt: true).Live);
        await Kit.Until(() => agent.Controls.Sampler.Policy.Rules.Count == 1, "the rules in force");

        var health = await app.Get("/healthz");
        var api = await app.Get("/api/orders/1");
        app.Services.GetRequiredService<TracerProvider>().ForceFlush();
        Assert.Equal(0, spans.Of(health.TraceId));
        Assert.EndsWith("-00", health.Downstream);
        Assert.NotEqual(true, health.ChildRecorded);
        Assert.Equal(3, spans.Of(api.TraceId)); // the request, its child, and the /echo it called
        Assert.Contains(spans.Exported, s => s.Root && s.Path == "/api/orders/1");
    }

    [Fact]
    public async Task TheApplicationsOwnFilterStillRunsFirst()
    {
        var body = Policy.Of(("sampling_rules", """{"rules":[{"rate":1,"kind":"server"}]}"""));
        await using var server = new OpAmpServer(Kit.Config(body));
        var (app, agent, spans) = await Start(server, ctx => !ctx.Request.Path.StartsWithSegments("/private"));
        await using var _ = app;
        await Kit.Until(() => agent.Controls.Sampler.Policy.Rules.Count == 1, "the rules in force");

        var hidden = await app.Get("/private/x");
        var shown = await app.Get("/public/x");
        app.Services.GetRequiredService<TracerProvider>().ForceFlush();
        Assert.Equal(0, spans.Of(hidden.TraceId));
        Assert.Equal(3, spans.Of(shown.TraceId));
    }
}
