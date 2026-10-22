// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Text;
using System.Threading.Channels;
using Google.Protobuf;
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Core;
using Header = Pydantic.OpAmp.Core.Header;

namespace Pydantic.OpAmp.Client;

/// <summary>The client's settings.</summary>
public sealed record OpAmpClientOptions
{
    /// <summary>The server URL: http(s)://... polls, ws(s)://... keeps one connection open.</summary>
    public required Uri Endpoint { get; init; }

    /// <summary>
    /// The instance UID to start with (16 bytes). The server may assign another. None: the core
    /// makes one (a UUIDv7, from the clock and the operating system's randomness), and with a
    /// <see cref="StateFile"/> keeps it across restarts; <see cref="OpAmpClient.InstanceUid"/> says
    /// which is in use.
    /// </summary>
    public byte[]? InstanceUid { get; init; }

    /// <summary>
    /// Capabilities the agent supports (AgentCapabilities bits), fixed for the run. ReportsStatus is
    /// always added, and the core adds the bits of what it does itself (packages, connection
    /// settings). Declare ReportsHeartbeat for heartbeats, and the Reports* bit of each status you
    /// set: a status whose bit isn't declared isn't sent.
    /// </summary>
    public ulong Capabilities { get; init; }

    /// <summary>Heartbeat (WebSocket) or polling (HTTP) interval until the server offers one; default 30 s.</summary>
    public TimeSpan HeartbeatInterval { get; init; }

    /// <summary>
    /// Headers for every request or upgrade (Authorization, say). One that can't be sent as given (a
    /// name that isn't a token, a control character in a value or whitespace at its ends, one of
    /// the headers the HTTP stack writes) fails each request, with why, in both modes.
    /// </summary>
    public IReadOnlyDictionary<string, string> Headers { get; init; } = new Dictionary<string, string>();

    /// <summary>
    /// How long a plain HTTP request may take; default 30 s. The core keeps every effect's time
    /// (and that of stop, 5 s): the client has no timeouts of its own.
    /// </summary>
    public TimeSpan RequestTimeout { get; init; }

    /// <summary>How long a WebSocket connection may take to open; default 30 s. The core keeps it.</summary>
    public TimeSpan ConnectTimeout { get; init; }

    /// <summary>
    /// How long stop may take (the goodbye, and the closing handshake), and any closing handshake;
    /// default 5 s. The core keeps it: <see cref="OpAmpClient.StopAsync"/> ends within it.
    /// </summary>
    public TimeSpan StopTimeout { get; init; }

    /// <summary>
    /// The largest message sent, encoded; default 64 MiB, and at least 1 KiB. A status that doesn't
    /// fit is left out (<see cref="OpAmpNotification.StatusTooLarge"/>).
    /// </summary>
    // covers: OPAMP-SIZE-02, OPAMP-SIZE-08
    public long MaxSendBytes { get; init; }

    /// <summary>The largest response or message accepted; default 64 MiB.</summary>
    // covers: OPAMP-SIZE-02, OPAMP-SIZE-08
    public long MaxReceiveBytes { get; init; }

    /// <summary>
    /// TLS for https:// and wss:// endpoints and package downloads: the CAs to trust, a client
    /// certificate, the TLS versions. Default: the system's CAs, TLS 1.2 or 1.3, no client certificate.
    /// </summary>
    public OpAmpTlsOptions? Tls { get; init; }

    /// <summary>
    /// A proxy for every connection (requests, the WebSocket, package downloads):
    /// <c>http://</c>, <c>https://</c>, <c>socks5://</c> or <c>socks5h://</c>, with credentials as
    /// <c>user:password@</c>. With <c>socks5://</c> and <c>socks5h://</c> alike the proxy resolves
    /// the server's host name (unlike curl, where <c>socks5://</c> resolves it locally), unless
    /// <see cref="ProxyResolveLocally"/>. <c>socks4://</c>, <c>socks4a://</c> and any other
    /// scheme are errors. Default: the proxy the environment names (<c>HTTP_PROXY</c>,
    /// <c>HTTPS_PROXY</c>, <c>ALL_PROXY</c>, <c>NO_PROXY</c>), by the rule every SDK's core mode
    /// shares (tests/shell-tables/README.md, its proxy rule); none under CGI
    /// (<c>REQUEST_METHOD</c> set: httpoxy). In <see cref="OpAmpMode.Platform"/> the default is the
    /// system's proxy settings instead (<see cref="System.Net.Http.HttpClient.DefaultProxy"/>: the
    /// environment's, or Windows' and macOS's own), held to the same schemes where the client can
    /// see them; under CGI a direct connection, unless the application set
    /// <see cref="System.Net.Http.HttpClient.DefaultProxy"/> itself.
    /// </summary>
    // covers: RULE-PROXY-01
    public Uri? Proxy { get; init; }

    /// <summary>
    /// Resolve the server's host name on the client and give a SOCKS5 proxy an address, instead of
    /// the proxy resolving it (a SOCKS5 proxy the environment names too). Default false. With an
    /// HTTP proxy it is an error: an HTTP proxy is always given the name. It is core mode's: in
    /// <see cref="OpAmpMode.Platform"/> it is an error (.NET's SOCKS5 always sends the name).
    /// </summary>
    public bool ProxyResolveLocally { get; init; }

    /// <summary>
    /// Connect directly, with no proxy, not even the environment's (or, in
    /// <see cref="OpAmpMode.Platform"/>, the system's). Not with <see cref="Proxy"/>.
    /// </summary>
    public bool DirectConnection { get; init; }

    /// <summary>
    /// Your own HttpClient, instead of <see cref="Tls"/>, <see cref="Proxy"/> and
    /// <see cref="DirectConnection"/> (setting any of them with it is an error). Setting it selects
    /// <see cref="OpAmpMode.Platform"/>: only .NET's stack can use it (with <see cref="Mode"/> set to
    /// <see cref="OpAmpMode.Core"/> it is an error). It carries everything: requests, the WebSocket
    /// upgrade and package downloads. Build it on a handler that doesn't follow redirects or
    /// decompress (<c>AllowAutoRedirect = false</c>, the default decompression): a 3xx goes to the
    /// core as it is, which decides what it means, and the core asks for no compression. The
    /// client doesn't dispose it.
    /// </summary>
    public HttpClient? HttpClient { get; init; }

    /// <summary>
    /// Where the client keeps state across restarts; default: nowhere. It holds secrets
    /// (credentials the server issued, private keys of telemetry settings' certificates), so it is
    /// written for the agent's user only: mode 0600 on Unix, an access-control list allowing that
    /// user alone (nothing inherited) on Windows. Keep it on a local path, not a shared one.
    /// </summary>
    public string? StateFile { get; init; }

    /// <summary>
    /// Take offers of new connection settings from the server (docs/connection-settings.md).
    /// Offers are refused without a <see cref="StateFile"/>: a restart would lose them.
    /// </summary>
    public bool AcceptConnectionSettings { get; init; }

    /// <summary>How long offered settings have to prove themselves; default 60 s.</summary>
    public TimeSpan VerifyTimeout { get; init; }

    /// <summary>
    /// Take packages from the server (docs/packages.md): the client downloads them beside the
    /// <see cref="StateFile"/> (its path and <c>.downloads</c>) and the application installs them
    /// (<see cref="OpAmpNotification.InstallPackage"/>, <see cref="OpAmpNotification.RemovePackage"/>).
    /// Only with a <see cref="StateFile"/>: a restart would forget what the agent has.
    /// </summary>
    public bool AcceptPackages { get; init; }

    /// <summary>
    /// Ask the server to assign the instance UID (the RequestInstanceUid flag,
    /// docs/instance-uid.md). The UID the client starts with (made up, or <see cref="InstanceUid"/>)
    /// is the temporary value until the server assigns one. With a <see cref="StateFile"/> the
    /// assigned UID is kept, and a restart doesn't ask again; without one every start asks again,
    /// and the server assigns a new UID each time: the agent is a new agent after every restart.
    /// covers: OPAMP-UID-04
    /// </summary>
    public bool RequestInstanceUid { get; init; }

    /// <summary>The largest package file downloaded; default 1 GiB.</summary>
    public ulong MaxPackageBytes { get; init; }

    /// <summary>
    /// How long a download may take; default 10 minutes. The core keeps this time and cancels a
    /// download that passes it.
    /// </summary>
    public TimeSpan DownloadTimeout { get; init; }

    /// <summary>
    /// The clock and timers the client keeps its time with (the core's timer, the core's clock,
    /// request deadlines, the bound on stop); default: the system's (<see cref="System.TimeProvider.System"/>).
    /// Tests give a fake one (FakeTimeProvider) to move time themselves.
    /// </summary>
    public TimeProvider? TimeProvider { get; init; }

    /// <summary>
    /// Who speaks HTTP and WebSocket. <see cref="OpAmpMode.Core"/>: the shared core itself
    /// (HTTP/1.1, WebSocket, CONNECT and SOCKS5), over the client's own TCP connections and
    /// SslStream with <see cref="Tls"/>; .NET only opens connections, starts TLS, answers which
    /// proxy to use and writes package files. <see cref="OpAmpMode.Platform"/>: .NET's HttpClient
    /// and ClientWebSocket. Default (null): core mode, unless <see cref="HttpClient"/> is set,
    /// which only platform mode can use. An option only one mode honours is never ignored: it
    /// selects that mode, or <c>Start</c> throws with what to set instead.
    /// </summary>
    public OpAmpMode? Mode { get; init; }

    /// <summary>
    /// Core mode: package files go from the socket to the file without passing through the core,
    /// where the response says how long the body is. Default true. It is core mode's: false in
    /// <see cref="OpAmpMode.Platform"/> is an error.
    /// </summary>
    public bool SpliceDownloads { get; init; } = true;

    /// <summary>
    /// What ToString prints: every member, but not the headers' values or a password in
    /// <see cref="Endpoint"/> or <see cref="Proxy"/> (and <see cref="Tls"/> prints no key).
    /// </summary>
    private bool PrintMembers(StringBuilder builder) => Redacted.Print(
        builder,
        (nameof(this.Endpoint), Redacted.Url(this.Endpoint)),
        (nameof(this.InstanceUid), this.InstanceUid),
        (nameof(this.Capabilities), this.Capabilities),
        (nameof(this.HeartbeatInterval), this.HeartbeatInterval),
        (nameof(this.Headers), Redacted.Headers(this.Headers)),
        (nameof(this.RequestTimeout), this.RequestTimeout),
        (nameof(this.ConnectTimeout), this.ConnectTimeout),
        (nameof(this.StopTimeout), this.StopTimeout),
        (nameof(this.MaxSendBytes), this.MaxSendBytes),
        (nameof(this.MaxReceiveBytes), this.MaxReceiveBytes),
        (nameof(this.Tls), this.Tls),
        (nameof(this.Proxy), Redacted.Url(this.Proxy)),
        (nameof(this.ProxyResolveLocally), this.ProxyResolveLocally),
        (nameof(this.DirectConnection), this.DirectConnection),
        (nameof(this.HttpClient), this.HttpClient),
        (nameof(this.StateFile), this.StateFile),
        (nameof(this.AcceptConnectionSettings), this.AcceptConnectionSettings),
        (nameof(this.VerifyTimeout), this.VerifyTimeout),
        (nameof(this.AcceptPackages), this.AcceptPackages),
        (nameof(this.RequestInstanceUid), this.RequestInstanceUid),
        (nameof(this.MaxPackageBytes), this.MaxPackageBytes),
        (nameof(this.DownloadTimeout), this.DownloadTimeout),
        (nameof(this.TimeProvider), this.TimeProvider),
        (nameof(this.Mode), this.Mode),
        (nameof(this.SpliceDownloads), this.SpliceDownloads));
}

/// <summary>Who speaks HTTP and WebSocket: <see cref="OpAmpClientOptions.Mode"/>.</summary>
public enum OpAmpMode
{
    /// <summary>The shared core itself, over .NET's sockets and SslStream (the default).</summary>
    Core,

    /// <summary>
    /// .NET's HttpClient and ClientWebSocket: for an application that brings its own HttpClient
    /// (<see cref="OpAmpClientOptions.HttpClient"/>, which selects it too) or needs .NET's own
    /// stack (the system's proxy settings).
    /// </summary>
    Platform,
}

/// <summary>The agent's status as the client starts. The first message reports it in full.</summary>
public sealed record OpAmpStatus
{
    /// <summary>The agent description.</summary>
    public AgentDescription? Description { get; init; }

    /// <summary>The health.</summary>
    public ComponentHealth? Health { get; init; }

    /// <summary>The effective config.</summary>
    public EffectiveConfig? EffectiveConfig { get; init; }

    /// <summary>The remote config status.</summary>
    public RemoteConfigStatus? RemoteConfigStatus { get; init; }

    /// <summary>
    /// The components the agent has (leave the hash empty: the core computes it). The server gets
    /// the hash, and the components in full when it asks.
    /// </summary>
    public AvailableComponents? AvailableComponents { get; init; }

    /// <summary>
    /// The custom capabilities the agent supports: custom messages for them reach the application,
    /// and it may send them.
    /// </summary>
    public CustomCapabilities? CustomCapabilities { get; init; }
}

/// <summary>What the application is told.</summary>
public abstract record OpAmpNotification
{
    private OpAmpNotification()
    {
    }

    /// <summary>Connected (WebSocket), or an HTTP request succeeded: the first, or the first after failures.</summary>
    public sealed record Connected : OpAmpNotification;

    /// <summary>Connecting or a request failed; <c>Status</c> is the HTTP status, if there was one.</summary>
    public sealed record ConnectFailed(int? Status, string Detail) : OpAmpNotification;

    /// <summary>The server sent an error_response (<c>Type</c> is a ServerErrorResponseType value).</summary>
    public sealed record ErrorResponse(int Type, string Message) : OpAmpNotification;

    /// <summary>A remote config to apply; answer with <see cref="OpAmpClient.SetRemoteConfigStatus"/>.</summary>
    public sealed record RemoteConfig(AgentRemoteConfig Config) : OpAmpNotification;

    /// <summary>
    /// Own telemetry and other connection settings, all of them; answer with
    /// <see cref="OpAmpClient.SetConnectionSettingsStatus"/>.
    /// </summary>
    public sealed record ConnectionSettings(ConnectionSettingsOffers Settings) : OpAmpNotification
    {
        /// <summary>What ToString prints: the offers, but no header's value, private key or URL's password.</summary>
        protected override bool PrintMembers(StringBuilder builder) =>
            Redacted.Print(builder, (nameof(this.Settings), Redacted.Message(this.Settings)));
    }

    /// <summary>A custom message from the server, for a custom capability the agent declared.</summary>
    public sealed record CustomMessage(global::OpAmp.Proto.V1.CustomMessage Message) : OpAmpNotification;

    /// <summary>The custom capabilities the server supports changed.</summary>
    public sealed record ServerCustomCapabilities(IReadOnlyList<string> Capabilities) : OpAmpNotification;

    /// <summary>A custom message the application sent won't be, and why.</summary>
    public sealed record CustomMessageNotSent(global::OpAmp.Proto.V1.CustomMessage Message, string Reason) : OpAmpNotification;

    /// <summary>A command from the server.</summary>
    public sealed record Command(ServerToAgentCommand Value) : OpAmpNotification;

    /// <summary>The server assigned a new instance UID; persist it if you persist the UID.</summary>
    public sealed record InstanceUidChanged(byte[] InstanceUid) : OpAmpNotification;

    /// <summary>The heartbeat or polling interval changed (zero: heartbeats off).</summary>
    public sealed record HeartbeatInterval(TimeSpan Interval) : OpAmpNotification;

    /// <summary>
    /// Install package <paramref name="Name"/> (<paramref name="Package"/>: version, hash, type, the
    /// file's signature) from the file at <paramref name="Path"/>; checking the signature is the
    /// application's job. Answer with <see cref="OpAmpClient.PackageInstalled"/>; the client
    /// deletes the file after the answer.
    /// </summary>
    public sealed record InstallPackage(string Name, PackageAvailable Package, string Path) : OpAmpNotification
    {
        /// <summary>What ToString prints: every member, but the download's headers without their values and its URL without a password.</summary>
        protected override bool PrintMembers(StringBuilder builder) => Redacted.Print(
            builder,
            (nameof(this.Name), this.Name),
            (nameof(this.Package), Redacted.Message(this.Package)),
            (nameof(this.Path), this.Path));
    }

    /// <summary>
    /// Remove package <paramref name="Name"/>, no longer offered; answer with
    /// <see cref="OpAmpClient.PackageRemoved"/>.
    /// </summary>
    public sealed record RemovePackage(string Name) : OpAmpNotification;

    /// <summary>
    /// A status field was left out of a message because it was too large: <paramref name="Field"/>
    /// is its name in AgentToServer ("health", say), as the core gives it.
    /// </summary>
    public sealed record StatusTooLarge(string Field, long Bytes) : OpAmpNotification;

    /// <summary>The server sent something that isn't a ServerToAgent message.</summary>
    public sealed record BadMessage(string Detail) : OpAmpNotification;

    /// <summary>
    /// The save under way has run for its stall time without ending (<paramref name="Stalled"/>),
    /// and the client waits on it still, saving nothing else meanwhile; or it has ended at last (not
    /// <paramref name="Stalled"/>). <paramref name="Waited"/>: how long it had run. A disk that hangs
    /// is the application's to see to.
    /// </summary>
    public sealed record SaveStalled(bool Stalled, TimeSpan Waited) : OpAmpNotification;

    /// <summary>
    /// The client has stopped: after <see cref="OpAmpClient.StopAsync"/>, or on its own because of
    /// <paramref name="Error"/> (which <c>StopAsync</c> throws too). Null after <c>StopAsync</c>.
    /// </summary>
    public sealed record Stopped(Exception? Error = null) : OpAmpNotification;
}

/// <summary>
/// An OpAMP client over plain HTTP or WebSocket.
/// </summary>
/// <remarks>
/// Everything OpAMP (what to send and when, backoff, heartbeats, full reports, instance UIDs,
/// agent_disconnect, and the requests and responses themselves: headers, framing, Retry-After, size
/// limits) is the shared core, run natively (a native library). By default
/// (<see cref="OpAmpMode.Core"/>) the core speaks HTTP and WebSocket itself too, over .NET's
/// sockets and SslStream; in <see cref="OpAmpMode.Platform"/> this class carries out the core's
/// requests with HttpClient and ClientWebSocket instead. Either way it feeds back what happened, so
/// it behaves exactly like the other languages' clients on the same core. Everything runs on one loop
/// of its own; the methods may be called from any thread, the callback's included. What the callback
/// asks before it returns (its answer to a remote config, say) reaches the core before anything the
/// client takes in next: what the server says, a connection that opened, a timer. It keeps no time
/// of its own: each effect ends with its result or the core's cancel, and stop ends when the core
/// says it has stopped.
/// </remarks>
public sealed class OpAmpClient
{
    // Once the core has stopped, at least this for the work it cancelled to unwind.
    private static readonly TimeSpan StopGrace = TimeSpan.FromMilliseconds(250);

    // stopCalled before StopAsync.
    private const long NotStopped = long.MinValue;

    // The longest interval a TimeSpan holds, in milliseconds: a longer one is told as that.
    private static readonly ulong MaxSpanMs = (ulong)(long.MaxValue / TimeSpan.TicksPerMillisecond);

    private readonly CoreInstance core;
    private readonly IClientTransport transport;
    private readonly Executor executor;
    private readonly Streams? streams;
    private readonly Action<OpAmpNotification> notify;
    private readonly Channel<Queued> inputs = Channel.CreateUnbounded<Queued>(new UnboundedChannelOptions { SingleReader = true });

    // The application's calls, in the order made, until the loop feeds them: each in its turn in
    // the inputs' queue, or sooner if a callback asked for it or for one behind it. Guarded by itself.
    private readonly Queue<Input> calls = new();
    private readonly TaskCompletionSource started = new(TaskCreationOptions.RunContinuationsAsynchronously);
    private readonly TimeSpan stopTimeout;
    private readonly TimeProvider time;
    private readonly long started0;
    private readonly Action<Queued, Output>? stepped;
    private readonly TaskCompletionSource done = new(TaskCreationOptions.RunContinuationsAsynchronously);
    private CancellationTokenSource? timer;
    private int queued;
    private long stopCalled = NotStopped;
    private byte[] instanceUid;

    private OpAmpClient(OpAmpClientOptions options, Action<OpAmpNotification>? notify, Seams? seams)
    {
        this.time = options.TimeProvider ?? TimeProvider.System;
        this.started0 = this.time.GetTimestamp();
        this.stepped = seams?.Stepped;
        var files = seams?.Files ?? FileOps.Real;
        var coreMode = options.Mode switch
        {
            // None named: core mode, unless the application brought its own HttpClient, which only
            // platform mode can use.
            null => options.HttpClient is null,
            OpAmpMode.Platform => false,
            OpAmpMode.Core => true,
            _ => throw new ArgumentException($"Mode is Core or Platform, not {options.Mode}", nameof(options)),
        };
        if (!coreMode && !options.SpliceDownloads)
        {
            // Nothing one mode alone honours is ignored.
            throw new ArgumentException("SpliceDownloads is core mode's (HttpClient writes package files itself): leave it true, or run in core mode (no HttpClient, and Mode unset or Core)", nameof(options));
        }

        // First: bad TLS or proxy settings throw here. In core mode the platform's transport
        // carries only what a core that isn't the real one hands over (the real core asks for
        // streams).
        this.transport = seams?.Transport ?? (coreMode
            ? ClientTransport.Create(CheckCore(options).Tls, options.Proxy, direct: options.Proxy is null)
            : ClientTransport.Create(options));
        // The transport is the endpoint's scheme, as the core reads it: whichever effects come
        // (requests or a connection) are carried out.
        var config = new Config
        {
            // None of the application's: the core makes one, from these.
            InstanceUid = options.InstanceUid is { } uid ? ByteString.CopyFrom(uid) : ByteString.Empty,
            Entropy = ByteString.CopyFrom(System.Security.Cryptography.RandomNumberGenerator.GetBytes(32)),
            UnixMs = (ulong)this.time.GetUtcNow().ToUnixTimeMilliseconds(),
            Capabilities = options.Capabilities,
            HeartbeatIntervalMs = Ms(options.HeartbeatInterval),
            // No Seed: the core makes the backoff's jitter seed from the entropy, so two clients
            // started in the same tick don't retry in step.
            // As the application wrote it: Uri.ToString() would unescape it (%41 sent as A, %20 as
            // a space the core refuses), so the server would get another target than from every
            // other client (tests/shell-tables/urls.json).
            Endpoint = options.Endpoint.OriginalString,
            RequestTimeoutMs = Ms(options.RequestTimeout),
            ConnectTimeoutMs = Ms(options.ConnectTimeout),
            StopTimeoutMs = Ms(options.StopTimeout),
            MaxSendBytes = Bytes(options.MaxSendBytes),
            MaxReceiveBytes = Bytes(options.MaxReceiveBytes),
            AcceptConnectionSettings = options.AcceptConnectionSettings,
            VerifyTimeoutMs = Ms(options.VerifyTimeout),
            AcceptPackages = options.AcceptPackages,
            RequestInstanceUid = options.RequestInstanceUid,
            MaxPackageBytes = options.MaxPackageBytes,
            DownloadTimeoutMs = Ms(options.DownloadTimeout),
            Streams = coreMode,
            SpliceDownloads = coreMode && options.SpliceDownloads,
            // Core mode: the core picks each connection's proxy from this.
            Proxy = coreMode ? CoreProxyChoice(options, seams?.Environment ?? Environment.GetEnvironmentVariable) : null,
        };
        if (options.StateFile is { } stateFile)
        {
            config.Persistence = true;
            // A client stopped before on the same file may have left a save writing.
            Client.StateFile.Settled(stateFile);
            config.SavedState = ByteString.CopyFrom(Client.StateFile.Load(stateFile));
        }

        config.Headers.AddRange(options.Headers.Select(h => new Header { Name = h.Key, Value = h.Value }));
        Func<string, string?>? getenv = seams?.Tap is { } tap ? name => name == CoreTap.Variable ? tap : null : null;
        this.core = new CoreInstance(config, getenv, seams?.Core);
        if (options.StateFile is { } downloadsOf)
        {
            // Files downloaded before a restart: the core downloads again what it still needs.
            // (Once the core has taken the config: not for one it refuses.)
            Executor.DeleteDownloads(downloadsOf, files);
        }

        if (this.core.Tap is { } t && options.StateFile is { } state)
        {
            // Tapped: the file operations of its saves and downloads are recorded too.
            files = new FileJournal(files, t, state);
        }

        this.instanceUid = this.core.Initial.InstanceUid.ToByteArray();
        this.notify = notify ?? (_ => { });
        this.executor = new Executor(this.transport, this.Enqueue, options.StateFile, this.time, files);
        if (coreMode)
        {
            this.streams = new Streams(
                this.executor, this.Enqueue, ClientTransport.SslOptions(options.Tls ?? new OpAmpTlsOptions()), _ => ProxyAnswerFor(options), seams?.Net, this.time);
        }

        // All of stop takes the core's stop timeout, as given or by default: its start reports it.
        this.stopTimeout = TimeSpan.FromMilliseconds(this.core.Initial.Limits?.StopTimeoutMs ?? 0);
    }

    /// <summary>
    /// Start a client. <paramref name="notify"/> is called for every notification, on the client's
    /// loop, never before <c>Start</c> returns: keep it quick. A remote config saved before a
    /// restart is handed over at once, perhaps before the caller has stored what <c>Start</c>
    /// returned: to answer through the client, take it from the overload that passes it to the
    /// callback.
    /// </summary>
    public static OpAmpClient Start(OpAmpClientOptions options, OpAmpStatus status, Action<OpAmpNotification>? notify = null) =>
        Run(new OpAmpClient(options, notify, null), status);

    /// <summary>
    /// Start a client. <paramref name="notify"/> is called for every notification with the client
    /// itself, on the client's loop, never before <c>Start</c> returns: keep it quick. Answer
    /// through the client it is given (a remote config saved before a restart is handed over as
    /// the client starts).
    /// </summary>
    public static OpAmpClient Start(OpAmpClientOptions options, OpAmpStatus status, Action<OpAmpClient, OpAmpNotification> notify)
    {
        ArgumentNullException.ThrowIfNull(notify);
        OpAmpClient? self = null;
        self = new OpAmpClient(options, n => notify(self!, n), null); // set before the loop starts
        return Run(self, status);
    }

    /// <summary>Start a client on <paramref name="core"/> instead of the native core: for tests only.</summary>
    internal static OpAmpClient Start(
        OpAmpClientOptions options, OpAmpStatus status, Action<OpAmpNotification>? notify, Func<ICoreEngine> core) =>
        Run(new OpAmpClient(options, notify, new Seams { Core = core }), status);

    /// <summary>Start a client with <paramref name="seams"/> in place of its own parts: for tests only.</summary>
    internal static OpAmpClient Start(OpAmpClientOptions options, OpAmpStatus status, Action<OpAmpNotification>? notify, Seams seams) =>
        Run(new OpAmpClient(options, notify, seams), status);

    private static OpAmpClient Run(OpAmpClient c, OpAmpStatus status)
    {
        if (status.Description is { } d)
        {
            c.Post(new Input { SetDescription = d });
        }

        if (status.Health is { } h)
        {
            c.Post(new Input { SetHealth = h });
        }

        if (status.EffectiveConfig is { } e)
        {
            c.Post(new Input { SetEffectiveConfig = e });
        }

        if (status.RemoteConfigStatus is { } r)
        {
            c.Post(new Input { SetRemoteConfigStatus = r });
        }

        if (status.AvailableComponents is { } a)
        {
            c.Post(new Input { SetAvailableComponents = a });
        }

        if (status.CustomCapabilities is { } cc)
        {
            c.Post(new Input { SetCustomCapabilities = cc });
        }

        c.Post(new Input { Start = new Empty() });
        _ = Task.Run(c.RunAsync);
        c.started.SetResult(); // the loop steps, and notifies, from now on
        return c;
    }

    /// <summary>
    /// What is saved at <paramref name="stateFile"/> (empty if there is no file yet), once the work
    /// a client stopped before on it left under way has ended: what a client started next on the
    /// file starts from. <see cref="StopAsync"/> doesn't wait for a save already writing (it lands
    /// by itself), so a read straight after it can find the state before.
    /// </summary>
    public static byte[] ReadState(string stateFile)
    {
        ArgumentNullException.ThrowIfNull(stateFile);
        Client.StateFile.Settled(stateFile);
        return Client.StateFile.Load(stateFile);
    }

    /// <summary>
    /// The instance UID in use (16 bytes): the application's, one saved before a restart, one the
    /// core made, or one the server assigned since (<see cref="OpAmpNotification.InstanceUidChanged"/>).
    /// </summary>
    public byte[] InstanceUid => (byte[])Volatile.Read(ref this.instanceUid).Clone();

    /// <summary>
    /// The endpoint the client uses now: where its latest OpAMP request or WebSocket connection went,
    /// which after the server moved the agent (an offer of connection settings) is the offered one,
    /// or the configured one again once an offer failed. Null before any outcome, and in core mode,
    /// where the core makes the requests itself (the configured one is all there is to name then).
    /// The URL is as the core gave it: redact it before logging.
    /// </summary>
    public string? Endpoint => this.streams is null ? this.executor.Used : null;

    /// <summary>Inputs queued for the loop, not yet stepped: for tests, to wait until it is idle.</summary>
    internal int Queued => Volatile.Read(ref this.queued);

    /// <summary>Effects' work still running (requests, connections, downloads, saves): for tests.</summary>
    internal int Working => this.executor.InFlight.Working + (this.streams?.In.Working ?? 0);

    /// <summary>Core mode's streams open, connecting or lingering: for tests.</summary>
    internal int LiveStreams => this.streams?.Live ?? 0;

    /// <summary>Core mode's work that waits on the network or the clock: for tests.</summary>
    internal int StreamsParked => this.streams?.Parked ?? 0;

    /// <summary>Change the agent description.</summary>
    public void SetDescription(AgentDescription value) => this.Call(new Input { SetDescription = value });

    /// <summary>Change the health.</summary>
    public void SetHealth(ComponentHealth value) => this.Call(new Input { SetHealth = value });

    /// <summary>Change the effective config.</summary>
    public void SetEffectiveConfig(EffectiveConfig value) => this.Call(new Input { SetEffectiveConfig = value });

    /// <summary>Change the custom capabilities the agent supports.</summary>
    public void SetCustomCapabilities(CustomCapabilities value) => this.Call(new Input { SetCustomCapabilities = value });

    /// <summary>
    /// Send a custom message, for one of the agent's custom capabilities. It goes with the next
    /// message the server takes, and again if that message is lost; if it can't be sent at all, a
    /// <see cref="OpAmpNotification.CustomMessageNotSent"/> says why.
    /// </summary>
    public void SendCustomMessage(global::OpAmp.Proto.V1.CustomMessage value) => this.Call(new Input { SendCustomMessage = value });

    /// <summary>Change the components the agent has (leave the hash empty: the core computes it).</summary>
    public void SetAvailableComponents(AvailableComponents value) => this.Call(new Input { SetAvailableComponents = value });

    /// <summary>Report on a remote config (the answer to a <see cref="OpAmpNotification.RemoteConfig"/>).</summary>
    public void SetRemoteConfigStatus(RemoteConfigStatus value) => this.Call(new Input { SetRemoteConfigStatus = value });

    /// <summary>Report on telemetry settings (the answer to a <see cref="OpAmpNotification.ConnectionSettings"/>).</summary>
    public void SetConnectionSettingsStatus(ConnectionSettingsStatus value) =>
        this.Call(new Input { SetConnectionSettingsStatus = value });

    /// <summary>
    /// Answer <see cref="OpAmpNotification.InstallPackage"/>: the package at <paramref name="hash"/>
    /// is installed (<paramref name="error"/> null), or why it isn't.
    /// </summary>
    public void PackageInstalled(string name, byte[] hash, string? error = null) =>
        this.Call(new Input
        {
            PackageInstalled = new Core.PackageInstalled
            {
                Name = name, Hash = ByteString.CopyFrom(hash), Ok = error is null, Detail = error ?? string.Empty,
            },
        });

    /// <summary>
    /// Answer <see cref="OpAmpNotification.RemovePackage"/>: the package is removed
    /// (<paramref name="error"/> null), or why it isn't. One that isn't is reported failed, and not
    /// removed again until it is offered and dropped again.
    /// </summary>
    public void PackageRemoved(string name, string? error = null) =>
        this.Call(new Input
        {
            PackageRemoved = new Core.PackageRemoved
            {
                Name = name, Failed = error is not null, Detail = error ?? string.Empty,
            },
        });

    /// <summary>
    /// Send agent_disconnect if a message can still be sent, then stop: the task ends when the
    /// core says it has stopped, within <see cref="OpAmpClientOptions.StopTimeout"/> (5 s), with
    /// nothing the client started still running but a save already writing, which finishes whole
    /// or not at all (a client started on the same state file waits for it).
    /// </summary>
    public Task StopAsync()
    {
        // All of stop takes the stop timeout, from the first call: what the core takes of it
        // (the goodbye, a save) is not given again to the drain.
        Interlocked.CompareExchange(ref this.stopCalled, this.time.GetTimestamp(), NotStopped);
        this.Call(new Input { Stop = new Empty() });
        return this.done.Task;
    }

    /// <summary>
    /// A time for the core, in whole milliseconds (toward zero). A negative one reaches it as its
    /// two's complement, which the core refuses at start, naming the field and the number meant, as
    /// in every SDK (-1 ms, as <see cref="Timeout.InfiniteTimeSpan"/> is, included).
    /// </summary>
    private static ulong Ms(TimeSpan t) => unchecked((ulong)(long)t.TotalMilliseconds);

    /// <summary>A size for the core: a negative one is refused at start, as a negative time is.</summary>
    private static ulong Bytes(long n) => unchecked((ulong)n);

    /// <summary>Core mode: what can't apply there is an error, at start.</summary>
    private static OpAmpClientOptions CheckCore(OpAmpClientOptions o)
    {
        if (o.HttpClient is not null)
        {
            throw new ArgumentException("HttpClient can't be used in core mode: the core speaks HTTP itself, over the client's own connections (set Tls and Proxy instead, or leave Mode unset: an HttpClient selects platform mode)", nameof(o));
        }

        if (o.Proxy is not null && o.DirectConnection)
        {
            throw new ArgumentException("Proxy and DirectConnection don't go together", nameof(o));
        }

        if (o.Proxy is { } p)
        {
            var scheme = p.IsAbsoluteUri ? p.Scheme.ToLowerInvariant() : string.Empty;
            if (scheme is not ("http" or "https" or "socks5" or "socks5h"))
            {
                throw new ArgumentException("Proxy is an http://, https://, socks5:// or socks5h:// URL", nameof(o));
            }

            if (o.ProxyResolveLocally && scheme is "http" or "https")
            {
                throw new ArgumentException("ProxyResolveLocally can't be honoured: an HTTP proxy is given the server's host name (CONNECT host:port) and resolves it itself", nameof(o));
            }
        }

        return o;
    }

    /// <summary>
    /// The variables core mode's proxy rule reads (tests/shell-tables/README.md, "core mode's proxy
    /// rule"). The core applies the rule; the client only reads them, once.
    /// </summary>
    internal static readonly string[] CoreProxyVariables =
    [
        "REQUEST_METHOD", "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
    ];

    /// <summary>
    /// What core mode tells the core about proxies, once (Config.proxy): none if DirectConnection;
    /// the configured proxy (with its credentials), if any; else the environment's variables, from
    /// which the core picks each connection's proxy by the shared rule.
    /// </summary>
    internal static ProxyChoice CoreProxyChoice(OpAmpClientOptions o, Func<string, string?> env)
    {
        if (o.DirectConnection)
        {
            return new ProxyChoice { Direct = true };
        }

        if (o.Proxy is { } p)
        {
            return new ProxyChoice { Url = p.OriginalString.Trim(), ResolveLocally = o.ProxyResolveLocally };
        }

        var choice = new ProxyChoice { ResolveLocally = o.ProxyResolveLocally };
        foreach (var name in CoreProxyVariables)
        {
            if (env(name) is { } value)
            {
                choice.Environment.Add(new Header { Name = name, Value = value });
            }
        }

        return choice;
    }

    /// <summary>
    /// Core mode's answer to the core's proxy question, which the core asks only when it wasn't
    /// told (<see cref="CoreProxyChoice"/>): the configured proxy (with its credentials), or none.
    /// The environment's rule is the core's.
    /// </summary>
    internal static ProxyAnswer ProxyAnswerFor(OpAmpClientOptions o) =>
        !o.DirectConnection && o.Proxy is { } p
            ? new ProxyAnswer { Proxy = p.OriginalString.Trim(), ResolveLocally = o.ProxyResolveLocally }
            : new ProxyAnswer();

    private OpAmpNotification? FromCore(Notification n) => n.KindCase switch
    {
        Notification.KindOneofCase.Connected => new OpAmpNotification.Connected(),
        Notification.KindOneofCase.ConnectFailed => new OpAmpNotification.ConnectFailed(
            n.ConnectFailed.HasStatus ? (int)n.ConnectFailed.Status : null, n.ConnectFailed.Detail),
        Notification.KindOneofCase.ErrorResponse => new OpAmpNotification.ErrorResponse(n.ErrorResponse.Kind, n.ErrorResponse.Message),
        Notification.KindOneofCase.RemoteConfig => new OpAmpNotification.RemoteConfig(n.RemoteConfig),
        Notification.KindOneofCase.ConnectionSettings => new OpAmpNotification.ConnectionSettings(n.ConnectionSettings),
        Notification.KindOneofCase.CustomMessage => new OpAmpNotification.CustomMessage(n.CustomMessage),
        Notification.KindOneofCase.ServerCustomCapabilities =>
            new OpAmpNotification.ServerCustomCapabilities(n.ServerCustomCapabilities.Capabilities.ToList()),
        Notification.KindOneofCase.CustomMessageNotSent =>
            new OpAmpNotification.CustomMessageNotSent(n.CustomMessageNotSent.Message, n.CustomMessageNotSent.Reason),
        Notification.KindOneofCase.InstallPackage => new OpAmpNotification.InstallPackage(
            n.InstallPackage.Name, n.InstallPackage.Package, this.executor.DownloadPath(n.InstallPackage.Download)),
        Notification.KindOneofCase.RemovePackage => new OpAmpNotification.RemovePackage(n.RemovePackage.Name_),
        Notification.KindOneofCase.Command => new OpAmpNotification.Command(n.Command),
        Notification.KindOneofCase.InstanceUidChanged => new OpAmpNotification.InstanceUidChanged(n.InstanceUidChanged.ToByteArray()),
        Notification.KindOneofCase.HeartbeatIntervalMs => new OpAmpNotification.HeartbeatInterval(
            n.HeartbeatIntervalMs > MaxSpanMs ? TimeSpan.MaxValue : TimeSpan.FromMilliseconds(n.HeartbeatIntervalMs)),
        Notification.KindOneofCase.StatusTooLarge => new OpAmpNotification.StatusTooLarge(n.StatusTooLarge.Name, (long)n.StatusTooLarge.Bytes),
        Notification.KindOneofCase.BadMessage => new OpAmpNotification.BadMessage(n.BadMessage.Detail_),
        Notification.KindOneofCase.SaveStalled => new OpAmpNotification.SaveStalled(n.SaveStalled.Stalled,
            n.SaveStalled.WaitedMs > MaxSpanMs ? TimeSpan.MaxValue : TimeSpan.FromMilliseconds(n.SaveStalled.WaitedMs)),
        Notification.KindOneofCase.Stopped => new OpAmpNotification.Stopped(),
        _ => null,
    };

    private void Post(Input input) => this.Enqueue(new Queued(input));

    /// <summary>
    /// One of the application's calls, from any thread: queued behind its calls before it, with
    /// its turn in the inputs' queue. The calls have a queue of their own so that those a callback
    /// made can be fed ahead of their turn, in order, without passing over any made before them.
    /// </summary>
    private void Call(Input input)
    {
        // Queued and given its turn in one: the turns come in the calls' order, so a call fed
        // ahead of its turn is the one whose turn comes next.
        lock (this.calls)
        {
            this.calls.Enqueue(input);
            Interlocked.Increment(ref this.queued);
            if (!this.inputs.Writer.TryWrite(new Queued(input, Turn: true)))
            {
                Interlocked.Decrement(ref this.queued);
                this.calls.Clear(); // stopped: nobody takes them
            }
        }
    }

    /// <summary>How many of the application's calls are waiting.</summary>
    private int Waiting()
    {
        lock (this.calls)
        {
            return this.calls.Count;
        }
    }

    /// <summary>The oldest of the application's calls waiting, taken; null if none is.</summary>
    private Input? NextCall()
    {
        lock (this.calls)
        {
            return this.calls.TryDequeue(out var input) ? input : null;
        }
    }

    private void Enqueue(Queued q)
    {
        Interlocked.Increment(ref this.queued);
        if (!this.inputs.Writer.TryWrite(q))
        {
            Interlocked.Decrement(ref this.queued); // stopped
        }
    }

    /// <summary>
    /// How long the drain may wait once the core has stopped: what is left of the stop timeout
    /// since <see cref="StopAsync"/> was first called, but <see cref="StopGrace"/>; all of it when
    /// the client stopped on its own.
    /// </summary>
    private TimeSpan DrainBound()
    {
        var at = Interlocked.Read(ref this.stopCalled);
        if (at == NotStopped)
        {
            return this.stopTimeout;
        }

        var left = this.stopTimeout - this.time.GetElapsedTime(at);
        return left > StopGrace ? left : StopGrace;
    }

    /// <summary>The client's clock: milliseconds since it was made, monotonic.</summary>
    private long NowMs() => (long)this.time.GetElapsedTime(this.started0).TotalMilliseconds;

    /// <summary>
    /// Step the core with a timer at <paramref name="wakeAtMs"/> on the client's clock, never before:
    /// any value, a wait longer than Task.Delay takes being made of several.
    /// covers: RULE-SHELL-08
    /// </summary>
    private async Task WakeAsync(ulong wakeAtMs, CancellationToken ct)
    {
        try
        {
            while (true)
            {
                var left = (long)Math.Min(wakeAtMs, (ulong)long.MaxValue) - this.NowMs();
                if (left <= 0)
                {
                    break;
                }

                await Task.Delay(TimeSpan.FromMilliseconds(Math.Min(left, int.MaxValue)), this.time, ct).ConfigureAwait(false);
            }

            this.Post(new Input { Timer = new Empty() });
        }
        catch (OperationCanceledException)
        {
            // replaced by the next step's
        }
    }

    /// <summary>Whether <paramref name="effect"/> starts something: a request, a connection, a download, a stream, a resolution.</summary>
    internal static bool Starts(Effect effect) => effect.KindCase is Effect.KindOneofCase.HttpRequest or Effect.KindOneofCase.WsOpen
        or Effect.KindOneofCase.Download or Effect.KindOneofCase.StreamOpen or Effect.KindOneofCase.StreamResolve;

    private async Task RunAsync()
    {
        Exception? error = null;
        // Nothing is stepped, so nothing notified, before Start has returned.
        await this.started.Task.ConfigureAwait(false);
        var told = false; // Stopped: the application hears it once

        // How many of the application's calls, from the oldest, are a callback's own: as far as the
        // last thing a callback of the step under way asked for (0: none asked for anything).
        var asked = 0;

        // Calls fed ahead of their turn: that many turns to come have nothing left to feed.
        var fedEarly = 0;

        // Step the core with q and carry out the effects: whether the core has stopped.
        bool Feed(Queued q)
        {
            // A result only while its effect is in flight: nothing about one the core
            // cancelled, even a result queued before the cancel.
            if (!(q.Registry ?? this.executor.InFlight).Take(q))
            {
                q.Ack?.TrySetResult();
                return false;
            }

            var output = this.core.Step(this.NowMs(), this.time.GetUtcNow().ToUnixTimeMilliseconds(), q.Input);
            this.stepped?.Invoke(q, output);
            if (output.InstanceUid.Length > 0)
            {
                Volatile.Write(ref this.instanceUid, output.InstanceUid.ToByteArray());
            }

            // Stopped is told last: the core may hand over a notification after it in the same
            // output (install_package, in the step that answers a save after stop), and
            // nothing reaches the application after Stopped (RULE-SHELL-01).
            OpAmpNotification.Stopped? stopped = null;
            this.streams?.Stepping(q);
            foreach (var effect in output.Effects)
            {
                if (effect.KindCase == Effect.KindOneofCase.Notify)
                {
                    switch (this.FromCore(effect.Notify))
                    {
                        case OpAmpNotification.Stopped s:
                            stopped = s;
                            break;
                        case { } n:
                            var before = this.Waiting();
                            this.notify(n);
                            var now = this.Waiting();
                            if (now > before)
                            {
                                asked = now;
                            }

                            break;
                    }
                }
                else if (output.Stopped && Starts(effect))
                {
                    // An output that says stopped starts nothing new (RULE-SHELL-07): what it
                    // would start could only be given up on.
                }
                else if (this.streams?.Run(effect) != true)
                {
                    // Core mode: what isn't a stream's or a download file's (a save, a cancel, a
                    // platform effect from a core that isn't the real one) is the executor's.
                    this.executor.Run(effect);
                }
            }

            this.streams?.Stepping(null);

            if (stopped is not null)
            {
                told = true;
                this.notify(stopped);
            }

            this.timer?.Cancel();
            this.timer?.Dispose();
            this.timer = null;
            if (output.HasWakeAtMs)
            {
                var cts = this.timer = new CancellationTokenSource();
                _ = this.WakeAsync(output.WakeAtMs, cts.Token);
            }

            // The stream that was read from may read on.
            q.Ack?.TrySetResult();

            // Nothing the core started is still running now: no more steps.
            return output.Stopped;
        }

        try
        {
            await foreach (var q in this.inputs.Reader.ReadAllAsync().ConfigureAwait(false))
            {
                Interlocked.Decrement(ref this.queued);
                var next = q;
                if (q.Turn)
                {
                    // The turn of one of the application's calls: the oldest still waiting,
                    // unless it was fed ahead of its turn.
                    if (fedEarly > 0)
                    {
                        fedEarly--;
                        continue;
                    }

                    if (this.NextCall() is not { } call)
                    {
                        continue;
                    }

                    next = new Queued(call);
                }

                var over = Feed(next);

                // What the application asked from inside the callback is its answer to that step:
                // it reaches the core before anything the client takes in next (RULE-SHELL-10). So
                // its calls are fed here, as far as the callback's last one, in order (with what
                // other callers asked before it: none is passed over). Left to their turns, they
                // waited behind whatever was queued while the callback ran: a connection that
                // opened, whose first message then said APPLYING for a saved config the callback
                // had answered. Calls from elsewhere keep their turn: where no callback calls the
                // client, the order is the queue's, as it was.
                while (!over && asked > 0)
                {
                    asked--;
                    if (this.NextCall() is not { } asking)
                    {
                        break; // (Each is there: only this loop takes from the queue.)
                    }

                    fedEarly++;
                    over = Feed(new Queued(asking));
                }

                if (over)
                {
                    break;
                }
            }
        }
        catch (Exception e)
        {
            error = e; // a CoreException, or a bug in the application's callback

            // The client stopped on its own: the application hears it now, not only from
            // StopAsync. If its callback is what failed, it may fail again: that is already the
            // error reported. If it failed on Stopped itself, it has heard it: not twice.
            try
            {
                if (!told)
                {
                    this.notify(new OpAmpNotification.Stopped(e));
                }
            }
            catch (Exception)
            {
            }
        }

        this.inputs.Writer.TryComplete();
        this.timer?.Cancel();

        // The core has waited for the goodbye and the closing handshake and cancelled the rest:
        // release everything (anything left, after an error), and let the cancelled work finish,
        // within what is left of the stop timeout (but a short grace to unwind; all of it when the
        // client stopped on its own): nothing the client started runs on after stop. Core mode: the
        // core has closed every stream by now (what is left after an error is reset).
        var bound = this.DrainBound();
        await Task.WhenAll(this.executor.ReleaseAsync(bound), this.streams?.ShutdownAsync(bound) ?? Task.CompletedTask).ConfigureAwait(false);
        this.transport.Dispose();
        this.core.Dispose();
        if (error is null)
        {
            this.done.TrySetResult();
        }
        else
        {
            this.done.TrySetException(error);
        }
    }
}

/// <summary>Parts of a client tests put in place of its own.</summary>
internal sealed record Seams
{
    /// <summary>Another core behind the same bytes (the WebAssembly module, a hostile one) instead of the native one.</summary>
    public Func<ICoreEngine>? Core { get; init; }

    /// <summary>What the effects are carried over, instead of one built from the options.</summary>
    public IClientTransport? Transport { get; init; }

    /// <summary>
    /// Called on the loop after each step, before its effects are carried out: what went in (and
    /// which effect it was a result of) and what came out.
    /// </summary>
    public Action<Queued, Output>? Stepped { get; init; }

    /// <summary>The file operations its saves and downloads go through (to inject faults).</summary>
    public IFileOps? Files { get; init; }

    /// <summary>Its own core tap's path, instead of OPAMP_CORE_TAP's.</summary>
    public string? Tap { get; init; }

    /// <summary>Core mode: the network (connects, TLS, resolution) instead of .NET's.</summary>
    public StreamsNet? Net { get; init; }

    /// <summary>Core mode: the environment the proxy rule reads, instead of the process's.</summary>
    public Func<string, string?>? Environment { get; init; }
}
