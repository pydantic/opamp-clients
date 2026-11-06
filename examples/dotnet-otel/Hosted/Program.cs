// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// The requests Program.cs makes, from an application that sets its SDK up in code: the generic
// host, AddOpenTelemetry() with the HttpClient instrumentation and the OTLP exporter (the OTEL_*
// variables), and WithOpAmp() last, the server from OTEL_OPAMP_*. An HTTP request every 200 ms
// until SIGTERM, when the host stops: WithOpAmp says goodbye and the SDK flushes.

using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using OpenTelemetry.Trace;

var builder = Host.CreateApplicationBuilder(args);
builder.Services.AddOpenTelemetry()
    .WithTracing(t => t.AddHttpClientInstrumentation().AddOtlpExporter())
    .WithOpAmp();
builder.Services.AddHostedService(_ => new Requests(args[0]));
await builder.Build().RunAsync();

sealed class Requests(string url) : BackgroundService
{
    protected override async Task ExecuteAsync(CancellationToken stop)
    {
        using var http = new HttpClient { Timeout = TimeSpan.FromSeconds(2) };
        while (!stop.IsCancellationRequested)
        {
            try
            {
                (await http.GetAsync(url, stop)).Dispose();
            }
            catch (Exception e) when (e is HttpRequestException or TaskCanceledException)
            {
                // 404s, and a server gone at the end of a run
            }
            try
            {
                await Task.Delay(200, stop);
            }
            catch (TaskCanceledException)
            {
            }
        }
    }
}
