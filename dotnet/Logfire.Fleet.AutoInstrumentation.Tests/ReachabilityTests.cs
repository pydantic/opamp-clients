// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using Logfire.Fleet.Client;
using Logfire.Fleet.Client.Sdk;
using Logfire.Fleet.Otel;
using AgentLog = Logfire.Fleet.Otel.AgentLog;
using Xunit;

namespace Logfire.Fleet.AutoInstrumentation.Tests;

public class ReachabilityTests
{
    private const string Uid = "00000000-0000-0000-0000-000000000007";

    private sealed class Said
    {
        public List<string> Lines { get; } = [];

        public AgentLog Log => new(m => this.Lines.Add($"Info: {m}"), m => this.Lines.Add($"Warn: {m}"),
            m => this.Lines.Add($"Error: {m}"), m => this.Lines.Add($"Debug: {m}"));
    }

    /// <summary>
    /// An agent for an endpoint (the server and the hints as the core's Setup gives them for
    /// OTEL_OPAMP_*), the SDK built with a service name, without a client: what it says of the
    /// notifications it is told. <see cref="InUse"/>: the endpoint the client would say it uses now.
    /// </summary>
    private sealed class Told
    {
        private readonly SdkAgent agent;

        public Told(string endpoint, string service, AgentLog log)
        {
            string? Env(string n) => n switch
            {
                "OTEL_OPAMP_ENDPOINT" => endpoint,
                "OTEL_OPAMP_STATE_DIR" => "none",
                "OTEL_RESOURCE_ATTRIBUTES" => "service.instance.id=" + Uid,
                _ => null,
            };
            var server = Server.FromEnv(Env, _ => { })!;
            this.agent = new SdkAgent(server, Env, null, _ => new Host(), log, new Controls());
            this.agent.BuiltForTest(service);
        }

        public string? InUse { get; set; }

        public void Notified(OpAmpNotification n) => this.agent.Tell(n, this.InUse);
    }

    private static Told For(string endpoint, string service, AgentLog log) => new(endpoint, service, log);

    [Fact]
    public void ConnectingFailingAndRecoveringAreEachSaidOnce()
    {
        var said = new Said();
        var r = For("https://user:secret@opamp.example.com/v1/opamp?token=x", "checkout", said.Log);
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
    /// The first connection goes to Notice where the log has one (the plugin's: shown without
    /// OTEL_LOG_LEVEL), so a working SDK doesn't look like one not loaded; a reconnection stays Info.
    /// </summary>
    [Fact]
    public void TheFirstConnectionIsANotice()
    {
        var lines = new List<string>();
        var log = new AgentLog(m => lines.Add($"Info: {m}"), m => lines.Add($"Warn: {m}"), m => lines.Add($"Error: {m}"),
            _ => { }, m => lines.Add($"Notice: {m}"));
        var r = For("https://opamp.example.com/v1/opamp", "checkout", log);
        r.Notified(new OpAmpNotification.Connected());
        r.Notified(new OpAmpNotification.ConnectFailed(null, "connection refused"));
        r.Notified(new OpAmpNotification.Connected());
        Assert.Equal("Notice: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance " + Uid, lines[0]);
        Assert.Equal("Info: connected to https://opamp.example.com/v1/opamp again", lines[2]);
    }

    /// <summary>
    /// After the server moved the agent to another endpoint, what it says names the one the client
    /// uses now (logged without credentials), not the one configured.
    /// </summary>
    [Fact]
    public void WhatIsSaidNamesTheEndpointInUse()
    {
        var said = new Said();
        var r = For("https://a.example.com/v1/opamp", string.Empty, said.Log); // none in use yet: the configured one
        r.Notified(new OpAmpNotification.Connected());
        r.InUse = "https://user:secret@b.example.com/v1/opamp?token=x";
        r.Notified(new OpAmpNotification.ConnectFailed(null, "connection refused"));
        r.Notified(new OpAmpNotification.Connected());
        Assert.Equal(3, said.Lines.Count(l => !l.StartsWith("Debug: ", StringComparison.Ordinal)));
        var lines = said.Lines.Where(l => !l.StartsWith("Debug: ", StringComparison.Ordinal)).ToList();
        Assert.StartsWith("Info: connected to https://a.example.com/v1/opamp;", lines[0], StringComparison.Ordinal);
        Assert.StartsWith("Warn: can't reach https://b.example.com/v1/opamp (connection refused)", lines[1], StringComparison.Ordinal);
        Assert.Equal("Info: connected to https://b.example.com/v1/opamp again", lines[2]);
        Assert.DoesNotContain(said.Lines, l => l.Contains("secret", StringComparison.Ordinal) || l.Contains("token=x", StringComparison.Ordinal));
    }

    [Fact]
    public void WithoutAServiceNameTheInstanceIsNamedAlone()
    {
        var said = new Said();
        For("wss://opamp.example.com/v1/opamp", string.Empty, said.Log).Notified(new OpAmpNotification.Connected());
        Assert.Equal(["Info: connected to wss://opamp.example.com/v1/opamp; this SDK is managed, instance " + Uid],
            said.Lines.Where(l => !l.StartsWith("Debug: ", StringComparison.Ordinal)));
    }

    [Theory]
    [InlineData(404, "nothing serves OpAMP at that path: check OTEL_OPAMP_ENDPOINT (it usually ends in /v1/opamp)")]
    [InlineData(403, "the server refused this agent's credentials: check OTEL_OPAMP_HEADERS (Authorization=Bearer <token>)")]
    [InlineData(503, "the server answered with an error")]
    public void EachFailureSaysWhatToCheck(int status, string hint)
    {
        var said = new Said();
        For("http://s/v1/opamp", string.Empty, said.Log).Notified(new OpAmpNotification.ConnectFailed(status, string.Empty));
        Assert.Contains(said.Lines, l => l.StartsWith("Warn: ", StringComparison.Ordinal) && l.Contains($": {hint}. Retrying", StringComparison.Ordinal));
    }

    [Theory]
    [InlineData("https://user:secret@opamp.example.com:4320/v1/opamp?token=x#f", "https://opamp.example.com:4320/v1/opamp")]
    [InlineData("https://user:secret@opamp.example.com:notaport/v1/opamp?token=x", "https://opamp.example.com:notaport/v1/opamp")]
    [InlineData("https://[::1/v1/opamp?k=v", "https://[::1/v1/opamp")]
    [InlineData("https://[::1]:4320", "https://[::1]:4320")]
    public void TheEndpointIsLoggedWithoutUserInfoOrQuery(string endpoint, string logged) => Assert.Equal(logged, Server.ShownFor(endpoint, "?"));

    [Fact]
    public void AnEndpointWithoutASchemeIsRefused()
    {
        // Taken as http://, its key would go in the clear: no client starts, and the warning says
        // why, without the endpoint's secrets.
        var said = new Said();
        Assert.Null(Server.FromEnv(n => n == "OTEL_OPAMP_ENDPOINT" ? "user:secret@opamp.example.com/v1/opamp?token=x" : null, said.Log.Warn));
        Assert.Equal(
            ["Warn: off, as OTEL_OPAMP_ENDPOINT \"opamp.example.com/v1/opamp\" has no scheme: no OpAMP client is started; name it in " +
             "full, as https://host/v1/opamp or wss://host/v1/opamp"],
            said.Lines);
    }
}
