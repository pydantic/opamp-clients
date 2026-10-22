// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// An application with no OpenTelemetry code at all: an HTTP request every 200 ms, and a log line
// for each through ILogger (logger "app"), plus one from a noisy library's logger ("app.noisy"),
// until SIGTERM. Run under automatic instrumentation (instrument.sh), its HttpClient
// instrumentation traces (and measures) the requests and its ILogger instrumentation exports the
// log lines; the plugin puts the SDK under the OpAMP server.
//
// With APP_LISTEN set (http://127.0.0.1:5080, say) it also serves /ping and /health with ASP.NET
// Core, whose instrumentation traces each request it is sent: what sampling_rules' path matches.
//
// It also reads a setting of its own the server sends (docs/app-settings.md): a feature flag,
// example.feature, through OpAmpAppSettings, which the plugin fills. While the server says "on",
// each round also requests /feature; and each time its settings change it requests /changed,
// once. Without a server, or until the server says, the flag is its default, "off".
//
// With EXAMPLE_PROBE=1 (integrations.py's run 3) each round also probes redaction: an activity of
// its own, example.probe (from the source "app", which instrumentation exports when
// OTEL_DOTNET_AUTO_TRACES_ADDITIONAL_SOURCES names it), with the URL it requests (url.full) on an
// event and on a link to the round before's probe, and a log line example.probe with that URL;
// each with a note (example.note) that says password, for scrubbing to replace.
// It probes before or after the request, at random: under a rate limit of one a second, the first
// record after the token comes takes it, and in a fixed order the probe might never be that one.

using System.Collections.Concurrent;
using System.Diagnostics;
using System.Runtime.InteropServices;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Hosting;
using Microsoft.Extensions.Logging;
using Pydantic.OpAmp;

using var stop = new CancellationTokenSource();
using var term = PosixSignalRegistration.Create(PosixSignal.SIGTERM, ctx =>
{
    ctx.Cancel = true; // return from Main: the plugin says goodbye and the SDK flushes as the process exits
    stop.Cancel();
});

// check_truth's setups (Setups.cs, EXAMPLE_SETUP): none otherwise.
var ownSdk = Example.Setup.Has("own-sdk") ? Example.OwnSdk.Build() : [];

using var loggers = LoggerFactory.Create(_ => { });
var log = loggers.CreateLogger("app");
var noisy = loggers.CreateLogger("app.noisy");
var probing = Environment.GetEnvironmentVariable("EXAMPLE_PROBE") == "1";
using var probes = new ActivitySource("app");
ActivityContext lastProbe = default;

WebApplication? server = null;
if (Environment.GetEnvironmentVariable("APP_LISTEN") is { Length: > 0 } listen)
{
    var builder = WebApplication.CreateSlimBuilder();
    builder.Logging.ClearProviders();
    builder.WebHost.UseUrls(listen);
    server = builder.Build();
    server.MapGet("/ping", () => { log.LogInformation("served {Path}", "/ping"); return "pong"; });
    server.MapGet("/health", () => "ok");
    await server.StartAsync(stop.Token);
}

// Told on the OpAMP client's thread: keep it quick, and do the work in the main loop.
var changes = new ConcurrentQueue<string>();
using var subscription = OpAmpAppSettings.OnChange((_, changed) => changes.Enqueue(string.Join(", ", changed)));
var root = args[0].EndsWith("/ping", StringComparison.Ordinal) ? args[0][..^"/ping".Length] : args[0];

using var http = new HttpClient();
while (!stop.IsCancellationRequested)
{
    try
    {
        var early = Random.Shared.Next(2) == 1;
        if (probing && early)
        {
            lastProbe = Probe(args[0], lastProbe);
        }

        (await http.GetAsync(args[0], stop.Token)).Dispose();
        log.LogInformation("requested {Url}", args[0]);
        if (probing && !early)
        {
            lastProbe = Probe(args[0], lastProbe);
        }
        noisy.LogInformation("connection pool: {Idle} idle", 3);
        if (OpAmpAppSettings.Get("example.feature", "off") == "on")
        {
            (await http.GetAsync($"{root}/feature", stop.Token)).Dispose();
        }

        while (changes.TryDequeue(out var keys))
        {
            log.LogInformation("settings changed: {Keys}", keys);
            (await http.GetAsync($"{root}/changed", stop.Token)).Dispose();
        }

        await Task.Delay(200, stop.Token);
    }
    catch (Exception) when (stop.IsCancellationRequested)
    {
    }
    catch (HttpRequestException)
    {
    }
}

if (server is not null)
{
    await server.StopAsync();
}

// An activity of its own with url on an event and on a link to last (none while it is the
// default), and a log line with url: what redaction may drop or mask. Returns the activity's
// context, for the next probe to link to (the last one's while none is sampled). Each carries a
// note too (example.note), which says password, so scrubbing's default patterns replace it.
ActivityContext Probe(string url, ActivityContext last)
{
    const string note = "the password is p4ss";
    var tags = new ActivityTagsCollection { ["url.full"] = url, ["example.note"] = note };
    var links = last == default ? null : new[] { new ActivityLink(last, tags) };
    using (var activity = probes.StartActivity("example.probe", ActivityKind.Internal, default(ActivityContext), links: links))
    {
        activity?.AddEvent(new ActivityEvent("example.probe", tags: tags));
        last = activity?.Context ?? last;
    }

    KeyValuePair<string, object?>[] state = [new("url.full", url), new("example.note", note), new("{OriginalFormat}", "example.probe")];
    log.Log(LogLevel.Information, default, state, null, (_, _) => "example.probe");
    return last;
}

foreach (var provider in ownSdk)
{
    provider.Dispose();
}
