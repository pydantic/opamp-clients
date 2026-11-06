// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Globalization;
using System.Net;
using System.Net.Sockets;
using System.Net.WebSockets;
using FsCheck;
using FsCheck.Fluent;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Xunit;

namespace Logfire.Fleet.Client.Tests;

/// <summary>
/// Properties of the client around the core, over random plans (FsCheck): the application's calls,
/// each made from one of up to four threads at once (with pauses of 0-2 ms), or from inside the
/// notification callback (so it reaches the client while it is still carrying out the step that
/// called back), over either transport. Order is only defined per caller. Whatever the plan: the
/// healths reach the server in the order each caller set them (a health set over another before it
/// went is never sent: only the latest counts), and the server ends with the health and the
/// description set last; sequence numbers run without a gap; the goodbye is the last message;
/// nothing is reported as an error. Runs: OPAMP_PROP_RUNS (default 25).
/// </summary>
public class PropertyTests
{
    private const int FromCallback = 0;
    private static readonly byte[] Uid = Enumerable.Repeat((byte)0x42, 16).ToArray();

    [Fact]
    public void CallsFromAnyThreadArriveInOrder()
    {
        var runs = int.TryParse(Environment.GetEnvironmentVariable("OPAMP_PROP_RUNS"), out var n) ? n : 25;
        var plans =
            from threads in Gen.Choose(1, 4)
            from length in Gen.Choose(1, 24)
            from steps in Gen.ArrayOf(
                from describe in Gen.Elements(true, false)
                from caller in Gen.Choose(FromCallback, threads)
                from pauseMs in Gen.Elements(0, 0, 1, 2)
                select new Step(describe, caller, pauseMs),
                length)
            from ws in Gen.Elements(true, false)
            select new Plan(steps, threads, ws);
        Check.One(
            Config.QuickThrowOnFailure.WithMaxTest(runs),
            Prop.ForAll(Arb.From(plans, Shrink), plan => Task.Run(() => RunAsync(plan)).GetAwaiter().GetResult()));
    }

    /// <summary>Shorter plans: one step fewer.</summary>
    private static IEnumerable<Plan> Shrink(Plan p) =>
        Enumerable.Range(0, p.Steps.Length)
            .Where(_ => p.Steps.Length > 1)
            .Select(i => p with { Steps = p.Steps.Where((_, j) => j != i).ToArray() });

    private static AgentDescription Description(int n)
    {
        var d = new AgentDescription();
        d.NonIdentifyingAttributes.Add(new KeyValue { Key = "n", Value = new AnyValue { IntValue = n } });
        return d;
    }

    private static async Task RunAsync(Plan plan)
    {
        var got = new List<AgentToServer>();
        byte[] Reply(AgentToServer msg)
        {
            lock (got)
            {
                got.Add(msg);
            }

            // Every answer carries a new remote config, so the application is called back often (the
            // callback answers each, so the core hands over the next).
            var hash = ByteString.CopyFromUtf8($"config-{got.Count}");
            var config = new AgentRemoteConfig { ConfigHash = hash, Config = new AgentConfigMap() };
            return new ServerToAgent { InstanceUid = msg.InstanceUid, RemoteConfig = config }.ToByteArray();
        }

        var (loopback, port) = LoopbackListener.Start();
        using var listener = loopback;
        var sockets = new List<Task>();
        var serving = Task.Run(async () =>
        {
            while (listener.IsListening)
            {
                HttpListenerContext ctx;
                try
                {
                    ctx = await listener.GetContextAsync();
                }
                catch (Exception e) when (e is HttpListenerException or ObjectDisposedException or InvalidOperationException)
                {
                    return; // stopped (InvalidOperationException: between the check and the call)
                }

                if (ctx.Request.IsWebSocketRequest)
                {
                    var conn = (await ctx.AcceptWebSocketAsync(null)).WebSocket;
                    lock (sockets)
                    {
                        sockets.Add(ServeWebSocketAsync(ctx, conn, Reply));
                    }

                    continue;
                }

                using var body = new MemoryStream();
                await ctx.Request.InputStream.CopyToAsync(body);
                var reply = Reply(AgentToServer.Parser.ParseFrom(body.ToArray()));
                ctx.Response.ContentType = "application/x-protobuf";
                await ctx.Response.OutputStream.WriteAsync(reply);
                ctx.Response.Close();
            }
        });

        // Who carries out each step: the callback takes each run of steps due from it at once.
        var steps = plan.Steps.Select((s, i) => (Id: i + 1, s.Describe, s.Caller, s.PauseMs)).ToArray();
        var bursts = new List<List<(int Id, bool Describe, int Caller, int PauseMs)>>();
        for (var i = 0; i < steps.Length; i++)
        {
            if (steps[i].Caller == FromCallback)
            {
                if (i == 0 || steps[i - 1].Caller != FromCallback)
                {
                    bursts.Add(new());
                }

                bursts[^1].Add(steps[i]);
            }
        }

        var sent = Enumerable.Range(0, plan.Threads + 1).Select(_ => new List<int>()).ToArray();
        var order = new object(); // descriptions and healths are set under it, so the last one set is known
        var lastDescription = 0;
        var lastHealth = 0;
        var errors = new List<string>();
        var burstsDone = 0;
        OpAmpClient? client = null;

        void CarryOut((int Id, bool Describe, int Caller, int PauseMs) s)
        {
            var c = Volatile.Read(ref client)!;
            if (s.Describe)
            {
                lock (order)
                {
                    lastDescription = s.Id;
                    c.SetDescription(Description(s.Id));
                }
            }
            else
            {
                lock (sent)
                {
                    sent[s.Caller].Add(s.Id);
                }

                lock (order)
                {
                    lastHealth = s.Id;
                    c.SetHealth(new ComponentHealth { Healthy = true, Status = s.Id.ToString(CultureInfo.InvariantCulture) });
                }
            }
        }

        void Notify(OpAmpNotification n)
        {
            lock (errors)
            {
                if (n is OpAmpNotification.Stopped { Error: { } e })
                {
                    errors.Add(e.ToString());
                }

                // Answered, so the core hands over the next remote config (and calls back again).
                if (n is OpAmpNotification.RemoteConfig rc && Volatile.Read(ref client) is { } h)
                {
                    h.SetRemoteConfigStatus(new RemoteConfigStatus
                    {
                        LastRemoteConfigHash = rc.Config.ConfigHash, Status = RemoteConfigStatuses.Applied,
                    });
                }
            }

            try
            {
                // The steps due from the callback, called from inside it.
                if (Volatile.Read(ref client) is not null && burstsDone < bursts.Count)
                {
                    bursts[burstsDone].ForEach(CarryOut);
                    Interlocked.Increment(ref burstsDone);
                }
            }
            catch (Exception e)
            {
                lock (errors)
                {
                    errors.Add(e.ToString());
                }
            }
        }

        var started = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Mode = OpAmpMode.Platform,
                Endpoint = new Uri($"{(plan.Ws ? "ws" : "http")}://127.0.0.1:{port}/v1/opamp"),
                InstanceUid = Uid,
                HeartbeatInterval = TimeSpan.FromMilliseconds(20),
                Capabilities = (ulong)(AgentCapabilities.AcceptsRemoteConfig | AgentCapabilities.ReportsRemoteConfig | AgentCapabilities.ReportsHealth),
            },
            new OpAmpStatus
            {
                Description = Description(0),
            },
            Notify);
        Volatile.Write(ref client, started);

        // The threads start together (spinning, so none is still waking up) and each carries out
        // its own steps, in order.
        var waiting = 0;
        var go = 0;
        var threads = Enumerable.Range(1, plan.Threads).Select(t => Task.Factory.StartNew(
            () =>
            {
                Interlocked.Increment(ref waiting);
                while (Volatile.Read(ref go) == 0)
                {
                    Thread.SpinWait(10);
                }

                foreach (var s in steps.Where(s => s.Caller == t))
                {
                    if (s.PauseMs > 0)
                    {
                        Thread.Sleep(s.PauseMs);
                    }

                    CarryOut(s);
                }
            },
            CancellationToken.None,
            TaskCreationOptions.LongRunning,
            TaskScheduler.Default)).ToArray();
        while (Volatile.Read(ref waiting) < plan.Threads)
        {
            Thread.Yield();
        }

        Volatile.Write(ref go, 1);
        await Task.WhenAll(threads).WaitAsync(TimeSpan.FromSeconds(10));
        string[] Errors()
        {
            lock (errors)
            {
                return errors.ToArray();
            }
        }

        for (var i = 0; i < 1000 && Volatile.Read(ref burstsDone) < bursts.Count && Errors().Length == 0; i++)
        {
            await Task.Delay(5);
        }

        Assert.Empty(Errors());
        Assert.True(Volatile.Read(ref burstsDone) == bursts.Count, "the plan stalled");

        int[] Sent()
        {
            lock (sent)
            {
                return sent.SelectMany(s => s).Order().ToArray();
            }
        }

        AgentToServer[] Got()
        {
            lock (got)
            {
                return got.ToArray();
            }
        }

        int[] Delivered() => Got().Where(m => m.Health is { Status: var st } && st.Length > 0 && char.IsAsciiDigit(st[0]))
            .Select(m => int.Parse(m.Health.Status, CultureInfo.InvariantCulture)).ToArray();
        long[] Descriptions() => Got().Where(m => m.AgentDescription is not null)
            .Select(m => m.AgentDescription.NonIdentifyingAttributes[0].Value.IntValue).ToArray();
        for (var i = 0; i < 1000; i++)
        {
            if ((Delivered().LastOrDefault(0) == Volatile.Read(ref lastHealth) && Descriptions().LastOrDefault(-1) == lastDescription)
                || Errors().Length > 0)
            {
                break;
            }

            await Task.Delay(5);
        }

        await started.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        Task[] open;
        lock (sockets)
        {
            open = sockets.ToArray();
        }

        // Over WebSocket the goodbye is in before the server has seen the close.
        await Task.WhenAll(open).WaitAsync(TimeSpan.FromSeconds(10));
        listener.Close();
        await serving;

        Assert.Empty(Errors());
        var delivered = Delivered();
        Assert.Equal(lastHealth, delivered.LastOrDefault(0)); // the last set goes
        Assert.Subset(Sent().ToHashSet(), delivered.ToHashSet()); // only what was set
        for (var c = 0; c <= plan.Threads; c++)
        {
            var mine = steps.Where(s => s.Caller == c).Select(s => s.Id).ToHashSet();
            var theirs = delivered.Where(mine.Contains).ToArray();
            Assert.Equal(theirs.Order(), theirs); // each caller's in its order
        }

        Assert.Equal(lastDescription, Descriptions().Last());
        var messages = Got();
        Assert.Equal(ClientTests.ExpectedSequence(messages), messages.Select(m => m.SequenceNum));
        Assert.NotNull(messages[^1].AgentDisconnect);
    }

    private static async Task ServeWebSocketAsync(HttpListenerContext ctx, WebSocket conn, Func<AgentToServer, byte[]> reply)
    {
        var buf = new byte[65536];
        using var message = new MemoryStream();
        try
        {
            while (true)
            {
                var r = await conn.ReceiveAsync(buf, CancellationToken.None);
                if (r.MessageType == WebSocketMessageType.Close)
                {
                    await conn.CloseOutputAsync(WebSocketCloseStatus.NormalClosure, string.Empty, CancellationToken.None);
                    return;
                }

                message.Write(buf, 0, r.Count);
                if (r.EndOfMessage)
                {
                    // The OpAMP header (0) before the message, both ways.
                    var answer = reply(AgentToServer.Parser.ParseFrom(message.ToArray().AsSpan(1)));
                    message.SetLength(0);
                    await conn.SendAsync(new byte[] { 0 }.Concat(answer).ToArray(), WebSocketMessageType.Binary, true, CancellationToken.None);
                }
            }
        }
        catch (Exception e) when (e is WebSocketException or IOException or ObjectDisposedException)
        {
            // the client went: what it said is in
        }
        finally
        {
            // The client waits for the connection to close after the Close frames: close it now.
            ctx.Response.Abort();
            conn.Dispose();
        }
    }

    /// <summary>A step: a new description or a new health, by caller 1..threads or from the callback (0).</summary>
    private sealed record Step(bool Describe, int Caller, int PauseMs)
    {
        public override string ToString() =>
            $"{(this.Describe ? "describe" : "health")}@{(this.Caller == FromCallback ? "callback" : $"t{this.Caller}")}"
            + (this.PauseMs > 0 ? $"+{this.PauseMs}ms" : string.Empty);
    }

    private sealed record Plan(Step[] Steps, int Threads, bool Ws)
    {
        public override string ToString() =>
            $"{(this.Ws ? "ws" : "http")}, {this.Threads} thread(s): {string.Join(' ', this.Steps.Select(s => s.ToString()))}";
    }
}
