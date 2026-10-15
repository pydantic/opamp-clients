// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Collections.Concurrent;
using System.Net;
using System.Net.Sockets;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

public class InstanceUidTests
{
    // With no UID of the application's, the core makes one (a UUIDv7 of the clock and 32 bytes of
    // the OS's randomness) and the client uses it; the application's, if it chose one.
    // covers: RULE-CLIENT-19
    [Fact]
    public async Task TheInstanceUidIsTheCoresUnlessTheApplicationChoseOne()
    {
        using var server = new TestServer();
        OpAmpClient Start(byte[]? uid) => OpAmpClient.Start(
            new OpAmpClientOptions { Mode = OpAmpMode.DotNet, Endpoint = new Uri(server.Endpoint("http")), InstanceUid = uid, HeartbeatInterval = TimeSpan.FromHours(1) },
            new OpAmpStatus());
        var a = Start(null);
        var b = Start(null);
        var chosen = Enumerable.Repeat((byte)0x42, 16).ToArray();
        var c = Start(chosen);
        Assert.True(await Wait.ForAsync(() => server.Messages.Count >= 3));
        foreach (var client in new[] { a, b, c })
        {
            await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        }

        Assert.NotEqual(a.InstanceUid, b.InstanceUid);
        foreach (var u in new[] { a.InstanceUid, b.InstanceUid })
        {
            Assert.Equal(16, u.Length);
            Assert.Equal(7, u[6] >> 4);
            Assert.Equal(2, u[8] >> 6);
            long ms = 0;
            for (var i = 0; i < 6; i++)
            {
                ms = (ms << 8) | u[i];
            }

            Assert.True(Math.Abs(DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() - ms) < 10_000);
            Assert.Contains(server.Messages, m => m.Message.InstanceUid.ToByteArray().SequenceEqual(u));
        }

        Assert.Equal(chosen, c.InstanceUid);
        Assert.Contains(server.Messages, m => m.Message.InstanceUid.ToByteArray().SequenceEqual(chosen));
    }

    // RequestInstanceUid reaches the core: the client asks until the server assigns a UID, then
    // uses it; without the option it never asks.
    // covers: OPAMP-UID-04
    [Theory]
    [InlineData(true)]
    [InlineData(false)]
    public async Task RequestInstanceUidAsksTheServer(bool on)
    {
        var assigned = Enumerable.Repeat((byte)0x77, 16).ToArray();
        var got = new ConcurrentQueue<AgentToServer>();
        var l = new TcpListener(IPAddress.Loopback, 0);
        l.Start();
        var port = ((IPEndPoint)l.LocalEndpoint).Port;
        l.Stop();
        using var listener = new HttpListener();
        listener.Prefixes.Add($"http://127.0.0.1:{port}/");
        listener.Start();
        var serving = Task.Run(async () =>
        {
            while (listener.IsListening)
            {
                HttpListenerContext ctx;
                try
                {
                    ctx = await listener.GetContextAsync();
                }
                catch (Exception e) when (e is HttpListenerException or ObjectDisposedException)
                {
                    return;
                }

                using var body = new MemoryStream();
                await ctx.Request.InputStream.CopyToAsync(body);
                var msg = AgentToServer.Parser.ParseFrom(body.ToArray());
                got.Enqueue(msg);
                var reply = new ServerToAgent { InstanceUid = msg.InstanceUid };
                if ((msg.Flags & (ulong)AgentToServerFlags.RequestInstanceUid) != 0)
                {
                    reply.AgentIdentification = new AgentIdentification { NewInstanceUid = ByteString.CopyFrom(assigned) };
                }

                ctx.Response.ContentType = "application/x-protobuf";
                await ctx.Response.OutputStream.WriteAsync(reply.ToByteArray());
                ctx.Response.Close();
            }
        });

        var notes = new ConcurrentQueue<OpAmpNotification>();
        var client = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Mode = OpAmpMode.DotNet,
                Endpoint = new Uri($"http://127.0.0.1:{port}/v1/opamp"),
                HeartbeatInterval = TimeSpan.FromMilliseconds(50),
                RequestInstanceUid = on,
            },
            new OpAmpStatus(),
            notes.Enqueue);
        for (var i = 0; i < 1000 && got.Count < 3; i++)
        {
            await Task.Delay(10);
        }

        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        listener.Close();

        var messages = got.ToArray();
        Assert.Equal(on, (messages[0].Flags & 1) != 0);
        Assert.All(messages.Skip(1), m => Assert.Equal(0UL, m.Flags & 1));
        if (on)
        {
            Assert.Equal(assigned, messages[^1].InstanceUid.ToByteArray());
            Assert.Contains(notes, n => n is OpAmpNotification.InstanceUidChanged);
        }
        else
        {
            Assert.NotEqual(assigned, messages[^1].InstanceUid.ToByteArray());
        }
    }
}
