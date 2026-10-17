// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using OpAmp.Proto.V1;
using OpenTelemetry;
using OpenTelemetry.Trace;
using Xunit;

namespace Pydantic.OpAmp.Extensions.Hosting.Tests;

/// <summary>
/// <see cref="OpAmpAppSettings"/>, which <c>WithOpAmp()</c> fills: one store per process, so these
/// run alone, no other test running.
/// </summary>
[CollectionDefinition(nameof(GlobalAppSettings), DisableParallelization = true)]
public sealed class GlobalAppSettings
{
}

[Collection(nameof(GlobalAppSettings))]
public sealed class AppSettingsTests : IDisposable
{
    private static CancellationToken Cancel => TestContext.Current.CancellationToken;

    public void Dispose() => OpAmpAppSettings.Store.Put(null);

    private static IHost NewHost(string endpoint, string stateDir)
    {
        var b = Microsoft.Extensions.Hosting.Host.CreateApplicationBuilder(new HostApplicationBuilderSettings { DisableDefaults = true });
        var home = Kit.TempDir(); // the user's state directory is the test's own, never the real one
        b.Configuration.AddInMemoryCollection(new Dictionary<string, string?>
        {
            ["OpAmp:Endpoint"] = endpoint,
            ["OpAmp:StateDirectory"] = stateDir,
            ["OpAmp:HeartbeatInterval"] = "100ms",
            ["XDG_STATE_HOME"] = home,
            ["HOME"] = home,
            ["LOCALAPPDATA"] = home,
        });
        b.Services.AddOpenTelemetry().WithTracing(t => t.AddSource("s")).WithOpAmp();
        return b.Build();
    }

    // The server's custom map is what the application reads, a handler it added is told, and with a
    // state directory the values are in force again at the next start, before any config arrives.
    [Fact]
    public async Task WithOpAmpFillsWhatTheApplicationReadsAndKeepsItAcrossARestart()
    {
        OpAmpAppSettings.Store.Put(null);
        var dir = Kit.TempDir();
        const string body = "{\"sampling_rate\":\"0.5\",\"custom\":{\"example.feature\":\"on\",\"limit\":\"9\"}}";
        var told = new List<string>();
        using (OpAmpAppSettings.OnChange((_, changed) => { lock (told) { told.Add(string.Join(",", changed)); } }))
        {
            await using (var server = new OpAmpServer(Kit.Config(body)))
            {
                using var host = NewHost(server.Endpoint, dir);
                await host.StartAsync(Cancel);
                Assert.Contains("custom", host.Services.GetRequiredService<OpAmpAgent>().HostNow(providersBuilt: true).Live);
                await Kit.Until(() => OpAmpAppSettings.Get("example.feature", "off") == "on", "the server's value read");
                Assert.Equal(9, OpAmpAppSettings.GetInt64("limit", 0));
                await Kit.Until(() => server.Messages.Any(m => m.RemoteConfigStatus is { Status: RemoteConfigStatuses.Applied }), "APPLIED");
                await host.StopAsync(Cancel);
            }

            lock (told)
            {
                Assert.Equal(["example.feature,limit"], told);
            }
        }

        // The next start, against a server that offers nothing.
        OpAmpAppSettings.Store.Put(null);
        await using (var quiet = new OpAmpServer())
        {
            using var again = NewHost(quiet.Endpoint, dir);
            await again.StartAsync(Cancel);
            Assert.Equal("on", OpAmpAppSettings.Get("example.feature", "off"));
            await again.StopAsync(Cancel);
        }
    }
}
