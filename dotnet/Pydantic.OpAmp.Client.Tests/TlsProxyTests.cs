// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Collections.Concurrent;
using System.Net;
using System.Reflection;
using System.Security.Authentication;
using System.Security.Cryptography;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// The application's TLS and proxy settings, over plain HTTP and WebSocket and for downloads,
/// against a test server and proxies of our own (NetworkTestKit.cs); in both modes: .NET's
/// HttpClient and ClientWebSocket, and Rust mode (the core's own HTTP, WebSocket, CONNECT and
/// SOCKS5 over .NET's sockets and SslStream).
/// </summary>
public class TlsProxyTests
{
    private static readonly byte[] Uid = Enumerable.Repeat((byte)0x42, 16).ToArray();

    /// <summary>Every transport, with no TLS, TLS or mutual TLS, direct or through either proxy, in either mode.</summary>
    public static TheoryData<string, string, string, OpAmpMode> Routes()
    {
        var routes = new TheoryData<string, string, string, OpAmpMode>();
        foreach (var mode in new[] { OpAmpMode.DotNet, OpAmpMode.Rust })
        {
            foreach (var transport in new[] { "http", "ws" })
            {
                foreach (var security in new[] { "plain", "tls", "mtls" })
                {
                    foreach (var proxy in new[] { "none", "http", "socks5" })
                    {
                        routes.Add(transport, security, proxy, mode);
                    }
                }
            }
        }

        return routes;
    }

    /// <summary>Each case given, in either mode.</summary>
    public static TheoryData<string, string, OpAmpMode> Both(params string[] cases)
    {
        var data = new TheoryData<string, string, OpAmpMode>();
        foreach (var mode in new[] { OpAmpMode.DotNet, OpAmpMode.Rust })
        {
            foreach (var c in cases)
            {
                var parts = c.Split('|');
                data.Add(parts[0], parts.Length > 1 ? parts[1] : string.Empty, mode);
            }
        }

        return data;
    }

    public static TheoryData<string, string, OpAmpMode> Unverifiable() => Both(
        "http|other CA", "ws|other CA", "http|system", "ws|system", "http|other CA and system", "ws|other CA and system",
        "http|wrong name", "ws|wrong name", "http|wrong name, and system");

    public static TheoryData<string, string, OpAmpMode> Transports() => Both("http", "ws");

    public static TheoryData<string, string, OpAmpMode> ProxyKinds() => Both("http", "socks5");

    public static TheoryData<string, string, OpAmpMode> SocksCases() => Both(
        "http|plain socks5", "ws|plain socks5", "http|tls socks5", "ws|tls socks5",
        "http|plain socks5h", "ws|plain socks5h", "http|tls socks5h", "ws|tls socks5h");

    // covers: RULE-TLS-01, RULE-TLS-02, RULE-PROXY-01
    [Theory]
    [MemberData(nameof(Routes))]
    public async Task EveryMessageTakesTheConfiguredRoute(string transport, string security, string proxy, OpAmpMode mode)
    {
        using var server = new TestServer(security == "plain" ? null : Pki.Server, security == "mtls" ? Pki.Ca : null);
        using var via = proxy == "none" ? null : new TestProxy(proxy);
        var tls = security == "plain" ? null : new OpAmpTlsOptions
        {
            CaPem = Pki.Pem(Pki.Ca),
            ClientCertPem = security == "mtls" ? Pki.Pem(Pki.Agent) : null,
            ClientKeyPem = security == "mtls" ? Pki.KeyPem(Pki.Agent) : null,
        };

        var (messages, _) = await RunAsync(server.Endpoint(transport), tls, via?.Url, server, 3, mode: mode);

        Assert.True(messages.Length >= 3, $"{messages.Length} message(s)");
        Assert.NotNull(messages[^1].Message.AgentDisconnect);
        foreach (var m in messages)
        {
            if (security == "plain")
            {
                Assert.Null(m.Tls);
            }
            else
            {
                Assert.Contains(m.Tls, new SslProtocols?[] { SslProtocols.Tls12, SslProtocols.Tls13 });
            }

            Assert.Equal(security == "mtls" ? "CN=agent" : null, m.ClientSubject);
        }

        if (via is not null)
        {
            var tunnels = via.Tunnels.ToArray();
            Assert.NotEmpty(tunnels);
            Assert.All(tunnels, t => Assert.Equal(TestProxy.User, t.User));
            Assert.All(tunnels, t => Assert.Equal($"127.0.0.1:{server.Port}", t.Target));

            // Plain HTTP goes in absolute form; everything else in a tunnel.
            var how = proxy == "socks5" ? "socks5" : transport == "http" && security == "plain" ? "forward" : "connect";
            if (mode == OpAmpMode.Rust && proxy == "http")
            {
                how = "connect"; // the shell tunnels plain HTTP too
            }

            Assert.All(tunnels, t => Assert.Equal(how, t.Via));
            var upstreams = tunnels.Select(t => t.Upstream).ToHashSet();
            Assert.All(messages, m => Assert.Contains(m.Remote, upstreams));
        }
    }

    // covers: RULE-TLS-01
    [Theory]
    [MemberData(nameof(Unverifiable))]
    public async Task AServerItCannotVerifyGetsNothing(string transport, string trust, OpAmpMode mode)
    {
        var name = trust.StartsWith("wrong name", StringComparison.Ordinal);
        using var server = new TestServer(name ? Pki.Issue(Pki.Ca, "elsewhere", ["elsewhere.example"]) : Pki.Server);
        var tls = trust == "system" ? null : new OpAmpTlsOptions
        {
            CaPem = Pki.Pem(name ? Pki.Ca : Pki.OtherCa),
            IncludeSystemCaCerts = trust.EndsWith("system", StringComparison.Ordinal),
        };

        var (messages, notes) = await RunAsync(server.Endpoint(transport), tls, null, server, 0, failures: 2, mode: mode);

        Assert.True(server.Connections > 0);
        Assert.Equal(0, server.Requests); // nothing: not even the head of a request
        Assert.Empty(messages);
        Assert.Contains(notes, n => n is OpAmpNotification.ConnectFailed);
    }

    // covers: RULE-TLS-01
    [Theory]
    [MemberData(nameof(Transports))]
    public async Task TheSystemsCasAndTheGivenOnesTogether(string transport, string none, OpAmpMode mode)
    {
        _ = none;
        using var server = new TestServer(Pki.Server);
        var tls = new OpAmpTlsOptions { CaPem = Pki.Pem(Pki.Ca), IncludeSystemCaCerts = true };

        var (messages, _) = await RunAsync(server.Endpoint(transport), tls, null, server, 2, mode: mode);

        Assert.NotEmpty(messages);
        Assert.NotNull(messages[^1].Message.AgentDisconnect);
    }

    // covers: RULE-TLS-02
    [Theory]
    [MemberData(nameof(Transports))]
    public async Task WithoutItsCertificateAServerThatAsksForOneRefusesTheClient(string transport, string none, OpAmpMode mode)
    {
        _ = none;
        using var server = new TestServer(Pki.Server, Pki.Ca);
        var tls = new OpAmpTlsOptions { CaPem = Pki.Pem(Pki.Ca) };

        var (messages, notes) = await RunAsync(server.Endpoint(transport), tls, null, server, 0, failures: 2, mode: mode);

        Assert.Empty(messages);
        Assert.Contains(notes, n => n is OpAmpNotification.ConnectFailed);
    }

    // covers: RULE-TLS-01
    [Theory]
    [InlineData(OpAmpMode.DotNet)]
    [InlineData(OpAmpMode.Rust)]
    public async Task TlsVersionsAreTheOnesAskedFor(OpAmpMode mode)
    {
        using (var server = new TestServer(Pki.Server))
        {
            var tls = new OpAmpTlsOptions { CaPem = Pki.Pem(Pki.Ca), MaxVersion = SslProtocols.Tls12 };
            var (messages, _) = await RunAsync(server.Endpoint("http"), tls, null, server, 2, mode: mode);
            Assert.All(messages, m => Assert.Equal(SslProtocols.Tls12, m.Tls));
        }

        using (var server = new TestServer(Pki.Server, protocols: SslProtocols.Tls12))
        {
            var tls = new OpAmpTlsOptions { CaPem = Pki.Pem(Pki.Ca), MinVersion = SslProtocols.Tls13 };
            var (messages, notes) = await RunAsync(server.Endpoint("ws"), tls, null, server, 0, failures: 2, mode: mode);
            Assert.Empty(messages);
            Assert.Contains(notes, n => n is OpAmpNotification.ConnectFailed);
        }
    }

    // covers: RULE-PROXY-01
    [Theory]
    [MemberData(nameof(Transports))]
    public async Task AnHttpsProxyIsReachedOverTls(string transport, string none, OpAmpMode mode)
    {
        _ = none;
        using var server = new TestServer(Pki.Server);
        using var via = new TestProxy("http", Pki.Server);
        var tls = new OpAmpTlsOptions { CaPem = Pki.Pem(Pki.Ca) };

        var (messages, _) = await RunAsync(server.Endpoint(transport), tls, via.Url, server, 2, mode: mode);

        Assert.NotEmpty(messages);
        Assert.NotEmpty(via.Tunnels);
        Assert.All(via.Tunnels, t => Assert.Equal("connect", t.Via));
    }

    // covers: RULE-PROXY-01
    [Theory]
    [MemberData(nameof(ProxyKinds))]
    public async Task WrongProxyCredentialsGetNothingThrough(string kind, string none, OpAmpMode mode)
    {
        _ = none;
        using var server = new TestServer();
        using var via = new TestProxy(kind);
        var wrong = new UriBuilder(via.Url) { Password = "wrong" }.Uri;

        var (messages, notes) = await RunAsync(server.Endpoint("ws"), null, wrong, server, 0, failures: 2, mode: mode);

        Assert.Empty(messages);
        Assert.Empty(via.Tunnels);
        Assert.True(via.Refused > 0);
        Assert.Contains(notes, n => n is OpAmpNotification.ConnectFailed);
    }

    // covers: RULE-TLS-01, RULE-TLS-02, RULE-PROXY-01
    [Theory]
    [InlineData("http")]
    [InlineData("socks5")]
    [InlineData("socks5h")]
    public async Task DownloadsTakeTheSameRoute(string proxy)
    {
        using var server = new TestServer(Pki.Server, Pki.Ca);
        using var via = new TestProxy(proxy == "http" ? "http" : "socks5");
        var url = new UriBuilder(via.Url) { Scheme = proxy }.Uri;
        var tls = new OpAmpTlsOptions { CaPem = Pki.Pem(Pki.Ca), ClientCertPem = Pki.Pem(Pki.Agent), ClientKeyPem = Pki.KeyPem(Pki.Agent) };
        using var transport = ClientTransport.Create(tls, url);

        var got = await DownloadAsync(transport, $"https://localhost:{server.Port}/redirect");

        Assert.Equal(200u, got.DownloadResponse.Status);
        Assert.Equal(SHA256.HashData(TestServer.File), got.DownloadResponse.Sha256.ToByteArray());
        Assert.Equal(new[] { "/redirect", "/file" }, server.Gets.ToArray());
        Assert.NotEmpty(via.Tunnels);
        Assert.All(via.Tunnels, t => Assert.Equal($"localhost:{server.Port}", t.Target)); // the proxy resolves the name
        Assert.All(via.Tunnels, t => Assert.Equal(proxy == "http" ? null : (byte?)3, t.Atyp));
        Assert.True(server.Handshakes > 0);

        // Without the client certificate, the same download fails.
        using var bare = ClientTransport.Create(tls with { ClientCertPem = null, ClientKeyPem = null }, url);
        Assert.Equal(Input.KindOneofCase.DownloadFailed, (await DownloadAsync(bare, $"https://localhost:{server.Port}/file")).KindCase);
    }

    /// <summary>
    /// With socks5:// and socks5h:// alike, the SOCKS5 proxy is given the server's host name
    /// (address type 3) and resolves it; socks5h:// is taken as socks5://, which .NET knows.
    /// </summary>
    // covers: RULE-PROXY-01
    [Theory]
    [MemberData(nameof(SocksCases))]
    public async Task TheSocksProxyResolvesTheServersName(string transport, string how, OpAmpMode mode)
    {
        var security = how.Split(' ')[0];
        var scheme = how.Split(' ')[1];
        using var server = new TestServer(security == "tls" ? Pki.Server : null);
        using var via = new TestProxy("socks5");
        var tls = security == "tls" ? new OpAmpTlsOptions { CaPem = Pki.Pem(Pki.Ca) } : null;
        var endpoint = server.Endpoint(transport).Replace("127.0.0.1", "localhost", StringComparison.Ordinal);

        var (messages, _) = await RunAsync(endpoint, tls, new UriBuilder(via.Url) { Scheme = scheme }.Uri, server, 2, mode: mode);

        Assert.NotEmpty(messages);
        Assert.NotNull(messages[^1].Message.AgentDisconnect);
        var tunnels = via.Tunnels.ToArray();
        Assert.NotEmpty(tunnels);
        Assert.All(tunnels, t => Assert.Equal((byte?)3, t.Atyp));
        Assert.All(tunnels, t => Assert.Equal($"localhost:{server.Port}", t.Target));
        Assert.All(tunnels, t => Assert.Equal(TestProxy.User, t.User));
        var upstreams = tunnels.Select(t => t.Upstream).ToHashSet();
        Assert.All(messages, m => Assert.Contains(m.Remote, upstreams));
    }

    /// <summary>
    /// Rust mode can resolve the server's name itself for a SOCKS5 proxy (ProxyUseLocalResolver,
    /// an error in .NET mode): the client resolves it, and the proxy is given an address. (The
    /// name's addresses are the test's, so that the first is the IPv4 one its server is on; the
    /// connections are real.)
    /// </summary>
    // covers: RULE-PROXY-01
    [Theory]
    [InlineData("http", "socks5")]
    [InlineData("ws", "socks5h")]
    public async Task RustModeResolvesTheNameForASocksProxyWhenAsked(string transport, string scheme)
    {
        using var server = new TestServer();
        using var via = new TestProxy("socks5");
        var net = new NamedNet();
        var client = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Endpoint = new Uri(server.Endpoint(transport).Replace("127.0.0.1", "server.test", StringComparison.Ordinal)),
                InstanceUid = Uid,
                HeartbeatInterval = TimeSpan.FromMilliseconds(50),
                Proxy = new UriBuilder(via.Url) { Scheme = scheme }.Uri,
                ProxyUseLocalResolver = true,
                Mode = OpAmpMode.Rust,
            },
            new OpAmpStatus(),
            null,
            new Seams { Net = net });
        await WaitAsync(() => server.Messages.Count >= 2);
        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));

        Assert.True(server.Messages.Count >= 2);
        Assert.Contains("server.test", net.Resolved);
        Assert.NotEmpty(via.Tunnels);
        Assert.All(via.Tunnels, t => Assert.Equal((byte?)1, t.Atyp)); // an IPv4 address, not the name
        Assert.All(via.Tunnels, t => Assert.Equal($"127.0.0.1:{server.Port}", t.Target));
    }

    /// <summary>
    /// A proxy the client can't use as asked is an error at start, never a direct connection:
    /// SOCKS4, unknown schemes, and resolving names locally (which .NET's proxies can't do).
    /// </summary>
    // covers: RULE-PROXY-01
    [Theory]
    [InlineData("socks4://127.0.0.1:1080", false, "SOCKS4")]
    [InlineData("socks4a://127.0.0.1:1080", false, "SOCKS4")]
    [InlineData("SOCKS4://127.0.0.1:1080", false, "SOCKS4")]
    [InlineData("socks://127.0.0.1:1080", false, "socks://")]
    [InlineData("socks5x://127.0.0.1:1080", false, "socks5x://")]
    [InlineData("ftp://127.0.0.1:1080", false, "ftp://")]
    [InlineData("socks5://127.0.0.1:1080", true, "ProxyUseLocalResolver")]
    [InlineData("socks5h://127.0.0.1:1080", true, "ProxyUseLocalResolver")]
    [InlineData("http://127.0.0.1:3128", true, "HTTP proxy")]
    [InlineData("https://127.0.0.1:3128", true, "HTTP proxy")]
    [InlineData(null, true, "ProxyUseLocalResolver")]
    public void AProxyThatCantBeUsedAsAskedIsAnError(string? proxy, bool local, string message)
    {
        var options = new OpAmpClientOptions
        {
            Mode = OpAmpMode.DotNet,
            Endpoint = new Uri("http://localhost:1/v1/opamp"),
            InstanceUid = Uid,
            Proxy = proxy is null ? null : new Uri(proxy),
            ProxyUseLocalResolver = local,
        };

        var e = Assert.Throws<ArgumentException>(() => OpAmpClient.Start(options, new OpAmpStatus()));

        Assert.Contains(message, e.Message, StringComparison.Ordinal);
        if (proxy is not null)
        {
            Assert.DoesNotContain("127.0.0.1", e.Message, StringComparison.Ordinal); // nor any credentials the URL has
        }
    }

    // covers: RULE-TLS-01
    [Fact]
    public async Task DownloadsVerifyTheServerToo()
    {
        using var server = new TestServer(Pki.Server);
        using var transport = ClientTransport.Create(new OpAmpTlsOptions { CaPem = Pki.Pem(Pki.OtherCa) }, null);

        var got = await DownloadAsync(transport, $"https://127.0.0.1:{server.Port}/file");

        Assert.Equal(Input.KindOneofCase.DownloadFailed, got.KindCase);
        Assert.Empty(server.Gets);
    }

    [Theory]
    [InlineData("http")]
    [InlineData("ws")]
    public async Task AnApplicationsHttpClientCarriesEverything(string transport)
    {
        using var server = new TestServer();
        using var via = new TestProxy("http");
        var credentials = new NetworkCredential(TestProxy.User, TestProxy.Password);
        using var app = new HttpClient(new SocketsHttpHandler
        {
            AllowAutoRedirect = false,
            Proxy = new WebProxy(new UriBuilder(via.Url) { UserName = string.Empty, Password = string.Empty }.Uri) { Credentials = credentials },
        });
        var notes = new ConcurrentQueue<OpAmpNotification>();
        var client = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Endpoint = new Uri(server.Endpoint(transport)),
                InstanceUid = Uid,
                HeartbeatInterval = TimeSpan.FromMilliseconds(50),
                HttpClient = app,
            },
            new OpAmpStatus(),
            notes.Enqueue);
        await WaitAsync(() => server.Messages.Count >= 2);
        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));

        Assert.True(server.Messages.Count >= 2);
        Assert.NotEmpty(via.Tunnels);
        Assert.All(via.Tunnels, t => Assert.Equal(transport == "ws" ? "connect" : "forward", t.Via));

        // Downloads too; and it is still the application's to use.
        var got = await DownloadAsync(ClientTransport.Of(app), $"http://127.0.0.1:{server.Port}/redirect");
        Assert.Equal(200u, got.DownloadResponse.Status);
        Assert.Equal(HttpStatusCode.NotFound, (await app.GetAsync($"http://127.0.0.1:{server.Port}/nothing")).StatusCode);
    }

    [Fact]
    public void BadSettingsThrowAtStart()
    {
        var endpoint = new Uri("https://127.0.0.1:1/v1/opamp");
        using var app = new HttpClient();
        var ca = Pki.Pem(Pki.Ca);
        OpAmpClientOptions[] bad =
        [
            new() { Endpoint = endpoint, HttpClient = app, Tls = new OpAmpTlsOptions() },
            new() { Endpoint = endpoint, HttpClient = app, Proxy = new Uri("http://127.0.0.1:1") },
            new() { Endpoint = endpoint, Proxy = new Uri("ftp://127.0.0.1:1") },
            new() { Endpoint = endpoint, Tls = new OpAmpTlsOptions { CaPem = "not a certificate" } },
            new() { Endpoint = endpoint, Tls = new OpAmpTlsOptions { CaPem = ca, ClientCertPem = Pki.Pem(Pki.Agent) } },
            new() { Endpoint = endpoint, Tls = new OpAmpTlsOptions { ClientCertPem = Pki.Pem(Pki.Agent), ClientKeyPem = Pki.KeyPem(Pki.Server) } },
            new() { Endpoint = endpoint, Tls = new OpAmpTlsOptions { MinVersion = (SslProtocols)0x300 } }, // TLS 1.1
            new() { Endpoint = endpoint, Tls = new OpAmpTlsOptions { MinVersion = SslProtocols.Tls13, MaxVersion = SslProtocols.Tls12 } },
        ];
        foreach (var options in bad)
        {
            Assert.Throws<ArgumentException>(() => OpAmpClient.Start(options, new OpAmpStatus()));
        }
    }

    /// <summary>Whatever the application configures reaches every connection: no client of the library's own is shared.</summary>
    [Fact]
    public void NoStaticClientsAreLeft()
    {
        var statics = typeof(OpAmpClient).Assembly.GetTypes()
            .SelectMany(t => t.GetFields(BindingFlags.Static | BindingFlags.Public | BindingFlags.NonPublic | BindingFlags.DeclaredOnly))
            .Where(f => typeof(HttpMessageInvoker).IsAssignableFrom(f.FieldType) || typeof(HttpMessageHandler).IsAssignableFrom(f.FieldType))
            .Select(f => $"{f.DeclaringType}.{f.Name}");
        Assert.Empty(statics);
    }

    /// <summary>
    /// Run a client until the server has <paramref name="messages"/> messages (or the client has
    /// failed <paramref name="failures"/> times), stop it, and return what the server got after
    /// the goodbye.
    /// </summary>
    private static async Task<(Received[] Messages, OpAmpNotification[] Notes)> RunAsync(
        string endpoint, OpAmpTlsOptions? tls, Uri? proxy, TestServer server, int messages, int failures = 0, OpAmpMode mode = OpAmpMode.DotNet,
        bool localResolver = false)
    {
        var notes = new ConcurrentQueue<OpAmpNotification>();
        var client = OpAmpClient.Start(
            new OpAmpClientOptions
            {
                Endpoint = new Uri(endpoint),
                InstanceUid = Uid,
                HeartbeatInterval = TimeSpan.FromMilliseconds(50),
                Tls = tls,
                Proxy = proxy,
                DirectConnection = proxy is null && mode == OpAmpMode.Rust, // the environment's proxies aren't the test's
                ProxyUseLocalResolver = localResolver,
                Mode = mode,
            },
            new OpAmpStatus(),
            notes.Enqueue);
        await WaitAsync(() => (messages > 0 && server.Messages.Count >= messages)
            || (failures > 0 && notes.Count(n => n is OpAmpNotification.ConnectFailed) >= failures));
        await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
        Assert.Equal(0, client.LiveStreams); // Rust mode: every stream closed when stop returns
        if (messages > 0)
        {
            // Over WebSocket the goodbye may reach the server just after the client stops.
            await WaitAsync(() => server.Messages.Any(m => m.Message.AgentDisconnect is not null));
        }

        return (server.Messages.ToArray(), notes.ToArray());
    }

    /// <summary>Download <paramref name="url"/>, and where a redirect points, as the core would ask.</summary>
    private static async Task<Input> DownloadAsync(ClientTransport transport, string url)
    {
        var dir = Directory.CreateTempSubdirectory();
        var loop = new TestLoop(transport, Path.Combine(dir.FullName, "state"));
        Input input;
        ulong id = 0;
        do
        {
            loop.Run(new Effect { Download = new Download { Id = ++id, Url = url, MaxBytes = 1 << 20, TimeoutMs = 10_000 } });
            Assert.True(loop.TryTake(out input, TimeSpan.FromSeconds(15)));
            url = input.DownloadResponse?.Location ?? string.Empty;
        }
        while (input.DownloadResponse is { Status: >= 300 and < 400 } && id < 3);

        await loop.X.ReleaseAsync(TimeSpan.FromSeconds(5));
        dir.Delete(recursive: true);
        return input;
    }

    /// <summary>.NET's network, but for the names it resolves: server.test is 127.0.0.1.</summary>
    private sealed class NamedNet : StreamsNet
    {
        public ConcurrentQueue<string> Resolved { get; } = new();

        public override Task<IReadOnlyList<string>> ResolveAsync(string host, CancellationToken ct)
        {
            this.Resolved.Enqueue(host);
            return host == "server.test" ? Task.FromResult<IReadOnlyList<string>>(["127.0.0.1"]) : base.ResolveAsync(host, ct);
        }
    }

    private static async Task WaitAsync(Func<bool> done)
    {
        for (var i = 0; i < 1000 && !done(); i++)
        {
            await Task.Delay(10);
        }
    }
}
