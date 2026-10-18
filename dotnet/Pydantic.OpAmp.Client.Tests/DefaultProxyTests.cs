// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Net;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>These tests set the process's default proxy, so nothing else runs beside them.</summary>
[CollectionDefinition(nameof(DefaultProxy), DisableParallelization = true)]
public sealed class DefaultProxy
{
}

/// <summary>
/// With no proxy configured the client uses the platform's (<see cref="HttpClient.DefaultProxy"/>)
/// on requests, the WebSocket and downloads alike; <see cref="OpAmpClientOptions.DirectConnection"/>
/// goes direct.
/// </summary>
[Collection(nameof(DefaultProxy))]
public class DefaultProxyTests
{
    private static readonly byte[] Uid = Enumerable.Repeat((byte)0x42, 16).ToArray();

    // covers: RULE-PROXY-01
    [Theory]
    [InlineData("http", false)]
    [InlineData("ws", false)]
    [InlineData("http", true)]
    [InlineData("ws", true)]
    public async Task ThePlatformsProxyUnlessToldToGoDirect(string transport, bool direct)
    {
        using var server = new TestServer();
        using var via = new TestProxy("http");
        var saved = HttpClient.DefaultProxy;
        HttpClient.DefaultProxy = new WebProxy(new UriBuilder(via.Url) { UserName = string.Empty, Password = string.Empty }.Uri)
        {
            Credentials = new NetworkCredential(TestProxy.User, TestProxy.Password),
        };
        try
        {
            var client = OpAmpClient.Start(
                new OpAmpClientOptions
                {
                    Mode = OpAmpMode.Platform,
                    Endpoint = new Uri(server.Endpoint(transport)),
                    InstanceUid = Uid,
                    HeartbeatInterval = TimeSpan.FromMilliseconds(50),
                    DirectConnection = direct,
                },
                new OpAmpStatus());
            for (var i = 0; i < 1000 && server.Messages.Count < 2; i++)
            {
                await Task.Delay(10);
            }

            await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));

            // Downloads too.
            using var transportForDownloads = ClientTransport.Create(null, null, direct);
            var dir = Directory.CreateTempSubdirectory();
            var loop = new TestLoop(transportForDownloads, Path.Combine(dir.FullName, "state"));
            loop.Run(new Effect { Download = new Download { Id = 1, Url = $"http://127.0.0.1:{server.Port}/file", MaxBytes = 1 << 20, TimeoutMs = 10_000 } });
            Assert.True(loop.TryTake(out var downloaded, TimeSpan.FromSeconds(15)));
            await loop.X.ReleaseAsync(TimeSpan.FromSeconds(5));
            dir.Delete(recursive: true);

            Assert.True(server.Messages.Count >= 2);
            Assert.Equal(200u, downloaded!.DownloadResponse.Status);
            var upstreams = via.Tunnels.Select(t => t.Upstream).ToHashSet();
            if (direct)
            {
                Assert.Empty(via.Tunnels);
                Assert.Equal(0, via.Refused);
            }
            else
            {
                Assert.All(server.Messages, m => Assert.Contains(m.Remote, upstreams));
                Assert.Contains(via.Tunnels, t => t.Via == (transport == "ws" ? "connect" : "forward"));
                Assert.Contains(server.Gets, g => g == "/file");
                Assert.True(via.Tunnels.Count >= 2); // the client's, and the download's
            }
        }
        finally
        {
            HttpClient.DefaultProxy = saved;
        }
    }

    /// <summary>
    /// The environment's proxy URLs are held to the same schemes as <see cref="OpAmpClientOptions.Proxy"/>:
    /// .NET would use a SOCKS4 one, and ignore socks5h:// or an unknown scheme and connect directly.
    /// Neither happens: the client doesn't start. DirectConnection, or a Proxy, doesn't look at them.
    /// </summary>
    // covers: RULE-PROXY-01
    [Theory]
    [InlineData("ALL_PROXY", "socks4://127.0.0.1:1080", "SOCKS4")]
    [InlineData("all_proxy", "socks4a://127.0.0.1:1080", "SOCKS4")]
    [InlineData("HTTPS_PROXY", "SOCKS4://127.0.0.1:1080", "SOCKS4")]
    [InlineData("http_proxy", "ftp://127.0.0.1:1080", "ftp://")]
    [InlineData("ALL_PROXY", "socks5h://user:secret@127.0.0.1:1080", "socks5h://")]
    [InlineData("https_proxy", "socks://127.0.0.1:1080", "socks://")]
    [InlineData("ALL_PROXY", "user:secret@proxy://127.0.0.1:1080", "isn't a proxy URL")]
    [InlineData("ALL_PROXY", "socks5://127.0.0.1:1080", null)]
    [InlineData("HTTPS_PROXY", "http://127.0.0.1:3128", null)]
    [InlineData("HTTP_PROXY", "127.0.0.1:3128", null)]
    public async Task TheEnvironmentsProxyKeepsToTheSameSchemes(string name, string value, string? error)
    {
        var saved = Environment.GetEnvironmentVariable(name);
        Environment.SetEnvironmentVariable(name, value);
        try
        {
            var options = new OpAmpClientOptions { Mode = OpAmpMode.Platform, Endpoint = new Uri("http://localhost:1/v1/opamp"), InstanceUid = Uid };
            if (error is null)
            {
                await OpAmpClient.Start(options, new OpAmpStatus()).StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
            }
            else
            {
                var e = Assert.Throws<ArgumentException>(() => OpAmpClient.Start(options, new OpAmpStatus()));
                Assert.Contains(error, e.Message, StringComparison.Ordinal);
                Assert.Contains(name, e.Message, StringComparison.Ordinal);
                Assert.DoesNotContain("secret", e.Message, StringComparison.Ordinal);
            }

            // Not looked at when the client goes direct, or through its own proxy.
            await OpAmpClient.Start(options with { DirectConnection = true }, new OpAmpStatus()).StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
            await OpAmpClient.Start(options with { Proxy = new Uri("socks5://127.0.0.1:1") }, new OpAmpStatus()).StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        }
        finally
        {
            Environment.SetEnvironmentVariable(name, saved);
        }
    }

    /// <summary>A WebProxy the application sets as the default proxy is held to the same schemes too.</summary>
    // covers: RULE-PROXY-01
    [Theory]
    [InlineData("socks4://127.0.0.1:1080", "SOCKS4")]
    [InlineData("socks4a://127.0.0.1:1080", "SOCKS4")]
    [InlineData("socks5h://127.0.0.1:1080", "socks5h://")]
    public void TheApplicationsDefaultProxyKeepsToTheSameSchemes(string proxy, string error)
    {
        var saved = HttpClient.DefaultProxy;
        HttpClient.DefaultProxy = new WebProxy(new Uri(proxy));
        try
        {
            var e = Assert.Throws<ArgumentException>(() => OpAmpClient.Start(
                new OpAmpClientOptions { Mode = OpAmpMode.Platform, Endpoint = new Uri("http://localhost:1/v1/opamp"), InstanceUid = Uid }, new OpAmpStatus()));
            Assert.Contains(error, e.Message, StringComparison.Ordinal);
        }
        finally
        {
            HttpClient.DefaultProxy = saved;
        }
    }

    /// <summary>
    /// Under CGI (REQUEST_METHOD set) the environment's proxy variables may be the request's
    /// (httpoxy): the client takes none of them and goes direct. A configured proxy still applies,
    /// and so does one the application set as the default proxy.
    /// </summary>
    // covers: RULE-PROXY-01
    [Fact]
    public void UnderCgiOnlyAConfiguredProxyApplies()
    {
        var target = new Uri("http://example.com/v1/opamp");
        Func<string, string?> Cgi(string? httpProxy = null) => name => name switch
        {
            "REQUEST_METHOD" => "GET",
            "HTTP_PROXY" => httpProxy,
            _ => null,
        };

        // Nothing configured: direct, and the environment's variables aren't even looked at (an
        // unsupported scheme there is no error).
        using (var h = ClientTransport.CreateHandler(null, null, getenv: Cgi("socks4://web-proxy:3128")))
        {
            Assert.False(h.UseProxy);
        }

        // Configured: through it.
        using (var h = ClientTransport.CreateHandler(null, new Uri("http://configured:3128"), getenv: Cgi("http://web-proxy:3128")))
        {
            Assert.True(h.UseProxy);
            Assert.Equal(new Uri("http://configured:3128"), h.Proxy!.GetProxy(target));
        }

        // Set by the application as the default proxy: still the platform's, which is that.
        var saved = HttpClient.DefaultProxy;
        HttpClient.DefaultProxy = new WebProxy(new Uri("http://app-default:3128"));
        try
        {
            using var h = ClientTransport.CreateHandler(null, null, getenv: Cgi("http://web-proxy:3128"));
            Assert.True(h.UseProxy);
            Assert.Null(h.Proxy);
            Assert.Equal(new Uri("http://app-default:3128"), HttpClient.DefaultProxy.GetProxy(target));
        }
        finally
        {
            HttpClient.DefaultProxy = saved;
        }

        // Not under CGI: the platform's proxy, as before.
        using (var h = ClientTransport.CreateHandler(null, null, getenv: name => name == "HTTP_PROXY" ? "http://web-proxy:3128" : null))
        {
            Assert.True(h.UseProxy);
        }
    }

    /// <summary>A client under CGI with a proxy configured: its traffic goes through it.</summary>
    // covers: RULE-PROXY-01
    [Fact]
    public async Task UnderCgiAConfiguredProxyCarriesTheTraffic()
    {
        using var server = new TestServer();
        using var via = new TestProxy("http");
        var saved = Environment.GetEnvironmentVariable("REQUEST_METHOD");
        Environment.SetEnvironmentVariable("REQUEST_METHOD", "GET");
        try
        {
            var client = OpAmpClient.Start(
                new OpAmpClientOptions
                {
                    Mode = OpAmpMode.Platform,
                    Endpoint = new Uri(server.Endpoint("http")),
                    InstanceUid = Uid,
                    HeartbeatInterval = TimeSpan.FromMilliseconds(50),
                    Proxy = via.Url,
                },
                new OpAmpStatus());
            Assert.True(await Wait.ForAsync(() => server.Messages.Count >= 2), "no messages");
            await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
            var upstreams = via.Tunnels.Select(t => t.Upstream).ToHashSet();
            Assert.All(server.Messages, m => Assert.Contains(m.Remote, upstreams));
        }
        finally
        {
            Environment.SetEnvironmentVariable("REQUEST_METHOD", saved);
        }
    }

    [Fact]
    public void DirectConnectionTakesNoProxy()
    {
        var endpoint = new Uri("http://127.0.0.1:1/v1/opamp");
        using var app = new HttpClient();
        Assert.Throws<ArgumentException>(() => OpAmpClient.Start(
            new OpAmpClientOptions { Mode = OpAmpMode.Platform, Endpoint = endpoint, DirectConnection = true, Proxy = new Uri("http://127.0.0.1:2") }, new OpAmpStatus()));
        Assert.Throws<ArgumentException>(() => OpAmpClient.Start(
            new OpAmpClientOptions { Endpoint = endpoint, DirectConnection = true, HttpClient = app }, new OpAmpStatus()));
    }
}
