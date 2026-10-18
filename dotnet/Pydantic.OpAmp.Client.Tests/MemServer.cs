// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Net.Sockets;
using System.Net.WebSockets;
using System.Text;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// The server at the far end of core mode's in-memory streams (<see cref="MemNet"/>), for the
/// schedule tests: a minimal HTTP/1.1 and WebSocket peer that turns each request, download and
/// upgrade the core sends into a <see cref="Call"/> of the <see cref="MemoryTransport"/>, which the
/// test answers as it answers platform mode's: a response (its body held, or failing half-way), a
/// connection (<see cref="MemSocket"/>: the server's pushes, its Close, a receive, send or close
/// that fails or never ends), or a failure (the connection reset). A call's cancel is the client
/// ending or resetting the stream it came on; a deaf call ignores it, and what it hands over late
/// goes to a dead stream (and is released). Connects are accepted at once, refused, or (deaf) held
/// until their cancel and then accepted: a connection the client must reset at once, untold.
/// </summary>
internal sealed class MemServer
{
    private readonly MemoryTransport transport;
    private readonly List<MemNet.Dial> late = new();

    /// <param name="refuse">Whether the n-th connect is refused.</param>
    /// <param name="deaf">Whether the n-th connect ignores its cancel, and is accepted after it.</param>
    public MemServer(MemoryTransport transport, MemNet net, Func<int, bool> refuse, Func<int, bool> deaf)
    {
        this.transport = transport;
        net.Deaf = (kind, n) => kind == "connect" && deaf(n);
        net.OnHandshake = h =>
        {
            transport.Touch();
            h.Ok(); // no TLS in memory: the stream goes on as it is
        };
        net.OnDial = d =>
        {
            transport.Touch();
            if (refuse(d.Index))
            {
                d.Fail(new SocketException((int)SocketError.ConnectionRefused));
            }
            else if (deaf(d.Index))
            {
                lock (this.late)
                {
                    this.late.Add(d);
                }

                d.Ct.Register(() => Task.Run(() =>
                {
                    if (d.Accept())
                    {
                        transport.Touch();
                        _ = this.ServeAsync(d.Server!);
                    }
                }));
            }
            else if (d.Accept())
            {
                _ = this.ServeAsync(d.Server!);
            }
        };
    }

    /// <summary>Connects held until their cancel, then accepted: each must be reset by the client.</summary>
    public MemNet.Dial[] Late
    {
        get
        {
            lock (this.late)
            {
                return [.. this.late];
            }
        }
    }

    /// <summary>A WebSocket frame, server to client (unmasked).</summary>
    private static byte[] Frame(byte op, ReadOnlySpan<byte> payload)
    {
        var head = new List<byte> { (byte)(0x80 | op) };
        if (payload.Length < 126)
        {
            head.Add((byte)payload.Length);
        }
        else if (payload.Length <= ushort.MaxValue)
        {
            head.Add(126);
            head.Add((byte)(payload.Length >> 8));
            head.Add((byte)payload.Length);
        }
        else
        {
            head.Add(127);
            for (var i = 7; i >= 0; i--)
            {
                head.Add((byte)((long)payload.Length >> (8 * i)));
            }
        }

        return [.. head, .. payload];
    }

    private async Task ServeAsync(MemEnd s)
    {
        var peer = new Peer(s);
        try
        {
            while (true)
            {
                if (await peer.ReadHeadAsync() is not { } head)
                {
                    return;
                }

                var (line, headers) = head;
                var parts = line.Split(' ');
                var body = headers.TryGetValue("Content-Length", out var len) ? await peer.ReadExactAsync(int.Parse(len, System.Globalization.CultureInfo.InvariantCulture)) : [];
                if (body is null)
                {
                    return;
                }

                var upgrade = headers.TryGetValue("Upgrade", out var u) && u.Equals("websocket", StringComparison.OrdinalIgnoreCase);
                var kind = upgrade ? CallKind.Connect : parts[0] == "POST" ? CallKind.Post : CallKind.Get;
                var url = new Uri($"{(upgrade ? "ws" : "http")}://{headers.GetValueOrDefault("Host", "opamp.test")}{parts[1]}");

                // The call's cancel: the client ending or resetting the stream it came on.
                var gone = new CancellationTokenSource();
                peer.Watch(gone);
                Call call;
                try
                {
                    call = this.transport.Serve(kind, url, body, gone.Token);
                }
                catch (Exception)
                {
                    s.Abort(); // the test's answer threw as the call was made: a failure
                    return;
                }

                object answer;
                try
                {
                    answer = await call.Headers.Task;
                }
                catch (WebSocketConnectException refused)
                {
                    await peer.WriteAsync(Encoding.ASCII.GetBytes($"HTTP/1.1 {refused.Status} Refused\r\nContent-Length: 0\r\n\r\n"));
                    continue;
                }

                if (answer is MemSocket socket)
                {
                    await this.WebSocketAsync(peer, s, socket, headers);
                    return;
                }

                using var resp = (HttpResponseMessage)answer;
                var mem = (MemBody)await resp.Content.ReadAsStreamAsync();
                var extra = new StringBuilder();
                if (resp.Headers.TryGetValues("Retry-After", out var retry))
                {
                    extra.Append($"Retry-After: {retry.First()}\r\n");
                }

                await peer.WriteAsync(Encoding.ASCII.GetBytes(
                    $"HTTP/1.1 {(int)resp.StatusCode} Answer\r\nContent-Type: application/x-protobuf\r\nContent-Length: {mem.Size}\r\n{extra}\r\n"));
                var buf = new byte[16 << 10];
                int n;
                while ((n = await mem.ReadAsync(buf, gone.Token)) > 0)
                {
                    await peer.WriteAsync(buf.AsMemory(0, n));
                }
            }
        }
        catch (Exception)
        {
            // A failure the test asked for, a body that failed or was given up on, or the client
            // gone: the connection goes.
            s.Abort();
        }
    }

    /// <summary>The server's side of an open WebSocket: the test's <paramref name="socket"/> on one side, the client's frames on the other.</summary>
    private async Task WebSocketAsync(Peer peer, MemEnd s, MemSocket socket, Dictionary<string, string> headers)
    {
        var gate = new object();
        bool clientClosed = false, serverClosed = false;
        using var writes = new SemaphoreSlim(1);

        async Task Send(byte op, byte[] payload)
        {
            await writes.WaitAsync();
            try
            {
                await peer.WriteAsync(Frame(op, payload));
            }
            finally
            {
                writes.Release();
            }
        }

        void Fin()
        {
            lock (gate)
            {
                if (!(clientClosed && serverClosed))
                {
                    return;
                }
            }

            s.ShutdownSend(); // both Closes said: the server ends the connection
        }

        void Reset()
        {
            s.Abort();
            socket.Abort();
        }

        try
        {
            await peer.WriteAsync(Encoding.ASCII.GetBytes(MemHttp.Upgrade(headers)));
        }
        catch (Exception)
        {
            Reset(); // the client went first: the connection handed over late is released
            return;
        }

        // The server's frames: the test's pushes, its Close (or the answer to the client's).
        var pushes = Task.Run(async () =>
        {
            var buf = new byte[1 << 20];
            try
            {
                while (true)
                {
                    var r = await socket.ReceiveAsync(buf, CancellationToken.None);
                    if (r.MessageType == WebSocketMessageType.Close)
                    {
                        var code = (ushort)(r.CloseStatus ?? WebSocketCloseStatus.NormalClosure);
                        await Send(0x8, [(byte)(code >> 8), (byte)code]);
                        lock (gate)
                        {
                            serverClosed = true;
                        }

                        Fin();
                        return;
                    }

                    await Send(0x2, buf.AsSpan(0, r.Count).ToArray());
                }
            }
            catch (Exception)
            {
                Reset(); // the receive broke (as the test asked), or the client went
            }
        });

        // The client's frames.
        var message = new MemoryStream();
        var both = false;
        try
        {
            while (await peer.ReadFrameAsync() is { } f)
            {
                switch (f.Op)
                {
                    case 0x0 or 0x1 or 0x2:
                        message.Write(f.Payload);
                        if (f.Fin)
                        {
                            await socket.SendAsync(message.ToArray(), WebSocketMessageType.Binary, true, CancellationToken.None);
                            message.SetLength(0);
                        }

                        break;
                    case 0x9:
                        await Send(0xA, f.Payload);
                        break;
                    case 0x8:
                        lock (gate)
                        {
                            clientClosed = true;
                        }

                        var code = f.Payload.Length >= 2 ? (WebSocketCloseStatus)(f.Payload[0] << 8 | f.Payload[1]) : WebSocketCloseStatus.Empty;

                        // Answered by the test's socket (its Close comes back through the pushes),
                        // failing, or never: the client's frames are read on meanwhile, to its end.
                        _ = Task.Run(async () =>
                        {
                            try
                            {
                                await socket.CloseOutputAsync(code, null, CancellationToken.None);
                                Fin();
                            }
                            catch (Exception)
                            {
                                Reset();
                            }
                        });
                        break;
                }
            }

            lock (gate)
            {
                both = clientClosed && serverClosed;
            }
        }
        catch (Exception)
        {
            // a send the test broke, or the client gone
        }

        if (!both)
        {
            Reset();
        }
        else
        {
            socket.Abort(); // the connection is over: a close the test still holds goes with it
        }

        await pushes;
    }

    /// <summary>The server's reading of one connection, with the one byte a watch may have read ahead.</summary>
    private sealed class Peer(MemEnd s)
    {
        private readonly byte[] one = new byte[1];
        private Task<int>? ahead;

        /// <summary>Read ahead one byte: if the client ends or resets the stream instead, cancel <paramref name="gone"/>.</summary>
        public void Watch(CancellationTokenSource gone)
        {
            var read = s.ReadAsync(this.one, 0, 1);
            this.ahead = read;
            _ = read.ContinueWith(
                t =>
                {
                    if (!t.IsCompletedSuccessfully || t.Result == 0)
                    {
                        try
                        {
                            gone.Cancel();
                        }
                        catch (ObjectDisposedException)
                        {
                        }
                    }
                },
                TaskScheduler.Default);
        }

        public Task WriteAsync(ReadOnlyMemory<byte> data) => s.WriteAsync(data).AsTask();

        public async Task<(string Line, Dictionary<string, string> Headers)?> ReadHeadAsync()
        {
            var head = new List<byte>();
            while (true)
            {
                if (await this.ReadByteAsync() is not { } b)
                {
                    return null;
                }

                head.Add(b);
                if (head.Count >= 4 && head[^1] == '\n' && head[^2] == '\r' && head[^3] == '\n' && head[^4] == '\r')
                {
                    break;
                }
            }

            var lines = Encoding.ASCII.GetString(head.ToArray()).Split("\r\n", StringSplitOptions.RemoveEmptyEntries);
            var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
            foreach (var l in lines.Skip(1))
            {
                var colon = l.IndexOf(':', StringComparison.Ordinal);
                headers[l[..colon].Trim()] = l[(colon + 1)..].Trim();
            }

            return (lines[0], headers);
        }

        /// <summary><paramref name="n"/> bytes, or null if the stream ends first.</summary>
        public async Task<byte[]?> ReadExactAsync(int n)
        {
            var buf = new byte[n];
            var at = 0;
            if (n > 0 && this.ahead is not null)
            {
                if (await this.ReadByteAsync() is not { } b)
                {
                    return null;
                }

                buf[at++] = b;
            }

            while (at < n)
            {
                var got = await s.ReadAsync(buf.AsMemory(at));
                if (got == 0)
                {
                    return null;
                }

                at += got;
            }

            return buf;
        }

        public async Task<(byte Op, bool Fin, byte[] Payload)?> ReadFrameAsync()
        {
            if (await this.ReadExactAsync(2) is not { } h)
            {
                return null;
            }

            var len = (long)(h[1] & 0x7F);
            if (len == 126)
            {
                var x = await this.ReadExactAsync(2) ?? throw new IOException("ended in a frame");
                len = x[0] << 8 | x[1];
            }
            else if (len == 127)
            {
                var x = await this.ReadExactAsync(8) ?? throw new IOException("ended in a frame");
                len = x.Aggregate(0L, (a, b) => a << 8 | b);
            }

            var mask = (h[1] & 0x80) != 0 ? await this.ReadExactAsync(4) ?? throw new IOException("ended in a frame") : null;
            var payload = await this.ReadExactAsync((int)len) ?? throw new IOException("ended in a frame");
            if (mask is not null)
            {
                for (var i = 0; i < payload.Length; i++)
                {
                    payload[i] ^= mask[i % 4];
                }
            }

            return ((byte)(h[0] & 0x0F), (h[0] & 0x80) != 0, payload);
        }

        private async Task<byte?> ReadByteAsync()
        {
            if (this.ahead is { } read)
            {
                this.ahead = null;
                return await read == 0 ? null : this.one[0];
            }

            var b = new byte[1];
            return await s.ReadAsync(b) == 0 ? null : b[0];
        }
    }
}
