// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Net.WebSockets;
using Google.Protobuf;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// Effect.cancel, carried out at once for each kind of effect, and nothing more about it reaching
/// the core; and a WebSocket the core closes reported closed once the handshake is done.
/// covers: RULE-CLIENT-15
/// </summary>
public class CancelTests
{
    private static readonly TimeSpan Quiet = TimeSpan.FromMilliseconds(500);

    [Fact]
    public async Task ACancelledRequestIsAbortedAndNeverReported()
    {
        // The server takes the request and never answers.
        using var server = new RawServer(async c => await c.ReadHeadAsync());
        var loop = TestLoop.New();
        loop.Run(new Effect { HttpRequest = new HttpRequest { Id = 1, Url = server.Url("http"), TimeoutMs = 60_000, MaxResponseBytes = 1 << 20 } });
        Assert.True(await Wait.ForAsync(() => server.Conns.TryPeek(out var c) && c.Head.IsCompleted));

        loop.Run(Cancel(1));

        Assert.True(await server.Conns.First().EndsWithinAsync(TimeSpan.FromSeconds(5)), "the request was not aborted");
        await Task.Delay(Quiet);
        Assert.Empty(loop.Drain());
        Assert.False(loop.X.InFlight.Has(1));
    }

    [Fact]
    public async Task ACancelledConnectingIsAbortedAndNeverReported()
    {
        // The server reads the upgrade and never answers it.
        using var server = new RawServer(async c => await c.ReadHeadAsync());
        var loop = TestLoop.New();
        loop.Run(new Effect { WsOpen = new WsOpen { Id = 2, Url = server.Url("ws"), MaxMessageBytes = 1 << 20 } });
        Assert.True(await Wait.ForAsync(() => server.Conns.TryPeek(out var c) && c.Head.IsCompleted));

        loop.Run(Cancel(2));

        Assert.True(await server.Conns.First().EndsWithinAsync(TimeSpan.FromSeconds(5)), "the upgrade was not aborted");
        await Task.Delay(Quiet);
        Assert.Empty(loop.Drain());
    }

    [Fact]
    public async Task ACancelledOpenConnectionIsDroppedAndItsQueuedInputsGoNowhere()
    {
        var sent = new TaskCompletionSource();
        var dropped = new TaskCompletionSource();
        using var server = new RawServer(async c =>
        {
            var (_, headers) = await c.ReadHeadAsync();
            var ws = await c.AcceptAsync(headers);
            await ws.SendAsync(new byte[] { 0, 1, 2 }, WebSocketMessageType.Binary, true, CancellationToken.None);
            sent.SetResult();
            try
            {
                await ws.ReceiveAsync(new byte[64], CancellationToken.None);
            }
            finally
            {
                dropped.SetResult();
            }
        });
        var loop = TestLoop.New();
        loop.Run(new Effect { WsOpen = new WsOpen { Id = 3, Url = server.Url("ws"), MaxMessageBytes = 1 << 20 } });
        Assert.Equal(Input.KindOneofCase.WsOpened, loop.Next().KindCase);
        await sent.Task;
        Assert.True(await Wait.ForAsync(() => loop.Queued == 1)); // the message, queued for the core

        loop.Run(Cancel(3));

        // The message queued before the cancel never reaches the core; nothing comes after.
        await dropped.Task.WaitAsync(TimeSpan.FromSeconds(5));
        await Task.Delay(Quiet);
        Assert.Empty(loop.Drain());
    }

    [Fact]
    public async Task ACancelledClosingConnectionIsDroppedWithoutWaitingForTheHandshake()
    {
        // The server never answers the Close: the client has no timeout of its own on it.
        var open = new TaskCompletionSource<WebSocket>();
        using var server = new RawServer(async c =>
        {
            var (_, headers) = await c.ReadHeadAsync();
            open.SetResult(await c.AcceptAsync(headers));
            await Task.Delay(Timeout.Infinite);
        });
        var loop = TestLoop.New();
        loop.Run(new Effect { WsOpen = new WsOpen { Id = 4, Url = server.Url("ws"), MaxMessageBytes = 1 << 20 } });
        Assert.Equal(Input.KindOneofCase.WsOpened, loop.Next().KindCase);
        await open.Task;

        loop.Run(new Effect { WsClose = new Id { Id_ = 4 } });
        await Task.Delay(TimeSpan.FromSeconds(3)); // longer than the 2 s close timeout the client had
        Assert.Empty(loop.Drain());
        loop.Run(Cancel(4));

        Assert.True(await server.Conns.First().EndsWithinAsync(TimeSpan.FromSeconds(5)), "the closing connection was not dropped");
        await Task.Delay(Quiet);
        Assert.Empty(loop.Drain());
    }

    [Fact]
    public async Task AClosedConnectionIsReportedClosedOnceTheHandshakeIsDone()
    {
        using var server = new RawServer(async c =>
        {
            var (_, headers) = await c.ReadHeadAsync();
            using var ws = await c.AcceptAsync(headers);
            var r = await ws.ReceiveAsync(new byte[64], CancellationToken.None);
            if (r.MessageType == WebSocketMessageType.Close)
            {
                await Task.Delay(300); // the handshake takes a while: the client waits for it
                await ws.CloseOutputAsync(WebSocketCloseStatus.NormalClosure, string.Empty, CancellationToken.None);
            }
        });
        var loop = TestLoop.New();
        loop.Run(new Effect { WsOpen = new WsOpen { Id = 5, Url = server.Url("ws"), MaxMessageBytes = 1 << 20 } });
        Assert.Equal(Input.KindOneofCase.WsOpened, loop.Next().KindCase);

        loop.Run(new Effect { WsClose = new Id { Id_ = 5 } });

        Assert.Equal(new Input { WsClosed = new WsClosed { Id = 5, Code = 1000 } }, loop.Next());
        Assert.False(loop.X.InFlight.Has(5)); // final: the connection is over
        await Task.Delay(Quiet);
        Assert.Empty(loop.Drain());
    }

    [Fact]
    public async Task TheServersCloseIsReportedThenOursEndsTheConnection()
    {
        using var server = new RawServer(async c =>
        {
            var (_, headers) = await c.ReadHeadAsync();
            using var ws = await c.AcceptAsync(headers);
            await ws.CloseOutputAsync(WebSocketCloseStatus.EndpointUnavailable, string.Empty, CancellationToken.None);
            await ws.ReceiveAsync(new byte[64], CancellationToken.None); // the answer
        });
        var loop = TestLoop.New();
        loop.Run(new Effect { WsOpen = new WsOpen { Id = 6, Url = server.Url("ws"), MaxMessageBytes = 1 << 20 } });
        Assert.Equal(Input.KindOneofCase.WsOpened, loop.Next().KindCase);
        Assert.Equal(new Input { WsClosed = new WsClosed { Id = 6, Code = 1001 } }, loop.Next());
        Assert.True(loop.X.InFlight.Has(6)); // not over until the core answers

        loop.Run(new Effect { WsClose = new Id { Id_ = 6 } }); // the answer

        Assert.Equal(new Input { WsClosed = new WsClosed { Id = 6, Code = 1001 } }, loop.Next());
        Assert.False(loop.X.InFlight.Has(6));
        await Task.Delay(Quiet);
        Assert.Empty(loop.Drain());
    }

    [Fact]
    public async Task ACancelledDownloadIsAbortedAndItsPartialFileDeleted()
    {
        using var server = new RawServer(async c =>
        {
            await c.ReadHeadAsync();
            await Http1.WriteAsync(c.Stream, "HTTP/1.1 200 OK\r\nContent-Length: 1000000\r\n\r\n", new byte[400_000]);
            await Task.Delay(Timeout.Infinite);
        });
        var loop = TestLoop.New();
        var file = loop.X.DownloadPath(7);
        loop.Run(new Effect { Download = new Download { Id = 7, Url = server.Url("http", "/file"), MaxBytes = 1 << 20 } });
        Assert.True(await Wait.ForAsync(() => File.Exists(file) && new FileInfo(file).Length > 0), "nothing was written");

        loop.Run(Cancel(7));

        Assert.True(await Wait.ForAsync(() => !File.Exists(file), 5), "the partial file is still there");
        Assert.True(await server.Conns.First().EndsWithinAsync(TimeSpan.FromSeconds(5)), "the download was not aborted");
        await Task.Delay(Quiet);
        Assert.Empty(loop.Drain());
    }

    [Fact]
    public async Task ACancelledSaveFinishesWholeAndIsNeverReported()
    {
        var loop = TestLoop.New();
        var state = loop.State!;
        using var started = new ManualResetEventSlim();
        using var release = new ManualResetEventSlim();
        var full = Path.GetFullPath(state);
        void Hold(string path)
        {
            if (Path.GetFullPath(path) == full)
            {
                started.Set();
                release.Wait();
            }
        }

        StateFile.Saving += Hold;
        try
        {
            loop.Run(new Effect { Store = new Core.Store { Id = 8, Data = ByteString.CopyFromUtf8("saved") } });
            Assert.True(started.Wait(TimeSpan.FromSeconds(10)));

            loop.Run(Cancel(8));
            release.Set();
            StateFile.Settled(state);
        }
        finally
        {
            StateFile.Saving -= Hold;
        }

        Assert.Equal("saved", File.ReadAllText(state));
        await Task.Delay(Quiet);
        Assert.Empty(loop.Drain());
    }

    private static Effect Cancel(ulong id) => new() { Cancel = new Id { Id_ = id } };
}
