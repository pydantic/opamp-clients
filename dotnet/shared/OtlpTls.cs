// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Net.Security;
using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;

namespace Pydantic.OpAmp.Otel;

/// <summary>
/// The TLS an OTLP exporter builds into its own HTTP client: a CA to trust
/// (OTEL_EXPORTER_OTLP_CERTIFICATE), and a client certificate and its key
/// (OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE, _CLIENT_KEY), PEM files. The SDK reads these for every
/// signal alike (it has no per-signal ones). A gated exporter's HTTP client is ours, so it carries
/// the same: no certificate is lost, and the signals stay controllable.
/// </summary>
internal sealed class OtlpTls
{
    private readonly X509Certificate2Collection? ca;
    private readonly X509Certificate2? client;

    private OtlpTls(X509Certificate2Collection? ca, X509Certificate2? client)
    {
        this.ca = ca;
        this.client = client;
    }

    /// <summary>
    /// What the variables say: null when none is set. Throws when a file can't be read or a
    /// certificate comes without its key (or a key without its certificate): the SDK's exporter
    /// does with those what a gate can't know, so the caller leaves its exporters ungated.
    /// </summary>
    public static OtlpTls? FromEnv(Func<string, string?> getenv)
    {
        string? Path(string name) => getenv($"OTEL_EXPORTER_OTLP_{name}") is { Length: > 0 } v ? v.Trim() : null;
        var caPath = Path("CERTIFICATE");
        var certPath = Path("CLIENT_CERTIFICATE");
        var keyPath = Path("CLIENT_KEY");
        if (caPath is null && certPath is null && keyPath is null)
        {
            return null;
        }

        if ((certPath is null) != (keyPath is null))
        {
            throw new ArgumentException("a client certificate and its key go together (OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE, _CLIENT_KEY)");
        }

        X509Certificate2Collection? ca = null;
        if (caPath is not null)
        {
            ca = [];
            ca.ImportFromPemFile(caPath);
            if (ca.Count == 0)
            {
                throw new ArgumentException($"no certificate in {caPath} (OTEL_EXPORTER_OTLP_CERTIFICATE)");
            }
        }

        X509Certificate2? client = null;
        if (certPath is not null)
        {
            // Through PKCS #12: on Windows, SslStream can't use the ephemeral key a PEM load gives.
            using var pem = X509Certificate2.CreateFromPemFile(certPath, keyPath);
            client = new X509Certificate2(pem.Export(X509ContentType.Pkcs12));
        }

        return new OtlpTls(ca, client);
    }

    /// <summary>
    /// <see cref="FromEnv"/>, or false, saying why, when the files can't be used: the exporters
    /// then keep their own HTTP clients, and no signal can be stopped.
    /// </summary>
    public static bool TryFromEnv(Func<string, string?> getenv, Action<string> warn, out OtlpTls? tls)
    {
        try
        {
            tls = FromEnv(getenv);
            return true;
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException or ArgumentException or CryptographicException)
        {
            tls = null;
            warn($"the OTLP exporters' TLS files can't be used ({e.Message}): no export can be stopped");
            return false;
        }
    }

    /// <summary>Put the certificates in a handler's TLS options.</summary>
    public void Configure(SocketsHttpHandler handler)
    {
        if (this.client is not null)
        {
            handler.SslOptions.ClientCertificates = new X509CertificateCollection { this.client };
        }

        if (this.ca is not null)
        {
            handler.SslOptions.RemoteCertificateValidationCallback = (_, cert, _, errors) => this.Trusted(cert, errors);
        }
    }

    /// <summary>
    /// A server certificate the system trusts, or one that chains to the CA's certificates, for the
    /// host asked for: only a chain the system can't build is let off, and only to the CA.
    /// </summary>
    internal bool Trusted(X509Certificate? cert, SslPolicyErrors errors)
    {
        if (errors == SslPolicyErrors.None)
        {
            return true;
        }

        if (errors != SslPolicyErrors.RemoteCertificateChainErrors || cert is null || this.ca is null)
        {
            return false;
        }

        using var chain = new X509Chain();
        chain.ChainPolicy.TrustMode = X509ChainTrustMode.CustomRootTrust;
        chain.ChainPolicy.CustomTrustStore.AddRange(this.ca);
        chain.ChainPolicy.RevocationMode = X509RevocationMode.NoCheck;
        using var leaf = new X509Certificate2(cert);
        return chain.Build(leaf);
    }
}
