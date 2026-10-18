// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Net;
using System.Net.Sockets;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

public class ClientTests
{
    private const string UidText = "42424242-4242-4242-4242-424242424242";
    private static readonly byte[] Uid = Enumerable.Repeat((byte)0x42, 16).ToArray();

    /// <summary>
    /// The sequence numbers a run should have: 0 until the server has answered a message (those
    /// sent before its first answer: RULE-CLIENT-20), then 1, 2, ...
    /// </summary>
    internal static ulong[] ExpectedSequence(AgentToServer[] got)
    {
        var first = 0;
        while (first < got.Length && got[first].SequenceNum == 0)
        {
            first++;
        }
        var zeros = Math.Max(1, first);
        return Enumerable.Range(0, got.Length).Select(i => i < zeros ? 0UL : (ulong)(i - zeros + 1)).ToArray();
    }

    [Theory]
    [InlineData(OpAmpMode.Platform)]
    [InlineData(OpAmpMode.Core)]
    public async Task HttpReportsThenSaysGoodbyeAfterTheRequestInFlight(OpAmpMode mode)
    {
        var got = new ConcurrentQueue<AgentToServer>();
        var headers = new ConcurrentQueue<NameValueCollectionSnapshot>();
        var (loopback, port) = LoopbackListener.Start();
        using var listener = loopback;
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

                headers.Enqueue(new NameValueCollectionSnapshot(
                    ctx.Request.ContentType, ctx.Request.Headers["OpAMP-Instance-UID"], ctx.Request.Headers["Authorization"]));
                using var body = new MemoryStream();
                await ctx.Request.InputStream.CopyToAsync(body);
                var msg = AgentToServer.Parser.ParseFrom(body.ToArray());
                got.Enqueue(msg);
                if (got.Count == 2)
                {
                    await Task.Delay(200); // hold a poll, so stop comes while it is in flight
                }

                var reply = new ServerToAgent { InstanceUid = msg.InstanceUid }.ToByteArray();
                ctx.Response.ContentType = "application/x-protobuf";
                await ctx.Response.OutputStream.WriteAsync(reply);
                ctx.Response.Close();
            }
        });

        var notes = new ConcurrentQueue<OpAmpNotification>();
        var description = new AgentDescription();
        description.IdentifyingAttributes.Add(new KeyValue { Key = "service.name", Value = new AnyValue { StringValue = "test" } });
        var client = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Endpoint = new Uri($"http://127.0.0.1:{port}/v1/opamp"),
                InstanceUid = Uid,
                HeartbeatInterval = TimeSpan.FromMilliseconds(50),
                Headers = new Dictionary<string, string> { ["Authorization"] = "Bearer t" },
                Mode = mode, DirectConnection = mode == OpAmpMode.Core,
            },
            new OpAmpStatus { Description = description },
            notes.Enqueue);
        for (var i = 0; i < 1000 && got.Count < 2; i++)
        {
            await Task.Delay(10);
        }

        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        listener.Close();

        var messages = got.ToArray();
        Assert.NotNull(messages[0].AgentDescription);
        Assert.NotNull(messages[^1].AgentDisconnect);
        Assert.Equal(ExpectedSequence(messages), messages.Select(m => m.SequenceNum));

        foreach (var h in headers)
        {
            Assert.Equal("application/x-protobuf", h.ContentType);
            Assert.Equal(UidText, h.InstanceUid);
            Assert.Equal("Bearer t", h.Authorization);
        }

        Assert.IsType<OpAmpNotification.Stopped>(notes.Last());
    }

    // covers: OPAMP-SIZE-02, OPAMP-SIZE-08
    [Fact]
    public async Task MaxSendBytesReachesTheCore()
    {
        var got = new ConcurrentQueue<AgentToServer>();
        var (loopback, port) = LoopbackListener.Start();
        using var listener = loopback;
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
                var reply = new ServerToAgent { InstanceUid = msg.InstanceUid }.ToByteArray();
                ctx.Response.ContentType = "application/x-protobuf";
                await ctx.Response.OutputStream.WriteAsync(reply);
                ctx.Response.Close();
            }
        });

        var notes = new ConcurrentQueue<OpAmpNotification>();
        var big = new AgentDescription();
        big.IdentifyingAttributes.Add(new KeyValue { Key = "big", Value = new AnyValue { StringValue = new string('x', 4096) } });
        var client = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Mode = OpAmpMode.Platform,
                Endpoint = new Uri($"http://127.0.0.1:{port}/v1/opamp"),
                InstanceUid = Uid,
                MaxSendBytes = 1024,
            },
            new OpAmpStatus { Description = big },
            notes.Enqueue);
        for (var i = 0; i < 1000 && got.IsEmpty; i++)
        {
            await Task.Delay(10);
        }

        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        listener.Close();

        Assert.Null(got.ToArray()[0].AgentDescription);
        Assert.Contains(notes, n => n is OpAmpNotification.StatusTooLarge { Field: "agent_description" });
    }

    private sealed record NameValueCollectionSnapshot(string? ContentType, string? InstanceUid, string? Authorization);
}
