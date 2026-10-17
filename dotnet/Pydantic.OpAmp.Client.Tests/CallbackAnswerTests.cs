// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// What the application asks from inside the callback reaches the core before anything the client
/// takes in next (RULE-SHELL-10, spec/shell-contract.md), over the in-memory transport with the
/// real core: what the core is fed and what it sends are read at its boundary
/// (<see cref="Seams.Stepped"/>).
/// </summary>
public class CallbackAnswerTests
{
    private static readonly ByteString Hash = ByteString.CopyFromUtf8("c1");

    // How long each wait may take (none decides the outcome: each is for something that comes).
    private static readonly TimeSpan Patience = TimeSpan.FromSeconds(60);

    /// <summary>
    /// Found by the differential, in the Rust client (remote-config-restart-ws, on CI): after a
    /// restart the core hands the saved remote config to the application again, in the step that
    /// starts connecting, and reports it APPLYING until the application answers. The connection is
    /// asked for before the callback is called, and here it opens while the callback runs: its
    /// opening was in the client's queue before the callback's answer, was stepped first, and the
    /// first message said APPLYING where it should say APPLIED.
    /// </summary>
    // covers: RULE-SHELL-10
    [Fact]
    public async Task AConfigAnsweredInTheCallbackAtARestartIsNeverReportedApplying()
    {
        var dir = Directory.CreateTempSubdirectory("opamp-callback-answer-");
        try
        {
            var state = Path.Combine(dir.FullName, "state");

            // The first life: the server offers the config, the application applies it, and it is
            // saved. (Offered by the server, a config is APPLYING in the message that goes before
            // the application is told.)
            var first = await Life.RunAsync(state, restart: false);
            Assert.Equal([RemoteConfigStatuses.Applying, RemoteConfigStatuses.Applied], first.Statuses());

            // The second, on what the first saved: its connection opens while the callback runs.
            var second = await Life.RunAsync(state, restart: true);
            var status = second.Sent[0].RemoteConfigStatus;
            Assert.True(status is not null && status.LastRemoteConfigHash == Hash, $"the first message after the restart reports on the saved config: {second.Sent[0]}");
            Assert.True(status!.Status == RemoteConfigStatuses.Applied, $"the first message after the restart says {status.Status}; the core was fed {string.Join(", ", second.Inputs)}");
            Assert.DoesNotContain(RemoteConfigStatuses.Applying, second.Statuses());

            // The answer is what the core was fed after the step that handed the config over.
            var start = second.Inputs.IndexOf(Input.KindOneofCase.Start);
            Assert.Equal(Input.KindOneofCase.SetRemoteConfigStatus, second.Inputs[start + 1]);
        }
        finally
        {
            dir.Delete(recursive: true);
        }
    }

    /// <summary>
    /// One life of a WebSocket client on a state file, over the in-memory transport: its callback
    /// answers every remote config APPLIED before it returns. In a restart's life the connection
    /// is opened while the callback runs, before it answers: when the callback asks, the
    /// connection's opening is already queued for the loop.
    /// </summary>
    private sealed class Life
    {
        private readonly object gate = new();
        private readonly List<AgentToServer> sent = new();
        private readonly List<Input.KindOneofCase> inputs = new();
        private int saves;
        private int saved;

        /// <summary>The messages the client sent (the core's ws_send effects), in order.</summary>
        public List<AgentToServer> Sent
        {
            get
            {
                lock (this.gate)
                {
                    return [.. this.sent];
                }
            }
        }

        /// <summary>What the core was fed, in order.</summary>
        public List<Input.KindOneofCase> Inputs
        {
            get
            {
                lock (this.gate)
                {
                    return [.. this.inputs];
                }
            }
        }

        /// <summary>Whether every save the core asked for is on the disk (and there was one).</summary>
        public bool Saved
        {
            get
            {
                lock (this.gate)
                {
                    return this.saves > 0 && this.saved == this.saves;
                }
            }
        }

        /// <summary>The statuses reported for the remote config, in order.</summary>
        public RemoteConfigStatuses[] Statuses() =>
            this.Sent.Where(m => m.RemoteConfigStatus is { } st && st.LastRemoteConfigHash == Hash).Select(m => m.RemoteConfigStatus.Status).ToArray();

        public static async Task<Life> RunAsync(string state, bool restart)
        {
            var life = new Life();
            var transport = new MemoryTransport();
            var socket = new MemSocket(transport);
            var handle = new TaskCompletionSource<OpAmpClient>(TaskCreationOptions.RunContinuationsAsynchronously);
            var answered = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            var offered = 0;
            var waitingFor = "the remote config";
            if (!restart)
            {
                transport.OnCall = call => call.Accept(socket);
            }

            var client = OpAmpClient.Start(
                new OpAmpClientOptions
                {
                    Mode = OpAmpMode.DotNet,
                    Endpoint = new Uri("ws://opamp.test/v1/opamp"),
                    StateFile = state,
                    Capabilities = (ulong)(AgentCapabilities.AcceptsRemoteConfig | AgentCapabilities.ReportsRemoteConfig),
                    HeartbeatInterval = TimeSpan.FromHours(1),
                },
                new OpAmpStatus(),
                n =>
                {
                    if (n is not OpAmpNotification.RemoteConfig rc)
                    {
                        return;
                    }

                    var c = handle.Task.GetAwaiter().GetResult();
                    if (restart)
                    {
                        // The connection opens now, while the callback runs, and the client has
                        // begun to read it: its opening is queued for the loop before the answer is
                        // given. (The connection's task runs on the thread pool, which other
                        // tests keep busy: it may take a while.)
                        Volatile.Write(ref waitingFor, "the connection to be asked for");
                        var opening = SpinWait.SpinUntil(() => transport.Calls.Any(k => k.Kind == CallKind.Connect), Patience)
                            && transport.Calls.Single(k => k.Kind == CallKind.Connect).Accept(socket);
                        Volatile.Write(ref waitingFor, "the client to read the connection");
                        if (!opening || !SpinWait.SpinUntil(() => socket.Receiving, Patience))
                        {
                            throw new InvalidOperationException("the connection did not open while the callback ran");
                        }
                    }

                    c.SetRemoteConfigStatus(new RemoteConfigStatus { LastRemoteConfigHash = rc.Config.ConfigHash, Status = RemoteConfigStatuses.Applied });
                    answered.TrySetResult();
                },
                new Seams
                {
                    Transport = transport,
                    Stepped = (q, output) =>
                    {
                        var sends = output.Effects.Where(e => e.KindCase == Effect.KindOneofCase.WsSend)
                            .Select(e => AgentToServer.Parser.ParseFrom(e.WsSend.Data.Span[1..])).ToArray();
                        lock (life.gate)
                        {
                            life.inputs.Add(q.Input.KindCase);
                            life.sent.AddRange(sends);
                            life.saves += output.Effects.Count(e => e.KindCase == Effect.KindOneofCase.Store);
                            life.saved += q.Input.KindCase == Input.KindOneofCase.Stored ? 1 : 0;
                        }

                        // The server's answer to the first message of the first life offers the config.
                        if (!restart && sends.Length > 0 && Interlocked.Exchange(ref offered, 1) == 0)
                        {
                            var offer = new ServerToAgent
                            {
                                InstanceUid = sends[0].InstanceUid,
                                RemoteConfig = new AgentRemoteConfig
                                {
                                    ConfigHash = Hash,
                                    Config = new AgentConfigMap { ConfigMap = { [string.Empty] = new AgentConfigObject { Body = ByteString.CopyFromUtf8("x") } } },
                                },
                            };
                            socket.Push([0, .. offer.ToByteArray()]);
                        }
                    },
                });
            handle.SetResult(client);
            try
            {
                Assert.True(await Wait.ForAsync(() => answered.Task.IsCompleted, 3 * Patience.TotalSeconds), $"the callback did not answer: it waits for {Volatile.Read(ref waitingFor)}");
                Assert.True(await Wait.ForAsync(() => life.Statuses().Contains(RemoteConfigStatuses.Applied), Patience.TotalSeconds), "APPLIED was never reported");

                // What the next life starts on is saved before this one stops (a stop gives up on
                // a save still writing at its bound).
                Assert.True(restart || await Wait.ForAsync(() => life.Saved, Patience.TotalSeconds), "the config was never saved");
            }
            finally
            {
                await client.StopAsync().WaitAsync(Patience);
            }

            return life;
        }
    }
}
