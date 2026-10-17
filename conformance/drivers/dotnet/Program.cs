// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Runs OpenTelemetry.OpAmp.Client under the conformance driver contract (conformance/DRIVERS.md).
// The client has no connect or error callbacks and can't change its identification after start,
// so this driver emits neither connect/connect_failed/error_response nor status_change.

using System.Globalization;
using System.Text.Json;
using OpenTelemetry.OpAmp.Client;
using OpenTelemetry.OpAmp.Client.Listeners;
using OpenTelemetry.OpAmp.Client.Messages;
using OpenTelemetry.OpAmp.Client.Settings;

static class Out
{
    private static readonly object Lock = new();

    public static void Emit(string evt, params (string Key, object? Value)[] fields)
    {
        var obj = new Dictionary<string, object?>
        {
            ["event"] = evt,
            ["wall"] = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() / 1000.0,
        };
        foreach (var (k, v) in fields)
        {
            obj[k] = v;
        }

        lock (Lock)
        {
            Console.WriteLine(JsonSerializer.Serialize(obj));
            Console.Out.Flush();
        }
    }
}

static class AgentState
{
    // The last remote config status, for full state reports.
    public static RemoteConfigStatusReport? LastRemoteConfigStatus;
}

sealed class RemoteConfigListener(Func<OpAmpClient> client) : IOpAmpListener<RemoteConfigMessage>
{
    public void HandleMessage(RemoteConfigMessage message)
    {
        var hash = message.ConfigHash.ToArray();
        Out.Emit("message", ("remote_config_hash", Convert.ToHexString(hash).ToLowerInvariant()));
        var status = new RemoteConfigStatusReport(hash, RemoteConfigStatusCode.Applied, string.Empty);
        AgentState.LastRemoteConfigStatus = status;
        // Off the listener's thread, then flushed: the status only goes out on a flush.
        _ = Task.Run(async () =>
        {
            try
            {
                client().SendRemoteConfigStatus(status);
                Out.Emit("remote_config_status_queued");
                await client().FlushAsync();
                Out.Emit("remote_config_status_flushed");
            }
            catch (Exception e)
            {
                Out.Emit("driver_error", ("error", e.ToString()));
            }
        });
    }
}

// The .NET client leaves answering ReportFullState to the application.
sealed class FlagsListener(Func<OpAmpClient> client) : IOpAmpListener<FlagsMessage>
{
    public void HandleMessage(FlagsMessage message)
    {
        Out.Emit("message", ("flags", message.Flags.ToString()));
        if (message.Flags.HasFlag(ServerSentFlags.ReportFullState))
        {
            _ = Task.Run(async () =>
            {
                client().SendFullStateReport(new FullStateReport { RemoteConfigStatus = AgentState.LastRemoteConfigStatus });
                await client().FlushAsync();
            });
        }
    }
}

static class Program
{
    static double Env(string key, double fallback) =>
        double.TryParse(Environment.GetEnvironmentVariable(key), NumberStyles.Float, CultureInfo.InvariantCulture, out var v)
            ? v : fallback;

    static async Task<int> Main()
    {
        var endpoint = new Uri(Environment.GetEnvironmentVariable("OPAMP_ENDPOINT")!);
        var runFor = TimeSpan.FromSeconds(Env("DRIVER_RUN_S", 30));
        var poll = TimeSpan.FromSeconds(Env("DRIVER_POLL_S", 2));

        OpAmpClient? client = null;
        client = new OpAmpClient(opts =>
        {
            opts.ServerUrl = endpoint;
            opts.ConnectionType = endpoint.Scheme.StartsWith("ws", StringComparison.Ordinal)
                ? ConnectionType.WebSocket : ConnectionType.Http;
            opts.Identification.AddIdentifyingAttribute("service.name", "conformance-dotnet");
            opts.Identification.AddNonIdentifyingAttribute("driver.change", "0");
            opts.Heartbeat.Interval = poll;
            opts.RemoteConfiguration.AcceptsRemoteConfig = true;
            opts.RemoteConfiguration.ReportsRemoteConfigStatus = true;
        });
        client.Subscribe(new RemoteConfigListener(() => client!));
        client.Subscribe(new FlagsListener(() => client!));

        await client.StartAsync();
        Out.Emit("started", ("client", "OpenTelemetry.OpAmp.Client"));

        // Not disposed: ProcessExit can fire after Main returns.
        var stop = new CancellationTokenSource();
        AppDomain.CurrentDomain.ProcessExit += (_, _) => stop.Cancel();
        try
        {
            await Task.Delay(runFor, stop.Token);
        }
        catch (TaskCanceledException)
        {
        }

        Out.Emit("stopping");
        using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));
        await client.StopAsync(timeout.Token);
        client.Dispose();
        Out.Emit("stopped");
        return 0;
    }
}
