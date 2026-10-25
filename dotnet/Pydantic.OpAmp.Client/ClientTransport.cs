// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Net;
using System.Net.Security;
using System.Security.Authentication;
using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;
using System.Text;

namespace Pydantic.OpAmp.Client;

/// <summary>
/// TLS for https:// and wss:// endpoints, and for package downloads. Only TLS 1.2 and 1.3 are
/// used, and the server's certificate is always verified.
/// </summary>
public sealed record OpAmpTlsOptions
{
    /// <summary>
    /// PEM of the CA certificates to trust for the server's certificate. They replace the
    /// system's, unless <see cref="IncludeSystemCas"/>. None: the system's.
    /// </summary>
    // covers: RULE-TLS-01
    public string? CaPem { get; init; }

    /// <summary>With <see cref="CaPem"/>: trust the system's CAs as well.</summary>
    public bool IncludeSystemCas { get; init; }

    /// <summary>
    /// PEM of the client certificate to present in every handshake, with
    /// <see cref="ClientKeyPem"/>. Certificates after the first are sent as its chain.
    /// </summary>
    // covers: RULE-TLS-02
    public string? ClientCertPem { get; init; }

    /// <summary>PEM of the client certificate's private key (PKCS #8, PKCS #1 or SEC 1; not encrypted).</summary>
    public string? ClientKeyPem { get; init; }

    /// <summary>The oldest TLS version: <see cref="SslProtocols.Tls12"/> (the default) or <see cref="SslProtocols.Tls13"/>.</summary>
    public SslProtocols MinVersion { get; init; }

    /// <summary>The newest TLS version: <see cref="SslProtocols.Tls13"/> (the default) or <see cref="SslProtocols.Tls12"/>.</summary>
    public SslProtocols MaxVersion { get; init; }

    /// <summary>What ToString prints: every member, but not <see cref="ClientKeyPem"/>'s value.</summary>
    private bool PrintMembers(StringBuilder builder) => Redacted.Print(
        builder,
        (nameof(this.CaPem), this.CaPem),
        (nameof(this.IncludeSystemCas), this.IncludeSystemCas),
        (nameof(this.ClientCertPem), this.ClientCertPem),
        (nameof(this.ClientKeyPem), Redacted.Secret(this.ClientKeyPem)),
        (nameof(this.MinVersion), this.MinVersion),
        (nameof(this.MaxVersion), this.MaxVersion));
}

/// <summary>
/// What the executor carries its effects over: requests and downloads, and WebSocket connections.
/// The client's own is <see cref="ClientTransport"/>; tests give one in memory, whose calls hold,
/// fail with any exception or complete after their cancel.
/// </summary>
internal interface IClientTransport : IDisposable
{
    /// <summary>
    /// Send <paramref name="request"/>, following no redirect, and return once the response's
    /// headers are in (its body is read from its content). <paramref name="ct"/> aborts it.
    /// </summary>
    Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken ct);

    /// <summary>
    /// A download's GET: the server's TLS settings (its client certificate) only for a file at the
    /// server's own origin (<paramref name="serverOrigin"/>, as the core says).
    /// </summary>
    Task<HttpResponseMessage> DownloadAsync(HttpRequestMessage request, bool serverOrigin, CancellationToken ct) =>
        this.SendAsync(request, ct);

    /// <summary>
    /// Open a WebSocket connection to <paramref name="url"/>, with <paramref name="headers"/> on the
    /// upgrade. <paramref name="ct"/> aborts the opening. On failure it throws: a
    /// <see cref="WebSocketConnectException"/> when the server answered (its status and headers),
    /// else what went wrong.
    /// </summary>
    Task<System.Net.WebSockets.WebSocket> ConnectAsync(Uri url, IEnumerable<KeyValuePair<string, string>> headers, CancellationToken ct);
}

/// <summary>A WebSocket upgrade the server answered, with something other than the connection.</summary>
internal sealed class WebSocketConnectException(Exception inner, int status, IReadOnlyDictionary<string, IEnumerable<string>>? headers)
    : Exception(inner.Message, inner)
{
    /// <summary>The answer's HTTP status.</summary>
    public int Status { get; } = status;

    /// <summary>The answer's headers, if any.</summary>
    public IReadOnlyDictionary<string, IEnumerable<string>>? Headers { get; } = headers;
}

/// <summary>
/// What carries the client's traffic: one handler for HTTP requests, WebSocket upgrades and
/// package downloads alike, so TLS and proxy settings apply to all three. Built from the options
/// once per client; the handler owns the connection pool.
/// </summary>
internal sealed class ClientTransport : IClientTransport
{
    private const string ServerAuth = "1.3.6.1.5.5.7.3.1";

    /// <summary>The environment variables .NET takes proxy URLs from.</summary>
    private static readonly string[] EnvironmentProxies = ["http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"];

    private readonly bool owned;

    /// <summary>
    /// Downloads of files not at the server's own origin: the same settings without the client
    /// certificate, which is the server's alone. Null: <see cref="Http"/> serves (no client
    /// certificate, or the application's HttpClient, which is its own).
    /// </summary>
    private readonly HttpClient? elsewhere;

    private ClientTransport(HttpClient http, HttpMessageInvoker webSocket, bool owned, HttpClient? elsewhere = null)
    {
        this.Http = http;
        this.WebSocket = webSocket;
        this.owned = owned;
        this.elsewhere = elsewhere;
    }

    /// <summary>For requests and downloads. It follows no redirect: a 3xx goes to the core as it is.</summary>
    public HttpClient Http { get; }

    /// <summary>For WebSocket upgrades.</summary>
    public HttpMessageInvoker WebSocket { get; }

    /// <summary>
    /// The application's HttpClient, if it gave one (for everything: it can't be combined with
    /// <see cref="OpAmpClientOptions.Tls"/> or <see cref="OpAmpClientOptions.Proxy"/>), or one of
    /// our own built from those. Bad settings throw here, at start.
    /// </summary>
    public static ClientTransport Create(OpAmpClientOptions options)
    {
        if (options.ProxyResolveLocally)
        {
            throw new ArgumentException(
                options.Proxy is { IsAbsoluteUri: true, Scheme: "http" or "https" }
                    ? "ProxyResolveLocally can't be honoured: an HTTP proxy is given the server's host name (CONNECT host:port) and resolves it itself"
                    : "ProxyResolveLocally can't be honoured in platform mode: .NET's SOCKS5 support always sends the server's host name for the proxy to resolve (core mode can: no HttpClient, and Mode unset or Core)",
                nameof(options));
        }

        if (options.HttpClient is { } app)
        {
            if (options.Tls is not null || options.Proxy is not null || options.DirectConnection)
            {
                throw new ArgumentException("HttpClient can't be combined with Tls, Proxy or DirectConnection: set them on its handler, or leave HttpClient out", nameof(options));
            }

            return Of(app);
        }

        if (options.Proxy is not null && options.DirectConnection)
        {
            throw new ArgumentException("Proxy and DirectConnection don't go together", nameof(options));
        }

        return Create(options.Tls, options.Proxy, options.DirectConnection);
    }

    /// <summary>One of our own: a handler with these settings, for everything.</summary>
    internal static ClientTransport Create(OpAmpTlsOptions? tls, Uri? proxy, bool direct = false)
    {
        var handler = CreateHandler(tls, proxy, direct);
        var elsewhere = tls?.ClientCertPem is null
            ? null
            : new HttpClient(CreateHandler(tls with { ClientCertPem = null, ClientKeyPem = null }, proxy, direct), disposeHandler: true)
            {
                Timeout = Timeout.InfiniteTimeSpan,
            };
        return new ClientTransport(
            new HttpClient(handler, disposeHandler: true) { Timeout = Timeout.InfiniteTimeSpan }, // the core times requests
            new HttpMessageInvoker(handler, disposeHandler: false),
            owned: true,
            elsewhere);
    }

    /// <summary>The application's HttpClient, for everything.</summary>
    internal static ClientTransport Of(HttpClient app) => new(app, app, owned: false);

    /// <summary>
    /// A handler with these settings: through <paramref name="proxy"/>, or direct, or else through
    /// the system's proxy (<see cref="HttpClient.DefaultProxy"/>). Under CGI (REQUEST_METHOD set,
    /// read through <paramref name="getenv"/>, the process's environment by default) the
    /// environment's proxy variables may come from the request being served (a Proxy header is
    /// HTTP_PROXY there: httpoxy), so the platform's proxy is used only if the application set
    /// one itself (a <see cref="WebProxy"/> as <see cref="HttpClient.DefaultProxy"/>); otherwise
    /// the handler goes direct, as the Go, Python and Rust clients do. A configured
    /// <paramref name="proxy"/> applies under CGI too. It doesn't follow redirects or
    /// decompress: a 3xx goes to the core, which decides what it means, and it asks for no compression.
    /// </summary>
    // covers: RULE-PROXY-01
    internal static SocketsHttpHandler CreateHandler(OpAmpTlsOptions? tls, Uri? proxy, bool direct = false, Func<string, string?>? getenv = null)
    {
        getenv ??= Environment.GetEnvironmentVariable;
        var handler = new SocketsHttpHandler
        {
            AllowAutoRedirect = false,
            AutomaticDecompression = DecompressionMethods.None,
            SslOptions = SslOptions(tls ?? new OpAmpTlsOptions()),
        };
        if (proxy is not null)
        {
            handler.Proxy = Proxy(proxy);
            handler.UseProxy = true;
        }
        else if (direct)
        {
            handler.UseProxy = false;
        }
        else if (getenv("REQUEST_METHOD") is not null && HttpClient.DefaultProxy is not WebProxy { Address: not null })
        {
            handler.UseProxy = false; // CGI: nothing from the environment's proxy variables
        }
        else
        {
            CheckPlatformProxy(getenv);
        }

        return handler;
    }

    public Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken ct) =>
        this.Http.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, ct);

    public Task<HttpResponseMessage> DownloadAsync(HttpRequestMessage request, bool serverOrigin, CancellationToken ct) =>
        (serverOrigin ? this.Http : this.elsewhere ?? this.Http).SendAsync(request, HttpCompletionOption.ResponseHeadersRead, ct);

    /// <summary>
    /// .NET can't take back a connect cleanly: a cancel cancels it and aborts the socket.
    /// </summary>
    public async Task<System.Net.WebSockets.WebSocket> ConnectAsync(Uri url, IEnumerable<KeyValuePair<string, string>> headers, CancellationToken ct)
    {
        var ws = new System.Net.WebSockets.ClientWebSocket();
        try
        {
            using var abort = ct.Register(ws.Abort);
            ws.Options.CollectHttpResponseDetails = true;
            foreach (var h in headers)
            {
                ws.Options.SetRequestHeader(h.Key, h.Value);
            }

            await ws.ConnectAsync(url, this.WebSocket, ct).ConfigureAwait(false);
            return ws;
        }
        catch (Exception e)
        {
            var status = ws.HttpStatusCode;
            var answered = ws.HttpResponseHeaders;
            ws.Dispose();
            if (status != 0)
            {
                throw new WebSocketConnectException(e, (int)status, answered);
            }

            throw;
        }
    }

    public void Dispose()
    {
        if (this.owned)
        {
            this.WebSocket.Dispose();
            this.Http.Dispose();
            this.elsewhere?.Dispose();
        }
    }

    /// <summary>The operator's TLS settings as SslStream takes them; bad ones throw (at start).</summary>
    internal static SslClientAuthenticationOptions SslOptions(OpAmpTlsOptions tls)
    {
        var ssl = new SslClientAuthenticationOptions { EnabledSslProtocols = Protocols(tls) };
        if (tls.CaPem is { } caPem)
        {
            var cas = new X509Certificate2Collection();
            try
            {
                cas.ImportFromPem(caPem);
            }
            catch (CryptographicException e)
            {
                throw new ArgumentException($"Tls.CaPem: {e.Message}", nameof(tls), e);
            }

            if (cas.Count == 0)
            {
                throw new ArgumentException("Tls.CaPem holds no certificate", nameof(tls));
            }

            if (tls.IncludeSystemCas)
            {
                // The system's chain first; if it fails only for want of a root, these roots.
                ssl.RemoteCertificateValidationCallback = (_, cert, chain, errors) => TrustedBySystemOr(cas, cert, chain, errors);
            }
            else
            {
                ssl.CertificateChainPolicy = Policy(cas);
            }
        }

        if (tls.ClientCertPem is not null || tls.ClientKeyPem is not null)
        {
            ssl.ClientCertificateContext = ClientCertificate(tls);
        }

        return ssl;
    }

    /// <summary>TLS 1.2 and 1.3, or the part of that the options ask for.</summary>
    private static SslProtocols Protocols(OpAmpTlsOptions tls)
    {
        var min = tls.MinVersion == SslProtocols.None ? SslProtocols.Tls12 : tls.MinVersion;
        var max = tls.MaxVersion == SslProtocols.None ? SslProtocols.Tls13 : tls.MaxVersion;
        if (min is not (SslProtocols.Tls12 or SslProtocols.Tls13) || max is not (SslProtocols.Tls12 or SslProtocols.Tls13))
        {
            throw new ArgumentException("Tls.MinVersion and Tls.MaxVersion are Tls12 or Tls13", nameof(tls));
        }

        if (min > max)
        {
            throw new ArgumentException("Tls.MinVersion is newer than Tls.MaxVersion", nameof(tls));
        }

        return min == max ? min : SslProtocols.Tls12 | SslProtocols.Tls13;
    }

    /// <summary>Chains end at one of these roots, and nowhere else.</summary>
    private static X509ChainPolicy Policy(X509Certificate2Collection roots)
    {
        var policy = new X509ChainPolicy
        {
            TrustMode = X509ChainTrustMode.CustomRootTrust,
            RevocationMode = X509RevocationMode.NoCheck, // as SslStream's default
        };
        policy.CustomTrustStore.AddRange(roots);
        policy.ApplicationPolicy.Add(new Oid(ServerAuth));
        return policy;
    }

    private static bool TrustedBySystemOr(X509Certificate2Collection roots, X509Certificate? cert, X509Chain? chain, SslPolicyErrors errors)
    {
        if (errors == SslPolicyErrors.None)
        {
            return true;
        }

        // A wrong name, or no certificate, is never excused.
        if (errors != SslPolicyErrors.RemoteCertificateChainErrors || cert is null)
        {
            return false;
        }

        using var custom = new X509Chain { ChainPolicy = Policy(roots) };
        if (chain is not null)
        {
            foreach (var e in chain.ChainElements)
            {
                custom.ChainPolicy.ExtraStore.Add(e.Certificate); // the intermediates the server sent
            }
        }

        return custom.Build(cert as X509Certificate2 ?? new X509Certificate2(cert));
    }

    private static SslStreamCertificateContext ClientCertificate(OpAmpTlsOptions tls)
    {
        if (tls.ClientCertPem is not { } certPem || tls.ClientKeyPem is not { } keyPem)
        {
            throw new ArgumentException("Tls.ClientCertPem and Tls.ClientKeyPem go together", nameof(tls));
        }

        X509Certificate2 cert;
        var chain = new X509Certificate2Collection();
        try
        {
            cert = X509Certificate2.CreateFromPem(certPem, keyPem);
            chain.ImportFromPem(certPem);
        }
        catch (CryptographicException e)
        {
            throw new ArgumentException($"Tls.ClientCertPem, Tls.ClientKeyPem: {e.Message}", nameof(tls), e);
        }

        if (OperatingSystem.IsWindows())
        {
            // SChannel can't use a key that lives only in memory: give it one it can.
            using var ephemeral = cert;
#if NET9_0_OR_GREATER
            cert = X509CertificateLoader.LoadPkcs12(ephemeral.Export(X509ContentType.Pkcs12), null);
#else
            cert = new X509Certificate2(ephemeral.Export(X509ContentType.Pkcs12));
#endif
        }

        chain.RemoveAt(0); // the certificate itself
        return SslStreamCertificateContext.Create(cert, chain, offline: true);
    }

    /// <summary>
    /// http://, https://, socks5:// or socks5h://, with the credentials the URL carries
    /// (user:password@). socks5h:// is socks5:// here: .NET's SOCKS5 sends the host name, so the
    /// proxy resolves it either way (.NET doesn't know the socks5h name itself).
    /// </summary>
    private static WebProxy Proxy(Uri uri)
    {
        if (!uri.IsAbsoluteUri)
        {
            throw new ArgumentException("Proxy is an http://, https://, socks5:// or socks5h:// URL", nameof(uri));
        }

        var scheme = Scheme(uri.Scheme, "Proxy", socks5h: true);
        var proxy = new WebProxy(new UriBuilder(uri) { Scheme = scheme, UserName = string.Empty, Password = string.Empty }.Uri);
        if (uri.UserInfo.Length > 0)
        {
            var colon = uri.UserInfo.IndexOf(':', StringComparison.Ordinal);
            var user = colon < 0 ? uri.UserInfo : uri.UserInfo[..colon];
            var password = colon < 0 ? string.Empty : uri.UserInfo[(colon + 1)..];
            proxy.Credentials = new NetworkCredential(Uri.UnescapeDataString(user), Uri.UnescapeDataString(password));
        }

        return proxy;
    }

    /// <summary>
    /// The platform's proxy (<see cref="HttpClient.DefaultProxy"/>) keeps to the same schemes, as far
    /// as we can see it: one the application set as a <see cref="WebProxy"/>, or else the
    /// environment's proxy URLs, which .NET reads. .NET would use a SOCKS4 one, and ignore one it
    /// can't read (socks5h://, or an unknown scheme) and connect directly; both are errors here.
    /// System proxy settings (Windows, macOS) aren't checked.
    /// </summary>
    private static void CheckPlatformProxy(Func<string, string?> getenv)
    {
        if (HttpClient.DefaultProxy is WebProxy { Address: { } set })
        {
            _ = Scheme(set.Scheme, "HttpClient.DefaultProxy", socks5h: false);
            return;
        }

        foreach (var name in EnvironmentProxies)
        {
            if (getenv(name) is not { Length: > 0 } value)
            {
                continue;
            }

            var end = value.IndexOf("://", StringComparison.Ordinal);
            if (end >= 0)
            {
                _ = Scheme(value[..end], $"The environment's {AsSet(name)}", socks5h: false); // host:port alone is http://
            }
        }
    }

    /// <summary>
    /// Variable <paramref name="name"/> as the environment spells it. On Windows names don't differ
    /// by case: all_proxy and ALL_PROXY are one variable, and an error names it as it was set.
    /// </summary>
    private static string AsSet(string name)
    {
        if (OperatingSystem.IsWindows())
        {
            foreach (var key in Environment.GetEnvironmentVariables().Keys)
            {
                if (key is string set && string.Equals(set, name, StringComparison.OrdinalIgnoreCase))
                {
                    return set;
                }
            }
        }

        return name;
    }

    /// <summary>
    /// A proxy's scheme as .NET's WebProxy takes it: http, https or socks5 (socks5h, if
    /// <paramref name="socks5h"/>, as socks5). Anything else is a configuration error: never a
    /// SOCKS4 proxy, and never a direct connection instead.
    /// </summary>
    private static string Scheme(string scheme, string what, bool socks5h)
    {
        if (scheme.Length == 0 || !scheme.All(c => char.IsAsciiLetterOrDigit(c) || c is '+' or '-' or '.'))
        {
            // Not a scheme at all: say nothing of it, in case it is part of a password.
            throw new ArgumentException($"{what} isn't a proxy URL: it is http://, https://, socks5://{(socks5h ? ", socks5h://" : string.Empty)} or host:port");
        }

        switch (scheme.ToLowerInvariant())
        {
            case "http":
            case "https":
            case "socks5":
                return scheme.ToLowerInvariant();
            case "socks5h" when socks5h:
                return "socks5";
            case "socks5h":
                throw new ArgumentException($"{what} is a socks5h:// URL, which .NET's platform proxy doesn't take: write it as socks5:// (the proxy still resolves host names), or set OpAmpClientOptions.Proxy");
            case "socks4":
            case "socks4a":
                throw new ArgumentException($"{what} is a {scheme}:// URL: SOCKS4 proxies aren't supported, use socks5://");
            default:
                throw new ArgumentException($"{what} is a {scheme}:// URL: a proxy is http://, https:// or socks5://{(socks5h ? " (or socks5h://)" : string.Empty)}");
        }
    }
}
