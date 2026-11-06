// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Diagnostics;
using System.Security.Cryptography;
using System.Text;
using Google.Protobuf;
using Microsoft.Extensions.Time.Testing;
using Logfire.Fleet.Core;
using Xunit;

namespace Logfire.Fleet.Client.Tests;

/// <summary>
/// Core mode's executor (<see cref="Streams"/>) on its own, over the in-memory network
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

    // The client certificate goes only to a handshake the core says is with the server's own
    // origin (stream_start_tls's server_origin); any other presents none, the CAs the same.
    // covers: RULE-TLS-02
    [Fact]
    public void TheClientCertificateOnlyOnAStreamToTheServersOrigin()
    {
        var dir = ShellTablesTests.Dir();
        var tls = new OpAmpTlsOptions
        {
            CaPem = File.ReadAllText(Path.Combine(dir, "tls", "ca.pem")),
            ClientCertPem = File.ReadAllText(Path.Combine(dir, "tls", "client-ec.pem")),
            ClientKeyPem = File.ReadAllText(Path.Combine(dir, "tls", "client-ec.key")),
        };
        using var r = new StreamsRig(tls: tls);
        r.Net.OnDial = d => d.Accept();
        r.Net.OnHandshake = h => h.Ok();
        foreach (var (id, origin) in new[] { (1UL, true), (2UL, false) })
        {
            r.Run(Fx.Open(id, "server.test", 443));
            Assert.Equal(Input.KindOneofCase.StreamOpened, r.Next()!.KindCase);
            r.Run(Fx.Tls(id, "server.test", origin));
            Assert.Equal(Input.KindOneofCase.StreamTlsReady, r.Next()!.KindCase);
            var h = r.Net.Handshakes[(int)id - 1];
            Assert.Equal("server.test", h.Host);
            Assert.Equal(origin, h.PresentsCertificate);
        }
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

    // The proxy question (which the core asks only when it wasn't told which proxy to use) is
    // answered from the options: the configured proxy, or none; the environment's is the core's.
    [Fact]
    public void TheProxyQuestionIsAnsweredFromTheOptions()
    {
        var endpoint = new Uri("https://server.test/v1/opamp");
        var configured = OpAmpClient.ProxyAnswerFor(new() { Endpoint = endpoint, Proxy = new Uri("socks5://user:pa%40ss@proxy.test:1080"), ProxyResolveLocally = true });
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
    public async Task CoreModeWaitsUntilARetryAfterDate(string scheme)
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
                Endpoint = new Uri(server.Url(scheme)), Mode = OpAmpMode.Core, DirectConnection = true,
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

    // What can't apply in core mode is an error at start; a SOCKS5 proxy with local resolution can.
    [Fact]
    public void CoreModeSettingsThatCantApplyThrowAtStart()
    {
        var endpoint = new Uri("http://127.0.0.1:1/v1/opamp");
        using var app = new HttpClient();
        OpAmpClientOptions[] bad =
        [
            new() { Endpoint = endpoint, Mode = OpAmpMode.Core, HttpClient = app },
            new() { Endpoint = endpoint, Mode = OpAmpMode.Core, Proxy = new Uri("socks4://127.0.0.1:1080") },
            new() { Endpoint = endpoint, Mode = OpAmpMode.Core, Proxy = new Uri("ftp://127.0.0.1:1080") },
            new() { Endpoint = endpoint, Mode = OpAmpMode.Core, Proxy = new Uri("http://127.0.0.1:3128"), ProxyResolveLocally = true },
            new() { Endpoint = endpoint, Mode = OpAmpMode.Core, Proxy = new Uri("http://127.0.0.1:3128"), DirectConnection = true },
            new() { Endpoint = endpoint, Mode = OpAmpMode.Core, Tls = new OpAmpTlsOptions { CaPem = "not a certificate" } },
            new() { Endpoint = endpoint, Mode = (OpAmpMode)7 },
        ];
        foreach (var options in bad)
        {
            Assert.Throws<ArgumentException>(() => OpAmpClient.Start(options, new OpAmpStatus()));
        }

        foreach (var proxy in new[] { "socks5://u:p@127.0.0.1:1080", "socks5h://127.0.0.1:1080" })
        {
            var c = OpAmpClient.Start(
                new OpAmpClientOptions { Endpoint = endpoint, Mode = OpAmpMode.Core, Proxy = new Uri(proxy), ProxyResolveLocally = true, StopTimeout = TimeSpan.FromSeconds(1) },
                new OpAmpStatus());
            Assert.True(c.StopAsync().Wait(TimeSpan.FromSeconds(10)));
        }
    }

    // The mode a client runs in: core mode with none named, platform mode only when named. Core mode asks
    // its proxy rule (the environment) before it connects; platform mode never reads it.
    [Fact]
    public async Task WithNoModeNamedTheClientRunsInCoreMode()
    {
        var endpoint = new Uri("http://127.0.0.1:1/v1/opamp");
        using var app = new HttpClient(new SocketsHttpHandler { AllowAutoRedirect = false });
        (OpAmpClientOptions Options, bool CoreMode)[] cases =
        [
            (new() { Endpoint = endpoint }, true),
            (new() { Endpoint = endpoint, Mode = OpAmpMode.Core }, true),
            (new() { Endpoint = endpoint, Mode = OpAmpMode.Platform }, false),
            (new() { Endpoint = endpoint, Mode = OpAmpMode.Platform, HttpClient = app }, false),
        ];
        foreach (var (options, coreMode) in cases)
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
            for (var i = 0; i < (coreMode ? 1500 : 50) && Volatile.Read(ref asked) == 0; i++)
            {
                await Task.Delay(10);
            }

            await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
            Assert.True(coreMode == Volatile.Read(ref asked) > 0, $"Mode {options.Mode?.ToString() ?? "unset"}, HttpClient {options.HttpClient is not null}: core mode {!coreMode}, want {coreMode}");
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
            // Regression: an HttpClient alone switched to platform mode silently; the mode must be named.
            (new() { Endpoint = endpoint, HttpClient = app }, "HttpClient needs platform mode"),
            (new() { Endpoint = endpoint, Mode = OpAmpMode.Core, HttpClient = app }, "HttpClient can't be used in core mode"),
            (new() { Endpoint = endpoint, Mode = OpAmpMode.Platform, Proxy = new Uri("socks5://127.0.0.1:1080"), ProxyResolveLocally = true }, "core mode can"),
            (new() { Endpoint = endpoint, Mode = OpAmpMode.Platform, HttpClient = app, ProxyResolveLocally = true }, "ProxyResolveLocally"),
        ];
        foreach (var (options, says) in bad)
        {
            var e = Assert.Throws<ArgumentException>(() => OpAmpClient.Start(options, new OpAmpStatus()));
            Assert.Contains(says, e.Message, StringComparison.Ordinal);
        }
    }
}

/// <summary>The effects of core mode, built.</summary>
internal static class Fx
{
    public static Effect Open(ulong id, string host = "server.test", uint port = 80) => new() { StreamOpen = new StreamOpen { Id = id, Host = host, Port = port } };

    public static Effect Tls(ulong id, string host, bool serverOrigin = true) =>
        new() { StreamStartTls = new StreamName { Id = id, Host = host, ServerOrigin = serverOrigin } };

    public static Effect Resolve(ulong id, string host) => new() { StreamResolve = new StreamName { Id = id, Host = host } };

    public static Effect Write(ulong id, string text) => Write(id, Encoding.ASCII.GetBytes(text));

    public static Effect Write(ulong id, byte[] data) => new() { StreamWrite = new WsMessage { Id = id, Data = ByteString.CopyFrom(data) } };

    public static Effect Close(ulong id, bool abort) => new() { StreamClose = new StreamClose { Id = id, Abort = abort } };
}

/// <summary>
/// A <see cref="Streams"/> and the platform executor beside it, on the in-memory network, with a
/// loop of the test's own: <see cref="Next"/> takes what reaches the core (dropping what its
/// registry no longer has), and acknowledges it when the test asks for the next one (or
/// <see cref="Ack"/>), as the client's loop does once it has carried out the step's effects.
/// </summary>
internal sealed class StreamsRig : IDisposable
{
    private Queued? last;

    public StreamsRig(TimeProvider? time = null, Func<string, ProxyAnswer>? proxy = null, OpAmpTlsOptions? tls = null)
    {
        this.X = new Executor(new MemoryTransport(), this.Queue.Add, time: time);
        this.S = new Streams(this.Queue.Add, ClientTransport.SslOptions(tls ?? new OpAmpTlsOptions()), proxy ?? (_ => new ProxyAnswer()), this.Net, time);
    }

    public BlockingCollection<Queued> Queue { get; } = new();

    public MemNet Net { get; } = new();

    public Executor X { get; }

    public Streams S { get; }

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
            return q.Input;
        }
    }

    /// <summary>The step with the last input is over: the stream it came from reads on.</summary>
    public void Ack()
    {
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
    }
}
