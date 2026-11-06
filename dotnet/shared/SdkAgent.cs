// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Text;
using Google.Protobuf;
using OpAmp.Proto.V1;
using OpenTelemetry;
using Logfire.Fleet.Client;
using Logfire.Fleet.Client.Sdk;
using Logfire.Fleet.Core;
using LayerLog = Logfire.Fleet.Client.Sdk.AgentLog;

namespace Logfire.Fleet.Otel;

/// <summary>
/// Where the server is: OTEL_OPAMP_*, or the hosting package's options, as the core's Setup decides
/// it. <paramref name="Shown"/> is the endpoint as it is safe to log and <paramref name="Hints"/>
/// what to check when the server can't be reached (null: as for OTEL_OPAMP_*);
/// <paramref name="DefaultSlots"/> the default state directory's slots, when none is named.
/// </summary>
internal sealed record Server(
    Uri Endpoint,
    IReadOnlyDictionary<string, string> Headers,
    string? StateDir,
    TimeSpan Heartbeat,
    string? Shown = null,
    SetupHints? Hints = null,
    IReadOnlyList<string>? DefaultSlots = null,
    string? AllSlotsTaken = null)
{
    /// <summary>The variables the core's Setup reads.</summary>
    private static readonly string[] Variables =
    [
        "OTEL_OPAMP_DISABLED", "OTEL_SDK_DISABLED", "OTEL_OPAMP_ENDPOINT", "OTEL_OPAMP_HEADERS", "OTEL_OPAMP_STATE_DIR",
        "OTEL_OPAMP_HEARTBEAT_INTERVAL", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES", "HOME", "XDG_STATE_HOME", "LOCALAPPDATA",
    ];

    /// <summary>The variables that turn OpAMP off, whatever else is given.</summary>
    private static readonly string[] Switches = ["OTEL_OPAMP_DISABLED", "OTEL_SDK_DISABLED"];

    /// <summary>The variables without the switches: where the server is, as the variables say it.</summary>
    private static readonly string[] Settings = Variables.Except(Switches).ToArray();

    /// <summary>
    /// What the hosting package's settings don't replace: the switches, and the variables that
    /// aren't OTEL_OPAMP_*.
    /// </summary>
    private static readonly string[] Others =
        Variables.Where(v => Switches.Contains(v) || !v.StartsWith("OTEL_OPAMP_", StringComparison.Ordinal)).ToArray();

    /// <summary>The endpoint as it is safe to log.</summary>
    public string ShownEndpoint => this.Shown ?? ShownFor(this.Endpoint.OriginalString, this.Endpoint.OriginalString);

    /// <summary>
    /// An endpoint as it is safe to log (no user info, query or fragment), as the core shows it;
    /// <paramref name="otherwise"/> if the core can't say.
    /// </summary>
    public static string ShownFor(string endpoint, string otherwise)
    {
        try
        {
            return OpAmpSetup.Start(new SetupStart { Code = new SetupInCode { Endpoint = endpoint } }).ShownEndpoint is { Length: > 0 } s ? s : otherwise;
        }
        catch (CoreException)
        {
            return otherwise;
        }
    }

    /// <summary>What to check when the server can't be reached.</summary>
    public SetupHints Hinted => this.Hints ?? HintsFor(this.Endpoint.OriginalString);

    /// <summary>
    /// Null when OTEL_OPAMP_ENDPOINT is unset, or OTEL_OPAMP_DISABLED or OTEL_SDK_DISABLED turns
    /// OpAMP off (said, through <paramref name="warn"/>): nothing is managed. The rules are the
    /// core's (its Setup call), the same in every language; a setting it can't use is the default,
    /// with a warning. An endpoint that isn't a URL throws. Without <paramref name="switches"/>
    /// the switches aren't read: the hosting package reads them with its options (FromOptions).
    /// </summary>
    public static Server? FromEnv(Func<string, string?> getenv, Action<string> warn, bool switches = true) =>
        FromCore(Start(getenv, only: switches ? null : Settings), warn);

    /// <summary>
    /// The hosting package's options, given in code (what the configuration and the variables said
    /// is in them already): the endpoint, headers, state directory and interval, named as its
    /// settings. Null: no endpoint, or OTEL_OPAMP_DISABLED or OTEL_SDK_DISABLED turns OpAMP off
    /// whatever the options say (said, through <paramref name="warn"/>).
    /// </summary>
    public static Server? FromOptions(Uri? endpoint, IDictionary<string, string> headers, string? stateDir, TimeSpan? heartbeat,
        Func<string, string?> getenv, Action<string> warn, string endpointName, string headersName, string heartbeatName)
    {
        var start = Start(getenv, only: Others);
        start.Code = new SetupInCode
        {
            HeadersGiven = true,
            EndpointName = endpointName,
            HeadersName = headersName,
            HeartbeatName = heartbeatName,
        };
        if (endpoint is not null)
        {
            start.Code.Endpoint = endpoint.OriginalString;
        }

        start.Code.Headers.AddRange(headers.Select(h => new Logfire.Fleet.Core.Header { Name = h.Key, Value = h.Value }));
        if (stateDir is not null)
        {
            start.Code.StateDir = stateDir;
        }

        if (heartbeat is { } h && h != TimeSpan.Zero)
        {
            start.Code.HeartbeatMs = (long)h.TotalMilliseconds;
        }

        return FromCore(start, warn);
    }

    /// <summary>The Setup call's start: the variables, this OS, the program and the working directory.</summary>
    internal static SetupStart Start(Func<string, string?> getenv, string? program = null, string? cwd = null, string[]? only = null)
    {
        if (cwd is null)
        {
            try
            {
                cwd = Environment.CurrentDirectory;
            }
            catch (Exception e) when (e is IOException or UnauthorizedAccessException)
            {
                cwd = string.Empty;
            }
        }

        var start = new SetupStart
        {
            Os = OperatingSystem.IsWindows() ? SetupOs.Windows : OperatingSystem.IsMacOS() ? SetupOs.Macos : SetupOs.Unix,
            Program = program ?? StateDirs.ProgramName(),
            Cwd = cwd,
        };
        foreach (var name in only ?? Variables)
        {
            if (getenv(name) is { Length: > 0 } value)
            {
                start.Env.Add(new Variable { Name = name, Value = value });
            }
        }

        return start;
    }

    private static Server? FromCore(SetupStart start, Action<string> warn)
    {
        var s = OpAmpSetup.Start(start);
        foreach (var w in s.Warnings)
        {
            warn(w);
        }

        if (s.Off)
        {
            // Turned off by a variable, or an endpoint without a scheme refused (said once, here); off for
            // want of an endpoint says nothing.
            if (s.OffReason.Length > 0)
            {
                warn(s.OffReason);
            }

            return null;
        }

        var headers = new Dictionary<string, string>();
        foreach (var h in s.Headers)
        {
            headers[h.Name] = h.Value;
        }

        return new Server(
            new Uri(s.Endpoint),
            headers,
            s.StateDir.Length > 0 ? s.StateDir : null,
            TimeSpan.FromMilliseconds(s.HeartbeatIntervalMs),
            s.ShownEndpoint,
            s.Hints,
            s.DefaultSlots.ToList(),
            s.AllSlotsTaken);
    }

    private static SetupHints HintsFor(string endpoint)
    {
        try
        {
            return OpAmpSetup.Start(new SetupStart
            {
                Env =
                {
                    new Variable { Name = "OTEL_OPAMP_ENDPOINT", Value = endpoint },
                    new Variable { Name = "OTEL_OPAMP_STATE_DIR", Value = "none" },
                },
            }).Hints;
        }
        catch (CoreException)
        {
            return new SetupHints();
        }
    }
}

/// <summary>
/// The clocks: the wall clock in milliseconds since the Unix epoch, which only debug mode reads (the
/// layer is given it), and a monotonic one in nanoseconds, for the rate caps and timers.
/// Injected in tests.
/// </summary>
internal sealed record Clock(Func<ulong> WallMs, Func<long> Nanos, Func<TimeSpan, Action, IDisposable>? Schedule = null)
{
    public static readonly Clock System = new(() => (ulong)DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(), TokenBucket.Monotonic);

    /// <summary>Run <paramref name="action"/> once, after <paramref name="delay"/>; disposing cancels it.</summary>
    public IDisposable After(TimeSpan delay, Action action)
    {
        if (this.Schedule is { } schedule)
        {
            return schedule(delay, action);
        }

        var timer = new Timer(_ => action(), null, Timeout.Infinite, Timeout.Infinite);
        timer.Change(delay, Timeout.InfiniteTimeSpan);
        return timer;
    }
}

/// <summary>Where what the agent says goes: the plugin's standard error, or the application's logger.</summary>
/// <remarks>
/// <c>Notice</c> is what a person checks for once, such as the first connection: information, shown
/// unless logging was asked to be quieter. Without one it is <c>Info</c>.
/// </remarks>
internal sealed record AgentLog(Action<string> Info, Action<string> Warn, Action<string> Error, Action<string>? Debug = null,
    Action<string>? Notice = null);

/// <summary>
/// The SDK under the server, whatever set the SDK up (the plugin, or the hosting package): the
/// instance UID kept in the state directory, the SDK settings layer's agent flow carried out on
/// <see cref="Controls"/>, and the client. What this SDK carries out is the
/// caller's (<c>host</c>, asked again once the providers are built).
/// </summary>
internal sealed class SdkAgent
{
    private readonly Server server;
    private readonly Func<string, string?> getenv;
    private readonly Func<bool, Host> hostNow;
    private readonly AgentLog log;
    private readonly SdkSettings layer;
    private readonly object gate = new();
    private readonly FileStream? stateLock;
    private readonly Clock clock;
    private Host host;
    private Settings inForce = new();

    // The agent flow's state, as the layer handed it over: passed back with each step. The flow
    // decides what is in force, what to answer and report, what to say, and when to step its timer.
    private ByteString flow = ByteString.Empty;

    // The monotonic clock's reading when the flow started: the flow's clock counts from it.
    private long startNanos;

    // The one timer, for the flow's next wake (on the flow's clock; 0: none). A step after a timer
    // was armed arms its own: the generation says the older one is stale.
    private IDisposable? timer;
    private ulong wakeAt;
    private int generation;
    private bool stopped;

    // The application's own health, and export health's entry as the flow last sent it.
    private ComponentHealth app = new() { Healthy = true };

    // Whether SetHealth has been called: its health is newer than the one Start is given.
    private bool appSet;
    private ComponentHealth? exportHealth;
    private EffectiveConfig effective = new();
    private OpAmpClient? client;
    private AvailableComponents? components;

    // Once started: what the SDK runs, as the caller reads it now, for the available components.
    // Read again by Recheck: an exporter added since is listed, and reported in export health.
    private Func<Inventory>? inventory;

    // The exporters last listed, governed or not: one child each in export health.
    private List<Exporter> exporters = [];

    // The exporters as last found and decided (Exporters), and the layer's warnings said.
    private readonly object decisions = new();
    private List<FoundExporter> found = [];
    private List<Exporter> decided = [];
    private readonly HashSet<string> warned = [];

    // Each exporter as the layer last listed it, by what was found: should a call fail, they keep their names.
    private readonly Dictionary<FoundExporter, Exporter> listedAs = [];

    /// <summary>The layer's Exporters call; a test's own in tests (one that fails, say).</summary>
    internal Func<List<FoundExporter>, IEnumerable<KeyValuePair<string, string>>, string, ExportersDecided> DecideExporters;

    /// <summary>
    /// Name the instance, start the flow, and put the settings to start with in force on
    /// <paramref name="controls"/>. <paramref name="hostNow"/> says what the SDK carries out (its
    /// argument: whether the providers are built).
    /// </summary>
    public SdkAgent(Server server, Func<string, string?> getenv, FileStream? stateLock, Func<bool, Host> hostNow, AgentLog log, Controls controls, Clock? clock = null)
    {
        this.clock = clock ?? Clock.System;
        this.server = server;
        this.getenv = getenv;
        this.stateLock = stateLock;
        this.hostNow = hostNow;
        this.log = log;
        this.Controls = controls;
        // What the application's own settings say goes where the agent says the rest.
        controls.App.Info = log.Info;
        controls.App.Error = log.Error;
        this.layer = new SdkSettings();
        this.DecideExporters = (found, env, protocol) => this.layer.Exporters(found, env, protocol);
        this.host = hostNow(false);
        this.Uid = this.Identity();
        var hints = server.Hinted;
        var start = new AgentStart
        {
            Host = this.host,
            StateDir = server.StateDir is not null,
            Endpoint = server.Endpoint.OriginalString,
            InstanceUid = ByteString.CopyFrom(Uuid.Bytes(this.Uid)),
            Hints = new AgentHints { Refused = hints.Refused, NotFound = hints.NotFound, Unreachable = hints.Unreachable, Error = hints.Error },
            NowUnixMs = this.clock.WallMs(),
            NowMonoMs = 0,
        };
        if (this.ReadCoreState() is { } coreState)
        {
            start.CoreState = ByteString.CopyFrom(coreState);
        }

        lock (this.gate)
        {
            this.startNanos = this.clock.Nanos();
            this.CarryOut(this.layer.Start(start), null);
            this.AtStart = this.inForce;
        }
    }

    public Controls Controls { get; }

    /// <summary>The instance UID, as <c>service.instance.id</c> reads it.</summary>
    public string Uid { get; }

    /// <summary>The settings last put in force (by a load, a decision or a refresh).</summary>
    public Settings InForce
    {
        get
        {
            lock (this.gate)
            {
                return this.inForce;
            }
        }
    }

    /// <summary>When the flow asks the layer again (the end of debug mode), in wall-clock milliseconds; 0: never.</summary>
    public ulong RefreshAt
    {
        get
        {
            lock (this.gate)
            {
                return this.State.RefreshAtUnixMs;
            }
        }
    }

    /// <summary>The flow's state, decoded.</summary>
    internal AgentState State
    {
        get
        {
            lock (this.gate)
            {
                return AgentState.Parser.ParseFrom(this.flow);
            }
        }
    }

    /// <summary>Export health's entry as the flow last sent it (null: not yet).</summary>
    public ComponentHealth? ExportHealth
    {
        get
        {
            lock (this.gate)
            {
                return this.exportHealth;
            }
        }
    }

    /// <summary>The agent's health as last sent with export health in it, by the flow or <see cref="SetHealth"/>.</summary>
    public ComponentHealth? HealthSent { get; private set; }

    /// <summary>The available components reported (the inventory), once started.</summary>
    public AvailableComponents? Components
    {
        get
        {
            lock (this.gate)
            {
                return this.components;
            }
        }
    }

    /// <summary>The settings in force at start, next-start ones included.</summary>
    public Settings AtStart { get; private set; } = new();

    /// <summary>The effective config, as last reported (or to be reported first).</summary>
    public EffectiveConfig Effective
    {
        get
        {
            lock (this.gate)
            {
                return this.effective;
            }
        }
    }

    /// <summary>
    /// One process per state directory. A child process the application starts inherits
    /// OTEL_OPAMP_STATE_DIR; two processes under one UID, reading and writing one state, would be
    /// one agent to the server that is neither. The lock is the operating system's, on a file in the
    /// directory: held while the process runs, never inherited by a child, gone when the process
    /// ends. A process that can't take it runs unmanaged, and says why.
    /// </summary>
    public static FileStream? Lock(string dir, AgentLog log)
    {
        try
        {
            return StateDirs.OpenLock(dir);
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            log.Warn($"the state directory {dir} can't be locked, another process has it (a child inherits " +
                     $"OTEL_OPAMP_STATE_DIR) or it isn't writable: the SDK runs unmanaged. Give each process its own. {e.Message}");
            return null;
        }
    }

    /// <summary>
    /// Once the SDK is built: join the fleet, described by the SDK's resource, with
    /// <paramref name="health"/> (null: healthy) and the <paramref name="inventory"/> of what it
    /// built, reported as its available components (read again by <see cref="Recheck"/>). The client
    /// runs with instrumentation suppressed, so its requests are never the application's spans.
    /// </summary>
    public void Start(IEnumerable<KeyValuePair<string, object>> attributes, Func<Inventory> inventory, ComponentHealth? health = null)
    {
        var resource = attributes.ToList(); // read twice below
        var service = Convert.ToString(resource.FirstOrDefault(a => a.Key == "service.name").Value,
            System.Globalization.CultureInfo.InvariantCulture) ?? string.Empty;
        Host described;
        ulong describedAt;
        ComponentHealth first;
        lock (this.gate)
        {
            // What this SDK turned out to carry out is known now: the flow decides again under it,
            // and a setting it turned out not to carry out is the SDK's own again.
            var now = this.clock.WallMs();
            var built = this.hostNow(true);
            if (!built.Equals(this.host))
            {
                this.host = built;
                this.Step(new AgentStep { Host = built });
            }

            // By the same host and clock: what it lists of the settings is what it carries out.
            this.inventory = inventory;
            var listed = inventory();
            this.exporters = [.. listed.Exporters];
            this.components = this.layer.Components(listed, this.host, now);
            described = this.host;
            describedAt = now;
            // A SetHealth that came while Start was on its way is newer than `health`: keep it.
            if (!this.appSet && health is not null)
            {
                this.app = health;
            }

            // The flow is told the SDK is built: export health's first report goes with the first message.
            this.HealthSent = null;
            this.Step(new AgentStep { Built = new AgentBuilt { Service = service, Exporters = { this.exporters } } });
            this.Step(this.ExportersNow());
            first = this.HealthSent ?? this.Merged();
            this.HealthSent = first;
        }

        var options = new OpAmpClientOptions
        {
            Endpoint = this.server.Endpoint,
            InstanceUid = Uuid.Bytes(this.Uid),
            // What the integration does and reports, declared: the client takes no status field
            // whose bit isn't.
            Capabilities = (ulong)(AgentCapabilities.AcceptsRemoteConfig
                | AgentCapabilities.ReportsRemoteConfig
                | AgentCapabilities.ReportsHeartbeat
                | AgentCapabilities.ReportsHealth
                | AgentCapabilities.ReportsEffectiveConfig
                | AgentCapabilities.ReportsAvailableComponents),
            HeartbeatInterval = this.server.Heartbeat,
            Headers = this.server.Headers,
            StateFile = this.server.StateDir is { } dir ? Path.Combine(dir, "opamp-state") : null,
            // A key of its own, offered by the server: taken with a state directory (StateFile)
            // and kept there; without one the client neither declares nor takes it.
            AcceptConnectionSettings = true,
            // Platform mode, asked for by name: the client's requests go through HttpClient, which
            // is what the suppression below keeps out of the application's traces.
            Mode = OpAmpMode.Platform,
        };
        var status = new OpAmpStatus
        {
            // By the same host and clock: the controls it lists are those it carries out.
            Description = this.layer.Describe(resource.Select(Attribute), described, describedAt),
            Health = first,
            EffectiveConfig = this.effective,
            AvailableComponents = this.components,
        };
        using (SuppressInstrumentationScope.Begin())
        {
            // Notifications come one at a time on the client's loop, never before Start returns,
            // each with the client to answer through (docs/sdk-contract.md).
            var started = OpAmpClient.Start(options, status, this.Notified);
            lock (this.gate)
            {
                this.client = started;
                // A SetHealth while the client was starting found no client to send through, and
                // its caller says nothing again until the health changes: send what it set now.
                // Health goes under the gate (SetHealth only queues), so it goes in the order set.
                if (!ReferenceEquals(this.HealthSent, first) && this.HealthSent is { } late)
                {
                    started.SetHealth(late);
                }
            }
        }
    }

    /// <summary>
    /// The application's health, once the client has started; export health, as last sent, stays
    /// under its <c>component_health_map["exporters"]</c>.
    /// </summary>
    public void SetHealth(ComponentHealth health)
    {
        ComponentHealth merged;
        lock (this.gate)
        {
            this.app = health;
            this.appSet = true;
            merged = this.Merged();
            this.HealthSent = merged;
            this.client?.SetHealth(merged); // under the gate: in the order set (it only queues)
        }
    }

    /// <summary>
    /// The exporters an integration found, as listed, decided by the layer as in every SDK: the
    /// names, and where each sends (its own endpoint, else the <c>OTEL_EXPORTER_*</c> variables).
    /// Decided again only when what is found changes; each warning the layer gives is logged once.
    /// Never throws: should the call fail, each keeps the name last decided, a new one listed as found.
    /// </summary>
    public List<Exporter> Exporters(IEnumerable<Exporters.Built> built, string defaultProtocol)
    {
        var now = built.Select(b => b.Found()).ToList();
        lock (this.decisions)
        {
            if (!now.SequenceEqual(this.found))
            {
                ExportersDecided d;
                try
                {
                    d = this.DecideExporters(now, ExporterVariables(this.getenv), defaultProtocol);
                }
                catch (Exception e)
                {
                    // Reporting only: the gates are in place whatever the list says.
                    this.log.Warn($"can't list the exporters; they are governed all the same: {e.Message}");
                    return [.. now.Select(f => this.listedAs.TryGetValue(f, out var e) ? e : AsFound(f))];
                }

                foreach (var w in d.Warnings.Where(this.warned.Add))
                {
                    this.log.Warn(w);
                }

                this.found = now;
                this.decided = [.. d.Exporters];
                if (d.Exporters.Count == now.Count)
                {
                    foreach (var (f, e) in now.Zip(d.Exporters))
                    {
                        this.listedAs[f] = e;
                    }
                }
            }

            return [.. this.decided];
        }
    }

    /// <summary>
    /// The name each exporter is listed under, as the layer gives it: to tell two views of one
    /// exporter apart. Never throws: should the call fail, each is named as it was found.
    /// </summary>
    public List<string> ExporterNames(IEnumerable<Exporters.Built> built)
    {
        var found = built.Select(b => b.Found()).ToList();
        try
        {
            return [.. this.DecideExporters(found, [], "http/protobuf").Exporters.Select(e => e.Name)];
        }
        catch (Exception)
        {
            // Exporters, called next with the same, says so.
            return [.. found.Select(f => f.Kind)];
        }
    }

    /// <summary>
    /// An exporter listed as it was found, when the layer can't name it: its kind, and no destination
    /// (its endpoint may hold credentials, which only the layer strips).
    /// </summary>
    private static Exporter AsFound(FoundExporter f) =>
        new() { Signal = f.Signal, Name = f.Kind, Gated = f.Gated };

    /// <summary>The variables the layer reads to say where an exporter built from them sends.</summary>
    private static List<KeyValuePair<string, string>> ExporterVariables(Func<string, string?> getenv) =>
        [.. new[] { string.Empty, "TRACES_", "METRICS_", "LOGS_" }
            .SelectMany(s => new[] { $"OTEL_EXPORTER_OTLP_{s}ENDPOINT", $"OTEL_EXPORTER_OTLP_{s}PROTOCOL" })
            .Append("OTEL_EXPORTER_ZIPKIN_ENDPOINT")
            .Select(k => KeyValuePair.Create(k, getenv(k) ?? string.Empty)).Where(kv => kv.Value.Length > 0)];

    /// <summary>
    /// Ask the caller again what the SDK runs, once started: a processor or reader the application
    /// added to a built provider (or a logger provider built since) exports through exporters not
    /// listed before. They are listed, the governed with the gates in front and the rest
    /// ungoverned, and the available components and export health are reported at once; the policy
    /// in force stays as it is. Also asked before each remote config is decided, and on the health timer.
    /// </summary>
    public void Recheck()
    {
        lock (this.gate)
        {
            this.RelistLocked();
        }
    }

    /// <summary>
    /// The host and the exporters now, if started: when the host differs (it can only lose the
    /// sampler's settings), the flow decides again under it; when the exporters differ they are
    /// listed again, the available components sent and the flow told (export health at once).
    /// </summary>
    private void RelistLocked()
    {
        if (this.inventory is not { } listing || this.stopped)
        {
            return;
        }

        var now = this.hostNow(true);
        var listed = listing();
        if (!now.Equals(this.host))
        {
            var lost = this.host.Live.Concat(this.host.NextStart).Except(now.Live.Concat(now.NextStart)).Order(StringComparer.Ordinal).ToList();
            this.host = now;
            if (lost.Count > 0)
            {
                this.log.Warn($"the SDK no longer carries out {string.Join(", ", lost)}: they are set aside");
            }

            this.Step(new AgentStep { Host = now });
        }

        if (!listed.Exporters.SequenceEqual(this.exporters))
        {
            this.exporters = [.. listed.Exporters];
            this.components = this.layer.Components(listed, now, this.clock.WallMs());
            this.client?.SetAvailableComponents(this.components); // under the gate: it only queues
            this.Step(this.ExportersNow());
        }
    }

    /// <summary>The exporters listed and the gates' counts now, as the flow takes them.</summary>
    private AgentStep ExportersNow() => new()
    {
        Exporters = new AgentExporters { Exporters = { this.exporters }, Counts = this.Controls.Tally.Counts() },
    };

    /// <summary>The application's health, with export health's entry as last sent (none yet: none).</summary>
    private ComponentHealth Merged()
    {
        if (this.exportHealth is not { } exporters)
        {
            return this.app;
        }

        var merged = this.app.Clone();
        merged.ComponentHealthMap["exporters"] = exporters;
        return merged;
    }

    /// <summary>
    /// The server hears the agent go (agent_disconnect); then the SDK may flush, and the state
    /// directory is free for the next process. It may run as the process exits: it never throws.
    /// </summary>
    public void Stop(TimeSpan timeout)
    {
        try
        {
            OpAmpClient? c;
            lock (this.gate)
            {
                this.stopped = true;
                this.timer?.Dispose();
                this.timer = null;
                c = this.client;
            }

            if (c is not null)
            {
                using (SuppressInstrumentationScope.Begin())
                {
                    Await(c.StopAsync(), timeout);
                }
            }
        }
        catch (Exception e)
        {
            this.log.Error($"the client did not stop cleanly: {e.Message}");
        }
        finally
        {
            this.stateLock?.Dispose();
        }
    }

    /// <summary>Wait for the client to stop, at most <paramref name="timeout"/>; a client that failed has stopped too.</summary>
    public static void Await(Task stopping, TimeSpan timeout)
    {
        try
        {
            stopping.Wait(timeout);
        }
        catch (AggregateException)
        {
            // It stopped on an error, said already (the Stopped notification).
        }
    }

    /// <summary>
    /// Answer a remote config as the flow decides (sent through <paramref name="to"/>, else the
    /// client, if it runs); the answer for the server.
    /// </summary>
    public RemoteConfigStatus RemoteConfig(AgentRemoteConfig config, OpAmpClient? to = null)
    {
        lock (this.gate)
        {
            // Listed as it is now: a processor added since is reported with the answer.
            this.RelistLocked();
            return this.Step(new AgentStep { RemoteConfig = config }, to) ?? new RemoteConfigStatus
            {
                LastRemoteConfigHash = config.ConfigHash,
                Status = RemoteConfigStatuses.Failed,
                ErrorMessage = "the settings layer gave no answer",
            };
        }
    }

    /// <summary>
    /// Tell the flow what happened (one input) and carry out what it says, sending through
    /// <paramref name="to"/>, else the client if it runs. The remote config status it gave, if any.
    /// Under the gate: what is sent goes in the order the flow said it (the client only queues).
    /// </summary>
    private RemoteConfigStatus? Step(AgentStep input, OpAmpClient? to = null)
    {
        lock (this.gate)
        {
            input.Agent = this.flow;
            input.NowUnixMs = this.clock.WallMs();
            input.NowMonoMs = this.Mono();
            AgentOutput output;
            try
            {
                output = this.layer.Step(input);
            }
            catch (CoreException e)
            {
                this.log.Warn($"the settings layer failed: {e.Message}");
                return null;
            }

            return this.CarryOut(output, to);
        }
    }

    /// <summary>The flow's clock: the monotonic one, in milliseconds since the flow started.</summary>
    private ulong Mono() => (ulong)Math.Max(0, (this.clock.Nanos() - this.startNanos) / 1_000_000);

    /// <summary>The flow's effects, in order, and its timer for the next wake.</summary>
    private RemoteConfigStatus? CarryOut(AgentOutput output, OpAmpClient? to)
    {
        this.flow = output.Agent;
        this.effective = output.Effective;
        var c = to ?? this.client;
        RemoteConfigStatus? status = null;
        foreach (var e in output.Effects)
        {
            switch (e.EffectCase)
            {
                case AgentEffect.EffectOneofCase.Apply:
                    this.Controls.Apply(e.Apply);
                    this.inForce = e.Apply;
                    break;
                case AgentEffect.EffectOneofCase.Effective:
                    c?.SetEffectiveConfig(e.Effective);
                    break;
                case AgentEffect.EffectOneofCase.Status:
                    status = e.Status;
                    c?.SetRemoteConfigStatus(e.Status);
                    break;
                case AgentEffect.EffectOneofCase.Health:
                    // The application's own health, with export health under it.
                    if (e.Health.ComponentHealthMap.TryGetValue("exporters", out var exporters))
                    {
                        this.exportHealth = exporters;
                    }

                    this.HealthSent = this.Merged();
                    c?.SetHealth(this.HealthSent);
                    break;
                case AgentEffect.EffectOneofCase.SaveUid:
                    this.Keep(e.SaveUid);
                    break;
                case AgentEffect.EffectOneofCase.Log:
                    this.Say(e.Log);
                    break;
            }
        }

        this.Arm(output.WakeAtMonoMs);
        return status;
    }

    /// <summary>A line of the flow's, said as the SDK's own.</summary>
    private void Say(LayerLog line)
    {
        switch (line.Level)
        {
            case LayerLog.Types.Level.Debug:
                this.log.Debug?.Invoke(line.Text);
                break;
            case LayerLog.Types.Level.Notice:
                (this.log.Notice ?? this.log.Info)(line.Text);
                break;
            case LayerLog.Types.Level.Warn:
                this.log.Warn(line.Text);
                break;
            case LayerLog.Types.Level.Error:
                this.log.Error(line.Text);
                break;
            default:
                this.log.Info(line.Text);
                break;
        }
    }

    /// <summary>The one timer, for the flow's next wake (on its clock; 0: none), in place of any earlier one.</summary>
    private void Arm(ulong wake)
    {
        this.timer?.Dispose();
        this.timer = null;
        this.generation++;
        this.wakeAt = wake;
        if (this.stopped || wake == 0)
        {
            return;
        }

        var generation = this.generation;
        var now = this.Mono();
        var delay = TimeSpan.FromMilliseconds(Math.Min(wake > now ? wake - now : 0, (ulong)TimeSpan.FromDays(2).TotalMilliseconds));
        this.timer = this.clock.After(delay, () => this.Tick(generation));
    }

    /// <summary>The timer went off: what the SDK runs looked at again, and the flow told, with the gates' counts.</summary>
    private void Tick(int generation)
    {
        try
        {
            lock (this.gate)
            {
                if (this.stopped || generation != this.generation)
                {
                    return; // stale: a step since armed its own
                }

                this.RelistLocked();
                this.Step(new AgentStep { Timer = this.Controls.Tally.Counts() });
            }
        }
        catch (Exception e)
        {
            this.log.Error($"the settings' timer failed: {e.Message}");
        }
    }

    /// <summary>For tests: the timer goes off now (whenever it is armed for).</summary>
    internal void TickForTest()
    {
        int generation;
        lock (this.gate)
        {
            generation = this.generation;
        }

        this.Tick(generation);
    }

    /// <summary>For tests: the SDK built (as Start tells the flow), named <paramref name="service"/>.</summary>
    internal void BuiltForTest(string service)
    {
        lock (this.gate)
        {
            this.Step(new AgentStep { Built = new AgentBuilt { Service = service, Exporters = { this.exporters } } });
            this.Step(this.ExportersNow());
        }
    }

    /// <summary>For tests: when the timer is armed for, on the flow's clock (0: not), and the flow's clock now.</summary>
    internal (ulong WakeAt, ulong Now) TimerForTest
    {
        get
        {
            lock (this.gate)
            {
                return (this.wakeAt, this.Mono());
            }
        }
    }

    /// <summary>For tests: the endpoint what the agent says of reaching its server names now.</summary>
    internal string? EndpointSaid
    {
        get
        {
            var configured = this.State.Endpoint;
            return Volatile.Read(ref this.client)?.Endpoint is { Length: > 0 } inUse ? Server.ShownFor(inUse, configured) : configured;
        }
    }

    /// <summary>A notification, on the client's loop: answered through <paramref name="client"/>.</summary>
    public void Notified(OpAmpClient? client, OpAmpNotification n) => this.Tell(n, client?.Endpoint, client);

    /// <summary>
    /// Tell the flow of a notification; <paramref name="endpoint"/>: the one the client uses now
    /// (null: the configured one). What the flow says to send goes through <paramref name="client"/>.
    /// </summary>
    internal void Tell(OpAmpNotification n, string? endpoint, OpAmpClient? client = null)
    {
        var inUse = endpoint ?? string.Empty;
        switch (n)
        {
            case OpAmpNotification.RemoteConfig rc:
                this.RemoteConfig(rc.Config, client);
                break;
            case OpAmpNotification.Connected:
                this.Step(new AgentStep { Connected = new AgentConnected { Endpoint = inUse } }, client);
                break;
            case OpAmpNotification.ConnectFailed f:
                this.Step(new AgentStep
                {
                    ConnectFailed = new AgentConnectFailed { Endpoint = inUse, Status = (uint)(f.Status ?? 0), Detail = f.Detail ?? string.Empty },
                }, client);
                break;
            case OpAmpNotification.InstanceUidChanged { InstanceUid: var uid }:
                this.Step(new AgentStep { InstanceUidChanged = ByteString.CopyFrom(uid) }, client);
                break;
            case OpAmpNotification.StateFromNewerVersion { Version: var version }:
                this.Step(new AgentStep { StateFromNewerVersion = (uint)version }, client);
                break;
            case OpAmpNotification.Stopped { Error: var e }:
                this.Step(new AgentStep { Stopped = new AgentStopped { Error = e?.Message ?? string.Empty } }, client);
                break;
        }
    }

    /// <summary>
    /// The client's saved state (opamp-state), raw: the settings to start with are the last config
    /// applied, which the client keeps there. Read as the client reads it at start: after a save a
    /// client stopped in this process left writing (a config applied just before a stop) has
    /// landed, or this start would begin from the config before.
    /// </summary>
    private byte[]? ReadCoreState()
    {
        if (this.server.StateDir is not { } dir)
        {
            return null;
        }

        try
        {
            Directory.CreateDirectory(dir);
            return OpAmpClient.ReadState(Path.Combine(dir, "opamp-state")) is { Length: > 0 } kept ? kept : null;
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            this.log.Warn($"the client's saved state unreadable, using the SDK's own settings: {e.Message}");
            return null;
        }
    }

    /// <summary>
    /// A UID the server assigned (or the client's saved state resumed) is the agent's from now on:
    /// the next start names the instance by it. This run's resource is built already.
    /// </summary>
    private void Keep(string uid)
    {
        if (this.server.StateDir is not { } dir)
        {
            return;
        }

        try
        {
            Write(Path.Combine(dir, OpAmpSetup.UidFile), Encoding.ASCII.GetBytes(uid));
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            this.Step(new AgentStep { UidNotSaved = e.Message });
        }
    }

    /// <summary>
    /// The instance UID: the one saved in the state directory; else a new one, saved there; else,
    /// where none can be kept (no state directory, or one it can't be saved in), a UUID
    /// service.instance.id given in OTEL_RESOURCE_ATTRIBUTES, the same at every start; else a new one.
    /// </summary>
    private string Identity()
    {
        var path = this.server.StateDir is { } dir ? Path.Combine(dir, OpAmpSetup.UidFile) : null;
        var saved = string.Empty;
        if (path is not null)
        {
            try
            {
                saved = File.ReadAllText(path);
            }
            catch (Exception e) when (e is IOException or UnauthorizedAccessException)
            {
                // None saved yet.
            }
        }

        SetupIdentified got;
        try
        {
            got = OpAmpSetup.Identify(new SetupIdentity
            {
                StateDir = path is not null,
                Saved = saved,
                ResourceAttributes = this.getenv("OTEL_RESOURCE_ATTRIBUTES") ?? string.Empty,
                Entropy = ByteString.CopyFrom(System.Security.Cryptography.RandomNumberGenerator.GetBytes(16)),
                UnixMs = (ulong)DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(),
            });
        }
        catch (CoreException e)
        {
            this.log.Error($"the core could not make an instance UID: {e.Message}");
            return Uuid.Format(Guid.NewGuid().ToByteArray());
        }

        if (got.Save && path is not null)
        {
            try
            {
                Write(path, Encoding.ASCII.GetBytes(got.Text));
            }
            catch (Exception e) when (e is IOException or UnauthorizedAccessException)
            {
                this.log.Warn($"can't save the instance UID: {e.Message}");
                return Uuid.Format(got.UnsavedUid.ToByteArray());
            }
        }

        return Uuid.Format(got.InstanceUid.ToByteArray());
    }

    /// <summary>Replace the file, all or nothing: on disk before it takes the old one's place.</summary>
    private static void Write(string path, byte[] content)
    {
        var tmp = path + ".tmp";
        using (var f = new FileStream(tmp, FileMode.Create, FileAccess.Write, FileShare.None))
        {
            f.Write(content);
            f.Flush(flushToDisk: true);
        }

        File.Move(tmp, path, overwrite: true);
    }

    private static KeyValue Attribute(KeyValuePair<string, object> a) => new()
    {
        Key = a.Key,
        Value = a.Value switch
        {
            bool b => new AnyValue { BoolValue = b },
            long or int or short or byte => new AnyValue { IntValue = Convert.ToInt64(a.Value) },
            double or float => new AnyValue { DoubleValue = Convert.ToDouble(a.Value) },
            _ => new AnyValue { StringValue = Convert.ToString(a.Value, System.Globalization.CultureInfo.InvariantCulture) ?? string.Empty },
        },
    };
}

/// <summary>UUIDs as OpAMP sends them: the 16 bytes in order (not <c>Guid.ToByteArray()</c>'s, which is mixed-endian).</summary>
internal static class Uuid
{
    public static string Format(byte[] b)
    {
        var h = Convert.ToHexString(b).ToLowerInvariant();
        return $"{h[..8]}-{h[8..12]}-{h[12..16]}-{h[16..20]}-{h[20..]}";
    }

    public static byte[] Bytes(string s) => Convert.FromHexString(s.Replace("-", string.Empty, StringComparison.Ordinal));
}
