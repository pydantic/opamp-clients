// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Collections.Concurrent;
using System.Net;
using System.Net.Sockets;
using System.Net.WebSockets;
using FsCheck;
using FsCheck.Fluent;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// The callback contract (spec/shell-contract.md, RULE-SHELL-01 to -03 and -07), checked against a
/// model over random runs (FsCheck generates each run's plan from its seed), on the real core and a
/// local WebSocket server. A plan is a sequence of steps: start (on a fresh state file, or one
/// holding a remote config saved before, which the core hands over on the first step), with a
/// callback that throws on its k-th call or never; an application call from the test's thread,
/// from another thread or from inside the callback; the server offering a remote config, a custom
/// message or a heartbeat interval; stop from outside, or requested from inside the callback.
/// After each step the notifications seen so far are checked against the model: never two
/// callbacks at once, none after <c>Stopped</c>, <c>Stopped</c> once and last, with the error the
/// callback threw; <c>StopAsync</c> returns within its bound with that error, every time; what the
/// server offered reaches the callback in the order offered, unless the client stopped first.
/// Runs: OPAMP_MODEL_RUNS (default 30), seeds from a random base, printed on failure;
/// OPAMP_MODEL_SEED=S replays one run. In both modes: .NET's WebSocket, and Rust mode's (the core's
/// own, over the client's streams).
/// </summary>
/// <remarks>
/// Known breaks (spec/shell-contract.md), tolerated here and logged, since they are races that can't fail
/// deterministically: S5, a callback before <c>Start</c> returned (the client now holds every
/// callback until <c>Start</c> has returned; what is still counted is the test's own flag, set
/// just after). S1 (stop held by a WebSocket that opened after it) is fixed, and fails here.
/// </remarks>
public class CallbackPropertyTests
{
    private const string Capability = "io.example.model";

    // Stop's bound, and a call's from another thread: over WebSocket nothing is in flight at stop
    // but the goodbye.
    private static readonly TimeSpan Bound = TimeSpan.FromSeconds(5);

    // How long to wait for something that must come (a notification, a save, a connection): only
    // a failure takes this long. Longer than Bound, which a loaded CI runner passed before the
    // first client's remote config reached the callback ("priming: the remote config never
    // reached the callback", seed 1191801237318366256): the first start loads and compiles the
    // core's module.
    private static readonly TimeSpan Wait = TimeSpan.FromSeconds(30);

    private static readonly Gen<Step> StartGen =
        from saved in Gen.Elements(true, false)
        from throwAt in Gen.Choose(-6, 8) // never (0) about 40% of the time; else on call 1-8
        from kind in Gen.Choose(0, 2)
        select (Step)new StartStep(saved, Math.Max(0, throwAt), kind);

    private static readonly Gen<Step> AnyStep = Gen.OneOf(
        StartGen,
        from caller in Gen.Elements(Where.Outside, Where.Thread, Where.Inside)
        from what in Gen.Choose(0, 3)
        select (Step)new CallStep(caller, what),
        from what in Gen.Elements(Offer.Config, Offer.Custom, Offer.Heartbeat)
        select (Step)new OfferStep(what),
        from what in Gen.Elements(Offer.Config, Offer.Custom, Offer.Heartbeat)
        select (Step)new OfferStep(what),
        from inside in Gen.Elements(true, false)
        select (Step)new StopStep(inside));

    // A plan starts a client, then does 1-12 steps (more starts among them: restarts).
    private static readonly Gen<Step[]> Plans =
        from first in StartGen
        from length in Gen.Choose(1, 12)
        from rest in Gen.ArrayOf(AnyStep, length)
        select rest.Prepend(first).ToArray();

    private readonly ITestOutputHelper output;

    public CallbackPropertyTests(ITestOutputHelper output) => this.output = output;

    private enum Where
    {
        Outside,
        Thread,
        Inside,
    }

    private enum Offer
    {
        Config,
        Custom,
        Heartbeat,
    }

    // covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-07
    [Theory]
    [InlineData(OpAmpMode.DotNet)]
    [InlineData(OpAmpMode.Rust)]
    public async Task CallbacksKeepTheContractWhateverTheApplicationAndServerDo(OpAmpMode mode)
    {
        var runs = int.TryParse(Environment.GetEnvironmentVariable("OPAMP_MODEL_RUNS"), out var n) ? n : 30;
        var replay = ulong.TryParse(Environment.GetEnvironmentVariable("OPAMP_MODEL_SEED"), out var s) ? s : (ulong?)null;
        var first = replay ?? (ulong)Random.Shared.NextInt64(1, long.MaxValue / 2);
        var seeds = replay is not null ? [first] : Enumerable.Range(0, runs).Select(i => first + (ulong)i).ToArray();
        var tolerated = new List<string>();
        foreach (var seed in seeds)
        {
            var plan = Gen.Sample(Plans, 1, new Rnd(seed), 20)[0];
            var run = new Run(plan, mode);
            try
            {
                await run.ExecuteAsync();
            }
            catch (Exception e)
            {
                this.output.WriteLine(string.Join('\n', run.Log));
                Assert.Fail($"seed {seed} failed: {e.Message}\nplan: {string.Join(", ", plan.AsEnumerable())}\n"
                    + $"replay: OPAMP_MODEL_SEED={seed} dotnet test Pydantic.OpAmp.Client.Tests --filter CallbackPropertyTests ({mode} mode)\n{e}");
            }
            finally
            {
                await run.DisposeAsync();
            }

            tolerated.AddRange(run.Tolerated.Select(t => $"seed {seed}: {t}"));
        }

        foreach (var t in tolerated)
        {
            this.output.WriteLine(t);
        }

        this.output.WriteLine($"{mode} mode: {seeds.Length} runs from seed {first}; {tolerated.Count} tolerated known breaks");
    }

    /// <summary>
    /// The model server's teardown returns though the listener never completes an accept it began.
    /// HttpListener.Stop (managed, as on Linux and macOS) completes the accepts waiting, then marks
    /// the listener stopped: an accept begun between the two (the serve loop going round for the
    /// next connection as Stop runs) is never completed. The teardown waited for the serve loop,
    /// and the nightly's 5,000 runs hung there, idle, until the job's three hours were up.
    /// </summary>
    [Fact]
    public async Task TheModelServerTearsDownThoughTheListenerLosesAnAccept()
    {
        var lost = new TaskCompletionSource<HttpListenerContext>(); // never completed, as Stop leaves it
        var server = new ModelServer(_ => lost.Task);
        var teardown = server.DisposeAsync().AsTask();
        Assert.True(
            await Task.WhenAny(teardown, Task.Delay(Bound)) == teardown,
            $"the model server's teardown didn't return within {Bound.TotalSeconds} s of an accept the listener lost");
        await teardown;
    }

    private static async Task<bool> UntilAsync(Func<bool> condition)
    {
        var deadline = DateTime.UtcNow + Wait;
        while (!condition())
        {
            if (DateTime.UtcNow > deadline)
            {
                return false;
            }

            await Task.Delay(2);
        }

        return true;
    }

    private abstract record Step;

    /// <summary>Start a client (if none is running): on a state file with a saved remote config or a fresh one; its callback throws on call <c>ThrowAt</c> (0: never).</summary>
    private sealed record StartStep(bool Saved, int ThrowAt, int ThrowKind) : Step;

    /// <summary>An application call, from the test's thread, another thread or the callback.</summary>
    private sealed record CallStep(Where From, int What) : Step;

    /// <summary>The server offers something over the client's connection.</summary>
    private sealed record OfferStep(Offer What) : Step;

    /// <summary>Stop from outside, or requested from inside the callback.</summary>
    private sealed record StopStep(bool Inside) : Step;

    private sealed class ModelException(string message) : Exception(message);

    /// <summary>One client, and what its callback saw: the callback model (also ScheduleTests's).</summary>
    internal sealed class Agent(int throwAt, int throwKind, int connsBefore)
    {
        private readonly object gate = new();
        private readonly List<OpAmpNotification> seen = new();
        private readonly List<string> violations = new();
        private readonly List<(string What, Func<OpAmpNotification, bool> Is)> expected = new();
        private int inProgress;
        private int calls;
        private int stopped;
        private OpAmpClient? handle;
        private int startReturned;

        public int ConnsBefore { get; } = connsBefore;

        public ConcurrentQueue<Action<OpAmpClient>> Inside { get; } = new();

        /// <summary>Told each thing the callback asks of the client from inside (the message it hands over), just before it asks.</summary>
        public Action<Google.Protobuf.IMessage>? Asked { get; set; }

        public Exception? Thrown { get; private set; }

        public bool ThrewOnStopped { get; private set; }

        public Task? StopFromInside { get; set; }

        public int Early { get; private set; }

        public byte[]? Outstanding { get; set; }

        public bool StopCalled { get; set; }

        public OpAmpClient Handle => Volatile.Read(ref this.handle)!;

        public bool IsStopped => Volatile.Read(ref this.stopped) > 0;

        public void Started(OpAmpClient client)
        {
            Volatile.Write(ref this.handle, client);
            Volatile.Write(ref this.startReturned, 1);
        }

        public void Expect(string what, Func<OpAmpNotification, bool> isIt)
        {
            lock (this.gate)
            {
                this.expected.Add((what, isIt));
            }
        }

        public bool Saw(Func<OpAmpNotification, bool> isIt)
        {
            lock (this.gate)
            {
                return this.seen.Any(isIt);
            }
        }

        public OpAmpNotification[] Seen()
        {
            lock (this.gate)
            {
                return this.seen.ToArray();
            }
        }

        /// <summary>The callback.</summary>
        public void Notify(OpAmpNotification n)
        {
            if (Interlocked.Increment(ref this.inProgress) > 1)
            {
                this.Violation($"two callbacks at once ({n})");
            }

            try
            {
                if (Volatile.Read(ref this.startReturned) == 0)
                {
                    this.Early++; // S5: tolerated, logged
                }

                if (this.IsStopped)
                {
                    this.Violation($"a callback after Stopped: {n}");
                }

                lock (this.gate)
                {
                    this.seen.Add(n);
                }

                // Widen the window for a second callback to overlap this one.
                Thread.SpinWait(200);
                var call = ++this.calls;
                if (n is OpAmpNotification.Stopped)
                {
                    Interlocked.Increment(ref this.stopped);
                }

                if (call == throwAt)
                {
                    this.ThrewOnStopped = n is OpAmpNotification.Stopped;
                    this.Thrown = throwKind switch
                    {
                        0 => new InvalidOperationException($"the application's bug, on call {call}"),
                        1 => new ModelException($"the model's bug, on call {call}"),
                        _ => new AggregateException(new ArgumentException($"an argument, on call {call}")),
                    };
                    throw this.Thrown;
                }

                var client = Volatile.Read(ref this.handle);
                if (client is null)
                {
                    return; // S5: the handle isn't there yet; what is due from inside waits
                }

                if (n is OpAmpNotification.RemoteConfig rc)
                {
                    // Answered from inside, through the handle.
                    var answer = new RemoteConfigStatus
                    {
                        LastRemoteConfigHash = rc.Config.ConfigHash, Status = RemoteConfigStatuses.Applied,
                    };
                    this.Asked?.Invoke(answer);
                    client.SetRemoteConfigStatus(answer);
                    this.Outstanding = null;
                }

                while (this.Inside.TryDequeue(out var action))
                {
                    action(client);
                }
            }
            catch (Exception e) when (!ReferenceEquals(e, this.Thrown))
            {
                this.Violation($"a call from inside the callback threw: {e}");
            }
            finally
            {
                Interlocked.Decrement(ref this.inProgress);
            }
        }

        public void Violation(string what)
        {
            lock (this.gate)
            {
                this.violations.Add(what);
            }
        }

        /// <summary>The model, against what was seen so far.</summary>
        public void Check(string who)
        {
            lock (this.gate)
            {
                if (this.violations.Count > 0)
                {
                    throw new InvalidOperationException($"{who}: {string.Join("; ", this.violations)}");
                }

                var stops = this.seen.OfType<OpAmpNotification.Stopped>().ToArray();
                if (stops.Length > 1)
                {
                    throw new InvalidOperationException($"{who}: Stopped {stops.Length} times: {string.Join(", ", stops.AsEnumerable())}");
                }

                if (stops.Length == 1)
                {
                    if (this.seen[^1] is not OpAmpNotification.Stopped)
                    {
                        throw new InvalidOperationException($"{who}: Stopped isn't last: {string.Join(", ", this.seen)}");
                    }

                    var error = stops[0].Error;
                    var want = this.ThrewOnStopped ? null : this.Thrown;
                    if (!ReferenceEquals(error, want) && !(this.ThrewOnStopped && ReferenceEquals(error, this.Thrown)))
                    {
                        throw new InvalidOperationException($"{who}: Stopped with {error?.ToString() ?? "no error"}, but the callback threw {want?.ToString() ?? "nothing"}");
                    }
                }

                // What the server offered, in the order offered.
                var at = -1;
                foreach (var (what, isIt) in this.expected)
                {
                    var i = this.seen.FindIndex(x => isIt(x));
                    if (i < 0)
                    {
                        continue; // checked when it was offered: the client stopped first
                    }

                    if (i < at)
                    {
                        throw new InvalidOperationException($"{who}: {what} came out of order: {string.Join(", ", this.seen)}");
                    }

                    at = i;
                }
            }
        }
    }

    /// <summary>One run of a plan: its server, its state file, the clients it started.</summary>
    private sealed class Run(Step[] plan, OpAmpMode mode) : IAsyncDisposable
    {
        private readonly ModelServer server = new();
        private readonly string dir = Directory.CreateTempSubdirectory("opamp-model-").FullName;
        private readonly List<Agent> agents = new();
        private byte[]? primed;
        private ByteString? primedHash;
        private int counter;

        public List<string> Log { get; } = new();

        public List<string> Tolerated { get; } = new();

        private string StateFile => Path.Combine(this.dir, "state");

        private Agent? Current => this.agents.Count > 0 ? this.agents[^1] : null;

        public async Task ExecuteAsync()
        {
            for (var i = 0; i < plan.Length; i++)
            {
                this.Log.Add($"step {i}: {plan[i]}");
                await (plan[i] switch
                {
                    StartStep s => this.StartAsync(s),
                    CallStep c => this.CallAsync(c),
                    OfferStep o => this.OfferAsync(o.What),
                    StopStep st => this.StopAsync(st.Inside),
                    _ => Task.CompletedTask,
                });
                this.CheckAll($"after step {i} ({plan[i]})");
            }

            // Every client is stopped in the end, and stop says the same every time.
            foreach (var a in this.agents)
            {
                await this.StopAndCheckAsync(a, a.Handle.StopAsync(), "at the end");
            }

            this.CheckAll("at the end");
            foreach (var a in this.agents.Where(a => a.Early > 0))
            {
                this.Tolerated.Add($"S5: {a.Early} callback(s) before Start returned");
            }
        }

        public async ValueTask DisposeAsync()
        {
            await this.server.DisposeAsync();
            try
            {
                Directory.Delete(this.dir, recursive: true);
            }
            catch (IOException)
            {
            }
        }

        private void CheckAll(string when)
        {
            for (var i = 0; i < this.agents.Count; i++)
            {
                this.agents[i].Check($"client {i}, {when}");
            }
        }

        private OpAmpClient StartClient(Agent a) => OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Endpoint = new Uri(this.server.Endpoint),
                StateFile = this.StateFile,
                Mode = mode,
                DirectConnection = mode == OpAmpMode.Rust,
                Capabilities = (ulong)(AgentCapabilities.AcceptsRemoteConfig | AgentCapabilities.ReportsRemoteConfig
                    | AgentCapabilities.ReportsHealth | AgentCapabilities.ReportsEffectiveConfig),
            },
            new OpAmpStatus
            {
                Description = Description(0),
                Health = new ComponentHealth { Healthy = true, Status = "starting" },
                EffectiveConfig = new EffectiveConfig(),
                CustomCapabilities = new CustomCapabilities { Capabilities = { Capability } },
            },
            a.Notify);

        private static AgentDescription Description(int n)
        {
            var d = new AgentDescription();
            d.NonIdentifyingAttributes.Add(new KeyValue { Key = "n", Value = new AnyValue { IntValue = n } });
            return d;
        }

        /// <summary>A state file holding a remote config the application hasn't answered: one client takes one, and stops.</summary>
        private async Task PrimeAsync()
        {
            if (this.primed is not null)
            {
                return;
            }

            File.Delete(this.StateFile);
            var a = new Agent(0, 0, this.server.Accepted);
            a.Started(this.StartClient(a));
            var hash = ByteString.CopyFromUtf8($"primed-{this.counter++}");
            if (!await this.server.PushAsync(a.ConnsBefore, ConfigOffer(hash), () => false)
                || !await UntilAsync(() => a.Saw(n => n is OpAmpNotification.RemoteConfig rc && rc.Config.ConfigHash == hash)))
            {
                throw new InvalidOperationException("priming: the remote config never reached the callback");
            }

            // Saved: saves run beside the loop, one at a time, and the core asks for none once
            // stopping, so a stop right after the offer could leave it unsaved (as a crash would).
            if (!await UntilAsync(() => Holds(this.StateFile, hash)))
            {
                throw new InvalidOperationException("priming: the remote config was never saved");
            }

            await this.StopAndCheckAsync(a, a.Handle.StopAsync(), "priming");
            this.primed = File.ReadAllBytes(this.StateFile);
            this.primedHash = hash;
        }

        /// <summary>Whether the state file at <paramref name="path"/> holds <paramref name="hash"/>.</summary>
        private static bool Holds(string path, ByteString hash)
        {
            try
            {
                return File.ReadAllBytes(path).AsSpan().IndexOf(hash.Span) >= 0;
            }
            catch (IOException)
            {
                return false;
            }
        }

        private static ServerToAgent ConfigOffer(ByteString hash)
        {
            var config = new AgentConfigMap();
            config.ConfigMap[string.Empty] = new AgentConfigObject { Body = hash, ContentType = "text/plain" };
            return new ServerToAgent { RemoteConfig = new AgentRemoteConfig { Config = config, ConfigHash = hash } };
        }

        private async Task StartAsync(StartStep s)
        {
            if (this.Current is { IsStopped: false })
            {
                return; // one client at a time
            }

            // A client stopped before may still be finishing a save (stop doesn't wait for one
            // already writing): the file is the test's only once that has landed, as a client
            // started on it would wait.
            global::Pydantic.OpAmp.Client.StateFile.Settled(this.StateFile);
            if (s.Saved)
            {
                await this.PrimeAsync();
                global::Pydantic.OpAmp.Client.StateFile.Settled(this.StateFile);
                File.WriteAllBytes(this.StateFile, this.primed!);
            }
            else
            {
                File.Delete(this.StateFile);
            }

            var a = new Agent(s.ThrowAt, s.ThrowKind, this.server.Accepted);
            this.agents.Add(a);
            var hash = this.primedHash;
            if (s.Saved)
            {
                // Handed over on the first step.
                a.Outstanding = hash!.ToByteArray();
                a.Expect("the saved remote config", n => n is OpAmpNotification.RemoteConfig rc && rc.Config.ConfigHash == hash);
            }

            a.Started(this.StartClient(a));

            // Started means connected (or stopped by the callback first): a stop while the
            // upgrade is unanswered is S1, which this test doesn't aim at.
            if (!await UntilAsync(() => a.IsStopped || a.Saw(n => n is OpAmpNotification.Connected)))
            {
                throw new InvalidOperationException("not connected, nor stopped, within the bound");
            }

            if (s.Saved && !await UntilAsync(() => a.IsStopped || a.Saw(n => n is OpAmpNotification.RemoteConfig rc && rc.Config.ConfigHash == hash)))
            {
                throw new InvalidOperationException("the saved remote config wasn't handed over");
            }
        }

        private async Task CallAsync(CallStep c)
        {
            if (this.Current is not { } a)
            {
                return;
            }

            var n = ++this.counter;
            Action<OpAmpClient> call = c.What switch
            {
                0 => h => h.SetHealth(new ComponentHealth { Healthy = n % 2 == 0, Status = $"s{n}" }),
                1 => h => h.SetDescription(Description(n)),
                2 => h => h.SendCustomMessage(new CustomMessage { Capability = Capability, Type = "t", Data = ByteString.CopyFromUtf8($"{n}") }),
                _ => h => h.SetEffectiveConfig(new EffectiveConfig { ConfigMap = new AgentConfigMap() }),
            };
            switch (c.From)
            {
                case Where.Outside:
                    call(a.Handle); // after stop too: it does nothing
                    break;
                case Where.Thread:
                    Exception? failed = null;
                    var t = new Thread(() =>
                    {
                        try
                        {
                            call(a.Handle);
                        }
                        catch (Exception e)
                        {
                            failed = e;
                        }
                    });
                    t.Start();
                    if (!t.Join(Bound) || failed is not null)
                    {
                        throw new InvalidOperationException($"a call from another thread failed: {failed}");
                    }

                    break;
                case Where.Inside when !a.IsStopped:
                    var ran = false;
                    a.Inside.Enqueue(h =>
                    {
                        call(h);
                        ran = true;
                    });
                    await this.TriggerAsync(a);
                    if (!await UntilAsync(() => Volatile.Read(ref ran) || a.IsStopped))
                    {
                        throw new InvalidOperationException("the call due from inside the callback never ran");
                    }

                    break;
            }
        }

        /// <summary>A custom message from the server, so the callback is called.</summary>
        private async Task TriggerAsync(Agent a) => await this.OfferAsync(Offer.Custom);

        private async Task OfferAsync(Offer what)
        {
            if (this.Current is not { IsStopped: false } a)
            {
                return;
            }

            var n = ++this.counter;
            ServerToAgent msg;
            Func<OpAmpNotification, bool> isIt;
            switch (what)
            {
                case Offer.Config:
                    if (a.Outstanding is { } h)
                    {
                        // The application is busy with the last config until it answers.
                        a.Handle.SetRemoteConfigStatus(new RemoteConfigStatus
                        {
                            LastRemoteConfigHash = ByteString.CopyFrom(h), Status = RemoteConfigStatuses.Applied,
                        });
                    }

                    var hash = ByteString.CopyFromUtf8($"config-{n}");
                    msg = ConfigOffer(hash);
                    isIt = x => x is OpAmpNotification.RemoteConfig rc && rc.Config.ConfigHash == hash;
                    a.Outstanding = hash.ToByteArray();
                    break;
                case Offer.Custom:
                    var data = ByteString.CopyFromUtf8($"custom-{n}");
                    msg = new ServerToAgent { CustomMessage = new CustomMessage { Capability = Capability, Type = "m", Data = data } };
                    isIt = x => x is OpAmpNotification.CustomMessage cm && cm.Message.Data == data;
                    break;
                default:
                    var seconds = (ulong)(100 + n);
                    msg = new ServerToAgent
                    {
                        ConnectionSettings = new ConnectionSettingsOffers { Opamp = new OpAMPConnectionSettings { HeartbeatIntervalSeconds = seconds } },
                    };
                    isIt = x => x is OpAmpNotification.HeartbeatInterval hb && hb.Interval == TimeSpan.FromSeconds(seconds);
                    break;
            }

            a.Expect($"{what} {n}", isIt);
            if (!await this.server.PushAsync(a.ConnsBefore, msg, () => a.IsStopped))
            {
                if (a.IsStopped)
                {
                    return;
                }

                throw new InvalidOperationException($"no connection to offer {what} on");
            }

            if (!await UntilAsync(() => a.IsStopped || a.Saw(isIt)))
            {
                throw new InvalidOperationException($"{what} {n} was offered, and never reached the callback");
            }
        }

        private async Task StopAsync(bool inside)
        {
            if (this.Current is not { } a)
            {
                return;
            }

            if (inside && !a.IsStopped)
            {
                a.Inside.Enqueue(h =>
                {
                    var called = DateTime.UtcNow;
                    a.StopFromInside = h.StopAsync(); // requested, not waited for
                    if (DateTime.UtcNow - called > TimeSpan.FromSeconds(1))
                    {
                        a.Violation("StopAsync, called from the callback, didn't return at once");
                    }
                });
                await this.TriggerAsync(a);
                if (!await UntilAsync(() => a.StopFromInside is not null || a.IsStopped))
                {
                    throw new InvalidOperationException("the stop due from inside the callback never ran");
                }

                if (a.StopFromInside is { } t)
                {
                    await this.StopAndCheckAsync(a, t, "stop requested from inside");
                    return;
                }
            }

            await this.StopAndCheckAsync(a, a.Handle.StopAsync(), "stop from outside");
        }

        /// <summary>Stop returns within its bound, with the error the callback threw, every time.</summary>
        private async Task StopAndCheckAsync(Agent a, Task stopping, string how)
        {
            // S1 is fixed: a WebSocket that opens after stop no longer holds StopAsync.
            if (await Task.WhenAny(stopping, Task.Delay(Bound)) != stopping)
            {
                throw new InvalidOperationException($"{how}: StopAsync didn't return within {Bound.TotalSeconds} s");
            }

            var got = stopping.IsFaulted ? stopping.Exception!.InnerException : null;
            var okay = ReferenceEquals(got, a.Thrown) || (a.ThrewOnStopped && got is null);
            if (!okay)
            {
                throw new InvalidOperationException($"{how}: StopAsync gave {got?.ToString() ?? "no error"}, but the callback threw {a.Thrown?.ToString() ?? "nothing"}");
            }

            if (!a.IsStopped)
            {
                throw new InvalidOperationException($"{how}: StopAsync returned before Stopped was told");
            }

            if (!a.StopCalled)
            {
                a.StopCalled = true;
                await this.StopAndCheckAsync(a, a.Handle.StopAsync(), how + ", again");
            }
        }
    }

    /// <summary>
    /// The OpAMP server: takes WebSocket connections (and says nothing back), and pushes what it is
    /// given over the newest connection opened since a point.
    /// </summary>
    private sealed class ModelServer : IAsyncDisposable
    {
        private readonly HttpListener listener;
        private readonly List<Conn> conns = new();
        private readonly Task serving;
        private readonly Func<HttpListener, Task<HttpListenerContext>> accept;
        private readonly CancellationTokenSource stopping = new();
        private int disposed;

        /// <param name="accept">How the next connection is taken from the listener (a test's, to lose one).</param>
        public ModelServer(Func<HttpListener, Task<HttpListenerContext>>? accept = null)
        {
            this.accept = accept ?? (l => l.GetContextAsync());
            (this.listener, var port) = LoopbackListener.Start();
            this.Endpoint = $"ws://127.0.0.1:{port}/v1/opamp";
            this.serving = Task.Run(this.ServeAsync);
        }

        public string Endpoint { get; }

        public int Accepted
        {
            get
            {
                lock (this.conns)
                {
                    return this.conns.Count;
                }
            }
        }

        /// <summary>Send <paramref name="msg"/> over the newest connection accepted from index <paramref name="from"/> on.</summary>
        public async Task<bool> PushAsync(int from, ServerToAgent msg, Func<bool> giveUp)
        {
            Conn? c = null;
            await UntilAsync(() => (c = this.Newest(from)) is not null || giveUp());
            if (c is null)
            {
                return false;
            }

            return await c.SendAsync(new byte[] { 0 }.Concat(msg.ToByteArray()).ToArray());
        }

        public void AbortFrom(int from)
        {
            lock (this.conns)
            {
                foreach (var c in this.conns.Skip(from))
                {
                    c.Abort();
                }
            }
        }

        public async ValueTask DisposeAsync()
        {
            // Once: a second Stop on a listener already closed throws (ObjectDisposedException, in
            // the nightly's 5,000 runs: the teardown failed a run whose checks had all passed).
            if (Interlocked.Exchange(ref this.disposed, 1) != 0)
            {
                return;
            }

            try
            {
                this.listener.Stop();
            }
            catch (ObjectDisposedException)
            {
                // Closed under us (a request's failure took the listener down): nothing to stop.
            }

            // Stop completes only the accepts already waiting when it clears them: one the serve
            // loop began as Stop ran (between the clearing and the listener marked stopped) is
            // never completed, and the loop would wait on it for ever. The loop gives up on it here.
            this.stopping.Cancel();
            this.AbortFrom(0);
            await this.serving;
            LoopbackListener.Close(this.listener);
            this.stopping.Dispose();
        }

        private Conn? Newest(int from)
        {
            lock (this.conns)
            {
                return this.conns.Skip(from).LastOrDefault(c => c.Open);
            }
        }

        private async Task ServeAsync()
        {
            while (this.listener.IsListening)
            {
                HttpListenerContext ctx;
                try
                {
                    ctx = await this.accept(this.listener).WaitAsync(this.stopping.Token);
                }
                catch (Exception e) when (e is HttpListenerException or ObjectDisposedException or InvalidOperationException
                    or OperationCanceledException)
                {
                    return;
                }

                if (!ctx.Request.IsWebSocketRequest)
                {
                    ctx.Response.StatusCode = 404;
                    ctx.Response.Close();
                    continue;
                }

                try
                {
                    var ws = (await ctx.AcceptWebSocketAsync(null)).WebSocket;
                    var c = new Conn(ctx, ws);
                    lock (this.conns)
                    {
                        this.conns.Add(c);
                    }

                    _ = Task.Run(c.ReadAsync);
                }
                catch (Exception e) when (e is WebSocketException or HttpListenerException or IOException or ObjectDisposedException)
                {
                    // the client went
                }
            }
        }

        private sealed class Conn(HttpListenerContext ctx, WebSocket ws)
        {
            private readonly SemaphoreSlim sending = new(1, 1);
            private volatile bool open = true;

            public bool Open => this.open;

            public async Task ReadAsync()
            {
                var buf = new byte[65536];
                try
                {
                    while (true)
                    {
                        var r = await ws.ReceiveAsync(buf, CancellationToken.None);
                        if (r.MessageType == WebSocketMessageType.Close)
                        {
                            this.open = false;
                            await ws.CloseOutputAsync(WebSocketCloseStatus.NormalClosure, string.Empty, CancellationToken.None);
                            return;
                        }
                    }
                }
                catch (Exception e) when (e is WebSocketException or IOException or ObjectDisposedException or HttpListenerException)
                {
                    // the client went
                }
                finally
                {
                    this.Abort();
                }
            }

            public async Task<bool> SendAsync(byte[] data)
            {
                await this.sending.WaitAsync();
                try
                {
                    await ws.SendAsync(data, WebSocketMessageType.Binary, true, CancellationToken.None);
                    return true;
                }
                catch (Exception e) when (e is WebSocketException or IOException or ObjectDisposedException or HttpListenerException or InvalidOperationException
                    or OperationCanceledException)
                {
                    // (OperationCanceledException: what ManagedWebSocket makes of a write to a socket the
                    // client has closed, "Broken pipe", when the close and the write cross.)
                    return false;
                }
                finally
                {
                    this.sending.Release();
                }
            }

            public void Abort()
            {
                this.open = false;
                ws.Abort();
                ctx.Response.Abort();
            }
        }
    }
}
