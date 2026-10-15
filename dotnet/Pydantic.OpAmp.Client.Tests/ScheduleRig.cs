// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Collections.Concurrent;
using System.Diagnostics;
using System.Net.WebSockets;
using System.Security.Cryptography;
using System.Text;
using Google.Protobuf;
using Microsoft.Extensions.Time.Testing;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Core;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// What crossed the core boundary (the client's <see cref="Seams.Stepped"/>), checked as it goes
/// against the effects rules of the shell contract: every input about an effect names one the core
/// issued, of its kind (RULE-SHELL-04); nothing after its end, at most one end; nothing after its
/// cancel (RULE-SHELL-06); no step after the output that said stopped (RULE-SHELL-07); what a
/// callback asked of the client (<see cref="Asked"/>) before anything the client took in after the
/// step that called it, but the application's calls queued ahead of it (RULE-SHELL-10). At the end,
/// every effect ended or was cancelled (RULE-SHELL-05). In Rust mode the boundary is the shell's:
/// each stream's inputs only as the stream's state allows (the proxy's answer once, the resolution
/// once, opened once, TLS once asked for, bytes and the end only while open, a splice's end only
/// after it, a failure once), nothing about a stream after the core closed it (RULE-SHELL-06), and
/// each download_done answered once, unless cancelled; at the end every stream closed or failed.
/// </summary>
internal sealed class Trace(bool rust = false)
{
    // Rust mode: each stream's state (asking, resolving, opening, tls, open, eof; answered,
    // resolved, failed, closed, let go: still asking when the core stopped), the splices it has
    // pending, and the downloads the core cancelled or discarded (their files must go).
    private readonly Dictionary<ulong, string> streams = new();
    private readonly Dictionary<ulong, int> splices = new();
    private readonly HashSet<ulong> discarded = new();
    private readonly Dictionary<string, int> streamTally = new();

    private readonly object gate = new();
    private readonly Dictionary<ulong, Fx> effects = new();
    private readonly Dictionary<CallKind, List<ulong>> issued = new()
    {
        [CallKind.Post] = new(), [CallKind.Get] = new(), [CallKind.Connect] = new(),
    };

    private readonly List<string> violations = new();
    private readonly List<string> log = new();
    private readonly List<string> app = new();

    // What a callback asked of the client that the core has not been fed yet, oldest first: the
    // message each call handed the client.
    private readonly List<IMessage> asked = new();
    private int count;
    private bool appStopped;

    /// <summary>How many of the callback's calls reached the core.</summary>
    public int Answered { get; private set; }

    /// <summary>How many times the server's Close crossed the core's ws_close (tolerated).</summary>
    public int CrossedCloses { get; private set; }

    /// <summary>How many steps so far.</summary>
    public int Count => Volatile.Read(ref this.count);

    /// <summary>Whether an output said stopped.</summary>
    public bool Stopped { get; private set; }

    /// <summary>Whether the core has taken the application's stop.</summary>
    public bool StopTaken
    {
        get
        {
            lock (this.gate)
            {
                return this.appStopped;
            }
        }
    }

    /// <summary>The application's calls that reached the core before its first stop, in order.</summary>
    public string[] App
    {
        get
        {
            lock (this.gate)
            {
                return this.app.ToArray();
            }
        }
    }

    public string[] Violations
    {
        get
        {
            lock (this.gate)
            {
                return this.violations.ToArray();
            }
        }
    }

    public string Log
    {
        get
        {
            lock (this.gate)
            {
                return string.Join("\n", this.log.TakeLast(80));
            }
        }
    }

    /// <summary>While set and not signalled, the loop waits here after each step (before its effects are carried out).</summary>
    public ManualResetEventSlim? HoldLoop { get; set; }

    /// <summary>Whether the loop is waiting on <see cref="HoldLoop"/>.</summary>
    public bool Holding { get; private set; }

    /// <summary>
    /// The callback has asked the client for something, from inside: <paramref name="call"/> is
    /// the message it handed over (the input the core is fed carries that very object).
    /// </summary>
    public void Asked(IMessage call)
    {
        lock (this.gate)
        {
            this.asked.Add(call);
        }
    }

    public void OnStep(Queued q, Output o)
    {
        if (this.HoldLoop is { } hold)
        {
            this.Holding = true;
            hold.Wait(TimeSpan.FromSeconds(30));
            this.Holding = false;
        }

        lock (this.gate)
        {
            this.count++;
            this.log.Add($"step {this.count}: {Describe(q)} -> [{string.Join(", ", o.Effects.Select(Describe))}]{(o.Stopped ? " stopped" : string.Empty)}");
            if (this.Stopped)
            {
                this.Violation($"a step after the output that said stopped (RULE-SHELL-07): {q.Input.KindCase}");
            }

            if (this.asked.Count > 0)
            {
                // What a callback asked comes before anything the client takes in next: until the
                // core has it, it is fed only the application's calls (those queued ahead of it),
                // and in Rust mode the shell's own answer to a proxy question of that step.
                var call = Call(q.Input);
                if (call is not null)
                {
                    this.Answered += this.asked.RemoveAll(a => ReferenceEquals(a, call));
                }
                else if (q.Input.KindCase is not (Input.KindOneofCase.Stop or Input.KindOneofCase.ProxyAnswer))
                {
                    this.Violation($"{Describe(q)} reached the core before what the callback asked (RULE-SHELL-10): {string.Join(", ", this.asked.Select(a => a.GetType().Name))}");
                    this.asked.Clear();
                }
            }

            if (q.Of is { } id && rust && q.Registry is not null)
            {
                this.StreamResult(id, q.Input, q.Final);
            }
            else if (q.Of is { } of)
            {
                this.Result(of, q.Input, q.Final);
            }
            else
            {
                this.Application(q.Input);
            }

            foreach (var e in o.Effects)
            {
                this.Issue(e);
            }

            this.Stopped |= o.Stopped;
            if (o.Stopped && rust)
            {
                // A proxy question whose answer (given at once) is still queued behind the step
                // that stopped: the shell lets go of a stream it never opened without a word, and
                // no step comes for the answer.
                foreach (var asking in this.streams.Where(kv => kv.Value == "asking").Select(kv => kv.Key).ToArray())
                {
                    this.streams[asking] = "let go";
                }
            }
        }
    }

    /// <summary>The effect the <paramref name="n"/>-th call of <paramref name="kind"/> carries out, if issued.</summary>
    public Fx? EffectOf(CallKind kind, int n)
    {
        lock (this.gate)
        {
            var ids = this.issued[kind];
            return n < ids.Count ? this.effects[ids[n]] : null;
        }
    }

    /// <summary>How many effects of <paramref name="kind"/> were issued.</summary>
    public int Issued(CallKind kind)
    {
        lock (this.gate)
        {
            return this.issued[kind].Count;
        }
    }

    /// <summary>The effects that neither ended nor were cancelled (RULE-SHELL-05); in Rust mode, the streams neither closed nor failed.</summary>
    public string[] Unended()
    {
        lock (this.gate)
        {
            return this.effects.Values.Where(f => !f.Ended && !f.Cancelled).Select(f => f.ToString())
                .Concat(this.streams.Where(kv => kv.Value is not ("closed" or "failed" or "answered" or "resolved" or "let go")).Select(kv => $"stream {kv.Key}, {kv.Value}"))
                .ToArray();
        }
    }

    /// <summary>What happened to the effects, for the statistics: issued, ended and cancelled, by kind.</summary>
    public IEnumerable<string> Tally()
    {
        lock (this.gate)
        {
            foreach (var fx in this.effects.Values)
            {
                yield return $"{fx.Kind} issued";
                if (fx.Cancelled)
                {
                    yield return $"{fx.Kind} cancelled";
                }

                if (fx.End is { } end)
                {
                    yield return $"{fx.Kind} ended {end}";
                }
            }

            foreach (var (what, n) in this.streamTally)
            {
                yield return n == 1 ? what : $"{what} (x{(n < 5 ? "2-4" : "5+")})";
            }

            yield return $"steps {(this.count < 10 ? "<10" : this.count < 30 ? "10-29" : "30+")}";
            if (this.CrossedCloses > 0)
            {
                yield return "the server's Close crossed ws_close (tolerated)";
            }

            if (this.Answered > 0)
            {
                yield return "a call from the callback reached the core (RULE-SHELL-10)";
            }
        }
    }

    /// <summary>Downloads the core cancelled before they ended (Rust mode: or discarded): their files must go.</summary>
    public ulong[] CancelledDownloads()
    {
        lock (this.gate)
        {
            return rust
                ? [.. this.discarded]
                : this.effects.Values.Where(f => f.Kind == Effect.KindOneofCase.Download && f.Cancelled).Select(f => f.Id).ToArray();
        }
    }

    /// <summary>The message one of the application's calls handed the client, if <paramref name="i"/> is one (but stop).</summary>
    private static IMessage? Call(Input i) => i.KindCase switch
    {
        Input.KindOneofCase.SetDescription => i.SetDescription,
        Input.KindOneofCase.SetHealth => i.SetHealth,
        Input.KindOneofCase.SetEffectiveConfig => i.SetEffectiveConfig,
        Input.KindOneofCase.SetCustomCapabilities => i.SetCustomCapabilities,
        Input.KindOneofCase.SendCustomMessage => i.SendCustomMessage,
        Input.KindOneofCase.SetAvailableComponents => i.SetAvailableComponents,
        Input.KindOneofCase.SetRemoteConfigStatus => i.SetRemoteConfigStatus,
        Input.KindOneofCase.SetConnectionSettingsStatus => i.SetConnectionSettingsStatus,
        Input.KindOneofCase.PackageInstalled => i.PackageInstalled,
        Input.KindOneofCase.PackageRemoved => i.PackageRemoved,
        _ => null,
    };

    private static string Describe(Queued q) =>
        q.Of is { } id ? $"{q.Input.KindCase}({id}{(q.Final ? ", final" : string.Empty)}{(q.Input.KindCase == Input.KindOneofCase.StreamData ? $", {q.Input.StreamData.Data.Length} B" : string.Empty)})" : q.Input.KindCase.ToString();

    private static string Describe(Effect e) => e.KindCase switch
    {
        Effect.KindOneofCase.HttpRequest => $"http_request {e.HttpRequest.Id}",
        Effect.KindOneofCase.WsOpen => $"ws_open {e.WsOpen.Id}",
        Effect.KindOneofCase.WsSend => $"ws_send {e.WsSend.Id}",
        Effect.KindOneofCase.WsClose => $"ws_close {e.WsClose.Id_}",
        Effect.KindOneofCase.Store => $"store {e.Store.Id}",
        Effect.KindOneofCase.Download => $"download {e.Download.Id}",
        Effect.KindOneofCase.Discard => $"discard {e.Discard.Id_}",
        Effect.KindOneofCase.Cancel => $"cancel {e.Cancel.Id_}",
        Effect.KindOneofCase.Notify => $"notify {e.Notify.KindCase}",
        Effect.KindOneofCase.ProxyQuery => $"proxy_query {e.ProxyQuery.Id}",
        Effect.KindOneofCase.StreamResolve => $"stream_resolve {e.StreamResolve.Id}",
        Effect.KindOneofCase.StreamOpen => $"stream_open {e.StreamOpen.Id} {e.StreamOpen.Host}:{e.StreamOpen.Port}",
        Effect.KindOneofCase.StreamStartTls => $"stream_start_tls {e.StreamStartTls.Id}",
        Effect.KindOneofCase.StreamWrite => $"stream_write {e.StreamWrite.Id} {e.StreamWrite.Data.Length} B",
        Effect.KindOneofCase.StreamClose => $"stream_close {e.StreamClose.Id}{(e.StreamClose.Abort ? " abort" : string.Empty)}",
        Effect.KindOneofCase.StreamSplice => $"stream_splice {e.StreamSplice.Id} -> {e.StreamSplice.Download} {e.StreamSplice.Bytes} B",
        Effect.KindOneofCase.DownloadData => $"download_data {e.DownloadData.Id} {e.DownloadData.Data.Length} B",
        Effect.KindOneofCase.DownloadDone => $"download_done {e.DownloadDone.Id_}",
        _ => e.KindCase.ToString(),
    };

    /// <summary>The id an input names, its effect's kind, and the status it carries.</summary>
    private (ulong Id, Effect.KindOneofCase Of, uint? Status)? Named(Input i) => i.KindCase switch
    {
        Input.KindOneofCase.HttpResponse => (i.HttpResponse.Id, Effect.KindOneofCase.HttpRequest, i.HttpResponse.Status),
        Input.KindOneofCase.HttpFailed => (i.HttpFailed.Id, Effect.KindOneofCase.HttpRequest, null),
        Input.KindOneofCase.WsOpened => (i.WsOpened.Id_, Effect.KindOneofCase.WsOpen, null),
        Input.KindOneofCase.WsFailed => (i.WsFailed.Id, Effect.KindOneofCase.WsOpen, i.WsFailed.HasStatus ? i.WsFailed.Status : null),
        Input.KindOneofCase.WsMessage => (i.WsMessage.Id, Effect.KindOneofCase.WsOpen, null),
        Input.KindOneofCase.WsClosed => (i.WsClosed.Id, Effect.KindOneofCase.WsOpen, null),
        Input.KindOneofCase.WsLost => (i.WsLost.Id, Effect.KindOneofCase.WsOpen, null),
        Input.KindOneofCase.Stored => (i.Stored.Id_, Effect.KindOneofCase.Store, null),
        Input.KindOneofCase.StoreFailed => (i.StoreFailed.Id, Effect.KindOneofCase.Store, null),
        Input.KindOneofCase.Downloaded => (i.Downloaded.Id, rust ? Effect.KindOneofCase.DownloadDone : Effect.KindOneofCase.Download, null),
        Input.KindOneofCase.DownloadFailed => (i.DownloadFailed.Id, rust ? Effect.KindOneofCase.DownloadDone : Effect.KindOneofCase.Download, null),
        Input.KindOneofCase.DownloadResponse => (i.DownloadResponse.Id, Effect.KindOneofCase.Download, i.DownloadResponse.Status),
        _ => null,
    };

    private void Violation(string what) => this.violations.Add($"step {this.count}: {what}");

    private void Application(Input i)
    {
        if (this.appStopped)
        {
            return;
        }

        switch (i.KindCase)
        {
            case Input.KindOneofCase.Stop:
                this.appStopped = true;
                break;
            case Input.KindOneofCase.SetHealth:
                this.app.Add($"health {i.SetHealth.Status}");
                break;
            case Input.KindOneofCase.SetEffectiveConfig:
                this.app.Add($"config {i.SetEffectiveConfig.ConfigMap?.ConfigMap.Keys.FirstOrDefault()}");
                break;
            case Input.KindOneofCase.SetDescription:
                this.app.Add($"description {i.SetDescription.NonIdentifyingAttributes.FirstOrDefault()?.Value.StringValue}");
                break;
        }
    }

    private void Issue(Effect e)
    {
        void Add(CallKind? kind, ulong id)
        {
            if (this.effects.ContainsKey(id))
            {
                this.Violation($"the core issued id {id} twice"); // the core's promise, not the shell's
                return;
            }

            this.effects[id] = new Fx(e.KindCase, id, this.count);
            if (kind is { } k)
            {
                this.issued[k].Add(id);
            }
        }

        switch (e.KindCase)
        {
            case Effect.KindOneofCase.HttpRequest:
                Add(CallKind.Post, e.HttpRequest.Id);
                break;
            case Effect.KindOneofCase.WsOpen:
                Add(CallKind.Connect, e.WsOpen.Id);
                break;
            case Effect.KindOneofCase.Download:
                Add(CallKind.Get, e.Download.Id);
                break;
            case Effect.KindOneofCase.Store:
                Add(null, e.Store.Id);
                break;
            case Effect.KindOneofCase.WsClose when this.effects.TryGetValue(e.WsClose.Id_, out var closing):
                closing.CloseAsked = true;
                break;
            case Effect.KindOneofCase.Cancel when this.effects.TryGetValue(e.Cancel.Id_, out var cancelled) && !cancelled.Ended:
                cancelled.Cancelled = true;
                if (rust)
                {
                    this.discarded.Add(e.Cancel.Id_);
                }

                break;
            case Effect.KindOneofCase.Cancel when rust:
                this.discarded.Add(e.Cancel.Id_); // a download whose file hadn't begun, or its done not asked
                break;
            case Effect.KindOneofCase.Discard when rust:
                this.discarded.Add(e.Discard.Id_);
                break;
            case Effect.KindOneofCase.DownloadDone:
                Add(null, e.DownloadDone.Id_);
                break;
            case Effect.KindOneofCase.ProxyQuery:
                this.streams[e.ProxyQuery.Id] = "asking";
                this.Tally("proxy asked");
                break;
            case Effect.KindOneofCase.StreamResolve:
                this.streams[e.StreamResolve.Id] = "resolving";
                break;
            case Effect.KindOneofCase.StreamOpen:
                this.streams[e.StreamOpen.Id] = "opening";
                this.Tally("stream opening");
                break;
            case Effect.KindOneofCase.StreamStartTls when this.streams.GetValueOrDefault(e.StreamStartTls.Id) == "open":
                this.streams[e.StreamStartTls.Id] = "tls";
                this.Tally("stream TLS");
                break;
            case Effect.KindOneofCase.StreamSplice:
                this.splices[e.StreamSplice.Id] = this.splices.GetValueOrDefault(e.StreamSplice.Id) + 1;
                this.Tally("stream spliced");
                break;
            case Effect.KindOneofCase.StreamClose:
                if (this.streams.TryGetValue(e.StreamClose.Id, out var was) && was is not ("failed" or "closed"))
                {
                    this.Tally($"stream closed{(e.StreamClose.Abort ? " (abort)" : string.Empty)} when {was}");
                }

                this.streams[e.StreamClose.Id] = "closed";
                this.splices.Remove(e.StreamClose.Id);
                break;
        }
    }

    private void Tally(string what) => this.streamTally[what] = this.streamTally.GetValueOrDefault(what) + 1;

    /// <summary>Rust mode: an input about stream <paramref name="id"/>, as its state allows.</summary>
    private void StreamResult(ulong id, Input input, bool final)
    {
        var kind = input.KindCase;
        ulong? named = kind switch
        {
            Input.KindOneofCase.ProxyAnswer => input.ProxyAnswer.Id,
            Input.KindOneofCase.StreamResolved => input.StreamResolved.Id,
            Input.KindOneofCase.StreamOpened => input.StreamOpened.Id_,
            Input.KindOneofCase.StreamTlsReady => input.StreamTlsReady.Id_,
            Input.KindOneofCase.StreamData => input.StreamData.Id,
            Input.KindOneofCase.StreamEof => input.StreamEof.Id_,
            Input.KindOneofCase.StreamFailed => input.StreamFailed.Id,
            Input.KindOneofCase.StreamSpliced => input.StreamSpliced.Id_,
            _ => null,
        };
        if (named is null)
        {
            this.Violation($"{kind} delivered as an input of stream {id}");
            return;
        }

        if (named != id)
        {
            this.Violation($"{kind} names stream {named}, but came as one of stream {id}");
        }

        if (!this.streams.TryGetValue(id, out var state))
        {
            this.Violation($"{kind} about stream {id}, which the core never asked for (RULE-SHELL-04)");
            return;
        }

        if (state == "closed")
        {
            this.Violation($"{kind} about stream {id} after the core closed it (RULE-SHELL-06)");
            return;
        }

        var (next, ends) = (kind, state) switch
        {
            (Input.KindOneofCase.ProxyAnswer, "asking") => ("answered", true),
            (Input.KindOneofCase.StreamResolved, "resolving") => ("resolved", true),
            (Input.KindOneofCase.StreamFailed, "resolving" or "opening" or "tls" or "open" or "eof") => ("failed", true),
            (Input.KindOneofCase.StreamOpened, "opening") => ("open", false),
            (Input.KindOneofCase.StreamTlsReady, "tls") => ("open", false),
            (Input.KindOneofCase.StreamData, "open") => ("open", false),
            (Input.KindOneofCase.StreamEof, "open") => ("eof", false),
            (Input.KindOneofCase.StreamSpliced, "open" or "eof") when this.splices.GetValueOrDefault(id) > 0 => (state, false),
            _ => ((string?)null, false),
        };
        if (next is null)
        {
            this.Violation($"{kind} about stream {id}, which is {state}{(kind == Input.KindOneofCase.StreamSpliced ? " with no splice asked" : string.Empty)} (RULE-SHELL-04)");
            return;
        }

        if (final != ends)
        {
            this.Violation($"{kind} of stream {id} {(final ? "ends" : "doesn't end")} it");
        }

        if (kind == Input.KindOneofCase.StreamSpliced)
        {
            this.splices[id]--;
        }

        if (kind is Input.KindOneofCase.StreamFailed or Input.KindOneofCase.StreamEof or Input.KindOneofCase.StreamTlsReady)
        {
            this.Tally($"stream {(kind == Input.KindOneofCase.StreamFailed ? "failed" : kind == Input.KindOneofCase.StreamEof ? "ended by the peer" : "TLS up")}");
        }

        this.streams[id] = next;
    }

    private void Result(ulong id, Input input, bool final)
    {
        var kind = input.KindCase;
        if (Named(input) is not { } named)
        {
            this.Violation($"{kind} delivered as a result of effect {id}");
            return;
        }

        if (named.Id != id)
        {
            this.Violation($"{kind} names effect {named.Id}, but came as a result of {id}");
        }

        if (!this.effects.TryGetValue(id, out var fx))
        {
            this.Violation($"{kind} about effect {id}, which the core never issued (RULE-SHELL-04)");
            return;
        }

        if (fx.Kind != named.Of)
        {
            this.Violation($"{kind} answers effect {id}, a {fx.Kind} (RULE-SHELL-04)");
        }

        if (fx.Cancelled)
        {
            this.Violation($"{kind} about effect {id} after its cancel (RULE-SHELL-06)");
            return;
        }

        if (fx.Ended)
        {
            if (fx.Crossed && final && kind is Input.KindOneofCase.WsClosed or Input.KindOneofCase.WsLost)
            {
                // The end of the core's close, after the server's Close that crossed it: the
                // core has taken that as the end already, and ignores this one.
                fx.Crossed = false;
                return;
            }

            this.Violation($"{kind} about effect {id} after its end, {fx.End} (RULE-SHELL-04)");
            return;
        }

        switch (kind)
        {
            case Input.KindOneofCase.WsOpened:
                if (fx.Opened || final)
                {
                    this.Violation($"ws_opened of {id} twice, or as its end");
                }

                fx.Opened = true;
                break;
            case Input.KindOneofCase.WsMessage when !fx.Opened || final:
                this.Violation($"ws_message of {id} before ws_opened, or as its end");
                break;
            case Input.KindOneofCase.WsLost when !fx.Opened:
                this.Violation($"ws_lost of {id} before ws_opened");
                break;
            case Input.KindOneofCase.WsClosed when !fx.Opened:
                this.Violation($"ws_closed of {id} before ws_opened");
                break;
            case Input.KindOneofCase.WsClosed when final && !fx.CloseAsked:
                this.Violation($"ws_closed ends {id}, but the core never asked to close it");
                break;
            case Input.KindOneofCase.WsClosed when !final:
                if (fx.ServerClosed)
                {
                    this.Violation($"the server's Close on {id} reported twice");
                }

                fx.ServerClosed = true;
                if (fx.CloseAsked)
                {
                    // The server's Close crossed the core's ws_close: to the core, which can't
                    // tell them apart, that is the end of its close (the shell's own end of it
                    // follows, and is ignored as stale; tolerated, and counted).
                    fx.Ended = true;
                    fx.End = kind;
                    fx.Crossed = true;
                    this.CrossedCloses++;
                }

                break;
            case Input.KindOneofCase.WsOpened or Input.KindOneofCase.WsMessage or Input.KindOneofCase.WsClosed:
                break;
            default:
                if (!final)
                {
                    this.Violation($"{kind} of {id} doesn't end it");
                }

                break;
        }

        if (final)
        {
            fx.Ended = true;
            fx.End = kind;
            fx.EndStatus = named.Status;
        }
    }

    /// <summary>One effect the core issued, and what the shell said of it.</summary>
    internal sealed class Fx(Effect.KindOneofCase kind, ulong id, int step)
    {
        public Effect.KindOneofCase Kind { get; } = kind;

        public ulong Id { get; } = id;

        public bool Opened { get; set; }

        public bool ServerClosed { get; set; }

        public bool CloseAsked { get; set; }

        public bool Ended { get; set; }

        public Input.KindOneofCase? End { get; set; }

        public uint? EndStatus { get; set; }

        public bool Cancelled { get; set; }

        /// <summary>Ended by the server's Close crossing the core's ws_close: one more end may follow.</summary>
        public bool Crossed { get; set; }

        public override string ToString() =>
            $"{this.Kind} {this.Id} (issued at step {step}){(this.Opened ? ", opened" : string.Empty)}{(this.CloseAsked ? ", closing" : string.Empty)}"
            + (this.Ended ? $", ended {this.End}" : this.Cancelled ? ", cancelled" : ", running");
    }
}

/// <summary>How a test answers a call (or a held body).</summary>
internal enum How
{
    Ok,
    Config,
    Package,
    Status,
    Throw,
    Hold,
    BodyFault,
}

/// <summary>
/// One client on the in-memory transport and fake time, its callback model
/// (<see cref="CallbackPropertyTests.Agent"/>) and its trace; started on first use, so that a
/// generator can make one without side effects. In Rust mode the client's streams are in memory
/// (<see cref="MemNet"/>), and the server at their far end (<see cref="MemServer"/>) turns what the
/// core sends into the same calls, answered the same way.
/// </summary>
internal sealed class Rig
{
    /// <summary>Its stop timeout: stop must return within this and a margin, in fake time (RULE-SHELL-07).</summary>
    public static readonly TimeSpan StopTimeout = TimeSpan.FromSeconds(2);

    private static readonly TimeSpan RealBound = TimeSpan.FromSeconds(10);
    private readonly object gate = new();
    private readonly string dir;
    private readonly List<string> log = new();
    private readonly Lazy<string[]> settled;
    private readonly bool stateful;
    private OpAmpClient? client;
    private Task? stop;
    private int counter;
    private bool finished;

    /// <summary>What the runs did, by kind, summed over the runs that passed (printed by each test).</summary>
    public static ConcurrentDictionary<string, int> Stats { get; } = new();

    /// <param name="ws">WebSocket, or HTTP.</param>
    /// <param name="deafMask">Which calls (the n-th of each kind, bit n) ignore their cancel.</param>
    /// <param name="onCall">Answers calls as they are made (else the test answers them).</param>
    /// <param name="stateful">With a state file (saves, and packages), or without.</param>
    /// <param name="rust">Rust mode: the core's own HTTP and WebSocket over in-memory streams.</param>
    /// <param name="tls">Rust mode: https:// or wss:// (TLS asked for, and none in memory).</param>
    /// <param name="connectMask">Rust mode: which connects (the n-th, n under 16) are refused (bit n) or held until their cancel and then accepted (bit n + 16).</param>
    public Rig(bool ws, ulong deafMask, Action<Call>? onCall = null, bool stateful = true, bool rust = false, bool tls = false, ulong connectMask = 0)
    {
        this.Ws = ws;
        this.stateful = stateful;
        this.DeafMask = deafMask;
        this.Rust = rust;
        this.Tls = tls;
        this.ConnectMask = connectMask;
        this.Trace = new Trace(rust);
        this.Agent.Asked = this.Trace.Asked;
        this.Transport = new MemoryTransport((_, n) => n < 64 && (deafMask >> n & 1) != 0, onCall);
        if (rust)
        {
            this.Server = new MemServer(
                this.Transport,
                this.Net,
                refuse: n => n < 16 && (connectMask >> n & 1) != 0,
                deaf: n => n < 16 && (connectMask >> n & 1) == 0 && (connectMask >> (n + 16) & 1) != 0);
        }

        this.dir = Path.Combine(Path.GetTempPath(), "opamp-schedule-" + Guid.NewGuid().ToString("N")[..12]);
        this.settled = new Lazy<string[]>(() =>
        {
            try
            {
                this.Finish("once the threads were done");
                var failed = this.Stops.Where(t => !t.IsCompletedSuccessfully).ToArray();
                if (failed.Length > 0)
                {
                    throw new InvalidOperationException($"{failed.Length} of {this.Stops.Count} stops didn't return, or failed{this.Report()}");
                }

                return this.Trace.App;
            }
            catch (Exception e)
            {
                this.Failure = e.Message;
                throw;
            }
        });
    }

    /// <summary>What the model expects of the steps just done (the schedule's model takes them).</summary>
    public ConcurrentQueue<Expected> Expecting { get; } = new();

    /// <summary>The client's work the test holds outside the transport (a save behind one of its own).</summary>
    public int HeldElsewhere { get; set; }

    /// <summary>Steps done so far.</summary>
    public int Ops { get; set; }

    /// <summary>Stops called from the threads (the parallel test).</summary>
    public ConcurrentBag<Task> Stops { get; } = new();

    /// <summary>Why <see cref="Settled"/> failed.</summary>
    public string? Failure { get; private set; }

    /// <summary>Once, however many threads ask: stop, check, and the application's calls the core took before its stop.</summary>
    public string[] Settled() => this.settled.Value;

    public bool Ws { get; }

    public ulong DeafMask { get; }

    public FakeTimeProvider Time { get; } = new(new DateTimeOffset(2026, 1, 1, 0, 0, 0, TimeSpan.Zero));

    public MemoryTransport Transport { get; }

    public Trace Trace { get; }

    public bool Rust { get; }

    public bool Tls { get; }

    public ulong ConnectMask { get; }

    /// <summary>Rust mode's network, and the server at its far end.</summary>
    public MemNet Net { get; } = new();

    public MemServer? Server { get; }

    public CallbackPropertyTests.Agent Agent { get; } = new(0, 0, 0);

    public OpAmpClient Client
    {
        get
        {
            lock (this.gate)
            {
                return this.client ??= this.Start();
            }
        }
    }

    public bool StopCalled => this.stop is not null;

    public bool Stopped => this.stop is { IsCompleted: true };

    public string StateFile => Path.Combine(this.dir, "state");

    public static ServerToAgent Plain(ByteString uid) => new() { InstanceUid = uid };

    public override string ToString() =>
        $"{(this.Rust ? "Rust mode" : ".NET mode")}, {(this.Ws ? "ws" : "http")}{(this.Tls ? " over TLS" : string.Empty)}, deaf calls {this.DeafMask:x}"
        + (this.Rust ? $", connects {this.ConnectMask:x}" : string.Empty);

    /// <summary>What happened: the ops, the calls, the trace's tail, the violations.</summary>
    public string Report()
    {
        string[] ops;
        lock (this.gate)
        {
            ops = this.log.ToArray();
        }

        return $"\n  {this}\n  ops: {string.Join("; ", ops)}\n  calls:\n    {string.Join("\n    ", this.Transport.Calls.Select(c => c.ToString()))}"
            + (this.Rust ? $"\n  connects:\n    {string.Join("\n    ", this.Net.Dials.Select(d => d.ToString()))}" : string.Empty)
            + $"\n  callbacks: {string.Join(", ", this.Agent.Seen().Select(n => n.GetType().Name))}"
            + $"\n  trace:\n{this.Trace.Log}\n  violations: {string.Join("; ", this.Trace.Violations)}";
    }

    public void Note(string what)
    {
        lock (this.gate)
        {
            this.log.Add(what);
        }
    }

    /// <summary>Wait (real time, a little) until nothing moves: no step, no call, no callback.</summary>
    public void Pace(int quiet = 3, int maxMs = 200)
    {
        var until = Stopwatch.GetTimestamp() + (maxMs * Stopwatch.Frequency / 1000);
        var last = this.Snapshot();
        var still = 0;
        while (still < quiet && Stopwatch.GetTimestamp() < until)
        {
            Thread.Sleep(1);
            var now = this.Snapshot();
            if (now == last && now.Item5 == 0)
            {
                still++;
            }
            else
            {
                still = 0;
                last = now;
            }
        }
    }

    /// <summary>Wait (real time) until <paramref name="done"/>: whether it came.</summary>
    public static bool Until(Func<bool> done, TimeSpan? within = null)
    {
        var until = Stopwatch.GetTimestamp() + (long)((within ?? RealBound).TotalSeconds * Stopwatch.Frequency);
        while (!done())
        {
            if (Stopwatch.GetTimestamp() > until)
            {
                return false;
            }

            Thread.Sleep(1);
        }

        return true;
    }

    /// <summary>The rules that hold at every moment; throws with the report if one is broken.</summary>
    public void CheckSafety(string when)
    {
        var problems = this.Trace.Violations.ToList();
        problems.AddRange(this.Transport.Late.Select(l => $"a call after stop returned (RULE-SHELL-07): {l}"));
        try
        {
            this.Agent.Check(when);
        }
        catch (InvalidOperationException e)
        {
            problems.Add($"the callback model: {e.Message}");
        }

        if (problems.Count > 0)
        {
            throw new InvalidOperationException($"{when}: {string.Join("; ", problems)}{this.Report()}");
        }
    }

    /// <summary>A message from the server: its bytes for this call (HTTP) or for the connection (with the OpAMP header).</summary>
    public byte[] Offer(How how, ByteString uid)
    {
        var n = Interlocked.Increment(ref this.counter);
        var msg = Plain(uid);
        if (how == How.Config)
        {
            var hash = ByteString.CopyFromUtf8($"config-{n}");
            var config = new AgentConfigMap();
            config.ConfigMap[string.Empty] = new AgentConfigObject { Body = hash, ContentType = "text/plain" };
            msg.RemoteConfig = new AgentRemoteConfig { Config = config, ConfigHash = hash };
        }
        else if (how == How.Package)
        {
            var url = $"http://packages.test/p{n}";
            // (Rust mode: some of 24 KiB or 48 KiB, which the shell splices to the file: from 16 KiB.)
            var file = Encoding.UTF8.GetBytes($"package {n} " + new string('x', (n % 7 * 100) + (this.Rust ? n % 3 * 24_000 : 0)));
            lock (this.Transport.Files)
            {
                this.Transport.Files[url] = file;
            }

            msg.PackagesAvailable = new PackagesAvailable { AllPackagesHash = ByteString.CopyFromUtf8($"all-{n}") };
            msg.PackagesAvailable.Packages["pkg"] = new PackageAvailable
            {
                Type = PackageType.TopLevel,
                Version = $"{n}",
                Hash = ByteString.CopyFromUtf8($"pkg-{n}"),
                File = new DownloadableFile { DownloadUrl = url, ContentHash = ByteString.CopyFrom(SHA256.HashData(file)) },
            };
        }

        return msg.ToByteArray();
    }

    /// <summary>The file of a package URL.</summary>
    public byte[] FileAt(Uri url)
    {
        lock (this.Transport.Files)
        {
            return this.Transport.Files.TryGetValue(url.ToString(), out var f) ? f : [];
        }
    }

    /// <summary>
    /// Answer <paramref name="call"/> (a pending one, or a held body) <paramref name="how"/>, with
    /// <paramref name="fault"/> if it fails: what the core must hear of it, unless it is cancelled
    /// (null: nothing to predict).
    /// </summary>
    public Expected? Answer(Call call, How how, Exception fault)
    {
        if (call.Headers.Task.IsCompleted)
        {
            // A held body: on, or failing.
            var failing = how is How.Throw or How.BodyFault;
            if (!call.Release(failing ? fault : null))
            {
                return null;
            }

            return this.Expect(call, failing ? Fail(call.Kind) : Done(call.Kind), null);
        }

        switch (call.Kind)
        {
            case CallKind.Post:
                var uid = AgentToServer.Parser.ParseFrom(call.Body).InstanceUid;
                return how switch
                {
                    How.Throw => call.Fail(fault) ? this.Expect(call, Fail(call.Kind), null) : null,
                    How.Status => call.Respond(503, [], retryAfter: "1") ? this.Expect(call, Done(call.Kind), 503) : null,
                    How.Hold => Held(call.Respond(200, this.Offer(How.Ok, uid), hold: true)),
                    How.BodyFault => call.Respond(200, this.Offer(How.Ok, uid), bodyFault: fault) ? this.Expect(call, Fail(call.Kind), null) : null,
                    _ => call.Respond(200, this.Offer(how, uid)) ? this.Expect(call, Done(call.Kind), 200) : null,
                };
            case CallKind.Get:
                var file = this.FileAt(call.Url);
                return how switch
                {
                    How.Throw => call.Fail(fault) ? this.Expect(call, Fail(call.Kind), null) : null,
                    How.Status => call.Respond(404, []) ? this.Expect(call, Done(call.Kind), 404) : null,
                    How.Hold => Held(call.Respond(200, file, hold: true)),
                    How.BodyFault => call.Respond(200, file, bodyFault: fault) ? this.Expect(call, Fail(call.Kind), null) : null,
                    _ => call.Respond(200, file) ? this.Expect(call, Done(call.Kind), 200) : null,
                };
            default:
                return how switch
                {
                    How.Throw or How.BodyFault => call.Fail(fault) ? this.Expect(call, Fail(call.Kind), null) : null,
                    How.Status => call.Fail(new WebSocketConnectException(new WebSocketException("refused (injected)"), 401, null))
                        ? this.Expect(call, Input.KindOneofCase.WsFailed, 401) : null,
                    _ => call.Accept(new MemSocket(this.Transport)) ? this.Expect(call, Input.KindOneofCase.WsOpened, null) : null,
                };
        }
    }

    /// <summary>The connection the server can talk on: the last one handed over and still open.</summary>
    public MemSocket? Socket() =>
        this.Transport.Calls.Where(c => c.Socket is { Released: false }).Select(c => c.Socket).LastOrDefault();

    /// <summary>
    /// Stop, and drive fake time until stop returns: within the stop timeout and a margin (all of
    /// stop: the core's wait and the drain's together), or it is a violation (RULE-SHELL-07).
    /// </summary>
    public void Stop(string why)
    {
        var client = this.Client;
        lock (this.gate)
        {
            this.stop ??= client.StopAsync();
        }

        // Timed from when the core took it (the loop may be slow to come to it, not the clock).
        Until(() => this.Trace.StopTaken || this.stop.IsCompleted);
        var start = this.Time.GetUtcNow();
        var limit = StopTimeout + TimeSpan.FromSeconds(1);
        var draining = false;
        while (!this.stop.IsCompleted)
        {
            this.Pace(quiet: 4, maxMs: 300);
            if (this.stop.IsCompleted)
            {
                break;
            }

            if (this.Time.GetUtcNow() - start > limit)
            {
                Until(() => this.stop.IsCompleted, TimeSpan.FromSeconds(2));
                if (!this.stop.IsCompleted)
                {
                    throw new InvalidOperationException($"{why}: stop hadn't returned {limit.TotalSeconds} s (fake) after it was called (RULE-SHELL-07){this.Report()}");
                }

                break;
            }

            // The clock moves only once the work that can finish has (on a slow machine, the
            // drain's bound would otherwise pass while it is still finishing).
            Until(() => this.Running() <= 0, TimeSpan.FromSeconds(3));

            // Once the core has stopped, the drain's bound is armed on the clock as the loop
            // comes to it: give a loop slow to come there (a busy machine) the moment it takes,
            // or the clock passes the limit before the bound is even armed.
            if (this.Trace.Stopped && !draining)
            {
                draining = true;
                Until(() => this.stop.IsCompleted, TimeSpan.FromMilliseconds(100));
            }

            this.Time.Advance(TimeSpan.FromMilliseconds(250));
        }

        this.Transport.ClosedBecause = "stop returned";
        if (this.stop.Exception is { } e)
        {
            throw new InvalidOperationException($"{why}: stop failed: {e.InnerException}{this.Report()}");
        }
    }

    /// <summary>
    /// Stop (if not yet), then check what must hold once stop has returned (RULE-SHELL-05, -06,
    /// -07): every effect ended or cancelled; nothing of the client's still connected or reading;
    /// what arrives late (a deaf call answered now) released at once and never reported; no call
    /// after stop; the files of cancelled downloads gone; the callback model.
    /// </summary>
    public void Finish(string why)
    {
        if (this.finished)
        {
            return;
        }

        this.finished = true;
        try
        {
            this.Stop(why);
            var problems = new List<string>();
            problems.AddRange(this.Trace.Unended().Select(u => $"never ended, nor cancelled (RULE-SHELL-05): {u}"));
            if (this.Rust)
            {
                // Every stream closed (the client's end reset or disposed) when stop returns, and
                // none left in the registry; then the server's side of each call settles.
                if (this.client!.LiveStreams > 0)
                {
                    problems.Add($"{this.client.LiveStreams} stream(s) left when stop returned (RULE-SHELL-07)");
                }

                problems.AddRange(this.Net.Dials.Where(d => d.Client is { Closed: false } && !d.Deaf).Select(d => $"a connection still open after stop returned (RULE-SHELL-07): {d}"));
                Until(() => this.Transport.Calls.Where(c => !c.Deaf).All(c => !c.Open && c.Released), TimeSpan.FromSeconds(5));
            }

            // Nothing the client started is running when stop returns, but
            // what the platform can't take back (a deaf call).
            foreach (var c in this.Transport.Calls.Where(c => !c.Deaf))
            {
                if (c.Open)
                {
                    problems.Add($"a call still running after stop returned (RULE-SHELL-07): {c}");
                }
                else if (!c.Released)
                {
                    problems.Add($"not released when stop returned (RULE-SHELL-07): {c}");
                }
            }

            // What the deaf calls hand over now is late: released at once, never reported.
            foreach (var c in this.Transport.Calls.Where(c => c.Open))
            {
                if (c.Headers.Task.IsCompleted)
                {
                    c.Release(null);
                }
                else if (c.Kind == CallKind.Connect)
                {
                    c.Accept(new MemSocket(this.Transport));
                }
                else
                {
                    c.Respond(200, c.Kind == CallKind.Get ? this.FileAt(c.Url) : [], hold: false);
                }
            }

            if (!Until(() => this.Transport.Calls.All(c => c.Released)))
            {
                problems.AddRange(this.Transport.Calls.Where(c => !c.Released).Select(c => $"a late resource never released (RULE-SHELL-06): {c}"));
            }

            // Rust mode: a connect that completed after its cancel is reset at once.
            if (this.Server is { } server && !Until(() => server.Late.All(d => !d.Done || d.Task.IsCanceled || d.Server is null || d.Server.WasReset)))
            {
                problems.AddRange(server.Late.Where(d => d.Server is { WasReset: false }).Select(d => $"a connection that came after its close stayed open (RULE-SHELL-06): {d}"));
            }

            // A cancelled download's file goes, written in full or in part.
            var cancelled = this.Trace.CancelledDownloads();
            string[] Left() => Directory.Exists(this.StateFile + ".downloads")
                ? Directory.GetFiles(this.StateFile + ".downloads")
                    .Where(f => cancelled.Any(id => f.EndsWith($"-{id}", StringComparison.Ordinal))).ToArray()
                : [];
            if (!Until(() => Left().Length == 0, TimeSpan.FromSeconds(5)))
            {
                problems.AddRange(Left().Select(f => $"a cancelled download's file stayed (RULE-SHELL-06): {f}"));
            }

            if (this.Agent.Seen().Count(n => n is OpAmpNotification.Stopped) != 1)
            {
                problems.Add("Stopped wasn't heard exactly once");
            }

            if (problems.Count > 0)
            {
                throw new InvalidOperationException($"{why}: {string.Join("; ", problems)}{this.Report()}");
            }

            this.CheckSafety(why);
            foreach (var t in this.Trace.Tally()
                .Concat(this.Transport.Calls.Where(c => c.Deaf && c.Answer is not null).Select(c => $"deaf {c.Kind} answered"))
                .Concat(this.Transport.Calls.Where(c => c.CancelledFirst).Select(c => $"{c.Kind} answered after its cancel"))
                .Append(this.Ws ? "runs over WebSocket" : "runs over HTTP"))
            {
                Stats.AddOrUpdate(t, 1, (_, n) => n + 1);
            }
        }
        finally
        {
            try
            {
                Directory.Delete(this.dir, recursive: true);
            }
            catch (IOException)
            {
            }
        }
    }

    /// <summary>Best effort, for a run that failed half-way: stop it and let it go.</summary>
    public void Abandon()
    {
        if (this.finished || this.client is null)
        {
            return;
        }

        this.finished = true;
        _ = this.client.StopAsync();
        for (var i = 0; i < 20; i++)
        {
            this.Time.Advance(TimeSpan.FromSeconds(1));
        }

        foreach (var c in this.Transport.Calls.Where(c => c.Open))
        {
            c.Fail(new IOException("abandoned"));
            c.Release(new IOException("abandoned"));
        }
    }

    /// <summary>Headers given and the body held: nothing to expect yet.</summary>
    private static Expected? Held(bool answered) => null;

    private static Input.KindOneofCase Done(CallKind kind) =>
        kind == CallKind.Post ? Input.KindOneofCase.HttpResponse : Input.KindOneofCase.DownloadResponse;

    private static Input.KindOneofCase Fail(CallKind kind) => kind switch
    {
        CallKind.Post => Input.KindOneofCase.HttpFailed,
        CallKind.Get => Input.KindOneofCase.DownloadFailed,
        _ => Input.KindOneofCase.WsFailed,
    };

    // Rust mode: the core's own HTTP effects are inside the shell, so what each answer becomes
    // isn't seen at the boundary; the trace holds the streams to their rules instead.
    private Expected? Expect(Call call, Input.KindOneofCase input, uint? status) =>
        call.CancelledFirst || this.Rust ? null : new Expected(call.Kind, call.OfKind, input, status, call.ToString());

    /// <summary>
    /// The client's work that can still finish on its own: what it tracks, but for what waits on
    /// the test (a call or a body it holds) or reads an open connection.
    /// </summary>
    private int Running()
    {
        if (this.Rust)
        {
            // A stream's task, waiting to read (or connect), and a closed stream's linger, waiting on
            // the clock.
            return (this.client?.Working ?? 0) - (this.client?.StreamsParked ?? 0) - this.HeldElsewhere;
        }

        var calls = this.Transport.Calls;
        return (this.client?.Working ?? 0) - calls.Count(c => c.Open) - calls.Count(c => c.Socket is { Released: false }) - this.HeldElsewhere;
    }

    private (int, int, int, int, int) Snapshot() =>
        (this.Trace.Count, this.Transport.Version, this.Agent.Seen().Length, this.Stopped ? 1 : 0, this.client?.Queued ?? 0);

    private OpAmpClient Start()
    {
        Directory.CreateDirectory(this.dir);
        var c = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Endpoint = new Uri($"{(this.Ws ? "ws" : "http")}{(this.Tls ? "s" : string.Empty)}://opamp.test/v1/opamp"),
                Mode = this.Rust ? OpAmpMode.Rust : OpAmpMode.DotNet,
                DirectConnection = this.Rust,
                StateFile = this.stateful ? this.StateFile : null,
                AcceptPackages = this.stateful,
                Capabilities = (ulong)(AgentCapabilities.AcceptsRemoteConfig | AgentCapabilities.ReportsRemoteConfig
                    | AgentCapabilities.ReportsHealth | AgentCapabilities.ReportsEffectiveConfig | AgentCapabilities.AcceptsPackages
                    | AgentCapabilities.ReportsPackageStatuses),
                HeartbeatInterval = TimeSpan.FromSeconds(30),
                RequestTimeout = TimeSpan.FromSeconds(10),
                ConnectTimeout = TimeSpan.FromSeconds(10),
                DownloadTimeout = TimeSpan.FromSeconds(60),
                StopTimeout = StopTimeout,
                TimeProvider = this.Time,
            },
            new OpAmpStatus(),
            this.Agent.Notify,
            new Seams { Transport = this.Transport, Stepped = this.Trace.OnStep, Net = this.Rust ? this.Net : null });
        this.Agent.Started(c);
        return c;
    }
}

/// <summary>What the core must hear about the <c>OfKind</c>-th call of <c>Kind</c>, unless it was cancelled.</summary>
internal sealed record Expected(CallKind Kind, int OfKind, Input.KindOneofCase Input, uint? Status, string Call);
