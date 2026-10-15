// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Collections.Concurrent;
using System.Net.Sockets;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// An output that says stopped starts nothing (RULE-SHELL-07): no request, WebSocket connection,
/// download, stream or name resolution, whatever else it carries; what it would start could only
/// be given up on. The real core puts none there, so the test adds them to that output
/// (<see cref="Seams.Stepped"/> runs before an output's effects are carried out) and watches the
/// transport and the network for any of them.
/// </summary>
public class StoppedOutputTests
{
    private const string Trap = "trap.invalid";

    // covers: RULE-SHELL-07
    [Theory]
    [InlineData(OpAmpMode.DotNet)]
    [InlineData(OpAmpMode.Rust)]
    public async Task AnOutputThatSaysStoppedStartsNothing(OpAmpMode mode)
    {
        using var server = new TestServer();
        var dir = Directory.CreateTempSubdirectory("opamp-stopped-output-");
        try
        {
            var rust = mode == OpAmpMode.Rust;
            var options = new OpAmpClientOptions
            {
                Endpoint = new Uri(server.Endpoint("http")),
                HeartbeatInterval = TimeSpan.FromHours(1),
                StopTimeout = TimeSpan.FromSeconds(2),
                Mode = mode,
                DirectConnection = rust,
                StateFile = Path.Combine(dir.FullName, "state"),
                AcceptPackages = true,
            };
            using var transport = new Watching(rust ? ClientTransport.Create(null, null, direct: true) : ClientTransport.Create(options));
            var net = new WatchingNet();
            var added = 0;
            var client = OpAmpClient.Start(options, new OpAmpStatus(), null, new Seams
            {
                Transport = transport,
                Net = rust ? net : null,
                Stepped = (_, output) =>
                {
                    if (output.Stopped && Interlocked.Exchange(ref added, 1) == 0)
                    {
                        output.Effects.Add(Starts());
                    }
                },
            });
            Assert.True(await Wait.ForAsync(() => !server.Messages.IsEmpty), "no request was sent");
            await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
            await Task.Delay(300); // what was started late would show by now
            Assert.Equal(1, Volatile.Read(ref added));
            Assert.Empty(transport.Trapped);
            Assert.Empty(net.Trapped);
        }
        finally
        {
            dir.Delete(recursive: true);
        }
    }

    /// <summary>One of everything that starts something, aimed at a host nothing else uses.</summary>
    private static Effect[] Starts() =>
    [
        new() { HttpRequest = new HttpRequest { Id = 900_001, Url = $"http://{Trap}/v1/opamp", TimeoutMs = 10_000, MaxResponseBytes = 1 << 20 } },
        new() { WsOpen = new WsOpen { Id = 900_002, Url = $"ws://{Trap}/v1/opamp", TimeoutMs = 10_000, MaxMessageBytes = 1 << 20 } },
        new() { Download = new Download { Id = 900_003, Url = $"http://{Trap}/file", MaxBytes = 1 << 20, TimeoutMs = 10_000 } },
        new() { StreamOpen = new StreamOpen { Id = 900_004, Host = Trap, Port = 80 } },
        new() { StreamResolve = new StreamName { Id = 900_005, Host = Trap } },
    ];

    /// <summary>The client's transport, noting every request and connection to the trap (which then fail).</summary>
    private sealed class Watching(IClientTransport inner) : IClientTransport
    {
        public ConcurrentQueue<string> Trapped { get; } = new();

        public Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken ct)
        {
            if (request.RequestUri?.Host == Trap)
            {
                this.Trapped.Enqueue($"{request.Method} {request.RequestUri}");
                return Task.FromException<HttpResponseMessage>(new HttpRequestException("the trap"));
            }

            return inner.SendAsync(request, ct);
        }

        public Task<System.Net.WebSockets.WebSocket> ConnectAsync(Uri url, IEnumerable<KeyValuePair<string, string>> headers, CancellationToken ct)
        {
            if (url.Host == Trap)
            {
                this.Trapped.Enqueue($"connect {url}");
                return Task.FromException<System.Net.WebSockets.WebSocket>(new HttpRequestException("the trap"));
            }

            return inner.ConnectAsync(url, headers, ct);
        }

        public void Dispose() => inner.Dispose();
    }

    /// <summary>Rust mode's network, noting every connection to the trap and every resolution of it.</summary>
    private sealed class WatchingNet : StreamsNet
    {
        public ConcurrentQueue<string> Trapped { get; } = new();

        public override Task<Stream> ConnectAsync(string host, int port, CancellationToken ct)
        {
            if (host == Trap)
            {
                this.Trapped.Enqueue($"stream to {host}:{port}");
                return Task.FromException<Stream>(new SocketException((int)SocketError.ConnectionRefused));
            }

            return base.ConnectAsync(host, port, ct);
        }

        public override Task<IReadOnlyList<string>> ResolveAsync(string host, CancellationToken ct)
        {
            if (host == Trap)
            {
                this.Trapped.Enqueue($"resolve {host}");
                return Task.FromException<IReadOnlyList<string>>(new SocketException((int)SocketError.HostNotFound));
            }

            return base.ResolveAsync(host, ct);
        }
    }
}
