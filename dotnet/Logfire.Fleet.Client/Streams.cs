// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Net;
using System.Net.Security;
using System.Net.Sockets;
using System.Security.Cryptography;
using Google.Protobuf;
using Logfire.Fleet.Core;

namespace Logfire.Fleet.Client;

/// <summary>
/// What core mode does its network work with: TCP connections (TCP_NODELAY), TLS on them
/// (SslStream), name resolution, and a stream's ends (FIN, reset). The real one is
/// <see cref="Real"/>; a test gives one over in-memory streams, whose connects, handshakes and
/// resolutions it decides.
/// </summary>
internal class StreamsNet
{
    /// <summary>.NET's sockets, SslStream and DNS.</summary>
    public static readonly StreamsNet Real = new();

    /// <summary>A TCP connection to <paramref name="host"/> (a name, or an address without brackets) and <paramref name="port"/>.</summary>
    public virtual async Task<Stream> ConnectAsync(string host, int port, CancellationToken ct)
    {
        var ip = IPAddress.TryParse(host, out var a) ? a : null; // an address is never resolved
        var socket = ip is not null
            ? new Socket(ip.AddressFamily, SocketType.Stream, ProtocolType.Tcp)
            : Socket.OSSupportsIPv6 ? new Socket(SocketType.Stream, ProtocolType.Tcp) : new Socket(AddressFamily.InterNetwork, SocketType.Stream, ProtocolType.Tcp);
        try
        {
            socket.NoDelay = true;
            await socket.ConnectAsync(ip is not null ? new IPEndPoint(ip, port) : new DnsEndPoint(host, port), ct).ConfigureAwait(false);
            return new NetworkStream(socket, ownsSocket: true);
        }
        catch
        {
            socket.Dispose();
            throw;
        }
    }

    /// <summary>TLS on <paramref name="inner"/> (which may be TLS already: to a proxy), as <paramref name="options"/> say; <paramref name="inner"/> is left open.</summary>
    public virtual async Task<Stream> AuthenticateAsync(Stream inner, SslClientAuthenticationOptions options, CancellationToken ct)
    {
        var ssl = new SslStream(inner, leaveInnerStreamOpen: true);
        try
        {
            await ssl.AuthenticateAsClientAsync(options, ct).ConfigureAwait(false);
            return ssl;
        }
        catch
        {
            await ssl.DisposeAsync().ConfigureAwait(false);
            throw;
        }
    }

    /// <summary><paramref name="host"/>'s addresses, the preferred first, without zones.</summary>
    public virtual async Task<IReadOnlyList<string>> ResolveAsync(string host, CancellationToken ct)
    {
        var addresses = await Dns.GetHostAddressesAsync(host, ct).ConfigureAwait(false);
        return addresses.Select(x => x.ToString().Split('%')[0]).Distinct().ToList();
    }

    /// <summary>The TCP stream's FIN: nothing more will be written to <paramref name="raw"/>.</summary>
    public virtual void ShutdownSend(Stream raw)
    {
        if (raw is NetworkStream n)
        {
            n.Socket.Shutdown(SocketShutdown.Send);
        }
    }

    /// <summary>Close the TCP stream at once: a reset, whatever is still unsent.</summary>
    public virtual void Abort(Stream raw)
    {
        if (raw is NetworkStream n)
        {
            try
            {
                n.Socket.LingerState = new LingerOption(true, 0);
            }
            catch (Exception e) when (e is SocketException or ObjectDisposedException)
            {
                // closed already
            }
        }

        raw.Dispose();
    }
}

/// <summary>
/// Core mode: the core speaks HTTP/1.1, WebSocket, CONNECT and SOCKS5 itself, and
/// this carries out only what needs the platform: TCP connections, TLS on them (SslStream, with the
/// operator's CAs, certificate and versions: <see cref="OpAmpClientOptions.Tls"/>), name resolution
/// (only for a SOCKS5 proxy with local resolution: otherwise the proxy resolves), the answer to the
/// core's proxy question (<see cref="OpAmpClient.ProxyAnswerFor"/>). It decides nothing:
/// every byte it writes, the core wrote, and the core keeps every time (it closes a stream it gives
/// up on). As node/opamp/src/streams.ts and
/// python/src/logfire_fleet_client/_streams.py do.
/// </summary>
/// <remarks>
/// <para>
/// One registry (<see cref="In"/>, an <see cref="InFlight"/> keyed by stream id) owns everything
/// under way on the streams: each stream (its socket and TLS, the task that connects it, does its
/// handshakes and reads it, the chain of its writes), each proxy question and each name being
/// resolved, and each closed stream still lingering. Each stream input goes to the core through
/// the client's queue naming its stream, and reaches the core only while its entry is there, so a
/// close (<c>stream_close</c>, or stop) ends what it names at once: nothing about it is reported
/// after, even what was queued before; a connection, a TLS handshake or a resolution that completes
/// after its close is closed as it arrives.
/// </para>
/// <para>
/// Each stream has one task that connects it, then reads it: it hands the core what it read and
/// waits until the core has stepped with it (and this has carried out what that step asked) before
/// it reads again. So what the core asks in answer (TLS, a close) applies to the next bytes read, as on a single-threaded loop; and the TLS handshake the
/// core asks for runs on that task, once what was written before it has gone.
/// </para>
/// <para>
/// Backpressure ("stops reading": it begins no new read; one already under way may still bring
/// one chunk): while more than <see cref="WriteHigh"/> bytes written to a stream are unsent,
/// the stream reads nothing more (a peer that doesn't read hears nothing more to answer), until
/// they are down to <see cref="WriteLow"/>; and a write that finds more than
/// <see cref="MaxUnsent"/> still unsent fails the stream (<c>stream_failed</c>: the core reconnects,
/// as for any lost connection) rather than buffering without end.
/// </para>
/// </remarks>
internal sealed class Streams
{
    /// <summary>How long a closed stream has to flush what was written (and hear the peer's end) before it is reset.</summary>
    public static readonly TimeSpan Linger = TimeSpan.FromSeconds(2);

    /// <summary>Bytes written to a stream and unsent beyond which it stops reading...</summary>
    public const long WriteHigh = 256 << 10;

    /// <summary>...until they are down to this.</summary>
    public const long WriteLow = 64 << 10;

    /// <summary>Bytes written to a stream and not yet taken by the peer beyond which the next write fails it.</summary>
    public const long MaxUnsent = 16 << 20;

    private const int ReadBuffer = 64 << 10;

    private readonly Action<Queued> deliver;
    private readonly StreamsNet net;
    private readonly TimeProvider time;
    private readonly Func<string, ProxyAnswer> proxyFor;
    private readonly SslClientAuthenticationOptions tls;
    private readonly object gate = new();

    // Streams by id, open or connecting (a closed one is gone from here at once).
    private readonly Dictionary<ulong, Conn> conns = new();

    // Closed gracefully, still flushing (Linger at most): stop resets them.
    private readonly HashSet<Conn> lingering = new();

    private bool closed;

    /// <param name="deliver">The client's queue.</param>
    /// <param name="tls">The operator's TLS settings (<see cref="ClientTransport.SslOptions"/>), copied for each handshake.</param>
    /// <param name="proxyFor">The answer to the core's proxy question for a URL.</param>
    /// <param name="net">The network (connects, TLS, resolution): .NET's if none.</param>
    /// <param name="time">The clock a closed stream's linger is kept with: the system's if none.</param>
    public Streams(Action<Queued> deliver, SslClientAuthenticationOptions tls, Func<string, ProxyAnswer> proxyFor, StreamsNet? net = null, TimeProvider? time = null)
    {
        this.deliver = deliver;
        this.tls = tls;
        this.proxyFor = proxyFor;
        this.net = net ?? StreamsNet.Real;
        this.time = time ?? TimeProvider.System;
        this.In = new InFlight(this.time);
    }

    /// <summary>Everything under way on the streams, by stream id.</summary>
    public InFlight In { get; }

    /// <summary>Streams open or connecting, and closed ones still lingering: for tests.</summary>
    internal int Live
    {
        get
        {
            lock (this.gate)
            {
                return this.conns.Count + this.lingering.Count;
            }
        }
    }

    /// <summary>
    /// Work of the streams' that waits on the network or the clock, not on the client: each live
    /// stream's task (connecting, or reading), and a closed stream's linger. For tests, to tell when
    /// the rest of the work is done.
    /// </summary>
    internal int Parked
    {
        get
        {
            lock (this.gate)
            {
                return this.conns.Values.Count(c => c.Driver is { IsCompleted: false })
                    + this.lingering.Sum(c => 1 + (c.Driver is { IsCompleted: false } ? 1 : 0));
            }
        }
    }

    /// <summary>On the loop: carry out <paramref name="e"/> if it is one of core mode's; false if it isn't (it goes to the executor).</summary>
    public bool Run(Effect e)
    {
        switch (e.KindCase)
        {
            case Effect.KindOneofCase.ProxyQuery:
                if (this.In.Register(e.ProxyQuery.Id) is not null)
                {
                    var answer = this.proxyFor(e.ProxyQuery.Url_);
                    answer.Id = e.ProxyQuery.Id;
                    this.Report(e.ProxyQuery.Id, new Input { ProxyAnswer = answer }, final: true);
                }

                return true;
            case Effect.KindOneofCase.StreamResolve:
                this.Resolve(e.StreamResolve.Id, e.StreamResolve.Host);
                return true;
            case Effect.KindOneofCase.StreamOpen:
                this.Open(e.StreamOpen.Id, e.StreamOpen.Host, (int)e.StreamOpen.Port);
                return true;
            case Effect.KindOneofCase.StreamStartTls:
                this.StartTls(e.StreamStartTls.Id, e.StreamStartTls.Host, e.StreamStartTls.ServerOrigin);
                return true;
            case Effect.KindOneofCase.StreamWrite:
                this.Write(e.StreamWrite.Id, e.StreamWrite.Data.ToByteArray());
                return true;
            case Effect.KindOneofCase.StreamClose:
                this.Close(e.StreamClose.Id, e.StreamClose.Abort);
                return true;
            default:
                return false;
        }
    }

    /// <summary>
    /// Stop: every stream is reset, every resolution stopped, every lingering stream reset; the task
    /// ends when their work is over, or at <paramref name="bound"/>.
    /// </summary>
    public Task ShutdownAsync(TimeSpan bound)
    {
        List<Conn> linger;
        lock (this.gate)
        {
            this.closed = true;
            linger = [.. this.lingering];
        }

        foreach (var c in linger)
        {
            this.Abort(c);
        }

        return this.In.CloseAsync(bound);
    }

    private static Input Failed(ulong id, string detail) => new() { StreamFailed = new Failed { Id = id, Detail = detail } };

    private static string Detail(Exception e, string fallback)
    {
        var detail = Executor.Detail(e);
        return detail.Length > 0 ? detail : fallback;
    }

    /// <summary>A stream input for the core, as a result of stream <paramref name="id"/>'s entry.</summary>
    private void Report(ulong id, Input input, bool final = false, TaskCompletionSource? ack = null) =>
        this.deliver(new Queued(input, id, final, this.In, ack));

    // ---- Streams.

    private void Resolve(ulong id, string host)
    {
        if (this.In.Register(id) is not { } ct)
        {
            return;
        }

        this.In.Track(Run());

        async Task Run()
        {
            await Task.Yield();
            Input result;
            try
            {
                var addresses = await this.net.ResolveAsync(host, ct).ConfigureAwait(false);
                var resolved = new StreamResolved { Id = id };
                resolved.Addresses.AddRange(addresses);
                result = new Input { StreamResolved = resolved };
            }
            catch (Exception e)
            {
                // Whatever went wrong (a name that doesn't resolve, a close: then nothing is told).
                result = Failed(id, Detail(e, "no address"));
            }

            this.Report(id, result, final: true);
        }
    }

    private void Open(ulong id, string host, int port)
    {
        lock (this.gate)
        {
            if (this.closed)
            {
                return;
            }
        }

        // A stream of that id still here (never, from the real core) is replaced: reset at once.
        this.Close(id, abort: true);
        if (this.In.Register(id) is not { } token)
        {
            return;
        }

        var c = new Conn(id, token);
        lock (this.gate)
        {
            this.conns[id] = c;
        }

        // Closed (by the core, or stop) from now on: reset, whatever it is doing.
        this.In.Adopt(id, () => this.Abort(c));
        c.Driver = this.DriveAsync(c, host, port);
        this.In.Track(c.Driver);
    }

    private void StartTls(ulong id, string host, bool serverOrigin)
    {
        if (this.Find(id) is not { } c)
        {
            return;
        }

        lock (c.Gate)
        {
            if (c.Gone || c.Closing || !c.Ready)
            {
                return;
            }

            // The stream's task does the handshake once the core has stepped with what asked for
            // it, after what was written before it; what is written after it waits for it.
            c.Ready = false;
            c.TlsHost = host;
            c.TlsServerOrigin = serverOrigin;
            c.BeforeTls = c.Chain;
            c.TlsUp = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            var up = c.TlsUp.Task;
            c.Chain = c.Chain.ContinueWith(_ => up, TaskScheduler.Default).Unwrap();
        }
    }

    private void Write(ulong id, byte[] data)
    {
        if (this.Find(id) is not { } c)
        {
            return;
        }

        lock (c.Gate)
        {
            if (c.Gone || c.Closing)
            {
                return;
            }
        }

        var unsent = Interlocked.Read(ref c.Unsent);
        if (unsent > MaxUnsent)
        {
            // The peer has stopped reading: give up on it rather than buffer without end.
            this.Fail(c, $"the peer has not taken the last {unsent} bytes written to it");
            return;
        }

        if (Interlocked.Add(ref c.Unsent, data.Length) > WriteHigh)
        {
            lock (c.Gate)
            {
                if (!c.Blocked)
                {
                    c.Blocked = true;
                    Hold(c);
                }
            }
        }

        this.Then(c, async () =>
        {
            if (c.Stream is not { } s || c.Gone)
            {
                return;
            }

            try
            {
                await s.WriteAsync(data, c.Token).ConfigureAwait(false);
            }
            catch (Exception e)
            {
                this.Fail(c, Detail(e, "writing failed"));
                return;
            }

            if (Interlocked.Add(ref c.Unsent, -data.Length) <= WriteLow)
            {
                var unblocked = false;
                lock (c.Gate)
                {
                    if (c.Blocked)
                    {
                        c.Blocked = false;
                        unblocked = true;
                    }
                }

                if (unblocked)
                {
                    Release(c);
                }
            }
        });
    }

    /// <summary>
    /// Close stream <paramref name="id"/>: nothing more about it is told. Reset at once if
    /// <paramref name="abort"/>, or if it never got ready (connecting, in a handshake: nothing
    /// written has anywhere to go); else FIN once what was written has gone, then reset once the
    /// peer has ended too, or at <see cref="Linger"/>.
    /// </summary>
    private void Close(ulong id, bool abort)
    {
        Conn? c;
        lock (this.gate)
        {
            if (this.conns.Remove(id, out c) && !abort)
            {
                lock (c.Gate)
                {
                    abort = c.Gone || !c.Ready;
                }
            }
        }

        if (c is null || abort)
        {
            this.In.Cancel(id); // a proxy question, a resolution, or a stream: its release resets it
            return;
        }

        this.In.Forget(id);
        TaskCompletionSource? readable;
        lock (c.Gate)
        {
            c.Closing = true;
            c.Holds = 0;
            readable = c.Readable; // it reads on, into nothing, until the peer's end
        }

        readable.TrySetResult();
        lock (this.gate)
        {
            this.lingering.Add(c);
        }

        this.In.Track(this.LingerAsync(c));
    }

    private async Task LingerAsync(Conn c)
    {
        try
        {
            await Graceful().WaitAsync(Linger, this.time, c.Token).ConfigureAwait(false);
        }
        catch (Exception)
        {
            // past the linger, reset, or the flush failed: reset
        }

        this.Abort(c);

        async Task Graceful()
        {
            await c.Chain.ConfigureAwait(false);
            List<Stream> layers;
            Stream? raw;
            lock (c.Gate)
            {
                layers = [.. c.Layers];
                raw = c.Raw;
            }

            for (var i = layers.Count - 1; i >= 0; i--)
            {
                if (layers[i] is SslStream ssl)
                {
                    await ssl.ShutdownAsync().ConfigureAwait(false); // close_notify
                }
            }

            if (raw is not null)
            {
                this.net.ShutdownSend(raw);
            }

            if (c.Driver is { } driver)
            {
                await driver.ConfigureAwait(false); // the peer's end
            }
        }
    }

    private Conn? Find(ulong id)
    {
        lock (this.gate)
        {
            return this.conns.TryGetValue(id, out var c) ? c : null;
        }
    }

    /// <summary>Run <paramref name="work"/> after everything written to <paramref name="c"/> before it.</summary>
    private void Then(Conn c, Func<Task> work)
    {
        Task next;
        lock (c.Gate)
        {
            next = c.Chain = c.Chain.ContinueWith(_ => work(), TaskScheduler.Default).Unwrap();
        }

        this.In.Track(next);
    }

    /// <summary>Stop reading <paramref name="c"/> (for one more reason).</summary>
    private static void Hold(Conn c)
    {
        lock (c.Gate)
        {
            if (c.Gone || c.Closing)
            {
                return;
            }

            if (c.Holds++ == 0)
            {
                c.Readable = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            }
        }
    }

    /// <summary>One reason fewer not to read <paramref name="c"/>: read again if it was the last.</summary>
    private static void Release(Conn c)
    {
        TaskCompletionSource? open = null;
        lock (c.Gate)
        {
            if (c.Holds == 0)
            {
                return;
            }

            if (--c.Holds == 0)
            {
                open = c.Readable;
            }
        }

        open?.TrySetResult();
    }

    /// <summary>
    /// The stream's task: connect, tell the core; then, each time the core has stepped with what it
    /// was told, start TLS if it asked, or read the next bytes for the core. A failure is told once (unless it was closed first), and resets it.
    /// </summary>
    private async Task DriveAsync(Conn c, string host, int port)
    {
        await Task.Yield();
        Stream raw;
        try
        {
            raw = await this.net.ConnectAsync(host, port, c.Token).ConfigureAwait(false);
        }
        catch (Exception e)
        {
            this.Fail(c, Detail(e, "connecting failed"));
            return;
        }

        lock (c.Gate)
        {
            if (!c.Gone)
            {
                c.Raw = c.Stream = raw;
                c.Ready = true;
            }
        }

        if (c.Raw != raw)
        {
            this.net.Abort(raw); // closed while it connected: it goes at once, untold
            return;
        }

        c.Opened.TrySetResult(); // what the core writes goes from now on
        if (!await this.TellAsync(c, new Input { StreamOpened = new Id { Id_ = c.Id } }).ConfigureAwait(false))
        {
            return;
        }

        var buf = new byte[ReadBuffer];
        try
        {
            while (true)
            {
                string? tlsHost;
                bool serverOrigin;
                TaskCompletionSource? up;
                Task before;
                lock (c.Gate)
                {
                    tlsHost = c.TlsHost;
                    serverOrigin = c.TlsServerOrigin;
                    up = c.TlsUp;
                    before = c.BeforeTls;
                    c.TlsHost = null;
                }

                if (tlsHost is not null)
                {
                    if (!await this.HandshakeAsync(c, tlsHost, serverOrigin, up!, before).ConfigureAwait(false))
                    {
                        return;
                    }

                    continue;
                }

                await this.Readable(c).WaitAsync(c.Token).ConfigureAwait(false);
                var n = await c.Stream!.ReadAsync(buf, c.Token).ConfigureAwait(false);
                bool closing;
                lock (c.Gate)
                {
                    closing = c.Closing;
                }

                if (n == 0)
                {
                    // The peer's FIN: told, unless the core closed the stream (this was its linger).
                    if (!closing)
                    {
                        await this.TellAsync(c, new Input { StreamEof = new Id { Id_ = c.Id } }).ConfigureAwait(false);
                    }

                    return;
                }

                if (closing)
                {
                    continue; // read into nothing until the peer's end
                }

                var data = new WsMessage { Id = c.Id, Data = ByteString.CopyFrom(buf, 0, n) };
                if (!await this.TellAsync(c, new Input { StreamData = data }).ConfigureAwait(false))
                {
                    return;
                }
            }
        }
        catch (Exception e)
        {
            // Gone, with an error or without (a reset, a close): the core must hear it.
            this.Fail(c, Detail(e, "the connection closed"));
        }
    }

    /// <summary>TLS on the stream, once what was written before it has gone; then tell the core.</summary>
    private async Task<bool> HandshakeAsync(Conn c, string host, bool serverOrigin, TaskCompletionSource up, Task before)
    {
        Stream tlsStream;
        try
        {
            await before.WaitAsync(c.Token).ConfigureAwait(false);
            tlsStream = await this.net.AuthenticateAsync(c.Stream!, this.Options(host, serverOrigin), c.Token).ConfigureAwait(false);
        }
        catch (Exception e)
        {
            this.Fail(c, Detail(e, "the TLS handshake failed"));
            up.TrySetCanceled();
            return false;
        }

        bool gone;
        lock (c.Gate)
        {
            gone = c.Gone;
            if (!gone)
            {
                if (!ReferenceEquals(tlsStream, c.Stream))
                {
                    c.Layers.Add(tlsStream);
                }

                c.Stream = tlsStream;
                c.Ready = true;
            }
        }

        if (gone)
        {
            // Closed while in the handshake: it goes at once, untold.
            if (!ReferenceEquals(tlsStream, c.Raw))
            {
                await tlsStream.DisposeAsync().ConfigureAwait(false);
            }

            up.TrySetCanceled();
            return false;
        }

        up.TrySetResult();
        return await this.TellAsync(c, new Input { StreamTlsReady = new Id { Id_ = c.Id } }).ConfigureAwait(false);
    }

    /// <summary>
    /// The operator's TLS settings, for a handshake with <paramref name="host"/> (its certificate
    /// verified for it; SNI unless it is an address). The client certificate only when the core
    /// says the handshake is with the server's own origin (<paramref name="serverOrigin"/>); the
    /// CAs and versions for every one.
    /// </summary>
    private SslClientAuthenticationOptions Options(string host, bool serverOrigin) => new()
    {
        TargetHost = host,
        EnabledSslProtocols = this.tls.EnabledSslProtocols,
        CertificateChainPolicy = this.tls.CertificateChainPolicy?.Clone(),
        RemoteCertificateValidationCallback = this.tls.RemoteCertificateValidationCallback,
        ClientCertificateContext = serverOrigin ? this.tls.ClientCertificateContext : null,
        CertificateRevocationCheckMode = this.tls.CertificateRevocationCheckMode,
    };

    private Task Readable(Conn c)
    {
        lock (c.Gate)
        {
            return c.Holds == 0 ? Task.CompletedTask : c.Readable.Task;
        }
    }

    /// <summary>
    /// Tell the core <paramref name="input"/>, read from <paramref name="c"/>, and wait until it has
    /// stepped with it (or dropped it): false if the stream is gone meanwhile.
    /// </summary>
    private async Task<bool> TellAsync(Conn c, Input input)
    {
        var ack = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        this.Report(c.Id, input, ack: ack);
        try
        {
            await ack.Task.WaitAsync(c.Token).ConfigureAwait(false);
        }
        catch (OperationCanceledException)
        {
            return false;
        }

        lock (c.Gate)
        {
            return !c.Gone;
        }
    }

    /// <summary>The stream failed: the core hears it once (unless it closed the stream first), and it is reset.</summary>
    private void Fail(Conn c, string detail)
    {
        if (Interlocked.Exchange(ref c.Failed, 1) != 0)
        {
            return;
        }

        bool tell;
        lock (c.Gate)
        {
            tell = !c.Gone && !c.Closing;
        }

        if (tell)
        {
            this.Report(c.Id, Failed(c.Id, detail), final: true); // its entry goes as the core takes it
        }

        this.Abort(c);
    }

    /// <summary>Reset <paramref name="c"/> at once: its task stops, its TLS and socket go.</summary>
    private void Abort(Conn c)
    {
        List<Stream> layers;
        Stream? raw;
        lock (c.Gate)
        {
            if (c.Gone)
            {
                return;
            }

            c.Gone = true;
            layers = [.. c.Layers];
            raw = c.Raw;
        }

        lock (this.gate)
        {
            this.lingering.Remove(c);
            if (this.conns.TryGetValue(c.Id, out var mine) && mine == c)
            {
                this.conns.Remove(c.Id);
            }
        }

        try
        {
            c.Cts.Cancel();
        }
        catch (AggregateException)
        {
        }

        c.Opened.TrySetCanceled();
        c.TlsUp?.TrySetCanceled();
        for (var i = layers.Count - 1; i >= 0; i--)
        {
            try
            {
                layers[i].Dispose();
            }
            catch (Exception)
            {
                // gone already
            }
        }

        if (raw is not null)
        {
            try
            {
                this.net.Abort(raw);
            }
            catch (Exception)
            {
                // gone already
            }
        }
    }

    /// <summary>One stream: its socket and TLS, its task, its writes, why it isn't reading.</summary>
    private sealed class Conn
    {
        public Conn(ulong id, CancellationToken registered)
        {
            this.Id = id;
            this.Cts = CancellationTokenSource.CreateLinkedTokenSource(registered);
            this.Token = this.Cts.Token;
            this.Chain = this.Opened.Task;
        }

        public ulong Id { get; }

        public object Gate { get; } = new();

        /// <summary>Cancelled when it is reset (a close of the core's, or stop: the registry's token too).</summary>
        public CancellationTokenSource Cts { get; }

        public CancellationToken Token { get; }

        /// <summary>Connected: the writes go from then on.</summary>
        public TaskCompletionSource Opened { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);

        /// <summary>The TCP stream.</summary>
        public Stream? Raw { get; set; }

        /// <summary>What the core's bytes go over: the TCP stream, or the last TLS on it.</summary>
        public Stream? Stream { get; set; }

        /// <summary>TLS on it, innermost first.</summary>
        public List<Stream> Layers { get; } = new();

        /// <summary>Connected (or TLS up), not in a handshake.</summary>
        public bool Ready { get; set; }

        /// <summary>The core asked for TLS for this host: the task starts it next.</summary>
        public string? TlsHost { get; set; }

        /// <summary>And the handshake is with the server's own origin: the client certificate goes.</summary>
        public bool TlsServerOrigin { get; set; }

        /// <summary>The writes before the TLS asked for, and what the writes after it wait for.</summary>
        public Task BeforeTls { get; set; } = Task.CompletedTask;

        public TaskCompletionSource? TlsUp { get; set; }

        /// <summary>Reset: nothing more is done or told.</summary>
        public bool Gone { get; set; }

        /// <summary>Closed by the core, lingering: nothing more is told.</summary>
        public bool Closing { get; set; }

        /// <summary>Why it is not reading: how many reasons (a full write buffer).</summary>
        public int Holds { get; set; }

        public bool Blocked { get; set; }

        /// <summary>Completed while it may read.</summary>
        public TaskCompletionSource Readable { get; set; } = Completed();

        /// <summary>The last of its writes (after the connect).</summary>
        public Task Chain { get; set; }

        public Task? Driver { get; set; }

        public long Unsent;

        public int Failed;

        private static TaskCompletionSource Completed()
        {
            var t = new TaskCompletionSource();
            t.SetResult();
            return t;
        }
    }
}
