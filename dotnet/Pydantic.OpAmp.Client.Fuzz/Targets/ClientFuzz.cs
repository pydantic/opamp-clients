// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Diagnostics;
using System.Net;
using System.Net.WebSockets;
using System.Text;
using System.Threading.Channels;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Client.Tests;
using Pydantic.OpAmp.Core;

namespace Pydantic.OpAmp.Client.Fuzz;

/// <summary>
/// SharpFuzz target: the .NET client's own code around the core, fed by a server whose every
/// answer is the fuzzer's and an application whose callback calls the client as the fuzzer says
/// (as python/tests/fuzz/fuzz_client.py does for Python and ClientFuzz.java for Java). One input is
/// one run of a real client on the native core (through <see cref="Native"/>'s buffers), in either
/// mode:
/// <list type="bullet">
/// <item>.NET mode: the executor's HttpClient path over a transport of the fuzzer's (bodies in
/// pieces, resets, error statuses with any headers, package downloads into the state file's
/// directory; WebSocket upgrades taken or refused, messages in pieces, closes and drops);</item>
/// <item>Rust mode: the core's own HTTP and WebSocket over <see cref="Streams"/>, on
/// <see cref="MemNet"/>, the server's bytes arriving in pieces (or any bytes at all, status line
/// included), package files spliced or written by the streams' file writer.</item>
/// </list>
/// The server answers each request with any bytes, a ServerToAgent built from the bytes (remote
/// configs, packages with files it serves, connection settings, commands, custom messages, errors,
/// a new instance UID) and perhaps damaged, an error status, or a reset. Each notification makes
/// the callback call the client (statuses, remote config and package answers, custom messages,
/// stop). Then the application stops the client. What must hold, whatever came: the client never
/// stops on its own (the callback never throws, so an error is this shell's or the core's);
/// callbacks one at a time, never after Stopped; Stopped once, last; stop completes within its
/// bound without an error; every answer the server gave reaches the core unless the core gave up
/// on it first (.NET mode); InstallPackage names a file under the state file's downloads
/// directory, there as the callback runs, whose SHA-256 is the offer's content hash when it has
/// one; a heartbeat interval is never negative; no temporary state file is left; nothing is
/// started once stop has returned.
/// <para>scripts/dotnet_fuzz.sh runs it under SharpFuzz; the tests' FuzzInputsTests run
/// <see cref="Seeds"/> and every input under crashes/ClientFuzz/ with every <c>dotnet test</c>.</para>
/// </summary>
internal static class ClientFuzz
{
    public const int StopMs = 2000;
    public const int MarginMs = 3000;

    /// <summary>How long the client may ask for nothing before the application stops it (it is backing off).</summary>
    public const int IdleMs = 60;

    public const int RunMs = 5000;

    /// <summary>.NET mode: the header that names each answer the server gave.</summary>
    public const string Tag = "X-Answer";

    /// <summary>Inputs that reach the deeper paths at once (ClientFuzz.java's, read alike).</summary>
    public static readonly byte[][] Seeds =
    [
        [0],
        [1, 0],

        // .NET mode, 1 ms heartbeats: a remote config, answered from the callback.
        [0, 0, 5, 2, 0, 2, (byte)'a', (byte)'b', 1, 0, 4, (byte)'c', (byte)'o', (byte)'n', (byte)'f', 0, 0, 1, 3, 1],

        // Rust mode: a package offered, its file served.
        [1, 0, 1, 5, 4, 0, 0, 1, 0, 0, 1, (byte)'1', 2, (byte)'h', (byte)'1', 1, 0, 5, (byte)'h', (byte)'e', (byte)'l', (byte)'l', (byte)'o', 0, 0, 0, 0, 0, 0, 1, 4, 1, 1, 0],

        // Connection settings offered, with a new endpoint.
        [0x24, 0, 6, 8, 0, 2, (byte)'c', (byte)'s', 1, 1, 1, 0, 0, 0, 0],

        // Raw bytes, a reset, a damaged message, an error status with Retry-After.
        [4, 0, 1, 5, 0x0a, 3, (byte)'a', (byte)'b', (byte)'c', 2, 9, 0xff, 3, 3, 7, 1, 1],
        [5, 0, 1, 4, 16, .. "HTTP/1.1 200 OK\r\n"u8, 1, 3, 0, 0xff],

        // A WebSocket endpoint, in each mode: the upgrade taken, a remote config over it, then refused.
        [0, 2, 0, 5, 2, 0, 2, (byte)'a', (byte)'b', 1, 0, 4, (byte)'c', (byte)'o', (byte)'n', (byte)'f', 0, 0, 1, 3, 1, 1, 6],
        [1, 2, 0, 5, 2, 0, 2, (byte)'a', (byte)'b', 1, 0, 4, (byte)'c', (byte)'o', (byte)'n', (byte)'f', 0, 0, 1, 3, 1, 1, 6],
    ];

    public static void Run(ReadOnlySpan<byte> data)
    {
        var tmp = Directory.CreateTempSubdirectory("opamp-fuzz-").FullName;
        try
        {
            new FuzzRun(data.ToArray(), tmp).Go();
        }
        finally
        {
            try
            {
                Directory.Delete(tmp, recursive: true);
            }
            catch (IOException)
            {
                // A save still writing, from a client that broke its promise: reported already.
            }
        }
    }

    /// <summary>A property broken: the fuzzer's crash.</summary>
    internal sealed class Problem(string message) : Exception(message);

    private sealed class FuzzRun
    {
        private readonly FuzzInput.Reader r;
        private readonly FuzzInput.Server server;
        private readonly bool rust;
        private readonly string state;
        private readonly OpAmpClientOptions options;
        private readonly MemNet net = new();
        private readonly FuzzTransport? transport;
        private readonly List<OpAmpNotification> notes = new();
        private readonly Dictionary<Type, OpAmpNotification> seen = new();
        private readonly ConcurrentQueue<string> problems = new();
        private readonly TaskCompletionSource<OpAmpClient> client = new(TaskCreationOptions.RunContinuationsAsynchronously);
        private readonly object stepGate = new();
        private readonly Dictionary<string, List<ulong>> posts = new();
        private readonly List<ulong> opens = new();
        private readonly HashSet<ulong> cancelled = new();
        private readonly HashSet<string> reached = new();
        private int active;
        private Task? stopped;
        private long lastAsk = Stopwatch.GetTimestamp();
        private volatile bool sealedOff;

        public FuzzRun(byte[] data, string tmp)
        {
            this.r = new FuzzInput.Reader(data);
            var flags = this.r.U8();
            this.rust = (flags & 1) != 0;
            var more = this.r.U8();
            this.server = new FuzzInput.Server(this.r);
            this.state = Path.Combine(tmp, "state");
            this.options = new OpAmpClientOptions
            {
                Endpoint = new Uri((more & 2) != 0 ? "ws://opamp.test/v1/opamp" : "http://opamp.test/v1/opamp"),
                InstanceUid = (flags & 2) != 0 ? new byte[16] : null,
                Capabilities = FuzzInput.Capabilities,
                HeartbeatInterval = TimeSpan.FromMilliseconds(new[] { 1, 5, 30_000, 0 }[(flags >> 2) & 3]),
                RequestTimeout = TimeSpan.FromMilliseconds((flags & 16) != 0 ? 1000 : 0),
                StateFile = this.state,
                AcceptPackages = true,
                AcceptConnectionSettings = (flags & 32) != 0,
                RequestInstanceUid = (flags & 64) != 0,
                DownloadTimeout = TimeSpan.FromSeconds(5),
                MaxPackageBytes = (flags & 128) != 0 ? 1000UL : 0UL,
                StopTimeout = TimeSpan.FromMilliseconds(StopMs),
                Mode = this.rust ? OpAmpMode.Rust : OpAmpMode.DotNet,
                DirectConnection = this.rust,
                RustSplice = !this.rust || (more & 1) != 0,
            };
            if (this.rust)
            {
                this.net.OnHandshake = h => h.Ok(); // no TLS in memory
                this.net.OnLookup = l => l.Answer("127.0.0.1");
                this.net.OnDial = d =>
                {
                    this.Asked();
                    if (this.sealedOff)
                    {
                        this.Problem($"a connection to {d.Host} made after stop returned");
                    }

                    if (d.Accept())
                    {
                        var s = d.Server!;
                        var host = d.Host;
                        _ = Task.Run(() => this.ServeConnAsync(s, host));
                    }
                };
            }
            else
            {
                this.transport = new FuzzTransport(this);
            }
        }

        public void Go()
        {
            var c = OpAmpClient.Start(
                this.options,
                new OpAmpStatus { Description = new AgentDescription(), CustomCapabilities = new CustomCapabilities { Capabilities = { FuzzInput.Capability } } },
                this.Notify,
                new Seams { Transport = this.transport, Net = this.rust ? this.net : null, Environment = _ => null, Stepped = this.Stepped });
            this.client.SetResult(c);
            var begun = Stopwatch.GetTimestamp();
            while (!this.r.Empty && Volatile.Read(ref this.stopped) is null && Stopwatch.GetElapsedTime(begun).TotalMilliseconds < RunMs)
            {
                Thread.Sleep(5);
                if (Stopwatch.GetElapsedTime(Interlocked.Read(ref this.lastAsk)).TotalMilliseconds > IdleMs)
                {
                    break;
                }
            }

            var asked = Stopwatch.GetTimestamp();
            Interlocked.CompareExchange(ref this.stopped, c.StopAsync(), null);
            var stop = Volatile.Read(ref this.stopped)!;
            try
            {
                if (!stop.Wait(StopMs + MarginMs))
                {
                    this.Problem($"stop took over {StopMs + MarginMs} ms");
                }
            }
            catch (AggregateException e)
            {
                this.Problem($"stop completed with {e.InnerException}");
            }

            this.sealedOff = true;
            var took = Stopwatch.GetElapsedTime(asked).TotalMilliseconds;
            if (took > StopMs + MarginMs)
            {
                this.Problem($"stop took {took:F0} ms");
            }

            OpAmpNotification[] told;
            lock (this.notes)
            {
                told = [.. this.notes];
            }

            var stops = told.Count(n => n is OpAmpNotification.Stopped);
            if (stops != 1 || told[^1] is not OpAmpNotification.Stopped)
            {
                this.Problem($"stopped told {stops} times, last {(told.Length == 0 ? "nothing" : told[^1])}");
            }

            if (File.Exists(this.state + ".tmp"))
            {
                this.Problem("a temporary state file left behind");
            }

            if (!this.rust)
            {
                this.CheckAnswers();
            }

            Thread.Sleep(1); // what a client broke its promise with comes now, if at all
            if (!this.problems.IsEmpty)
            {
                throw new Problem($"{(this.rust ? "Rust" : ".NET")} mode, {this.server.Answers} answers: {string.Join("; ", this.problems)}");
            }
        }

        private void Problem(string what) => this.problems.Enqueue(what);

        private void Asked() => Interlocked.Exchange(ref this.lastAsk, Stopwatch.GetTimestamp());

        private void Notify(OpAmpNotification n)
        {
            if (Interlocked.Increment(ref this.active) > 1)
            {
                this.Problem($"two callbacks at once: {n}");
            }

            try
            {
                lock (this.notes)
                {
                    if (this.notes.Any(m => m is OpAmpNotification.Stopped))
                    {
                        this.Problem($"a callback after stopped: {n}");
                    }

                    this.notes.Add(n);
                }

                this.seen[n.GetType()] = n;
                switch (n)
                {
                    case OpAmpNotification.Stopped { Error: { } e }:
                        this.Problem($"the client stopped on its own: {e}");
                        break;
                    case OpAmpNotification.HeartbeatInterval h when h.Interval < TimeSpan.Zero:
                        this.Problem($"heartbeat interval {h.Interval}");
                        break;
                    case OpAmpNotification.InstallPackage ip:
                        this.CheckPackage(ip);
                        break;
                }

                if (n is OpAmpNotification.Stopped)
                {
                    return;
                }

                var c = this.client.Task.GetAwaiter().GetResult();
                for (var k = this.r.U8() % 4; k > 0; k--)
                {
                    if (FuzzInput.Call(this.r, c, this.r.Pick(FuzzInput.Calls), this.seen))
                    {
                        Interlocked.CompareExchange(ref this.stopped, c.StopAsync(), null);
                    }
                }
            }
            finally
            {
                Interlocked.Decrement(ref this.active);
            }
        }

        private void CheckPackage(OpAmpNotification.InstallPackage ip)
        {
            var dir = Executor.DownloadsDir(this.state);
            if (string.IsNullOrEmpty(ip.Path) || Path.GetDirectoryName(ip.Path) != dir)
            {
                this.Problem($"InstallPackage {ip.Name}: {ip.Path} is not in {dir}");
                return;
            }

            byte[] got;
            try
            {
                got = File.ReadAllBytes(ip.Path);
            }
            catch (Exception e) when (e is IOException or UnauthorizedAccessException)
            {
                this.Problem($"InstallPackage {ip.Name}: its file can't be read: {e.Message}");
                return;
            }

            var want = ip.Package.File?.ContentHash.ToByteArray() ?? [];
            if (want.Length > 0 && !FuzzInput.Sha256(got).AsSpan().SequenceEqual(want))
            {
                this.Problem($"InstallPackage {ip.Name}: the file isn't the one offered ({got.Length} bytes)");
            }
        }

        /// <summary>Each step, as the loop takes it: the effects it issued and the answers it took.</summary>
        private void Stepped(Queued q, Output output)
        {
            lock (this.stepGate)
            {
                var i = q.Input;
                var hs = i.KindCase switch
                {
                    Input.KindOneofCase.HttpResponse => i.HttpResponse.Headers,
                    Input.KindOneofCase.WsFailed => i.WsFailed.Headers,
                    _ => null,
                };
                foreach (var h in hs ?? [])
                {
                    if (string.Equals(h.Name, Tag, StringComparison.OrdinalIgnoreCase))
                    {
                        this.reached.Add(h.Value);
                    }
                }

                foreach (var e in output.Effects)
                {
                    switch (e.KindCase)
                    {
                        case Effect.KindOneofCase.HttpRequest:
                            {
                                var key = Convert.ToHexString(e.HttpRequest.Body.Span);
                                if (!this.posts.TryGetValue(key, out var ids))
                                {
                                    this.posts[key] = ids = new List<ulong>();
                                }

                                ids.Add(e.HttpRequest.Id);
                                break;
                            }

                        case Effect.KindOneofCase.WsOpen:
                            this.opens.Add(e.WsOpen.Id);
                            break;
                        case Effect.KindOneofCase.Cancel:
                            this.cancelled.Add(e.Cancel.Id_);
                            break;
                    }
                }
            }
        }

        /// <summary>
        /// .NET mode: every answer the server gave reached the core, unless the core gave up on its
        /// effect first. A request is the http_request with its body; the k-th upgrade the shell
        /// asked for is the core's k-th ws_open (checked only when the shell asked for each).
        /// </summary>
        private void CheckAnswers()
        {
            lock (this.stepGate)
            {
                var lost = new List<string>();
                foreach (var (tag, body) in this.transport!.PostTags)
                {
                    var ids = this.posts.GetValueOrDefault(Convert.ToHexString(body)) ?? [];
                    if (!this.reached.Contains(tag) && ids.Count > 0 && !ids.Any(this.cancelled.Contains))
                    {
                        lost.Add($"request [{string.Join(", ", ids)}]");
                    }
                }

                var openTags = this.transport.OpenTags.ToArray();
                for (var i = 0; i < openTags.Length && openTags.Length == this.opens.Count; i++)
                {
                    if (openTags[i].Length > 0 && !this.reached.Contains(openTags[i]) && !this.cancelled.Contains(this.opens[i]))
                    {
                        lost.Add($"upgrade {this.opens[i]}");
                    }
                }

                if (lost.Count > 0)
                {
                    this.Problem($"answers the server gave never reached the core: {string.Join(", ", lost)}");
                }
            }
        }

        private static byte[] Head(int status, IEnumerable<(string Name, string Value)> headers, int length)
        {
            var s = new StringBuilder($"HTTP/1.1 {status} X\r\nContent-Length: {length}\r\n");
            foreach (var (name, value) in headers)
            {
                s.Append(name).Append(": ").Append(value).Append("\r\n");
            }

            return Encoding.Latin1.GetBytes(s.Append("\r\n").ToString());
        }

        private static byte[] Frame(int op, byte[] payload)
        {
            var f = new List<byte> { (byte)(0x80 | op) };
            var n = payload.Length;
            if (n < 126)
            {
                f.Add((byte)n);
            }
            else if (n < 65536)
            {
                f.AddRange([126, (byte)(n >> 8), (byte)n]);
            }
            else
            {
                f.Add(127);
                for (var i = 7; i >= 0; i--)
                {
                    f.Add(i >= 4 ? (byte)0 : (byte)(n >> (8 * i)));
                }
            }

            f.AddRange(payload);
            return [.. f];
        }

        /// <summary>Rust mode: one connection's server, reading requests and writing answers, in pieces.</summary>
        private async Task ServeConnAsync(MemEnd end, string host)
        {
            try
            {
                while (true)
                {
                    var head = await MemHttp.ReadHeadAsync(end);
                    if (head is not { } h)
                    {
                        return;
                    }

                    var body = await MemHttp.ReadBodyAsync(end, h.Headers);
                    this.Asked();
                    if (h.Headers.TryGetValue("Upgrade", out var up) && up.Equals("websocket", StringComparison.OrdinalIgnoreCase))
                    {
                        var u = this.server.Upgrade();
                        if (u.How == "reset")
                        {
                            end.Abort();
                            return;
                        }

                        if (u.How == "status")
                        {
                            await end.WriteAsync(Head(u.Status, u.Headers, 0));
                            continue;
                        }

                        await end.WriteAsync(MemHttp.Upgrade(h.Headers));
                        await this.FramesAsync(end);
                        return;
                    }

                    var first = h.Line.Split(' ');
                    var a = h.Line.StartsWith("POST", StringComparison.Ordinal) ? this.server.Respond(body)
                        : this.server.Download($"http://{host}{(first.Length > 1 ? first[1] : "/")}");
                    if (a.How == "reset")
                    {
                        end.Abort();
                        return;
                    }

                    byte[] raw;
                    var cuts = new List<int>();
                    if (a.How == "garbage")
                    {
                        raw = a.Data;
                    }
                    else
                    {
                        var top = Head(a.Status, a.Headers, a.Data.Length);
                        raw = [.. top, .. a.Data];
                        cuts.AddRange(a.Cuts.Select(at => at + top.Length));
                    }

                    foreach (var p in FuzzInput.Pieces(raw, cuts))
                    {
                        await end.WriteAsync(p);
                        await Task.Yield();
                    }

                    if (a.How == "garbage")
                    {
                        end.ShutdownSend();
                        return;
                    }
                }
            }
            catch (Exception e) when (e is IOException or ObjectDisposedException or OperationCanceledException or KeyNotFoundException or FormatException or IndexOutOfRangeException or ArgumentException)
            {
                end.Abort();
            }
        }

        /// <summary>Rust mode: an upgraded connection's frames from the client (masked), each answered.</summary>
        private async Task FramesAsync(MemEnd end)
        {
            var message = new List<byte>();
            while (true)
            {
                var two = await end.ReadExactAsync(2);
                long n = two[1] & 0x7f;
                if (n == 126)
                {
                    var b = await end.ReadExactAsync(2);
                    n = (b[0] << 8) | b[1];
                }
                else if (n == 127)
                {
                    var b = await end.ReadExactAsync(8);
                    n = 0;
                    foreach (var x in b)
                    {
                        n = (n << 8) | x;
                    }
                }

                var mask = (two[1] & 0x80) != 0 ? await end.ReadExactAsync(4) : new byte[4];
                if (n < 0 || n > 1 << 24)
                {
                    end.Abort();
                    return;
                }

                var payload = n == 0 ? [] : await end.ReadExactAsync((int)n);
                for (var i = 0; i < payload.Length; i++)
                {
                    payload[i] ^= mask[i % 4];
                }

                var op = two[0] & 0x0f;
                if (op == 8)
                {
                    // The client's Close: answered, and the connection ended.
                    await end.WriteAsync(Frame(8, payload[..Math.Min(2, payload.Length)]));
                    end.ShutdownSend();
                    return;
                }

                if (op == 9)
                {
                    await end.WriteAsync(Frame(10, payload));
                    continue;
                }

                if (op is not (0 or 2))
                {
                    continue;
                }

                message.AddRange(payload);
                if ((two[0] & 0x80) == 0)
                {
                    continue;
                }

                this.Asked();
                var a = this.server.WsAnswer([.. message]);
                message.Clear();
                switch (a.How)
                {
                    case "drop":
                        end.Abort();
                        return;
                    case "close":
                        await end.WriteAsync(Frame(8, a.Code == 0 ? [] : [(byte)(a.Code >> 8), (byte)a.Code]));
                        break;
                    case "raw":
                        await end.WriteAsync(a.Data); // any bytes: frames or not
                        break;
                    default:
                        await end.WriteAsync(Frame(2, a.Data));
                        break;
                }
            }
        }

        /// <summary>.NET mode: what the executor's requests, downloads and upgrades reach: the fuzzer's server.</summary>
        private sealed class FuzzTransport(FuzzRun run) : IClientTransport
        {
            private int tags;

            /// <summary>The tag of each OpAMP request's answer, with the body it answered.</summary>
            public ConcurrentDictionary<string, byte[]> PostTags { get; } = new();

            /// <summary>Each upgrade's answer's tag, in the order the shell asked (empty: none to tag).</summary>
            public ConcurrentQueue<string> OpenTags { get; } = new();

            public Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken ct)
            {
                run.Asked();
                if (run.sealedOff)
                {
                    run.Problem($"a request to {request.RequestUri} made after stop returned");
                }

                var post = request.Method == HttpMethod.Post;
                var body = request.Content is { } content ? content.ReadAsByteArrayAsync(CancellationToken.None).GetAwaiter().GetResult() : [];
                var a = post ? run.server.Respond(body) : run.server.Download(request.RequestUri!.AbsoluteUri);
                if (a.How != "ok")
                {
                    return Task.FromException<HttpResponseMessage>(new HttpRequestException("connection reset by the server"));
                }

                var resp = new HttpResponseMessage((HttpStatusCode)a.Status)
                {
                    Content = new StreamContent(new PieceStream(FuzzInput.Pieces(a.Data, a.Cuts), ct)),
                    RequestMessage = request,
                };
                var headers = a.Headers.ToList();
                if (post)
                {
                    var tag = Interlocked.Increment(ref this.tags).ToString(System.Globalization.CultureInfo.InvariantCulture);
                    this.PostTags[tag] = body;
                    headers.Add((Tag, tag));
                }

                foreach (var (name, value) in headers)
                {
                    if (!resp.Headers.TryAddWithoutValidation(name, value))
                    {
                        resp.Content.Headers.TryAddWithoutValidation(name, value);
                    }
                }

                return Task.FromResult(resp);
            }

            public Task<WebSocket> ConnectAsync(Uri url, IEnumerable<KeyValuePair<string, string>> headers, CancellationToken ct)
            {
                run.Asked();
                if (run.sealedOff)
                {
                    run.Problem($"a connection to {url} made after stop returned");
                }

                var u = run.server.Upgrade();
                switch (u.How)
                {
                    case "reset":
                        this.OpenTags.Enqueue(string.Empty);
                        return Task.FromException<WebSocket>(new WebSocketException(WebSocketError.ConnectionClosedPrematurely, "connection reset by the server"));
                    case "status":
                        {
                            var tag = Interlocked.Increment(ref this.tags).ToString(System.Globalization.CultureInfo.InvariantCulture);
                            this.OpenTags.Enqueue(tag);
                            var hs = u.Headers.Append((Name: Tag, Value: tag)).GroupBy(h => h.Name, StringComparer.OrdinalIgnoreCase)
                                .ToDictionary(g => g.Key, g => g.Select(h => h.Value), StringComparer.OrdinalIgnoreCase);
                            return Task.FromException<WebSocket>(new WebSocketConnectException(
                                new WebSocketException(WebSocketError.NotAWebSocket, $"The server returned status code '{u.Status}' when status code '101' was expected."), u.Status, hs));
                        }

                    default:
                        this.OpenTags.Enqueue(string.Empty);
                        return Task.FromResult<WebSocket>(new FuzzSocket(run));
                }
            }

            public void Dispose()
            {
            }
        }

        /// <summary>A response body, read in the pieces it arrives in.</summary>
        private sealed class PieceStream(List<byte[]> pieces, CancellationToken ct) : Stream
        {
            private int piece;
            private int at;

            public override bool CanRead => true;

            public override bool CanSeek => false;

            public override bool CanWrite => false;

            public override long Length => throw new NotSupportedException();

            public override long Position { get => throw new NotSupportedException(); set => throw new NotSupportedException(); }

            public override int Read(byte[] buffer, int offset, int count)
            {
                ct.ThrowIfCancellationRequested();
                while (this.piece < pieces.Count && this.at == pieces[this.piece].Length)
                {
                    this.piece++;
                    this.at = 0;
                }

                if (this.piece == pieces.Count || count == 0)
                {
                    return 0;
                }

                var n = Math.Min(count, pieces[this.piece].Length - this.at);
                Array.Copy(pieces[this.piece], this.at, buffer, offset, n);
                this.at += n;
                return n;
            }

            public override async ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken cancellationToken = default)
            {
                await Task.Yield();
                var b = new byte[buffer.Length];
                var n = this.Read(b, 0, b.Length);
                b.AsMemory(0, n).CopyTo(buffer);
                return n;
            }

            public override Task<int> ReadAsync(byte[] buffer, int offset, int count, CancellationToken cancellationToken) =>
                this.ReadAsync(buffer.AsMemory(offset, count), cancellationToken).AsTask();

            public override void Flush()
            {
            }

            public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();

            public override void SetLength(long value) => throw new NotSupportedException();

            public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();
        }

        /// <summary>
        /// .NET mode: a WebSocket connection whose server answers each message the client sends, as
        /// the fuzzer says; a message longer than the reader's buffer arrives in parts.
        /// </summary>
        private sealed class FuzzSocket(FuzzRun run) : WebSocket
        {
            private readonly Channel<Frame> frames = Channel.CreateUnbounded<Frame>();
            private readonly object gate = new();
            private readonly List<byte> sending = new();
            private WebSocketState state = WebSocketState.Open;
            private WebSocketCloseStatus? closeStatus;
            private byte[]? rest;

            public override WebSocketCloseStatus? CloseStatus => this.closeStatus;

            public override string? CloseStatusDescription => null;

            public override WebSocketState State => this.state;

            public override string? SubProtocol => null;

            public override void Abort() => this.End(WebSocketState.Aborted);

            public override Task CloseAsync(WebSocketCloseStatus closeStatus, string? statusDescription, CancellationToken cancellationToken) =>
                this.CloseOutputAsync(closeStatus, statusDescription, cancellationToken);

            public override Task CloseOutputAsync(WebSocketCloseStatus closeStatus, string? statusDescription, CancellationToken cancellationToken)
            {
                lock (this.gate)
                {
                    if (this.state is not (WebSocketState.Open or WebSocketState.CloseReceived))
                    {
                        throw new WebSocketException(WebSocketError.InvalidState, "not open");
                    }

                    this.state = this.state == WebSocketState.CloseReceived ? WebSocketState.Closed : WebSocketState.CloseSent;
                }

                this.frames.Writer.TryWrite(new Frame(null, closeStatus, null)); // the server's answer
                return Task.CompletedTask;
            }

            public override void Dispose() => this.End(WebSocketState.Closed);

            public override async Task<WebSocketReceiveResult> ReceiveAsync(ArraySegment<byte> buffer, CancellationToken cancellationToken)
            {
                var data = Interlocked.Exchange(ref this.rest, null);
                if (data is null)
                {
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
                            this.state = this.state == WebSocketState.CloseSent ? WebSocketState.Closed : WebSocketState.CloseReceived;
                        }

                        return new WebSocketReceiveResult(0, WebSocketMessageType.Close, true, status, null);
                    }

                    data = f.Data!;
                }

                var n = Math.Min(data.Length, buffer.Count);
                data.AsSpan(0, n).CopyTo(buffer.AsSpan());
                if (n < data.Length)
                {
                    this.rest = data[n..];
                }

                return new WebSocketReceiveResult(n, WebSocketMessageType.Binary, n == data.Length);
            }

            public override Task SendAsync(ArraySegment<byte> buffer, WebSocketMessageType messageType, bool endOfMessage, CancellationToken cancellationToken)
            {
                byte[] message;
                lock (this.gate)
                {
                    if (this.state is not (WebSocketState.Open or WebSocketState.CloseReceived))
                    {
                        throw new WebSocketException(WebSocketError.InvalidState, "not open");
                    }

                    this.sending.AddRange(buffer);
                    if (!endOfMessage)
                    {
                        return Task.CompletedTask;
                    }

                    message = [.. this.sending];
                    this.sending.Clear();
                }

                run.Asked();
                var a = run.server.WsAnswer(message);
                switch (a.How)
                {
                    case "drop":
                        this.frames.Writer.TryWrite(new Frame(null, null, new WebSocketException(WebSocketError.ConnectionClosedPrematurely, "reset by the server")));
                        break;
                    case "close":
                        this.frames.Writer.TryWrite(new Frame(null, (WebSocketCloseStatus)(a.Code == 0 ? 1005 : a.Code), null));
                        break;
                    default:
                        this.frames.Writer.TryWrite(new Frame(a.Data, null, null));
                        break;
                }

                return Task.CompletedTask;
            }

            private void End(WebSocketState to)
            {
                lock (this.gate)
                {
                    if (this.state is not (WebSocketState.Closed or WebSocketState.Aborted))
                    {
                        this.state = to;
                    }
                }

                this.frames.Writer.TryWrite(new Frame(null, null, new WebSocketException(WebSocketError.InvalidState, "aborted")));
            }

            private sealed record Frame(byte[]? Data, WebSocketCloseStatus? Close, Exception? Fault);
        }
    }
}
