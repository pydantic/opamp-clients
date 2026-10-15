// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

public class UnknownEffectTests
{
    [Fact]
    public void AnEffectThisClientDoesntKnowIsAnError()
    {
        using var http = ClientTransport.Create(null, null);
        var executor = new Executor(http, _ => { });
        var e = Assert.Throws<InvalidOperationException>(() => executor.Run(new Effect()));
        Assert.Contains("older than its core", e.Message);
    }
}
