// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Collections.Concurrent;
using System.Net;
using System.Net.Security;
using System.Net.Sockets;
using System.Net.WebSockets;
using System.Security.Authentication;
using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;
using System.Text;
using Google.Protobuf;
using OpAmp.Proto.V1;
using CertificateRequest = System.Security.Cryptography.X509Certificates.CertificateRequest;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>Certificates made for the tests: two CAs, a server's and an agent's.</summary>
internal static class Pki
{
    public static readonly X509Certificate2 Ca = NewCa("test CA");
    public static readonly X509Certificate2 OtherCa = NewCa("other CA");
    public static readonly X509Certificate2 Server = Issue(Ca, "server", ["127.0.0.1", "localhost"]);
    public static readonly X509Certificate2 Agent = Issue(Ca, "agent", null);

    public static string Pem(X509Certificate2 cert) => cert.ExportCertificatePem();

    public static string KeyPem(X509Certificate2 cert) => cert.GetECDsaPrivateKey()!.ExportPkcs8PrivateKeyPem();

    public static X509Certificate2 NewCa(string name)
    {
        using var key = ECDsa.Create(ECCurve.NamedCurves.nistP256);
        var req = new CertificateRequest($"CN={name}", key, HashAlgorithmName.SHA256);
        req.CertificateExtensions.Add(new X509BasicConstraintsExtension(true, false, 0, true));
        req.CertificateExtensions.Add(new X509KeyUsageExtension(X509KeyUsageFlags.KeyCertSign | X509KeyUsageFlags.CrlSign, true));
        req.CertificateExtensions.Add(new X509SubjectKeyIdentifierExtension(req.PublicKey, false));
        return req.CreateSelfSigned(DateTimeOffset.UtcNow.AddDays(-1), DateTimeOffset.UtcNow.AddDays(1));
    }

    /// <summary>A server certificate for <paramref name="hosts"/>, or (none) a client one.</summary>
    public static X509Certificate2 Issue(X509Certificate2 ca, string name, string[]? hosts)
    {
        var key = ECDsa.Create(ECCurve.NamedCurves.nistP256);
        var req = new CertificateRequest($"CN={name}", key, HashAlgorithmName.SHA256);
        req.CertificateExtensions.Add(new X509BasicConstraintsExtension(false, false, 0, false));
        req.CertificateExtensions.Add(new X509KeyUsageExtension(X509KeyUsageFlags.DigitalSignature, true));
        var usage = hosts is null ? "1.3.6.1.5.5.7.3.2" : "1.3.6.1.5.5.7.3.1";
        req.CertificateExtensions.Add(new X509EnhancedKeyUsageExtension(new OidCollection { new Oid(usage) }, false));
        if (hosts is not null)
        {
            var san = new SubjectAlternativeNameBuilder();
            foreach (var h in hosts)
            {
                if (IPAddress.TryParse(h, out var ip))
                {
                    san.AddIpAddress(ip);
                }
                else
                {
                    san.AddDnsName(h);
                }
            }

            req.CertificateExtensions.Add(san.Build());
        }

        req.CertificateExtensions.Add(X509AuthorityKeyIdentifierExtension.CreateFromCertificate(ca, true, false));
        var serial = RandomNumberGenerator.GetBytes(16);
        serial[0] &= 0x7f;
        using var cert = req.Create(ca, DateTimeOffset.UtcNow.AddHours(-1), DateTimeOffset.UtcNow.AddHours(12), serial);
        return cert.CopyWithPrivateKey(key);
    }

    public static bool SignedBy(X509Certificate2 ca, X509Certificate cert)
    {
        using var chain = new X509Chain();
        chain.ChainPolicy.TrustMode = X509ChainTrustMode.CustomRootTrust;
        chain.ChainPolicy.RevocationMode = X509RevocationMode.NoCheck;
        chain.ChainPolicy.CustomTrustStore.Add(ca);
        return chain.Build(new X509Certificate2(cert));
    }
}

/// <summary>Reading HTTP/1.1 heads off a stream, a byte at a time (nothing past the head is read).</summary>
internal static class Http1
{
    public static async Task<(string Line, Dictionary<string, string> Headers)?> ReadHeadAsync(Stream s)
    {
        var head = new List<byte>();
        var one = new byte[1];
        while (true)
        {
            if (await s.ReadAsync(one) == 0)
            {
                return null;
            }

            head.Add(one[0]);
            if (head.Count >= 4 && head[^4] == '\r' && head[^3] == '\n' && head[^2] == '\r' && head[^1] == '\n')
            {
                break;
            }
        }

        var lines = Encoding.ASCII.GetString(head.ToArray()).Split("\r\n", StringSplitOptions.RemoveEmptyEntries);
        var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        foreach (var l in lines.Skip(1))
        {
            var colon = l.IndexOf(':');
            headers[l[..colon].Trim()] = l[(colon + 1)..].Trim();
        }

        return (lines[0], headers);
    }

    public static Task WriteAsync(Stream s, string head, byte[]? body = null)
    {
        var bytes = Encoding.ASCII.GetBytes(head).Concat(body ?? []).ToArray();
        return s.WriteAsync(bytes).AsTask();
    }

    public static async Task PipeAsync(Stream a, Stream b)
    {
        using var cts = new CancellationTokenSource();
        var ab = a.CopyToAsync(b, cts.Token);
        var ba = b.CopyToAsync(a, cts.Token);
        try
        {
            await Task.WhenAny(ab, ba);
        }
        finally
        {
            cts.Cancel();
            a.Dispose();
            b.Dispose();
        }
    }
}

/// <summary>What the server got: a message, and the connection it came on.</summary>
internal sealed record Received(AgentToServer Message, string Remote, SslProtocols? Tls, string? ClientSubject);

/// <summary>
/// An OpAMP server over HTTP and WebSocket, with TLS if given a certificate, and client
/// certificates if given a CA to check them against. GET /file serves <see cref="File"/>, and
/// /redirect sends there.
/// </summary>
internal sealed class TestServer : IDisposable
{
    public static readonly byte[] File = Encoding.ASCII.GetBytes(string.Concat(Enumerable.Repeat("package", 100)));

    private readonly TcpListener listener = new(IPAddress.Loopback, 0);
    private readonly X509Certificate2? cert;
    private readonly X509Certificate2? clientCa;
    private readonly SslProtocols protocols;
    private int handshakes;
    private int connections;
    private int requests;

    public TestServer(X509Certificate2? cert = null, X509Certificate2? clientCa = null, SslProtocols protocols = SslProtocols.None)
    {
        this.cert = cert;
        this.clientCa = clientCa;
        this.protocols = protocols;
        this.listener.Start();
        _ = Task.Run(this.AcceptAsync);
    }

    public int Port => ((IPEndPoint)this.listener.LocalEndpoint).Port;

    /// <summary>Offered in every answer (the client hears it once: the hash doesn't change).</summary>
    public AgentRemoteConfig? RemoteConfig { get; init; }

    /// <summary>How long a WebSocket upgrade waits before it is answered, to stop a client mid-handshake.</summary>
    public TimeSpan HoldUpgrade { get; init; }

    public ConcurrentQueue<Received> Messages { get; } = new();

    public ConcurrentQueue<string> Gets { get; } = new();

    /// <summary>TLS handshakes done, as the server sees them (with TLS 1.3 the client checks the server after that).</summary>
    public int Handshakes => Volatile.Read(ref this.handshakes);

    /// <summary>TCP connections accepted.</summary>
    public int Connections => Volatile.Read(ref this.connections);

    /// <summary>Requests (and upgrades) read.</summary>
    public int Requests => Volatile.Read(ref this.requests);

    /// <summary>An address as both ends print it (IPv4, not IPv4 mapped to IPv6).</summary>
    public static string Address(EndPoint? e) =>
        e is IPEndPoint ip ? new IPEndPoint(ip.Address.IsIPv4MappedToIPv6 ? ip.Address.MapToIPv4() : ip.Address, ip.Port).ToString() : string.Empty;

    public string Endpoint(string transport) =>
        $"{transport}{(this.cert is null ? string.Empty : "s")}://127.0.0.1:{this.Port}/v1/opamp";

    public void Dispose() => this.listener.Stop();

    private async Task AcceptAsync()
    {
        while (true)
        {
            TcpClient c;
            try
            {
                c = await this.listener.AcceptTcpClientAsync();
            }
            catch (Exception e) when (e is SocketException or ObjectDisposedException)
            {
                return;
            }

            _ = Task.Run(() => this.ServeAsync(c));
        }
    }

    private async Task ServeAsync(TcpClient c)
    {
        using var tcp = c;
        Interlocked.Increment(ref this.connections);
        Stream s = tcp.GetStream();
        var remote = Address(tcp.Client.RemoteEndPoint);
        SslProtocols? tls = null;
        string? subject = null;
        try
        {
            if (this.cert is not null)
            {
                var ssl = new SslStream(s);
                try
                {
                    await ssl.AuthenticateAsServerAsync(new SslServerAuthenticationOptions
                    {
                        ServerCertificate = this.cert,
                        EnabledSslProtocols = this.protocols,
                        ClientCertificateRequired = this.clientCa is not null,
                        RemoteCertificateValidationCallback = (_, cert, _, _) =>
                            this.clientCa is null || (cert is not null && Pki.SignedBy(this.clientCa, cert)),
                    });
                }
                catch (Exception e) when (e is AuthenticationException or IOException)
                {
                    return; // refused, by either end
                }

                Interlocked.Increment(ref this.handshakes);
                tls = ssl.SslProtocol;
                subject = ssl.RemoteCertificate?.Subject;
                s = ssl;
            }

            while (await Http1.ReadHeadAsync(s) is { } head)
            {
                var (line, headers) = head;
                Interlocked.Increment(ref this.requests);
                var parts = line.Split(' ');
                var path = parts[1].StartsWith('/') ? parts[1] : new Uri(parts[1]).AbsolutePath; // absolute form, from a proxy
                if (headers.TryGetValue("Upgrade", out var upgrade) && upgrade.Equals("websocket", StringComparison.OrdinalIgnoreCase))
                {
                    var accept = Convert.ToBase64String(SHA1.HashData(Encoding.ASCII.GetBytes(
                        headers["Sec-WebSocket-Key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")));
                    if (this.HoldUpgrade > TimeSpan.Zero)
                    {
                        await Task.Delay(this.HoldUpgrade);
                    }

                    await Http1.WriteAsync(s, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                        + $"Sec-WebSocket-Accept: {accept}\r\n\r\n");
                    using var ws = WebSocket.CreateFromStream(s, new WebSocketCreationOptions { IsServer = true });
                    await this.ServeWebSocketAsync(ws, remote, tls, subject);
                    return;
                }

                if (parts[0] == "POST")
                {
                    var body = new byte[int.Parse(headers["Content-Length"])];
                    await s.ReadExactlyAsync(body);
                    var reply = this.Answer(AgentToServer.Parser.ParseFrom(body), remote, tls, subject);
                    await Http1.WriteAsync(s, $"HTTP/1.1 200 OK\r\nContent-Type: application/x-protobuf\r\nContent-Length: {reply.Length}\r\n\r\n", reply);
                    continue;
                }

                this.Gets.Enqueue(path);
                if (path == "/file")
                {
                    await Http1.WriteAsync(s, $"HTTP/1.1 200 OK\r\nContent-Length: {File.Length}\r\n\r\n", File);
                }
                else if (path == "/redirect")
                {
                    await Http1.WriteAsync(s, "HTTP/1.1 302 Found\r\nLocation: /file\r\nContent-Length: 0\r\n\r\n");
                }
                else
                {
                    await Http1.WriteAsync(s, "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n");
                }
            }
        }
        catch (Exception e) when (e is IOException or WebSocketException or ObjectDisposedException)
        {
            // the client went
        }
    }

    private byte[] Answer(AgentToServer msg, string remote, SslProtocols? tls, string? subject)
    {
        this.Messages.Enqueue(new Received(msg, remote, tls, subject));
        var answer = new ServerToAgent { InstanceUid = msg.InstanceUid };
        if (this.RemoteConfig is { } offer)
        {
            answer.RemoteConfig = offer;
        }

        return answer.ToByteArray();
    }

    private async Task ServeWebSocketAsync(WebSocket ws, string remote, SslProtocols? tls, string? subject)
    {
        var buf = new byte[65536];
        using var message = new MemoryStream();
        while (true)
        {
            var r = await ws.ReceiveAsync(buf, CancellationToken.None);
            if (r.MessageType == WebSocketMessageType.Close)
            {
                await ws.CloseOutputAsync(WebSocketCloseStatus.NormalClosure, string.Empty, CancellationToken.None);
                return;
            }

            message.Write(buf, 0, r.Count);
            if (r.EndOfMessage)
            {
                // The OpAMP header (0) before the message, both ways.
                var answer = this.Answer(AgentToServer.Parser.ParseFrom(message.ToArray().AsSpan(1)), remote, tls, subject);
                message.SetLength(0);
                await ws.SendAsync(new byte[] { 0 }.Concat(answer).ToArray(), WebSocketMessageType.Binary, true, CancellationToken.None);
            }
        }
    }
}

/// <summary>
/// A tunnel the proxy opened: how (connect, forward or socks5), to where, for whom, and from which
/// local address; for SOCKS5, the address type the client gave the target as (1 IPv4, 3 a name, 4 IPv6).
/// </summary>
internal sealed record Tunnel(string Via, string Target, string User, string Upstream, byte? Atyp = null);

/// <summary>
/// A proxy that wants user <c>agent</c>: HTTP (CONNECT, and requests in absolute form; with TLS
/// if given a certificate) or SOCKS5.
/// </summary>
internal sealed class TestProxy : IDisposable
{
    public const string User = "agent";
    public const string Password = "p@ss:word"; // escaped in the URL

    private readonly TcpListener listener = new(IPAddress.Loopback, 0);
    private readonly string kind;
    private readonly X509Certificate2? cert;
    private int refused;

    public TestProxy(string kind, X509Certificate2? cert = null)
    {
        this.kind = kind;
        this.cert = cert;
        this.listener.Start();
        _ = Task.Run(this.AcceptAsync);
    }

    public Uri Url => new($"{(this.kind == "socks5" ? "socks5" : this.cert is null ? "http" : "https")}://{User}:{Uri.EscapeDataString(Password)}@127.0.0.1:{((IPEndPoint)this.listener.LocalEndpoint).Port}");

    public ConcurrentQueue<Tunnel> Tunnels { get; } = new();

    public int Refused => Volatile.Read(ref this.refused);

    public void Dispose() => this.listener.Stop();

    private async Task AcceptAsync()
    {
        while (true)
        {
            TcpClient c;
            try
            {
                c = await this.listener.AcceptTcpClientAsync();
            }
            catch (Exception e) when (e is SocketException or ObjectDisposedException)
            {
                return;
            }

            _ = Task.Run(async () =>
            {
                try
                {
                    Stream s = c.GetStream();
                    if (this.cert is not null)
                    {
                        var ssl = new SslStream(s);
                        await ssl.AuthenticateAsServerAsync(new SslServerAuthenticationOptions { ServerCertificate = this.cert });
                        s = ssl;
                    }

                    await (this.kind == "socks5" ? this.Socks5Async(s) : this.HttpAsync(s));
                }
                catch (Exception e) when (e is IOException or SocketException or AuthenticationException or ObjectDisposedException)
                {
                    // the client or the server went
                }
                finally
                {
                    c.Dispose();
                }
            });
        }
    }

    private async Task<(TcpClient Up, string Local)> DialAsync(string host, int port)
    {
        var up = new TcpClient();
        await up.ConnectAsync(host, port);
        return (up, TestServer.Address(up.Client.LocalEndPoint));
    }

    private async Task HttpAsync(Stream s)
    {
        if (await Http1.ReadHeadAsync(s) is not { } first)
        {
            return;
        }

        var (line, headers) = first;

        var expected = "Basic " + Convert.ToBase64String(Encoding.UTF8.GetBytes($"{User}:{Password}"));
        if (!headers.TryGetValue("Proxy-Authorization", out var auth) || auth != expected)
        {
            Interlocked.Increment(ref this.refused);
            await Http1.WriteAsync(s, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"test\"\r\n"
                + "Content-Length: 0\r\nConnection: close\r\n\r\n");
            return;
        }

        var parts = line.Split(' ');
        if (parts[0] == "CONNECT")
        {
            var target = parts[1];
            var colon = target.LastIndexOf(':');
            var (up, local) = await this.DialAsync(target[..colon], int.Parse(target[(colon + 1)..]));
            this.Tunnels.Enqueue(new Tunnel("connect", target, User, local));
            await Http1.WriteAsync(s, "HTTP/1.1 200 Connection Established\r\n\r\n");
            await Http1.PipeAsync(s, up.GetStream());
            return;
        }

        // A request in absolute form: on to its server, head and all (the server takes that form).
        var url = new Uri(parts[1]);
        var (upstream, from) = await this.DialAsync(url.Host, url.Port);
        this.Tunnels.Enqueue(new Tunnel("forward", $"{url.Host}:{url.Port}", User, from));
        var head = new StringBuilder(line + "\r\n");
        foreach (var h in headers.Where(h => !h.Key.Equals("Proxy-Authorization", StringComparison.OrdinalIgnoreCase)))
        {
            head.Append($"{h.Key}: {h.Value}\r\n");
        }

        await Http1.WriteAsync(upstream.GetStream(), head + "\r\n");
        await Http1.PipeAsync(s, upstream.GetStream());
    }

    private async Task Socks5Async(Stream s)
    {
        var hello = new byte[2];
        await s.ReadExactlyAsync(hello);
        var methods = new byte[hello[1]];
        await s.ReadExactlyAsync(methods);
        if (!methods.Contains((byte)2))
        {
            Interlocked.Increment(ref this.refused);
            await s.WriteAsync(new byte[] { 5, 0xff });
            return;
        }

        await s.WriteAsync(new byte[] { 5, 2 });
        var ver = new byte[2];
        await s.ReadExactlyAsync(ver);
        var user = new byte[ver[1]];
        await s.ReadExactlyAsync(user);
        var plen = new byte[1];
        await s.ReadExactlyAsync(plen);
        var pass = new byte[plen[0]];
        await s.ReadExactlyAsync(pass);
        if (Encoding.UTF8.GetString(user) != User || Encoding.UTF8.GetString(pass) != Password)
        {
            Interlocked.Increment(ref this.refused);
            await s.WriteAsync(new byte[] { 1, 1 });
            return;
        }

        await s.WriteAsync(new byte[] { 1, 0 });
        var req = new byte[4];
        await s.ReadExactlyAsync(req);
        string host;
        switch (req[3])
        {
            case 1:
            case 4:
                var ip = new byte[req[3] == 1 ? 4 : 16];
                await s.ReadExactlyAsync(ip);
                host = new IPAddress(ip).ToString();
                break;
            default:
                var n = new byte[1];
                await s.ReadExactlyAsync(n);
                var name = new byte[n[0]];
                await s.ReadExactlyAsync(name);
                host = Encoding.ASCII.GetString(name);
                break;
        }

        var portBytes = new byte[2];
        await s.ReadExactlyAsync(portBytes);
        var port = (portBytes[0] << 8) | portBytes[1];
        var (up, local) = await this.DialAsync(host, port);
        this.Tunnels.Enqueue(new Tunnel("socks5", $"{host}:{port}", Encoding.UTF8.GetString(user), local, req[3]));
        await s.WriteAsync(new byte[] { 5, 0, 0, 1, 0, 0, 0, 0, 0, 0 });
        await Http1.PipeAsync(s, up.GetStream());
    }
}
