// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Diagnostics;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>The in-flight registry's invariants, alone and under seeded interleavings.</summary>
public class InFlightTests
{
    private static Queued Result(ulong id, bool final) => new(new Input { HttpFailed = new Failed { Id = id } }, id, final);

    [Fact]
    public void ALateResultIsDropped()
    {
        var f = new InFlight();
        Assert.NotNull(f.Register(1));
        f.Cancel(1);
        Assert.False(f.Take(Result(1, final: true)));
        Assert.False(f.Take(Result(9, final: false))); // never registered
        Assert.True(f.Take(new Queued(new Input { Timer = new Empty() }))); // the application's
    }

    [Fact]
    public void AFinalResultEndsTheEffect()
    {
        var f = new InFlight();
        f.Register(1);
        Assert.True(f.Take(Result(1, final: false)));
        Assert.True(f.Take(Result(1, final: true)));
        Assert.False(f.Take(Result(1, final: true)));
        Assert.False(f.Has(1));
    }

    [Fact]
    public async Task ALateResourceIsRefusedAndNothingRegistersAfterClose()
    {
        var f = new InFlight();
        var token = f.Register(1)!.Value;
        f.Cancel(1);
        Assert.True(token.IsCancellationRequested);
        Assert.False(f.Adopt(1, () => throw new InvalidOperationException("never held")));

        var released = 0;
        var stillOpen = f.Register(2)!.Value;
        Assert.True(f.Adopt(2, () => released++));
        f.Register(3);
        await f.CloseAsync(TimeSpan.FromSeconds(1));
        Assert.True(stillOpen.IsCancellationRequested);
        Assert.Equal(1, released);
        Assert.False(f.Adopt(3, () => { }));
        Assert.Null(f.Register(4));
        Assert.False(f.Take(Result(2, final: true)));
    }

    [Fact]
    public async Task StopIsBounded()
    {
        var f = new InFlight();
        f.Register(1);
        f.Track(new TaskCompletionSource().Task); // never ends
        var done = new TaskCompletionSource();
        f.Track(done.Task);
        var clock = Stopwatch.StartNew();
        await f.CloseAsync(TimeSpan.FromMilliseconds(200)).WaitAsync(TimeSpan.FromSeconds(5));
        Assert.InRange(clock.ElapsedMilliseconds, 150, 3000);

        // Work that ends is waited for, no longer.
        var g = new InFlight();
        var ends = Task.Delay(100);
        g.Track(ends);
        clock.Restart();
        await g.CloseAsync(TimeSpan.FromSeconds(10));
        Assert.True(ends.IsCompleted);
        Assert.True(clock.ElapsedMilliseconds < 5000);
    }

    // Workers adopt resources and queue results while the loop takes, cancels and finally closes,
    // in an order each seed picks. Whatever the order: no result of an effect reaches the core
    // after the loop cancelled it; every resource adopted is released once if its effect was
    // cancelled or the registry closed, and never otherwise; a resource refused is never adopted.
    [Fact]
    public async Task UnderSeededInterleavingsTheInvariantsHold()
    {
        for (var seed = 0; seed < 300; seed++)
        {
            await OneRun(seed);
        }
    }

    private static async Task OneRun(int seed)
    {
        const int Ids = 8;
        var rng = new Random(seed);
        var f = new InFlight();
        var queue = new BlockingCollection<Queued>();
        var released = new int[Ids + 1];
        var adopted = new bool[Ids + 1];
        var finalTaken = new bool[Ids + 1];
        var cancelled = new HashSet<ulong>();
        for (ulong id = 1; id <= Ids; id++)
        {
            Assert.NotNull(f.Register(id));
        }

        // Workers: each effect's I/O, on the thread pool, with seeded pauses.
        var workers = Enumerable.Range(1, Ids).Select(i =>
        {
            var id = (ulong)i;
            var pauses = new Random(seed * 31 + i);
            return Task.Run(() =>
            {
                Thread.SpinWait(pauses.Next(2000));
                adopted[id] = f.Adopt(id, () => Interlocked.Increment(ref released[id]));
                for (var n = pauses.Next(4); n > 0; n--)
                {
                    Thread.SpinWait(pauses.Next(500));
                    queue.Add(Result(id, final: false));
                }

                if (pauses.Next(2) == 0)
                {
                    queue.Add(Result(id, final: true));
                }
            });
        }).ToArray();

        // The loop: takes what is queued, cancels effects, in a seeded order.
        var all = Task.WhenAll(workers);
        while (!all.IsCompleted || queue.Count > 0)
        {
            if (rng.Next(3) == 0)
            {
                var id = (ulong)rng.Next(1, Ids + 1);
                f.Cancel(id);
                cancelled.Add(id);
            }
            else if (queue.TryTake(out var q, 1))
            {
                var taken = f.Take(q);
                var id = q.Of!.Value;
                Assert.False(taken && cancelled.Contains(id), $"seed {seed}: a result of {id} reached the core after its cancel");
                Assert.False(taken && finalTaken[id], $"seed {seed}: a result of {id} reached the core after its final one");
                finalTaken[id] |= taken && q.Final;
            }
        }

        await f.CloseAsync(TimeSpan.FromSeconds(1));
        Assert.Null(f.Register(Ids + 1));
        for (var id = 1; id <= Ids; id++)
        {
            var shouldRelease = adopted[id] && !finalTaken[id];
            Assert.True(released[id] == (shouldRelease ? 1 : 0), $"seed {seed}: effect {id} released {released[id]} times (adopted {adopted[id]}, ended {finalTaken[id]})");
        }
    }
}
