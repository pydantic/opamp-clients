// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Diagnostics;
using System.Security.Cryptography;
using System.Text;
using Google.Protobuf;
using Microsoft.Extensions.Time.Testing;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// Rust mode's executor (<see cref="Streams"/>) on its own, over the in-memory network
/// (<see cref="MemNet"/>): its registry (a close, a cancel and stop end what they name at once; a
/// connect, TLS handshake or resolution that completes after its close is closed and untold;
/// nothing is told after a close or after stop; stop is bounded), its backpressure both ways, its
/// splices, and its download files (made new, deleted on a discard, a cancel or a failure, durable
/// before <c>downloaded</c>, journaled).
/// </summary>
public class StreamsTests
{
    private static readonly TimeSpan Quiet = TimeSpan.FromMilliseconds(300);

    // covers: RULE-SHELL-04
    [Fact]
    public async Task AStreamOpensCarriesBytesBothWaysAndEnds()
    {
        using var r = new StreamsRig();
        r.Net.OnDial = d => d.Accept();
        r.Run(Fx.Open(1, "server.test", 8080));
        Assert.Equal(Input.KindOneofCase.StreamOpened, r.Next()!.KindCase);
        var dial = r.Net.Dials.Single();
        Assert.Equal(("server.test", 8080), (dial.Host, dial.Port));
        var server = dial.Server!;

        r.Run(Fx.Write(1, "hello"));
        Assert.Equal("hello", Encoding.ASCII.GetString(await server.ReadExactAsync(5)));
        await server.WriteAsync("world");
        Assert.Equal("world", Encoding.ASCII.GetString(r.Data(1, 5)));

        // The peer's end, then the core's close: a FIN once what was written has gone.
        server.ShutdownSend();
        Assert.Equal(Input.KindOneofCase.StreamEof, r.Next()!.KindCase);
        r.Run(Fx.Write(1, "bye"));
        r.Run(Fx.Close(1, abort: false));
        Assert.Equal("bye", Encoding.ASCII.GetString(await server.ReadToEndAsync(TimeSpan.FromSeconds(5))));
        Assert.True(server.Inbound.Finished, "no FIN");
        Assert.True(StreamsRig.Until(() => r.S.Live == 0), "the stream outlived the ends of both sides");
        Assert.Null(r.Next(Quiet));
    }

    // covers: RULE-SHELL-06
    [Fact]
    public void AConnectionThatCompletesAfterItsCloseIsResetUntold()
    {
        using var r = new StreamsRig();
        r.Net.Deaf = (kind, _) => kind == "connect";
        r.Run(Fx.Open(1));
        Assert.True(StreamsRig.Until(() => r.Net.Dials.Length == 1));
        r.Run(Fx.Close(1, abort: false)); // never ready: reset at once
        var dial = r.Net.Dials[0];
        Assert.True(dial.Ct.IsCancellationRequested, "the connect wasn't cancelled");
        Assert.True(dial.Accept(), "a deaf connect completes after its cancel");
        Assert.True(StreamsRig.Until(() => dial.Server!.WasReset), "the late connection stayed open");
        Assert.Null(r.Next(Quiet));
        Assert.Equal(0, r.S.Live);
    }

    // covers: RULE-SHELL-06
    [Fact]
    public void AHandshakeThatCompletesAfterItsCloseIsResetUntold()
    {
        using var r = new StreamsRig();
        r.Net.OnDial = d => d.Accept();
        r.Net.Deaf = (kind, _) => kind == "tls";
        r.Run(Fx.Open(1, "server.test", 443));
        Assert.Equal(Input.KindOneofCase.StreamOpened, r.Next()!.KindCase);
        r.Run(Fx.Tls(1, "server.test")); // in the step that heard it opened, as the shell asks
        r.Ack();
        Assert.True(StreamsRig.Until(() => r.Net.Handshakes.Length == 1), "no handshake");
        var h = r.Net.Handshakes[0];
        Assert.Equal("server.test", h.Host);
        r.Run(Fx.Close(1, abort: false)); // in a handshake: reset
        Assert.True(h.Ok());
        Assert.True(StreamsRig.Until(() => r.Net.Dials[0].Server!.WasReset));
        Assert.Null(r.Next(Quiet));
        Assert.Equal(0, r.S.Live);
    }

    // covers: RULE-SHELL-04, RULE-SHELL-06
    [Fact]
    public void AResolutionIsToldOnceAndNotAfterItsClose()
    {
        using var r = new StreamsRig();
        r.Net.Deaf = (kind, n) => kind == "resolve" && n == 0;
        r.Run(Fx.Resolve(1, "late.test"));
        Assert.True(StreamsRig.Until(() => r.Net.Lookups.Length == 1));
        r.Run(Fx.Close(1, abort: true));
        Assert.True(r.Net.Lookups[0].Answer("192.0.2.1"));
        Assert.Null(r.Next(Quiet));

        r.Run(Fx.Resolve(2, "name.test"));
        Assert.True(StreamsRig.Until(() => r.Net.Lookups.Length == 2));
        r.Net.Lookups[1].Answer("192.0.2.7", "2001:db8::7");
        var resolved = r.Next()!.StreamResolved;
        Assert.Equal(2u, resolved.Id);
        Assert.Equal(["192.0.2.7", "2001:db8::7"], resolved.Addresses);

        r.Run(Fx.Resolve(3, "nowhere.test"));
        Assert.True(StreamsRig.Until(() => r.Net.Lookups.Length == 3));
        r.Net.Lookups[2].Fail(new System.Net.Sockets.SocketException(11001));
        Assert.Equal(3u, r.Next()!.StreamFailed.Id);
        Assert.Null(r.Next(Quiet));
    }

    // covers: RULE-SHELL-06
    [Fact]
    public async Task WhatWasQueuedBeforeACloseIsNeverTold()
    {
        using var r = new StreamsRig();
        r.Net.OnDial = d => d.Accept();
        r.Run(Fx.Open(1));
        Assert.Equal(Input.KindOneofCase.StreamOpened, r.Next()!.KindCase);
        r.Ack();
        var server = r.Net.Dials[0].Server!;
        await server.WriteAsync("queued");
        Assert.True(StreamsRig.Until(() => r.Queue.Count > 0), "nothing read");
        r.Run(Fx.Close(1, abort: true));
        Assert.Null(r.Next(Quiet));
        Assert.True(StreamsRig.Until(() => server.WasReset), "an aborted stream wasn't reset");
    }

    // covers: RULE-SHELL-04, RULE-SHELL-05
    [Fact]
    public async Task AFailureIsToldOnce()
    {
        using var r = new StreamsRig();
        r.Net.OnDial = d => d.Accept();
        r.Run(Fx.Open(1));
        Assert.Equal(Input.KindOneofCase.StreamOpened, r.Next()!.KindCase);
        r.Net.Dials[0].Server!.Abort();
        Assert.Equal(1u, r.Next()!.StreamFailed.Id);
        r.Run(Fx.Write(1, "after"));
        r.Run(Fx.Close(1, abort: false));
        Assert.Null(r.Next(Quiet));

        // A refused connect, likewise.
        r.Net.OnDial = d => d.Fail(new System.Net.Sockets.SocketException(10061));
        r.Run(Fx.Open(2));
        Assert.Equal(2u, r.Next()!.StreamFailed.Id);
        Assert.Null(r.Next(Quiet));
        await Task.CompletedTask;
    }

    // covers: RULE-SHELL-07
    [Fact]
    public async Task StopResetsEverythingWithinItsBound()
    {
        using var r = new StreamsRig();
        r.Net.Deaf = (kind, n) => (kind == "connect" && n == 2) || kind == "resolve";
        r.Net.OnDial = d =>
        {
            if (d.Index is 0 or 3)
            {
                d.Accept();
            }
        };

        r.Run(Fx.Open(1)); // open
        Assert.Equal(Input.KindOneofCase.StreamOpened, r.Next()!.KindCase);
        r.Ack();
        r.Run(Fx.Open(2)); // connecting
        r.Run(Fx.Open(3)); // connecting, deaf
        r.Run(Fx.Open(4)); // closed, lingering: its peer never ends
        Assert.Equal(Input.KindOneofCase.StreamOpened, r.Next()!.KindCase);
        r.Ack();
        r.Run(Fx.Close(4, abort: false));
        r.Run(Fx.Resolve(5, "late.test")); // resolving, deaf
        Assert.True(StreamsRig.Until(() => r.Net.Dials.Length == 4 && r.Net.Lookups.Length == 1));

        var clock = Stopwatch.StartNew();
        await r.S.ShutdownAsync(TimeSpan.FromSeconds(1)).WaitAsync(TimeSpan.FromSeconds(10));
        Assert.True(clock.Elapsed < TimeSpan.FromSeconds(5), $"stop took {clock.Elapsed}");

        var dials = r.Net.Dials;
        Assert.True(dials[0].Server!.WasReset && dials[3].Server!.WasReset, "an open or lingering stream outlived stop");
        Assert.True(dials[1].Task.IsCanceled, "a connect outlived stop");

        // What the platform hands over late is reset at once, and told to no one.
        Assert.True(dials[2].Accept());
        Assert.True(StreamsRig.Until(() => dials[2].Server!.WasReset), "a connection that came after stop stayed open");
        r.Net.Lookups[0].Answer("192.0.2.1");
        Assert.Null(r.Next(Quiet));
        Assert.Equal(0, r.S.Live);

        // Nothing starts after stop.
        r.Run(Fx.Open(6));
        Assert.Equal(4, r.Net.Dials.Length);
    }

    // covers: RULE-SHELL-07
    [Fact]
    public async Task AClosedStreamLingersTwoSecondsAtMost()
    {
        var time = new FakeTimeProvider();
        using var r = new StreamsRig(time);
        r.Net.OnDial = d => d.Accept();
        r.Run(Fx.Open(1));
        Assert.Equal(Input.KindOneofCase.StreamOpened, r.Next()!.KindCase);
        r.Ack();
        var server = r.Net.Dials[0].Server!;
        r.Run(Fx.Write(1, "last words"));
        r.Run(Fx.Close(1, abort: false));
        Assert.Equal("last words", Encoding.ASCII.GetString(await server.ReadExactAsync(10)));
        Assert.True(StreamsRig.Until(() => server.Inbound.Finished), "no FIN");
        await server.WriteAsync("ignored: the core closed it"); // read into nothing
        time.Advance(TimeSpan.FromMilliseconds(1900));
        await Task.Delay(100);
        Assert.False(server.WasReset, "reset before its linger was over");
        Assert.Equal(1, r.S.Live);
        time.Advance(TimeSpan.FromMilliseconds(200));
        Assert.True(StreamsRig.Until(() => server.WasReset), "the peer never ended, and the stream outlived its linger");
        Assert.Equal(0, r.S.Live);
        Assert.Null(r.Next(Quiet));
    }

    // covers: RULE-SHELL-05
    [Fact]
    public async Task ASlowFileStopsTheStreamThatFeedsIt()
    {
        using var hold = new ManualResetEventSlim();
        using var r = new StreamsRig();
        r.Files.HoldWrites = hold;
        r.Net.OnDial = d => d.Accept();
        r.Run(Fx.Open(1));
        Assert.Equal(Input.KindOneofCase.StreamOpened, r.Next()!.KindCase);
        const int size = 8 << 20;
        r.Run(Fx.Splice(1, 7, size));
        r.Ack();
        var body = RandomNumberGenerator.GetBytes(size);
        var server = r.Net.Dials[0].Server!;
        var writing = server.WriteAsync(body).AsTask();

        // The file's writes are held: the stream stops reading past the high-water mark, and the
        // server's writes wait (its window full).
        Assert.True(StreamsRig.Until(() => r.S.Unwritten > Streams.HighWater), "the file's writes never backed up");
        await Task.Delay(300);
        var waiting = r.S.Unwritten;
        Assert.True(waiting <= Streams.HighWater + (64 << 10), $"{waiting} bytes waiting for the file: the stream read on");
        Assert.False(writing.IsCompleted, "the server wrote everything: nothing held it");

        hold.Set();
        await writing.WaitAsync(TimeSpan.FromSeconds(20));
        Assert.Equal(Input.KindOneofCase.StreamSpliced, r.Next(TimeSpan.FromSeconds(20))!.KindCase);
        r.Run(Fx.DownloadDone(7));
        var done = r.Next(TimeSpan.FromSeconds(20))!;
        Assert.Equal(SHA256.HashData(body), done.Downloaded.Sha256.ToByteArray());
        Assert.Equal(body, await File.ReadAllBytesAsync(r.X.DownloadPath(7)));
    }

    // covers: RULE-SHELL-05
    [Fact]
    public async Task APeerThatDoesntReadStopsTheStreamAndThenFailsIt()
    {
        using var r = new StreamsRig();
        r.Net.OnDial = d => d.Accept();
        r.Run(Fx.Open(1));
        Assert.Equal(Input.KindOneofCase.StreamOpened, r.Next()!.KindCase);
        r.Ack();
        var server = r.Net.Dials[0].Server!;
        var mib = new byte[1 << 20];

        // Over the write high-water mark: the stream begins no read until the peer has taken it
        // (one already under way may still take what comes first).
        r.Run(Fx.Write(1, mib));
        await Task.Delay(100);
        await server.WriteAsync("ping");
        var early = r.Next(Quiet);
        Assert.True(early is null || early.StreamData.Data.ToStringUtf8() == "ping", $"{early}");
        await server.WriteAsync("pong");
        Assert.Null(r.Next(Quiet));
        await server.ReadExactAsync(mib.Length);
        var rest = early is null ? "pingpong" : "pong";
        Assert.Equal(rest, Encoding.ASCII.GetString(r.Data(1, rest.Length)));
        r.Ack();

        // A peer that never reads: past MaxUnsent, the next write fails the stream.
        for (var i = 0; i <= Streams.MaxUnsent / mib.Length; i++)
        {
            r.Run(Fx.Write(1, mib));
        }

        Assert.Null(r.Next(Quiet));
        r.Run(Fx.Write(1, mib));
        var failed = r.Next()!;
        Assert.Contains("has not taken", failed.StreamFailed.Detail, StringComparison.Ordinal);
        Assert.True(StreamsRig.Until(() => server.WasReset));
        Assert.Null(r.Next(Quiet));
    }

    // covers: RULE-SHELL-09
    [Fact]
    public async Task ASpliceGoesToTheFileAndWhatFollowsToTheCore()
    {
        using var r = new StreamsRig();
        r.Net.OnDial = d => d.Accept();
        r.Run(Fx.Open(1));
        Assert.Equal(Input.KindOneofCase.StreamOpened, r.Next()!.KindCase);
        r.Ack();
        var server = r.Net.Dials[0].Server!;
        await server.WriteAsync("HEAD+pre");
        Assert.Equal("HEAD+pre", Encoding.ASCII.GetString(r.Data(1, 8)));

        // In the step that read it: the body's first bytes as download_data, the rest spliced.
        r.Run(Fx.DownloadData(9, "pre"));
        r.Run(Fx.Splice(1, 9, 10));
        r.Ack();
        await server.WriteAsync("0123456789TAIL");
        Assert.Equal(Input.KindOneofCase.StreamSpliced, r.Next()!.KindCase);
        Assert.Equal("TAIL", Encoding.ASCII.GetString(r.Data(1, 4)));
        r.Run(Fx.DownloadDone(9));
        var done = r.Next()!.Downloaded;
        var file = Encoding.ASCII.GetBytes("pre0123456789");
        Assert.Equal(9u, done.Id);
        Assert.Equal(SHA256.HashData(file), done.Sha256.ToByteArray());
        Assert.Equal(file, await File.ReadAllBytesAsync(r.X.DownloadPath(9)));
        var log = r.Files.Log;
        Assert.True(Array.IndexOf(log, $"sync {Path.GetFileName(r.X.DownloadPath(9))}") >= 0, "never synced");
    }

    // A download's blocking file work (making its file, the flush to the disk, deleting it) runs
    // on a thread of its own, as a save is written: on one of the pool's, for as long as the disk
    // took, it kept a thread the loop, transport and timers need.
    // covers: RULE-SHELL-07
    [Fact]
    public void ADownloadsFileWorkHoldsNoThreadOfThePool()
    {
        using var r = new StreamsRig();
        r.Run(Fx.DownloadData(5, "whole"));
        r.Run(Fx.DownloadDone(5));
        Assert.True(StreamsRig.Until(() => r.Queue.Count > 0), "never answered");
        r.Run(new Effect { Cancel = new Id { Id_ = 5 } }); // answered, the answer not yet taken: only its file is left, to delete
        Assert.Null(r.Next(Quiet));
        Assert.True(StreamsRig.Until(() => !File.Exists(r.X.DownloadPath(5))), "the cancelled download's file stayed");
        r.Files.FailWrite = new IOException("the disk is full (injected)");
        r.Run(Fx.DownloadData(6, "lost"));
        r.Run(Fx.DownloadDone(6));
        Assert.Equal(6u, r.Next()!.DownloadFailed.Id); // its file deleted
        Assert.False(File.Exists(r.X.DownloadPath(6)));
        var name = Path.GetFileName(r.X.DownloadPath(5));
        Assert.Contains($"sync {name}", r.Files.Log);
        Assert.Contains($"remove {name} True", r.Files.Log);
        Assert.Contains($"remove {Path.GetFileName(r.X.DownloadPath(6))} True", r.Files.Log);
        Assert.Empty(r.Files.OnPool);
    }

    // covers: RULE-SHELL-09
    [Fact]
    public async Task ADownloadsFileIsMadeNew()
    {
        using var r = new StreamsRig();
        var stale = r.X.DownloadPath(3);
        Directory.CreateDirectory(Path.GetDirectoryName(stale)!);
        await File.WriteAllTextAsync(stale, "a crashed run's");
        r.Run(Fx.DownloadData(3, "new"));
        r.Run(Fx.DownloadDone(3));
        Assert.Equal(3u, r.Next()!.DownloadFailed.Id);
        Assert.Equal("a crashed run's", await File.ReadAllTextAsync(stale));
    }

    // covers: RULE-SHELL-06, RULE-SHELL-09
    [Theory]
    [InlineData("discard")]
    [InlineData("cancel")]
    public void ADiscardedOrCancelledFileGoesOnceItsWriteIsOverUntold(string how)
    {
        using var hold = new ManualResetEventSlim();
        using var r = new StreamsRig();
        r.Files.HoldWrites = hold;
        r.Run(Fx.DownloadData(4, new byte[1024]));
        var path = r.X.DownloadPath(4);
        Assert.True(StreamsRig.Until(() => File.Exists(path)), "never made");
        r.Run(how == "discard" ? new Effect { Discard = new Id { Id_ = 4 } } : new Effect { Cancel = new Id { Id_ = 4 } });
        r.Run(Fx.DownloadData(4, new byte[1024])); // after it: nowhere
        r.Run(Fx.DownloadDone(4));
        hold.Set();
        Assert.True(StreamsRig.Until(() => !File.Exists(path)), "the file stayed");
        Assert.Null(r.Next(Quiet));
    }

    // covers: RULE-SHELL-06
    [Fact]
    public void ACancelAfterTheAnswerDropsItAndTheFile()
    {
        using var r = new StreamsRig();
        r.Run(Fx.DownloadData(5, "whole"));
        r.Run(Fx.DownloadDone(5));
        Assert.True(StreamsRig.Until(() => r.Queue.Count > 0), "never answered");
        r.Run(new Effect { Cancel = new Id { Id_ = 5 } });
        Assert.Null(r.Next(Quiet));
        Assert.True(StreamsRig.Until(() => !File.Exists(r.X.DownloadPath(5))), "the cancelled download's file stayed");
    }

    // covers: RULE-SHELL-09
    [Fact]
    public void AFailedWriteDeletesTheFileAndFailsTheDownload()
    {
        using var r = new StreamsRig();
        r.Files.FailWrite = new IOException("the disk is full (injected)");
        r.Run(Fx.DownloadData(6, "lost"));
        r.Run(Fx.DownloadDone(6));
        var failed = r.Next()!.DownloadFailed;
        Assert.Contains("the disk is full", failed.Detail, StringComparison.Ordinal);
        Assert.False(File.Exists(r.X.DownloadPath(6)));
        Assert.Null(r.Next(Quiet));
    }

    // covers: RULE-SHELL-09
    [Fact]
    public void TheFilesOperationsAreJournaledAndDurableBeforeTheAnswer()
    {
        var dir = Directory.CreateTempSubdirectory("opamp-streams-journal-").FullName;
        var state = Path.Combine(dir, "state");
        var tapPath = Path.Combine(dir, "core.tap");
        using (var tap = CoreTap.Open(tapPath)!)
        {
            using var r = new StreamsRig(files: new FileJournal(FileOps.Real, tap, state), state: state);
            r.Run(Fx.DownloadData(8, "one"));
            r.Run(Fx.DownloadData(8, "two"));
            r.Run(Fx.DownloadDone(8));
            Assert.Equal(8u, r.Next()!.Downloaded.Id);
            r.Run(new Effect { Discard = new Id { Id_ = 8 } });
            Assert.True(StreamsRig.Until(() => !File.Exists(r.X.DownloadPath(8))));
        }

        var ops = TapKit.Files(TapKit.Read(tapPath)).Select(f => f.Field("op")).ToList();
        Assert.Equal("state", ops[0]);
        Assert.Equal("mkdir", ops[1]);
        Assert.Equal("create", ops[2]);
        var sync = ops.IndexOf("sync");
        Assert.True(sync > 2 && ops.Skip(3).Take(sync - 3).All(o => o == "write"), string.Join(", ", ops));
        Assert.Equal("remove", ops[^1]);
        Directory.Delete(dir, recursive: true);
    }

    // The proxy question (which the core asks only when it wasn't told which proxy to use) is
    // answered from the options: the configured proxy, or none; the environment's is the core's.
    [Fact]
    public void TheProxyQuestionIsAnsweredFromTheOptions()
    {
        var endpoint = new Uri("https://server.test/v1/opamp");
        var configured = OpAmpClient.ProxyAnswerFor(new() { Endpoint = endpoint, Proxy = new Uri("socks5://user:pa%40ss@proxy.test:1080"), ProxyUseLocalResolver = true });
        Assert.Equal(("socks5://user:pa%40ss@proxy.test:1080", true), (configured.Proxy, configured.ResolveLocally));
        Assert.Equal(string.Empty, OpAmpClient.ProxyAnswerFor(new() { Endpoint = endpoint, DirectConnection = true }).Proxy);
        Assert.Equal(string.Empty, OpAmpClient.ProxyAnswerFor(new() { Endpoint = endpoint }).Proxy);

        // Through the effect, naming its stream.
        using var r = new StreamsRig(proxy: _ => new ProxyAnswer { Proxy = "http://p:1" });
        r.Run(new Effect { ProxyQuery = new Url { Id = 4, Url_ = "http://server.test/" } });
        var answer = r.Next()!.ProxyAnswer;
        Assert.Equal((4ul, "http://p:1"), (answer.Id, answer.Proxy));
    }

    // A 503 whose Retry-After is an HTTP-date a few seconds ahead holds the next attempt (a
    // heartbeat's, every 100 ms) for about that long: the core reads the date against the
    // client's wall clock (its TimeProvider's, here a day ahead of the system's), which each step
    // carries, not 1970 (the hour's cap).
    [Theory]
    [InlineData("http")]
    [InlineData("ws")]
    public async Task RustModeWaitsUntilARetryAfterDate(string scheme)
    {
        var time = new AheadTime(TimeSpan.FromDays(1));
        var refused = 0L;
        var again = new TaskCompletionSource<long>(TaskCreationOptions.RunContinuationsAsynchronously);
        using var server = new RawServer(async c =>
        {
            while (true)
            {
                var (_, headers) = await c.ReadHeadAsync();
                if (headers.ContainsKey("Content-Length"))
                {
                    await c.ReadBodyAsync(headers);
                }

                var now = Environment.TickCount64;
                if (Interlocked.CompareExchange(ref refused, now, 0) != 0)
                {
                    again.TrySetResult(now);
                }

                var date = time.GetUtcNow().AddSeconds(3).ToString("r", System.Globalization.CultureInfo.InvariantCulture);
                await Http1.WriteAsync(c.Stream, $"HTTP/1.1 503 Service Unavailable\r\nRetry-After: {date}\r\nContent-Length: 0\r\n\r\n");
            }
        });
        var client = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Endpoint = new Uri(server.Url(scheme)), Mode = OpAmpMode.Rust, DirectConnection = true,
                HeartbeatInterval = TimeSpan.FromMilliseconds(100), TimeProvider = time,
            },
            new OpAmpStatus());
        try
        {
            var at = await again.Task.WaitAsync(TimeSpan.FromSeconds(15));
            // The date has whole seconds: 2 to 3 s after the 503.
            var waited = at - Interlocked.Read(ref refused);
            Assert.True(waited >= 1500, $"tried again {waited} ms after a Retry-After 3 s ahead");
        }
        catch (TimeoutException)
        {
            Assert.Fail("not tried again within 15 s of a Retry-After 3 s ahead");
        }
        finally
        {
            await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        }
    }

    /// <summary>The system's time, its wall clock moved by <paramref name="by"/>.</summary>
    private sealed class AheadTime(TimeSpan by) : TimeProvider
    {
        public override DateTimeOffset GetUtcNow() => base.GetUtcNow() + by;
    }

    // What can't apply in Rust mode is an error at start; a SOCKS5 proxy with local resolution can.
    [Fact]
    public void RustModeSettingsThatCantApplyThrowAtStart()
    {
        var endpoint = new Uri("http://127.0.0.1:1/v1/opamp");
        using var app = new HttpClient();
        OpAmpClientOptions[] bad =
        [
            new() { Endpoint = endpoint, Mode = OpAmpMode.Rust, HttpClient = app },
            new() { Endpoint = endpoint, Mode = OpAmpMode.Rust, Proxy = new Uri("socks4://127.0.0.1:1080") },
            new() { Endpoint = endpoint, Mode = OpAmpMode.Rust, Proxy = new Uri("ftp://127.0.0.1:1080") },
            new() { Endpoint = endpoint, Mode = OpAmpMode.Rust, Proxy = new Uri("http://127.0.0.1:3128"), ProxyUseLocalResolver = true },
            new() { Endpoint = endpoint, Mode = OpAmpMode.Rust, Proxy = new Uri("http://127.0.0.1:3128"), DirectConnection = true },
            new() { Endpoint = endpoint, Mode = OpAmpMode.Rust, Tls = new OpAmpTlsOptions { CaPem = "not a certificate" } },
            new() { Endpoint = endpoint, Mode = (OpAmpMode)7 },
        ];
        foreach (var options in bad)
        {
            Assert.Throws<ArgumentException>(() => OpAmpClient.Start(options, new OpAmpStatus()));
        }

        foreach (var proxy in new[] { "socks5://u:p@127.0.0.1:1080", "socks5h://127.0.0.1:1080" })
        {
            var c = OpAmpClient.Start(
                new OpAmpClientOptions { Endpoint = endpoint, Mode = OpAmpMode.Rust, Proxy = new Uri(proxy), ProxyUseLocalResolver = true, StopTimeout = TimeSpan.FromSeconds(1) },
                new OpAmpStatus());
            Assert.True(c.StopAsync().Wait(TimeSpan.FromSeconds(10)));
        }
    }

    // The mode a client runs in: Rust mode with none named, .NET mode when asked for, by Mode or by
    // an HttpClient of the application's (which only .NET mode can use). Rust mode asks its proxy
    // rule (the environment) before it connects; .NET mode never reads it.
    [Fact]
    public async Task WithNoModeNamedTheClientRunsInRustMode()
    {
        var endpoint = new Uri("http://127.0.0.1:1/v1/opamp");
        using var app = new HttpClient(new SocketsHttpHandler { AllowAutoRedirect = false });
        (OpAmpClientOptions Options, bool Rust)[] cases =
        [
            (new() { Endpoint = endpoint }, true),
            (new() { Endpoint = endpoint, Mode = OpAmpMode.Rust }, true),
            (new() { Endpoint = endpoint, RustSplice = false }, true),
            (new() { Endpoint = endpoint, Mode = OpAmpMode.DotNet }, false),
            (new() { Endpoint = endpoint, HttpClient = app }, false),
            (new() { Endpoint = endpoint, Mode = OpAmpMode.DotNet, HttpClient = app }, false),
        ];
        foreach (var (options, rust) in cases)
        {
            var asked = 0;
            var client = OpAmpClient.Start(
                options,
                new OpAmpStatus(),
                null,
                new Seams
                {
                    Environment = _ =>
                    {
                        Interlocked.Increment(ref asked);
                        return null;
                    },
                });
            for (var i = 0; i < (rust ? 1500 : 50) && Volatile.Read(ref asked) == 0; i++)
            {
                await Task.Delay(10);
            }

            await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
            Assert.True(rust == Volatile.Read(ref asked) > 0, $"Mode {options.Mode?.ToString() ?? "unset"}, HttpClient {options.HttpClient is not null}: Rust mode {!rust}, want {rust}");
        }
    }

    // Nothing one mode alone honours is ignored: it is an error that says what to set.
    [Fact]
    public void AnOptionOfTheOtherModeThrowsAtStart()
    {
        var endpoint = new Uri("http://127.0.0.1:1/v1/opamp");
        using var app = new HttpClient();
        (OpAmpClientOptions Options, string Says)[] bad =
        [
            (new() { Endpoint = endpoint, Mode = OpAmpMode.DotNet, RustSplice = false }, "RustSplice is Rust mode's"),
            (new() { Endpoint = endpoint, HttpClient = app, RustSplice = false }, "RustSplice is Rust mode's"),
            (new() { Endpoint = endpoint, Mode = OpAmpMode.Rust, HttpClient = app }, "HttpClient can't be used in Rust mode"),
            (new() { Endpoint = endpoint, Mode = OpAmpMode.DotNet, Proxy = new Uri("socks5://127.0.0.1:1080"), ProxyUseLocalResolver = true }, "Rust mode can"),
            (new() { Endpoint = endpoint, HttpClient = app, ProxyUseLocalResolver = true }, "ProxyUseLocalResolver"),
        ];
        foreach (var (options, says) in bad)
        {
            var e = Assert.Throws<ArgumentException>(() => OpAmpClient.Start(options, new OpAmpStatus()));
            Assert.Contains(says, e.Message, StringComparison.Ordinal);
        }
    }
}

/// <summary>The effects of Rust mode, built.</summary>
internal static class Fx
{
    public static Effect Open(ulong id, string host = "server.test", uint port = 80) => new() { StreamOpen = new StreamOpen { Id = id, Host = host, Port = port } };

    public static Effect Tls(ulong id, string host) => new() { StreamStartTls = new StreamName { Id = id, Host = host } };

    public static Effect Resolve(ulong id, string host) => new() { StreamResolve = new StreamName { Id = id, Host = host } };

    public static Effect Write(ulong id, string text) => Write(id, Encoding.ASCII.GetBytes(text));

    public static Effect Write(ulong id, byte[] data) => new() { StreamWrite = new WsMessage { Id = id, Data = ByteString.CopyFrom(data) } };

    public static Effect Close(ulong id, bool abort) => new() { StreamClose = new StreamClose { Id = id, Abort = abort } };

    public static Effect Splice(ulong id, ulong download, ulong bytes) => new() { StreamSplice = new StreamSplice { Id = id, Download = download, Bytes = bytes } };

    public static Effect DownloadData(ulong id, string text) => DownloadData(id, Encoding.ASCII.GetBytes(text));

    public static Effect DownloadData(ulong id, byte[] data) => new() { DownloadData = new WsMessage { Id = id, Data = ByteString.CopyFrom(data) } };

    public static Effect DownloadDone(ulong id) => new() { DownloadDone = new Id { Id_ = id } };
}

/// <summary>
/// A <see cref="Streams"/> and the platform executor beside it, on the in-memory network, with a
/// loop of the test's own: <see cref="Next"/> takes what reaches the core (dropping what its
/// registry no longer has), and acknowledges it when the test asks for the next one (or
/// <see cref="Ack"/>), as the client's loop does once it has carried out the step's effects.
/// </summary>
internal sealed class StreamsRig : IDisposable
{
    private readonly string? dir;
    private Queued? last;

    public StreamsRig(TimeProvider? time = null, Func<string, ProxyAnswer>? proxy = null, IFileOps? files = null, string? state = null)
    {
        this.dir = state is null ? Directory.CreateTempSubdirectory("opamp-streams-").FullName : null; // the test's own, if it gives one
        this.State = state ?? Path.Combine(this.dir!, "state");
        this.Files = new HeldFiles(files ?? FileOps.Real);
        this.X = new Executor(new MemoryTransport(), this.Queue.Add, this.State, time, this.Files);
        this.S = new Streams(this.X, this.Queue.Add, ClientTransport.SslOptions(new OpAmpTlsOptions()), proxy ?? (_ => new ProxyAnswer()), this.Net, time);
    }

    public BlockingCollection<Queued> Queue { get; } = new();

    public MemNet Net { get; } = new();

    public HeldFiles Files { get; }

    public Executor X { get; }

    public Streams S { get; }

    public string State { get; }

    /// <summary>Wait (real time) until <paramref name="done"/>.</summary>
    public static bool Until(Func<bool> done, TimeSpan? within = null)
    {
        var until = Stopwatch.GetTimestamp() + (long)((within ?? TimeSpan.FromSeconds(10)).TotalSeconds * Stopwatch.Frequency);
        while (!done())
        {
            if (Stopwatch.GetTimestamp() > until)
            {
                return false;
            }

            Thread.Sleep(1);
        }

        return true;
    }

    /// <summary>On the loop: carry out <paramref name="e"/>, as the client does.</summary>
    public void Run(Effect e)
    {
        if (!this.S.Run(e))
        {
            this.X.Run(e);
        }
    }

    /// <summary>The next input that reaches the core within <paramref name="within"/> (10 s), or null.</summary>
    public Input? Next(TimeSpan? within = null)
    {
        this.Ack();
        var until = DateTime.UtcNow + (within ?? TimeSpan.FromSeconds(10));
        while (true)
        {
            var left = until - DateTime.UtcNow;
            if (left < TimeSpan.Zero || !this.Queue.TryTake(out var q, left))
            {
                return null;
            }

            if (!(q.Registry ?? this.X.InFlight).Take(q))
            {
                q.Ack?.TrySetResult();
                continue;
            }

            this.last = q;
            this.S.Stepping(q);
            return q.Input;
        }
    }

    /// <summary>The step with the last input is over: the stream it came from reads on.</summary>
    public void Ack()
    {
        this.S.Stepping(null);
        this.last?.Ack?.TrySetResult();
        this.last = null;
    }

    /// <summary>The next <paramref name="n"/> bytes stream <paramref name="id"/> tells the core (stream_data, in pieces or not).</summary>
    public byte[] Data(ulong id, int n)
    {
        var got = new List<byte>();
        while (got.Count < n)
        {
            var i = this.Next() ?? throw new InvalidOperationException($"{got.Count} of {n} bytes told");
            Assert.Equal(Input.KindOneofCase.StreamData, i.KindCase);
            Assert.Equal(id, i.StreamData.Id);
            got.AddRange(i.StreamData.Data);
        }

        return [.. got];
    }

    public void Dispose()
    {
        this.Ack();
        _ = this.S.ShutdownAsync(TimeSpan.FromSeconds(2));
        _ = this.X.ReleaseAsync(TimeSpan.FromSeconds(2));
        try
        {
            if (this.dir is not null)
            {
                Directory.Delete(this.dir, recursive: true);
            }
        }
        catch (IOException)
        {
        }
    }
}
