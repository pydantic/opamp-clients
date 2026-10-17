// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.Notification;
import dev.pydantic.opamp.OpampClient;
import dev.pydantic.opamp.core.CoreProto;
import dev.pydantic.opamp.sdk.SdkSettings;
import dev.pydantic.opamp.sdk.SettingsProto;
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
import java.util.function.Supplier;
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
      "mask_attributes");
  static final List<String> NEXT_START = List.of("deactivate_instrumentations", "deactivate_all_instrumentations");
  /** What each signal's gate holds: carried out only where every exporter of the signal is gated. */
  static final Map<String, List<String>> GATED = Map.of(
      "traces", List.of("send_traces", "drop_attributes", "mask_attributes"),
      "metrics", List.of("send_metrics"),
      "logs", List.of("send_logs", "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second",
          "debug_send_logs_level", "drop_attributes", "mask_attributes"));
  /** What the extension's sampler holds: carried out only where the SDK's sampler is the extension's. */
  static final List<String> SAMPLED = List.of("sampling_rate", "sampling_rules", "sampling_traces_per_second",
      "debug_sampling_rate");
  /**
   * Every setting: autoconfigure wraps every exporter it builds, and so does a configuration file
   * of batch and simple processors and periodic and pull readers ({@link Declarative#ungated}).
   */
  static final SettingsProto.Host HOST = host(Set.of());

  /** The host where the exporters of {@code ungated} signals pass no gate of the agent's. */
  static SettingsProto.Host host(Set<String> ungated) {
    return host(ungated, false);
  }

  /**
   * The host where the exporters of {@code ungated} signals pass no gate of the agent's, and, with
   * {@code samplerOutside}, the SDK samples with a sampler other than the extension's.
   */
  static SettingsProto.Host host(Set<String> ungated, boolean samplerOutside) {
    Set<String> out = new HashSet<>();
    ungated.forEach(signal -> out.addAll(GATED.get(signal)));
    if (samplerOutside) {
      out.addAll(SAMPLED);
    }
    return SettingsProto.Host.newBuilder().addAllLive(LIVE.stream().filter(k -> !out.contains(k)).toList())
        .addAllNextStart(NEXT_START).build();
  }

  /**
   * The application's own settings, {@code custom}, the policy's app_settings control
   * (docs/app-settings.md): declared where the
   * application can read them, that is when the bridge to {@code dev.pydantic.opamp.AppSettings} is
   * in place ({@link Controls#app}).
   */
  static SettingsProto.Host withApp(SettingsProto.Host host) {
    return host.toBuilder().addLive("custom").build();
  }

  /** {@link #HOST} with {@code custom}. */
  static final SettingsProto.Host HOST_WITH_APP = withApp(HOST);

  /** Puts the bridge to the application's {@code AppSettings} in place: null when it can't. */
  java.util.function.Supplier<AppSettingsSink> appSettings = AppSettingsSink::install;
  /** The application's own settings in force before {@link #properties}: what a failed setup puts back. */
  private Map<String, String> appBefore;

  final Controls controls = new Controls();
  /** What another extension added to the SDK past the gates, under autoconfigure. */
  final Outside outside = new Outside();
  /** What the agent runs, as the exporters were built: reported at start, as available components. */
  final Inventory inventory = new Inventory();
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
   * What the agent carries out ({@link #HOST}, narrowed by {@link #notGated}, {@link #outside} and
   * {@link #built}); {@link #host} adds {@code custom}.
   */
  private SettingsProto.Host declared = HOST;
  /** The signals whose telemetry goes past a gate, and whether the SDK's sampler isn't the extension's. */
  private final Set<String> ungated = new java.util.TreeSet<>();
  private boolean samplerOutside;
  /** The settings loaded at start: the next-start ones in force until the next restart. */
  private SettingsProto.Loaded loaded;
  private volatile ByteString current;
  /** The live settings in force: the last the layer handed over. */
  private volatile SettingsProto.Settings inForce = SettingsProto.Settings.getDefaultInstance();
  /**
   * The one timer for the layer's {@code refresh_at_unix_ms} (the end of debug mode), and the
   * generation it was armed in: a decision after it arms its own, and the older one does nothing.
   */
  private java.util.concurrent.ScheduledExecutorService timer;
  private java.util.concurrent.ScheduledFuture<?> armed;
  private long generation;
  private long refreshAt;
  /** Export health: the layer's last report, and the agent health last sent with it. */
  private SettingsProto.HealthReport healthReport;
  private volatile Opamp.ComponentHealth health = Opamp.ComponentHealth.newBuilder().setHealthy(true).build();
  /** How often the layer is asked whether export health is to be sent. */
  static final Duration HEALTH_EVERY = Duration.ofSeconds(30);
  private volatile Opamp.EffectiveConfig effective;
  private UUID uid;
  private Resource resource;
  private final CompletableFuture<OpampClient> client = new CompletableFuture<>();
  private volatile Reachability reachability;

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
        s = dev.pydantic.opamp.Setup.start(start.build());
      } catch (dev.pydantic.opamp.Setup.Failed e) {
        LOG.log(Level.SEVERE, "OpAMP: the core could not read the settings; the SDK runs unmanaged", e);
        return null;
      }
      for (String warning : s.getWarningsList()) {
        LOG.warning("OpAMP: " + named(warning, false));
      }
      if (s.getOff()) {
        return null;
      }
      Map<String, String> headers = new LinkedHashMap<>();
      for (CoreProto.Header h : s.getHeadersList()) {
        headers.put(h.getName(), h.getValue());
      }
      Path stateDir = s.getStateDir().isEmpty() ? null : Path.of(s.getStateDir());
      FileLock lock = null;
      if (stateDir == null) {
        Claimed claimed = slot(s.getDefaultSlotsList(), s.getAllSlotsTaken());
        if (claimed != null) {
          stateDir = claimed.dir();
          lock = claimed.lock();
        }
      }
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
      return hints(dev.pydantic.opamp.Setup.start(CoreProto.SetupStart.newBuilder()
          .addEnv(CoreProto.Variable.newBuilder().setName("OTEL_OPAMP_ENDPOINT").setValue(endpoint))
          .addEnv(CoreProto.Variable.newBuilder().setName("OTEL_OPAMP_STATE_DIR").setValue("none"))
          .build()).getHints());
    } catch (dev.pydantic.opamp.Setup.Failed e) {
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

  /** A default state directory and the lock that makes it this JVM's. */
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

  /**
   * The first of the default state directory's slots (docs/default-state-dir.md, as the core's
   * Setup names them) this JVM can make and lock, and its lock, held for the JVM's lifetime; null
   * where there's none to be had (not writable, every slot taken: {@code allTaken} is the line to
   * say then): each start is then a new instance. Never throws.
   */
  static Claimed slot(List<String> slots, String allTaken) {
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
        Path lockFile = d.resolve(dev.pydantic.opamp.Setup.LOCK_FILE).toAbsolutePath();
        synchronized (HELD) {
          if (HELD.contains(lockFile)) {
            continue;
          }
          FileChannel ch;
          try {
            Files.createDirectories(d, dirAttrs);
            ch = FileChannel.open(lockFile, Set.of(StandardOpenOption.CREATE, StandardOpenOption.WRITE), fileAttrs);
          } catch (IOException | RuntimeException e) {
            LOG.log(Level.FINE, "OpAMP: no state directory of its own (" + e + "): each start is a new instance");
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
      LOG.log(Level.FINE, "OpAMP: no state directory of its own (" + e + "): each start is a new instance");
      return null;
    }
  }

  /** For tests: let a default state directory go, as the JVM's end does. */
  static void release(Setup setup) throws IOException {
    if (setup != null && setup.stateLock() != null) {
      synchronized (HELD) {
        setup.stateLock().channel().close();
        HELD.remove(setup.stateDir().resolve(dev.pydantic.opamp.Setup.LOCK_FILE).toAbsolutePath());
      }
    }
  }

  /**
   * An endpoint as it is safe to log (no user info, query or fragment), as the core shows it;
   * {@code otherwise} if the core can't say.
   */
  static String shown(String endpoint, String otherwise) {
    try {
      String shown = dev.pydantic.opamp.Setup.start(CoreProto.SetupStart.newBuilder()
          .setCode(CoreProto.SetupInCode.newBuilder().setEndpoint(endpoint))
          .build()).getShownEndpoint();
      return shown.isEmpty() ? otherwise : shown;
    } catch (dev.pydantic.opamp.Setup.Failed e) {
      return otherwise;
    }
  }

  /** What to check when the server can't be reached, by what went wrong, as the core words it. */
  static String hint(CoreProto.SetupHints hints, Integer status) {
    if (status == null) {
      return hints.getUnreachable();
    }
    if (status == 401 || status == 403) {
      return hints.getRefused();
    }
    if (status == 404) {
      return hints.getNotFound();
    }
    return hints.getError();
  }

  /**
   * Says when the client connects, when it can't (once a run of failures, with what to check), and
   * when it's back: setup that works or doesn't is never silent. On the client's thread.
   */
  static final class Reachability {
    /** The configured endpoint, as it is safe to log. */
    private final String endpoint;
    /**
     * The endpoint the client uses now, as the client gives it (null, or null from it: the
     * configured one): after the server moved the agent, the one it moved it to.
     */
    volatile Supplier<String> inUse;
    private final String service;
    private final UUID uid;
    private final CoreProto.SetupHints hints;
    private boolean ever;
    private boolean failing;

    Reachability(String shown, String service, UUID uid, CoreProto.SetupHints hints) {
      this.endpoint = shown;
      this.service = service;
      this.uid = uid;
      this.hints = hints;
    }

    /** The endpoint the client uses now, as it is safe to log. */
    String at() {
      Supplier<String> f = inUse;
      String e = f == null ? null : f.get();
      return e == null || e.isEmpty() ? endpoint : shown(e, endpoint);
    }

    synchronized void notified(Notification n) {
      String endpoint = at();
      if (n instanceof Notification.Connected) {
        if (!ever) {
          String who = service == null || service.isEmpty() ? "" : " as " + service;
          LOG.info("OpAMP: connected to " + endpoint + "; this SDK is managed" + who + ", instance " + uid);
        } else if (failing) {
          LOG.info("OpAMP: connected to " + endpoint + " again");
        }
        ever = true;
        failing = false;
      } else if (n instanceof Notification.ConnectFailed f) {
        if (!failing) {
          String why = f.detail() != null && !f.detail().isEmpty() ? f.detail() : "HTTP " + f.status();
          LOG.warning("OpAMP: can't reach " + endpoint + " (" + why + "): " + hint(hints, f.status())
              + ". Retrying in the background; the SDK keeps its current settings meanwhile");
        } else {
          LOG.fine("OpAMP: still can't reach " + endpoint + ": " + f.detail());
        }
        failing = true;
      }
    }
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
    ungated.clear();
    samplerOutside = false;
    setup = Setup.from(config, env, System.getProperty("sun.java.command"));
    if (setup == null) {
      return Map.of();
    }
    byte[] saved = null;
    byte[] coreState = null;
    canSave = writable(setup.stateDir());
    if (setup.stateDir() != null) {
      try {
        // The settings to start with are the last config applied, which the client keeps in its
        // state (opamp-state); sdk-settings.json is what this agent kept itself before, read only
        // while the client's state holds no config applied.
        Path state = setup.stateDir().resolve("opamp-state");
        if (Files.exists(state)) {
          coreState = Files.readAllBytes(state);
        }
        Path file = setup.stateDir().resolve("sdk-settings.json");
        if (Files.exists(file)) {
          saved = Files.readAllBytes(file);
        }
      } catch (IOException e) {
        LOG.log(Level.WARNING, "OpAMP: saved SDK settings unreadable, using the agent's own", e);
      }
    }
    if (controls.app == null) {
      controls.app = appSettings.get();
    }
    appBefore = controls.app == null ? null : controls.app.current();
    start(coreState, saved);
    return Controls.startProperties(loaded.getSettings());
  }

  /**
   * Under a configuration file, once the model is read: the signals whose processors or readers
   * aren't ones the agent gates. What their gates hold isn't carried out: the settings are loaded
   * again without it, before the SDK is built.
   */
  synchronized void notGated(Set<String> signals) {
    ungated.addAll(signals);
    if (!narrow().isEmpty()) {
      LOG.warning("OpAMP: the configuration file's " + String.join(", ", new java.util.TreeSet<>(signals))
          + " pass a processor or reader other than batch, simple, periodic or pull, which the agent can't"
          + " gate; the server's settings for them aren't carried out");
    }
  }

  /**
   * Under autoconfigure, once every customizer has run: another extension added {@code added} (a
   * span or log record processor, or a metric reader, by name) to the SDK's {@code signal}, and
   * what it exports goes past the gate. The gate's settings for that signal are set aside: the
   * settings are loaded again without them, before the SDK is built. The inventory lists each as
   * ungated.
   */
  synchronized void outside(String signal, List<String> added) {
    if (added.isEmpty()) {
      return;
    }
    inventory.outside(signal, added);
    ungated.add(signal);
    List<String> lost = narrow();
    if (!lost.isEmpty()) {
      LOG.warning("OpAMP: another extension added " + String.join(", ", added) + " to the SDK's " + signal
          + ", which exports past this extension's gate; set aside, as it can't carry them out: "
          + String.join(", ", lost));
    }
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
      LOG.warning("OpAMP: the SDK samples with " + sdk.getSdkTracerProvider().getSampler().getDescription()
          + ", which another extension set in place of this extension's sampler; set aside, as it can't carry"
          + " them out: " + String.join(", ", lost));
    }
  }

  /**
   * Declare only what the agent still carries out, and load the settings again under that host.
   * The live settings it declares no more, in order (none: nothing changed).
   */
  private List<String> narrow() {
    SettingsProto.Host narrowed = host(ungated, samplerOutside);
    if (narrowed.equals(declared) || loaded == null) {
      return List.of();
    }
    List<String> lost = declared.getLiveList().stream().filter(k -> !narrowed.getLiveList().contains(k)).toList();
    declared = narrowed;
    start(null, loaded.getCurrent().toByteArray());
    return lost;
  }

  private void start(byte[] coreState, byte[] saved) {
    loaded = layer().loadFrom(coreState, saved, host(), wallClock.getAsLong());
    if (!loaded.getWarning().isEmpty()) {
      LOG.warning("OpAMP: " + loaded.getWarning());
    }
    current = loaded.getCurrent();
    effective = loaded.getEffective();
    controls.apply(loaded.getSettings());
    inForce = loaded.getSettings();
    arm(loaded.getRefreshAtUnixMs());
  }

  /** The settings loaded at start, next-start ones included. */
  synchronized SettingsProto.Settings startSettings() {
    return loaded.getSettings();
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
    return uid.toString();
  }

  /**
   * The instance UID: the one saved in the state directory; else a new one, saved there; else,
   * where none can be kept (no state directory, or one it can't be saved in), a UUID
   * service.instance.id given, the same at every start; else a new one.
   */
  private UUID identity(String given) {
    Path file = setup.stateDir() == null ? null : setup.stateDir().resolve(dev.pydantic.opamp.Setup.UID_FILE);
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
      got = dev.pydantic.opamp.Setup.identify(CoreProto.SetupIdentity.newBuilder()
          .setStateDir(file != null)
          .setSaved(saved)
          .setResourceAttributes(attributes)
          .setEntropy(ByteString.copyFrom(entropy))
          .setUnixMs(System.currentTimeMillis())
          .build());
    } catch (dev.pydantic.opamp.Setup.Failed e) {
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
        .capabilities(Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE);
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
    // Java mode, asked for by name (an HTTP client selects it too): the client's requests go
    // through the JDK's HTTP client, one of the agent's own that isn't traced.
    b.mode(OpampClient.Mode.JAVA).httpClient(untracedHttpClient());
    reachability = new Reachability(s.shown(), resource.getAttribute(SERVICE_NAME), uid, s.hints());
    reachability.inUse = () -> {
      OpampClient c = client.getNow(null);
      return c == null ? null : c.endpoint();
    };
    // Export health from the first message: the layer always sends its first report.
    health(controls.counts(), inForce, wallClock.getAsLong());
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
    } catch (RuntimeException e) {
      LOG.log(Level.SEVERE, "OpAMP: the client did not start; the agent runs unmanaged", e);
      return;
    }
    long every = HEALTH_EVERY.toMillis();
    timer().scheduleAtFixedRate(this::healthTick, every, every, java.util.concurrent.TimeUnit.MILLISECONDS);
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
   * On the client's thread: keep it quick; the first may come before start returns. (The client
   * tells a UID at start too, if it resumes one the server assigned before.)
   */
  void notified(Notification n) {
    Reachability r = reachability;
    if (r != null) {
      r.notified(n);
    }
    if (n instanceof Notification.RemoteConfig rc) {
      Opamp.RemoteConfigStatus answer = remoteConfig(rc.config());
      OpampClient c = client.join();
      c.setEffectiveConfig(effective);
      c.setRemoteConfigStatus(answer);
    } else if (n instanceof Notification.Stopped st && st.error() != null) {
      LOG.log(Level.SEVERE, "OpAMP: the client stopped; the agent runs unmanaged", st.error());
    } else if (n instanceof Notification.InstanceUidChanged changed) {
      uidChanged(changed.instanceUid());
    }
  }

  /**
   * The server named this instance anew. The client keeps the UID in its own state; kept here too,
   * it is service.instance.id from the next start (the resource is built once).
   */
  private synchronized void uidChanged(byte[] raw) {
    if (raw.length != 16) {
      return;
    }
    ByteBuffer buf = ByteBuffer.wrap(raw);
    UUID named = new UUID(buf.getLong(), buf.getLong());
    if (named.equals(uid)) {
      return;
    }
    if (!canSave) {
      LOG.info("OpAMP: the server named this instance " + named + "; service.instance.id stays " + uid);
      return;
    }
    try {
      write(setup.stateDir().resolve(dev.pydantic.opamp.Setup.UID_FILE), named.toString().getBytes(StandardCharsets.UTF_8));
      LOG.info("OpAMP: the server named this instance " + named + "; service.instance.id stays " + uid
          + " until the next start");
    } catch (IOException e) {
      LOG.log(Level.WARNING, "OpAMP: can't save the instance UID the server assigned", e);
    }
  }

  /** Carry out the layer's decision on a remote config; the answer for the server. */
  synchronized Opamp.RemoteConfigStatus remoteConfig(Opamp.AgentRemoteConfig config) {
    // With a state directory the client keeps the config this applies, in its own state, and tells
    // the server APPLIED once that is saved: nothing for the agent to save. Without one, nothing is
    // kept, and a next-start change is refused.
    long now = wallClock.getAsLong();
    SettingsProto.Decision d = layer().decide(config, current, loaded.getCurrent(), host(), now,
        setup.stateDir() == null ? null : doc -> { });
    if (d.hasApply()) {
      controls.apply(d.getApply());
      inForce = d.getApply();
    }
    if (d.getPendingRestartCount() > 0) {
      LOG.info("OpAMP: " + String.join(", ", d.getPendingRestartList())
          + " changed; in force when the application restarts");
    }
    current = d.getCurrent();
    effective = d.getEffective();
    if (!d.hasApply() && refreshAt != 0 && now >= refreshAt) {
      // FAILED, so nothing changes, but what is reported is now's: once the wall clock has passed
      // the end of debug mode (a machine that slept, before the timer went off), it ends here, as
      // the report says.
      refresh(now);
    } else {
      arm(d.getRefreshAtUnixMs());
    }
    return d.getStatus();
  }

  /**
   * Arm the one timer for {@code at} (ms since the Unix epoch; 0: none), in place of any earlier
   * one. The time left is measured now and waited on the executor's monotonic clock, so the wall
   * clock moving meanwhile doesn't move the end.
   */
  private synchronized void arm(long at) {
    generation++;
    refreshAt = at;
    if (armed != null) {
      armed.cancel(false);
      armed = null;
    }
    if (at == 0) {
      return;
    }
    long gen = generation;
    armed = timer().schedule(() -> fire(gen, at), Math.max(0, at - wallClock.getAsLong()),
        java.util.concurrent.TimeUnit.MILLISECONDS);
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
   * The timer armed in {@code gen} for {@code at} went off: what is in force now. A decision since
   * armed its own, so this one does nothing. The time waited is the timer's: the layer is asked at
   * {@code at} at least, however the wall clock moved.
   */
  void fire(long gen, long at) {
    Opamp.EffectiveConfig report;
    synchronized (this) {
      if (gen != generation) {
        return;
      }
      report = refresh(Math.max(wallClock.getAsLong(), at));
    }
    OpampClient c = client.getNow(null);
    if (c != null) {
      c.setEffectiveConfig(report);
    }
  }

  /** Ask the layer what is in force at {@code nowUnixMs}, put it in force, and arm the next timer. */
  synchronized Opamp.EffectiveConfig refresh(long nowUnixMs) {
    SettingsProto.Refreshed r = layer().refresh(current, loaded.getCurrent(), host(), nowUnixMs);
    if (!r.getWarning().isEmpty()) {
      LOG.warning("OpAMP: " + r.getWarning());
    }
    controls.apply(r.getSettings());
    inForce = r.getSettings();
    effective = r.getEffective();
    if (refreshAt != 0 && r.getRefreshAtUnixMs() == 0) {
      LOG.info("OpAMP: debug mode is over; the settings sent before it are in force again");
    }
    arm(r.getRefreshAtUnixMs());
    return effective;
  }

  /**
   * Export health, every {@link #HEALTH_EVERY}: what the gates counted, through the layer, which
   * says whether to send it (at once on a change of health or status, at most every 60 s for the
   * counters alone, never while nothing changes).
   */
  void healthTick() {
    try {
      Opamp.ComponentHealth h = health(controls.counts(), inForce, wallClock.getAsLong());
      OpampClient c = client.getNow(null);
      if (h != null && c != null) {
        c.setHealth(h);
      }
    } catch (RuntimeException e) {
      // A periodic task that throws is never run again.
      LOG.log(Level.WARNING, "OpAMP: export health", e);
    }
  }

  /**
   * The agent health to send for these counts and settings at {@code nowUnixMs}, or null when the
   * layer says not to send. The top level stays healthy (the application's); export health is its
   * {@code component_health_map["exporters"]}.
   */
  synchronized Opamp.ComponentHealth health(SettingsProto.ExportCounts counts, SettingsProto.Settings settings,
      long nowUnixMs) {
    SettingsProto.HealthReport r = layer().health(counts, healthReport, nowUnixMs, settings);
    healthReport = r;
    if (!r.getSend()) {
      return null;
    }
    health = Opamp.ComponentHealth.newBuilder().setHealthy(true)
        .putComponentHealthMap("exporters", r.getExporters()).build();
    return health;
  }

  /** The agent health last sent. */
  Opamp.ComponentHealth health() {
    return health;
  }

  /** The live settings in force. */
  SettingsProto.Settings inForce() {
    return inForce;
  }

  /** When the timer is armed for (0: not), and in which generation. */
  synchronized long[] armedForTest() {
    return new long[] {generation, refreshAt};
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
    return reachability.at();
  }

  /** For tests: stop the client, as the shutdown hook does (the server hears the agent go). */
  void stopForTest() throws Exception {
    client.join().stop().get();
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
    e.loaded = SettingsProto.Loaded.getDefaultInstance();
    e.current = ByteString.copyFromUtf8("{}");
    e.effective = Opamp.EffectiveConfig.getDefaultInstance();
    return e;
  }
}
