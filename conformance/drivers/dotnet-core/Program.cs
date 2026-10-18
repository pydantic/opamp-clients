// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Runs our .NET client (dotnet/Pydantic.OpAmp.Client: the shared core in Wasmtime) under the
// conformance driver contract (conformance/DRIVERS.md). It runs in core mode (the core
// speaks HTTP, WebSocket, CONNECT and SOCKS5 itself, over .NET's sockets and SslStream), where
// DRIVER_PROXY may also be socks5://user:password@host:port; OPAMP_DOTNET_MODE=platform runs it in
// platform mode.

using System.Globalization;
using System.Text.Json;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Client;

var outLock = new object();

void Emit(string evt, Dictionary<string, object?>? fields = null)
{
    var line = new Dictionary<string, object?>(fields ?? new()) { ["event"] = evt, ["wall"] = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() / 1000.0 };
    lock (outLock)
    {
        Console.Out.WriteLine(JsonSerializer.Serialize(line));
        Console.Out.Flush();
    }
}

double EnvDouble(string key, double def) =>
    double.TryParse(Environment.GetEnvironmentVariable(key), NumberStyles.Float, CultureInfo.InvariantCulture, out var v) ? v : def;

// All zeros (zero), the 32 hex characters given, or none (random, none): the core makes one, a UUIDv7.
byte[]? InstanceUid() => Environment.GetEnvironmentVariable("DRIVER_UID") switch
{
    "zero" => new byte[16],
    null or "" or "random" or "none" => null,
    var hex => Convert.FromHexString(hex),
};

KeyValue Kv(string k, string v) => new() { Key = k, Value = new AnyValue { StringValue = v } };

AgentDescription Description(int change)
{
    var d = new AgentDescription();
    d.IdentifyingAttributes.Add(Kv("service.name", "conformance-dotnet-core"));
    d.NonIdentifyingAttributes.Add(Kv("driver.change", change.ToString(CultureInfo.InvariantCulture)));
    // DRIVER_DESCRIPTION_PAD: a description that long (with DRIVER_MAX_SEND_BYTES, too large to send).
    if (int.TryParse(Environment.GetEnvironmentVariable("DRIVER_DESCRIPTION_PAD"), NumberStyles.Integer, CultureInfo.InvariantCulture, out var pad))
    {
        d.NonIdentifyingAttributes.Add(Kv("driver.pad", new string('x', pad)));
    }
    return d;
}

// The custom capability DRIVERS.md names: ping is answered with pong.
const string Echo = "io.opentelemetry.echo";

var platform = Environment.GetEnvironmentVariable("OPAMP_DOTNET_MODE") == "platform";
OpAmpClient? client = null;
// The status to start from, a restart after a crash going on from where the application was
// (DRIVER_FIRST_CHANGE): the changes count on from it.
var firstChange = int.TryParse(Environment.GetEnvironmentVariable("DRIVER_FIRST_CHANGE"), NumberStyles.Integer,
    CultureInfo.InvariantCulture, out var fc) ? fc : 0;
var status = new OpAmpStatus
{
    Description = Description(firstChange),
    Health = new ComponentHealth { Healthy = true, StartTimeUnixNano = (ulong)DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() * 1_000_000UL },
    EffectiveConfig = new EffectiveConfig { ConfigMap = new AgentConfigMap() },
    AvailableComponents = Environment.GetEnvironmentVariable("DRIVER_COMPONENTS") == "1" ? Components() : null,
    CustomCapabilities = Environment.GetEnvironmentVariable("DRIVER_CUSTOM") == "1"
        ? new CustomCapabilities { Capabilities = { Echo } }
        : null,
};
var accept = Environment.GetEnvironmentVariable("DRIVER_ACCEPT_CONNECTION_SETTINGS") == "1";
// DRIVER_THROW_ON_CONFIG: the callback itself throws on the first remote config (RULE-SHELL-02).
var throwOnConfig = Environment.GetEnvironmentVariable("DRIVER_THROW_ON_CONFIG") == "1";
var thrown = 0;
// The client stopped on its own: the driver no longer stops it, and exits 1.
var failed = 0;

// A PEM file the runner names (under /out, in the container), if it names one.
string? Pem(string key) => Environment.GetEnvironmentVariable(key) is { Length: > 0 } path ? File.ReadAllText(path) : null;

try
{
    var ca = Pem("DRIVER_CA_FILE");
    var clientCert = Pem("DRIVER_CLIENT_CERT");
    var clientKey = Pem("DRIVER_CLIENT_KEY");
    client = OpAmpClient.Start(
        new OpAmpClientOptions
        {
            Endpoint = new Uri(Environment.GetEnvironmentVariable("OPAMP_ENDPOINT") ?? string.Empty),
            InstanceUid = InstanceUid(),
            Capabilities = (ulong)AgentCapabilities.AcceptsRemoteConfig
                | (Environment.GetEnvironmentVariable("DRIVER_OWN_TELEMETRY") == "1"
                    ? (ulong)(AgentCapabilities.ReportsOwnMetrics | AgentCapabilities.ReportsOwnTraces | AgentCapabilities.ReportsOwnLogs)
                    : 0UL),
            HeartbeatInterval = TimeSpan.FromSeconds(EnvDouble("DRIVER_POLL_S", 2)),
            MaxSendBytes = long.TryParse(Environment.GetEnvironmentVariable("DRIVER_MAX_SEND_BYTES"), NumberStyles.Integer, CultureInfo.InvariantCulture, out var maxSend) ? maxSend : 0,
            Headers = Environment.GetEnvironmentVariable("DRIVER_TOKEN") is { Length: > 0 } token
                ? new Dictionary<string, string> { ["Authorization"] = "Bearer " + token }
                : new Dictionary<string, string>(),
            AcceptConnectionSettings = accept,
            AcceptPackages = Environment.GetEnvironmentVariable("DRIVER_PACKAGES") == "1",
            RequestInstanceUid = Environment.GetEnvironmentVariable("DRIVER_REQUEST_UID") == "1",
            StateFile = Environment.GetEnvironmentVariable("DRIVER_STATE_FILE") is { Length: > 0 } state ? state : null,
            // For messages and package downloads alike: one handler carries both.
            Tls = ca is null && clientCert is null
                ? null
                : new OpAmpTlsOptions { CaPem = ca, ClientCertPem = clientCert, ClientKeyPem = clientKey },
            Proxy = Environment.GetEnvironmentVariable("DRIVER_PROXY") is { Length: > 0 } proxy ? new Uri(proxy) : null,
            // Always asked for by name, never left to the client's default: dotnet-core is core
            // mode, dotnet-core-platform (OPAMP_DOTNET_MODE=platform) platform mode.
            Mode = platform ? OpAmpMode.Platform : OpAmpMode.Core,
        },
        status,
        // Answers go through the client the callback is given: a remote config saved by an earlier
        // run is handed over as the client starts.
        (handle, n) =>
        {
            switch (n)
            {
                case OpAmpNotification.Stopped { Error: { } error }:
                    // The client stopped on its own. Stay DRIVER_LINGER_S, still reporting what the
                    // client says (nothing, if it keeps RULE-SHELL-02: a second stopped is a second
                    // fatal), then exit 1.
                    Emit("fatal", new() { ["error"] = error.ToString() });
                    if (Interlocked.Exchange(ref failed, 1) == 0)
                    {
                        _ = Task.Run(async () =>
                        {
                            await Task.Delay(TimeSpan.FromSeconds(EnvDouble("DRIVER_LINGER_S", 0)));
                            Environment.Exit(1);
                        });
                    }

                    break;
                case OpAmpNotification.RemoteConfig when throwOnConfig && Interlocked.Exchange(ref thrown, 1) == 0:
                    Emit("throwing");
                    throw new InvalidOperationException("callback-throws: the driver's callback threw on the first remote config");
                case OpAmpNotification.Connected:
                    Emit("connect");
                    break;
                case OpAmpNotification.ConnectFailed f:
                    Emit("connect_failed", new() { ["error"] = f.Detail, ["status"] = f.Status });
                    break;
                case OpAmpNotification.ErrorResponse e:
                    Emit("error_response", new() { ["type"] = e.Type, ["message"] = e.Message });
                    break;
                case OpAmpNotification.RemoteConfig r:
                    Emit("message", new() { ["remote_config_hash"] = Convert.ToHexString(r.Config.ConfigHash.ToByteArray()).ToLowerInvariant() });
                    handle.SetRemoteConfigStatus(new RemoteConfigStatus
                    {
                        LastRemoteConfigHash = r.Config.ConfigHash,
                        Status = RemoteConfigStatuses.Applied,
                    });
                    break;
                case OpAmpNotification.CustomMessage { Message: var m }:
                    Emit("custom_message", new()
                    {
                        ["capability"] = m.Capability,
                        ["type"] = m.Type,
                        ["data"] = Convert.ToHexString(m.Data.ToByteArray()).ToLowerInvariant(),
                    });
                    if (m.Type == "ping")
                    {
                        handle.SendCustomMessage(new CustomMessage { Capability = Echo, Type = "pong", Data = m.Data });
                    }

                    break;
                case OpAmpNotification.CustomMessageNotSent ns:
                    Emit("custom_message_not_sent", new() { ["reason"] = ns.Reason });
                    break;
                case OpAmpNotification.InstallPackage ip:
                {
                    string? sha = null;
                    string? error = null;
                    try
                    {
                        sha = Convert.ToHexString(System.Security.Cryptography.SHA256.HashData(File.ReadAllBytes(ip.Path))).ToLowerInvariant();
                    }
                    catch (IOException e)
                    {
                        error = e.Message;
                    }

                    Emit("install_package", new() { ["name"] = ip.Name, ["version"] = ip.Package.Version, ["sha256"] = sha, ["error"] = error });
                    handle.PackageInstalled(ip.Name, ip.Package.Hash.ToByteArray(), error);
                    break;
                }

                case OpAmpNotification.RemovePackage rp:
                    Emit("remove_package", new() { ["name"] = rp.Name });
                    handle.PackageRemoved(rp.Name);
                    break;
                case OpAmpNotification.ServerCustomCapabilities sc:
                    Emit("server_custom_capabilities", new() { ["capabilities"] = sc.Capabilities });
                    break;
                case OpAmpNotification.ConnectionSettings s:
                    Emit("telemetry_settings", new()
                    {
                        ["hash"] = Convert.ToHexString(s.Settings.Hash.ToByteArray()).ToLowerInvariant(),
                        ["metrics_endpoint"] = s.Settings.OwnMetrics?.DestinationEndpoint,
                    });
                    handle.SetConnectionSettingsStatus(new ConnectionSettingsStatus
                    {
                        LastConnectionSettingsHash = s.Settings.Hash,
                        Status = ConnectionSettingsStatuses.Applied,
                    });
                    break;
                case OpAmpNotification.InstanceUidChanged u:
                    Emit("message", new() { ["new_instance_uid"] = Convert.ToHexString(u.InstanceUid).ToLowerInvariant() });
                    break;
                case OpAmpNotification.HeartbeatInterval h:
                    Emit("connection_settings", new() { ["heartbeat_interval_seconds"] = (long)h.Interval.TotalSeconds });
                    break;
            }
        });
}
catch (Exception e)
{
    Emit("fatal", new() { ["error"] = e.ToString() });
    return 1;
}

// The UID in use: the one given, a saved one, or the one the core made.
Emit("started", new() { ["uid"] = Convert.ToHexString(client.InstanceUid).ToLowerInvariant(), ["client"] = platform ? "opamp-dotnet-core (ours, platform mode)" : "opamp-dotnet-core (ours)" });

var changes = (Environment.GetEnvironmentVariable("DRIVER_CHANGES_AT_S") ?? string.Empty)
    .Split(',', StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries)
    .Select(s => double.TryParse(s, NumberStyles.Float, CultureInfo.InvariantCulture, out var v) ? v : double.NaN)
    .Where(v => !double.IsNaN(v))
    .ToList();
// Not disposed: the exit handler below may still fire after the driver is done.
var stop = new CancellationTokenSource(TimeSpan.FromSeconds(EnvDouble("DRIVER_RUN_S", 30)));
Console.CancelKeyPress += (_, e) =>
{
    e.Cancel = true;
    stop.Cancel();
};
AppDomain.CurrentDomain.ProcessExit += (_, _) => stop.Cancel();
var started = DateTime.UtcNow;
// One after another, in one loop: with a timer each, two that come due together (the process was
// frozen) would race, and the application could end on an older change than its last.
var changing = Task.Run(async () =>
{
    for (var i = 0; i < changes.Count; i++)
    {
        var wait = started.AddSeconds(changes[i]) - DateTime.UtcNow;
        try
        {
            await Task.Delay(wait > TimeSpan.Zero ? wait : TimeSpan.Zero, stop.Token);
        }
        catch (OperationCanceledException)
        {
            return;
        }
        Emit("status_change", new() { ["change"] = firstChange + i + 1 });
        client.SetDescription(Description(firstChange + i + 1));
    }
});

try
{
    await Task.Delay(Timeout.Infinite, stop.Token);
}
catch (OperationCanceledException)
{
}
await changing;

if (Volatile.Read(ref failed) == 1)
{
    await Task.Delay(Timeout.Infinite); // exits 1 when DRIVER_LINGER_S is over
}

Emit("stopping");
string? error = null;
try
{
    await client.StopAsync(); // bounded by the core (OpAmpClientOptions.StopTimeout)
}
catch (Exception e)
{
    error = e.ToString();
}

Emit("stopped", new() { ["error"] = error });
// Stay alive a while, doing nothing, so the server can see anything the client left running.
if (EnvDouble("DRIVER_LINGER_S", 0) is var linger and > 0)
{
    await Task.Delay(TimeSpan.FromSeconds(linger));
}

return 0;

// The components DRIVERS.md names: an OTLP receiver (with its version) and a debug exporter.
static AvailableComponents Components()
{
    var otlp = new ComponentDetails();
    otlp.Metadata.Add(new KeyValue { Key = "version", Value = new AnyValue { StringValue = "1.0.0" } });
    var receivers = new ComponentDetails();
    receivers.SubComponentMap.Add("otlp", otlp);
    var exporters = new ComponentDetails();
    exporters.SubComponentMap.Add("debug", new ComponentDetails());
    var components = new AvailableComponents();
    components.Components.Add("receivers", receivers);
    components.Components.Add("exporters", exporters);
    return components;
}
