// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Diagnostics;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// A client started and stopped again and again in one process leaves nothing behind: an agent
/// that reconfigures its client runs for months, starting a new one each time. Each test churns
/// clients through one way of stopping, in both modes and over both transports, then counts what
/// the process holds against what it held after a warm-up: the native core's sessions, threads,
/// file descriptors, timers, and the GC heap after a full collection. A leak of one of anything
/// per client shows as growth with the number of clients; what a warm-up fills once (the thread
/// pool, connection pools, JIT'd code) does not. The counts are the process's: these tests run
/// alone, not beside other tests.
/// </summary>
[Collection(nameof(ChurnTests))]
public class ChurnTests
{
    // Clients started before counting, then counted.
    private const int Warm = 5;
    private const int Cycles = 30;

    // What a count may grow by over Cycles clients and still be noise: the thread pool adding
    // threads, a connection the server has yet to see end. A leak of one per client is Cycles.
    private const int ThreadSlack = 10;
    private const int FdSlack = 8;
    private const int TimerSlack = 10;

    // The GC heap moves by up to a few tens of KB on its own (pools, caches the runtime fills
    // lazily); even a stopped client kept alive is about 8 KB, and Cycles of them are more.
    private const long HeapSlack = 128 << 10;

    private const string FdDir = "/proc/self/fd";

    private static readonly AgentRemoteConfig Remote = new()
    {
        Config = new AgentConfigMap { ConfigMap = { [string.Empty] = new AgentConfigObject { Body = ByteString.CopyFromUtf8("x: 1") } } },
        ConfigHash = ByteString.CopyFromUtf8("h1"),
    };

    // Stopped before the loop has stepped much: the first request or upgrade is cancelled before
    // it has a socket, or as it gets one.
    [Theory]
    [InlineData(OpAmpMode.DotNet, "http")]
    [InlineData(OpAmpMode.DotNet, "ws")]
    [InlineData(OpAmpMode.Rust, "http")]
    [InlineData(OpAmpMode.Rust, "ws")]
    public async Task ClientsStoppedRightAfterStartLeaveNothingBehind(OpAmpMode mode, string transport)
    {
        using var server = new TestServer();
        var sessions = new Sessions();
        await ChurnAsync(sessions, server, async () =>
        {
            var client = OpAmpClient.Start(Options(mode, server.Endpoint(transport)), new OpAmpStatus(), null, sessions.Seams);
            await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        });
    }

    // The server holds each upgrade longer than the client lives: the dial is given up on with
    // the connection open and the upgrade sent, unanswered.
    [Theory]
    [InlineData(OpAmpMode.DotNet)]
    [InlineData(OpAmpMode.Rust)]
    public async Task ClientsStoppedDuringTheWebSocketHandshakeLeaveNothingBehind(OpAmpMode mode)
    {
        using var server = new TestServer { HoldUpgrade = TimeSpan.FromMilliseconds(300) };
        var sessions = new Sessions();
        await ChurnAsync(sessions, server, async () =>
        {
            var client = OpAmpClient.Start(Options(mode, server.Endpoint("ws")), new OpAmpStatus(), null, sessions.Seams);
            await Task.Delay(30);
            await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        });
    }

    // Stopped once the server has answered and the next heartbeat (or poll) is in: the goodbye
    // goes out, and over WebSocket the closing handshake runs.
    [Theory]
    [InlineData(OpAmpMode.DotNet, "http")]
    [InlineData(OpAmpMode.DotNet, "ws")]
    [InlineData(OpAmpMode.Rust, "http")]
    [InlineData(OpAmpMode.Rust, "ws")]
    public async Task ClientsStoppedWhileConnectedLeaveNothingBehind(OpAmpMode mode, string transport)
    {
        using var server = new TestServer();
        var sessions = new Sessions();
        await ChurnAsync(sessions, server, async () =>
        {
            var client = OpAmpClient.Start(Options(mode, server.Endpoint(transport)), new OpAmpStatus(), null, sessions.Seams);
            Assert.True(await Wait.ForAsync(() => server.Messages.Count >= 2), "not connected");
            await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        });
    }

    // The callback answers the remote config and stops the client itself: the stop is queued
    // while the step that told the callback is still under way.
    [Theory]
    [InlineData(OpAmpMode.DotNet, "http")]
    [InlineData(OpAmpMode.DotNet, "ws")]
    [InlineData(OpAmpMode.Rust, "http")]
    [InlineData(OpAmpMode.Rust, "ws")]
    public async Task ClientsStoppedFromTheRemoteConfigCallbackLeaveNothingBehind(OpAmpMode mode, string transport)
    {
        using var server = new TestServer { RemoteConfig = Remote };
        var sessions = new Sessions();
        await ChurnAsync(sessions, server, async () =>
        {
            var stopping = new TaskCompletionSource<Task>(TaskCreationOptions.RunContinuationsAsynchronously);
            OpAmpClient? client = null;
            client = OpAmpClient.Start(
                Options(mode, server.Endpoint(transport)) with { Capabilities = (ulong)AgentCapabilities.AcceptsRemoteConfig },
                new OpAmpStatus(),
                n =>
                {
                    if (n is OpAmpNotification.RemoteConfig rc)
                    {
                        // (Set: nothing is notified before Start has returned.)
                        client!.SetRemoteConfigStatus(new RemoteConfigStatus { LastRemoteConfigHash = rc.Config.ConfigHash, Status = RemoteConfigStatuses.Applied });
                        stopping.TrySetResult(client.StopAsync());
                    }
                },
                sessions.Seams);
            var stop = await stopping.Task.WaitAsync(TimeSpan.FromSeconds(10));
            await stop.WaitAsync(TimeSpan.FromSeconds(10));
        });
    }

    // One state file for every client, as an agent restarting its client has: each saves (its
    // first answer, a remote config) and the next starts from what it saved.
    [Theory]
    [InlineData(OpAmpMode.DotNet, "http")]
    [InlineData(OpAmpMode.DotNet, "ws")]
    [InlineData(OpAmpMode.Rust, "http")]
    [InlineData(OpAmpMode.Rust, "ws")]
    public async Task ClientsOnOneStateFileLeaveNothingBehind(OpAmpMode mode, string transport)
    {
        using var server = new TestServer { RemoteConfig = Remote };
        var dir = Directory.CreateTempSubdirectory();
        var state = Path.Combine(dir.FullName, "state");
        var sessions = new Sessions();
        try
        {
            await ChurnAsync(sessions, server, async () =>
            {
                var told = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
                var client = OpAmpClient.Start(
                    Options(mode, server.Endpoint(transport)) with { StateFile = state, Capabilities = (ulong)AgentCapabilities.AcceptsRemoteConfig },
                    new OpAmpStatus(),
                    n =>
                    {
                        if (n is OpAmpNotification.RemoteConfig)
                        {
                            told.TrySetResult();
                        }
                    },
                    sessions.Seams);
                await told.Task.WaitAsync(TimeSpan.FromSeconds(10));
                await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
            });
        }
        finally
        {
            dir.Delete(recursive: true);
        }
    }

    // The callback throws on the remote config: the client stops on its own, mid-connection,
    // with no stop called, and StopAsync throws the callback's exception.
    [Theory]
    [InlineData(OpAmpMode.DotNet, "http")]
    [InlineData(OpAmpMode.DotNet, "ws")]
    [InlineData(OpAmpMode.Rust, "http")]
    [InlineData(OpAmpMode.Rust, "ws")]
    public async Task ClientsWhoseCallbackThrowsStopThemselvesAndLeaveNothingBehind(OpAmpMode mode, string transport)
    {
        using var server = new TestServer { RemoteConfig = Remote };
        var sessions = new Sessions();
        await ChurnAsync(sessions, server, async () =>
        {
            var stopped = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            var client = OpAmpClient.Start(
                Options(mode, server.Endpoint(transport)) with { Capabilities = (ulong)AgentCapabilities.AcceptsRemoteConfig },
                new OpAmpStatus(),
                n =>
                {
                    switch (n)
                    {
                        case OpAmpNotification.Stopped:
                            stopped.TrySetResult();
                            break;
                        case OpAmpNotification.RemoteConfig:
                            throw new Bug();
                    }
                },
                sessions.Seams);
            await stopped.Task.WaitAsync(TimeSpan.FromSeconds(10));
            await Assert.ThrowsAsync<Bug>(() => client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10)));
        });
    }

    private static OpAmpClientOptions Options(OpAmpMode mode, string endpoint) => new()
    {
        Endpoint = new Uri(endpoint),
        InstanceUid = new byte[16],
        HeartbeatInterval = TimeSpan.FromMilliseconds(50),

        // Longer than the counts are waited for: anything armed for the stop timeout and left
        // armed after the stop is still there to count.
        StopTimeout = TimeSpan.FromSeconds(30),
        Mode = mode,
        DirectConnection = mode == OpAmpMode.Rust,
    };

    /// <summary>
    /// Run <paramref name="cycle"/> Warm times, count, Cycles times more, and count again: nothing
    /// has grown. What winds down after a stop (the server closing its end, a pool's idle thread)
    /// is given a few seconds to; a leak never comes back down.
    /// </summary>
    private static async Task ChurnAsync(Sessions sessions, TestServer server, Func<Task> cycle)
    {
        async Task Once()
        {
            await cycle();
            server.Messages.Clear(); // the server's record of them, not the client's
        }

        for (var i = 0; i < Warm; i++)
        {
            await Once();
        }

        await Task.Delay(200);
        var before = Counts.Take(sessions);
        for (var i = 0; i < Cycles; i++)
        {
            await Once();
        }

        // Each client freed its core's session by the time its stop ended: no finalizer is
        // waited for (a SafeHandle's would free it, whenever the GC got to it).
        Assert.Equal(0, sessions.Live);

        var after = before;
        await Wait.ForAsync(() => (after = Counts.Take(sessions)).Within(before), 5);
        Assert.True(after.Native <= before.Native, $"native sessions: {before.Native} after the warm-up, {after.Native} after {Cycles} more clients");
        Assert.True(after.Threads <= before.Threads + ThreadSlack, $"threads: {before.Threads} after the warm-up, {after.Threads} after {Cycles} more clients");
        Assert.True(after.Fds <= before.Fds + FdSlack, $"file descriptors: {before.Fds} after the warm-up, {after.Fds} after {Cycles} more clients");
        Assert.True(after.Timers <= before.Timers + TimerSlack, $"timers: {before.Timers} after the warm-up, {after.Timers} after {Cycles} more clients");
        Assert.True(after.Heap <= before.Heap + HeapSlack, $"GC heap: {before.Heap} bytes after the warm-up, {after.Heap} after {Cycles} more clients");
    }

    /// <summary>What the process holds, after a full, compacting collection and the finalizers it queued.</summary>
    private sealed record Counts(int Native, int Threads, int Fds, long Timers, long Heap)
    {
        public static Counts Take(Sessions sessions)
        {
            for (var i = 0; i < 2; i++)
            {
                GC.Collect(GC.MaxGeneration, GCCollectionMode.Forced, blocking: true, compacting: true);
                GC.WaitForPendingFinalizers();
            }

            using var me = Process.GetCurrentProcess();
            return new Counts(
                sessions.Live,
                me.Threads.Count,
                Directory.Exists(FdDir) ? Directory.GetFileSystemEntries(FdDir).Length : me.HandleCount,
                Timer.ActiveCount,
                GC.GetTotalMemory(forceFullCollection: true));
        }

        public bool Within(Counts before) =>
            this.Native <= before.Native && this.Threads <= before.Threads + ThreadSlack && this.Fds <= before.Fds + FdSlack
            && this.Timers <= before.Timers + TimerSlack && this.Heap <= before.Heap + HeapSlack;
    }

    /// <summary>
    /// The native core's sessions alive, of the clients started with <see cref="Seams"/>: each
    /// client's core is the real one, counted as it is made and as the client disposes it. The
    /// library keeps no count of its own.
    /// </summary>
    private sealed class Sessions
    {
        private int live;

        public Sessions() => this.Seams = new Seams { Core = this.New };

        public Seams Seams { get; }

        public int Live => Volatile.Read(ref this.live);

        private ICoreEngine New()
        {
            var core = new Counted(new NativeCore(), this);
            Interlocked.Increment(ref this.live);
            return core;
        }

        private sealed class Counted(NativeCore inner, Sessions sessions) : ICoreEngine
        {
            private int disposed;

            public byte[] Init(byte[] config) => inner.Init(config);

            public byte[] Step(byte[] step) => inner.Step(step);

            public void Dispose()
            {
                if (Interlocked.Exchange(ref this.disposed, 1) == 0)
                {
                    inner.Dispose();
                    Interlocked.Decrement(ref sessions.live);
                }
            }
        }
    }

    private sealed class Bug : Exception;
}

/// <summary>The churn tests count the process's threads, descriptors and heap: they run on their own.</summary>
[CollectionDefinition(nameof(ChurnTests), DisableParallelization = true)]
public sealed class ChurnTestsAlone;
