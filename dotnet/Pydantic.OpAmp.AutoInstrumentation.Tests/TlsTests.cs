// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Net;
using System.Net.Security;
using System.Net.Sockets;
using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;
using System.Text;
using OpenTelemetry.Exporter;
using Pydantic.OpAmp.Otel;
using Xunit;

namespace Pydantic.OpAmp.AutoInstrumentation.Tests;

// The TLS an OTLP exporter builds into its own HTTP client, from the SDK's variables: a gated
// exporter's client is ours, and carries the same.
public class TlsTests
{
    private static CancellationToken Cancel => TestContext.Current.CancellationToken;

    /// <summary>A CA, a server certificate it signed for localhost, and a client certificate it signed, as PEM files.</summary>
    internal sealed class Pki
    {
        public Pki()
        {
            this.Dir = Directory.CreateTempSubdirectory("opamp-tls-").FullName;
            using var caKey = RSA.Create(2048);
            var caReq = new CertificateRequest("CN=OpAMP test CA", caKey, HashAlgorithmName.SHA256, RSASignaturePadding.Pkcs1);
            caReq.CertificateExtensions.Add(new X509BasicConstraintsExtension(true, false, 0, true));
            caReq.CertificateExtensions.Add(new X509KeyUsageExtension(X509KeyUsageFlags.KeyCertSign, true));
            this.Ca = caReq.CreateSelfSigned(DateTimeOffset.UtcNow.AddDays(-1), DateTimeOffset.UtcNow.AddDays(1));
            this.Server = Issue(this.Ca, "CN=localhost", server: true);
            this.Client = Issue(this.Ca, "CN=opamp client", server: false);
            File.WriteAllText(this.CaPath, this.Ca.ExportCertificatePem());
            File.WriteAllText(this.CertPath, this.Client.ExportCertificatePem());
            File.WriteAllText(this.KeyPath, this.Client.GetRSAPrivateKey()!.ExportPkcs8PrivateKeyPem());
        }

        public string Dir { get; }

        public string CaPath => Path.Combine(this.Dir, "ca.pem");

        public string CertPath => Path.Combine(this.Dir, "client.pem");

        public string KeyPath => Path.Combine(this.Dir, "client-key.pem");

        public X509Certificate2 Ca { get; }

        public X509Certificate2 Server { get; }

        public X509Certificate2 Client { get; }

        public Dictionary<string, string?> Env => new()
        {
            ["OTEL_EXPORTER_OTLP_CERTIFICATE"] = this.CaPath,
            ["OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE"] = this.CertPath,
            ["OTEL_EXPORTER_OTLP_CLIENT_KEY"] = this.KeyPath,
        };

        private static X509Certificate2 Issue(X509Certificate2 ca, string subject, bool server)
        {
            using var key = RSA.Create(2048);
            var req = new CertificateRequest(subject, key, HashAlgorithmName.SHA256, RSASignaturePadding.Pkcs1);
            var san = new SubjectAlternativeNameBuilder();
            san.AddDnsName("localhost");
            san.AddIpAddress(IPAddress.Loopback);
            req.CertificateExtensions.Add(san.Build());
            req.CertificateExtensions.Add(new X509EnhancedKeyUsageExtension(
                [new Oid(server ? "1.3.6.1.5.5.7.3.1" : "1.3.6.1.5.5.7.3.2")], false));
            var serial = new byte[8];
            RandomNumberGenerator.Fill(serial);
            using var cert = req.Create(ca, DateTimeOffset.UtcNow.AddHours(-1), DateTimeOffset.UtcNow.AddHours(12), serial);
            using var withKey = cert.CopyWithPrivateKey(key);
            return new X509Certificate2(withKey.Export(X509ContentType.Pkcs12));
        }
    }

    /// <summary>
    /// An HTTPS endpoint with the server certificate that asks for a client certificate, and answers
    /// each request 202, keeping the client certificate it was shown.
    /// </summary>
    private sealed class Collector : IDisposable
    {
        private readonly TcpListener listener = new(IPAddress.Loopback, 0);
        private readonly X509Certificate2 cert;

        public Collector(X509Certificate2 cert)
        {
            this.cert = cert;
            this.listener.Start();
        }

        public int Port => ((IPEndPoint)this.listener.LocalEndpoint).Port;

        /// <summary>Gets how many requests' heads arrived whole.</summary>
        public int Requests { get; private set; }

        public List<string?> ClientCertificates { get; } = [];

        public void Dispose() => this.listener.Stop();

        public async Task AnswerOne(CancellationToken cancel)
        {
            using var tcp = await this.listener.AcceptTcpClientAsync(cancel);
            await using var tls = new SslStream(tcp.GetStream(), false, (_, _, _, _) => true);
            await tls.AuthenticateAsServerAsync(
                new SslServerAuthenticationOptions { ServerCertificate = this.cert, ClientCertificateRequired = true }, cancel);
            this.ClientCertificates.Add(tls.RemoteCertificate is { } c ? new X509Certificate2(c).Thumbprint : null);
            var head = new StringBuilder();
            var one = new byte[1];
            while (!head.ToString().EndsWith("\r\n\r\n", StringComparison.Ordinal) && await tls.ReadAsync(one, cancel) == 1)
            {
                head.Append((char)one[0]);
            }

            if (!head.ToString().EndsWith("\r\n\r\n", StringComparison.Ordinal))
            {
                return; // closed before a request: nothing to answer
            }

            this.Requests++;
            var length = head.ToString().Split("\r\n")
                .Where(l => l.StartsWith("Content-Length:", StringComparison.OrdinalIgnoreCase))
                .Select(l => int.Parse(l["Content-Length:".Length..].Trim()))
                .FirstOrDefault();
            await tls.ReadExactlyAsync(new byte[length], cancel);
            await tls.WriteAsync("HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"u8.ToArray(), cancel);
        }
    }

    private static HttpRequestMessage Export(int port) => new(HttpMethod.Post, $"https://localhost:{port}/v1/traces")
    {
        Content = new ByteArrayContent([1, 2, 3]),
    };

    [Fact]
    public async Task AGatedExporterShowsTheClientCertificateAndTrustsTheCa()
    {
        var pki = new Pki();
        using var collector = new Collector(pki.Server);
        var controls = new Controls();
        var options = new OtlpExporterOptions();
        Controls.Gate(options, controls, Signal.Traces, OtlpTls.FromEnv(name => pki.Env.GetValueOrDefault(name)));
        using var client = options.HttpClientFactory();

        var answered = collector.AnswerOne(Cancel);
        var response = await client.SendAsync(Export(collector.Port), Cancel);
        await answered;
        Assert.Equal(HttpStatusCode.Accepted, response.StatusCode);
        Assert.Equal([pki.Client.Thumbprint], collector.ClientCertificates);

        controls.Apply(new Pydantic.OpAmp.Client.Sdk.Settings { SendTraces = false });
        Assert.Equal(HttpStatusCode.OK, (await client.SendAsync(Export(collector.Port), Cancel)).StatusCode);
        Assert.Single(collector.ClientCertificates); // answered by the gate: nothing left
    }

    [Fact]
    public async Task WithoutTheCaTheCollectorIsNotTrusted()
    {
        var pki = new Pki();
        using var collector = new Collector(pki.Server);
        var env = pki.Env;
        env.Remove("OTEL_EXPORTER_OTLP_CERTIFICATE");
        var options = new OtlpExporterOptions();
        Controls.Gate(options, new Controls(), Signal.Traces, OtlpTls.FromEnv(name => env.GetValueOrDefault(name)));
        using var client = options.HttpClientFactory();

        var answered = collector.AnswerOne(Cancel);
        await Assert.ThrowsAsync<HttpRequestException>(() => client.SendAsync(Export(collector.Port), Cancel));
        // Under TLS 1.3 the collector's side of the handshake can finish before the exporter rejects
        // its certificate, so the collector sees either a failed handshake or a connection closed
        // before a request; never the request. An exporter that failed before it connected (to a
        // proxy, say) never reaches the collector: that fails here, with a deadline, not a hang.
        var seen = await Record.ExceptionAsync(() => answered.WaitAsync(TimeSpan.FromSeconds(10), Cancel));
        Assert.False(seen is TimeoutException, "the exporter never connected to the collector");
        Assert.Equal(0, collector.Requests);
    }

    [Fact]
    public void TheCaTrustsOnlyWhatItSigned()
    {
        var pki = new Pki();
        var other = new Pki();
        var tls = OtlpTls.FromEnv(name => pki.Env.GetValueOrDefault(name))!;
        Assert.True(tls.Trusted(pki.Server, SslPolicyErrors.RemoteCertificateChainErrors));
        Assert.False(tls.Trusted(other.Server, SslPolicyErrors.RemoteCertificateChainErrors));
        Assert.False(tls.Trusted(pki.Server, SslPolicyErrors.RemoteCertificateNameMismatch)); // the host is still checked
        Assert.True(tls.Trusted(other.Server, SslPolicyErrors.None)); // the system's trust still holds
    }

    [Fact]
    public void FilesThatCantBeUsedAreSaid()
    {
        Assert.Null(OtlpTls.FromEnv(_ => null));
        var warned = new List<string>();
        Assert.False(OtlpTls.TryFromEnv(n => n == "OTEL_EXPORTER_OTLP_CERTIFICATE" ? "/nonexistent/ca.pem" : null, warned.Add, out var tls));
        Assert.Null(tls);
        Assert.Single(warned);
        var pki = new Pki();
        Assert.False(OtlpTls.TryFromEnv(n => n == "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE" ? pki.CertPath : null, warned.Add, out _));
        Assert.Contains("go together", warned[^1], StringComparison.Ordinal);
        File.WriteAllText(pki.CaPath, "not a certificate");
        Assert.False(OtlpTls.TryFromEnv(n => n == "OTEL_EXPORTER_OTLP_CERTIFICATE" ? pki.CaPath : null, warned.Add, out _));
        Assert.True(OtlpTls.TryFromEnv(n => n == "OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE" ? "/etc/tls/client.pem" : null, warned.Add, out tls));
        Assert.Null(tls); // the SDK reads no TLS variables of a signal's own
    }
}
