// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Pydantic.OpAmp.Core;

namespace Pydantic.OpAmp.Client;

/// <summary>
/// An input on its way to the core: from the application (<see cref="Of"/> null), or the result of
/// effect <see cref="Of"/>; <see cref="Final"/> if it ends that effect. <see cref="Registry"/>: the
/// registry <see cref="Of"/> is in (null: the executor's; Rust mode's streams have their own, their
/// ids being the shell's, not the core's). <see cref="Ack"/>, if given, is completed once the loop
/// has stepped with it and carried out the step's effects, or dropped it. <see cref="Turn"/>: one
/// of the application's calls, in the queue to mark its turn (the call itself waits in the calls'
/// own queue, and may be fed before its turn: RULE-SHELL-10).
/// </summary>
internal readonly record struct Queued(
    Input Input, ulong? Of = null, bool Final = false, InFlight? Registry = null, TaskCompletionSource? Ack = null, bool Turn = false);

/// <summary>
/// The effects in flight, by id: the one place that decides which results reach the core, what a
/// cancel ends, and when stop is over. The executor does the I/O and hands its results here
/// (through the loop's queue).
/// </summary>
/// <remarks>
/// Invariants:
/// <list type="bullet">
/// <item>Every effect in flight (a request, a dial, an open or closing connection, a download, a
/// save) has an entry under its id, holding the <see cref="CancellationTokenSource"/> that aborts
/// its work.</item>
/// <item><see cref="Cancel"/> removes the entry, cancels its token and releases what it holds.</item>
/// <item>A result reaches the core only if its entry is there when the loop takes it
/// (<see cref="Take"/>); a final result removes the entry. Cancel and take both run on the loop,
/// so checking and removing are one step: nothing about a cancelled id reaches the core, even a
/// result queued before the cancel.</item>
/// <item>A resource ready after its entry is gone (a dial that completes after its cancel, or
/// after stop) is refused by <see cref="Adopt"/> and closed at once by the caller.</item>
/// <item><see cref="CloseAsync"/> (stop) marks the registry closed, so nothing registers after;
/// cancels everything; and waits for the work to finish, within a bound.</item>
/// </list>
/// </remarks>
internal sealed class InFlight(TimeProvider? time = null)
{
    private readonly TimeProvider time = time ?? TimeProvider.System;
    private readonly object gate = new();
    private readonly Dictionary<ulong, Entry> entries = new();
    private readonly HashSet<Task> work = new();
    private bool closed;

    /// <summary>How many tracked tasks are still running: for tests, to wait for the work, not the clock.</summary>
    internal int Working
    {
        get
        {
            lock (this.gate)
            {
                return this.work.Count;
            }
        }
    }

    /// <summary>Whether effect <paramref name="id"/> is in flight.</summary>
    public bool Has(ulong id)
    {
        lock (this.gate)
        {
            return this.entries.ContainsKey(id);
        }
    }

    /// <summary>
    /// Effect <paramref name="id"/> starts: the token that aborts its work (also after
    /// <paramref name="timeout"/>, the core's own time for it, if given), or null if the registry
    /// is closed (then it doesn't start). An entry of that id still there (never, from the real
    /// core) is given up on first.
    /// </summary>
    public CancellationToken? Register(ulong id, TimeSpan? timeout = null)
    {
        Entry? old;
        CancellationToken token;
        lock (this.gate)
        {
            if (this.closed)
            {
                return null;
            }

            var cts = new CancellationTokenSource(Timeout.InfiniteTimeSpan, this.time);
            if (timeout is { } t)
            {
                cts.CancelAfter(t);
            }

            this.entries.Remove(id, out old);
            this.entries[id] = new Entry(cts);
            token = cts.Token;
        }

        old?.Stop();
        return token;
    }

    /// <summary>
    /// On the loop: effect <paramref name="id"/> is over for the core, but its work goes on to
    /// finish by itself (a stream closed gracefully, flushing): its entry goes, without aborting
    /// it, and nothing more about it reaches the core.
    /// </summary>
    public bool Forget(ulong id)
    {
        lock (this.gate)
        {
            return this.entries.Remove(id);
        }
    }

    /// <summary>
    /// What effect <paramref name="id"/> holds is ready (a connection that opened, a file being
    /// written): <paramref name="release"/> lets go of it on a cancel. False if the effect was
    /// cancelled or the registry closed first: release it at once.
    /// </summary>
    public bool Adopt(ulong id, Action release)
    {
        lock (this.gate)
        {
            if (this.closed || !this.entries.TryGetValue(id, out var entry))
            {
                return false;
            }

            entry.Release = release;
            return true;
        }
    }

    /// <summary>Count <paramref name="task"/>, an effect's work, until it ends: stop waits for it.</summary>
    public void Track(Task task)
    {
        lock (this.gate)
        {
            this.work.Add(task);
        }

        task.ContinueWith(
            done =>
            {
                lock (this.gate)
                {
                    this.work.Remove(done);
                }
            },
            TaskScheduler.Default);
    }

    /// <summary>
    /// On the loop: whether <paramref name="q"/> may go to the core. An application's input may; a
    /// result only while its effect is in flight, and a final one ends it.
    /// </summary>
    public bool Take(Queued q)
    {
        if (q.Of is not { } id)
        {
            return true;
        }

        lock (this.gate)
        {
            if (!this.entries.TryGetValue(id, out var entry))
            {
                return false;
            }

            if (q.Final)
            {
                this.entries.Remove(id);
                entry.Cts.Dispose();
            }

            return true;
        }
    }

    /// <summary>
    /// On the loop: give up on effect <paramref name="id"/> at once: its entry goes, its work is
    /// aborted and what it holds is released. Nothing more about it reaches the core.
    /// covers: RULE-CLIENT-15
    /// </summary>
    public void Cancel(ulong id)
    {
        Entry? entry;
        lock (this.gate)
        {
            this.entries.Remove(id, out entry);
        }

        entry?.Stop();
    }

    /// <summary>
    /// Stop: nothing registers from now on, everything in flight is cancelled, and the task ends
    /// when the work has finished, or at <paramref name="bound"/>, whichever comes first.
    /// </summary>
    public Task CloseAsync(TimeSpan bound)
    {
        List<Entry> all;
        Task[] running;
        lock (this.gate)
        {
            this.closed = true;
            all = [.. this.entries.Values];
            this.entries.Clear();
        }

        foreach (var entry in all)
        {
            entry.Stop();
        }

        // What the releases started (a file's deletion) is waited for too.
        lock (this.gate)
        {
            running = [.. this.work];
        }

        return this.WithinAsync(Task.WhenAll(running), bound);
    }

    /// <summary>
    /// Wait for <paramref name="work"/> until <paramref name="bound"/>, whichever comes first, never
    /// throwing. The bound's timer goes as soon as the work is over: left to run out, every stopped
    /// client kept one armed for the rest of its stop timeout.
    /// </summary>
    private async Task WithinAsync(Task work, TimeSpan bound)
    {
        if (work.IsCompleted)
        {
            return;
        }

        using var cts = new CancellationTokenSource();
        await Task.WhenAny(work, Task.Delay(bound, this.time, cts.Token)).ConfigureAwait(false);
        cts.Cancel();
    }

    private sealed class Entry(CancellationTokenSource cts)
    {
        public CancellationTokenSource Cts { get; } = cts;

        public Action? Release { get; set; }

        /// <summary>Abort the work and let go of what it holds.</summary>
        public void Stop()
        {
            try
            {
                this.Cts.Cancel();
            }
            catch (ObjectDisposedException)
            {
            }
            catch (AggregateException)
            {
                // a callback of the work's own failed: it is being given up on anyway
            }

            try
            {
                this.Release?.Invoke();
            }
            catch (ObjectDisposedException)
            {
            }
        }
    }
}
