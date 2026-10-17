// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

using System.Text;
using System.Text.RegularExpressions;
using Google.Protobuf;
using OpAmp.Proto.V1;
using OpenTelemetry;
using Pydantic.OpAmp.Client;
using Pydantic.OpAmp.Client.Sdk;

namespace Pydantic.OpAmp.Otel;

/// <summary>Where the server is: OTEL_OPAMP_*, or the hosting package's options.</summary>
internal sealed record Server(Uri Endpoint, IReadOnlyDictionary<string, string> Headers, string? StateDir, TimeSpan Heartbeat, SettingNames? Names = null)
{
    /// <summary>What these settings are called, for what the agent says.</summary>
    public SettingNames Named => this.Names ?? SettingNames.Env;

    /// <summary>
    /// Null when OTEL_OPAMP_ENDPOINT is unset: nothing is managed. A heartbeat interval it can't read
    /// is the client's default, with a warning; an endpoint that isn't a URL throws.
    /// </summary>
    public static Server? FromEnv(Func<string, string?> getenv, Action<string> warn)
    {
        var endpoint = getenv("OTEL_OPAMP_ENDPOINT")?.Trim();
        if (string.IsNullOrEmpty(endpoint))
        {
            return null;
        }

        var stateDir = getenv("OTEL_OPAMP_STATE_DIR")?.Trim();
        var heartbeat = getenv("OTEL_OPAMP_HEARTBEAT_INTERVAL")?.Trim();
        return new Server(
            new Uri(endpoint),
            Variables.Headers(getenv("OTEL_OPAMP_HEADERS") ?? string.Empty),
            string.IsNullOrEmpty(stateDir) ? null : stateDir,
            string.IsNullOrEmpty(heartbeat) ? TimeSpan.Zero : Variables.Interval("OTEL_OPAMP_HEARTBEAT_INTERVAL", heartbeat, warn) ?? TimeSpan.Zero);
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
internal sealed record AgentLog(Action<string> Info, Action<string> Warn, Action<string> Error, Action<string>? Debug = null);

/// <summary>
/// The SDK under the server, whatever set the SDK up (the plugin, or the hosting package): the
/// instance UID and the settings kept in the state directory, the SDK settings layer's decisions
/// carried out on <see cref="Controls"/>, and the client. What this SDK carries out is the
/// caller's (<c>host</c>, asked again once the providers are built).
/// </summary>
internal sealed class SdkAgent
{
    /// <summary>How often the layer is asked about export health.</summary>
    internal static readonly TimeSpan HealthEvery = TimeSpan.FromSeconds(30);

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

    // The one timer (refresh and health), and what it waits for: the end of debug mode as a
    // monotonic deadline (refreshAt 0: none), and the next health report (once started). A
    // Decide after a timer was armed makes its refresh stale: the generation says so.
    private IDisposable? timer;
    private long refreshDeadline;
    private ulong refreshAt;
    private long? nextHealth;
    private int generation;
    private bool stopped;

    // Export health: the application's own health, and the layer's last report.
    private ComponentHealth app = new() { Healthy = true };
    private HealthReport? lastHealth;
    private Loaded atStart;
    private ByteString current;
    private EffectiveConfig effective;
    private OpAmpClient? client;
    private Reachability? reachability;
    private AvailableComponents? components;

    // Once started: what the SDK runs, as the caller reads it now, for the available components.
    // A host that narrows (Recheck) lists them again.
    private Func<Inventory>? inventory;
    private AvailableComponents? unsent;

    /// <summary>
    /// Load the saved settings and put them in force on <paramref name="controls"/>; name the instance.
    /// <paramref name="hostNow"/> says what the SDK carries out (its argument: whether the providers are built).
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
        if (!server.Endpoint.OriginalString.Contains("://", StringComparison.Ordinal))
        {
            log.Warn($"{server.Named.Endpoint} '{Reachability.Where(server.Endpoint)}' has no scheme: name it in full, " +
                     "as https://host/v1/opamp or wss://host/v1/opamp");
        }

        // What the application's own settings say goes where the agent says the rest.
        controls.App.Info = log.Info;
        controls.App.Error = log.Error;
        this.layer = new SdkSettings();
        this.host = hostNow(false);
        var now = this.clock.WallMs();
        this.atStart = this.layer.Load(this.ReadSaved(), this.host, now, this.ReadCoreState());
        if (this.atStart.Warning.Length > 0)
        {
            log.Warn(this.atStart.Warning);
        }

        this.current = this.atStart.Current;
        this.effective = this.atStart.Effective;
        this.Put(this.atStart.Settings);
        this.Uid = this.Identity();
        lock (this.gate)
        {
            this.Arm(this.atStart.RefreshAtUnixMs, now);
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

    /// <summary>When the layer said to ask again (the end of debug mode), in wall-clock milliseconds; 0: never.</summary>
    public ulong RefreshAt
    {
        get
        {
            lock (this.gate)
            {
                return this.refreshAt;
            }
        }
    }

    /// <summary>The layer's last export health report (null: not asked yet).</summary>
    public HealthReport? LastHealth
    {
        get
        {
            lock (this.gate)
            {
                return this.lastHealth;
            }
        }
    }

    /// <summary>The agent's health as last sent with export health in it, by the timer or <see cref="SetHealth"/>.</summary>
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

    /// <summary>The settings in force at start.</summary>
    public Settings AtStart => this.atStart.Settings;

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
    /// built, reported as its available components (read again if the host narrows). The client
    /// runs with instrumentation suppressed, so its requests are never the application's spans.
    /// </summary>
    public void Start(IEnumerable<KeyValuePair<string, object>> attributes, Func<Inventory> inventory, ComponentHealth? health = null)
    {
        var resource = attributes.ToList(); // read twice below
        lock (this.gate)
        {
            // What this SDK turned out to carry out is known now: answer and report by it.
            this.host = this.hostNow(true);
            var now = this.clock.WallMs();
            var loaded = this.layer.Load(this.ReadSaved(), this.host, now, this.ReadCoreState());
            this.atStart = loaded;
            this.current = loaded.Current;
            this.effective = loaded.Effective;
            // A setting it turned out not to carry out is the SDK's own again.
            this.Put(loaded.Settings);
            this.Arm(loaded.RefreshAtUnixMs, now);
            // By the same host and clock: what it lists of the settings is what it carries out. It
            // changes only if the host narrows (Recheck).
            this.inventory = inventory;
            this.components = this.layer.Components(inventory(), this.host, now);
        }

        var options = new OpAmpClientOptions
        {
            Endpoint = this.server.Endpoint,
            InstanceUid = Uuid.Bytes(this.Uid),
            Capabilities = (ulong)AgentCapabilities.AcceptsRemoteConfig,
            HeartbeatInterval = this.atStart.Settings.HasOpampPollingIntervalMs
                ? TimeSpan.FromMilliseconds(this.atStart.Settings.OpampPollingIntervalMs)
                : this.server.Heartbeat,
            Headers = this.server.Headers,
            StateFile = this.server.StateDir is { } dir ? Path.Combine(dir, "opamp-state") : null,
            // A key of its own, offered by the server: taken with a state directory (StateFile)
            // and kept there; without one the client neither declares nor takes it.
            AcceptConnectionSettings = true,
            // .NET mode, asked for by name: the client's requests go through HttpClient, which is
            // what the suppression below keeps out of the application's traces.
            Mode = OpAmpMode.DotNet,
        };
        ComponentHealth first;
        lock (this.gate)
        {
            // Export health's first report goes with the first message: no second one at once.
            this.app = health ?? new ComponentHealth { Healthy = true };
            first = this.HealthLocked() ?? this.app;
            this.HealthSent = first;
        }

        var status = new OpAmpStatus
        {
            Description = this.layer.Describe(resource.Select(Attribute)),
            Health = first,
            EffectiveConfig = this.effective,
            AvailableComponents = this.components,
        };
        var service = resource.FirstOrDefault(a => a.Key == "service.name").Value;
        this.reachability = new Reachability(this.server.Endpoint, this.server.Named,
            Convert.ToString(service, System.Globalization.CultureInfo.InvariantCulture) ?? string.Empty, this.Uid, this.log)
        {
            InUse = () => Volatile.Read(ref this.client)?.Endpoint,
        };
        using (SuppressInstrumentationScope.Begin())
        {
            // Notifications come one at a time on the client's loop, never before Start returns,
            // each with the client to answer through (docs/sdk-contract.md).
            var started = OpAmpClient.Start(options, status, this.Notified);
            lock (this.gate)
            {
                this.client = started;
                // Export health: every 30 s from now, on the settings' timer.
                this.nextHealth = this.clock.Nanos() + (HealthEvery.Ticks * 100);
                this.Rearm();
            }
        }
    }

    /// <summary>
    /// The application's health, once the client has started; export health, as last sent, stays
    /// under its <c>component_health_map["exporters"]</c>.
    /// </summary>
    public void SetHealth(ComponentHealth health)
    {
        OpAmpClient? c;
        ComponentHealth merged;
        lock (this.gate)
        {
            this.app = health;
            merged = this.Merged();
            c = this.client;
            this.HealthSent = merged;
        }

        c?.SetHealth(merged);
    }

    /// <summary>
    /// Ask the caller again what the SDK carries out, once started: a processor or reader the
    /// application added to a built provider exports past the gates, and the settings it takes
    /// from them are set aside from then on. When the host narrows, what is in force is decided
    /// again (the layer's refresh), and the effective config and the available components are
    /// reported. Also asked before each remote config is decided, and on the health timer.
    /// </summary>
    public void Recheck()
    {
        EffectiveConfig? report = null;
        AvailableComponents? listed = null;
        OpAmpClient? c;
        lock (this.gate)
        {
            if (this.RehostLocked(this.clock.WallMs()))
            {
                report = this.effective;
                listed = this.unsent;
                this.unsent = null;
            }

            c = this.client;
        }

        if (c is not null && report is not null)
        {
            c.SetEffectiveConfig(report);
            if (listed is not null)
            {
                c.SetAvailableComponents(listed);
            }
        }
    }

    /// <summary>
    /// The host now, if started (before, the host is the one to start with): when it differs, the
    /// settings in force are what the layer decides under it, and the components are listed
    /// again. Whether it changed.
    /// </summary>
    private bool RehostLocked(ulong nowUnixMs)
    {
        if (this.inventory is not { } listing || this.stopped)
        {
            return false;
        }

        var now = this.hostNow(true);
        if (now.Equals(this.host))
        {
            return false;
        }

        var lost = this.host.Live.Concat(this.host.NextStart).Except(now.Live.Concat(now.NextStart)).Order(StringComparer.Ordinal).ToList();
        this.host = now;
        if (lost.Count > 0)
        {
            this.log.Warn($"the SDK now exports past the gates (an exporter, processor or reader that isn't the gated OTLP one): {string.Join(", ", lost)} can't be carried out and are set aside");
        }

        this.RefreshLocked(nowUnixMs);
        this.Rearm();
        this.unsent = this.components = this.layer.Components(listing(), now, nowUnixMs);
        return true;
    }

    /// <summary>The application's health, with export health's entry as last sent (none yet: none).</summary>
    private ComponentHealth Merged()
    {
        if (this.lastHealth?.Exporters is not { } exporters)
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
                this.nextHealth = null;
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

    /// <summary>Carry out the layer's decision on a remote config; the answer for the server.</summary>
    public RemoteConfigStatus RemoteConfig(AgentRemoteConfig config)
    {
        lock (this.gate)
        {
            // With a state directory the client keeps the config this applies, in its own state, and
            // tells the server APPLIED once that is saved: nothing for the agent to save. Without one,
            // nothing is kept, and a next-start change is refused.
            var now = this.clock.WallMs();
            // Decided under the host as it is now: a processor added since may have narrowed it.
            this.RehostLocked(now);
            Action<byte[]>? save = this.server.StateDir == null ? null : _ => { };
            var d = this.layer.Decide(config, this.current, this.atStart.Current, this.host, now, save);
            if (d.Apply is { } apply)
            {
                this.Put(apply);
                if (apply.Unsupported.Count > 0)
                {
                    this.log.Info($"settings this SDK doesn't carry out: {string.Join(", ", apply.Unsupported)}");
                }

                if (apply.Ignored.Count > 0)
                {
                    this.log.Info($"settings unknown to this SDK: {string.Join(", ", apply.Ignored)}");
                }
            }

            if (d.PendingRestart.Count > 0)
            {
                this.log.Info($"{string.Join(", ", d.PendingRestart)} changed; in force when the application restarts");
            }

            this.current = d.Current;
            this.effective = d.Effective;
            if (d.Apply is null && this.refreshAt != 0 && now >= this.refreshAt)
            {
                // FAILED, so nothing changes, but what is reported is now's: once the wall clock
                // has passed the end of debug mode (a machine that slept, before the timer went
                // off), it ends here, as the report says.
                this.generation++;
                this.RefreshLocked(now);
                this.Rearm();
            }
            else
            {
                // A new window, or none: the timer for it replaces any earlier one.
                this.Arm(d.RefreshAtUnixMs, now);
            }

            return d.Status;
        }
    }

    /// <summary>
    /// What is in force at <paramref name="nowUnixMs"/> (the end of debug mode): put in force, and
    /// reported to the server if the client runs. What the timer calls; tests too.
    /// </summary>
    public void Refresh(ulong nowUnixMs)
    {
        EffectiveConfig report;
        OpAmpClient? c;
        lock (this.gate)
        {
            report = this.RefreshLocked(nowUnixMs);
            this.Rearm();
            c = this.client;
        }

        c?.SetEffectiveConfig(report);
    }

    private EffectiveConfig RefreshLocked(ulong nowUnixMs)
    {
        var r = this.layer.Refresh(this.current, this.atStart.Current, nowUnixMs, this.host);
        if (r.Warning.Length > 0)
        {
            this.log.Warn(r.Warning);
        }

        this.Put(r.Settings);
        this.effective = r.Effective;
        this.SetRefresh(r.RefreshAtUnixMs, nowUnixMs);
        return r.Effective;
    }

    /// <summary>
    /// Export health now, from the gates' counts: the agent's health to send (the application's,
    /// with the layer's <c>exporters</c> entry), or null when the layer says not to send.
    /// </summary>
    private ComponentHealth? HealthLocked()
    {
        var report = this.layer.Health(this.Controls.Tally.Counts(), this.lastHealth, this.clock.WallMs(), this.inForce);
        // Passed back next time, sent or not: when not, its entry is the one last sent.
        this.lastHealth = report;
        return report.Send ? this.Merged() : null;
    }

    private void Put(Settings settings)
    {
        this.Controls.Apply(settings);
        lock (this.gate)
        {
            this.inForce = settings;
        }
    }

    /// <summary>
    /// The layer said to ask again at <paramref name="refreshAtUnixMs"/> (0: never), at wall time
    /// <paramref name="nowUnixMs"/>: arm the timer for it, in place of any earlier one, whose
    /// refresh is stale from now on. The remaining time becomes a monotonic deadline: the wall
    /// clock jumping while the process runs doesn't move it.
    /// </summary>
    private void Arm(ulong refreshAtUnixMs, ulong nowUnixMs)
    {
        this.generation++;
        this.SetRefresh(refreshAtUnixMs, nowUnixMs);
        this.Rearm();
    }

    private void SetRefresh(ulong refreshAtUnixMs, ulong nowUnixMs)
    {
        this.refreshAt = refreshAtUnixMs;
        this.refreshDeadline = refreshAtUnixMs == 0
            ? 0
            : this.clock.Nanos() + (long)Math.Min(refreshAtUnixMs > nowUnixMs ? refreshAtUnixMs - nowUnixMs : 0, (ulong)TimeSpan.FromDays(2).TotalMilliseconds) * 1_000_000;
    }

    /// <summary>The one timer, for whichever comes first: the refresh, or the next health report.</summary>
    private void Rearm()
    {
        this.timer?.Dispose();
        this.timer = null;
        long? due = this.refreshAt == 0 ? this.nextHealth
            : this.nextHealth is not { } health ? this.refreshDeadline
            : Math.Min(this.refreshDeadline, health);
        if (this.stopped || due is not { } at)
        {
            return;
        }

        var generation = this.generation;
        var delay = TimeSpan.FromTicks(Math.Max(0, at - this.clock.Nanos()) / 100);
        this.timer = this.clock.After(delay, () => this.Tick(generation));
    }

    /// <summary>The timer went off: the refresh, if it is due and no Decide came since; health, if due.</summary>
    private void Tick(int generation)
    {
        try
        {
            EffectiveConfig? report = null;
            AvailableComponents? listed = null;
            ComponentHealth? health = null;
            OpAmpClient? c;
            lock (this.gate)
            {
                if (this.stopped || generation != this.generation)
                {
                    return; // stale: a Decide since armed its own
                }

                var now = this.clock.Nanos();
                if (this.nextHealth is { } due && now >= due && this.RehostLocked(this.clock.WallMs()))
                {
                    // With the health report: the host narrowed since the last decision.
                    report = this.effective;
                    listed = this.unsent;
                    this.unsent = null;
                }

                if (this.refreshAt != 0 && now >= this.refreshDeadline)
                {
                    // On the monotonic deadline, the window is over whatever the wall clock says.
                    report = this.RefreshLocked(Math.Max(this.clock.WallMs(), this.refreshAt));
                }

                if (this.nextHealth is { } next && now >= next)
                {
                    health = this.HealthLocked();
                    this.nextHealth = now + (HealthEvery.Ticks * 100);
                }

                this.Rearm();
                c = this.client;
            }

            if (report is not null)
            {
                c?.SetEffectiveConfig(report);
            }

            if (listed is not null)
            {
                c?.SetAvailableComponents(listed);
            }

            if (health is not null)
            {
                this.HealthSent = health;
                c?.SetHealth(health);
            }
        }
        catch (Exception e)
        {
            this.log.Error($"the settings' timer failed: {e.Message}");
        }
    }

    /// <summary>For tests: the endpoint what the agent says of reaching its server names now.</summary>
    internal string? EndpointSaid => this.reachability?.At();

    /// <summary>A notification, on the client's loop: answered through <paramref name="client"/>.</summary>
    public void Notified(OpAmpClient client, OpAmpNotification n)
    {
        this.reachability?.Notified(n);
        switch (n)
        {
            case OpAmpNotification.RemoteConfig rc:
                var status = this.RemoteConfig(rc.Config);
                EffectiveConfig eff;
                AvailableComponents? listed;
                lock (this.gate)
                {
                    eff = this.effective;
                    listed = this.unsent;
                    this.unsent = null;
                }

                client.SetEffectiveConfig(eff);
                if (listed is not null)
                {
                    client.SetAvailableComponents(listed);
                }

                client.SetRemoteConfigStatus(status);
                break;
            case OpAmpNotification.InstanceUidChanged { InstanceUid: var uid }:
                this.Keep(uid);
                break;
            case OpAmpNotification.Stopped { Error: { } e }:
                this.log.Error($"the client stopped; the SDK runs unmanaged: {e.Message}");
                break;
        }
    }

    /// <summary>
    /// What this agent kept itself before the client kept the config applied (sdk-settings.json):
    /// read only while the client's state holds no config applied.
    /// </summary>
    private byte[]? ReadSaved() => this.ReadStateFile("sdk-settings.json");

    /// <summary>
    /// The client's saved state (opamp-state), raw: the settings to start with are the last config
    /// applied, which the client keeps there. Read as the client reads it at start: after a save a
    /// client stopped in this process left writing (a config applied just before a stop) has
    /// landed, or this start would begin from the config before.
    /// </summary>
    private byte[]? ReadCoreState() => this.ReadStateFile("opamp-state", OpAmpClient.ReadState);

    private byte[]? ReadStateFile(string name, Func<string, byte[]>? read = null)
    {
        if (this.server.StateDir is not { } dir)
        {
            return null;
        }

        try
        {
            Directory.CreateDirectory(dir);
            var path = Path.Combine(dir, name);
            if (read is not null)
            {
                return read(path) is { Length: > 0 } kept ? kept : null;
            }

            return File.Exists(path) ? File.ReadAllBytes(path) : null;
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            this.log.Warn($"saved SDK settings unreadable, using the SDK's own: {e.Message}");
            return null;
        }
    }

    /// <summary>
    /// A UID the server assigned (or the client's saved state resumed) is the agent's from now on:
    /// the next start names the instance by it. This run's resource is built already.
    /// </summary>
    private void Keep(byte[] uid)
    {
        if (this.server.StateDir is not { } dir || uid.Length != 16)
        {
            return;
        }

        try
        {
            Write(Path.Combine(dir, "instance-uid"), Encoding.ASCII.GetBytes(Uuid.Format(uid)));
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            this.log.Warn($"can't save the instance UID the server assigned: {e.Message}");
        }
    }

    /// <summary>
    /// The instance UID: the one saved in the state directory; else a new one, saved there; else,
    /// where none can be kept (no state directory, or one it can't be saved in), a UUID
    /// service.instance.id given in OTEL_RESOURCE_ATTRIBUTES, the same at every start; else a new one.
    /// </summary>
    private string Identity()
    {
        if (this.server.StateDir is { } dir)
        {
            var path = Path.Combine(dir, "instance-uid");
            try
            {
                var saved = File.ReadAllText(path).Trim();
                if (Uuid.Valid(saved))
                {
                    return saved.ToLowerInvariant();
                }
            }
            catch (Exception e) when (e is IOException or UnauthorizedAccessException)
            {
                // None saved yet.
            }

            var made = Uuid.Format(Uuid.NewV7());
            try
            {
                Write(path, Encoding.ASCII.GetBytes(made));
                return made;
            }
            catch (Exception e) when (e is IOException or UnauthorizedAccessException)
            {
                this.log.Warn($"can't save the instance UID: {e.Message}");
            }
        }

        return Uuid.Chosen(this.getenv("OTEL_RESOURCE_ATTRIBUTES")) ?? Uuid.Format(Uuid.NewV7());
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
internal static partial class Uuid
{
    public static bool Valid(string s) => Pattern().IsMatch(s);

    /// <summary>
    /// The <c>service.instance.id</c> that <paramref name="resourceAttributes"/> (as
    /// OTEL_RESOURCE_ATTRIBUTES) gives, if it is a UUID: the instance UID the application chose.
    /// </summary>
    public static string? Chosen(string? resourceAttributes)
    {
        foreach (var kv in (resourceAttributes ?? string.Empty).Split(','))
        {
            var eq = kv.IndexOf('=');
            if (eq > 0 && kv[..eq].Trim() == "service.instance.id" && Uri.UnescapeDataString(kv[(eq + 1)..].Trim()) is var id && Valid(id))
            {
                return id.ToLowerInvariant();
            }
        }

        return null;
    }

    /// <summary>A UUIDv7: the time in milliseconds, then random bits.</summary>
    public static byte[] NewV7()
    {
        var uid = System.Security.Cryptography.RandomNumberGenerator.GetBytes(16);
        var ms = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
        for (var i = 5; i >= 0; i--, ms >>= 8)
        {
            uid[i] = (byte)ms;
        }

        uid[6] = (byte)(0x70 | (uid[6] & 0x0f));
        uid[8] = (byte)(0x80 | (uid[8] & 0x3f));
        return uid;
    }

    public static string Format(byte[] b)
    {
        var h = Convert.ToHexString(b).ToLowerInvariant();
        return $"{h[..8]}-{h[8..12]}-{h[12..16]}-{h[16..20]}-{h[20..]}";
    }

    public static byte[] Bytes(string s) => Convert.FromHexString(s.Replace("-", string.Empty, StringComparison.Ordinal));

    [GeneratedRegex("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")]
    private static partial Regex Pattern();
}
