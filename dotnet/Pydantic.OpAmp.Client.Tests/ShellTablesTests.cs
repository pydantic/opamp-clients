// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

using System.Collections.Concurrent;
using System.Diagnostics;
using System.Net;
using System.Runtime.CompilerServices;
using System.Security.Authentication;
using System.Text.Json;
using Google.Protobuf;
using Pydantic.OpAmp.Core;
using Xunit;

namespace Pydantic.OpAmp.Client.Tests;

/// <summary>
/// The shells' shared tables (tests/shell-tables/), as this client reads them: every URL the
/// core accepts is taken by the executor's URL type, read alike and sent as exactly its target;
/// the environment's proxies route as the table says; every TLS setting is taken or refused as it
/// says. Where .NET's convention differs, the case is listed below with the reason, and the test
/// fails if the behaviour changes. In Rust mode (the core sends the requests itself, over .NET's
/// streams) every URL is sent as its target by the core, the proxy is the table's with no
/// difference at all (tests/shell-tables/README.md, "Rust mode's proxy rule"), and the TLS settings
/// are taken or refused as in platform mode. The tables are found at OPAMP_SHELL_TABLES (a Docker
/// run that mounts only dotnet/ mounts them there), or else at tests/shell-tables above the tests.
/// </summary>
public class ShellTablesTests
{
    /// <summary>
    /// Proxy cases where platform mode's choice (HttpClient.DefaultProxy, read from the environment
    /// by HttpEnvironmentProxy, held to the schemes the client takes) differs from the reference:
    /// what .NET picks ("refused": the client refuses the configuration at start), and why. Rust
    /// mode has none (<see cref="RustModesProxyIsTheTablesExactly"/>).
    /// </summary>
    private static readonly Dictionary<string, (string? Proxy, string Why)> ProxyDifferences = new()
    {
        ["http-proxy-upper-first"] = ("http://lower-proxy:1", ".NET reads the lower-case name first (curl's order), the reference the upper-case one"),
        ["no-proxy-subdomain"] = ("http://web-proxy:3128", ".NET's NO_PROXY: a name without a leading dot is that host only; .example.com covers its subdomains"),
        ["no-proxy-star"] = ("http://web-proxy:3128", ".NET's NO_PROXY has no * entry"),
        ["no-proxy-cidr"] = ("http://web-proxy:3128", ".NET's NO_PROXY has no CIDR networks: entries are names (or addresses) compared as text"),
        ["no-proxy-ipv6"] = ("http://web-proxy:3128", ".NET's NO_PROXY compares the host as text, which for an IPv6 literal is [::1], not ::1"),
        ["upper-empty-hides-lower"] = ("http://lower-proxy:1", ".NET reads the lower-case name first, and an empty one as unset: the upper-case one, empty, hides nothing"),
        ["value-trimmed"] = ("refused", ".NET doesn't trim the variable, so \"  http\" is no scheme it knows: the client refuses at start rather than go direct"),
        ["no-proxy-ipv6-cidr"] = ("http://web-proxy:3128", ".NET's NO_PROXY has no CIDR networks"),
        ["no-proxy-network-host-bits"] = ("http://web-proxy:3128", ".NET's NO_PROXY has no CIDR networks"),
        ["no-proxy-bracketed-entry"] = (null, ".NET's NO_PROXY compares the host as text, [::1] with its brackets, so the bracketed entry matches it"),
    };

    /// <summary>TLS cases where .NET differs from the reference: what it does, and why.</summary>
    private static readonly Dictionary<string, (string Got, string Why)> TlsDifferences = new()
    {
    };

    private readonly ITestOutputHelper output;

    public ShellTablesTests(ITestOutputHelper output) => this.output = output;

    /// <summary>The directory of the tables.</summary>
    internal static string Dir()
    {
        if (Environment.GetEnvironmentVariable("OPAMP_SHELL_TABLES") is { Length: > 0 } set)
        {
            return Directory.Exists(set) ? set : throw new DirectoryNotFoundException($"OPAMP_SHELL_TABLES={set}: no such directory");
        }

        for (var dir = new DirectoryInfo(AppContext.BaseDirectory); dir is not null; dir = dir.Parent)
        {
            var tables = Path.Combine(dir.FullName, "tests", "shell-tables");
            if (Directory.Exists(tables))
            {
                return tables;
            }
        }

        throw new DirectoryNotFoundException($"no tests/shell-tables above {AppContext.BaseDirectory} (set OPAMP_SHELL_TABLES)");
    }

    private static JsonElement Table(string name) => JsonDocument.Parse(File.ReadAllText(Path.Combine(Dir(), name))).RootElement;

    private static string? Str(JsonElement e, string name) =>
        e.TryGetProperty(name, out var v) && v.ValueKind == JsonValueKind.String ? v.GetString() : null;

    // Every URL the core accepts: the executor's URL type takes it and reads its parts as the
    // core does, and the executor's request sends exactly its target.
    [Fact]
    public async Task EveryUrlTheCoreAcceptsIsTakenAndSentAsItsTarget()
    {
        var targets = new ConcurrentQueue<string>();
        using var server = new RawServer(async c =>
        {
            while (true)
            {
                var (line, headers) = await c.ReadHeadAsync();
                targets.Enqueue(line.Split(' ')[1]);
                await c.ReadBodyAsync(headers);
                await Http1.WriteAsync(c.Stream, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n");
            }
        });
        var loop = new TestLoop(ClientTransport.Create(null, null, direct: true));
        var urls = Table("urls.json").GetProperty("urls").EnumerateArray().ToList();
        Assert.True(urls.Count > 200);
        var wrong = new List<string>();
        ulong id = 0;
        foreach (var e in urls)
        {
            var url = Str(e, "url")!;
            var want = (Host: Str(e, "host")!, Port: e.GetProperty("port").GetInt32(), Path: Str(e, "path")!, Query: Str(e, "query"));
            try
            {
                var u = Executor.RequestUri(url);
                var hostOk = u.HostNameType == UriHostNameType.IPv6
                    ? IPAddress.TryParse(want.Host.Trim('[', ']'), out var a) && IPAddress.Parse(u.Host.Trim('[', ']')).Equals(a)
                    : string.Equals(u.Host, want.Host, StringComparison.OrdinalIgnoreCase);
                var query = u.Query.Length == 0 ? null : u.Query[1..];
                if (!hostOk || u.Port != want.Port || u.AbsolutePath != want.Path || query != want.Query)
                {
                    wrong.Add($"{url}: read as ({u.Host}, {u.Port}, {u.AbsolutePath}, {query ?? "null"}), the core as {want}");
                }
            }
            catch (Exception x) when (x is UriFormatException or InvalidOperationException)
            {
                wrong.Add($"{url}: refused: {x.Message}");
            }

            var target = Str(e, "target")!;
            loop.Run(new Effect
            {
                HttpRequest = new HttpRequest { Id = ++id, Url = $"http://127.0.0.1:{server.Port}{target}", TimeoutMs = 10_000, MaxResponseBytes = 1 << 20 },
            });
            var input = loop.Next();
            if (input.KindCase != Input.KindOneofCase.HttpResponse)
            {
                wrong.Add($"{url}: not sent: {input}");
                continue;
            }

            var sent = targets.Last();
            if (sent != target)
            {
                wrong.Add($"{url}: sent {sent}, not {target}");
            }
        }

        // An empty path is sent as /: the table's targets all start with one.
        foreach (var (rest, target) in new[] { (string.Empty, "/"), ("?a=%41", "/?a=%41") })
        {
            loop.Run(new Effect
            {
                HttpRequest = new HttpRequest { Id = ++id, Url = $"http://127.0.0.1:{server.Port}{rest}", TimeoutMs = 10_000, MaxResponseBytes = 1 << 20 },
            });
            Assert.Equal(Input.KindOneofCase.HttpResponse, loop.Next().KindCase);
            Assert.Equal(target, targets.Last());
        }

        await loop.X.ReleaseAsync(TimeSpan.FromSeconds(5));
        Assert.True(wrong.Count == 0, $"{wrong.Count} of {urls.Count}:\n{string.Join('\n', wrong)}");
    }

    // A WebSocket upgrade's target is the core's too, escapes and all.
    [Fact]
    public async Task AWebSocketUpgradeIsSentAsItsTarget()
    {
        var target = new TaskCompletionSource<string>(TaskCreationOptions.RunContinuationsAsynchronously);
        using var server = new RawServer(async c =>
        {
            var (line, headers) = await c.ReadHeadAsync();
            target.TrySetResult(line.Split(' ')[1]);
            using var ws = await c.AcceptAsync(headers);
            await Task.Delay(Timeout.Infinite);
        });
        var loop = new TestLoop(ClientTransport.Create(null, null, direct: true));
        loop.Run(new Effect { WsOpen = new WsOpen { Id = 1, Url = $"ws://127.0.0.1:{server.Port}/a%41/%7e%2f?q=%42", MaxMessageBytes = 1 << 20 } });
        Assert.Equal(Input.KindOneofCase.WsOpened, loop.Next().KindCase);
        Assert.Equal("/a%41/%7e%2f?q=%42", await target.Task.WaitAsync(TimeSpan.FromSeconds(10)));
        await loop.X.ReleaseAsync(TimeSpan.FromSeconds(5));
    }

    // Every URL the core accepts, in Rust mode: a client on it sends exactly its target, over its
    // own stream (the core writes the request; .NET only carries its bytes).
    [Fact]
    public async Task RustModeSendsEveryUrlAsItsTarget()
    {
        var targets = new ConcurrentQueue<string>();
        using var server = new RawServer(async c =>
        {
            while (true)
            {
                var (line, headers) = await c.ReadHeadAsync();
                targets.Enqueue(line.Split(' ')[1]);
                if (headers.ContainsKey("Upgrade"))
                {
                    return; // seen: no need to open it
                }

                await c.ReadBodyAsync(headers);
                await Http1.WriteAsync(c.Stream, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n");
            }
        });
        var urls = Table("urls.json").GetProperty("urls").EnumerateArray().ToList();
        Assert.True(urls.Count > 200);
        var wrong = new List<string>();
        foreach (var e in urls)
        {
            var url = Str(e, "url")!;
            var target = Str(e, "target")!;
            var ws = Str(e, "scheme") is "ws" or "wss";
            var endpoint = $"{(ws ? "ws" : "http")}://127.0.0.1:{server.Port}{target}";
            var before = targets.Count;
            var client = OpAmpClient.Start(
                new OpAmpClientOptions
                {
                    Endpoint = new Uri(endpoint),
                    Mode = OpAmpMode.Rust,
                    DirectConnection = true,
                    HeartbeatInterval = TimeSpan.FromMinutes(1),
                    InstanceUid = Enumerable.Repeat((byte)1, 16).ToArray(),
                    StopTimeout = TimeSpan.FromSeconds(1),
                },
                new OpAmpStatus());
            try
            {
                if (!await Wait.ForAsync(() => targets.Count > before))
                {
                    wrong.Add($"{url}: nothing sent for {endpoint}");
                    continue;
                }
            }
            finally
            {
                await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
            }

            var sent = targets.ElementAt(before);
            if (sent != target)
            {
                wrong.Add($"{url}: sent {sent}, not {target}");
            }
        }

        Assert.True(wrong.Count == 0, $"{wrong.Count} of {urls.Count}:\n{string.Join('\n', wrong)}");
    }

    // Rust mode's proxy: the table's, exactly, with no known difference. The client hands the core
    // the environment's variables (Config.proxy) and the core picks by the shared rule: each case
    // through this client's reading of the environment (injected) and the core it ships.
    // covers: RULE-PROXY-02
    [Fact]
    public void RustModesProxyIsTheTablesExactly()
    {
        var cases = Table("proxy.json").GetProperty("cases").EnumerateArray().ToList();
        Assert.True(cases.Count > 30);
        var wrong = new List<string>();
        foreach (var c in cases)
        {
            var env = c.GetProperty("env").EnumerateObject().ToDictionary(p => p.Name, p => p.Value.GetString()!);
            var url = Str(c, "url")!;
            var choice = OpAmpClient.RustProxyChoice(new OpAmpClientOptions { Endpoint = new Uri(url), Mode = OpAmpMode.Rust }, n => env.GetValueOrDefault(n));
            var got = CoreRoute(choice, url);
            if (got != Str(c, "proxy"))
            {
                wrong.Add($"{Str(c, "id")}: {got ?? "direct"}, the table says {Str(c, "proxy") ?? "direct"} ({Str(c, "why")})");
            }
        }

        Assert.True(wrong.Count == 0, string.Join('\n', wrong));
    }

    // What Rust mode tells the core: the configured proxy and no variables; none with
    // DirectConnection; otherwise each variable the rule reads that is set, even empty, by its own
    // name. A proxy question (only a core that wasn't told asks) hears the configured proxy or none.
    // covers: RULE-PROXY-02
    [Fact]
    public void RustModeTellsTheCoreItsProxy()
    {
        Func<string, string?> env = n => n switch
        {
            "HTTP_PROXY" => "http://env:1", "no_proxy" => string.Empty, "REQUEST_METHOD" => "GET", "OTHER" => "x", _ => null,
        };
        var configured = new OpAmpClientOptions
        {
            Endpoint = new Uri("http://x/"), Mode = OpAmpMode.Rust, Proxy = new Uri("socks5://u:p%40ss@configured:1080"), ProxyUseLocalResolver = true,
        };
        Assert.Equal(new ProxyChoice { Url = "socks5://u:p%40ss@configured:1080", ResolveLocally = true }, OpAmpClient.RustProxyChoice(configured, env));
        Assert.Equal(("socks5://u:p%40ss@configured:1080", true), (OpAmpClient.ProxyAnswerFor(configured).Proxy, OpAmpClient.ProxyAnswerFor(configured).ResolveLocally));
        var direct = configured with { Proxy = null, DirectConnection = true };
        Assert.Equal(new ProxyChoice { Direct = true }, OpAmpClient.RustProxyChoice(direct, env));
        Assert.Equal(string.Empty, OpAmpClient.ProxyAnswerFor(direct).Proxy);
        var fromEnv = configured with { Proxy = null };
        var choice = OpAmpClient.RustProxyChoice(fromEnv, env);
        Assert.Equal(["REQUEST_METHOD=GET", "HTTP_PROXY=http://env:1", "no_proxy="], choice.Environment.Select(h => $"{h.Name}={h.Value}").ToArray());
        Assert.True(choice.ResolveLocally);
        Assert.Equal(string.Empty, OpAmpClient.ProxyAnswerFor(fromEnv).Proxy);

        // The core reads them by the rule: CGI, so none.
        Assert.Null(CoreRoute(choice, "http://example.com/"));
    }

    // Where Rust mode's core connects first for `url` when told `choice`: the proxy as the table
    // writes a route (scheme://host:port, credentials left out), or null for directly. The core
    // picks it itself: it asks no proxy question.
    private static string? CoreRoute(ProxyChoice choice, string url)
    {
        using var core = new CoreInstance(new Config
        {
            InstanceUid = ByteString.CopyFrom(new byte[16]), Endpoint = url, Streams = true, Entropy = ByteString.CopyFrom(new byte[32]), Proxy = choice,
        });
        var out1 = core.Step(1, 1, new Input { Start = new Empty() });
        Assert.DoesNotContain(out1.Effects, e => e.KindCase == Effect.KindOneofCase.ProxyQuery);
        var open = out1.Effects.Where(e => e.KindCase == Effect.KindOneofCase.StreamOpen).Select(e => e.StreamOpen).FirstOrDefault();
        Assert.True(open is not null, $"{url}: no stream opened");
        var u = new Uri(url);
        var secure = u.Scheme is "https" or "wss";
        var port = u.IsDefaultPort || u.Port < 0 ? (secure ? 443 : 80) : u.Port;
        var host = u.Host.Trim('[', ']');
        if (string.Equals(open.Host, host, StringComparison.OrdinalIgnoreCase) && open.Port == port)
        {
            return null;
        }

        // Which kind of proxy: SOCKS5 greets with version 5, an https:// one is TLS first.
        var next = core.Step(2, 2, new Input { StreamOpened = new Id { Id_ = open.Id } });
        var scheme = "http";
        foreach (var e in next.Effects)
        {
            if (e.KindCase == Effect.KindOneofCase.StreamWrite && e.StreamWrite.Data.Length > 0 && e.StreamWrite.Data[0] == 5)
            {
                scheme = "socks5";
            }

            if (e.KindCase == Effect.KindOneofCase.StreamStartTls)
            {
                scheme = "https";
            }
        }

        return $"{scheme}://{(open.Host.Contains(':') ? $"[{open.Host}]" : open.Host)}:{open.Port}";
    }

    // Each TLS setting, in Rust mode: the client starts with it (Rust mode's SslStream takes the
    // same settings), or refuses it at once, as the table says.
    [Fact]
    public async Task RustModeTakesOrRefusesEveryTlsSettingAsTheTableSays()
    {
        string? Read(JsonElement c, string name) => Str(c, name) is { } p ? File.ReadAllText(Path.Combine(Dir(), p)) : null;
        var wrong = new List<string>();
        foreach (var c in Table("tls.json").GetProperty("cases").EnumerateArray())
        {
            var id = Str(c, "id")!;
            string got;
            var detail = string.Empty;
            if (Version(Str(c, "min")) is not { } min || Version(Str(c, "max")) is not { } max)
            {
                got = "refused";
            }
            else
            {
                try
                {
                    var client = OpAmpClient.Start(
                        new OpAmpClientOptions
                        {
                            Endpoint = new Uri("https://127.0.0.1:1/v1/opamp"),
                            Mode = OpAmpMode.Rust,
                            DirectConnection = true,
                            StopTimeout = TimeSpan.FromSeconds(1),
                            Tls = new OpAmpTlsOptions { CaPem = Read(c, "ca"), ClientCertPem = Read(c, "cert"), ClientKeyPem = Read(c, "key"), MinVersion = min, MaxVersion = max },
                        },
                        new OpAmpStatus());
                    await client.StopAsync().WaitAsync(TimeSpan.FromSeconds(10));
                    got = "ok";
                }
                catch (ArgumentException e)
                {
                    got = "refused";
                    detail = $": {e.Message}";
                }
            }

            var table = Str(c, "expect")!;
            var want = TlsDifferences.TryGetValue(id, out var known) ? known.Got : table;
            if (got != want)
            {
                wrong.Add($"{id}: {got}{detail}, {(known.Why is null ? $"the table says {table} ({Str(c, "why")})" : $"the known difference says {want}")}");
            }
        }

        Assert.True(wrong.Count == 0, string.Join('\n', wrong));
    }

    /// <summary>A proxy URL as a route: scheme://host:port, credentials left out (no scheme: http).</summary>
    private static string Route(string proxy)
    {
        var u = new Uri(System.Text.RegularExpressions.Regex.IsMatch(proxy, "^[a-zA-Z][a-zA-Z0-9+.-]*://") ? proxy : "http://" + proxy);
        var scheme = u.Scheme;
        var port = u.Port > 0 ? u.Port : scheme == "https" ? 443 : scheme.StartsWith("socks", StringComparison.Ordinal) ? 1080 : 80;
        return $"{scheme}://{u.Host}:{port}";
    }

    // The proxy the environment names for each case, as the client's default (the platform's)
    // proxy picks it: in a process of its own with exactly the case's variables.
    [Fact]
    public async Task TheEnvironmentsProxyRoutesAsTheTableSays()
    {
        var cases = Table("proxy.json").GetProperty("cases").EnumerateArray().ToList();
        Assert.True(cases.Count > 30);
        using var slots = new SemaphoreSlim(6);
        var results = await Task.WhenAll(cases.Select(async c =>
        {
            await slots.WaitAsync();
            try
            {
                var env = c.GetProperty("env").EnumerateObject().ToDictionary(p => p.Name, p => p.Value.GetString()!);
                var (route, refused) = await ProxyProbe.RouteAsync(env, Str(c, "url")!);
                return (Case: c, Got: route, Refused: refused);
            }
            finally
            {
                slots.Release();
            }
        }));
        var wrong = new List<string>();
        foreach (var (c, got, refused) in results)
        {
            var id = Str(c, "id")!;
            if (refused is not null)
            {
                this.output.WriteLine($"{id}: refused at start: {refused}");
            }

            var table = Str(c, "proxy");
            if (ProxyDifferences.TryGetValue(id, out var known))
            {
                this.output.WriteLine($"{id}: {got ?? "direct"}, the table says {table ?? "direct"}: known difference: {known.Why}");
                if (got != known.Proxy)
                {
                    wrong.Add($"{id}: {got ?? "direct"}, no longer the known difference ({known.Proxy ?? "direct"}); the table says {table ?? "direct"}");
                }

                continue;
            }

            if (got != table)
            {
                wrong.Add($"{id}: {got ?? "direct"}, the table says {table ?? "direct"} ({Str(c, "why")})");
            }
        }

        Assert.True(wrong.Count == 0, string.Join('\n', wrong));
    }

    // Each TLS setting, as an application gives it: the client's transport is made, or refused at once.
    [Fact]
    public void EveryTlsSettingIsTakenOrRefusedAsTheTableSays()
    {
        string? Read(JsonElement c, string name) => Str(c, name) is { } p ? File.ReadAllText(Path.Combine(Dir(), p)) : null;
        var cases = Table("tls.json").GetProperty("cases").EnumerateArray().ToList();
        Assert.True(cases.Count > 20);
        var wrong = new List<string>();
        foreach (var c in cases)
        {
            var id = Str(c, "id")!;
            string got;
            string detail = string.Empty;
            if (Version(Str(c, "min")) is not { } min || Version(Str(c, "max")) is not { } max)
            {
                got = "refused"; // a version this client's API can't name
            }
            else
            {
                var tls = new OpAmpTlsOptions
                {
                    CaPem = Read(c, "ca"),
                    ClientCertPem = Read(c, "cert"),
                    ClientKeyPem = Read(c, "key"),
                    MinVersion = min,
                    MaxVersion = max,
                };
                try
                {
                    using var transport = ClientTransport.Create(new OpAmpClientOptions { Endpoint = new Uri("https://127.0.0.1:1/v1/opamp"), Tls = tls, DirectConnection = true });
                    got = "ok";
                }
                catch (ArgumentException e)
                {
                    got = "refused";
                    detail = $": {e.Message}";
                }
            }

            var table = Str(c, "expect")!;
            if (TlsDifferences.TryGetValue(id, out var known))
            {
                this.output.WriteLine($"{id}: {got}, the table says {table}: known difference: {known.Why}");
                if (got != known.Got)
                {
                    wrong.Add($"{id}: {got}{detail}, no longer the known difference ({known.Got}); the table says {table}");
                }
            }
            else if (got != table)
            {
                wrong.Add($"{id}: {got}{detail}, the table says {table} ({Str(c, "why")})");
            }
        }

        Assert.True(wrong.Count == 0, string.Join('\n', wrong));
    }

    /// <summary>A version as the API names it (None: the default), or null if it can't.</summary>
    private static SslProtocols? Version(string? v) => v switch
    {
        null => SslProtocols.None,
        "1.2" => SslProtocols.Tls12,
        "1.3" => SslProtocols.Tls13,
#pragma warning disable SYSLIB0039 // the old versions, named to be refused
        "1.1" => SslProtocols.Tls11,
        "1.0" => SslProtocols.Tls,
#pragma warning restore SYSLIB0039
        _ => null,
    };
}

/// <summary>
/// The proxy .NET's default proxy (what the client uses when given none) picks for a URL, in a
/// process of its own whose environment holds exactly a case's variables: the test process's
/// own environment is left alone, and .NET reads the environment's proxies once per process. The
/// test assembly runs itself with OPAMP_PROXY_PROBE set; this module initializer answers, and the
/// process exits before any test runs.
/// </summary>
internal static class ProxyProbe
{
    private const string Variable = "OPAMP_PROXY_PROBE";

    private static readonly string[] Names = ["HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "REQUEST_METHOD"];

    /// <summary>The route for <paramref name="url"/> under <paramref name="env"/>: scheme://host:port, or null for directly.</summary>
    public static async Task<string?> RunAsync(Dictionary<string, string> env, string url)
    {
        var self = Environment.ProcessPath!;
        var start = Path.GetFileNameWithoutExtension(self) == "dotnet"
            ? new ProcessStartInfo(self, [typeof(ProxyProbe).Assembly.Location])
            : new ProcessStartInfo(self);
        start.RedirectStandardOutput = true;
        start.RedirectStandardError = true;
        foreach (var name in start.Environment.Keys.Where(k => Names.Contains(k.ToUpperInvariant())).ToList())
        {
            start.Environment.Remove(name);
        }

        foreach (var (name, value) in env)
        {
            start.Environment[name] = value;
        }

        start.Environment[Variable] = url;
        using var p = Process.Start(start)!;
        var stderr = p.StandardError.ReadToEndAsync();
        var stdout = await p.StandardOutput.ReadToEndAsync();
        await p.WaitForExitAsync();
        if (p.ExitCode != 0 || !stdout.StartsWith("route ", StringComparison.Ordinal))
        {
            throw new InvalidOperationException($"the probe for {url} failed ({p.ExitCode}): {stdout}{await stderr}");
        }

        var route = stdout["route ".Length..].Trim();
        return route == "direct" ? null : route;
    }

    /// <summary>
    /// As <see cref="RunAsync"/>, but a configuration the client refuses at start (an exception from
    /// building its handler) is a result too: "refused", with its message.
    /// </summary>
    public static async Task<(string? Route, string? Refused)> RouteAsync(Dictionary<string, string> env, string url)
    {
        try
        {
            return (await RunAsync(env, url), null);
        }
        catch (InvalidOperationException e) when (e.Message.Contains(": error ", StringComparison.Ordinal))
        {
            return ("refused", e.Message[(e.Message.IndexOf(": error ", StringComparison.Ordinal) + 8)..]);
        }
    }

#pragma warning disable CA2255 // a module initializer in a test assembly, to answer as a child process
    [ModuleInitializer]
#pragma warning restore CA2255
    internal static void Answer()
    {
        if (Environment.GetEnvironmentVariable(Variable) is not { Length: > 0 } url)
        {
            return;
        }

        try
        {
            // The client's handler, as it is set up with no proxy configured: its own choice (direct,
            // under CGI), or else the platform's proxy, which reads the environment.
            using var handler = ClientTransport.CreateHandler(null, null);

            // A WebSocket upgrade is an http(s):// request to the handler: ws:// as http://, wss:// as https://.
            var uri = new Uri(url);
            uri = uri.Scheme switch
            {
                "ws" => new UriBuilder(uri) { Scheme = "http", Port = uri.Port }.Uri,
                "wss" => new UriBuilder(uri) { Scheme = "https", Port = uri.Port }.Uri,
                _ => uri,
            };
            var proxy = handler.Proxy ?? HttpClient.DefaultProxy;
            var route = !handler.UseProxy || proxy.IsBypassed(uri) || proxy.GetProxy(uri) is not { } via
                ? "direct"
                : $"{via.Scheme}://{via.Host}:{via.Port}";
            Console.Out.Write($"route {route}");
            Console.Out.Flush();
            Environment.Exit(0);
        }
        catch (Exception e)
        {
            Console.Out.Write($"error {e.Message}");
            Console.Out.Flush();
            Environment.Exit(3);
        }
    }
}
