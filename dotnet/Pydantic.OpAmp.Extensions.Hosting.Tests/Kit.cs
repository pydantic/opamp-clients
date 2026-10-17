// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Collections.Concurrent;
using System.Net;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Xunit;

namespace Pydantic.OpAmp.Extensions.Hosting.Tests;

/// <summary>What the tests run the package against: an OpAMP server and a collector, on loopback.</summary>
internal static class Kit
{
    /// <summary>A remote config whose only file is the policy <paramref name="document"/>.</summary>
    public static AgentRemoteConfig Config(string document) => Pydantic.OpAmp.Tests.Policy.Config(document);

    public static string TempDir()
    {
        var dir = Path.Combine(Path.GetTempPath(), "opamp-hosting-" + Guid.NewGuid());
        Directory.CreateDirectory(dir);
        return dir;
    }

    public static int FreePort()
    {
        var l = new System.Net.Sockets.TcpListener(IPAddress.Loopback, 0);
        l.Start();
        var port = ((IPEndPoint)l.LocalEndpoint).Port;
        l.Stop();
        return port;
    }

    /// <summary>Wait for <paramref name="done"/> to hold, at most 20 s.</summary>
    public static async Task Until(Func<bool> done, string what)
    {
        var until = DateTime.UtcNow + TimeSpan.FromSeconds(20);
        while (!done())
        {
            Assert.True(DateTime.UtcNow < until, $"never: {what}");
            await Task.Delay(20, TestContext.Current.CancellationToken);
        }
    }
}

/// <summary>An OpAMP server over plain HTTP: it keeps every message, and offers a remote config in its first answer.</summary>
internal sealed class OpAmpServer : IAsyncDisposable
{
    private readonly HttpListener listener = new();
    private readonly Task serving;
    private int answered;

    public OpAmpServer(AgentRemoteConfig? offer = null)
    {
        this.Offer = offer;
        var port = Kit.FreePort();
        this.Endpoint = $"http://127.0.0.1:{port}/v1/opamp";
        this.listener.Prefixes.Add($"http://127.0.0.1:{port}/");
        this.listener.Start();
        this.serving = Task.Run(this.Serve);
    }

    public string Endpoint { get; }

    public AgentRemoteConfig? Offer { get; set; }

    /// <summary>Whether its answers ask for the available components in full.</summary>
    public bool AskForComponents { get; set; }

    public ConcurrentQueue<AgentToServer> Messages { get; } = new();

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
            this.Messages.Enqueue(m);
            var reply = new ServerToAgent { InstanceUid = m.InstanceUid };
            if (Interlocked.Increment(ref this.answered) == 1 && this.Offer is { } offer)
            {
                reply.RemoteConfig = offer;
            }

            if (this.AskForComponents)
            {
                reply.Flags = (ulong)ServerToAgentFlags.ReportAvailableComponents;
            }

            var bytes = reply.ToByteArray();
            ctx.Response.ContentType = "application/x-protobuf";
            ctx.Response.ContentLength64 = bytes.Length;
            await ctx.Response.OutputStream.WriteAsync(bytes);
            ctx.Response.Close();
        }
    }
}

/// <summary>A collector: it answers every export, and keeps each one's path.</summary>
internal sealed class Collector : IAsyncDisposable
{
    private readonly HttpListener listener = new();
    private readonly Task serving;

    public Collector()
    {
        var port = Kit.FreePort();
        this.Endpoint = $"http://127.0.0.1:{port}";
        this.listener.Prefixes.Add($"http://127.0.0.1:{port}/");
        this.listener.Start();
        this.serving = Task.Run(this.Serve);
    }

    public string Endpoint { get; }

    public ConcurrentQueue<string> Paths { get; } = new();

    /// <summary>Each export's path and body, as it arrived.</summary>
    public ConcurrentQueue<(string Path, byte[] Body)> Exports { get; } = new();

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

            using var body = new MemoryStream();
            await ctx.Request.InputStream.CopyToAsync(body);
            this.Exports.Enqueue((ctx.Request.Url!.AbsolutePath, body.ToArray()));
            this.Paths.Enqueue(ctx.Request.Url!.AbsolutePath);
            ctx.Response.StatusCode = 200;
            ctx.Response.ContentLength64 = 0;
            ctx.Response.Close();
        }
    }
}
