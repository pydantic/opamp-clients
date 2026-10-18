// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Diagnostics;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// Stop ends when the core says, and nothing the client started outlives it; the shell bugs of
/// the audit (S1, S2, S4, S5, S7), each shown by a test that failed before. Those of the network
/// run in both modes (.NET's HTTP and WebSocket, and core mode's streams).
/// </summary>
public class StopTests
{
    // The goodbye is waited for until the core's stop timeout, no longer, then aborted.
    // covers: RULE-CLIENT-16
    [Theory]
    [InlineData(OpAmpMode.Platform)]
    [InlineData(OpAmpMode.Core)]
    public async Task StopEndsWhenTheCoreSays(OpAmpMode mode)
    {
        var goodbye = new TaskCompletionSource<RawConn>();
        var requests = 0;
        using var server = new RawServer(async c =>
        {
            while (true)
            {
                var (_, headers) = await c.ReadHeadAsync();
                if (Interlocked.Increment(ref requests) == 1)
                {
                    await c.AnswerAsync(headers);
                    continue;
                }

                goodbye.TrySetResult(c); // never answered
                await Task.Delay(Timeout.Infinite);
            }
        });
        var client = OpAmpClient.Start(
            new OpAmpClientOptions { Endpoint = new Uri(server.Url("http")), HeartbeatInterval = TimeSpan.FromHours(1), StopTimeout = TimeSpan.FromMilliseconds(800), Mode = mode, DirectConnection = mode == OpAmpMode.Core },
            new OpAmpStatus());
        Assert.True(await Wait.ForAsync(() => Volatile.Read(ref requests) >= 1));
        await Task.Delay(200); // the answer is in

        var clock = Stopwatch.StartNew();
        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));

        Assert.InRange(clock.ElapsedMilliseconds, 600, 4000);
        var held = await goodbye.Task.WaitAsync(TimeSpan.FromSeconds(1));
        Assert.True(await held.EndsWithinAsync(TimeSpan.FromSeconds(2)), "the goodbye outlived stop");
    }

    // S1: a stop during the WebSocket handshake returns, and the connection, if it opens after,
    // is closed at once.
    [Theory]
    [InlineData(OpAmpMode.Platform)]
    [InlineData(OpAmpMode.Core)]
    public async Task S1_StopDuringTheHandshakeReturnsAndClosesTheConnection(OpAmpMode mode)
    {
        var ended = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        using var server = new RawServer(async c =>
        {
            var (_, headers) = await c.ReadHeadAsync();
            if (await c.EndsWithinAsync(TimeSpan.FromSeconds(2)))
            {
                ended.TrySetResult(true); // given up on before it opened
                return;
            }

            try
            {
                await c.AcceptAsync(headers);
                ended.TrySetResult(await c.EndsWithinAsync(TimeSpan.FromSeconds(5)));
            }
            catch (IOException)
            {
                ended.TrySetResult(true);
            }
        });
        var client = OpAmpClient.Start(new OpAmpClientOptions { Endpoint = new Uri(server.Url("ws")), Mode = mode, DirectConnection = mode == OpAmpMode.Core }, new OpAmpStatus());
        await Task.Delay(300);

        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(5));

        Assert.True(await ended.Task.WaitAsync(TimeSpan.FromSeconds(10)), "the connection stayed open after stop");
    }

    // S2: an upgrade that is never answered is given up on (the core's connect timeout, carried
    // out by aborting the dial), and the client tries again.
    [Theory]
    [InlineData(OpAmpMode.Platform)]
    [InlineData(OpAmpMode.Core)]
    public async Task S2_AnUpgradeNeverAnsweredIsAbortedAndTriedAgain(OpAmpMode mode)
    {
        var upgrades = 0;
        using var server = new RawServer(async c =>
        {
            await c.ReadHeadAsync();
            Interlocked.Increment(ref upgrades);
            await c.EndsWithinAsync(TimeSpan.FromMinutes(5)); // never answered
        });
        var client = OpAmpClient.Start(
            new OpAmpClientOptions { Endpoint = new Uri(server.Url("ws")), ConnectTimeout = TimeSpan.FromSeconds(1), Mode = mode, DirectConnection = mode == OpAmpMode.Core },
            new OpAmpStatus());
        try
        {
            Assert.True(await Wait.ForAsync(() => Volatile.Read(ref upgrades) >= 2, 20), "no second attempt");
            Assert.True(await server.Conns.First().EndsWithinAsync(TimeSpan.FromSeconds(1)), "the first dial was not aborted");
        }
        finally
        {
            await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        }
    }

    // S4: a slow save holds up neither the heartbeats nor stop, and the next client on the state
    // file waits for it and starts from what it saved.
    [Fact]
    public async Task S4_ASlowSaveHoldsUpNothingAndTheNextClientWaitsForIt()
    {
        using var server = new TestServer();
        var dir = Directory.CreateTempSubdirectory();
        var state = Path.Combine(dir.FullName, "state");
        var full = Path.GetFullPath(state);
        using var held = new ManualResetEventSlim();
        using var release = new ManualResetEventSlim();
        var first = 0;
        void Hold(string path)
        {
            if (Path.GetFullPath(path) == full && Interlocked.Exchange(ref first, 1) == 0)
            {
                held.Set();
                release.Wait(TimeSpan.FromSeconds(30));
            }
        }

        StateFile.Saving += Hold;
        try
        {
            var options = new OpAmpClientOptions
            {
                Mode = OpAmpMode.Platform,
                Endpoint = new Uri(server.Endpoint("http")),
                HeartbeatInterval = TimeSpan.FromMilliseconds(100),
                StateFile = state,
                StopTimeout = TimeSpan.FromMilliseconds(500),
            };
            var a = OpAmpClient.Start(options, new OpAmpStatus());
            Assert.True(held.Wait(TimeSpan.FromSeconds(10)), "no save");
            // While the save is held, three heartbeats come: waited for, not timed, so a busy
            // machine's late heartbeats don't count against the client.
            var before = server.Messages.Count;
            var waited = Stopwatch.StartNew();
            while (server.Messages.Count - before < 3 && waited.Elapsed < TimeSpan.FromSeconds(10))
            {
                await Task.Delay(20);
            }

            Assert.True(server.Messages.Count - before >= 3, $"heartbeats held up by the save: {server.Messages.Count - before} in 10 s");
            var clock = Stopwatch.StartNew();
            await a.StopAsync().WaitAsync(TimeSpan.FromSeconds(5));
            Assert.True(clock.ElapsedMilliseconds < 3000, $"stop waited for the save: {clock.ElapsedMilliseconds} ms");

            var b = Task.Run(() => OpAmpClient.Start(options, new OpAmpStatus()));
            await Task.Delay(300);
            Assert.False(b.IsCompleted, "the next client started while a save was still writing");
            release.Set();
            var started = await b.WaitAsync(TimeSpan.FromSeconds(10));
            Assert.Equal(a.InstanceUid, started.InstanceUid);
            await started.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        }
        finally
        {
            release.Set();
            StateFile.Saving -= Hold;
        }
    }

    // ReadState right after StopAsync: a save the client left writing lands first, and what it
    // saved is what is read, as the next client on the state file would start from it.
    [Fact]
    public async Task ReadStateWaitsForTheSaveAStoppedClientLeftWriting()
    {
        using var server = new TestServer();
        var dir = Directory.CreateTempSubdirectory();
        var state = Path.Combine(dir.FullName, "state");
        var full = Path.GetFullPath(state);
        using var held = new ManualResetEventSlim();
        using var release = new ManualResetEventSlim();
        var first = 0;
        void Hold(string path)
        {
            if (Path.GetFullPath(path) == full && Interlocked.Exchange(ref first, 1) == 0)
            {
                held.Set();
                release.Wait(TimeSpan.FromSeconds(30));
            }
        }

        StateFile.Saving += Hold;
        try
        {
            var options = new OpAmpClientOptions
            {
                Mode = OpAmpMode.Platform,
                Endpoint = new Uri(server.Endpoint("http")),
                HeartbeatInterval = TimeSpan.FromHours(1),
                StateFile = state,
                StopTimeout = TimeSpan.FromMilliseconds(300),
            };
            var a = OpAmpClient.Start(options, new OpAmpStatus());
            Assert.True(held.Wait(TimeSpan.FromSeconds(10)), "no save");
            await a.StopAsync().WaitAsync(TimeSpan.FromSeconds(5));
            Assert.False(File.Exists(state), "the save landed before it was let go");

            var read = Task.Run(() => OpAmpClient.ReadState(state));
            await Task.Delay(300);
            Assert.False(read.IsCompleted, "read while the save was still writing");
            release.Set();
            var got = await read.WaitAsync(TimeSpan.FromSeconds(10));
            Assert.NotEmpty(got);
            Assert.Equal(File.ReadAllBytes(state), got);
            Assert.Empty(OpAmpClient.ReadState(Path.Combine(dir.FullName, "none")));
        }
        finally
        {
            release.Set();
            StateFile.Saving -= Hold;
        }
    }

    // S5: a remote config saved before a restart is handed over as the client starts; the
    // callback answers through the client it is given, before Start has even returned to a caller
    // that is slow to take it.
    [Fact]
    public async Task S5_ACallbackAtStartAnswersThroughTheClient()
    {
        var config = new AgentRemoteConfig { ConfigHash = ByteString.CopyFromUtf8("hash-1"), Config = new AgentConfigMap() };
        config.Config.ConfigMap.Add("a", new AgentConfigObject { Body = ByteString.CopyFromUtf8("b") });
        var statuses = new ConcurrentQueue<RemoteConfigStatus>();
        using var server = new RawServer(async c =>
        {
            while (true)
            {
                var (_, headers) = await c.ReadHeadAsync();
                await c.AnswerAsync(headers, m =>
                {
                    if (m.RemoteConfigStatus is { } s)
                    {
                        statuses.Enqueue(s);
                    }

                    return new ServerToAgent { InstanceUid = m.InstanceUid, RemoteConfig = config };
                });
            }
        });
        var dir = Directory.CreateTempSubdirectory();
        var options = new OpAmpClientOptions
        {
            Mode = OpAmpMode.Platform,
            Endpoint = new Uri(server.Url("http")),
            Capabilities = (ulong)(AgentCapabilities.AcceptsRemoteConfig | AgentCapabilities.ReportsRemoteConfig),
            HeartbeatInterval = TimeSpan.FromMilliseconds(100),
            StateFile = Path.Combine(dir.FullName, "state"),
        };

        // The first run gets the config, and keeps it.
        var first = OpAmpClient.Start(options, new OpAmpStatus(), (c, n) =>
        {
            if (n is OpAmpNotification.RemoteConfig r)
            {
                c.SetRemoteConfigStatus(new RemoteConfigStatus { LastRemoteConfigHash = r.Config.ConfigHash, Status = RemoteConfigStatuses.Applied });
            }
        });
        Assert.True(await Wait.ForAsync(() => !statuses.IsEmpty));
        await first.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));

        // The second (the same settings: other ones would discard the state) is handed it at
        // start, and answers at once.
        var errors = new ConcurrentQueue<Exception>();
        var answered = 0;
        var seen = new ConcurrentQueue<OpAmpNotification>();
        var second = Slow(OpAmpClient.Start(options, new OpAmpStatus(), (c, n) =>
        {
            seen.Enqueue(n);
            switch (n)
            {
                case OpAmpNotification.RemoteConfig r:
                    c.SetRemoteConfigStatus(new RemoteConfigStatus { LastRemoteConfigHash = r.Config.ConfigHash, Status = RemoteConfigStatuses.Applied });
                    Interlocked.Increment(ref answered);
                    break;
                case OpAmpNotification.Stopped { Error: { } e }:
                    errors.Enqueue(e);
                    break;
            }
        }));
        Assert.True(await Wait.ForAsync(() => Volatile.Read(ref answered) > 0), string.Join(", ", seen));
        await second.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        Assert.Empty(errors);
    }

    // S7: timers past what .NET's timers take (a 60-day heartbeat and request timeout) stop
    // nothing: every request ends with an input, and the timer fires early and is set again.
    [Theory]
    [InlineData(OpAmpMode.Platform)]
    [InlineData(OpAmpMode.Core)]
    public async Task S7_TimersPastThePlatformsLimitStopNothing(OpAmpMode mode)
    {
        using var server = new TestServer();
        var errors = new ConcurrentQueue<Exception>();
        var client = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Endpoint = new Uri(server.Endpoint("http")),
                HeartbeatInterval = TimeSpan.FromDays(60),
                RequestTimeout = TimeSpan.FromDays(60),
                Mode = mode, DirectConnection = mode == OpAmpMode.Core,
            },
            new OpAmpStatus(),
            n =>
            {
                if (n is OpAmpNotification.Stopped { Error: { } e })
                {
                    errors.Enqueue(e);
                }
            });
        Assert.True(await Wait.ForAsync(() => !server.Messages.IsEmpty), "no request was sent");
        await Task.Delay(500); // the answer is in, and the next poll set 60 days off
        Assert.Empty(errors);
        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        Assert.True(await Wait.ForAsync(() => server.Messages.Any(m => m.Message.AgentDisconnect is not null)), "no goodbye");
    }

    /// <summary>A caller slow to take what Start returned.</summary>
    private static T Slow<T>(T value)
    {
        Thread.Sleep(500);
        return value;
    }
}
