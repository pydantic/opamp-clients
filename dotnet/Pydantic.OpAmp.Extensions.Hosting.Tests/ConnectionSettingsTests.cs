// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Net;
using Google.Protobuf;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using OpAmp.Proto.V1;
using Xunit;

namespace Pydantic.OpAmp.Extensions.Hosting.Tests;

/// <summary>
/// A key of the agent's own (docs/connection-settings.md): with a state directory the agent says it
/// takes OpAMP connection settings, takes the key the server offers, says APPLIED on it, and keeps it
/// for the next start; without one it says nothing of the kind.
/// </summary>
public class ConnectionSettingsTests
{
    private const ulong TakesKeys = (ulong)(AgentCapabilities.AcceptsOpAmpconnectionSettings | AgentCapabilities.ReportsConnectionSettingsStatus);

    private static CancellationToken Cancel => TestContext.Current.CancellationToken;

    /// <summary>The agent under <paramref name="server"/> with the enrollment key, until <paramref name="until"/> holds.</summary>
    private static async Task RunAgent(KeyServer server, string stateDirectory, Func<bool> until, string what)
    {
        var b = Host.CreateApplicationBuilder(new HostApplicationBuilderSettings { DisableDefaults = true });
        var home = Kit.TempDir();
        b.Configuration.AddInMemoryCollection(new Dictionary<string, string?>
        {
            ["XDG_STATE_HOME"] = home,
            ["HOME"] = home,
            ["LOCALAPPDATA"] = home,
            ["OpAmp:Endpoint"] = server.Endpoint,
            ["OpAmp:Headers:Authorization"] = "Bearer enroll",
            ["OpAmp:StateDirectory"] = stateDirectory,
            ["OpAmp:HeartbeatInterval"] = "100ms",
        });
        b.Services.AddOpenTelemetry().WithOpAmp();
        using var host = b.Build();
        await host.StartAsync(Cancel);
        await Kit.Until(() => { lock (server) { return until(); } }, what);
        await host.StopAsync(Cancel);
    }

    [Fact]
    public async Task WithAStateDirectoryTheAgentTakesAKeyOfItsOwnAndKeepsIt()
    {
        await using var server = new KeyServer();
        var dir = Kit.TempDir();
        await RunAgent(server, dir, () => server.Applied, "the key offered APPLIED on it");
        int before;
        lock (server)
        {
            Assert.Equal(TakesKeys, server.Caps[0] & TakesKeys);
            before = server.Auth.Count;
        }

        await RunAgent(server, dir, () => server.Auth.Count > before, "the next start reaching the server");
        lock (server)
        {
            Assert.Equal("Bearer own", server.Auth[before]);
        }
    }

    [Fact]
    public async Task WithoutAStateDirectoryTheAgentDoesntSayItTakesConnectionSettings()
    {
        await using var server = new KeyServer();
        await RunAgent(server, "none", () => server.Caps.Count >= 2, "two messages");
        lock (server)
        {
            Assert.All(server.Caps, c => Assert.Equal(0UL, c & TakesKeys));
            Assert.All(server.Auth, a => Assert.Equal("Bearer enroll", a));
        }
    }

    /// <summary>
    /// An OpAMP server over plain HTTP that takes an enrollment key and, as the reference server does,
    /// hands an agent that says it takes connection settings a key of its own in its answer to the
    /// agent's first message with the enrollment key; once the agent uses its own key, the enrollment
    /// key speaks for it no more (401). Its lists are read under a lock on it.
    /// </summary>
    private sealed class KeyServer : IAsyncDisposable
    {
        private static readonly ByteString OfferHash = ByteString.CopyFromUtf8("own-key-offer");
        private readonly HttpListener listener = new();
        private readonly Task serving;
        private bool ownUsed;

        public KeyServer()
        {
            var port = Kit.FreePort();
            this.Endpoint = $"http://127.0.0.1:{port}/v1/opamp";
            this.listener.Prefixes.Add($"http://127.0.0.1:{port}/");
            this.listener.Start();
            this.serving = Task.Run(this.Serve);
        }

        public string Endpoint { get; }

        public List<ulong> Caps { get; } = [];

        public List<string?> Auth { get; } = [];

        public bool Applied { get; private set; }

        public async ValueTask DisposeAsync()
        {
            this.listener.Close();
            await this.serving;
        }

        private async Task Serve()
        {
            while (this.listener.IsListening)
            {
                HttpListenerContext ctx;
                try
                {
                    ctx = await this.listener.GetContextAsync();
                }
                catch (Exception)
                {
                    return;
                }

                using var buf = new MemoryStream();
                await ctx.Request.InputStream.CopyToAsync(buf);
                var m = AgentToServer.Parser.ParseFrom(buf.ToArray());
                var auth = ctx.Request.Headers["Authorization"];
                var reply = new ServerToAgent { InstanceUid = m.InstanceUid };
                var refused = false;
                lock (this)
                {
                    this.Auth.Add(auth);
                    var enroll = auth == "Bearer enroll";
                    if ((!enroll && auth != "Bearer own") || (enroll && this.ownUsed))
                    {
                        refused = true;
                    }
                    else
                    {
                        this.ownUsed |= !enroll;
                        this.Caps.Add(m.Capabilities);
                        if (!enroll && m.ConnectionSettingsStatus is { Status: ConnectionSettingsStatuses.Applied } s &&
                            s.LastConnectionSettingsHash == OfferHash)
                        {
                            this.Applied = true;
                        }

                        if (enroll && (m.Capabilities & TakesKeys) == TakesKeys)
                        {
                            reply.ConnectionSettings = new ConnectionSettingsOffers
                            {
                                Hash = OfferHash,
                                Opamp = new OpAMPConnectionSettings
                                {
                                    DestinationEndpoint = this.Endpoint,
                                    Headers = new Headers { Headers_ = { new Header { Key = "Authorization", Value = "Bearer own" } } },
                                    HeartbeatIntervalSeconds = 1,
                                },
                            };
                        }
                    }
                }

                if (refused)
                {
                    ctx.Response.StatusCode = 401;
                    ctx.Response.Close();
                    continue;
                }

                var bytes = reply.ToByteArray();
                ctx.Response.ContentType = "application/x-protobuf";
                ctx.Response.ContentLength64 = bytes.Length;
                await ctx.Response.OutputStream.WriteAsync(bytes);
                ctx.Response.Close();
            }
        }
    }
}
