// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Net.WebSockets;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// The core closes a connection (ws_close, as at every stop): once the closing handshake is done
/// the core hears ws_closed with the server's code, and that ends the connection, as every other
/// shell reports it; a server that drops the connection instead of answering: ws_lost. Servers
/// answer the way real ones do: at once, then the TCP connection goes.
/// </summary>
public class WsCloseTests
{
    private static readonly TimeSpan Quiet = TimeSpan.FromMilliseconds(500);

    /// <summary>What the server does once the client's Close has come (or, crossing: at once).</summary>
    public enum Answer
    {
        /// <summary>Its own Close, with code 1000, then the TCP connection goes.</summary>
        Close1000,

        /// <summary>Its own Close, with code 4001, then the TCP connection goes.</summary>
        Close4001,

        /// <summary>Its own Close, after a while, and the connection stays.</summary>
        LateClose,

        /// <summary>No Close: the TCP connection goes.</summary>
        Drop,
    }

    [Theory]
    [InlineData(Answer.Close1000, 1000u)]
    [InlineData(Answer.Close4001, 4001u)]
    [InlineData(Answer.LateClose, 1000u)]
    [InlineData(Answer.Drop, null)]
    public async Task TheCoresCloseEndsWithTheServersAnswer(Answer answer, uint? code)
    {
        using var server = new RawServer(async c =>
        {
            var (_, headers) = await c.ReadHeadAsync();
            using var ws = await c.AcceptAsync(headers);
            var r = await ws.ReceiveAsync(new byte[64], CancellationToken.None);
            Assert.Equal(WebSocketMessageType.Close, r.MessageType);
            await ServerAnswersAsync(ws, c, answer);
        });
        var loop = TestLoop.New();
        loop.Run(new Effect { WsOpen = new WsOpen { Id = 1, Url = server.Url("ws"), MaxMessageBytes = 1 << 20 } });
        Assert.Equal(Input.KindOneofCase.WsOpened, loop.Next().KindCase);

        loop.Run(new Effect { WsClose = new Id { Id_ = 1 } });

        var end = loop.Next();
        if (code is { } want)
        {
            Assert.Equal(new Input { WsClosed = new WsClosed { Id = 1, Code = want } }, end);
        }
        else
        {
            Assert.Equal(Input.KindOneofCase.WsLost, end.KindCase);
        }

        Assert.False(loop.X.InFlight.Has(1)); // final
        await Task.Delay(Quiet);
        Assert.Empty(loop.Drain());
        await loop.X.ReleaseAsync(TimeSpan.FromSeconds(5));
    }

    // As at every stop: the core sends its last message (the goodbye) and closes at once. The
    // server answers the message, then the Close (and the TCP connection goes), as the
    // conformance server does: the answer comes while the client is closing, and the closing
    // handshake still ends closed, with the server's code.
    [Fact]
    public async Task AnAnswerInFlightAsTheCoreClosesDoesntSpoilTheHandshake()
    {
        for (ulong id = 1; id <= 8; id++)
        {
            using var server = new RawServer(async c =>
            {
                var (_, headers) = await c.ReadHeadAsync();
                using var ws = await c.AcceptAsync(headers);
                var buf = new byte[1024];
                while (true)
                {
                    var r = await ws.ReceiveAsync(buf, CancellationToken.None);
                    if (r.MessageType == WebSocketMessageType.Close)
                    {
                        await ws.CloseOutputAsync(WebSocketCloseStatus.NormalClosure, string.Empty, CancellationToken.None);
                        c.Tcp.Dispose();
                        return;
                    }

                    await ws.SendAsync(new byte[] { 0, 1, 2, 3 }, WebSocketMessageType.Binary, true, CancellationToken.None);
                }
            });
            var loop = TestLoop.New();
            loop.Run(new Effect { WsOpen = new WsOpen { Id = id, Url = server.Url("ws"), MaxMessageBytes = 1 << 20 } });
            Assert.Equal(Input.KindOneofCase.WsOpened, loop.Next().KindCase);

            loop.Run(new Effect { WsSend = new WsMessage { Id = id, Data = Google.Protobuf.ByteString.CopyFrom(0, 9) } });
            loop.Run(new Effect { WsClose = new Id { Id_ = id } });

            var end = loop.Next();
            while (end.KindCase == Input.KindOneofCase.WsMessage)
            {
                end = loop.Next(); // the answer, if it came before the Close was sent
            }

            Assert.Equal(new Input { WsClosed = new WsClosed { Id = id, Code = 1000 } }, end);
            Assert.False(loop.X.InFlight.Has(id));
            await loop.X.ReleaseAsync(TimeSpan.FromSeconds(5));
        }
    }

    // The server's Close crosses ours: the server closes as the core does, neither answering the
    // other. Whichever the client saw first, the connection ends once, closed, with the server's code.
    [Fact]
    public async Task AServersCloseCrossingOursEndsItOnce()
    {
        using var go = new SemaphoreSlim(0);
        using var server = new RawServer(async c =>
        {
            var (_, headers) = await c.ReadHeadAsync();
            using var ws = await c.AcceptAsync(headers);
            await go.WaitAsync();
            await ws.CloseOutputAsync((WebSocketCloseStatus)4002, string.Empty, CancellationToken.None);
            await ws.ReceiveAsync(new byte[64], CancellationToken.None); // the client's
            c.Tcp.Dispose();
        });
        var loop = TestLoop.New();
        loop.Run(new Effect { WsOpen = new WsOpen { Id = 2, Url = server.Url("ws"), MaxMessageBytes = 1 << 20 } });
        Assert.Equal(Input.KindOneofCase.WsOpened, loop.Next().KindCase);

        go.Release();
        loop.Run(new Effect { WsClose = new Id { Id_ = 2 } });

        var ends = new List<Input> { loop.Next() };
        if (ends[0].KindCase == Input.KindOneofCase.WsClosed && loop.X.InFlight.Has(2))
        {
            // The server's Close came first and was reported: our close then ends it.
            ends.Add(loop.Next());
        }

        Assert.All(ends, e => Assert.Equal(new WsClosed { Id = 2, Code = 4002 }, e.WsClosed));
        Assert.False(loop.X.InFlight.Has(2));
        await Task.Delay(Quiet);
        Assert.Empty(loop.Drain());
        await loop.X.ReleaseAsync(TimeSpan.FromSeconds(5));
    }

    private static async Task ServerAnswersAsync(WebSocket ws, RawConn c, Answer answer)
    {
        switch (answer)
        {
            case Answer.Close1000:
                await ws.CloseOutputAsync(WebSocketCloseStatus.NormalClosure, string.Empty, CancellationToken.None);
                c.Tcp.Dispose();
                break;
            case Answer.Close4001:
                await ws.CloseOutputAsync((WebSocketCloseStatus)4001, string.Empty, CancellationToken.None);
                c.Tcp.Dispose();
                break;
            case Answer.LateClose:
                await Task.Delay(300);
                await ws.CloseOutputAsync(WebSocketCloseStatus.NormalClosure, string.Empty, CancellationToken.None);
                await Task.Delay(Timeout.Infinite);
                break;
            case Answer.Drop:
                c.Tcp.Dispose();
                break;
        }
    }
}
