// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using Pydantic.OpAmp.Client;
using Pydantic.OpAmp.Client.Sdk;
using Pydantic.OpAmp.Otel;
using Xunit;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

public class ReachabilityTests
{
    private const string Uid = "00000000-0000-0000-0000-000000000007";

    private sealed class Said
    {
        public List<string> Lines { get; } = [];

        public AgentLog Log => new(m => this.Lines.Add($"Info: {m}"), m => this.Lines.Add($"Warn: {m}"),
            m => this.Lines.Add($"Error: {m}"), m => this.Lines.Add($"Debug: {m}"));
    }

    [Fact]
    public void ConnectingFailingAndRecoveringAreEachSaidOnce()
    {
        var said = new Said();
        var r = new Reachability(new Uri("https://user:secret@opamp.example.com/v1/opamp?token=x"), SettingNames.Env, "checkout", Uid, said.Log);
        r.Notified(new OpAmpNotification.ConnectFailed(401, "HTTP 401"));
        r.Notified(new OpAmpNotification.ConnectFailed(401, "HTTP 401"));
        r.Notified(new OpAmpNotification.Connected());
        r.Notified(new OpAmpNotification.Connected());
        r.Notified(new OpAmpNotification.ConnectFailed(null, "connection refused"));
        r.Notified(new OpAmpNotification.Connected());
        Assert.Equal(
            [
                "Warn: can't reach https://opamp.example.com/v1/opamp (HTTP 401): the server refused this agent's credentials: " +
                "check OTEL_OPAMP_HEADERS (Authorization=Bearer <token>). Retrying in the background; the SDK keeps its current settings meanwhile",
                "Info: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance " + Uid,
                "Warn: can't reach https://opamp.example.com/v1/opamp (connection refused): check OTEL_OPAMP_ENDPOINT and that the server is up. " +
                "Retrying in the background; the SDK keeps its current settings meanwhile",
                "Info: connected to https://opamp.example.com/v1/opamp again",
            ],
            said.Lines.Where(l => !l.StartsWith("Debug: ", StringComparison.Ordinal)));
        Assert.Equal(["Debug: still can't reach https://opamp.example.com/v1/opamp: HTTP 401"], said.Lines.Where(l => l.StartsWith("Debug: ", StringComparison.Ordinal)));
        Assert.DoesNotContain(said.Lines, l => l.Contains("secret", StringComparison.Ordinal) || l.Contains("token=x", StringComparison.Ordinal));
    }

    /// <summary>
    /// After the server moved the agent to another endpoint, what it says names the one the client
    /// uses now (logged without credentials), not the one configured.
    /// </summary>
    [Fact]
    public void WhatIsSaidNamesTheEndpointInUse()
    {
        var said = new Said();
        string? inUse = null; // none yet: the configured one
        var r = new Reachability(new Uri("https://a.example.com/v1/opamp"), SettingNames.Env, string.Empty, Uid, said.Log) { InUse = () => inUse };
        r.Notified(new OpAmpNotification.Connected());
        inUse = "https://user:secret@b.example.com/v1/opamp?token=x";
        r.Notified(new OpAmpNotification.ConnectFailed(null, "connection refused"));
        r.Notified(new OpAmpNotification.Connected());
        Assert.Equal(3, said.Lines.Count);
        Assert.StartsWith("Info: connected to https://a.example.com/v1/opamp;", said.Lines[0], StringComparison.Ordinal);
        Assert.StartsWith("Warn: can't reach https://b.example.com/v1/opamp (connection refused)", said.Lines[1], StringComparison.Ordinal);
        Assert.Equal("Info: connected to https://b.example.com/v1/opamp again", said.Lines[2]);
        Assert.DoesNotContain(said.Lines, l => l.Contains("secret", StringComparison.Ordinal) || l.Contains("token=x", StringComparison.Ordinal));
    }

    [Fact]
    public void WithoutAServiceNameTheInstanceIsNamedAlone()
    {
        var said = new Said();
        new Reachability(new Uri("wss://opamp.example.com/v1/opamp"), SettingNames.Env, string.Empty, Uid, said.Log).Notified(new OpAmpNotification.Connected());
        Assert.Equal(["Info: connected to wss://opamp.example.com/v1/opamp; this SDK is managed, instance " + Uid], said.Lines);
    }

    [Theory]
    [InlineData(404, "nothing serves OpAMP at that path: check OTEL_OPAMP_ENDPOINT (it usually ends in /v1/opamp)")]
    [InlineData(403, "the server refused this agent's credentials: check OTEL_OPAMP_HEADERS (Authorization=Bearer <token>)")]
    [InlineData(503, "the server answered with an error")]
    public void EachFailureSaysWhatToCheck(int status, string hint) => Assert.Equal(hint, Reachability.Hint(status, SettingNames.Env));

    [Theory]
    [InlineData("https://user:secret@opamp.example.com:4320/v1/opamp?token=x#f", "https://opamp.example.com:4320/v1/opamp")]
    [InlineData("https://user:secret@opamp.example.com:notaport/v1/opamp?token=x", "https://opamp.example.com:notaport/v1/opamp")]
    [InlineData("https://[::1/v1/opamp?k=v", "https://[::1/v1/opamp")]
    [InlineData("https://[::1]:4320", "https://[::1]:4320")]
    [InlineData("user:secret@opamp.example.com/v1/opamp?token=x", "opamp.example.com/v1/opamp")]
    public void TheEndpointIsLoggedWithoutUserInfoOrQuery(string endpoint, string logged) => Assert.Equal(logged, Reachability.Where(endpoint));

    [Fact]
    public void AnEndpointWithoutASchemeIsWarnedAbout()
    {
        var said = new Said();
        var server = new Server(new Uri("opamp.example.com/v1/opamp", UriKind.Relative), new Dictionary<string, string>(), null, TimeSpan.Zero);
        _ = new SdkAgent(server, _ => null, null, _ => new Host(), said.Log, new Controls());
        Assert.Equal(
            ["Warn: OTEL_OPAMP_ENDPOINT 'opamp.example.com/v1/opamp' has no scheme: name it in full, as https://host/v1/opamp or wss://host/v1/opamp"],
            said.Lines);
    }
}
