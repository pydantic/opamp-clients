// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.logfire.fleet.javaagent;

import com.google.protobuf.ByteString;
import dev.pydantic.logfire.fleet.Notification;
import dev.pydantic.logfire.fleet.OpampClient;
import dev.pydantic.logfire.fleet.core.CoreProto;
import dev.pydantic.logfire.fleet.sdk.SdkSettings;
import dev.pydantic.logfire.fleet.sdk.SettingsProto;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.impl.InstrumentationUtil;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import io.opentelemetry.sdk.resources.Resource;
import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.channels.FileChannel;
import java.nio.channels.FileLock;
import java.nio.channels.OverlappingFileLockException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.nio.file.StandardOpenOption;
import java.nio.file.attribute.FileAttribute;
import java.nio.file.attribute.PosixFilePermissions;
import java.security.SecureRandom;
import java.time.Duration;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.CompletableFuture;
import java.util.logging.Level;
import java.util.logging.Logger;
import opamp.proto.v1.Anyvalue;
import opamp.proto.v1.Opamp;

/**
 * The extension's state. The agent builds it through the SPI classes, in this order: the
 * configuration properties ({@link #properties}), the resource ({@link #resource}), the sampler and
 * exporters ({@link #controls}, each exporter listed in the {@link #inventory}), and then, with the
 * SDK built, {@link #start}. Under declarative configuration the same steps are taken from the
 * configuration model ({@link Declarative}).
 *
 * <p>What the settings mean, what to answer and what to report is the SDK settings layer's
 * ({@link SdkSettings}), the same in every language; this class carries out its decisions on the
 * Java agent.
 */
final class Extension {
  static final Extension INSTANCE = new Extension();
  private static final Logger LOG = Logger.getLogger(Extension.class.getName());
  private static final AttributeKey<String> INSTANCE_ID = AttributeKey.stringKey("service.instance.id");
  private static final AttributeKey<String> SERVICE_NAME = AttributeKey.stringKey("service.name");

  /**
   * The settings this integration carries out at once, and at the next start, declared to the
   * layer (docs/sdk-settings.md#the-host). The layer refuses a policy that overrides one left out,
   * so the agent never answers APPLIED for one it does not carry out, and the description lists only
   * the controls declared. Instrumentations are switched as the agent starts, by its properties.
   */
  static final List<String> LIVE = List.of("sampling_rate", "sampling_rules", "sampling_traces_per_second",
      "debug_until", "debug_sampling_rate", "send_traces", "send_metrics", "send_logs", "send_logs_level",
      "send_logs_level_by_scope", "send_logs_per_second", "debug_send_logs_level", "drop_attributes",
      "mask_attributes", "scrub_defaults", "scrub_patterns");
  static final List<String> NEXT_START = List.of("deactivate_instrumentations", "deactivate_all_instrumentations");
  /** What the extension's sampler holds: carried out only where the SDK's sampler is the extension's. */
  static final List<String> SAMPLED = List.of("sampling_rate", "sampling_rules", "sampling_traces_per_second",
      "debug_sampling_rate");
  /**
   * Every setting. The policy governs every exporter the agent builds; one it can't reach (another
   * extension's processor or reader, a component of the configuration file's own) is reported
   * ungoverned in the inventory and export health, and never narrows what is declared.
   */
  static final SettingsProto.Host HOST = host(false);

  /**
   * The host: every setting but, with {@code samplerOutside}, those the extension's sampler holds,
   * as the SDK samples with another extension's sampler.
   */
  static SettingsProto.Host host(boolean samplerOutside) {
    return SettingsProto.Host.newBuilder()
        .addAllLive(LIVE.stream().filter(k -> !samplerOutside || !SAMPLED.contains(k)).toList())
        .addAllNextStart(NEXT_START).build();
  }

  /**
   * The application's own settings, {@code custom}, the policy's app_settings control
   * (docs/app-settings.md): declared where the
   * application can read them, that is when the bridge to {@code dev.pydantic.logfire.fleet.AppSettings} is
   * in place ({@link Controls#app}).
   */
  static SettingsProto.Host withApp(SettingsProto.Host host) {
    return host.toBuilder().addLive("custom").build();
  }

  /** {@link #HOST} with {@code custom}. */
  static final SettingsProto.Host HOST_WITH_APP = withApp(HOST);

  /**
   * Why the client core's and the SDK settings layer's modules can't be loaded here, or null when
   * they can (a test puts its own in place).
   */
  java.util.function.Supplier<String> coreUnavailable = dev.pydantic.logfire.fleet.Setup::unavailable;
  java.util.function.Supplier<String> layerUnavailable = SdkSettings::unavailable;
  /** Whether a core that can't be loaded has been said: once a JVM. */
  private boolean coreSaid;

  /** Puts the bridge to the application's {@code AppSettings} in place: null when it can't. */
  java.util.function.Supplier<AppSettingsSink> appSettings = AppSettingsSink::install;
  /** The application's own settings in force before {@link #properties}: what a failed setup puts back. */
  private Map<String, String> appBefore;

  final Controls controls = new Controls();
  /** What another extension added to the SDK past the gates, under autoconfigure. */
  final Outside outside = new Outside();
  /** What the agent runs, as the exporters were built: reported at start, as available components. */
  final Inventory inventory = new Inventory();

  {
    // The listed exporters are decided by the extension's own layer.
    inventory.layer = this::layer;
  }
  private volatile Opamp.AvailableComponents components;
  /** The wall clock, in milliseconds since the Unix epoch: what debug mode is decided by. */
  java.util.function.LongSupplier wallClock = System::currentTimeMillis;
  /**
   * Made when the extension is on, as the settings below (set by {@link #properties}): the module
   * and the protobuf types take a moment to load, too long for every JVM the jar is given to.
   */
  private SdkSettings layer;
  private Setup setup;
  /** Whether the agent ran either of the extension's SDK hooks (autoconfigure or declarative). */
  private boolean hooked;
  /**
   * Whether the state directory can be written: without that, the instance UID is made afresh each
   * start. The client keeps the config applied wherever there is a state directory, and tells the
   * server APPLIED once that is saved, as in every language.
   */
  private boolean canSave;
  /**
   * What the agent carries out ({@link #HOST}, without the sampler's settings once {@link #built}
   * finds another sampler); {@link #host} adds {@code custom}.
   */
  private SettingsProto.Host declared = HOST;
  /** Whether the SDK samples with a sampler other than the extension's. */
  private boolean samplerOutside;
  /**
   * The agent flow's state, as the layer handed it over (null before {@link #properties}): kept
   * here, passed back with each step. The flow decides what is in force, what to answer and report,
   * what to say, and when to step its timer.
   */
  private ByteString flow;
  /** The client's saved state read at start: the flow starts again from it once the UID is known. */
  private byte[] coreState;
  /** The settings in force at start, next-start ones included. */
  private SettingsProto.Settings startSettings = SettingsProto.Settings.getDefaultInstance();
  /** The live settings in force: the last the flow handed over. */
  private volatile SettingsProto.Settings inForce = SettingsProto.Settings.getDefaultInstance();
  /** The monotonic clock, in milliseconds: what the flow's timer is measured on. */
  java.util.function.LongSupplier monoClock = () -> System.nanoTime() / 1_000_000;
  /** The monotonic clock's reading when the flow started: the flow's clock counts from it. */
  private long base;
  /**
   * The one timer, for the flow's next wake, and the generation it was armed in: a step after it
   * arms its own, and the older one does nothing.
   */
  private java.util.concurrent.ScheduledExecutorService timer;
  private java.util.concurrent.ScheduledFuture<?> armed;
  /** For tests on their own clock: the timer is armed but never scheduled (see clockForTest). */
  private boolean timerHeld;
  private long generation;
  /** When the timer is armed for, on the flow's clock (0: not). */
  private long wakeAt;
  /** The agent health last sent. */
  private volatile Opamp.ComponentHealth health = Opamp.ComponentHealth.newBuilder().setHealthy(true).build();
  private volatile Opamp.EffectiveConfig effective;
  private UUID uid;
  private Resource resource;
  private final CompletableFuture<OpampClient> client = new CompletableFuture<>();

  /**
   * Where the server is, from otel.opamp.*, as the core's Setup decides it; null when the extension
   * is off. {@code stateLock} is the lock on the default state directory, held while the JVM runs
   * (null for a named one). {@code shown} is the endpoint as it is safe to log; {@code hints}, what
   * to check when the server can't be reached.
   */
  record Setup(String endpoint, Map<String, String> headers, Path stateDir, Duration heartbeat, FileLock stateLock,
      String shown, CoreProto.SetupHints hints) {
    /** Tests: a setup with the endpoint as the core shows it, and its hints. */
    Setup(String endpoint, Map<String, String> headers, Path stateDir, Duration heartbeat) {
      this(endpoint, headers, stateDir, heartbeat, null, Extension.shown(endpoint, endpoint), hintsFor(endpoint));
    }

    static Setup from(ConfigProperties config) {
      return from(config, System.getenv(), System.getProperty("sun.java.command"));
    }

    /**
     * {@code env} for the user's state directory; {@code command} as sun.java.command, for the
     * program. The rules are the core's (its Setup call), the same in every language; what is here
     * is the I/O: reading the properties, taking a slot of the default state directory, and saying
     * the warnings. A setting it can't use is never an error: the default, with a warning.
     */
    static Setup from(ConfigProperties config, Map<String, String> env, String command) {
      CoreProto.SetupStart.Builder start = CoreProto.SetupStart.newBuilder()
          .setOs(os())
          .setProgram(programName(command))
          .setCwd(System.getProperty("user.dir", ""));
      for (Map.Entry<String, String> v : PROPERTIES.entrySet()) {
        String value;
        try {
          value = config.getString(v.getValue());
        } catch (RuntimeException e) {
          value = null; // one it can't read: as if not given
        }
        variable(start, v.getKey(), value);
      }
      for (String name : List.of("HOME", "XDG_STATE_HOME", "LOCALAPPDATA")) {
        variable(start, name, env.get(name));
      }
      CoreProto.SetupStarted s;
      try {
        s = dev.pydantic.logfire.fleet.Setup.start(start.build());
      } catch (dev.pydantic.logfire.fleet.Setup.Failed e) {
        LOG.log(Level.SEVERE, "OpAMP: the core could not read the settings; the SDK runs unmanaged", e);
        return null;
      }
      for (String warning : s.getWarningsList()) {
        LOG.warning("OpAMP: " + named(warning, false));
      }
      if (s.getOff()) {
        // Turned off by a property, or an endpoint without a scheme refused (said once, here); off for
        // want of an endpoint says nothing.
        if (!s.getOffReason().isEmpty()) {
          LOG.warning("OpAMP: " + named(s.getOffReason(), false));
        }
        return null;
      }
      Map<String, String> headers = new LinkedHashMap<>();
      for (CoreProto.Header h : s.getHeadersList()) {
        headers.put(h.getName(), h.getValue());
      }
      // A directory named is locked as the default one is, so processes sharing it never join as
      // one instance.
      String named = s.getStateDir();
      Claimed claimed = named.isEmpty()
          ? slot(s.getDefaultSlotsList(), s.getAllSlotsTaken(), null)
          : slot(namedSlots(named), SLOTS + " processes already keep their identity in " + named
              + ": this one is a new instance each start", named);
      Path stateDir = claimed == null ? null : claimed.dir();
      FileLock lock = claimed == null ? null : claimed.lock();
      return new Setup(s.getEndpoint(), headers, stateDir, Duration.ofMillis(s.getHeartbeatIntervalMs()), lock,
          s.getShownEndpoint(), Extension.hints(s.getHints()));
    }

    private static void variable(CoreProto.SetupStart.Builder start, String name, String value) {
      if (value != null && !value.isEmpty()) {
        start.addEnv(CoreProto.Variable.newBuilder().setName(name).setValue(value));
      }
    }
  }

  /**
   * The properties the core's Setup reads, by the variable it reads each as. A property is given as
   * a system property or as its variable (the agent's ConfigProperties reads both).
   */
  private static final Map<String, String> PROPERTIES = orderedMap(
      "OTEL_OPAMP_DISABLED", "otel.opamp.disabled",
      "OTEL_SDK_DISABLED", "otel.sdk.disabled",
      "OTEL_OPAMP_ENDPOINT", "otel.opamp.endpoint",
      "OTEL_OPAMP_HEADERS", "otel.opamp.headers",
      "OTEL_OPAMP_STATE_DIR", "otel.opamp.state.dir",
      "OTEL_OPAMP_HEARTBEAT_INTERVAL", "otel.opamp.heartbeat.interval",
      "OTEL_SERVICE_NAME", "otel.service.name",
      "OTEL_RESOURCE_ATTRIBUTES", "otel.resource.attributes");

  private static Map<String, String> orderedMap(String... kv) {
    Map<String, String> m = new LinkedHashMap<>();
    for (int i = 0; i < kv.length; i += 2) {
      m.put(kv[i], kv[i + 1]);
    }
    return java.util.Collections.unmodifiableMap(m);
  }

  private static final java.util.regex.Pattern VARIABLE =
      java.util.regex.Pattern.compile("\\bOTEL_OPAMP_(ENDPOINT|HEADERS|STATE_DIR|HEARTBEAT_INTERVAL)\\b");

  /**
   * The core's text, naming each OTEL_OPAMP_* variable as the agent's property: in a warning, the
   * property (otel.opamp.endpoint); in a hint, both (otel.opamp.endpoint / OTEL_OPAMP_ENDPOINT),
   * as either can give it.
   */
  static String named(String text, boolean both) {
    return VARIABLE.matcher(text).replaceAll(m -> {
      String property = PROPERTIES.get(m.group());
      return java.util.regex.Matcher.quoteReplacement(both ? property + " / " + m.group() : property);
    });
  }

  /** The core's hints, naming the properties. */
  static CoreProto.SetupHints hints(CoreProto.SetupHints h) {
    return CoreProto.SetupHints.newBuilder()
        .setRefused(named(h.getRefused(), true))
        .setNotFound(named(h.getNotFound(), true))
        .setUnreachable(named(h.getUnreachable(), true))
        .setError(named(h.getError(), true))
        .build();
  }

  /** The hints for an endpoint and headers given as the properties. */
  static CoreProto.SetupHints hintsFor(String endpoint) {
    try {
      return hints(dev.pydantic.logfire.fleet.Setup.start(CoreProto.SetupStart.newBuilder()
          .addEnv(CoreProto.Variable.newBuilder().setName("OTEL_OPAMP_ENDPOINT").setValue(endpoint))
          .addEnv(CoreProto.Variable.newBuilder().setName("OTEL_OPAMP_STATE_DIR").setValue("none"))
          .build()).getHints());
    } catch (dev.pydantic.logfire.fleet.Setup.Failed e) {
      return CoreProto.SetupHints.getDefaultInstance();
    }
  }

  /** This operating system, as Setup names it. */
  private static CoreProto.SetupOs os() {
    String os = System.getProperty("os.name", "").toLowerCase(java.util.Locale.ROOT);
    if (os.startsWith("windows")) {
      return CoreProto.SetupOs.SETUP_OS_WINDOWS;
    }
    if (os.startsWith("mac")) {
      return CoreProto.SetupOs.SETUP_OS_MACOS;
    }
    return CoreProto.SetupOs.SETUP_OS_UNIX;
  }

  /**
   * The slots this JVM holds. A second channel on a held lock file is never opened: closing it
   * could drop the JVM's lock on the file (POSIX locks are the process's, not the channel's).
   */
  private static final Set<Path> HELD = new HashSet<>();

  /** A state directory and the lock that makes it this JVM's. */
  record Claimed(Path dir, FileLock lock) {}

  /**
   * The program, as a name: the main class, or the jar run with -jar (without .jar), from
   * sun.java.command; "java" when the JVM doesn't say.
   */
  static String programName(String command) {
    String first = command == null ? "" : command.trim().split("\\s+", 2)[0];
    String name = first.substring(Math.max(first.lastIndexOf('/'), first.lastIndexOf('\\')) + 1);
    if (name.endsWith(".jar")) {
      name = name.substring(0, name.length() - 4);
    }
    return name.isEmpty() ? "java" : name;
  }

  /** How many processes running at once keep their own identity in one state directory. */
  static final int SLOTS = 16;

  /**
   * A named state directory's slots (docs/default-state-dir.md): itself, then its folders 2 to 16.
   * The first process keeps the directory and its UID, as before slots; one sharing it (a process
   * started with the same {@code OTEL_OPAMP_STATE_DIR}) takes a folder of its own instead of its UID.
   */
  static List<String> namedSlots(String dir) {
    List<String> slots = new java.util.ArrayList<>();
    slots.add(dir);
    for (int n = 2; n <= SLOTS; n++) {
      slots.add(Path.of(dir, Integer.toString(n)).toString());
    }
    return slots;
  }

  /**
   * The first of a state directory's slots (the default one's as the core's Setup names them, or
   * the one {@code named}'s) this JVM can make and lock, and its lock, held for the JVM's lifetime;
   * null where there's none to be had (not writable, every slot taken: {@code allTaken} is the line
   * to say then): each start is then a new instance. Never throws; a directory named that can't be
   * used is said as a warning, the default one's at FINE.
   */
  static Claimed slot(List<String> slots, String allTaken, String named) {
    if (slots.isEmpty()) {
      return null;
    }
    try {
      Path first = Path.of(slots.get(0));
      boolean posix = first.getFileSystem().supportedFileAttributeViews().contains("posix");
      FileAttribute<?>[] dirAttrs = posix
          ? new FileAttribute<?>[] {PosixFilePermissions.asFileAttribute(PosixFilePermissions.fromString("rwx------"))}
          : new FileAttribute<?>[0];
      FileAttribute<?>[] fileAttrs = posix
          ? new FileAttribute<?>[] {PosixFilePermissions.asFileAttribute(PosixFilePermissions.fromString("rw-------"))}
          : new FileAttribute<?>[0];
      for (String slot : slots) {
        Path d = Path.of(slot);
        Path lockFile = d.resolve(dev.pydantic.logfire.fleet.Setup.LOCK_FILE).toAbsolutePath();
        synchronized (HELD) {
          if (HELD.contains(lockFile)) {
            continue;
          }
          FileChannel ch;
          try {
            Files.createDirectories(d, dirAttrs);
            ch = FileChannel.open(lockFile, Set.of(StandardOpenOption.CREATE, StandardOpenOption.WRITE), fileAttrs);
          } catch (IOException | RuntimeException e) {
            unusable(named, e);
            return null;
          }
          FileLock lock = null;
          try {
            lock = ch.tryLock();
          } catch (IOException | OverlappingFileLockException e) {
            // Taken.
          }
          if (lock != null) {
            HELD.add(lockFile);
            return new Claimed(d, lock);
          }
          ch.close();
        }
      }
      LOG.info("OpAMP: " + allTaken);
      return null;
    } catch (Exception e) {
      unusable(named, e);
      return null;
    }
  }

  private static void unusable(String named, Exception e) {
    if (named != null) {
      LOG.warning("OpAMP: the state directory " + named + " can't be used (" + e + "): each start is a new instance");
    } else {
      LOG.log(Level.FINE, "OpAMP: no state directory of its own (" + e + "): each start is a new instance");
    }
  }

  /** For tests: let a state directory go, as the JVM's end does. */
  static void release(Setup setup) throws IOException {
    if (setup != null && setup.stateLock() != null) {
      synchronized (HELD) {
        setup.stateLock().channel().close();
        HELD.remove(setup.stateDir().resolve(dev.pydantic.logfire.fleet.Setup.LOCK_FILE).toAbsolutePath());
      }
    }
  }

  /**
   * An endpoint as it is safe to log (no user info, query or fragment), as the core shows it;
   * {@code otherwise} if the core can't say.
   */
  static String shown(String endpoint, String otherwise) {
    try {
      String shown = dev.pydantic.logfire.fleet.Setup.start(CoreProto.SetupStart.newBuilder()
          .setCode(CoreProto.SetupInCode.newBuilder().setEndpoint(endpoint))
          .build()).getShownEndpoint();
      return shown.isEmpty() ? otherwise : shown;
    } catch (dev.pydantic.logfire.fleet.Setup.Failed e) {
      return otherwise;
    }
  }

  /**
   * Whether the core can be loaded, by {@code unavailable} (why it can't, or null); when it can't,
   * said once, and the agent runs unmanaged.
   */
  private boolean coreLoads(String unavailable) {
    if (unavailable != null && !coreSaid) {
      coreSaid = true;
      LOG.warning("OpAMP: the core can't be loaded on " + System.getProperty("os.name") + "-"
          + System.getProperty("os.arch") + "; the agent runs unmanaged: " + unavailable);
    }
    return unavailable == null;
  }

  /** Whether an endpoint is configured: without one the extension changes nothing. */
  synchronized boolean on() {
    return setup != null;
  }

  /** Whether the extension saw the resource built (under autoconfigure, not declarative config). */
  synchronized boolean seesResource() {
    return resource != null;
  }

  /** First: load the saved settings; the next-start ones become agent properties. */
  synchronized Map<String, String> properties(ConfigProperties config) {
    return properties(config, System.getenv());
  }

  /** {@link #properties(ConfigProperties)}, with {@code env} for the user's state directory. */
  synchronized Map<String, String> properties(ConfigProperties config, Map<String, String> env) {
    hooked = true;
    declared = HOST;
    samplerOutside = false;
    if (!coreLoads(coreUnavailable.get())) {
      setup = null;
      return Map.of();
    }
    setup = Setup.from(config, env, System.getProperty("sun.java.command"));
    if (setup == null) {
      return Map.of();
    }
    // The settings layer only once there is a server: an agent without one never loads it.
    if (!coreLoads(layerUnavailable.get())) {
      try {
        release(setup);
      } catch (IOException e) {
        // The JVM's end lets it go.
      }
      setup = null;
      return Map.of();
    }
    byte[] coreState = null;
    canSave = writable(setup.stateDir());
    if (setup.stateDir() != null) {
      try {
        // The settings to start with are the last config applied, which the client keeps in its
        // state (opamp-state).
        Path state = setup.stateDir().resolve("opamp-state");
        if (Files.exists(state)) {
          coreState = Files.readAllBytes(state);
        }
      } catch (IOException e) {
        LOG.log(Level.WARNING, "OpAMP: the client's saved state unreadable, using the agent's own settings", e);
      }
    }
    if (controls.app == null) {
      controls.app = appSettings.get();
    }
    appBefore = controls.app == null ? null : controls.app.current();
    this.coreState = coreState;
    begin();
    return Controls.startProperties(startSettings);
  }

  /**
   * Under autoconfigure, once every customizer has run: another extension added {@code added} (a
   * span or log record processor, or a metric reader, by name) to the SDK's {@code signal}, and
   * what it exports goes past the gates. Each is listed ungoverned in the inventory and export
   * health; the policy still governs every exporter the agent built, so nothing is set aside.
   */
  synchronized void outside(String signal, List<String> added) {
    if (added.isEmpty()) {
      return;
    }
    inventory.outside(signal, added);
    // Decided at once, so the layer's warning for each is logged now.
    inventory.listed();
  }

  /**
   * Once the SDK is built, before the client starts: whether it samples with the extension's
   * sampler. Another extension's tracer provider customizer can set a sampler of its own in its
   * place, and then the settings the extension's sampler holds are set aside.
   */
  synchronized void built(io.opentelemetry.sdk.OpenTelemetrySdk sdk) {
    if (setup == null || samplerOutside || sdk.getSdkTracerProvider().getSampler() == controls.sampler) {
      return;
    }
    samplerOutside = true;
    List<String> lost = narrow();
    if (!lost.isEmpty()) {
      LOG.warning("OpAMP: the SDK samples with " + LiveSampler.describe(sdk.getSdkTracerProvider().getSampler())
          + ", which another extension set in place of this extension's sampler; set aside, as it can't carry"
          + " them out: " + String.join(", ", lost));
    }
  }

  /**
   * Declare only what the agent still carries out: the flow decides again under that host. The
   * live settings it declares no more, in order (none: nothing changed).
   */
  private List<String> narrow() {
    SettingsProto.Host narrowed = host(samplerOutside);
    if (narrowed.equals(declared) || flow == null) {
      return List.of();
    }
    List<String> lost = declared.getLiveList().stream().filter(k -> !narrowed.getLiveList().contains(k)).toList();
    declared = narrowed;
    step(SettingsProto.AgentStep.newBuilder().setHost(host()));
    return lost;
  }

  /** The flow's start: the settings to start with put in force, and its timer armed. */
  private void begin() {
    carryOut(layer().start(agentStart()));
    startSettings = inForce;
  }

  private SettingsProto.AgentStart agentStart() {
    base = monoClock.getAsLong();
    CoreProto.SetupHints h = setup.hints() == null ? CoreProto.SetupHints.getDefaultInstance() : setup.hints();
    SettingsProto.AgentStart.Builder start = SettingsProto.AgentStart.newBuilder()
        .setHost(host())
        .setStateDir(setup.stateDir() != null)
        .setEndpoint(setup.endpoint())
        .setInstanceUid(ByteString.copyFrom(uid == null ? new byte[16] : bytes(uid)))
        .setHints(SettingsProto.AgentHints.newBuilder().setRefused(h.getRefused()).setNotFound(h.getNotFound())
            .setUnreachable(h.getUnreachable()).setError(h.getError()))
        .setNowUnixMs(wallClock.getAsLong())
        .setNowMonoMs(0);
    if (coreState != null) {
      start.setCoreState(ByteString.copyFrom(coreState));
    }
    return start.build();
  }

  /**
   * The UID is known (after the settings were put in force: the agent reads the properties first):
   * the same start again, naming the instance. Only the flow is kept: what it said to do was done.
   */
  private void named() {
    if (flow != null) {
      flow = layer().start(agentStart()).getAgent();
    }
  }

  /** The settings in force at start, next-start ones included. */
  synchronized SettingsProto.Settings startSettings() {
    return startSettings;
  }

  /**
   * Leave the agent unmanaged: the controls couldn't be put in. The application's own settings go
   * back to those in force before (none, at a JVM's start): the server no longer manages them.
   */
  synchronized void off() {
    setup = null;
    if (appBefore != null) {
      controls.restoreApp(appBefore);
      appBefore = null;
    }
  }

  /** What the extension declares to the layer: {@code custom} too where the application can read it. */
  SettingsProto.Host host() {
    return controls.app == null ? declared : withApp(declared);
  }

  /**
   * Once the SDK is built: why the extension is off although an endpoint is configured, when the
   * agent called none of its hooks (null when it did, or there's no endpoint). Unsaid, the server
   * would wait for this instance in vain.
   */
  synchronized String notHooked(ConfigProperties config) {
    if (hooked) {
      return null;
    }
    String endpoint = config.getString("otel.opamp.endpoint");
    if (endpoint == null || endpoint.isBlank()) {
      return null;
    }
    return "OpAMP: otel.opamp.endpoint is set, but the agent built its SDK without calling the extension's "
        + "autoconfigure or declarative configuration hooks, so it won't connect to "
        + shown(endpoint.trim(), "the server it names")
        + " and the agent runs unmanaged";
  }

  /**
   * Whether {@code dir} can be made and written, tried with a file: the permission bits alone don't
   * say (root, ACLs, a read-only mount).
   */
  private static boolean writable(Path dir) {
    if (dir == null) {
      return false;
    }
    try {
      Files.createDirectories(dir);
      Files.delete(Files.createTempFile(dir, ".probe", null));
      return true;
    } catch (IOException | RuntimeException e) {
      LOG.log(Level.WARNING, "OpAMP: can't write the state directory " + dir
          + "; the instance UID isn't kept, and the client can't keep the settings applied", e);
      return false;
    }
  }

  private synchronized SdkSettings layer() {
    if (layer == null) {
      layer = new SdkSettings();
    }
    return layer;
  }

  /**
   * Second: one UID names the instance to the server (OpAMP) and in the telemetry
   * (service.instance.id). With a state directory it is kept across restarts.
   */
  synchronized Resource resource(Resource r) {
    if (setup == null) {
      return r;
    }
    uid = identity(r.getAttribute(INSTANCE_ID));
    named();
    resource = r.merge(Resource.create(Attributes.of(INSTANCE_ID, uid.toString())));
    return resource;
  }

  /**
   * The same under declarative configuration, where the resource is built from the model: the UID
   * to put in it, given the {@code service.instance.id} the model names, if any. The resource the
   * agent built is handed over at {@link #start}.
   */
  synchronized String instanceId(String given) {
    if (setup == null) {
      return given;
    }
    uid = identity(given);
    named();
    return uid.toString();
  }

  /**
   * The instance UID: the one saved in the state directory; else a new one, saved there; else,
   * where none can be kept (no state directory, or one it can't be saved in), a UUID
   * service.instance.id given, the same at every start; else a new one.
   */
  private UUID identity(String given) {
    Path file = setup.stateDir() == null ? null : setup.stateDir().resolve(dev.pydantic.logfire.fleet.Setup.UID_FILE);
    String saved = "";
    if (file != null && Files.exists(file)) {
      try {
        saved = Files.readString(file);
      } catch (IOException | RuntimeException e) {
        LOG.log(Level.WARNING, "OpAMP: saved instance UID unreadable, making a new one", e);
      }
    }
    byte[] entropy = new byte[16];
    new SecureRandom().nextBytes(entropy);
    // Only a UUID can be the instance UID; anything else (commas included) is not passed on.
    String attributes = isUuid(given) ? "service.instance.id=" + given : "";
    CoreProto.SetupIdentified got;
    try {
      got = dev.pydantic.logfire.fleet.Setup.identify(CoreProto.SetupIdentity.newBuilder()
          .setStateDir(file != null)
          .setSaved(saved)
          .setResourceAttributes(attributes)
          .setEntropy(ByteString.copyFrom(entropy))
          .setUnixMs(System.currentTimeMillis())
          .build());
    } catch (dev.pydantic.logfire.fleet.Setup.Failed e) {
      LOG.log(Level.SEVERE, "OpAMP: the core could not make an instance UID", e);
      return UUID.randomUUID();
    }
    if (!saved.isEmpty() && got.getFrom() != CoreProto.SetupUidFrom.SETUP_UID_FROM_SAVED) {
      LOG.warning("OpAMP: saved instance UID unreadable, making a new one");
    }
    if (got.getSave()) {
      if (!canSave) {
        return uuid(got.getUnsavedUid());
      }
      try {
        write(file, got.getText().getBytes(StandardCharsets.UTF_8));
      } catch (IOException e) {
        LOG.log(Level.WARNING, "OpAMP: can't save the instance UID", e);
        return uuid(got.getUnsavedUid());
      }
    }
    return uuid(got.getInstanceUid());
  }

  /** {@code given} (a service.instance.id; null: none) is a UUID, so it can be the instance UID. */
  static boolean isUuid(String given) {
    if (given == null) {
      return false;
    }
    try {
      UUID.fromString(given);
      return true;
    } catch (IllegalArgumentException e) {
      return false;
    }
  }

  private static UUID uuid(ByteString b) {
    ByteBuffer buf = b.asReadOnlyByteBuffer();
    return new UUID(buf.getLong(), buf.getLong());
  }

  /**
   * Last, once the SDK is built: join the fleet. {@code built} is the resource the agent built
   * (null: unknown), taken under declarative configuration, where the extension doesn't see it
   * being built.
   */
  void start(Resource built) {
    Setup s;
    synchronized (this) {
      s = setup;
      if (s != null && resource == null && uid != null) {
        Resource id = Resource.create(Attributes.of(INSTANCE_ID, uid.toString()));
        resource = built == null ? id : built.merge(id);
      }
    }
    if (s == null || resource == null) {
      return;
    }
    OpampClient.Config.Builder b = OpampClient.Config.builder(s.endpoint(), bytes(uid))
        // What the integration does and reports, declared: the client takes no status field
        // whose bit isn't.
        .capabilities(
            Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE
                | Opamp.AgentCapabilities.AgentCapabilities_ReportsRemoteConfig_VALUE
                | Opamp.AgentCapabilities.AgentCapabilities_ReportsHeartbeat_VALUE
                | Opamp.AgentCapabilities.AgentCapabilities_ReportsHealth_VALUE
                | Opamp.AgentCapabilities.AgentCapabilities_ReportsEffectiveConfig_VALUE
                | Opamp.AgentCapabilities.AgentCapabilities_ReportsAvailableComponents_VALUE);
    s.headers().forEach(b::header);
    Duration heartbeat = s.heartbeat();
    if (!heartbeat.isZero()) {
      b.heartbeatInterval(heartbeat);
    }
    if (s.stateDir() != null) {
      b.stateFile(s.stateDir().resolve("opamp-state"));
    }
    // A key of its own, offered by the server: taken with a state directory (the state file) and
    // kept there; without one the client neither declares nor takes it.
    b.acceptConnectionSettings(true);
    // Platform mode, asked for by name (an HTTP client selects it too): the client's requests go
    // through the JDK's HTTP client, one of the agent's own that isn't traced.
    b.mode(OpampClient.Mode.PLATFORM).httpClient(untracedHttpClient());
    // The flow is told the SDK is built: export health from the first message.
    synchronized (this) {
      String service = resource.getAttribute(SERVICE_NAME);
      step(SettingsProto.AgentStep.newBuilder().setBuilt(SettingsProto.AgentBuilt.newBuilder()
          .setService(service == null ? "" : service).addAllExporters(inventory.listed())));
      step(exporters());
    }
    // The inventory, once: the agent builds its SDK and instrumentations once, and the settings it
    // carries out (its host, the application's own among them once they're read) don't change
    // while it runs.
    components = layer().components(inventory.build(), host(), wallClock.getAsLong());
    OpampClient.Status status = new OpampClient.Status(layer().describe(attributes(resource), host(), wallClock.getAsLong()), health, effective,
        null, components);
    try {
      // The client's own requests aren't the application's. Everything it does runs on its
      // executor, and the agent carries the submitting context into executor tasks: started
      // with instrumentation suppressed, none of its requests is traced.
      OpampClient.Config config = b.build();
      InstrumentationUtil.suppressInstrumentation(
          () -> client.complete(OpampClient.start(config, status, this::notified)));
    } catch (RuntimeException | LinkageError | StackOverflowError e) {
      LOG.log(Level.SEVERE, "OpAMP: the client did not start; the agent runs unmanaged", e);
      return;
    }
    Runtime.getRuntime().addShutdownHook(new Thread(() -> {
      try {
        // The server hears the agent go (agent_disconnect). The SDK's own shutdown hook flushes
        // meanwhile: the JVM runs hooks in no set order, and the agent keeps no handle on its
        // hook to run one after the other. Stopped with instrumentation suppressed too: the
        // goodbye is the agent's own request. The core bounds how long stop takes (5 s).
        CompletableFuture<Void> stopped = new CompletableFuture<>();
        InstrumentationUtil.suppressInstrumentation(
            () -> client.join().stop().whenComplete((v, e) -> stopped.complete(null)));
        stopped.get();
      } catch (Exception e) {
        LOG.log(Level.FINE, "OpAMP: stopping the client", e);
      }
    }, "opamp-stop"));
  }

  /**
   * The client's HTTP client, whose every task runs with instrumentation suppressed: the client
   * carries on from its responses, on these threads, and whatever it sends next is the agent's
   * own too. (Starting it suppressed isn't enough: a response's thread has the ordinary context.)
   */
  private static java.net.http.HttpClient untracedHttpClient() {
    java.util.concurrent.ExecutorService pool = java.util.concurrent.Executors.newCachedThreadPool(r -> {
      Thread t = new Thread(r, "opamp-http");
      t.setDaemon(true);
      return t;
    });
    return java.net.http.HttpClient.newBuilder()
        .followRedirects(java.net.http.HttpClient.Redirect.NEVER)
        .executor(task -> pool.execute(() -> InstrumentationUtil.suppressInstrumentation(task)))
        .build();
  }

  /**
   * On the client's thread: keep it quick; the first may come before start returns, so what the
   * flow says to send waits for the client. (The client tells a UID at start too, if it resumes one
   * the server assigned before.)
   */
  void notified(Notification n) {
    OpampClient c = client.getNow(null);
    Out out = tell(n, c == null ? null : c.endpoint());
    if (out != null && !out.sends().isEmpty()) {
      out.send(client.join());
    }
  }

  /** Tell the flow of a notification; {@code inUse}: the endpoint the client uses now (null: the configured one). */
  private Out tell(Notification n, String endpoint) {
    String inUse = endpoint == null ? "" : endpoint;
    SettingsProto.AgentStep.Builder in = SettingsProto.AgentStep.newBuilder();
    if (n instanceof Notification.RemoteConfig rc) {
      in.setRemoteConfig(rc.config());
    } else if (n instanceof Notification.Connected) {
      in.setConnected(SettingsProto.AgentConnected.newBuilder().setEndpoint(inUse));
    } else if (n instanceof Notification.ConnectFailed f) {
      in.setConnectFailed(SettingsProto.AgentConnectFailed.newBuilder().setEndpoint(inUse)
          .setStatus(f.status() == null ? 0 : f.status()).setDetail(f.detail() == null ? "" : f.detail()));
    } else if (n instanceof Notification.InstanceUidChanged changed) {
      in.setInstanceUidChanged(ByteString.copyFrom(changed.instanceUid()));
    } else if (n instanceof Notification.StateFromNewerVersion newer) {
      in.setStateFromNewerVersion((int) Math.min(newer.version(), 0xffffffffL));
    } else if (n instanceof Notification.Stopped st) {
      in.setStopped(SettingsProto.AgentStopped.newBuilder()
          .setError(st.error() == null ? "" : String.valueOf(st.error())));
    } else {
      return null;
    }
    synchronized (this) {
      return step(in);
    }
  }

  /**
   * What a step said: the answer and health it gave, if any, and what to send (sent at once while
   * the client runs; else left to the caller).
   */
  record Out(Opamp.RemoteConfigStatus status, Opamp.ComponentHealth health,
      List<java.util.function.Consumer<OpampClient>> sends) {
    void send(OpampClient c) {
      if (c != null) {
        sends.forEach(s -> s.accept(c));
      }
    }
  }

  /** Tell the flow what happened (one input), and carry out what it says. */
  private synchronized Out step(SettingsProto.AgentStep.Builder input) {
    if (flow == null) {
      return new Out(null, null, List.of());
    }
    SettingsProto.AgentOutput out;
    try {
      out = layer().step(input.setAgent(flow).setNowUnixMs(wallClock.getAsLong()).setNowMonoMs(mono()).build());
    } catch (RuntimeException e) {
      LOG.log(Level.WARNING, "OpAMP: the settings layer failed", e);
      return new Out(null, null, List.of());
    }
    return carryOut(out);
  }

  /** The flow's clock: the monotonic one, in milliseconds since it started. */
  private long mono() {
    return Math.max(0, monoClock.getAsLong() - base);
  }

  /** The flow's effects, in order, and its timer for the next wake. */
  private synchronized Out carryOut(SettingsProto.AgentOutput out) {
    flow = out.getAgent();
    effective = out.getEffective();
    Opamp.RemoteConfigStatus status = null;
    Opamp.ComponentHealth sent = null;
    List<java.util.function.Consumer<OpampClient>> sends = new ArrayList<>();
    for (SettingsProto.AgentEffect e : out.getEffectsList()) {
      switch (e.getEffectCase()) {
        case APPLY -> {
          controls.apply(e.getApply());
          inForce = e.getApply();
        }
        case EFFECTIVE -> {
          Opamp.EffectiveConfig c = e.getEffective();
          sends.add(client -> client.setEffectiveConfig(c));
        }
        case STATUS -> {
          Opamp.RemoteConfigStatus st = e.getStatus();
          status = st;
          sends.add(client -> client.setRemoteConfigStatus(st));
        }
        case HEALTH -> {
          Opamp.ComponentHealth h = e.getHealth();
          sent = h;
          health = h;
          sends.add(client -> client.setHealth(h));
        }
        case SAVE_UID -> saveUid(e.getSaveUid());
        case LOG -> say(e.getLog());
        default -> { }
      }
    }
    arm(out.getWakeAtMonoMs());
    return new Out(status, sent, sends);
  }

  /** The UID the server assigned, kept: service.instance.id from the next start. */
  private void saveUid(String named) {
    if (setup == null || setup.stateDir() == null) {
      return;
    }
    try {
      write(setup.stateDir().resolve(dev.pydantic.logfire.fleet.Setup.UID_FILE), named.getBytes(StandardCharsets.UTF_8));
    } catch (IOException e) {
      step(SettingsProto.AgentStep.newBuilder().setUidNotSaved(String.valueOf(e.getMessage())));
    }
  }

  /** A line of the flow's, said as the agent's own. */
  private static void say(SettingsProto.AgentLog line) {
    Level level = switch (line.getLevel()) {
      case DEBUG -> Level.FINE;
      case WARN -> Level.WARNING;
      case ERROR -> Level.SEVERE;
      default -> Level.INFO;
    };
    LOG.log(level, "OpAMP: " + line.getText());
  }

  /** The exporters listed and the gates' counts now, as the flow takes them. */
  private SettingsProto.AgentStep.Builder exporters() {
    return SettingsProto.AgentStep.newBuilder().setExporters(SettingsProto.AgentExporters.newBuilder()
        .addAllExporters(inventory.listed()).setCounts(controls.counts()));
  }

  /** Answer a remote config as the flow decides; the answer for the server (sent too, while the client runs). */
  synchronized Opamp.RemoteConfigStatus remoteConfig(Opamp.AgentRemoteConfig config) {
    Out out = step(SettingsProto.AgentStep.newBuilder().setRemoteConfig(config));
    out.send(client.getNow(null));
    return out.status() != null ? out.status() : Opamp.RemoteConfigStatus.newBuilder()
        .setLastRemoteConfigHash(config.getConfigHash())
        .setStatus(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_FAILED)
        .setErrorMessage("the settings layer gave no answer").build();
  }

  /**
   * Arm the one timer for {@code at} (the flow's clock; 0: none), in place of any earlier one. It
   * waits on the executor's monotonic clock, so the wall clock moving meanwhile doesn't move it.
   */
  private synchronized void arm(long at) {
    generation++;
    wakeAt = at;
    if (armed != null) {
      armed.cancel(false);
      armed = null;
    }
    if (at == 0 || timerHeld) {
      return;
    }
    long gen = generation;
    armed = timer().schedule(() -> fire(gen, at), Math.max(0, at - mono()), java.util.concurrent.TimeUnit.MILLISECONDS);
  }

  private synchronized java.util.concurrent.ScheduledExecutorService timer() {
    if (timer == null) {
      timer = java.util.concurrent.Executors.newSingleThreadScheduledExecutor(r -> {
        Thread t = new Thread(r, "opamp-settings-timer");
        t.setDaemon(true);
        return t;
      });
    }
    return timer;
  }

  /**
   * The timer armed in {@code gen} (for {@code at}) went off: the flow is told, with the gates'
   * counts. A step since armed its own, so this one does nothing.
   */
  void fire(long gen, long at) {
    Out out;
    synchronized (this) {
      if (gen != generation) {
        return;
      }
      armed = null;
      try {
        out = step(SettingsProto.AgentStep.newBuilder().setTimer(controls.counts()));
      } catch (RuntimeException e) {
        LOG.log(Level.WARNING, "OpAMP: the settings timer", e);
        return;
      }
    }
    out.send(client.getNow(null));
  }

  /** The agent health last sent. */
  Opamp.ComponentHealth health() {
    return health;
  }

  /** The live settings in force. */
  SettingsProto.Settings inForce() {
    return inForce;
  }

  /**
   * For tests: read the wall clock from {@code clock}. A timer armed meanwhile is held, not run on
   * the real clock: the test fires it, at its own time (its delay is the test clock's, so a timer
   * armed a moment before a test's deadline would otherwise go off on its own, mid-test).
   */
  synchronized void clockForTest(java.util.function.LongSupplier clock) {
    clockForTest(clock, clock);
  }

  /**
   * For tests: the wall clock from {@code wall}, the monotonic one from {@code mono}. A flow already
   * started starts again on them (a flow never steps back in time, and the test's clock is earlier).
   */
  synchronized void clockForTest(java.util.function.LongSupplier wall, java.util.function.LongSupplier mono) {
    wallClock = wall;
    monoClock = mono;
    timerHeld = true;
    if (armed != null) {
      armed.cancel(false);
      armed = null;
    }
    if (flow != null) {
      begin();
    }
  }

  /** For tests: the instance named {@code id}, as resource() names it. */
  synchronized void namedForTest(UUID id) {
    uid = id;
    named();
  }

  /** For tests: tell the flow of {@code n}, the client using {@code inUse} (null: the configured endpoint). */
  void tellForTest(Notification n, String inUse) {
    tell(n, inUse);
  }

  /**
   * For tests: the SDK built (as start tells the flow), with the exporters listed and {@code counts};
   * the timer fired first if due, with them. The agent health sent meanwhile (null: none).
   */
  synchronized Opamp.ComponentHealth builtForTest(String service, SettingsProto.ExportCounts counts) {
    Opamp.ComponentHealth before = health;
    long[] a = armedForTest();
    if (a[1] != 0 && a[1] <= mono()) {
      step(SettingsProto.AgentStep.newBuilder().setTimer(counts));
    }
    step(SettingsProto.AgentStep.newBuilder().setBuilt(SettingsProto.AgentBuilt.newBuilder().setService(service)
        .addAllExporters(inventory.listed())));
    step(SettingsProto.AgentStep.newBuilder().setExporters(SettingsProto.AgentExporters.newBuilder()
        .addAllExporters(inventory.listed()).setCounts(counts)));
    return health == before ? null : health;
  }

  /** When the timer is armed for on the flow's clock (0: not), and in which generation. */
  synchronized long[] armedForTest() {
    return new long[] {generation, wakeAt};
  }

  /** For tests: fire the timer if it is due by the test clock's now. */
  void fireDueForTest() {
    long[] a = armedForTest();
    if (a[1] != 0 && a[1] <= mono()) {
      fire(a[0], a[1]);
    }
  }

  /** For tests: when the flow asks the layer again (the end of debug mode; wall clock, ms; 0: never). */
  synchronized long refreshAtForTest() {
    return state().getRefreshAtUnixMs();
  }

  /** For tests: the exporters and counts now, told to the flow; the agent health last sent. */
  Opamp.ComponentHealth exportersNowForTest() {
    step(exporters());
    return health;
  }

  /** The flow's state, decoded. */
  synchronized SettingsProto.AgentState state() {
    try {
      return SettingsProto.AgentState.parseFrom(flow);
    } catch (com.google.protobuf.InvalidProtocolBufferException e) {
      throw new IllegalStateException(e);
    }
  }

  /** The available components sent at start (null before). */
  Opamp.AvailableComponents components() {
    return components;
  }

  /** What is in force, to report as the effective config. */
  Opamp.EffectiveConfig effective() {
    return effective;
  }

  /** The resource's attributes, typed as OpAMP's KeyValue. */
  static List<Anyvalue.KeyValue> attributes(Resource resource) {
    List<Anyvalue.KeyValue> out = new ArrayList<>();
    resource.getAttributes().forEach((key, value) -> {
      Anyvalue.AnyValue.Builder v = Anyvalue.AnyValue.newBuilder();
      if (value instanceof Boolean b) {
        v.setBoolValue(b);
      } else if (value instanceof Long l) {
        v.setIntValue(l);
      } else if (value instanceof Double d) {
        v.setDoubleValue(d);
      } else {
        v.setStringValue(String.valueOf(value));
      }
      out.add(Anyvalue.KeyValue.newBuilder().setKey(key.getKey()).setValue(v).build());
    });
    return out;
  }

  /**
   * Replace {@code file} with {@code content}, all or nothing, even across a crash; readable by the
   * agent's user only, as the client's state file.
   */
  private static void write(Path file, byte[] content) throws IOException {
    Path tmp = file.resolveSibling(file.getFileName() + ".tmp");
    try {
      Files.deleteIfExists(tmp);
      FileAttribute<?>[] attrs = file.getFileSystem().supportedFileAttributeViews().contains("posix")
          ? new FileAttribute<?>[] {PosixFilePermissions.asFileAttribute(PosixFilePermissions.fromString("rw-------"))}
          : new FileAttribute<?>[0];
      try (FileChannel ch = FileChannel.open(tmp, Set.of(StandardOpenOption.CREATE_NEW, StandardOpenOption.WRITE), attrs)) {
        ByteBuffer buf = ByteBuffer.wrap(content);
        while (buf.hasRemaining()) {
          ch.write(buf);
        }
        ch.force(true);
      }
      Files.move(tmp, file, StandardCopyOption.REPLACE_EXISTING, StandardCopyOption.ATOMIC_MOVE);
    } catch (IOException e) {
      Files.deleteIfExists(tmp);
      throw e;
    }
    // The rename itself must reach the disk (not every platform can sync a directory).
    try (FileChannel ch = FileChannel.open(file.toAbsolutePath().getParent(), StandardOpenOption.READ)) {
      ch.force(true);
    } catch (IOException e) {
      // best effort
    }
  }

  private static byte[] bytes(UUID u) {
    return ByteBuffer.allocate(16).putLong(u.getMostSignificantBits()).putLong(u.getLeastSignificantBits()).array();
  }

  /** For tests: where the server is, and the state directory. */
  synchronized Setup setupForTest() {
    return setup;
  }

  /** For tests: the endpoint what the agent says of reaching its server names now. */
  String endpointSaidForTest() {
    OpampClient c = client.getNow(null);
    String inUse = c == null ? null : c.endpoint();
    String configured = state().getEndpoint();
    return inUse == null || inUse.isEmpty() ? configured : shown(inUse, configured);
  }

  /**
   * For tests: stop the client, as the shutdown hook does (the server hears the agent go), and let
   * the state directory go, as the JVM's end then does.
   */
  void stopForTest() throws Exception {
    client.join().stop().get();
    release(setup);
  }

  /** For tests: a fresh instance with {@code setup}. */
  static Extension forTest(Setup setup) {
    return forTest(setup, null);
  }

  /** For tests: a fresh instance with {@code setup}, handing the application's settings to {@code app} (null: nowhere). */
  static Extension forTest(Setup setup, AppSettingsSink app) {
    Extension e = new Extension();
    e.controls.app = app;
    e.appSettings = () -> null;
    e.setup = setup;
    e.canSave = setup != null && setup.stateDir() != null;
    e.effective = Opamp.EffectiveConfig.getDefaultInstance();
    if (setup != null) {
      e.begin();
    }
    return e;
  }
}
