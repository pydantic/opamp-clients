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

using System.Collections.Concurrent;
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
        (await http.GetAsync(args[0], stop.Token)).Dispose();
        log.LogInformation("requested {Url}", args[0]);
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

foreach (var provider in ownSdk)
{
    provider.Dispose();
}
