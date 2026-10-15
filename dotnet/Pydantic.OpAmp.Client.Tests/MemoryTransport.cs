// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Net;
using System.Net.Sockets;
using System.Net.WebSockets;
using System.Security.Authentication;
using System.Threading.Channels;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// The exceptions a transport may throw, by number: those the HTTP stack documents, and those it
/// doesn't (S7's class: a URL the parser refuses, a timer's range, a cancel that isn't ours, a
/// disposed object, an application's own type).
/// </summary>
internal static class Faults
{
    public static readonly string[] Names =
    [
        "HttpRequestException", "IOException", "OperationCanceledException (another token)", "TaskCanceledException",
        "TimeoutException", "ArgumentOutOfRangeException", "UriFormatException", "ObjectDisposedException",
        "InvalidOperationException", "NotSupportedException", "NullReferenceException", "WebSocketException",
        "SocketException", "AuthenticationException", "AggregateException", "TransportBug (custom)",
        "ArgumentException", "FormatException", "KeyNotFoundException", "HttpIOException",
    ];

    public static int Count => Names.Length;

    public static Exception Make(int i) => (i % Count) switch
    {
        0 => new HttpRequestException("refused (injected)"),
        1 => new IOException("broken pipe (injected)"),
        2 => new OperationCanceledException("cancelled, but not by the client (injected)", new CancellationToken(true)),
        3 => new TaskCanceledException("a task cancelled (injected)"),
        4 => new TimeoutException("timed out (injected)"),
        5 => new ArgumentOutOfRangeException("delay", "past the timer's range (injected)"),
        6 => new UriFormatException("a URL the parser refuses (injected)"),
        7 => new ObjectDisposedException("handler", "disposed (injected)"),
        8 => new InvalidOperationException("an invalid operation (injected)"),
        9 => new NotSupportedException("not supported (injected)"),
        10 => new NullReferenceException("a null (injected)"),
        11 => new WebSocketException(WebSocketError.ConnectionClosedPrematurely, "closed early (injected)"),
        12 => new SocketException((int)SocketError.ConnectionReset),
        13 => new AuthenticationException("TLS refused (injected)"),
        14 => new AggregateException(new IOException("inside an aggregate (injected)")),
        15 => new TransportBug("an exception type nobody expects (injected)"),
        16 => new ArgumentException("a header the stack refuses (injected)"),
        17 => new FormatException("a bad format (injected)"),
        18 => new KeyNotFoundException("no such key (injected)"),
        _ => new HttpIOException(HttpRequestError.ResponseEnded, "the response ended (injected)"),
    };

    /// <summary>An exception type of the application's or a library's own.</summary>
    internal sealed class TransportBug(string message) : Exception(message);
}

/// <summary>What a transport call is.</summary>
internal enum CallKind
{
    Post,
    Get,
    Connect,
}

/// <summary>
/// A transport in memory (<see cref="IClientTransport"/>): every request, download and WebSocket
/// connect is a <see cref="Call"/> the test answers when it likes, with a response, a connection,
/// or any exception; it may hold, and a deaf call ignores its cancel and completes after it. It
/// keeps everything it handed out, to check that the client released it.
/// </summary>
internal sealed class MemoryTransport : IClientTransport
{
    private readonly object gate = new();
    private readonly List<Call> calls = new();
    private readonly Func<CallKind, int, bool> deaf;
    private int version;

    /// <param name="deaf">Whether the n-th call of a kind ignores its cancel.</param>
    /// <param name="onCall">Called as each call is made (on the client's thread): it may answer at once, or throw (synchronously).</param>
    public MemoryTransport(Func<CallKind, int, bool>? deaf = null, Action<Call>? onCall = null)
    {
        this.deaf = deaf ?? ((_, _) => false);
        this.OnCall = onCall;
    }

    public Action<Call>? OnCall { get; set; }

    /// <summary>Set once stop has returned: a call after it is the client starting work after stop.</summary>
    public string? ClosedBecause { get; set; }

    /// <summary>Calls made after <see cref="ClosedBecause"/> was set.</summary>
    public List<string> Late { get; } = new();

    /// <summary>Changes with every call and answer: the test waits for it to settle.</summary>
    public int Version => Volatile.Read(ref this.version);

    /// <summary>Package files served, by URL.</summary>
    public Dictionary<string, byte[]> Files { get; } = new();

    public Call[] Calls
    {
        get
        {
            lock (this.gate)
            {
                return this.calls.ToArray();
            }
        }
    }

    public void Touch() => Interlocked.Increment(ref this.version);

    public Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken ct)
    {
        var body = request.Content is { } content ? content.ReadAsByteArrayAsync(CancellationToken.None).GetAwaiter().GetResult() : [];
        var kind = request.Method == HttpMethod.Post ? CallKind.Post : CallKind.Get;
        var call = this.Make(kind, request.RequestUri!, body, ct);
        return Unwrap(call);

        static async Task<HttpResponseMessage> Unwrap(Call call) => (HttpResponseMessage)await call.Headers.Task.ConfigureAwait(false);
    }

    public Task<WebSocket> ConnectAsync(Uri url, IEnumerable<KeyValuePair<string, string>> headers, CancellationToken ct)
    {
        var call = this.Make(CallKind.Connect, url, [], ct);
        return Unwrap(call);

        static async Task<WebSocket> Unwrap(Call call) => (WebSocket)await call.Headers.Task.ConfigureAwait(false);
    }

    public void Dispose()
    {
    }

    /// <summary>
    /// A call made by Rust mode's in-memory server (<see cref="MemServer"/>) for a request the core
    /// sent over a stream: answered by the test as any other; <paramref name="ct"/> is the client
    /// leaving the stream.
    /// </summary>
    public Call Serve(CallKind kind, Uri url, byte[] body, CancellationToken ct) => this.Make(kind, url, body, ct);

    private Call Make(CallKind kind, Uri url, byte[] body, CancellationToken ct)
    {
        Call call;
        lock (this.gate)
        {
            var n = this.calls.Count(c => c.Kind == kind);
            call = new Call(this, kind, this.calls.Count, n, url, body, ct, this.deaf(kind, n));
            this.calls.Add(call);
            if (this.ClosedBecause is { } why)
            {
                this.Late.Add($"{kind} {url} ({why})");
            }
        }

        this.Touch();
        this.OnCall?.Invoke(call); // may throw: a synchronous failure
        call.ListenForCancel();
        return call;
    }
}

/// <summary>
/// One call: its headers (or connection) pending until answered; then, for an HTTP call, its body,
/// which may be held too.
/// </summary>
internal sealed class Call
{
    private readonly MemoryTransport owner;

    public Call(MemoryTransport owner, CallKind kind, int index, int ofKind, Uri url, byte[] body, CancellationToken ct, bool deaf)
    {
        this.owner = owner;
        this.Kind = kind;
        this.Index = index;
        this.OfKind = ofKind;
        this.Url = url;
        this.Body = body;
        this.Ct = ct;
        this.Deaf = deaf;
    }

    public CallKind Kind { get; }

    /// <summary>Among all calls.</summary>
    public int Index { get; }

    /// <summary>Among calls of its kind: the n-th request, download or connect, as the effects were issued.</summary>
    public int OfKind { get; }

    public Uri Url { get; }

    public byte[] Body { get; }

    public CancellationToken Ct { get; }

    /// <summary>Ignores its cancel: answered only by the test, perhaps after the cancel.</summary>
    public bool Deaf { get; }

    /// <summary>The response or the connection, or the failure.</summary>
    public TaskCompletionSource<object> Headers { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);

    /// <summary>The response's body, once answered (HTTP).</summary>
    public MemBody? Response { get; private set; }

    /// <summary>The connection, once answered (WebSocket).</summary>
    public MemSocket? Socket { get; private set; }

    /// <summary>How the test answered, for the log.</summary>
    public string? Answer { get; private set; }

    /// <summary>Whether its cancel had come when the test answered it.</summary>
    public bool CancelledFirst { get; private set; }

    /// <summary>Still waiting: the headers, or a held body the client still reads.</summary>
    public bool Open => !this.Headers.Task.IsCompleted || this.Response is { Held: true, Released: false };

    /// <summary>Whatever it handed the client has been let go of (or it handed nothing).</summary>
    public bool Released =>
        (this.Response?.Released ?? true) && (this.Socket?.Released ?? true);

    public override string ToString() =>
        $"#{this.Index} {this.Kind} {this.OfKind} {this.Url}{(this.Deaf ? " deaf" : string.Empty)}: {this.Answer ?? "unanswered"}"
        + $"{(this.Ct.IsCancellationRequested ? ", cancelled" : string.Empty)}{(this.Released ? string.Empty : ", NOT released")}";

    /// <summary>A response: <paramref name="status"/>, <paramref name="body"/>; the body held if <paramref name="hold"/>, or failing with <paramref name="bodyFault"/>.</summary>
    public bool Respond(int status, byte[] body, bool hold = false, Exception? bodyFault = null, string? retryAfter = null)
    {
        var mem = new MemBody(this.owner, body, this.Ct, this.Deaf) { Held = hold, Fault = bodyFault };
        var resp = new HttpResponseMessage((HttpStatusCode)status) { Content = new MemContent(mem) };
        if (retryAfter is not null)
        {
            resp.Headers.TryAddWithoutValidation("Retry-After", retryAfter);
        }

        resp.Content.Headers.TryAddWithoutValidation("Content-Type", "application/x-protobuf");
        this.CancelledFirst = this.Ct.IsCancellationRequested;
        if (!this.Headers.TrySetResult(resp))
        {
            resp.Dispose();
            return false;
        }

        this.Response = mem;
        this.Answer = $"{status}{(hold ? " body held" : string.Empty)}{(bodyFault is null ? string.Empty : $" body fails: {bodyFault.GetType().Name}")}";
        this.owner.Touch();
        return true;
    }

    /// <summary>A WebSocket connection.</summary>
    public bool Accept(MemSocket socket)
    {
        this.CancelledFirst = this.Ct.IsCancellationRequested;
        if (!this.Headers.TrySetResult(socket))
        {
            return false;
        }

        this.Socket = socket;
        this.Answer = "connected";
        this.owner.Touch();
        return true;
    }

    /// <summary>Fail with <paramref name="e"/>.</summary>
    public bool Fail(Exception e)
    {
        this.CancelledFirst = this.Ct.IsCancellationRequested;
        if (!this.Headers.TrySetException(e))
        {
            return false;
        }

        this.Answer = $"threw {e.GetType().Name}";
        this.owner.Touch();
        return true;
    }

    /// <summary>A held body: go on (with what it holds) or fail.</summary>
    public bool Release(Exception? fault)
    {
        if (this.Response is not { Held: true } body)
        {
            return false;
        }

        this.CancelledFirst = this.Ct.IsCancellationRequested;
        this.Answer += fault is null ? ", then the body" : $", then the body threw {fault.GetType().Name}";
        body.Go(fault);
        this.owner.Touch();
        return true;
    }

    /// <summary>A call that hears its cancel fails with it, as the platform's would.</summary>
    public void ListenForCancel()
    {
        if (this.Deaf)
        {
            return;
        }

        this.Ct.Register(() =>
        {
            if (this.Headers.TrySetCanceled(this.Ct))
            {
                this.Answer ??= "cancelled";
                this.owner.Touch();
            }
        });
    }
}

/// <summary>A response's content: its body, tracked.</summary>
internal sealed class MemContent(MemBody body) : HttpContent
{
    protected override Task SerializeToStreamAsync(Stream stream, TransportContext? context) => body.CopyToAsync(stream);

    protected override Task<Stream> CreateContentReadStreamAsync() => Task.FromResult<Stream>(body);

    protected override Task<Stream> CreateContentReadStreamAsync(CancellationToken cancellationToken) => Task.FromResult<Stream>(body);

    protected override bool TryComputeLength(out long length)
    {
        length = -1;
        return false;
    }

    protected override void Dispose(bool disposing)
    {
        body.Dispose();
        base.Dispose(disposing);
    }
}

/// <summary>A body that serves its bytes, or holds until told, or fails; released once disposed.</summary>
internal sealed class MemBody(MemoryTransport owner, byte[] data, CancellationToken ct, bool deaf) : Stream
{
    private readonly TaskCompletionSource gate = new(TaskCreationOptions.RunContinuationsAsynchronously);
    private int at;
    private int released;

    public bool Held { get; set; }

    public Exception? Fault { get; set; }

    /// <summary>How long the body is (Rust mode's server says so in Content-Length).</summary>
    public int Size => data.Length;

    public bool Released => Volatile.Read(ref this.released) != 0;

    public override bool CanRead => true;

    public override bool CanSeek => false;

    public override bool CanWrite => false;

    public override long Length => throw new NotSupportedException();

    public override long Position { get => this.at; set => throw new NotSupportedException(); }

    public void Go(Exception? fault)
    {
        this.Fault ??= fault;
        this.Held = false;
        this.gate.TrySetResult();
    }

    public override async ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken cancellationToken = default)
    {
        if (this.Held)
        {
            if (deaf)
            {
                await this.gate.Task.ConfigureAwait(false);
            }
            else
            {
                await this.gate.Task.WaitAsync(cancellationToken).ConfigureAwait(false);
            }
        }

        if (this.Fault is { } e)
        {
            throw e;
        }

        if (!deaf)
        {
            cancellationToken.ThrowIfCancellationRequested();
        }

        var n = Math.Min(buffer.Length, data.Length - this.at);
        data.AsMemory(this.at, n).CopyTo(buffer);
        this.at += n;
        return n;
    }

    public override Task<int> ReadAsync(byte[] buffer, int offset, int count, CancellationToken cancellationToken) =>
        this.ReadAsync(buffer.AsMemory(offset, count), cancellationToken).AsTask();

    public override int Read(byte[] buffer, int offset, int count) => this.ReadAsync(buffer, offset, count, ct).GetAwaiter().GetResult();

    public override void Flush()
    {
    }

    public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();

    public override void SetLength(long value) => throw new NotSupportedException();

    public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();

    protected override void Dispose(bool disposing)
    {
        if (Interlocked.Exchange(ref this.released, 1) == 0)
        {
            owner.Touch();
        }

        base.Dispose(disposing);
    }
}

/// <summary>
/// A WebSocket connection in memory: the test plays the server (messages, its Close, failures of
/// the receive, the next send or the close); released once aborted, disposed or closed.
/// </summary>
internal sealed class MemSocket(MemoryTransport owner) : WebSocket
{
    private readonly Channel<Frame> frames = Channel.CreateUnbounded<Frame>();
    private readonly object gate = new();
    private readonly TaskCompletionSource aborted = new(TaskCreationOptions.RunContinuationsAsynchronously);
    private WebSocketState state = WebSocketState.Open;
    private WebSocketCloseStatus? closeStatus;
    private Exception? sendFault;
    private Exception? closeFault;
    private bool holdClose;
    private int released;
    private int receiving;

    /// <summary>What the client sent, each message whole (the OpAMP header byte and the message).</summary>
    public List<byte[]> Sent { get; } = new();

    public bool Released => Volatile.Read(ref this.released) != 0;

    /// <summary>Whether the client has begun to receive (it reports the connection opened before it does).</summary>
    public bool Receiving => Volatile.Read(ref this.receiving) != 0;

    /// <summary>A broken send fails its task, rather than throwing as it is called.</summary>
    public bool FaultsAsync { get; init; }

    public override WebSocketCloseStatus? CloseStatus => this.closeStatus;

    public override string? CloseStatusDescription => null;

    public override WebSocketState State => this.state;

    public override string? SubProtocol => null;

    /// <summary>The server sends <paramref name="data"/>.</summary>
    public void Push(byte[] data) => this.frames.Writer.TryWrite(new Frame(data, null, null));

    /// <summary>The server sends its Close.</summary>
    public void PushClose(WebSocketCloseStatus status) => this.frames.Writer.TryWrite(new Frame(null, status, null));

    /// <summary>The pending (or next) receive fails with <paramref name="e"/>.</summary>
    public void BreakReceive(Exception e) => this.frames.Writer.TryWrite(new Frame(null, null, e));

    /// <summary>The next send fails with <paramref name="e"/>.</summary>
    public void BreakSend(Exception e)
    {
        lock (this.gate)
        {
            this.sendFault = e;
        }
    }

    /// <summary>The close fails with <paramref name="e"/>, or (null) is never answered.</summary>
    public void BreakClose(Exception? e)
    {
        lock (this.gate)
        {
            this.closeFault = e;
            this.holdClose = e is null;
        }
    }

    public override void Abort()
    {
        lock (this.gate)
        {
            if (this.state is not (WebSocketState.Closed or WebSocketState.Aborted))
            {
                this.state = WebSocketState.Aborted;
            }
        }

        this.frames.Writer.TryWrite(new Frame(null, null, new WebSocketException(WebSocketError.InvalidState, "aborted")));
        this.aborted.TrySetResult();
        this.Release();
    }

    public override async Task CloseAsync(WebSocketCloseStatus closeStatus, string? statusDescription, CancellationToken cancellationToken)
    {
        Exception? fault;
        bool hold;
        lock (this.gate)
        {
            fault = this.closeFault;
            hold = this.holdClose;
            if (this.state is WebSocketState.Aborted or WebSocketState.Closed)
            {
                throw new WebSocketException(WebSocketError.InvalidState, "not open");
            }
        }

        if (fault is not null)
        {
            throw fault;
        }

        if (hold)
        {
            await this.aborted.Task.WaitAsync(cancellationToken).ConfigureAwait(false);
            throw new WebSocketException(WebSocketError.InvalidState, "aborted while closing");
        }

        // The server answers with its own Close, which the reader takes; then it is closed.
        this.frames.Writer.TryWrite(new Frame(null, closeStatus, null));
        lock (this.gate)
        {
            this.closeStatus ??= closeStatus;
            this.state = WebSocketState.Closed;
        }

        this.Release();
    }

    public override Task CloseOutputAsync(WebSocketCloseStatus closeStatus, string? statusDescription, CancellationToken cancellationToken) =>
        this.CloseAsync(closeStatus, statusDescription, cancellationToken);

    public override void Dispose()
    {
        lock (this.gate)
        {
            if (this.state is not (WebSocketState.Closed or WebSocketState.Aborted))
            {
                this.state = WebSocketState.Aborted;
            }
        }

        this.frames.Writer.TryWrite(new Frame(null, null, new ObjectDisposedException(nameof(MemSocket))));
        this.aborted.TrySetResult();
        this.Release();
    }

    public override async Task<WebSocketReceiveResult> ReceiveAsync(ArraySegment<byte> buffer, CancellationToken cancellationToken)
    {
        Volatile.Write(ref this.receiving, 1);
        var f = await this.frames.Reader.ReadAsync(cancellationToken).ConfigureAwait(false);
        if (f.Fault is { } e)
        {
            this.frames.Writer.TryWrite(f); // broken for good
            throw e;
        }

        if (f.Close is { } status)
        {
            lock (this.gate)
            {
                this.closeStatus ??= status;
                if (this.state == WebSocketState.Open)
                {
                    this.state = WebSocketState.CloseReceived;
                }
            }

            return new WebSocketReceiveResult(0, WebSocketMessageType.Close, true, status, null);
        }

        var data = f.Data!;
        data.CopyTo(buffer.Array!, buffer.Offset);
        return new WebSocketReceiveResult(data.Length, WebSocketMessageType.Binary, true);
    }

    public override Task SendAsync(ArraySegment<byte> buffer, WebSocketMessageType messageType, bool endOfMessage, CancellationToken cancellationToken)
    {
        lock (this.gate)
        {
            if (this.sendFault is { } e)
            {
                this.sendFault = null;
                return this.FaultsAsync ? Task.FromException(e) : throw e;
            }

            if (this.state is not (WebSocketState.Open or WebSocketState.CloseReceived))
            {
                throw new WebSocketException(WebSocketError.InvalidState, "not open");
            }

            this.Sent.Add(buffer.ToArray());
        }

        owner.Touch();
        return Task.CompletedTask;
    }

    private void Release()
    {
        if (Interlocked.Exchange(ref this.released, 1) == 0)
        {
            owner.Touch();
        }
    }

    private sealed record Frame(byte[]? Data, WebSocketCloseStatus? Close, Exception? Fault);
}
