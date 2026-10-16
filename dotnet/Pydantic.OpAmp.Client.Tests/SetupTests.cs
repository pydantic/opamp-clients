// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Google.Protobuf;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// The core's Setup call, through the native library: off without an endpoint; the endpoint, the
/// headers and the heartbeat as it reads them; the instance UID saved, or made and to be saved.
/// </summary>
public class SetupTests
{
    [Fact]
    public void SetupIsTheCoresCall()
    {
        Assert.True(OpAmpSetup.Start(new SetupStart()).Off);
        var s = OpAmpSetup.Start(new SetupStart
        {
            Env =
            {
                new Variable { Name = "OTEL_OPAMP_ENDPOINT", Value = "https://user:pw@opamp.example.com/v1/opamp?k=v" },
                new Variable { Name = "OTEL_OPAMP_HEADERS", Value = "Authorization=Bearer%20k" },
                new Variable { Name = "OTEL_OPAMP_HEARTBEAT_INTERVAL", Value = "30s" },
                new Variable { Name = "OTEL_OPAMP_STATE_DIR", Value = "none" },
            },
        });
        Assert.False(s.Off);
        Assert.Equal("https://opamp.example.com/v1/opamp", s.ShownEndpoint);
        Assert.Equal("Bearer k", Assert.Single(s.Headers).Value);
        Assert.Equal(30_000UL, s.HeartbeatIntervalMs);
        Assert.Equal(string.Empty, s.StateDir);
        Assert.Empty(s.DefaultSlots);

        const string saved = "0199e3a0-0000-7000-8000-000000000007";
        var entropy = ByteString.CopyFrom(new byte[16]);
        var id = OpAmpSetup.Identify(new SetupIdentity { StateDir = true, Saved = saved + "\n", Entropy = entropy });
        Assert.Equal(SetupUidFrom.Saved, id.From);
        Assert.False(id.Save);
        Assert.Equal(saved, id.Text);
        var made = OpAmpSetup.Identify(new SetupIdentity { StateDir = true, Entropy = entropy, UnixMs = 1 });
        Assert.Equal(SetupUidFrom.Made, made.From);
        Assert.True(made.Save);
        Assert.Equal(16, made.InstanceUid.Length);
        // No entropy: the core refuses the call.
        Assert.Throws<CoreException>(() => OpAmpSetup.Identify(new SetupIdentity()));
    }
}
