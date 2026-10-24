// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Net.WebSockets;
using System.Security.Cryptography;
using Google.Protobuf;
using Pydantic.OpAmp.Core;

namespace Pydantic.OpAmp.Client;

/// <summary>
/// Carries out the core's effects with HttpClient and ClientWebSocket and hands back what happened,
/// each result naming its effect. It decides nothing about OpAMP and keeps no time of its own: each
/// effect is registered <see cref="InFlight"/>, which aborts it on the core's cancel and decides
/// which results reach the core. <see cref="Run"/> is called on the client's loop; results come
/// back through <c>deliver</c> from any thread.
/// </summary>
internal sealed class Executor
{
    // Headers HttpClient sets itself (and puts on the content, not the request).
    private static readonly HashSet<string> ContentHeaders = new(StringComparer.OrdinalIgnoreCase)
    {
        "Content-Type", "Content-Length", "Content-Encoding",
    };

    private readonly IClientTransport transport;
    private readonly Action<Queued> deliver;
    private readonly string? stateFile;
    private readonly TimeProvider time;
    private readonly IFileOps files;

    // This client's own: download files are named by it, so no other client's work touches them.
    private readonly string instance = Guid.NewGuid().ToString("N")[..12];

    // The core's URLs are sent as they are (see RequestUri).
    private static readonly UriCreationOptions RawPathAndQuery = new() { DangerousDisablePathAndQueryCanonicalization = true };

    // Connections open or closing, for the sends and the close asked of them.
    private readonly ConcurrentDictionary<ulong, Conn> conns = new();

    private string? used;

    /// <summary>
    /// Carry out effects over <paramref name="transport"/>: one handler for requests, WebSocket
    /// upgrades and downloads, so the application's TLS and proxy settings apply to all of them.
    /// <paramref name="time"/> (default: the system's) dates responses and bounds stop. Saves and
    /// downloads go through <paramref name="files"/> (the real file system if none).
    /// </summary>
    public Executor(
        IClientTransport transport, Action<Queued> deliver, string? stateFile = null, TimeProvider? time = null, IFileOps? files = null)
    {
        this.transport = transport;
        this.deliver = deliver;
        this.stateFile = stateFile;
        this.time = time ?? TimeProvider.System;
        this.InFlight = new InFlight(this.time);
        this.files = files ?? FileOps.Real;
    }

    /// <summary>The effects in flight.</summary>
    public InFlight InFlight { get; }

    /// <summary>
    /// The URL of the OpAMP request or connection whose outcome came last (null: none yet). Set
    /// before the outcome reaches the core: a notification of it finds it in place.
    /// </summary>
    internal string? Used => Volatile.Read(ref this.used);

    /// <summary>The file operations saves and downloads go through (journaled with the core tap on).</summary>
    internal IFileOps Files => this.files;

    /// <summary>The state file, if any: downloads go beside it.</summary>
    internal string? StatePath => this.stateFile;

    public void Run(Effect effect)
    {
        switch (effect.KindCase)
        {
            case Effect.KindOneofCase.HttpRequest:
                this.Post(effect.HttpRequest);
                break;
            case Effect.KindOneofCase.WsOpen:
                this.Open(effect.WsOpen);
                break;
            case Effect.KindOneofCase.WsSend:
                if (this.conns.TryGetValue(effect.WsSend.Id, out var c))
                {
                    c.Send(effect.WsSend.Data.ToByteArray()); // else it is gone, and said so
                }

                break;
            case Effect.KindOneofCase.WsClose:
                if (this.conns.TryGetValue(effect.WsClose.Id_, out var closing))
                {
                    closing.Close();
                }

                break;
            case Effect.KindOneofCase.Store:
                this.Store(effect.Store);
                break;
            case Effect.KindOneofCase.Download:
                this.Download(effect.Download);
                break;
            case Effect.KindOneofCase.Discard:
                if (this.stateFile is not null)
                {
                    this.TryDelete(this.DownloadPath(effect.Discard.Id_));
                }

                break;
            case Effect.KindOneofCase.Cancel:
                this.InFlight.Cancel(effect.Cancel.Id_);
                break;
            case Effect.KindOneofCase.Notify:
                break; // notifications are the client's
            default:
                // A newer module than this client: fail loudly rather than skip what the core asked for.
                throw new InvalidOperationException("the core asked for an effect this client doesn't know: the client is older than its core");
        }
    }

    /// <summary>
    /// Stop: everything still in flight is cancelled, and the task ends when its work has finished
    /// (quickly, cancelled), or at <paramref name="bound"/>.
    /// </summary>
    public Task ReleaseAsync(TimeSpan bound) => this.InFlight.CloseAsync(bound);

    /// <summary>Where package files are downloaded, beside the state file.</summary>
    internal static string DownloadsDir(string stateFile) => stateFile + ".downloads";

    /// <summary>Delete what was downloaded (before a restart).</summary>
    internal static void DeleteDownloads(string stateFile, IFileOps? files = null)
    {
        try
        {
            (files ?? FileOps.Real).Clear(DownloadsDir(stateFile));
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            // what is left is never used: each download's file is made new, under a name of its own
        }
    }

    /// <summary>
    /// Where a redirect points: <paramref name="location"/> resolved against the URL requested by
    /// .NET's URL library (<c>new Uri(base, relative)</c>), and that URL's origin (port 0 when it is
    /// the scheme's default or absent). A Location that doesn't resolve goes as given, with no
    /// origin: the core refuses it.
    /// </summary>
    internal static (string Location, Origin? Origin) Resolve(Uri requested, string location)
    {
        Uri url;
        try
        {
            url = new Uri(requested, location);
        }
        catch (UriFormatException)
        {
            return (location, null);
        }

        return (url.AbsoluteUri, new Origin
        {
            Scheme = url.Scheme,
            Host = url.Host,
            Port = url.IsDefaultPort || url.Port < 0 ? 0u : (uint)url.Port,
        });
    }

    /// <summary>
    /// A URL of the core's (a request's, a download's, a connection's), as the client sends it:
    /// its path and query exactly as the core gave them. .NET's Uri would otherwise rewrite them,
    /// decoding escapes of unreserved characters (<c>%41</c> sent as <c>A</c>) and upper-casing
    /// the others' hex, so the server would get another request target than every other client's
    /// (tests/shell-tables/urls.json). The core has checked the URL: only RFC 3986 characters,
    /// escapes complete, no dot segments, so there is nothing for .NET to fix but an empty path,
    /// which means <c>/</c>.
    /// </summary>
    internal static Uri RequestUri(string url)
    {
        var authority = url.IndexOf("://", StringComparison.Ordinal) is var at and >= 0 ? at + 3 : 0;
        var end = url.IndexOfAny(['/', '?'], authority);
        if (end < 0)
        {
            url += "/";
        }
        else if (url[end] == '?')
        {
            url = url.Insert(end, "/");
        }

        return new Uri(url, RawPathAndQuery);
    }

    /// <summary>The file of download <paramref name="id"/>: this client's own.</summary>
    internal string DownloadPath(ulong id) =>
        Path.Combine(DownloadsDir(this.stateFile!), $"{this.instance}-{id.ToString(System.Globalization.CultureInfo.InvariantCulture)}");

    /// <summary>The message and its causes' (a TLS failure is "the SSL connection could not be established", then why).</summary>
    internal static string Detail(Exception e)
    {
        var detail = e.Message;
        for (var inner = e.InnerException; inner is not null; inner = inner.InnerException)
        {
            detail += ": " + inner.Message;
        }

        return detail;
    }

    private ulong UnixMs() => (ulong)this.time.GetUtcNow().ToUnixTimeMilliseconds();

    private static IEnumerable<Header> FromHeaders(IEnumerable<KeyValuePair<string, IEnumerable<string>>> headers) =>
        headers.SelectMany(h => h.Value.Select(v => new Header { Name = h.Key, Value = v }));

    private void TryDelete(string path)
    {
        try
        {
            this.files.Remove(path);
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            // it goes at the next start
        }
    }

    /// <summary>A result of effect <paramref name="id"/>; <paramref name="final"/> if the effect is over.</summary>
    private void Report(ulong id, Input input, bool final = true) => this.deliver(new Queued(input, id, final));

    /// <summary>
    /// Save beside the loop, through the state file's one ordered writer: a slow disk holds up
    /// nothing. A cancel stops the waiting: a save already writing finishes whole or not at all,
    /// unreported.
    /// </summary>
    private void Store(Core.Store store)
    {
        if (this.InFlight.Register(store.Id) is null)
        {
            return;
        }

        if (this.stateFile is null)
        {
            this.Report(store.Id, new Input { StoreFailed = new Failed { Id = store.Id, Detail = "no state file" } });
            return;
        }

        // A save cancelled before its turn is left out; one already writing finishes. Answered in
        // the writer, so in the order the saves land.
        this.InFlight.Track(StateFile.SaveBeside(
            this.stateFile,
            store.Data.ToByteArray(),
            () => this.InFlight.Has(store.Id),
            e => this.Report(store.Id, e is null
                ? new Input { Stored = new Id { Id_ = store.Id } }
                : new Input { StoreFailed = new Failed { Id = store.Id, Detail = e.Message } }),
            this.files));
    }

    private void Download(Download d)
    {
        if (this.InFlight.Register(d.Id) is not { } ct)
        {
            return;
        }

        if (this.stateFile is null)
        {
            this.Report(d.Id, new Input { DownloadFailed = new Failed { Id = d.Id, Detail = "no state file" } });
            return;
        }

        var file = new DownloadFile(this.DownloadPath(d.Id));
        var work = StateFile.Begin(this.stateFile); // the next client on the state file waits for it
        var task = this.DownloadAsync(d, file, ct, work);
        this.InFlight.Track(task);
        // Cancelled, the file goes, written in full or in part, once the download lets go of it.
        this.InFlight.Adopt(d.Id, () => task.ContinueWith(_ => this.Drop(file), CancellationToken.None, TaskContinuationOptions.LongRunning, TaskScheduler.Default));
    }

    /// <summary>Delete a download's file, if it made one: never one it found there.</summary>
    private void Drop(DownloadFile file)
    {
        if (file.Made)
        {
            this.TryDelete(file.Path);
        }
    }

    /// <summary>
    /// GET a package file into its path whatever the status, hashing it as it goes, following no
    /// redirect: the status goes to the core, with a redirect's Location. On failure nothing is left.
    /// </summary>
    private async Task DownloadAsync(Download d, DownloadFile file, CancellationToken ct, IDisposable work)
    {
        using (work)
        {
            await Task.Yield(); // registered before it runs
            Input result;
            try
            {
                result = new Input { DownloadResponse = await this.FetchAsync(d, file, ct).ConfigureAwait(false) };
            }
            catch (Exception e)
            {
                // Whatever went wrong, the download ends with an input, and nothing is left.
                await FileOps.OffPool(() => this.Drop(file)).ConfigureAwait(false);
                result = new Input { DownloadFailed = new Failed { Id = d.Id, Detail = Detail(e) } };
            }

            if (ct.IsCancellationRequested)
            {
                await FileOps.OffPool(() => this.Drop(file)).ConfigureAwait(false); // cancelled: whatever it was, it goes
            }

            this.Report(d.Id, result);
        }
    }

    /// <summary>
    /// The GET, into a file made new (never one already there: one left by a run that crashed is
    /// neither written over nor handed over), flushed to the disk before the response is answered.
    /// </summary>
    private async Task<DownloadResponse> FetchAsync(Download d, DownloadFile into, CancellationToken ct)
    {
        var url = RequestUri(d.Url);
        using var req = new HttpRequestMessage(HttpMethod.Get, url);
        foreach (var h in d.Headers)
        {
            req.Headers.TryAddWithoutValidation(h.Name, h.Value);
        }

        // The server's client certificate only for a file at its own origin (the core says which).
        using var resp = await this.transport.DownloadAsync(req, d.ServerOrigin, ct).ConfigureAwait(false);
        var status = (int)resp.StatusCode;
        var response = new DownloadResponse { Id = d.Id, Status = (uint)status };
        if (status is >= 300 and < 400 && resp.Headers.TryGetValues("Location", out var locations) && locations.FirstOrDefault() is { } given)
        {
            var (location, origin) = Resolve(url, given);
            response.Location = location;
            response.LocationOrigin = origin;
        }

        using var sha = IncrementalHash.CreateHash(HashAlgorithmName.SHA256);
        await using (var body = await resp.Content.ReadAsStreamAsync(ct).ConfigureAwait(false))
        {
            using var file = await FileOps.OffPool(() =>
            {
                this.files.MakeDir(Path.GetDirectoryName(into.Path)!);
                return this.files.CreateNew(into.Path, download: true);
            }).ConfigureAwait(false);
            into.Made = true;
            var buf = new byte[64 * 1024];
            ulong size = 0;
            int n;
            while ((n = await body.ReadAsync(buf, ct).ConfigureAwait(false)) > 0)
            {
                size += (ulong)n;
                if (size > d.MaxBytes)
                {
                    throw new IOException($"file over {d.MaxBytes} bytes");
                }

                sha.AppendData(buf, 0, n);
                await file.WriteAsync(buf.AsMemory(0, n), ct).ConfigureAwait(false);
            }

            await FileOps.OffPool(file.Sync).ConfigureAwait(false);
        }

        response.Sha256 = ByteString.CopyFrom(sha.GetHashAndReset());
        return response;
    }

    private void Post(HttpRequest r)
    {
        // The core's own timeout for the request, handed to HttpClient as the Rust client hands it
        // to reqwest (within what a CancellationTokenSource takes); a cancel ends it sooner.
        var timeout = TimeSpan.FromMilliseconds(Math.Min(r.TimeoutMs, int.MaxValue));
        if (this.InFlight.Register(r.Id, timeout) is { } ct)
        {
            this.InFlight.Track(this.PostAsync(r, ct));
        }
    }

    private async Task PostAsync(HttpRequest r, CancellationToken ct)
    {
        await Task.Yield();
        Input Failed(string detail) => new() { HttpFailed = new Failed { Id = r.Id, Detail = detail } };

        var limit = (long)Math.Min(r.MaxResponseBytes, long.MaxValue);
        Input? result = null;
        try
        {
            using var req = new HttpRequestMessage(HttpMethod.Post, RequestUri(r.Url))
            {
                Content = new ByteArrayContent(r.Body.ToByteArray()),
            };
            foreach (var h in r.Headers)
            {
                if (ContentHeaders.Contains(h.Name))
                {
                    req.Content.Headers.TryAddWithoutValidation(h.Name, h.Value);
                }
                else
                {
                    req.Headers.TryAddWithoutValidation(h.Name, h.Value);
                }
            }

            using var resp = await this.transport.SendAsync(req, ct).ConfigureAwait(false);
            using var stream = await resp.Content.ReadAsStreamAsync(ct).ConfigureAwait(false);
            using var body = new MemoryStream();
            var buf = new byte[81920];
            int n;
            while ((n = await stream.ReadAsync(buf, ct).ConfigureAwait(false)) > 0)
            {
                if (body.Length + n > limit)
                {
                    result = Failed($"response over {limit} bytes");
                    break;
                }

                body.Write(buf, 0, n);
            }

            if (result is null)
            {
                var input = new HttpResponse
                {
                    Id = r.Id,
                    Status = (uint)resp.StatusCode,
                    Body = ByteString.CopyFrom(body.ToArray()),
                    UnixMs = this.UnixMs(),
                };
                input.Headers.AddRange(FromHeaders(resp.Headers));
                input.Headers.AddRange(FromHeaders(resp.Content.Headers));
                result = new Input { HttpResponse = input };
            }
        }
        catch (Exception e)
        {
            // Whatever went wrong, the request ends with an input.
            result = Failed(Detail(e));
        }

        Volatile.Write(ref this.used, r.Url);
        this.Report(r.Id, result);
    }

    private void Open(WsOpen o)
    {
        if (this.InFlight.Register(o.Id) is { } ct)
        {
            this.InFlight.Track(this.OpenAsync(o, ct));
        }
    }

    /// <summary>
    /// Open the connection, with no time limit of its own (the core's cancel ends it), then read it.
    /// A cancel aborts the opening, and a connection that opens anyway is closed at once.
    /// </summary>
    private async Task OpenAsync(WsOpen o, CancellationToken ct)
    {
        await Task.Yield();
        WebSocket ws;
        try
        {
            ws = await this.transport.ConnectAsync(RequestUri(o.Url), o.Headers.Select(h => KeyValuePair.Create(h.Name, h.Value)), ct)
                .ConfigureAwait(false);
        }
        catch (Exception e)
        {
            // Whatever went wrong, the connecting ends with an input.
            var answered = e as WebSocketConnectException;
            var failed = new WsFailed { Id = o.Id, Detail = Detail(answered?.InnerException ?? e), UnixMs = this.UnixMs() };
            if (answered is not null)
            {
                failed.Status = (uint)answered.Status;
                if (answered.Headers is { } hs)
                {
                    failed.Headers.AddRange(FromHeaders(hs));
                }
            }

            Volatile.Write(ref this.used, o.Url);
            this.Report(o.Id, new Input { WsFailed = failed });
            return;
        }

        using var owned = ws;
        var conn = new Conn(this, o.Id, ws, (long)Math.Min(o.MaxMessageBytes, long.MaxValue));
        if (!this.InFlight.Adopt(o.Id, conn.Drop))
        {
            ws.Abort(); // cancelled, or stopped, while it opened: closed at once
            return;
        }

        this.conns[o.Id] = conn;
        Volatile.Write(ref this.used, o.Url);
        this.Report(o.Id, new Input { WsOpened = new Id { Id_ = o.Id } }, final: false);
        await conn.ReadAsync().ConfigureAwait(false);
        await conn.Ended.ConfigureAwait(false);
        this.conns.TryRemove(o.Id, out _);
    }

    /// <summary>
    /// One WebSocket connection: its reader, and the sends and close asked of it, in order. It
    /// reports the server's Close frame; after ours (or the answer to the server's), once the
    /// closing handshake is done, the connection closed, or lost if that failed; nothing after that,
    /// or after it is lost or dropped. Our Close is sent alone (CloseOutputAsync), and the one
    /// reader receives the server's: .NET's CloseAsync would receive too, beside the reader, and
    /// whichever of the two didn't get the server's Close would find the connection gone (lost,
    /// where the handshake was in fact done).
    /// </summary>
    private sealed class Conn
    {
        private const int Open = 0;
        private const int ServerClosed = 1;
        private const int Closing = 2;
        private const int Over = 3;

        private readonly Executor x;
        private readonly ulong id;
        private readonly WebSocket ws;
        private readonly long maxMessageBytes;
        private readonly object order = new();
        private readonly TaskCompletionSource ended = new(TaskCreationOptions.RunContinuationsAsynchronously);
        private readonly TaskCompletionSource closeSent = new(TaskCreationOptions.RunContinuationsAsynchronously);
        private Task last = Task.CompletedTask;
        private int state;

        public Conn(Executor x, ulong id, WebSocket ws, long maxMessageBytes)
        {
            this.x = x;
            this.id = id;
            this.ws = ws;
            this.maxMessageBytes = maxMessageBytes;
        }

        /// <summary>Done with: lost, closed after our Close, or dropped.</summary>
        public Task Ended => this.ended.Task;

        /// <summary>Report the connection's messages until the server's Close, or it fails.</summary>
        public async Task ReadAsync()
        {
            var buf = new byte[65536];
            using var message = new MemoryStream();
            try
            {
                while (true)
                {
                    var r = await this.ws.ReceiveAsync(buf, CancellationToken.None).ConfigureAwait(false);
                    if (r.MessageType == WebSocketMessageType.Close)
                    {
                        var code = (uint)(r.CloseStatus ?? (WebSocketCloseStatus)1005);
                        if (Interlocked.CompareExchange(ref this.state, ServerClosed, Open) == Open)
                        {
                            // The core answers it (a ws_close effect), which ends the connection.
                            this.x.Report(this.id, new Input { WsClosed = new WsClosed { Id = this.id, Code = code } }, final: false);
                            return;
                        }

                        // Closing: the answer to ours, or a Close that crossed it. Once ours is
                        // out, the closing handshake is done.
                        await this.closeSent.Task.ConfigureAwait(false);
                        if (Interlocked.CompareExchange(ref this.state, Over, Closing) == Closing)
                        {
                            this.x.Report(this.id, new Input { WsClosed = new WsClosed { Id = this.id, Code = code } });
                            this.ended.TrySetResult();
                        }

                        return;
                    }

                    if (message.Length + r.Count > this.maxMessageBytes)
                    {
                        // Too big: drop the connection (the spec's 1009, as a lost connection).
                        this.Fail($"message over {this.maxMessageBytes} bytes");
                        return;
                    }

                    message.Write(buf, 0, r.Count);
                    if (r.EndOfMessage)
                    {
                        if (r.MessageType == WebSocketMessageType.Binary && Volatile.Read(ref this.state) == Open)
                        {
                            var data = ByteString.CopyFrom(message.ToArray());
                            this.x.Report(this.id, new Input { WsMessage = new WsMessage { Id = this.id, Data = data } }, final: false);
                        }

                        message.SetLength(0); // text messages mean nothing to OpAMP
                    }
                }
            }
            catch (Exception e)
            {
                // Whatever the receive threw, the connection is lost, and said so.
                this.Fail(e.Message);
            }
        }

        public void Send(byte[] data) => this.Then(async () =>
        {
            try
            {
                await this.ws.SendAsync(data, WebSocketMessageType.Binary, true, CancellationToken.None).ConfigureAwait(false);
            }
            catch (Exception e)
            {
                // Whatever the send threw, the connection is lost, and said so.
                this.Fail(e.Message);
            }
        });

        /// <summary>
        /// Our Close frame (or the answer to the server's), after anything sent before it. If the
        /// server's Close came first, that is the closing handshake done: closed. Otherwise the
        /// reader hears the server's answer (closed), or the connection go (lost). No time limit
        /// of its own: the core cancels a handshake that takes too long.
        /// </summary>
        public void Close()
        {
            var was = Volatile.Read(ref this.state);
            if (was is not (Open or ServerClosed) || Interlocked.CompareExchange(ref this.state, Closing, was) != was)
            {
                return; // lost already, and said so
            }

            this.Then(async () =>
            {
                Input? result = null;
                try
                {
                    await this.ws.CloseOutputAsync(WebSocketCloseStatus.NormalClosure, string.Empty, CancellationToken.None)
                        .ConfigureAwait(false);
                    if (was == ServerClosed)
                    {
                        var code = (uint)(this.ws.CloseStatus ?? (WebSocketCloseStatus)1005);
                        result = new Input { WsClosed = new WsClosed { Id = this.id, Code = code } };
                    }
                }
                catch (Exception e)
                {
                    // Whatever the close threw, the connection is lost, and goes: its reader too.
                    result = new Input { WsLost = new Failed { Id = this.id, Detail = e.Message } };
                    this.ws.Abort();
                }
                finally
                {
                    this.closeSent.TrySetResult();
                }

                if (result is not null && Interlocked.CompareExchange(ref this.state, Over, Closing) == Closing)
                {
                    this.x.Report(this.id, result);
                    this.ended.TrySetResult();
                }
            });
        }

        /// <summary>Drop the connection now, saying nothing: closed without waiting.</summary>
        public void Drop()
        {
            Volatile.Write(ref this.state, Over);
            this.ws.Abort();
            this.ended.TrySetResult();
        }

        /// <summary>The connection failed, open or closing: lost, unless it is over.</summary>
        private void Fail(string detail)
        {
            var was = Volatile.Read(ref this.state);
            if (was is (Open or ServerClosed or Closing) && Interlocked.CompareExchange(ref this.state, Over, was) == was)
            {
                this.x.Report(this.id, new Input { WsLost = new Failed { Id = this.id, Detail = detail } });
                this.ended.TrySetResult();
            }

            this.ws.Abort();
        }

        /// <summary>Run <paramref name="work"/> after everything asked of the connection before it.</summary>
        private void Then(Func<Task> work)
        {
            lock (this.order)
            {
                this.last = this.last.ContinueWith(_ => work(), TaskScheduler.Default).Unwrap();
            }
        }
    }

    /// <summary>A download's file: where, and whether the download made it (so it is its to delete).</summary>
    private sealed class DownloadFile(string path)
    {
        public string Path { get; } = path;

        public bool Made { get; set; }
    }
}
