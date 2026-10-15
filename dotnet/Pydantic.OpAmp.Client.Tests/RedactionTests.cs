// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using Google.Protobuf;
using OpAmp.Proto.V1;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// Noninterference of secrets, as Python's tests/test_redaction.py and the core's own tests: two options that differ only in their secrets' values, each as long in both,
/// print the same. What an application logs of them (a record's ToString, as <c>$"{options}"</c>
/// or a structured logger formats it) is the same text. The secrets: the headers sent to the
/// server, a password in the endpoint's or the proxy's URL, and the client key. The same for the
/// notifications that carry what the server offers: a ConnectionSettings and an InstallPackage
/// print no header's value, private key, or password in an endpoint's, proxy's or download's URL.
/// </summary>
public class RedactionTests
{
    // A secret's characters: those a URL's user-info holds as they are.
    private const string Chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~!$&'()*+,;=";

    private static string Shown(string token, string password, string key)
    {
        var tls = new OpAmpTlsOptions { CaPem = "CA", ClientCertPem = "CERT", ClientKeyPem = key };
        var options = new OpAmpClientOptions
        {
            Endpoint = new Uri($"https://agent:{password}@opamp.example/v1/opamp"),
            Headers = new Dictionary<string, string> { ["Authorization"] = token },
            Tls = tls,
            Proxy = new Uri($"http://agent:{password}@proxy.example:3128"),
        };
        return $"{tls}\n{options}\n{Offered(token, password, key)}";
    }

    private static string Offered(string token, string password, string key)
    {
        Headers Headers() => new() { Headers_ = { new Header { Key = "Authorization", Value = token } } };
        TLSCertificate Certificate() => new()
        {
            Cert = ByteString.CopyFromUtf8("CERT"),
            PrivateKey = ByteString.CopyFromUtf8(key),
            CaCert = ByteString.CopyFromUtf8("CA"),
        };
        ProxyConnectionSettings Proxy() => new() { Url = $"http://agent:{password}@proxy.example:3128", ConnectHeaders = Headers() };
        var offers = new ConnectionSettingsOffers
        {
            Opamp = new OpAMPConnectionSettings
            {
                DestinationEndpoint = $"https://agent:{password}@opamp.example/v1/opamp",
                Headers = Headers(),
                Certificate = Certificate(),
                Proxy = Proxy(),
            },
            OwnLogs = new TelemetryConnectionSettings
            {
                DestinationEndpoint = $"https://agent:{password}@otlp.example/v1/logs",
                Headers = Headers(),
                Certificate = Certificate(),
                Proxy = Proxy(),
            },
            OtherConnections =
            {
                ["db"] = new OtherConnectionSettings
                {
                    DestinationEndpoint = $"postgres://agent:{password}@db.example:5432",
                    Headers = Headers(),
                    Certificate = Certificate(),
                    Proxy = Proxy(),
                },
            },
        };
        var package = new PackageAvailable
        {
            Version = "1.0",
            File = new DownloadableFile { DownloadUrl = $"https://agent:{password}@cdn.example/pkg", Headers = Headers() },
        };
        return $"{new OpAmpNotification.ConnectionSettings(offers)}\n{new OpAmpNotification.InstallPackage("pkg", package, "/downloads/pkg")}";
    }

    /// <summary>Two different secrets of one length.</summary>
    private static (string, string) Pair(Random random)
    {
        var length = random.Next(1, 41);
        string One() => new(Enumerable.Range(0, length).Select(_ => Chars[random.Next(Chars.Length)]).ToArray());
        var a = One();
        var b = One();
        while (b == a)
        {
            b = One();
        }

        return (a, b);
    }

    [Fact]
    public void SecretsChangeNothingShown()
    {
        var random = new Random(120);
        for (var i = 0; i < 200; i++)
        {
            var (token, password, key) = (Pair(random), Pair(random), Pair(random));
            Assert.Equal(Shown(token.Item1, password.Item1, key.Item1), Shown(token.Item2, password.Item2, key.Item2));
        }
    }

    [Fact]
    public void WhatIsNotSecretIsStillShown()
    {
        var shown = Shown("tok", "pw", "key");
        foreach (var part in new[] { "opamp.example/v1/opamp", "Authorization", "proxy.example:3128", "agent", "CERT", "CA", "otlp.example/v1/logs", "db.example:5432", "cdn.example/pkg", "/downloads/pkg", "\"value\": \"<redacted>\"", "\"privateKey\": \"<redacted>\"" })
        {
            Assert.Contains(part, shown, StringComparison.Ordinal);
        }
    }

    /// <summary>Each type prints its own members, so a property added later is printed too, or this fails.</summary>
    [Fact]
    public void EveryPropertyIsPrinted()
    {
        var tls = new OpAmpTlsOptions();
        var options = new OpAmpClientOptions { Endpoint = new Uri("https://opamp.example/v1/opamp") };
        var settings = new OpAmpNotification.ConnectionSettings(new ConnectionSettingsOffers());
        var install = new OpAmpNotification.InstallPackage("pkg", new PackageAvailable(), "/downloads/pkg");
        foreach (var (value, type) in new (object, Type)[]
        {
            (tls, typeof(OpAmpTlsOptions)),
            (options, typeof(OpAmpClientOptions)),
            (settings, typeof(OpAmpNotification.ConnectionSettings)),
            (install, typeof(OpAmpNotification.InstallPackage)),
        })
        {
            var shown = value.ToString();
            foreach (var property in type.GetProperties(System.Reflection.BindingFlags.Public | System.Reflection.BindingFlags.Instance))
            {
                Assert.Contains($"{property.Name} = ", shown, StringComparison.Ordinal);
            }
        }
    }
}
