// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

public class StopOnErrorTests
{
    [Fact]
    public async Task ACallbackThatThrowsStopsTheClientWhichSaysSoOnce()
    {
        var bug = new InvalidOperationException("the application's bug");
        var notes = new ConcurrentQueue<OpAmpNotification>();
        var stopped = new TaskCompletionSource<OpAmpNotification.Stopped>();
        var client = OpAmpClient.Start(
            new OpAmpClientOptions { Mode = OpAmpMode.DotNet, Endpoint = new Uri("http://127.0.0.1:1"), InstanceUid = new byte[16] },
            new OpAmpStatus(),
            n =>
            {
                notes.Enqueue(n);
                if (n is OpAmpNotification.Stopped s)
                {
                    stopped.TrySetResult(s);
                }

                // The refused connection is told, and the callback throws; Stopped is thrown at too.
                throw bug;
            });

        // The application hears without calling StopAsync.
        var s = await stopped.Task.WaitAsync(TimeSpan.FromSeconds(10));
        Assert.Same(bug, s.Error);
        Assert.Single(notes.OfType<OpAmpNotification.Stopped>());
        var e = await Assert.ThrowsAsync<InvalidOperationException>(() => client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10)));
        Assert.Same(bug, e);
    }
}
