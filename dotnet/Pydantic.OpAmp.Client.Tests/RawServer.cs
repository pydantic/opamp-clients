// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Collections.Concurrent;
using System.Net;
using System.Net.Sockets;
using System.Net.WebSockets;
using System.Security.Cryptography;
using System.Text;
using Google.Protobuf;
using OpAmp.Proto.V1;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// A server that does, for each connection, what a test scripts: answer, hold an answer, never
/// answer. It sees each connection end.
/// </summary>
internal sealed class RawServer : IDisposable
{
    private readonly TcpListener listener = new(IPAddress.Loopback, 0);
    private readonly Func<RawConn, Task> serve;

    public RawServer(Func<RawConn, Task> serve)
    {
        this.serve = serve;
        this.listener.Start();
        _ = Task.Run(this.AcceptAsync);
    }

    public int Port => ((IPEndPoint)this.listener.LocalEndpoint).Port;

    /// <summary>Every connection accepted, in order.</summary>
    public ConcurrentQueue<RawConn> Conns { get; } = new();

    public string Url(string scheme, string path = "/v1/opamp") => $"{scheme}://127.0.0.1:{this.Port}{path}";

    public void Dispose()
    {
        this.listener.Stop();
        foreach (var c in this.Conns)
        {
            c.Tcp.Dispose();
        }
    }

    private async Task AcceptAsync()
    {
        while (true)
        {
            TcpClient tcp;
            try
            {
                tcp = await this.listener.AcceptTcpClientAsync();
            }
            catch (Exception e) when (e is SocketException or ObjectDisposedException)
            {
                return;
            }

            var conn = new RawConn(tcp);
            this.Conns.Enqueue(conn);
            _ = Task.Run(async () =>
            {
                try
                {
                    await this.serve(conn);
                }
                catch (Exception e) when (e is IOException or WebSocketException or ObjectDisposedException or SocketException)
                {
                    // the client went
                }
            });
        }
    }
}

/// <summary>One connection to a <see cref="RawServer"/>.</summary>
internal sealed class RawConn(TcpClient tcp)
{
    private readonly TaskCompletionSource<(string Line, Dictionary<string, string> Headers)> head =
        new(TaskCreationOptions.RunContinuationsAsynchronously);

    public TcpClient Tcp { get; } = tcp;

    public Stream Stream { get; } = tcp.GetStream();

    /// <summary>The first request's head, once read.</summary>
    public Task<(string Line, Dictionary<string, string> Headers)> Head => this.head.Task;

    /// <summary>Read a request's head (the first is also <see cref="Head"/>).</summary>
    public async Task<(string Line, Dictionary<string, string> Headers)> ReadHeadAsync()
    {
        var h = await Http1.ReadHeadAsync(this.Stream) ?? throw new IOException("the client went");
        this.head.TrySetResult(h);
        return h;
    }

    /// <summary>Answer an upgrade: the server's end of the WebSocket.</summary>
    public async Task<WebSocket> AcceptAsync(Dictionary<string, string> headers)
    {
        var accept = Convert.ToBase64String(SHA1.HashData(Encoding.ASCII.GetBytes(
            headers["Sec-WebSocket-Key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")));
        await Http1.WriteAsync(this.Stream, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
            + $"Sec-WebSocket-Accept: {accept}\r\n\r\n");
        return WebSocket.CreateFromStream(this.Stream, new WebSocketCreationOptions { IsServer = true, KeepAliveInterval = TimeSpan.Zero });
    }

    /// <summary>
    /// Whether the client ends the connection within <paramref name="within"/>, reading (and
    /// dropping) whatever it sends until then.
    /// </summary>
    public async Task<bool> EndsWithinAsync(TimeSpan within)
    {
        using var cts = new CancellationTokenSource(within);
        var buf = new byte[4096];
        try
        {
            while (await this.Stream.ReadAsync(buf, cts.Token) > 0)
            {
            }

            return true;
        }
        catch (OperationCanceledException)
        {
            return false;
        }
        catch (Exception e) when (e is IOException or ObjectDisposedException or SocketException)
        {
            return true;
        }
    }

    /// <summary>Read a POST's body (after its head).</summary>
    public async Task<byte[]> ReadBodyAsync(Dictionary<string, string> headers)
    {
        var body = new byte[int.Parse(headers["Content-Length"], System.Globalization.CultureInfo.InvariantCulture)];
        await this.Stream.ReadExactlyAsync(body);
        return body;
    }

    /// <summary>Answer an OpAMP POST with <paramref name="reply"/> (by default, one naming the agent).</summary>
    public async Task<AgentToServer> AnswerAsync(Dictionary<string, string> headers, Func<AgentToServer, ServerToAgent>? reply = null)
    {
        var msg = AgentToServer.Parser.ParseFrom(await this.ReadBodyAsync(headers));
        var bytes = (reply?.Invoke(msg) ?? new ServerToAgent { InstanceUid = msg.InstanceUid }).ToByteArray();
        await Http1.WriteAsync(this.Stream, $"HTTP/1.1 200 OK\r\nContent-Type: application/x-protobuf\r\nContent-Length: {bytes.Length}\r\n\r\n", bytes);
        return msg;
    }
}

/// <summary>Waiting in tests.</summary>
internal static class Wait
{
    /// <summary>Wait until <paramref name="done"/>, for at most <paramref name="seconds"/>: whether it came.</summary>
    public static async Task<bool> ForAsync(Func<bool> done, double seconds = 10)
    {
        var until = DateTime.UtcNow.AddSeconds(seconds);
        while (!done())
        {
            if (DateTime.UtcNow > until)
            {
                return false;
            }

            await Task.Delay(10);
        }

        return true;
    }
}

/// <summary>
/// An executor with a loop of its own, as the client's: results go through
/// <see cref="InFlight.Take"/>, and only what passes would reach the core.
/// </summary>
internal sealed class TestLoop
{
    private readonly BlockingCollection<Queued> raw = new();

    public TestLoop(ClientTransport transport, string? state = null, IFileOps? files = null)
    {
        this.State = state;
        this.X = new Executor(transport, this.raw.Add, state, files: files);
    }

    public Executor X { get; }

    public string? State { get; }

    /// <summary>A new loop over a direct transport, with a state file in a new directory.</summary>
    public static TestLoop New(IFileOps? files = null)
    {
        var dir = Directory.CreateTempSubdirectory();
        return new TestLoop(ClientTransport.Create(null, null, direct: true), Path.Combine(dir.FullName, "state"), files);
    }

    public void Run(Pydantic.OpAmp.Core.Effect effect) => this.X.Run(effect);

    /// <summary>The next input that reaches the core, within <paramref name="within"/>.</summary>
    public bool TryTake(out Pydantic.OpAmp.Core.Input input, TimeSpan within)
    {
        var until = DateTime.UtcNow + within;
        while (true)
        {
            var left = until - DateTime.UtcNow;
            if (left < TimeSpan.Zero || !this.raw.TryTake(out var q, left))
            {
                input = null!;
                return false;
            }

            if (this.X.InFlight.Take(q))
            {
                input = q.Input;
                return true;
            }
        }
    }

    /// <summary>The next input that reaches the core (failing after 10 s).</summary>
    public Pydantic.OpAmp.Core.Input Next()
    {
        Xunit.Assert.True(this.TryTake(out var input, TimeSpan.FromSeconds(10)), "no input came");
        return input;
    }

    /// <summary>Everything queued now that would reach the core.</summary>
    public List<Pydantic.OpAmp.Core.Input> Drain()
    {
        var all = new List<Pydantic.OpAmp.Core.Input>();
        while (this.raw.TryTake(out var q))
        {
            if (this.X.InFlight.Take(q))
            {
                all.Add(q.Input);
            }
        }

        return all;
    }

    /// <summary>How many results are queued, wanted or not.</summary>
    public int Queued => this.raw.Count;
}
