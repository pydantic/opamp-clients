// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Diagnostics;
using System.Globalization;
using System.Net.WebSockets;
using System.Text.RegularExpressions;
using CsCheck;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// The shell contract (spec/shell-contract.md) over schedules the network and the clock never
/// give a real server: the client runs the real core on an in-memory transport
/// (<see cref="MemoryTransport"/>) and fake time (FakeTimeProvider), and CsCheck generates what the
/// server, the network, the clock and the application do, in which order, from which thread. A
/// call can hold, fail with any exception type, or (deaf) ignore its cancel and complete after it;
/// a body can hold or fail half-way; a connection can be closed by the server, or its receive, its
/// next send or its close can fail. Every step across the core boundary is checked as it goes
/// (<see cref="Trace"/>), and every client is stopped at the end and checked once stop returns
/// (<see cref="Rig.Finish"/>).
/// </summary>
/// <remarks>
/// Runs: OPAMP_SCHEDULE_RUNS (default 200) iterations of each property. CsCheck draws a seed per
/// iteration; a failure prints the smallest failing case and its seed: replay it with
/// OPAMP_SCHEDULE_SEED=&lt;seed&gt; (or CsCheck's own CsCheck_Seed), which runs that one case.
/// Real threads run the client, so a failure that depends on a race may need a few replays.
/// The model-based, linearizability and stop-bound properties run in both modes: in Rust mode the
/// client's streams are in memory and a minimal HTTP/1.1 and WebSocket server at their far end
/// (<see cref="MemServer"/>) turns what the core sends into the same calls, answered the same way;
/// connects may be refused or complete after their cancel, and https:// and wss:// ask for TLS.
/// The properties of exception types (where the transport throws, and which type) are platform
/// mode's alone: in Rust mode .NET carries bytes, and every such failure is a reset stream.
/// They keep several threads busy, so they run alone, after the rest: the timing of other
/// tests (the adversary's timers) is not theirs to disturb.
/// </remarks>
[Collection(nameof(ScheduleTests))]
public class ScheduleTests
{
    private const int MaxOps = 40;

    private static readonly ConcurrentBag<Rig> Rigs = new();

    private readonly ITestOutputHelper output;

    public ScheduleTests(ITestOutputHelper output)
    {
        this.output = output;

        // The samples block their threads while the clients run on the pool.
        ThreadPool.GetMinThreads(out var workers, out var io);
        ThreadPool.SetMinThreads(Math.Max(workers, 64), io);
    }

    /// <summary>Where the transport throws, in the exception-type property.</summary>
    public enum Site
    {
        PostSend,
        PostBody,
        GetSend,
        GetBody,
        Connect,
        WsReceive,
        WsSend,
        WsClose,
    }

    private static long Runs => long.TryParse(Environment.GetEnvironmentVariable("OPAMP_SCHEDULE_RUNS"), NumberStyles.None, CultureInfo.InvariantCulture, out var n) ? n : 200;

    private static string? Seed =>
        Environment.GetEnvironmentVariable("OPAMP_SCHEDULE_SEED") is { Length: > 0 } s ? s : Check.Seed;

    private static int Threads => Math.Clamp(Environment.ProcessorCount, 2, 4);

    /// <summary>
    /// Model-based: a random sequence of what the server, network, clock and application do (up
    /// to 40 steps, then stop), on HTTP or WebSocket, against the contract. The model is what each
    /// answer must become: a response, a status or an exception as http_response with that status,
    /// http_failed, download_response, download_failed, ws_opened or ws_failed (with the refusal's
    /// status), unless the core cancelled the effect first.
    /// </summary>
    // covers: RULE-SHELL-01, RULE-SHELL-04, RULE-SHELL-05, RULE-SHELL-06, RULE-SHELL-07, RULE-SHELL-10
    [Theory]
    [InlineData(false)]
    [InlineData(true)]
    public void ScheduleKeepsTheContract(bool rust) => this.Run($"{nameof(this.ScheduleKeepsTheContract)}({rust})", (seed, iter) =>
        (from ws in Gen.Bool
         from a in Gen.ULong
         from b in Gen.ULong
         from deafness in Gen.Int[0, 3]
         from tls in Gen.Bool
         from c in Gen.ULong
         from d in Gen.ULong
         from connects in Gen.Int[0, 2]
         let rig = Track(new Rig(ws, deafness == 0 ? 0 : a & b, rust: rust, tls: rust && tls, connectMask: rust && connects == 0 ? c & d & 0xFFFF_FFFF : 0))
         select (rig, new Model(rig)))
        .SampleModelBased(
            [AnswerOp, AnswerOp, AnswerOp, AnswerOp, AnswerOp, AnswerOp, AdvanceOp, AdvanceOp, AdvanceOp, CallOp, CallOp, ServerOp, ServerOp, ServerOp, StopOp],
            equal: (rig, model) =>
            {
                rig.Finish("at the end");
                model.Check();
                return true;
            },
            seed: seed,
            iter: iter,
            threads: Threads,
            printActual: r => r.ToString(),
            printModel: m => m.ToString()));

    /// <summary>
    /// Linearizable: the public API from several threads at once (calls and stop), over HTTP with
    /// a server that answers at once. The calls that reached the core before its stop, in order,
    /// must be those of some order of the operations (each thread's in its own order); then stop
    /// returns, every time it was called, with nothing left running, and the callback model holds.
    /// </summary>
    // covers: RULE-SHELL-01, RULE-SHELL-03, RULE-SHELL-07
    [Theory]
    [InlineData(false)]
    [InlineData(true)]
    public void CallsAndStopFromSeveralThreadsAreLinearizable(bool rust) => this.Run($"{nameof(this.CallsAndStopFromSeveralThreadsAreLinearizable)}({rust})", (seed, iter) =>
        Gen.Const(() =>
            {
                var rig = Track(new Rig(ws: false, 0, onCall: AnswerAtOnce, stateful: false, rust: rust));
                return (rig, new Calls());
            })
        .SampleParallel(
            [
                Gen.Int[0, 999].Operation<Rig, Calls>(i => $"health {i}", (r, i) => r.Client.SetHealth(new ComponentHealth { Status = $"h{i}" }), (m, i) => m.Add($"health h{i}")),
                Gen.Int[0, 999].Operation<Rig, Calls>(i => $"description {i}", (r, i) => r.Client.SetDescription(Description($"d{i}")), (m, i) => m.Add($"description d{i}")),
                Gen.Int[0, 999].Operation<Rig, Calls>(i => $"config {i}", (r, i) => r.Client.SetEffectiveConfig(Config($"c{i}")), (m, i) => m.Add($"config c{i}")),
                Gen.Operation<Rig, Calls>("stop", r => r.Stops.Add(r.Client.StopAsync()), m => m.Stopped = true),
            ],
            equal: (rig, calls) => rig.Settled().SequenceEqual(calls.List),
            seed: seed,
            iter: iter,
            threads: Threads,
            printActual: r => r.Report() + (r.Failure is { } f ? $"\n  failed: {f}" : string.Empty),
            printModel: m => string.Join(", ", m.List)));

    /// <summary>
    /// Whatever the transport throws, from wherever (sending, the body, connecting, a connection's
    /// receive, send or close), synchronously or in its task, the effect ends: the core hears
    /// http_failed, download_failed, ws_failed or ws_lost, once, without waiting on any timer.
    /// This is the test of the catch filters (S7's class).
    /// </summary>
    // covers: RULE-SHELL-04, RULE-SHELL-05, RULE-SHELL-07
    [Fact]
    public void EveryEffectEndsWhateverTheTransportThrows() => this.Run(nameof(this.EveryEffectEndsWhateverTheTransportThrows), (seed, iter) =>
        (from site in Gen.Int[0, 7]
         from fault in Gen.Int[0, Faults.Count - 1]
         from sync in Gen.Bool
         select ((Site)site, fault, sync))
        .Sample(
            t => EndsWhateverIsThrown(t.Item1, t.fault, t.sync),
            seed: seed,
            iter: iter,
            threads: Threads,
            print: t => $"{t.Item1}, {Faults.Names[t.fault]}, {(t.sync ? "thrown as called" : "in the task")}"));

    /// <summary>
    /// Found by <see cref="EveryEffectEndsWhateverTheTransportThrows"/> (and the schedules): a
    /// connection whose receive, send or close threw anything but the four types its filters named
    /// never ended (no ws_lost: the core held a dead connection until its own deadline), and one
    /// whose close failed was never aborted (its reader outlived stop).
    /// </summary>
    // covers: RULE-SHELL-05, RULE-SHELL-07
    [Theory]
    [InlineData(Site.WsReceive, 8, false)]
    [InlineData(Site.WsReceive, 15, false)]
    [InlineData(Site.WsSend, 5, true)]
    [InlineData(Site.WsSend, 16, false)]
    [InlineData(Site.WsClose, 9, true)]
    [InlineData(Site.WsClose, 11, false)]
    public void AConnectionEndsWhateverItThrows(Site site, int fault, bool sync) => Assert.True(EndsWhateverIsThrown(site, fault, sync));

    /// <summary>
    /// Found by <see cref="ScheduleKeepsTheContract"/> (seed bjyC-mrZtLl2): the core's output that
    /// says stopped can carry a notification after <c>Stopped</c> (a package downloaded while
    /// stopping, whose save was the last thing stop waited for, is handed over in the same
    /// output, after it), and the client delivered it: the application heard
    /// <c>InstallPackage</c> after <c>Stopped</c>. Within each output <c>Stopped</c> is told last,
    /// after the output's other notifications (RULE-SHELL-01). Here the save is held behind one of
    /// the test's own until the stop is in.
    /// </summary>
    // covers: RULE-SHELL-01
    [Fact]
    public void StoppedIsToldLastWhateverTheCoreHandsOver()
    {
        Rig rig = null!;
        rig = Track(new Rig(ws: false, 0, onCall: c =>
        {
            if (c.Kind == CallKind.Post)
            {
                c.Respond(200, rig.Offer(c.OfKind == 0 ? How.Package : How.Ok, AgentToServer.Parser.ParseFrom(c.Body).InstanceUid));
            }
        }));
        bool Saving() => rig.Trace.Unended().Any(u => u.StartsWith("Store", StringComparison.Ordinal));
        using var hold = new ManualResetEventSlim();
        try
        {
            _ = rig.Client;
            Assert.True(Rig.Until(() => rig.Transport.Calls.Any(c => c.Kind == CallKind.Get) && !Saving()), "no download, or a save never ended");

            // The saves from now on wait behind this one: the download's, until the stop is in.
            _ = StateFile.SaveBeside(rig.StateFile, [], () => hold.Wait(TimeSpan.FromSeconds(30)) && false, _ => { });
            var get = rig.Transport.Calls.First(c => c.Kind == CallKind.Get);
            Assert.True(get.Respond(200, rig.FileAt(get.Url)));
            Assert.True(Rig.Until(() => rig.Trace.EffectOf(CallKind.Get, 0) is { Ended: true } && Saving()), "no save after the download");
            rig.Stops.Add(rig.Client.StopAsync());
            Assert.True(Rig.Until(() => rig.Trace.StopTaken), "stop not taken");
        }
        finally
        {
            hold.Set();
        }

        rig.Finish("stopped while the download's save was held");
        var seen = rig.Agent.Seen();
        Assert.True(seen.Any(n => n is OpAmpNotification.InstallPackage), $"the package handed over with Stopped was never told: {string.Join(", ", seen.AsEnumerable())}");
    }

    /// <summary>
    /// A download that finishes, file and all, just as the core cancels it (its result queued
    /// behind the timer that cancels it) leaves no file: the cancel deletes what the download
    /// wrote (RULE-SHELL-06). The schedules reach this only by chance (the window is a queue's
    /// length), so here the loop is held while the timer and the result queue up.
    /// </summary>
    // covers: RULE-SHELL-06
    [Fact]
    public void ADownloadDoneAsItIsCancelledLeavesNoFile()
    {
        Rig rig = null!;
        rig = Track(new Rig(ws: false, 0, onCall: c =>
        {
            if (c.Kind == CallKind.Post)
            {
                c.Respond(200, rig.Offer(c.OfKind == 0 ? How.Package : How.Ok, AgentToServer.Parser.ParseFrom(c.Body).InstanceUid));
            }
        }));
        using var hold = new ManualResetEventSlim();
        try
        {
            _ = rig.Client;
            Assert.True(Rig.Until(() => rig.Transport.Calls.Any(c => c.Kind == CallKind.Get)), "no download");
            rig.Pace();

            // The loop held in its next step; the download's deadline passes (its timer queues),
            // then the download finishes (its result queues behind).
            rig.Trace.HoldLoop = hold;
            rig.Client.SetHealth(new ComponentHealth { Status = "hold the loop" });
            Assert.True(Rig.Until(() => rig.Trace.Holding), "the loop never stepped");
            var queued = rig.Client.Queued;
            rig.Time.Advance(TimeSpan.FromSeconds(61));
            Assert.True(Rig.Until(() => rig.Client.Queued > queued), "no timer");
            var get = rig.Transport.Calls.First(c => c.Kind == CallKind.Get);
            Assert.True(get.Respond(200, rig.FileAt(get.Url)));
            Assert.True(Rig.Until(() => rig.Client.Queued > queued + 1), "the download never finished");
        }
        finally
        {
            rig.Trace.HoldLoop = null;
            hold.Set();
        }

        Assert.True(Rig.Until(() => rig.Trace.EffectOf(CallKind.Get, 0) is { Cancelled: true } or { Ended: true }));
        Assert.True(rig.Trace.EffectOf(CallKind.Get, 0)!.Cancelled, $"the download wasn't cancelled first: {rig.Report()}");
        rig.Finish("a download done as it was cancelled");
    }

    /// <summary>
    /// All of stop takes one stop timeout (RULE-SHELL-07), though a save is held writing and the
    /// goodbye unanswered: the core waits its stop timeout for them and cancels them, and what is
    /// left of the timeout (or a short grace) is all the drain then waits for the save. It waited a
    /// whole stop timeout more (Go and Python had the same bug).
    /// </summary>
    // covers: RULE-SHELL-07
    [Theory]
    [InlineData(false)]
    [InlineData(true)]
    public void StopTakesOneStopTimeoutWithASaveAndTheGoodbyeHeld(bool rust)
    {
        var rig = Track(new Rig(ws: false, 0, rust: rust));
        using var hold = new ManualResetEventSlim();
        try
        {
            _ = rig.Client;
            Assert.True(Rig.Until(() => rig.Transport.Calls.Length == 1 && !rig.Trace.Unended().Any(u => u.StartsWith("Store", StringComparison.Ordinal))), "no request, or the first save never ended");

            // Every save of the client's from now on waits behind this one; the server's answer
            // (a remote config) makes one, and the goodbye is never answered.
            _ = StateFile.SaveBeside(rig.StateFile, [], () => hold.Wait(TimeSpan.FromSeconds(60)) && false, _ => { });
            var first = rig.Transport.Calls[0];
            Assert.True(rig.Answer(first, How.Config, new IOException()) is not null || (rust && first.Answer is not null), "not answered");
            Assert.True(Rig.Until(() => rig.Trace.Unended().Any(u => u.StartsWith("Store", StringComparison.Ordinal))), "no save after the remote config");
            rig.HeldElsewhere = 1;
            rig.Stop("a save and the goodbye held"); // within StopTimeout and a margin
            Assert.Contains(rig.Transport.Calls, c => c.OfKind > 0); // the goodbye, never answered
        }
        finally
        {
            hold.Set();
        }

        rig.HeldElsewhere = 0;
        rig.Finish("a save and the goodbye held");
    }

    private static GenOperation<Rig, Model> AnswerOp { get; } =
        (from pick in Gen.Int[0, 7]
         from how in Gen.Int[0, 6]
         from fault in Gen.Int[0, Faults.Count - 1]
         select (pick, how: (How)how, fault))
        .Operation<Rig, Model>(
            t => $"answer {t.pick} {t.how}{(t.how is How.Throw or How.BodyFault ? $" {Faults.Names[t.fault]}" : string.Empty)}",
            (r, t) => Act(r, $"answer {t.pick} {t.how} {t.fault}", () =>
            {
                var open = r.Transport.Calls.Where(c => c.Open).ToArray();
                if (open.Length == 0)
                {
                    return;
                }

                if (r.Answer(open[t.pick % open.Length], t.how, Faults.Make(t.fault)) is { } e)
                {
                    r.Expecting.Enqueue(e);
                }
            }),
            (m, _) => m.Absorb());

    private static GenOperation<Rig, Model> AdvanceOp { get; } =
        Gen.OneOfConst(1, 250, 1_000, 5_000, 10_001, 30_000, 61_000, 600_000)
        .Operation<Rig, Model>(
            ms => $"advance {ms} ms",
            (r, ms) => Act(r, $"advance {ms}", () => r.Time.Advance(TimeSpan.FromMilliseconds(ms))),
            (m, _) => m.Absorb());

    private static GenOperation<Rig, Model> CallOp { get; } =
        (from what in Gen.Int[0, 2]
         from elsewhere in Gen.Bool
         select (what, elsewhere))
        .Operation<Rig, Model>(
            t => $"call {t.what}{(t.elsewhere ? " from another thread" : string.Empty)}",
            (r, t) => Act(r, $"call {t.what}", () =>
            {
                var c = r.Client;
                var n = r.Transport.Calls.Length;
                Action call = t.what switch
                {
                    0 => () => c.SetHealth(new ComponentHealth { Healthy = n % 2 == 0, Status = $"s{n}" }),
                    1 => () => c.SetDescription(Description($"d{n}")),
                    _ => () => c.SetEffectiveConfig(Config($"c{n}")),
                };
                if (t.elsewhere)
                {
                    Task.Run(call).Wait();
                }
                else
                {
                    call();
                }
            }),
            (m, _) => m.Absorb());

    /// <summary>The server on the connection (WebSocket; over HTTP the server only answers).</summary>
    private static GenOperation<Rig, Model> ServerOp { get; } =
        (from what in Gen.Int[0, 8]
         from fault in Gen.Int[0, Faults.Count - 1]
         select (what, fault))
        .Operation<Rig, Model>(
            t => $"server {t.what}{(t.what is >= 4 and <= 6 ? $" {Faults.Names[t.fault]}" : string.Empty)}",
            (r, t) => Act(r, $"server {t.what} {t.fault}", () =>
            {
                if (r.Socket() is not { } s)
                {
                    return;
                }

                var uid = ByteString.CopyFrom(r.Client.InstanceUid);
                switch (t.what)
                {
                    case <= 2:
                        s.Push([0, .. r.Offer(t.what switch { 0 => How.Config, 1 => How.Package, _ => How.Ok }, uid)]);
                        break;
                    case 3:
                        s.PushClose(WebSocketCloseStatus.NormalClosure);
                        break;
                    case 4:
                        s.BreakReceive(Faults.Make(t.fault));
                        var call = r.Transport.Calls.First(c => ReferenceEquals(c.Socket, s));
                        if (!r.Rust)
                        {
                            r.Expecting.Enqueue(new Expected(CallKind.Connect, call.OfKind, Input.KindOneofCase.None, null, $"{call}, its receive broken"));
                        }
                        break;
                    case 5:
                        s.BreakSend(Faults.Make(t.fault));
                        break;
                    case 6:
                        s.BreakClose(Faults.Make(t.fault));
                        break;
                    case 7:
                        s.BreakClose(null); // never answered
                        break;
                    default:
                        // A remote config and, at once, another message: the second is queued
                        // for the loop while the callback answers the first, and the answer
                        // reaches the core before it (RULE-SHELL-10, the trace's check).
                        s.Push([0, .. r.Offer(How.Config, uid)]);
                        s.Push([0, .. r.Offer(How.Ok, uid)]);
                        break;
                }
            }),
            (m, _) => m.Absorb());

    private static GenOperation<Rig, Model> StopOp { get; } =
        Gen.Operation<Rig, Model>("stop", r => Act(r, "stop", () => r.Stop("the stop step")), m => m.Absorb());

    private static Rig Track(Rig rig)
    {
        Rigs.Add(rig);
        return rig;
    }

    /// <summary>One step of a schedule: nothing once stopped, or after <see cref="MaxOps"/>; then pace, and the rules that hold at every moment.</summary>
    private static void Act(Rig r, string name, Action action)
    {
        if (r.StopCalled || r.Ops >= MaxOps)
        {
            return;
        }

        r.Ops++;
        r.Note(name);
        _ = r.Client;
        action();
        r.Pace();
        r.CheckSafety($"after {name}");
    }

    private static void AnswerAtOnce(Call c)
    {
        if (c.Kind == CallKind.Post)
        {
            c.Respond(200, Rig.Plain(AgentToServer.Parser.ParseFrom(c.Body).InstanceUid).ToByteArray());
        }
    }

    private static AgentDescription Description(string value)
    {
        var d = new AgentDescription();
        d.NonIdentifyingAttributes.Add(new KeyValue { Key = "n", Value = new AnyValue { StringValue = value } });
        return d;
    }

    private static EffectiveConfig Config(string name)
    {
        var map = new AgentConfigMap();
        map.ConfigMap[name] = new AgentConfigObject { Body = ByteString.CopyFromUtf8(name) };
        return new EffectiveConfig { ConfigMap = map };
    }

    /// <summary>
    /// One client whose transport throws <paramref name="fault"/> at <paramref name="site"/>
    /// (everything else answered at once): the effect must end with its failure, without any timer.
    /// </summary>
    private static bool EndsWhateverIsThrown(Site site, int fault, bool sync)
    {
        Rig rig = null!;
        rig = Track(new Rig(site >= Site.Connect, 0, onCall: c => Throwing(rig, site, Faults.Make(fault), sync, c)));
        var (kind, end) = site switch
        {
            Site.PostSend or Site.PostBody => (CallKind.Post, Input.KindOneofCase.HttpFailed),
            Site.GetSend or Site.GetBody => (CallKind.Get, Input.KindOneofCase.DownloadFailed),
            Site.Connect => (CallKind.Connect, Input.KindOneofCase.WsFailed),
            _ => (CallKind.Connect, Input.KindOneofCase.WsLost),
        };
        var what = $"{site}, {Faults.Names[fault]}, {(sync ? "thrown as called" : "in the task")}";
        _ = rig.Client;
        if (site == Site.WsReceive)
        {
            // Once the first message has gone, so that no send of the client's ends the
            // connection for it: the receive's failure alone must.
            if (!Rig.Until(() => rig.Transport.Calls.FirstOrDefault()?.Socket is { } s && s.Sent.Count > 0))
            {
                throw new InvalidOperationException($"{what}: never connected{rig.Report()}");
            }

            rig.Pace();
            rig.Transport.Calls[0].Socket!.BreakReceive(Faults.Make(fault));
        }

        if (site == Site.WsClose)
        {
            // Closed by stop, without moving the clock: the close fails, and that ends it.
            if (!Rig.Until(() => rig.Trace.EffectOf(kind, 0) is { Opened: true }))
            {
                throw new InvalidOperationException($"{what}: never connected{rig.Report()}");
            }

            rig.Stops.Add(rig.Client.StopAsync());
        }

        if (!Rig.Until(() => rig.Trace.EffectOf(kind, 0) is { Ended: true } or { Cancelled: true }))
        {
            throw new InvalidOperationException($"{what}: the effect never ended (RULE-SHELL-05): {rig.Trace.EffectOf(kind, 0)?.ToString() ?? "never issued"}{rig.Report()}");
        }

        if (rig.Trace.EffectOf(kind, 0) is { Ended: true } fx && fx.End != end)
        {
            throw new InvalidOperationException($"{what}: ended with {fx.End}, not {end}{rig.Report()}");
        }

        rig.Finish(what);
        return true;
    }

    private static void Throwing(Rig rig, Site site, Exception fault, bool sync, Call c)
    {
        void Throw()
        {
            c.Fail(fault);
            if (sync)
            {
                throw fault;
            }
        }

        switch (c.Kind)
        {
            case CallKind.Post:
                var uid = AgentToServer.Parser.ParseFrom(c.Body).InstanceUid;
                var first = c.OfKind == 0;
                if (first && site == Site.PostSend)
                {
                    Throw();
                    return;
                }

                var body = rig.Offer(first && site is Site.GetSend or Site.GetBody ? How.Package : How.Ok, uid);
                c.Respond(200, body, bodyFault: first && site == Site.PostBody ? fault : null);
                break;
            case CallKind.Get when site == Site.GetSend:
                Throw();
                break;
            case CallKind.Get:
                c.Respond(200, rig.FileAt(c.Url), bodyFault: site == Site.GetBody ? fault : null);
                break;
            case CallKind.Connect when site == Site.Connect:
                Throw();
                break;
            default:
                var s = new MemSocket(rig.Transport) { FaultsAsync = !sync };
                switch (site)
                {
                    case Site.WsSend:
                        s.BreakSend(fault);
                        break;
                    case Site.WsClose:
                        s.BreakClose(fault);
                        break;
                }

                c.Accept(s);
                break;
        }
    }

    /// <summary>Run a property: the settings, the time it took, and how to replay a failure.</summary>
    private void Run(string name, Action<string?, long> property)
    {
        var seed = Seed;
        var iter = seed is null ? Runs : 1;
        var started = Stopwatch.StartNew();
        Rig.Stats.Clear();
        try
        {
            property(seed, iter);
        }
        catch (Exception e) when (e.GetType().Name == "CsCheckException")
        {
            var found = Regex.Match(e.Message, "Set seed: \"([^\"]+)\"");
            Assert.Fail($"{name}: {e.Message}\n\nreplay: OPAMP_SCHEDULE_SEED={(found.Success ? found.Groups[1].Value : "<seed>")} "
                + $"dotnet test Pydantic.OpAmp.Client.Tests --filter FullyQualifiedName~{nameof(ScheduleTests)}.{name}\n{e.InnerException}");
        }
        finally
        {
            foreach (var rig in Rigs)
            {
                rig.Abandon();
            }

            Rigs.Clear();
        }

        this.output.WriteLine($"{name}: {iter} iteration(s){(seed is null ? string.Empty : $" of seed {seed}")} in {started.Elapsed.TotalSeconds:0.0} s, "
            + $"on {Threads} threads (OPAMP_SCHEDULE_RUNS, OPAMP_SCHEDULE_SEED)");
        this.output.WriteLine(string.Join("\n", Rig.Stats.OrderBy(kv => kv.Key, StringComparer.Ordinal).Select(kv => $"  {kv.Key}: {kv.Value}")));
        Rig.Stats.Clear();
    }

    /// <summary>The model of a schedule: what each answer must become, unless cancelled first.</summary>
    private sealed class Model(Rig rig)
    {
        private readonly List<Expected> expected = new();

        /// <summary>Take what the step just done expects.</summary>
        public void Absorb()
        {
            while (rig.Expecting.TryDequeue(out var e))
            {
                this.expected.Add(e);
            }
        }

        public void Check()
        {
            this.Absorb();
            var problems = new List<string>();
            foreach (var e in this.expected)
            {
                if (rig.Trace.EffectOf(e.Kind, e.OfKind) is not { } fx)
                {
                    problems.Add($"{e.Call}: no effect issued for it");
                    continue;
                }

                if (fx.Cancelled && !fx.Ended)
                {
                    continue; // nothing more about it: the core gave up first
                }

                var call = rig.Transport.Calls.First(c => c.Kind == e.Kind && c.OfKind == e.OfKind);
                var ok = e.Input switch
                {
                    Input.KindOneofCase.WsOpened => fx.Opened,
                    Input.KindOneofCase.None => fx.Ended,

                    // The client's own deadline may have cut it short, answered or not.
                    _ when call.Ct.IsCancellationRequested && fx.Ended => true,
                    _ => fx.Ended && fx.End == e.Input && (e.Status is null || fx.EndStatus == e.Status),
                };
                if (!ok)
                {
                    problems.Add($"{e.Call}: the core should have heard {e.Input}{(e.Status is { } s ? $" ({s})" : string.Empty)}, but: {fx}");
                }
            }

            if (problems.Count > 0)
            {
                throw new InvalidOperationException($"the model: {string.Join("; ", problems)}{rig.Report()}");
            }
        }

        public override string ToString() => $"{this.expected.Count} expected: {string.Join(", ", this.expected.Select(e => $"{e.Kind} {e.OfKind} -> {e.Input}"))}";
    }

    /// <summary>The model of the parallel test: the calls before the first stop, in order.</summary>
    private sealed class Calls
    {
        public List<string> List { get; } = new();

        public bool Stopped { get; set; }

        public void Add(string call)
        {
            if (!this.Stopped)
            {
                this.List.Add(call);
            }
        }
    }
}

/// <summary>The schedule tests run on their own, not beside other tests.</summary>
[CollectionDefinition(nameof(ScheduleTests), DisableParallelization = true)]
public sealed class ScheduleTestsAlone;
