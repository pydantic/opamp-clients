// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Net.Security;
using System.Text;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// One direction of an in-memory connection: what one end writes, the other reads, in order, with
/// a bounded buffer (a writer waits while it is full: backpressure, as a socket's window), an end
/// (FIN: the reader reads what is left, then 0) and a reset (both ends' next operation throws).
/// </summary>
internal sealed class MemPipe(int capacity)
{
    private readonly object gate = new();
    private readonly LinkedList<byte[]> chunks = new();
    private TaskCompletionSource changed = new(TaskCreationOptions.RunContinuationsAsynchronously);
    private int buffered;
    private int offset;
    private bool finished;
    private Exception? reset;

    /// <summary>Bytes written and not yet read.</summary>
    public int Buffered
    {
        get
        {
            lock (this.gate)
            {
                return this.buffered;
            }
        }
    }

    public bool Finished
    {
        get
        {
            lock (this.gate)
            {
                return this.finished;
            }
        }
    }

    public bool IsReset
    {
        get
        {
            lock (this.gate)
            {
                return this.reset is not null;
            }
        }
    }

    public async ValueTask WriteAsync(ReadOnlyMemory<byte> data, CancellationToken ct)
    {
        while (true)
        {
            Task wait;
            lock (this.gate)
            {
                if (this.reset is { } e)
                {
                    throw new IOException("the connection was reset", e);
                }

                if (this.finished)
                {
                    throw new IOException("written after the end");
                }

                var room = capacity - this.buffered;
                if (room > 0)
                {
                    var n = Math.Min(room, data.Length);
                    this.chunks.AddLast(data[..n].ToArray());
                    this.buffered += n;
                    data = data[n..];
                    this.Signal();
                    if (data.IsEmpty)
                    {
                        return;
                    }
                }

                wait = this.changed.Task;
            }

            await wait.WaitAsync(ct).ConfigureAwait(false);
        }
    }

    public async ValueTask<int> ReadAsync(Memory<byte> into, CancellationToken ct)
    {
        while (true)
        {
            Task wait;
            lock (this.gate)
            {
                if (this.reset is { } e)
                {
                    throw new IOException("the connection was reset", e);
                }

                if (this.buffered > 0 && into.Length > 0)
                {
                    var n = 0;
                    while (n < into.Length && this.chunks.First is { } first)
                    {
                        var take = Math.Min(into.Length - n, first.Value.Length - this.offset);
                        first.Value.AsMemory(this.offset, take).CopyTo(into[n..]);
                        n += take;
                        this.offset += take;
                        if (this.offset == first.Value.Length)
                        {
                            this.chunks.RemoveFirst();
                            this.offset = 0;
                        }
                    }

                    this.buffered -= n;
                    this.Signal();
                    return n;
                }

                if (this.finished || into.Length == 0)
                {
                    return 0;
                }

                wait = this.changed.Task;
            }

            await wait.WaitAsync(ct).ConfigureAwait(false);
        }
    }

    /// <summary>The writer's end (FIN).</summary>
    public void Finish()
    {
        lock (this.gate)
        {
            this.finished = true;
            this.Signal();
        }
    }

    /// <summary>A reset: what is buffered is lost, and both ends' next operation throws.</summary>
    public void Reset(Exception why)
    {
        lock (this.gate)
        {
            this.reset ??= why;
            this.chunks.Clear();
            this.buffered = 0;
            this.Signal();
        }
    }

    private void Signal()
    {
        var old = this.changed;
        this.changed = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        old.TrySetResult();
    }
}

/// <summary>One end of an in-memory connection: reads one pipe, writes the other.</summary>
internal sealed class MemEnd : Stream
{
    private int closed;

    private MemEnd(MemPipe inbound, MemPipe outbound, string name)
    {
        this.Inbound = inbound;
        this.Outbound = outbound;
        this.Name = name;
    }

    public MemPipe Inbound { get; }

    public MemPipe Outbound { get; }

    public string Name { get; }

    /// <summary>Reset or disposed (by this end).</summary>
    public bool Closed => Volatile.Read(ref this.closed) != 0;

    /// <summary>Its peer reset the connection, or it did.</summary>
    public bool WasReset => this.Inbound.IsReset || this.Outbound.IsReset;

    public override bool CanRead => true;

    public override bool CanSeek => false;

    public override bool CanWrite => true;

    public override long Length => throw new NotSupportedException();

    public override long Position { get => throw new NotSupportedException(); set => throw new NotSupportedException(); }

    /// <summary>A connection: the client's end and the server's.</summary>
    public static (MemEnd Client, MemEnd Server) Pair(int capacity = 64 << 10, string name = "conn")
    {
        var up = new MemPipe(capacity);
        var down = new MemPipe(capacity);
        return (new MemEnd(down, up, name + " (client)"), new MemEnd(up, down, name + " (server)"));
    }

    public override ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken cancellationToken = default) =>
        this.Inbound.ReadAsync(buffer, cancellationToken);

    public override Task<int> ReadAsync(byte[] buffer, int offset, int count, CancellationToken cancellationToken) =>
        this.ReadAsync(buffer.AsMemory(offset, count), cancellationToken).AsTask();

    public override int Read(byte[] buffer, int offset, int count) => this.ReadAsync(buffer, offset, count, CancellationToken.None).GetAwaiter().GetResult();

    public override ValueTask WriteAsync(ReadOnlyMemory<byte> buffer, CancellationToken cancellationToken = default) =>
        this.Outbound.WriteAsync(buffer, cancellationToken);

    public override Task WriteAsync(byte[] buffer, int offset, int count, CancellationToken cancellationToken) =>
        this.WriteAsync(buffer.AsMemory(offset, count), cancellationToken).AsTask();

    public override void Write(byte[] buffer, int offset, int count) => this.WriteAsync(buffer, offset, count, CancellationToken.None).GetAwaiter().GetResult();

    public override void Flush()
    {
    }

    public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();

    public override void SetLength(long value) => throw new NotSupportedException();

    /// <summary>FIN: nothing more is written.</summary>
    public void ShutdownSend() => this.Outbound.Finish();

    /// <summary>A reset, both ways.</summary>
    public void Abort()
    {
        Interlocked.Exchange(ref this.closed, 1);
        var why = new IOException($"{this.Name} reset");
        this.Outbound.Reset(why);
        this.Inbound.Reset(why);
    }

    /// <summary>Read what comes until the peer's end (or a reset): what was read.</summary>
    public async Task<byte[]> ReadToEndAsync(TimeSpan within)
    {
        using var buf = new MemoryStream();
        var chunk = new byte[16 << 10];
        using var cts = new CancellationTokenSource(within);
        try
        {
            int n;
            while ((n = await this.ReadAsync(chunk, cts.Token)) > 0)
            {
                buf.Write(chunk, 0, n);
            }
        }
        catch (IOException)
        {
        }

        return buf.ToArray();
    }

    /// <summary>Read until <paramref name="n"/> bytes have come.</summary>
    public async Task<byte[]> ReadExactAsync(int n, TimeSpan? within = null)
    {
        var buf = new byte[n];
        using var cts = new CancellationTokenSource(within ?? TimeSpan.FromSeconds(10));
        await this.ReadExactlyAsync(buf, cts.Token);
        return buf;
    }

    /// <summary>Write <paramref name="text"/> (ASCII).</summary>
    public Task WriteAsync(string text) => this.WriteAsync(Encoding.ASCII.GetBytes(text)).AsTask();

    protected override void Dispose(bool disposing)
    {
        if (Interlocked.Exchange(ref this.closed, 1) == 0)
        {
            var why = new ObjectDisposedException(this.Name);
            this.Outbound.Reset(why);
            this.Inbound.Reset(why);
        }

        base.Dispose(disposing);
    }
}

/// <summary>
/// Core mode's network in memory (<see cref="StreamsNet"/>): every connect, TLS handshake and name
/// resolution waits for the test (or <see cref="OnDial"/>) to decide it. A deaf one ignores its
/// cancel and completes when told, after it. TLS here is no TLS: the handshake hands back the
/// stream it was given, so the server reads the core's bytes as they are.
/// </summary>
internal sealed class MemNet : StreamsNet
{
    private readonly object gate = new();
    private readonly List<Dial> dials = new();
    private readonly List<Handshake> handshakes = new();
    private readonly List<Lookup> lookups = new();

    /// <summary>Whether the n-th connect, handshake or lookup ignores its cancel.</summary>
    public Func<string, int, bool> Deaf { get; set; } = (_, _) => false;

    /// <summary>Called as each connect is made: it may answer it at once.</summary>
    public Action<Dial>? OnDial { get; set; }

    /// <summary>Called as each handshake is asked for: it may answer it at once.</summary>
    public Action<Handshake>? OnHandshake { get; set; }

    /// <summary>Called as each lookup is asked for: it may answer it at once.</summary>
    public Action<Lookup>? OnLookup { get; set; }

    /// <summary>The capacity of each direction of a connection (its window).</summary>
    public int Capacity { get; set; } = 64 << 10;

    public Dial[] Dials
    {
        get
        {
            lock (this.gate)
            {
                return [.. this.dials];
            }
        }
    }

    public Handshake[] Handshakes
    {
        get
        {
            lock (this.gate)
            {
                return [.. this.handshakes];
            }
        }
    }

    public Lookup[] Lookups
    {
        get
        {
            lock (this.gate)
            {
                return [.. this.lookups];
            }
        }
    }

    public override Task<Stream> ConnectAsync(string host, int port, CancellationToken ct)
    {
        Dial d;
        lock (this.gate)
        {
            d = new Dial(this, host, port, ct, this.Deaf("connect", this.dials.Count), this.dials.Count);
            this.dials.Add(d);
        }

        this.OnDial?.Invoke(d);
        d.Listen();
        return d.Task;
    }

    public override Task<Stream> AuthenticateAsync(Stream inner, SslClientAuthenticationOptions options, CancellationToken ct)
    {
        Handshake h;
        lock (this.gate)
        {
            h = new Handshake(inner, options.TargetHost ?? string.Empty, ct, this.Deaf("tls", this.handshakes.Count))
            {
                PresentsCertificate = options.ClientCertificateContext is not null || options.ClientCertificates is { Count: > 0 },
            };
            this.handshakes.Add(h);
        }

        this.OnHandshake?.Invoke(h);
        h.Listen();
        return h.Task;
    }

    public override Task<IReadOnlyList<string>> ResolveAsync(string host, CancellationToken ct)
    {
        Lookup l;
        lock (this.gate)
        {
            l = new Lookup(host, ct, this.Deaf("resolve", this.lookups.Count));
            this.lookups.Add(l);
        }

        this.OnLookup?.Invoke(l);
        l.Listen();
        return l.Task;
    }

    public override void ShutdownSend(Stream raw) => ((MemEnd)raw).ShutdownSend();

    public override void Abort(Stream raw) => ((MemEnd)raw).Abort();

    /// <summary>Something the test decides: a result, or a failure; deaf, it ignores its cancel.</summary>
    internal abstract class Pending<T>(CancellationToken ct, bool deaf)
    {
        private readonly TaskCompletionSource<T> tcs = new(TaskCreationOptions.RunContinuationsAsynchronously);

        public CancellationToken Ct { get; } = ct;

        public bool Deaf { get; } = deaf;

        public Task<T> Task => this.tcs.Task;

        public bool Done => this.tcs.Task.IsCompleted;

        /// <summary>Whether its cancel had come when it was answered.</summary>
        public bool CancelledFirst { get; private set; }

        public bool Fail(Exception e)
        {
            this.CancelledFirst = this.Ct.IsCancellationRequested;
            return this.tcs.TrySetException(e);
        }

        public void Listen()
        {
            if (!this.Deaf)
            {
                this.Ct.Register(() => this.tcs.TrySetCanceled(this.Ct));
            }
        }

        protected bool Set(T value)
        {
            this.CancelledFirst = this.Ct.IsCancellationRequested;
            return this.tcs.TrySetResult(value);
        }
    }

    /// <summary>A TCP connect: accepted (the server's end is <see cref="Server"/>) or refused.</summary>
    internal sealed class Dial(MemNet net, string host, int port, CancellationToken ct, bool deaf, int index) : Pending<Stream>(ct, deaf)
    {
        public string Host { get; } = host;

        public int Port { get; } = port;

        public int Index { get; } = index;

        public MemEnd? Server { get; private set; }

        public MemEnd? Client { get; private set; }

        public bool Accept()
        {
            var (c, s) = MemEnd.Pair(net.Capacity, $"{this.Host}:{this.Port} #{this.Index}");
            this.Server = s;
            this.Client = c;
            if (!this.Set(c))
            {
                s.Abort();
                return false;
            }

            return true;
        }

        public override string ToString() =>
            $"connect #{this.Index} {this.Host}:{this.Port}{(this.Deaf ? " deaf" : string.Empty)}: "
            + $"{(this.Task.IsCompletedSuccessfully ? "connected" : this.Task.IsCanceled ? "cancelled" : this.Task.IsFaulted ? "refused" : "pending")}"
            + (this.Server is { } s ? (s.WasReset ? ", reset" : s.Inbound.Finished ? ", ended" : ", open") : string.Empty);
    }

    /// <summary>A TLS handshake (none: the stream it was given goes on).</summary>
    internal sealed class Handshake(Stream inner, string host, CancellationToken ct, bool deaf) : Pending<Stream>(ct, deaf)
    {
        public Stream Inner { get; } = inner;

        public string Host { get; } = host;

        /// <summary>Whether it would present a client certificate.</summary>
        public bool PresentsCertificate { get; init; }

        public bool Ok() => this.Set(this.Inner);
    }

    /// <summary>A name resolution.</summary>
    internal sealed class Lookup(string host, CancellationToken ct, bool deaf) : Pending<IReadOnlyList<string>>(ct, deaf)
    {
        public string Host { get; } = host;

        public bool Answer(params string[] addresses) => this.Set(addresses);
    }
}

/// <summary>A minimal HTTP/1.1 and WebSocket peer over a <see cref="MemEnd"/>, for the server's side of core mode's tests.</summary>
internal static class MemHttp
{
    /// <summary>The request line and headers (names as sent), or null at the end of the connection.</summary>
    public static async Task<(string Line, Dictionary<string, string> Headers)?> ReadHeadAsync(Stream s, CancellationToken ct = default)
    {
        var head = new List<byte>();
        var one = new byte[1];
        while (true)
        {
            int n;
            try
            {
                n = await s.ReadAsync(one, ct);
            }
            catch (IOException)
            {
                return null;
            }

            if (n == 0)
            {
                return null;
            }

            head.Add(one[0]);
            if (head.Count >= 4 && head[^1] == '\n' && head[^2] == '\r' && head[^3] == '\n' && head[^4] == '\r')
            {
                break;
            }
        }

        var lines = Encoding.ASCII.GetString(head.ToArray()).Split("\r\n", StringSplitOptions.RemoveEmptyEntries);
        var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        foreach (var line in lines.Skip(1))
        {
            var colon = line.IndexOf(':', StringComparison.Ordinal);
            headers[line[..colon].Trim()] = line[(colon + 1)..].Trim();
        }

        return (lines[0], headers);
    }

    /// <summary>The body, by its Content-Length.</summary>
    public static async Task<byte[]> ReadBodyAsync(Stream s, Dictionary<string, string> headers, CancellationToken ct = default)
    {
        var n = headers.TryGetValue("Content-Length", out var len) ? int.Parse(len, System.Globalization.CultureInfo.InvariantCulture) : 0;
        var body = new byte[n];
        await s.ReadExactlyAsync(body, ct);
        return body;
    }

    /// <summary>The WebSocket handshake's answer for a request with <paramref name="headers"/>.</summary>
    public static string Upgrade(Dictionary<string, string> headers)
    {
        var accept = Convert.ToBase64String(System.Security.Cryptography.SHA1.HashData(
            Encoding.ASCII.GetBytes(headers["Sec-WebSocket-Key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")));
        return $"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: {accept}\r\n\r\n";
    }
}

/// <summary>A file system's operations that a test holds, fails or watches (core mode's download files).</summary>
internal sealed class HeldFiles(IFileOps inner) : IFileOps
{
    private readonly ConcurrentQueue<string> log = new();
    private readonly ConcurrentQueue<string> onPool = new();

    /// <summary>While set and not signalled, every download write waits here.</summary>
    public ManualResetEventSlim? HoldWrites { get; set; }

    /// <summary>The next download write fails with this.</summary>
    public Exception? FailWrite { get; set; }

    public string[] Log => [.. this.log];

    /// <summary>The blocking operations (make, flush, delete) that ran on a thread of the pool.</summary>
    public string[] OnPool => [.. this.onPool];

    public IWritingFile CreateNew(string path, bool download)
    {
        this.Pooled("create");
        var f = inner.CreateNew(path, download);
        this.log.Enqueue($"create {Path.GetFileName(path)}");
        return new Held(this, f, Path.GetFileName(path));
    }

    public void Rename(string from, string to) => inner.Rename(from, to);

    public bool SyncDir(string dir) => inner.SyncDir(dir);

    public bool Remove(string path)
    {
        this.Pooled("remove");
        var removed = inner.Remove(path);
        this.log.Enqueue($"remove {Path.GetFileName(path)} {removed}");
        return removed;
    }

    public bool MakeDir(string dir)
    {
        this.Pooled("mkdir");
        return inner.MakeDir(dir);
    }

    public bool Clear(string dir) => inner.Clear(dir);

    private void Pooled(string op)
    {
        if (Thread.CurrentThread.IsThreadPoolThread)
        {
            this.onPool.Enqueue(op);
        }
    }

    private sealed class Held(HeldFiles owner, IWritingFile file, string name) : IWritingFile
    {
        public void Write(ReadOnlySpan<byte> data)
        {
            file.Write(data);
            owner.log.Enqueue($"write {name} {data.Length}");
        }

        public async ValueTask WriteAsync(ReadOnlyMemory<byte> data, CancellationToken ct)
        {
            owner.HoldWrites?.Wait(TimeSpan.FromSeconds(60));
            if (owner.FailWrite is { } e)
            {
                owner.FailWrite = null;
                throw e;
            }

            await file.WriteAsync(data, ct);
            owner.log.Enqueue($"write {name} {data.Length}");
        }

        public void Sync()
        {
            owner.Pooled("sync");
            file.Sync();
            owner.log.Enqueue($"sync {name}");
        }

        public void Dispose()
        {
            file.Dispose();
            owner.log.Enqueue($"close {name}");
        }
    }
}
