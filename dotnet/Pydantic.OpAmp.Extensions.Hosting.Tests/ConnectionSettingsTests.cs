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
    private static Task RunAgent(KeyServer server, string stateDirectory, Func<bool> until, string what) =>
        RunAgent(server, server, stateDirectory, until, what);

    /// <summary>
    /// The agent configured with <paramref name="server"/> and the enrollment key, until <paramref name="until"/>
    /// holds of <paramref name="watched"/> (under its lock); <paramref name="then"/> is given the running host.
    /// </summary>
    private static async Task RunAgent(
        KeyServer server, KeyServer watched, string stateDirectory, Func<bool> until, string what, Action<IHost>? then = null)
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
        await Kit.Until(() => { lock (watched) { return until(); } }, what);
        then?.Invoke(host);
        await host.StopAsync(Cancel);
    }

    private static ConnectionSettingsOffers Offer(string hash, string endpoint, string key) => new()
    {
        Hash = ByteString.CopyFromUtf8(hash),
        Opamp = new OpAMPConnectionSettings
        {
            DestinationEndpoint = endpoint,
            Headers = new Headers { Headers_ = { new Header { Key = "Authorization", Value = $"Bearer {key}" } } },
        },
    };

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
    /// Once it has its own key, A offers another key at server B's endpoint: the agent goes to B with
    /// it, says APPLIED of the move there, and the next start goes to B with it first.
    /// </summary>
    [Fact]
    public async Task TheAgentFollowsAMoveToAnotherServerAndKeepsIt()
    {
        await using var a = new KeyServer();
        await using var b = new KeyServer(["moved"], enroll: false);
        lock (a)
        {
            a.Next = Offer("move-to-b", b.Endpoint, "moved");
        }

        var dir = Kit.TempDir();
        await RunAgent(a, b, dir, () => b.Said(ConnectionSettingsStatuses.Applied, "move-to-b"), "the move APPLIED at B",
            // What the agent says of reaching its server names B from now on.
            host => Assert.Equal(b.Endpoint, host.Services.GetRequiredService<OpAmpAgent>().Agent?.EndpointSaid));
        int atA, atB;
        lock (b)
        {
            Assert.All(b.Auth, x => Assert.Equal("Bearer moved", x));
            atB = b.Auth.Count;
        }

        lock (a)
        {
            atA = a.Auth.Count;
        }

        await RunAgent(a, b, dir, () => b.Auth.Count > atB, "the next start reaching B");
        lock (a)
        {
            Assert.Equal(atA, a.Auth.Count); // the next start went to B, not to A
        }

        lock (b)
        {
            Assert.Equal("Bearer moved", b.Auth[atB]);
        }
    }

    /// <summary>A second key at the same endpoint: the agent takes it, keeps it across a restart, and never sends "own" again.</summary>
    [Fact]
    public async Task TheAgentTakesASecondKeyAndKeepsIt()
    {
        await using var server = new KeyServer(["own", "own2"]);
        lock (server)
        {
            server.Retires = "own2";
            server.Next = Offer("second-key", server.Endpoint, "own2");
        }

        var dir = Kit.TempDir();
        var since = -1;
        await RunAgent(server, dir, () =>
        {
            if (since < 0 && server.Said(ConnectionSettingsStatuses.Applied, "second-key"))
            {
                since = server.Auth.Count;
            }

            return since >= 0 && server.Auth.Count > since + 2;
        }, "the second key APPLIED, and used");
        int before;
        lock (server)
        {
            before = server.Auth.Count;
        }

        await RunAgent(server, dir, () => server.Auth.Count > before + 2, "the next start reaching the server");
        lock (server)
        {
            Assert.All(server.Auth.Skip(since), x => Assert.Equal("Bearer own2", x));
        }
    }

    /// <summary>A move nobody answers: the agent goes back with the key it has, says FAILED of it, and carries on.</summary>
    [Fact]
    public async Task AMoveNobodyAnswersFailsAndTheAgentStaysWhereItWas()
    {
        var nowhere = $"http://127.0.0.1:{Kit.FreePort()}/v1/opamp";
        await using var server = new KeyServer();
        lock (server)
        {
            server.Next = Offer("move-nowhere", nowhere, "moved");
        }

        var since = -1;
        await RunAgent(server, Kit.TempDir(), () =>
        {
            if (since < 0 && server.Said(ConnectionSettingsStatuses.Failed, "move-nowhere"))
            {
                since = server.Auth.Count;
            }

            return since >= 0 && server.Auth.Count > since + 2;
        }, "the move FAILED, and the agent carrying on");
        lock (server)
        {
            Assert.All(server.Auth.Skip(since), x => Assert.Equal("Bearer own", x));
            Assert.False(server.Said(ConnectionSettingsStatuses.Applied, "move-nowhere"));
        }
    }

    /// <summary>
    /// An OpAMP server over plain HTTP that takes an enrollment key and, as the reference server does,
    /// hands an agent that says it takes connection settings a key of its own in its answer to the
    /// agent's first message with the enrollment key; once the agent uses its own key, the enrollment
    /// key speaks for it no more (401). Once the agent has said APPLIED of its own key, it offers
    /// <see cref="Next"/> (if set) in answer to each message with it: another key, here or at another
    /// server's endpoint. Its lists are read under a lock on it.
    /// </summary>
    private sealed class KeyServer : IAsyncDisposable
    {
        private static readonly ByteString OfferHash = ByteString.CopyFromUtf8("own-key-offer");
        private readonly HttpListener listener = new();
        private readonly Task serving;
        private readonly string[] keys;
        private readonly bool enroll;
        private bool ownUsed;
        private bool ownGone;

        /// <summary>A server that takes <paramref name="keys"/> (default "own"), and the enrollment key with <paramref name="enroll"/>.</summary>
        public KeyServer(string[]? keys = null, bool enroll = true)
        {
            this.keys = keys ?? ["own"];
            this.enroll = enroll;
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

        /// <summary>Each status reported: its status and the hash it names.</summary>
        public List<(ConnectionSettingsStatuses Status, ByteString Hash)> Statuses { get; } = [];

        /// <summary>Offered in answer to each message with "own" once the agent said APPLIED of it.</summary>
        public ConnectionSettingsOffers? Next { get; set; }

        /// <summary>Once a request comes with this key, "own" speaks for the agent no more.</summary>
        public string? Retires { get; set; }

        /// <summary>Whether the agent reported <paramref name="status"/> of the offer <paramref name="hash"/> here (under the lock).</summary>
        public bool Said(ConnectionSettingsStatuses status, string hash) => this.Statuses.Contains((status, ByteString.CopyFromUtf8(hash)));

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
                    var key = auth is not null && auth.StartsWith("Bearer ", StringComparison.Ordinal) ? auth["Bearer ".Length..] : string.Empty;
                    if (!((enroll && this.enroll && !this.ownUsed) || (this.keys.Contains(key) && !(key == "own" && this.ownGone))))
                    {
                        refused = true;
                    }
                    else
                    {
                        this.ownUsed |= key == "own";
                        this.ownGone |= key == this.Retires;
                        this.Caps.Add(m.Capabilities);
                        if (m.ConnectionSettingsStatus is { } st)
                        {
                            this.Statuses.Add((st.Status, st.LastConnectionSettingsHash));
                        }

                        if (key == "own" && m.ConnectionSettingsStatus is { Status: ConnectionSettingsStatuses.Applied } s &&
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
                        else if (key == "own" && this.Next is not null && this.Applied)
                        {
                            reply.ConnectionSettings = this.Next;
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
