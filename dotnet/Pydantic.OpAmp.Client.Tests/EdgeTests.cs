// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Collections.Concurrent;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// Edges of the client's own layer, around the core: headers the application gives that can't be
/// sent as given, times and sizes that can't be negative, a stop waited for synchronously where a
/// synchronization context is single-threaded, and a state file whose directory isn't there. Each
/// is held the same in both modes.
/// </summary>
public class EdgeTests
{
    // Headers the core refuses in both modes, and why: it checks every header list it sends by the
    // rule an offered header meets (issue #218). .NET mode sent a value's CR LF as it was (the
    // server read a second header, "Injected"), sent a Host of the caller's in place of its own and
    // a Connection beside the upgrade's, and dropped a name that isn't a token without a word.
    public static TheoryData<OpAmpMode, string, string, string, string> Refused()
    {
        const string Value = "the value of header 1 (X) holds a control or non-ASCII character, or a space at either end";
        var cases = new (string Name, string Value, string Why)[]
        {
            ("A:B", "v", "the name of header 1 is not a token"),
            ("Bad Name", "v", "the name of header 1 is not a token"),
            ("Host", "evil.example", "header 1 (Host) is for the HTTP stack to set"),
            ("Content-Length", "0", "header 1 (Content-Length) is for the HTTP stack to set"),
            ("transfer-encoding", "chunked", "header 1 (transfer-encoding) is for the HTTP stack to set"),
            ("Connection", "keep-alive", "header 1 (Connection) is for the HTTP stack to set"),
            ("Upgrade", "h2c", "header 1 (Upgrade) is for the HTTP stack to set"),
            ("Expect", "100-continue", "header 1 (Expect) is for the HTTP stack to set"),
            ("Sec-WebSocket-Key", "k", "header 1 (Sec-WebSocket-Key) is for the HTTP stack to set"),
            ("X", " padded", Value),
            ("X", "padded\t", Value),
            ("X", "a\r\nInjected: 1", Value),
            ("X", "a\0b", Value),
            ("X", "a\u007fb", Value),
            ("X", "caf\u00e9", Value),
        };
        var data = new TheoryData<OpAmpMode, string, string, string, string>();
        foreach (var mode in new[] { OpAmpMode.DotNet, OpAmpMode.Rust })
        {
            foreach (var transport in new[] { "http", "ws" })
            {
                foreach (var (name, value, why) in cases)
                {
                    data.Add(mode, transport, name, value, why);
                }
            }
        }

        return data;
    }

    [Theory]
    [MemberData(nameof(Refused))]
    public async Task AHeaderThatCantBeSentAsGivenFailsTheRequestAndNothingIsSent(
        OpAmpMode mode, string transport, string name, string value, string why)
    {
        using var server = new RawServer(async c => await c.ReadHeadAsync());
        var got = new ConcurrentQueue<OpAmpNotification>();
        var client = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Endpoint = new Uri(server.Url(transport)), Mode = mode, DirectConnection = true,
                Headers = new Dictionary<string, string> { [name] = value },
            },
            new OpAmpStatus(),
            got.Enqueue);
        Assert.True(await Wait.ForAsync(() => got.Any(n => n is OpAmpNotification.ConnectFailed)));
        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        var failed = got.OfType<OpAmpNotification.ConnectFailed>().First();
        Assert.Equal($"not sent: {why}", failed.Detail);
        Assert.Null(failed.Status);
        Assert.Empty(server.Conns); // not even a connection
    }

    [Theory]
    [InlineData(OpAmpMode.DotNet, "http")]
    [InlineData(OpAmpMode.DotNet, "ws")]
    [InlineData(OpAmpMode.Rust, "http")]
    [InlineData(OpAmpMode.Rust, "ws")]
    public async Task HeadersThatPassTheCheckReachTheServer(OpAmpMode mode, string transport)
    {
        var heads = new ConcurrentQueue<Dictionary<string, string>>();
        using var server = new RawServer(async c => heads.Enqueue((await c.ReadHeadAsync()).Headers));
        var client = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Endpoint = new Uri(server.Url(transport)), Mode = mode, DirectConnection = true,
                Headers = new Dictionary<string, string> { ["Authorization"] = "Bearer abc", ["X-Tenant"] = "t1" },
            },
            new OpAmpStatus());
        Assert.True(await Wait.ForAsync(() => !heads.IsEmpty));
        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        heads.TryPeek(out var head);
        Assert.Equal("Bearer abc", head!["Authorization"]);
        Assert.Equal("t1", head["X-Tenant"]);
    }

    // Each was cast to the unsigned value the core takes: -1 ms (Timeout.InfiniteTimeSpan) became
    // 2^64 - 1, a negative heartbeat interval none for half a billion years, a negative
    // MaxReceiveBytes no limit at all. The core refuses them now, naming the field and the number
    // meant, as in every SDK.
    public static TheoryData<OpAmpMode, string> Negatives()
    {
        var data = new TheoryData<OpAmpMode, string>();
        foreach (var mode in new[] { OpAmpMode.DotNet, OpAmpMode.Rust })
        {
            foreach (var name in new[] { "HeartbeatInterval", "RequestTimeout", "ConnectTimeout", "StopTimeout", "VerifyTimeout", "DownloadTimeout", "MaxSendBytes", "MaxReceiveBytes" })
            {
                data.Add(mode, name);
            }
        }

        return data;
    }

    [Theory]
    [MemberData(nameof(Negatives))]
    public async Task ANegativeTimeOrSizeIsAnErrorAtStart(OpAmpMode mode, string name)
    {
        using var server = new TestServer();
        var o = new OpAmpClientOptions { Endpoint = new Uri(server.Endpoint("http")), Mode = mode, DirectConnection = true };
        o = name switch
        {
            "HeartbeatInterval" => o with { HeartbeatInterval = TimeSpan.FromSeconds(-1) },
            "RequestTimeout" => o with { RequestTimeout = Timeout.InfiniteTimeSpan },
            "ConnectTimeout" => o with { ConnectTimeout = TimeSpan.FromMilliseconds(-1) },
            "StopTimeout" => o with { StopTimeout = Timeout.InfiniteTimeSpan },
            "VerifyTimeout" => o with { VerifyTimeout = TimeSpan.FromSeconds(-5) },
            "DownloadTimeout" => o with { DownloadTimeout = TimeSpan.MinValue },
            "MaxSendBytes" => o with { MaxSendBytes = -1 },
            _ => o with { MaxReceiveBytes = long.MinValue },
        };
        var e = Assert.Throws<CoreException>(() => OpAmpClient.Start(o, new OpAmpStatus()));
        var (field, meant) = name switch
        {
            "HeartbeatInterval" => ("heartbeat_interval_ms", "-1000"),
            "RequestTimeout" => ("request_timeout_ms", "-1"),
            "ConnectTimeout" => ("connect_timeout_ms", "-1"),
            "StopTimeout" => ("stop_timeout_ms", "-1"),
            "VerifyTimeout" => ("verify_timeout_ms", "-5000"),
            "DownloadTimeout" => ("download_timeout_ms", "-922337203685477"),
            "MaxSendBytes" => ("max_send_bytes", "-1"),
            _ => ("max_receive_bytes", "-9223372036854775808"),
        };
        Assert.Contains($"{field} is {meant}: negative", e.Message);
        await Task.Delay(100);
        Assert.Equal(0, server.Connections); // nothing started
    }

    [Theory]
    [InlineData(OpAmpMode.DotNet)]
    [InlineData(OpAmpMode.Rust)]
    public async Task ZeroAndTheLargestTimesAndSizesAreTaken(OpAmpMode mode)
    {
        using var server = new TestServer();
        var client = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Endpoint = new Uri(server.Endpoint("http")), Mode = mode, DirectConnection = true,
                RequestTimeout = TimeSpan.MaxValue, ConnectTimeout = TimeSpan.MaxValue, VerifyTimeout = TimeSpan.MaxValue,
                DownloadTimeout = TimeSpan.MaxValue, MaxSendBytes = long.MaxValue, MaxReceiveBytes = long.MaxValue,
                StopTimeout = TimeSpan.Zero, HeartbeatInterval = TimeSpan.Zero,
            },
            new OpAmpStatus());
        Assert.True(await Wait.ForAsync(() => !server.Messages.IsEmpty));
        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
    }

    // An application on a single-threaded synchronization context (a UI thread, classic ASP.NET)
    // that waits for stop synchronously: nothing of the client's needs that thread back.
    [Theory]
    [InlineData(OpAmpMode.DotNet, "http")]
    [InlineData(OpAmpMode.DotNet, "ws")]
    [InlineData(OpAmpMode.Rust, "http")]
    [InlineData(OpAmpMode.Rust, "ws")]
    public async Task StopWaitedForSynchronouslyOnASingleThreadedContextDoesNotDeadlock(OpAmpMode mode, string transport)
    {
        using var server = new TestServer();
        var outcome = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        var thread = new Thread(() =>
        {
            var context = new OneThreadContext();
            SynchronizationContext.SetSynchronizationContext(context);
            context.Post(
                _ =>
                {
                    var client = OpAmpClient.Start(
                        new OpAmpClientOptions { Endpoint = new Uri(server.Endpoint(transport)), Mode = mode, DirectConnection = true, HeartbeatInterval = TimeSpan.FromMilliseconds(50) },
                        new OpAmpStatus());
                    var deadline = DateTime.UtcNow.AddSeconds(10);
                    while (server.Messages.Count < 2 && DateTime.UtcNow < deadline)
                    {
                        Thread.Sleep(10); // the context's thread blocked, as the application's would be
                    }

                    outcome.TrySetResult(client.StopAsync().Wait(TimeSpan.FromSeconds(10)));
                    context.Complete();
                },
                null);
            context.Run();
        }) { IsBackground = true };
        thread.Start();
        Assert.True(await outcome.Task.WaitAsync(TimeSpan.FromSeconds(30)), "stop deadlocked on the context's thread");
        Assert.True(server.Messages.Count >= 2);
    }

    // As Python's client: a file not there yet in a directory that is, is a first start; no
    // directory for it is a setting that can't work.
    [Theory]
    [InlineData(OpAmpMode.DotNet)]
    [InlineData(OpAmpMode.Rust)]
    public async Task AStateFileInADirectoryThatIsntThereIsAnErrorAtStart(OpAmpMode mode)
    {
        var dir = Directory.CreateTempSubdirectory().FullName;
        try
        {
            using var server = new TestServer();
            var o = new OpAmpClientOptions { Endpoint = new Uri(server.Endpoint("http")), Mode = mode, DirectConnection = true };
            Assert.Throws<DirectoryNotFoundException>(() => OpAmpClient.Start(o with { StateFile = Path.Combine(dir, "missing", "state") }, new OpAmpStatus()));
            var client = OpAmpClient.Start(o with { StateFile = Path.Combine(dir, "state") }, new OpAmpStatus());
            Assert.True(await Wait.ForAsync(() => File.Exists(Path.Combine(dir, "state"))));
            await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        }
        finally
        {
            Directory.Delete(dir, recursive: true);
        }
    }

    /// <summary>A synchronization context that runs what is posted to it on one thread, in order.</summary>
    private sealed class OneThreadContext : SynchronizationContext
    {
        private readonly BlockingCollection<(SendOrPostCallback, object?)> queue = new();

        public override void Post(SendOrPostCallback d, object? state) => this.queue.Add((d, state));

        public override void Send(SendOrPostCallback d, object? state) => throw new NotSupportedException();

        public void Complete() => this.queue.CompleteAdding();

        public void Run()
        {
            foreach (var (d, state) in this.queue.GetConsumingEnumerable())
            {
                d(state);
            }
        }
    }
}
