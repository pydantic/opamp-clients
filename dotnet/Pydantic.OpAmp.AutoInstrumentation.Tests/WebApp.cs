// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Diagnostics;
using System.Text.Json;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Http;
using Microsoft.Extensions.Logging;
using OpenTelemetry;

namespace Pydantic.OpAmp.Tests;

/// <summary>What a request saw: its server span's flags, whether its child span was recorded, the traceparent its outgoing call sent.</summary>
internal sealed record Answer(string? ServerFlags, bool? ChildRecorded, string? Downstream, string TraceId);

/// <summary>
/// A real Kestrel application on .NET 8: each request starts a child span ("work", of
/// <see cref="Source"/>) and calls <c>/echo</c> on itself, which answers the traceparent it was sent.
/// </summary>
internal sealed class WebApp : IAsyncDisposable
{
    public static readonly ActivitySource Source = new("webapp");
    private static readonly HttpClient Client = new();
    private readonly WebApplication app;

    private WebApp(WebApplication app) => this.app = app;

    public string Url => this.app.Urls.First();

    public IServiceProvider Services => this.app.Services;

    /// <summary>Start the application; <paramref name="configure"/> adds to its builder before it is built.</summary>
    public static async Task<WebApp> Start(Action<WebApplicationBuilder>? configure = null)
    {
        var builder = WebApplication.CreateSlimBuilder();
        builder.Logging.ClearProviders();
        builder.WebHost.UseUrls("http://127.0.0.1:0");
        configure?.Invoke(builder);
        var app = builder.Build();
        app.MapGet("/echo", (HttpRequest r) => r.Headers.TryGetValue("traceparent", out var v) ? v.ToString() : "none");
        app.MapGet("/{**rest}", async (HttpContext ctx) =>
        {
            var server = Activity.Current;
            bool? recorded;
            using (var child = Source.StartActivity("work"))
            {
                recorded = child?.Recorded;
            }

            var downstream = await Client.GetStringAsync($"{ctx.Request.Scheme}://{ctx.Request.Host}/echo");
            return Results.Text(JsonSerializer.Serialize(new Answer(server?.ActivityTraceFlags.ToString(), recorded, downstream, server?.TraceId.ToString() ?? string.Empty)));
        });
        await app.StartAsync();
        return new WebApp(app);
    }

    /// <summary>GET <paramref name="path"/>, with a traceparent if given; never the test's own activity.</summary>
    public async Task<Answer> Get(string path, string? traceparent = null)
    {
        using var request = new HttpRequestMessage(HttpMethod.Get, this.Url + path);
        if (traceparent is not null)
        {
            request.Headers.TryAddWithoutValidation("traceparent", traceparent);
        }

        var current = Activity.Current;
        Activity.Current = null;
        try
        {
            using var response = await Client.SendAsync(request);
            response.EnsureSuccessStatusCode();
            return JsonSerializer.Deserialize<Answer>(await response.Content.ReadAsStringAsync())!;
        }
        finally
        {
            Activity.Current = current;
        }
    }

    public async ValueTask DisposeAsync()
    {
        await this.app.StopAsync();
        await this.app.DisposeAsync();
    }
}

/// <summary>The spans exported: (trace id, name, whether a root).</summary>
internal sealed class Spans : BaseExporter<Activity>
{
    public ConcurrentQueue<(string TraceId, string Name, bool Root, string? Path)> Exported { get; } = new();

    public int Of(string traceId) => this.Exported.Count(s => s.TraceId == traceId);

    /// <summary>
    /// The spans of a trace exported once `count` of them are, or five seconds have passed: the
    /// server's request span ends after the response is written, so it may end, and be exported,
    /// after the client has the response.
    /// </summary>
    public async Task<int> OfAtLeast(string traceId, int count)
    {
        var waited = Stopwatch.StartNew();
        while (this.Of(traceId) < count && waited.Elapsed < TimeSpan.FromSeconds(5))
        {
            await Task.Delay(10);
        }

        return this.Of(traceId);
    }

    public int RootsOf(string traceId) => this.Exported.Count(s => s.TraceId == traceId && s.Root);

    /// <summary>A processor that sees every recorded activity end, sampled or not.</summary>
    public EndsSeen Ends { get; } = new();

    /// <summary>The root activities of a trace the processors saw end.</summary>
    public int EndedRootsOf(string traceId) => this.Ends.Roots.Count(t => t == traceId);

    public sealed class EndsSeen : BaseProcessor<Activity>
    {
        public ConcurrentQueue<string> Roots { get; } = new();

        public override void OnEnd(Activity data)
        {
            if (data.ParentSpanId == default)
            {
                this.Roots.Enqueue(data.TraceId.ToString());
            }
        }
    }

    public override ExportResult Export(in Batch<Activity> batch)
    {
        foreach (var a in batch)
        {
            this.Exported.Enqueue((a.TraceId.ToString(), a.OperationName, a.ParentSpanId == default, a.GetTagItem("url.path") as string));
        }

        return ExportResult.Success;
    }
}
