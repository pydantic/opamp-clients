// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.net.http.HttpClient;
import java.nio.file.Path;
import java.time.Duration;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.Objects;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.ScheduledFuture;
import java.util.concurrent.ScheduledThreadPoolExecutor;
import java.util.concurrent.TimeUnit;
import java.util.function.Consumer;
import opamp.proto.v1.Opamp;

/**
 * An OpAMP client over plain HTTP or WebSocket, with TLS or without, directly or through a proxy.
 *
 * <p>Everything OpAMP (what to send and when, backoff, heartbeats, full reports, instance UIDs,
 * agent_disconnect, and the requests and responses themselves: headers, framing, Retry-After, size
 * limits) is the shared core, run as WebAssembly in Chicory. By default
 * ({@link Mode#CORE}) the core speaks HTTP and WebSocket itself too, over the JDK's sockets
 * ({@link Streams}); in {@link Mode#PLATFORM} this class carries out the core's requests with the
 * JDK's HTTP client instead. Either way it feeds back what happened, so it behaves exactly like
 * the other languages' clients on the same core. Everything runs on one
 * thread of its own; the methods may be called from any thread, the callback's included. What the
 * callback asks before it returns (its answer to a remote config, say) reaches the core before
 * anything the client takes in next: what the server says, a connection that opened, a timer.
 */
public final class OpampClient {
  /** Who speaks HTTP and WebSocket: the shared core itself, or the JDK's client. */
  public enum Mode {
    /**
     * The default: the shared core speaks HTTP/1.1, WebSocket, CONNECT and SOCKS5 itself,
     * over the JDK's sockets and TLS ({@code SSLEngine}, with {@link
     * Config.Builder#tls}); the client only opens connections, starts TLS, picks the proxy and
     * writes package files.
     */
    CORE,
    /**
     * The JDK's HTTP client and WebSocket: for an application that brings its own {@code
     * HttpClient} ({@link Config.Builder#httpClient}, which selects it too) or needs the JDK's
     * own stack (its {@code ProxySelector} and proxy system properties).
     */
    PLATFORM
  }

  /** The client's settings. */
  public static final class Config {
    final String endpoint;
    /** The application's instance UID, or null: the core makes one. */
    final byte[] instanceUid;
    final long capabilities;
    final Duration heartbeatInterval;
    final Map<String, String> headers;
    final Duration requestTimeout;
    final long maxSendBytes;
    final long maxReceiveBytes;
    final HttpClient httpClient;
    final Tls tls;
    final Transport.Proxy proxy;
    final boolean direct;
    final Path stateFile;
    final boolean acceptConnectionSettings;
    final Duration verifyTimeout;
    final boolean acceptPackages;
    final boolean requestInstanceUid;
    final long maxPackageBytes;
    final Duration downloadTimeout;
    final Duration stopTimeout;
    final Duration connectTimeout;
    final Mode mode;
    final boolean spliceDownloads;
    /** Core mode's configured proxy, as given, or null. */
    final String coreProxy;
    final boolean proxyResolveLocally;
    /** Where core mode reads HTTP_PROXY and the like: the process environment, or a test's. */
    final Map<String, String> environment;
    /** Core mode's sockets: the JDK's, or a test's in memory. */
    final Streams.Platform streamsPlatform;

    private Config(Builder b, Mode resolved) {
      endpoint = b.endpoint;
      instanceUid = b.instanceUid == null ? null : b.instanceUid.clone();
      capabilities = b.capabilities;
      heartbeatInterval = b.heartbeatInterval;
      headers = Map.copyOf(b.headers);
      requestTimeout = b.requestTimeout;
      maxSendBytes = b.maxSendBytes;
      maxReceiveBytes = b.maxReceiveBytes;
      httpClient = b.httpClient;
      tls = b.tls;
      mode = resolved;
      spliceDownloads = b.spliceDownloads;
      proxyResolveLocally = b.proxyResolveLocally;
      // The proxy URL is read as the mode reads it: the JDK's client takes HTTP proxies only.
      proxy = b.proxyUrl == null || mode == Mode.CORE ? null : Transport.Proxy.parse(b.proxyUrl);
      coreProxy = b.proxyUrl == null || mode != Mode.CORE ? null : CoreProxy.check(b.proxyUrl, b.proxyResolveLocally);
      environment = b.environment;
      streamsPlatform = b.streamsPlatform;
      direct = b.direct;
      stateFile = b.stateFile;
      acceptConnectionSettings = b.acceptConnectionSettings;
      verifyTimeout = b.verifyTimeout;
      acceptPackages = b.acceptPackages;
      requestInstanceUid = b.requestInstanceUid;
      maxPackageBytes = b.maxPackageBytes;
      downloadTimeout = b.downloadTimeout;
      stopTimeout = b.stopTimeout;
      connectTimeout = b.connectTimeout;
    }

    /**
     * Settings for a server at {@code endpoint} (http(s)://... polls, ws(s)://... keeps one
     * connection open), starting with {@code instanceUid} (16 bytes).
     */
    public static Builder builder(String endpoint, byte[] instanceUid) {
      return new Builder(endpoint, Objects.requireNonNull(instanceUid));
    }

    /**
     * Settings for a server at {@code endpoint}, with an instance UID the core makes (a UUIDv7,
     * from the clock and the operating system's randomness): {@link OpampClient#instanceUid} says
     * which. With a {@link Builder#stateFile} it is kept across restarts (the one in use is told at
     * start, as {@link Notification.InstanceUidChanged}).
     */
    public static Builder builder(String endpoint) {
      return new Builder(endpoint, null);
    }

    /** Builds a {@link Config}. */
    public static final class Builder {
      private final String endpoint;
      private final byte[] instanceUid;
      private long capabilities;
      private Duration heartbeatInterval = Duration.ZERO;
      private final Map<String, String> headers = new LinkedHashMap<>();
      private Duration requestTimeout = Duration.ZERO;
      private long maxSendBytes;
      private long maxReceiveBytes;
      private HttpClient httpClient;
      private Tls tls;
      private String proxyUrl;
      private boolean proxyResolveLocally;
      private boolean direct;
      /** Null: none named ({@link #build} picks it). */
      private Mode mode;
      private boolean spliceDownloads = true;
      private Map<String, String> environment;
      private Streams.Platform streamsPlatform = Streams.SOCKETS;
      private Path stateFile;
      private boolean acceptConnectionSettings;
      private Duration verifyTimeout = Duration.ZERO;
      private boolean acceptPackages;
      private boolean requestInstanceUid;
      private long maxPackageBytes;
      private Duration downloadTimeout = Duration.ZERO;
      private Duration stopTimeout = Duration.ZERO;
      private Duration connectTimeout = Duration.ZERO;

      private Builder(String endpoint, byte[] instanceUid) {
        this.endpoint = Objects.requireNonNull(endpoint);
        this.instanceUid = instanceUid;
      }

      /**
       * Capabilities the agent supports (AgentCapabilities bits), fixed for the run. ReportsStatus is
       * always added, and the core adds the bits of what it does itself (packages, connection
       * settings). Declare ReportsHeartbeat for heartbeats, and the Reports* bit of each status you
       * set: a status whose bit isn't declared isn't sent.
       */
      public Builder capabilities(long capabilities) {
        this.capabilities = capabilities;
        return this;
      }

      /** Heartbeat (WebSocket) or polling (HTTP) interval until the server offers one; default 30 s. */
      public Builder heartbeatInterval(Duration interval) {
        this.heartbeatInterval = interval;
        return this;
      }

      /** A header for every request or upgrade (Authorization, say). */
      public Builder header(String name, String value) {
        headers.put(name, value);
        return this;
      }

      /** How long a plain HTTP request may take (the core's deadline); default 30 s. */
      public Builder requestTimeout(Duration timeout) {
        this.requestTimeout = timeout;
        return this;
      }

      /**
       * The largest message sent, encoded; default 64 MiB, and at least 1 KiB. A status that doesn't
       * fit is left out ({@link Notification.StatusTooLarge}).
       */
      // covers: OPAMP-SIZE-02, OPAMP-SIZE-08
      public Builder maxSendBytes(long bytes) {
        this.maxSendBytes = bytes;
        return this;
      }

      /** The largest response or message accepted; default 64 MiB. */
      // covers: OPAMP-SIZE-02, OPAMP-SIZE-08
      public Builder maxReceiveBytes(long bytes) {
        this.maxReceiveBytes = bytes;
        return this;
      }

      /**
       * TLS for {@code https://} and {@code wss://} servers and for package downloads: the CAs to
       * trust, a client certificate, the versions allowed. Default: the system's CAs, no client
       * certificate, TLS 1.2 and 1.3.
       */
      public Builder tls(Tls tls) {
        this.tls = tls;
        return this;
      }

      /**
       * Reach the server, and package files, through this proxy: {@code http://}, {@code
       * https://} (TLS to the proxy, with {@link #tls} too), {@code socks5://} or {@code
       * socks5h://} (the proxy resolves the server's name, unless {@link #proxyResolveLocally}),
       * each with {@code user:password@} if it needs them. The core speaks to the proxy itself:
       * every connection through an HTTP proxy is a CONNECT tunnel, and no JVM property is needed
       * for credentials through it.
       *
       * <p>Without one (and without {@link #directConnection}), the proxy comes from the
       * environment, by the rule every SDK follows in core mode (HTTP_PROXY for http:// and
       * ws://, HTTPS_PROXY for https:// and wss://, ALL_PROXY for either, NO_PROXY: names, their
       * subdomains, addresses and CIDR networks; upper case first; none under CGI). The JDK's
       * {@code ProxySelector} and its system properties ({@code https.proxyHost} and the like)
       * are not read: they are {@link Mode#PLATFORM}'s.
       *
       * <p>In {@link Mode#PLATFORM} the JDK's HTTP client goes through the proxy: {@code
       * http://[user:password@]host[:port]} only (port 80 by default; it has no SOCKS and no
       * {@code https://} proxies, and {@link #build} refuses those URLs there). {@code https://},
       * {@code ws://} and {@code wss://} go through a CONNECT tunnel; {@code http://} requests go
       * to the proxy in absolute form. Credentials are sent as Basic Proxy-Authorization, which
       * the JDK sends through a CONNECT tunnel only when the JVM runs with {@code
       * -Djdk.http.auth.tunneling.disabledSchemes=} (set before the first HTTP client is built; it
       * is the JVM's, so this client doesn't set it). Without one, platform mode uses the
       * platform's proxy settings, as the JDK does: {@code ProxySelector.getDefault()}, which reads
       * {@code http.proxyHost}, {@code https.proxyHost}, {@code http.nonProxyHosts} and {@code
       * java.net.useSystemProxies}. {@link #directConnection} ignores them.
       */
      public Builder proxy(String url) {
        this.proxyUrl = url;
        return this;
      }

      /**
       * Through a SOCKS5 {@link #proxy}: resolve the server's name here and give the proxy its
       * address, rather than the name; default false (the proxy resolves it). It is core mode's:
       * in {@link Mode#PLATFORM} (the JDK's HTTP client has no SOCKS5) {@link #build} refuses it.
       */
      public Builder proxyResolveLocally(boolean resolveLocally) {
        this.proxyResolveLocally = resolveLocally;
        return this;
      }

      /**
       * Who speaks HTTP and WebSocket: {@link Mode#CORE}, the shared core itself, over the JDK's
       * sockets and TLS; or {@link Mode#PLATFORM}, the JDK's HTTP client. Not called: core mode,
       * unless an {@link #httpClient} is given, which only platform mode can use (core mode with
       * one is an error). An option only one mode honours is never ignored: it selects that mode,
       * or {@link #build} refuses it with what to set instead.
       */
      public Builder mode(Mode mode) {
        this.mode = Objects.requireNonNull(mode);
        return this;
      }

      /**
       * {@link Mode#CORE}: package files go from the socket to the file without passing through
       * the core, where the response says how long the body is; default true. It is core mode's:
       * {@code false} in {@link Mode#PLATFORM} is refused by {@link #build}.
       */
      public Builder spliceDownloads(boolean splice) {
        this.spliceDownloads = splice;
        return this;
      }

      /** Tests only: the environment core mode reads its proxy from (default: the process's). */
      Builder environment(Map<String, String> env) {
        this.environment = env == null ? null : Map.copyOf(env);
        return this;
      }

      /** Tests only: core mode's sockets (default: the JDK's). */
      Builder streamsPlatform(Streams.Platform platform) {
        this.streamsPlatform = Objects.requireNonNull(platform);
        return this;
      }

      /**
       * Connect straight to the server and to package files, ignoring the environment's proxy
       * (in {@link Mode#PLATFORM}, the platform's proxy settings). Not with {@link #proxy}.
       */
      public Builder directConnection() {
        this.direct = true;
        return this;
      }

      /**
       * Your own HTTP client, used as it is for OpAMP and package downloads. Giving one selects
       * {@link Mode#PLATFORM}: only the JDK's stack can use it. It replaces {@link #tls} and {@link
       * #proxy}: set TLS and proxies on it instead (giving both is an error). It must not follow
       * redirects ({@code HttpClient.Redirect.NEVER}, the JDK's default): every 3xx goes to the
       * core as it is, a download's too, and the core decides whether to follow it.
       */
      public Builder httpClient(HttpClient client) {
        if (client != null && client.followRedirects() != HttpClient.Redirect.NEVER) {
          throw new IllegalArgumentException("the HTTP client follows redirects: build it with Redirect.NEVER");
        }
        this.httpClient = client;
        return this;
      }

      /**
       * Where the client keeps state across restarts; default: nowhere. It holds secrets
       * (credentials the server issued, private keys of telemetry settings' certificates), so it
       * is written for the agent's user only: mode 0600 on POSIX, an access-control list allowing
       * that user alone on Windows. Keep it on a local path, not a shared one.
       */
      public Builder stateFile(Path path) {
        this.stateFile = path;
        return this;
      }

      /**
       * Take offers of new connection settings from the server (docs/connection-settings.md).
       * Offers are refused without a {@link #stateFile}: a restart would lose them.
       */
      public Builder acceptConnectionSettings(boolean accept) {
        this.acceptConnectionSettings = accept;
        return this;
      }

      /** How long offered settings have to prove themselves; default 60 s. */
      public Builder verifyTimeout(Duration timeout) {
        this.verifyTimeout = timeout;
        return this;
      }

      /**
       * Take packages from the server (docs/packages.md): the client downloads them beside the
       * state file (its path and {@code .downloads}) and the application installs them ({@link
       * Notification.InstallPackage}, {@link Notification.RemovePackage}). Only with a {@link
       * #stateFile}: a restart would forget what the agent has.
       */
      public Builder acceptPackages(boolean accept) {
        this.acceptPackages = accept;
        return this;
      }

      /**
       * Ask the server to assign the instance UID (the RequestInstanceUid flag,
       * docs/instance-uid.md). The UID the client starts with (the core's, or the application's) is
       * the temporary value until the server assigns one. With a {@link #stateFile} the assigned
       * UID is kept, and a restart doesn't ask again; without one every start asks again, and the
       * server assigns a new UID each time: the agent is a new agent after every restart.
       *
       * <p>covers: OPAMP-UID-04
       */
      public Builder requestInstanceUid(boolean request) {
        this.requestInstanceUid = request;
        return this;
      }

      /** The largest package file downloaded; default 1 GiB. */
      public Builder maxPackageBytes(long bytes) {
        this.maxPackageBytes = bytes;
        return this;
      }

      /**
       * How long a download may take (the core's deadline: it cancels a slower one); default 10
       * minutes.
       */
      public Builder downloadTimeout(Duration timeout) {
        this.downloadTimeout = timeout;
        return this;
      }

      /**
       * How long {@link OpampClient#stop} may take, the goodbye and any closing handshake
       * included (the core's bound: then it gives up on what is left); default 5 s.
       */
      public Builder stopTimeout(Duration timeout) {
        this.stopTimeout = timeout;
        return this;
      }

      /**
       * How long a WebSocket connection may take to open (the core's deadline: then it gives up on
       * it and tries again later); default 30 s.
       */
      public Builder connectTimeout(Duration timeout) {
        this.connectTimeout = timeout;
        return this;
      }

      /** The settings. */
      public Config build() {
        if (mode == Mode.CORE && httpClient != null) {
          throw new IllegalStateException("core mode speaks HTTP itself and takes no httpClient: set tls and proxy instead,"
              + " or don't call mode (an httpClient selects Mode.PLATFORM)");
        }
        // With no mode named: core mode, unless the application brought its own HTTP client,
        // which only platform mode can use.
        Mode resolved = mode != null ? mode : httpClient != null ? Mode.PLATFORM : Mode.CORE;
        if (resolved == Mode.PLATFORM && !spliceDownloads) {
          throw new IllegalStateException("spliceDownloads is core mode's (the JDK's HTTP client writes package files itself):"
              + " leave it out, or run in core mode (no httpClient, and mode(Mode.CORE) or none)");
        }
        if (httpClient != null && (tls != null || proxyUrl != null || direct)) {
          throw new IllegalStateException("httpClient replaces tls, proxy and directConnection: set them on your HTTP client instead");
        }
        if (proxyUrl != null && direct) {
          throw new IllegalStateException("proxy and directConnection: choose one");
        }
        if (proxyResolveLocally && resolved != Mode.CORE) {
          throw new IllegalStateException("proxyResolveLocally is core mode's (the JDK's HTTP client has no SOCKS5):"
              + " leave it out, or run in core mode (no httpClient, and mode(Mode.CORE) or none)");
        }
        if (proxyResolveLocally && proxyUrl == null) {
          throw new IllegalStateException("proxyResolveLocally goes with a socks5:// or socks5h:// proxy");
        }
        return new Config(this, resolved);
      }
    }
  }

  /**
   * The agent's status as the client starts. The first message reports it in full. {@code
   * availableComponents}: the components the agent has (leave the hash empty: the core computes
   * it); the server gets the hash, and the components in full when it asks.
   */
  public record Status(
      Opamp.AgentDescription description,
      Opamp.ComponentHealth health,
      Opamp.EffectiveConfig effectiveConfig,
      Opamp.RemoteConfigStatus remoteConfigStatus,
      Opamp.AvailableComponents availableComponents,
      Opamp.CustomCapabilities customCapabilities) {
    /** Without available components or custom capabilities. */
    public Status(
        Opamp.AgentDescription description,
        Opamp.ComponentHealth health,
        Opamp.EffectiveConfig effectiveConfig,
        Opamp.RemoteConfigStatus remoteConfigStatus) {
      this(description, health, effectiveConfig, remoteConfigStatus, null, null);
    }

    /** Without custom capabilities. */
    public Status(
        Opamp.AgentDescription description,
        Opamp.ComponentHealth health,
        Opamp.EffectiveConfig effectiveConfig,
        Opamp.RemoteConfigStatus remoteConfigStatus,
        Opamp.AvailableComponents availableComponents) {
      this(description, health, effectiveConfig, remoteConfigStatus, availableComponents, null);
    }

    /** Just the description. */
    public static Status of(Opamp.AgentDescription description) {
      return new Status(description, null, null, null, null, null);
    }
  }

  private final Core core;
  private final Executor executor;
  /** Core mode's sockets and files; null in platform mode. */
  private final Streams streams;
  private final Consumer<Notification> notify;
  private final ScheduledExecutorService thread;
  private final long start = System.nanoTime();
  private final CompletableFuture<Void> done = new CompletableFuture<>();
  /** Start has returned the client: notifications may go. */
  private final java.util.concurrent.CountDownLatch returned = new java.util.concurrent.CountDownLatch(1);
  private ScheduledFuture<?> timer;
  private boolean finished;
  /** The application has been told stopped (or is being told): never twice. */
  private boolean toldStopped;
  /** How long stop may take, as the core has it. */
  private final long stopTimeoutMs;
  /** When stop was first asked for (System.nanoTime), or null: all of stop shares one stop timeout from it. */
  private final java.util.concurrent.atomic.AtomicReference<Long> stopAsked = new java.util.concurrent.atomic.AtomicReference<>();
  /** Once the core has stopped, at least this for what it cancelled to unwind (Go's stopGrace). */
  private static final long STOP_GRACE_MS = 250;
  /** The instance UID in use, as the core last said. */
  private volatile byte[] instanceUid;
  /**
   * The application's calls, in the order made, until the client's thread feeds them: each in a
   * turn of the thread of its own ({@link #turn}), or sooner if a callback asked for it or for one
   * behind it ({@link #answer}). Guarded by itself.
   */
  private final java.util.ArrayDeque<CoreProto.Input> calls = new java.util.ArrayDeque<>();
  /**
   * How many of {@link #calls}, from the oldest, are a callback's own: as far as the last thing a
   * callback of the step under way asked for (0: none asked for anything). The client's thread only.
   */
  private int asked;
  /** Calls fed ahead of their turn ({@link #answer}): that many turns to come have nothing left to feed. */
  private int fedEarly;
  /** {@link #answer} is feeding: the steps it runs leave their callbacks' calls to it. */
  private boolean answering;

  private OpampClient(Config config, Consumer<Notification> notify, com.dylibso.chicory.wasm.WasmModule coreModule,
      String tapPath, java.util.function.UnaryOperator<Disk> disk) {
    // First what can fail: nothing is left running then.
    boolean coreMode = config.mode == Mode.CORE;
    Transport transport;
    if (config.httpClient != null) {
      transport = Transport.of(config.httpClient);
    } else if (coreMode) {
      // Platform effects reach a core-mode client only from a core that ignores streams (the
      // adversary): the JDK's client for them is made if one comes, through the proxy if it is
      // one the JDK can use, else directly.
      Transport.Proxy http = config.coreProxy != null && CoreProxy.scheme(config.coreProxy).equals("http")
          ? Transport.Proxy.parse(config.coreProxy) : null;
      transport = Transport.lazy(config.tls, http, config.direct || (config.coreProxy != null && http == null));
    } else {
      transport = Transport.build(config.tls, config.proxy, config.direct);
    }
    Streams.Engines engines = coreMode ? Streams.originEngines(config.tls) : null;
    // The transport is the endpoint's, as the core reads it: this client carries out whichever
    // effects come. The core keeps every effect's time (this client carries out its cancels),
    // decides what a download's status means, and makes the backoff's jitter seed from the
    // entropy (seed left 0: two clients started in the same tick don't retry in step).
    CoreProto.Config.Builder c = CoreProto.Config.newBuilder()
        .setEntropy(ByteString.copyFrom(entropy()))
        .setUnixMs(System.currentTimeMillis())
        .setCapabilities(config.capabilities)
        .setHeartbeatIntervalMs(ms(config.heartbeatInterval))
        .setEndpoint(config.endpoint)
        .setRequestTimeoutMs(ms(config.requestTimeout))
        .setMaxSendBytes(config.maxSendBytes)
        .setMaxReceiveBytes(config.maxReceiveBytes)
        .setAcceptConnectionSettings(config.acceptConnectionSettings)
        .setVerifyTimeoutMs(ms(config.verifyTimeout))
        .setAcceptPackages(config.acceptPackages)
        .setRequestInstanceUid(config.requestInstanceUid)
        .setMaxPackageBytes(config.maxPackageBytes)
        .setDownloadTimeoutMs(ms(config.downloadTimeout))
        .setStopTimeoutMs(ms(config.stopTimeout))
        .setConnectTimeoutMs(ms(config.connectTimeout))
        // Core mode: the core speaks HTTP and WebSocket over the streams this client opens.
        .setStreams(coreMode)
        .setSpliceDownloads(coreMode && config.spliceDownloads);
    if (coreMode) {
      // The core picks each connection's proxy from this.
      c.setProxy(proxyChoice(config));
    }
    if (config.stateFile != null) {
      // A save or a download's file work of a client stopped before on this file may still be
      // finishing: it lands before this client reads the file, not after.
      StateFile.settled(config.stateFile);
      c.setPersistence(true).setSavedState(ByteString.copyFrom(StateFile.load(config.stateFile)));
    }
    if (config.instanceUid != null) {
      c.setInstanceUid(ByteString.copyFrom(config.instanceUid));
    }
    config.headers.forEach((k, v) -> c.addHeaders(CoreProto.Header.newBuilder().setName(k).setValue(v)));
    this.core = Core.of(coreModule, c.build(), tapPath);
    this.instanceUid = core.initOutput().getInstanceUid().toByteArray();
    // All of stop takes the core's stop timeout, as given or by default: its start reports it.
    this.stopTimeoutMs = core.initOutput().getLimits().getStopTimeoutMs();
    // The file operations of saves and downloads, journaled into the core tap if there is one.
    Disk files = disk.apply(Disk.REAL);
    if (config.stateFile != null) {
      if (core.tap() != null) {
        Disk.Journal.state(core.tap(), config.stateFile);
        files = new Disk.Journal(files, core.tap(), config.stateFile);
      }
      // Files downloaded before a restart: the core downloads again what it still needs.
      Executor.deleteDownloads(files, config.stateFile);
    }
    this.notify = notify == null ? n -> {} : notify;
    ScheduledThreadPoolExecutor loop = new ScheduledThreadPoolExecutor(1, r -> {
      Thread t = new Thread(r, "opamp-client");
      t.setDaemon(true);
      return t;
    });
    // A timer or stop's bound cancelled goes at once: once shut down, the thread ends as soon as
    // nothing is left to run, not when they would have.
    loop.setRemoveOnCancelPolicy(true);
    this.thread = loop;
    this.executor = new Executor(transport, this::post, config.stateFile, files);
    this.streams = coreMode
        ? new Streams(thread, in -> {
          if (!finished) {
            step(in);
          }
        }, config.streamsPlatform, engines, proxies(config), files, config.stateFile, executor::downloadPath)
        : null;
    // Nothing is stepped, so nothing is told, until start has returned the client.
    thread.execute(() -> {
      try {
        returned.await();
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
      }
    });
  }

  /**
   * Start a client. {@code notify} is called for every notification, on the client's thread: keep
   * it quick. The first comes after start has returned; a callback that uses the client should
   * still take it from where start's caller puts it once it is there (a {@code CompletableFuture}
   * completed with it, say), as the caller may not have stored it yet.
   */
  public static OpampClient start(Config config, Status status, Consumer<Notification> notify) {
    return start(config, status, notify, null);
  }

  /**
   * Tests only: {@link #start}, running {@code coreModule} ({@link Core#parse}; null: the embedded
   * core) instead of the embedded core.
   */
  static OpampClient start(Config config, Status status, Consumer<Notification> notify,
      com.dylibso.chicory.wasm.WasmModule coreModule) {
    return start(config, status, notify, coreModule, null, d -> d);
  }

  /**
   * Tests only: {@link #start}, the core tapped to {@code tapPath} (null: as {@code OPAMP_CORE_TAP}
   * says) and the file operations of saves and downloads on {@code disk} applied to the real file
   * system (faults injected there, under the journal).
   */
  static OpampClient start(Config config, Status status, Consumer<Notification> notify,
      com.dylibso.chicory.wasm.WasmModule coreModule, String tapPath, java.util.function.UnaryOperator<Disk> disk) {
    OpampClient c = new OpampClient(config, notify, coreModule, tapPath, disk);
    if (status.description() != null) {
      c.post(CoreProto.Input.newBuilder().setSetDescription(status.description()).build());
    }
    if (status.health() != null) {
      c.post(CoreProto.Input.newBuilder().setSetHealth(status.health()).build());
    }
    if (status.effectiveConfig() != null) {
      c.post(CoreProto.Input.newBuilder().setSetEffectiveConfig(status.effectiveConfig()).build());
    }
    if (status.remoteConfigStatus() != null) {
      c.post(CoreProto.Input.newBuilder().setSetRemoteConfigStatus(status.remoteConfigStatus()).build());
    }
    if (status.availableComponents() != null) {
      c.post(CoreProto.Input.newBuilder().setSetAvailableComponents(status.availableComponents()).build());
    }
    if (status.customCapabilities() != null) {
      c.post(CoreProto.Input.newBuilder().setSetCustomCapabilities(status.customCapabilities()).build());
    }
    c.post(CoreProto.Input.newBuilder().setStart(CoreProto.Empty.getDefaultInstance()).build());
    c.returned.countDown();
    return c;
  }

  /**
   * What core mode tells the core about proxies, once ({@code Config.proxy}): the proxy
   * configured, as given; none for a direct connection; otherwise the environment's variables,
   * from which the core picks by core mode's rule ({@link CoreProxy}).
   */
  static CoreProto.ProxyChoice proxyChoice(Config config) {
    CoreProto.ProxyChoice.Builder c = CoreProto.ProxyChoice.newBuilder();
    if (config.coreProxy != null) {
      return c.setUrl(config.coreProxy).setResolveLocally(config.proxyResolveLocally).build();
    }
    if (config.direct) {
      return c.setDirect(true).build();
    }
    Map<String, String> env = config.environment != null ? config.environment : System.getenv();
    for (String name : CoreProxy.VARIABLES) {
      String v = env.get(name);
      if (v != null) {
        c.addEnvironment(CoreProto.Header.newBuilder().setName(name).setValue(v));
      }
    }
    return c.build();
  }

  /**
   * Core mode's answer to the core's proxy_query, which the core asks only when it wasn't told
   * ({@link #proxyChoice}): the proxy configured, or none (the environment's rule is the core's).
   */
  static java.util.function.Function<String, CoreProto.ProxyAnswer.Builder> proxies(Config config) {
    return url -> {
      CoreProto.ProxyAnswer.Builder a = CoreProto.ProxyAnswer.newBuilder();
      if (config.coreProxy != null) {
        a.setProxy(config.coreProxy).setResolveLocally(config.proxyResolveLocally);
      }
      return a;
    };
  }

  /**
   * {@code d} in whole milliseconds for the core (toward zero), where 0 is its default. A negative
   * one reaches the core as its two's complement, which the core refuses at start, naming the field
   * and the number meant, as in every SDK. One past a {@code long}'s milliseconds is the longest
   * there is (or, negative, the most negative).
   */
  static long ms(Duration d) {
    try {
      return d.isNegative() ? -d.negated().toMillis() : d.toMillis();
    } catch (ArithmeticException tooLong) {
      return d.isNegative() ? Long.MIN_VALUE : Long.MAX_VALUE;
    }
  }

  /** 32 bytes from the platform's CSPRNG, for the instance UID the core makes (and in core mode the WebSocket keys and masks). */
  static byte[] entropy() {
    byte[] b = new byte[32];
    new java.security.SecureRandom().nextBytes(b);
    return b;
  }

  /**
   * The instance UID in use: the application's, one saved before a restart, one the core made, or
   * one the server assigned since ({@link Notification.InstanceUidChanged} tells of a new one).
   */
  public byte[] instanceUid() {
    return instanceUid.clone();
  }

  /**
   * The endpoint the client uses now: where its latest OpAMP request or WebSocket connection went,
   * which after the server moved the agent (an offer of connection settings) is the offered one, or
   * the configured one again once an offer failed. Null before any outcome, and in core mode, where
   * the core makes the requests itself (the configured one is all there is to name then). The URL
   * is as the core gave it: redact it before logging.
   */
  public String endpoint() {
    return streams == null ? executor.used : null;
  }

  /** Tests only: the core tap's file, or null. */
  String tapPath() {
    return core.tapPath();
  }

  /** Change the agent description. */
  public void setDescription(Opamp.AgentDescription value) {
    call(CoreProto.Input.newBuilder().setSetDescription(value).build());
  }

  /** Change the health. */
  public void setHealth(Opamp.ComponentHealth value) {
    call(CoreProto.Input.newBuilder().setSetHealth(value).build());
  }

  /** Change the effective config. */
  public void setEffectiveConfig(Opamp.EffectiveConfig value) {
    call(CoreProto.Input.newBuilder().setSetEffectiveConfig(value).build());
  }

  /** Change the custom capabilities the agent supports. */
  public void setCustomCapabilities(Opamp.CustomCapabilities value) {
    call(CoreProto.Input.newBuilder().setSetCustomCapabilities(value).build());
  }

  /**
   * Send a custom message, for one of the agent's custom capabilities. It goes with the next
   * message the server takes, and again if that message is lost; if it can't be sent at all, a
   * {@link Notification.CustomMessageNotSent} says why.
   */
  public void sendCustomMessage(Opamp.CustomMessage value) {
    call(CoreProto.Input.newBuilder().setSendCustomMessage(value).build());
  }

  /** Change the components the agent has (leave the hash empty: the core computes it). */
  public void setAvailableComponents(Opamp.AvailableComponents value) {
    call(CoreProto.Input.newBuilder().setSetAvailableComponents(value).build());
  }

  /** Report on a remote config (the answer to a {@link Notification.RemoteConfig}). */
  public void setRemoteConfigStatus(Opamp.RemoteConfigStatus value) {
    call(CoreProto.Input.newBuilder().setSetRemoteConfigStatus(value).build());
  }

  /** Report on telemetry settings (the answer to a {@link Notification.ConnectionSettings}). */
  public void setConnectionSettingsStatus(Opamp.ConnectionSettingsStatus value) {
    call(CoreProto.Input.newBuilder().setSetConnectionSettingsStatus(value).build());
  }

  /**
   * Answer {@link Notification.InstallPackage}: the package at {@code hash} is installed ({@code
   * error} null), or why it isn't.
   */
  public void packageInstalled(String name, byte[] hash, String error) {
    CoreProto.PackageInstalled.Builder p = CoreProto.PackageInstalled.newBuilder()
        .setName(name).setHash(ByteString.copyFrom(hash)).setOk(error == null);
    if (error != null) {
      p.setDetail(error);
    }
    call(CoreProto.Input.newBuilder().setPackageInstalled(p).build());
  }

  /** Answer {@link Notification.RemovePackage}: the package is removed. */
  public void packageRemoved(String name) {
    packageRemoved(name, null);
  }

  /**
   * Answer {@link Notification.RemovePackage}: the package is removed ({@code error} null), or why
   * it isn't. One that isn't is reported failed, and not removed again until it is offered and
   * dropped again.
   */
  public void packageRemoved(String name, String error) {
    CoreProto.PackageRemoved.Builder p = CoreProto.PackageRemoved.newBuilder()
        .setName(name).setFailed(error != null);
    if (error != null) {
      p.setDetail(error);
    }
    call(CoreProto.Input.newBuilder().setPackageRemoved(p).build());
  }

  /**
   * Send agent_disconnect if a message can still be sent, then stop. The future completes when the
   * core says it has stopped, within {@link Config.Builder#stopTimeout} (5 s by default): nothing
   * the client started is still running then.
   */
  public CompletableFuture<Void> stop() {
    stopAsked.compareAndSet(null, System.nanoTime());
    call(CoreProto.Input.newBuilder().setStop(CoreProto.Empty.getDefaultInstance()).build());
    return done;
  }

  /** What happened (an effect's result, a timer), or what the client starts with: fed in its turn of the client's thread. */
  private void post(CoreProto.Input input) {
    try {
      thread.execute(() -> feed(input));
    } catch (java.util.concurrent.RejectedExecutionException e) {
      // stopped
    }
  }

  /**
   * One of the application's calls, from any thread: queued behind its calls before it, and fed
   * in a turn of the client's thread. The calls have a queue of their own so that those a callback
   * made can be fed ahead of their turn, in order, without passing over any made before them.
   */
  private void call(CoreProto.Input input) {
    // Queued and given its turn in one: the turns come in the queue's order, so a call fed ahead
    // of its turn is the one whose turn comes next.
    synchronized (calls) {
      calls.add(input);
      try {
        thread.execute(this::turn);
      } catch (java.util.concurrent.RejectedExecutionException e) {
        // stopped: nobody takes them
        calls.clear();
      }
    }
  }

  /** A call's turn: the oldest call still waiting is fed, unless that many were fed ahead of their turn. */
  private void turn() {
    if (fedEarly > 0) {
      fedEarly--;
      return;
    }
    CoreProto.Input input;
    synchronized (calls) {
      input = calls.poll();
    }
    if (input != null) {
      feed(input);
    }
  }

  /** How many of the application's calls are waiting. */
  private int waiting() {
    synchronized (calls) {
      return calls.size();
    }
  }

  /**
   * What the application asked from inside the callback is its answer to that step: it reaches
   * the core before anything the client takes in next (RULE-SHELL-10). So its calls are fed here,
   * as the step ends, as far as the callback's last one, in order (with what other callers asked
   * before it: none is passed over). Left to their turns, they waited behind whatever reached the
   * client's thread while the callback ran: a connection that opened, whose first message then
   * said APPLYING for a saved config the callback had answered. Calls from elsewhere keep their
   * turn: where no callback calls the client, the order is the thread's, as it was.
   *
   * <p>In core mode this runs inside the turn that fed the core a stream's bytes ({@link Streams}
   * names that stream as the source of the download_data the step makes): a call brings the core
   * no bytes, so its step makes none.
   */
  private void answer() {
    if (answering) {
      return; // a step of this loop's: what its callback asked is fed below, in order
    }
    answering = true;
    try {
      while (asked > 0 && !finished) {
        asked--;
        CoreProto.Input input;
        synchronized (calls) {
          input = calls.poll();
        }
        if (input == null) {
          break; // (Each is there: only this thread takes from the queue.)
        }
        fedEarly++;
        feed(input);
      }
    } finally {
      answering = false;
    }
  }

  /** Step the core with one input and carry out the effects. On the client's thread only. */
  private void feed(CoreProto.Input input) {
    // Nothing about an effect the core gave up on (or already has the result of) reaches it.
    if (finished || !executor.admit(input)) {
      return;
    }
    step(input);
  }

  /**
   * Step the core with {@code input} (admitted: core mode's streams decide what of theirs reaches
   * the core) and carry out the effects. On the client's thread only.
   */
  private void step(CoreProto.Input input) {
    CoreProto.Output out;
    try {
      out = core.step((System.nanoTime() - start) / 1_000_000, System.currentTimeMillis(), input);
      if (!out.getInstanceUid().isEmpty()) {
        instanceUid = out.getInstanceUid().toByteArray();
      }
      // stopped is told last: the core may emit a notification after it in the same output
      // (install_package, in the step that answers a save after stop), and nothing reaches the
      // application after stopped (RULE-SHELL-01).
      Notification stopped = null;
      for (CoreProto.Effect e : out.getEffectsList()) {
        if (e.hasNotify()) {
          Notification n = notification(e.getNotify());
          if (n instanceof Notification.Stopped) {
            stopped = n;
          } else if (n != null) {
            int before = waiting();
            notify.accept(n);
            int now = waiting();
            if (now > before) {
              asked = now;
            }
          }
        } else if (!(out.getStopped() && starts(e))) {
          // An output that says stopped starts nothing new (RULE-SHELL-07): its notifications,
          // cancels and discards only. In core mode the stream effects are Streams'; the rest
          // (a save, a save's cancel, a platform effect from a core that ignores streams) the
          // executor's.
          if (streams == null || !streams.run(e)) {
            executor.run(e);
          }
        }
      }
      if (stopped != null) {
        // Told before the call: a callback that throws on stopped hears no second one.
        toldStopped = true;
        notify.accept(stopped);
      }
    } catch (Throwable e) {
      // A CoreException, an effect this client doesn't know, or a bug in the application's
      // callback (an Error too): the client stops, and stop() reports it. A VirtualMachineError
      // goes on up after.
      end(e);
      if (e instanceof VirtualMachineError v) {
        throw v;
      }
      return;
    }
    if (timer != null) {
      timer.cancel(false);
      timer = null;
    }
    if (out.hasWakeAtMs()) {
      // A uint64: one of 2^63 ms or more (negative here) is as good as never, and waits the
      // longest the scheduler can (it never fires early).
      long at = out.getWakeAtMs();
      long now = (System.nanoTime() - start) / 1_000_000;
      long delay = at < 0 ? Long.MAX_VALUE : Math.max(0, at - now);
      CoreProto.Input tick = CoreProto.Input.newBuilder().setTimer(CoreProto.Empty.getDefaultInstance()).build();
      timer = thread.schedule(() -> feed(tick), delay, TimeUnit.MILLISECONDS);
    }
    // The core has stopped only when nothing it started is still running: release everything.
    if (out.getStopped()) {
      end(null);
    }
    answer();
  }

  /** Whether {@code e} starts something: a request, a connection, a send, a download or a save. */
  private static boolean starts(CoreProto.Effect e) {
    return switch (e.getKindCase()) {
      case HTTP_REQUEST, WS_OPEN, WS_SEND, DOWNLOAD, STORE, STREAM_OPEN -> true;
      default -> false;
    };
  }

  /** The client ends: nothing more is carried out, and the core is not stepped again. */
  private void end(Throwable error) {
    finished = true;
    if (timer != null) {
      timer.cancel(false);
    }
    // Everything still in flight is aborted; what is still finishing (a save writing) is waited
    // for, within the stop timeout.
    CompletableFuture<Void> over = executor.close();
    if (streams != null) {
      // Core mode: every stream closed at once; files still being written are deleted once their
      // write in flight is over.
      over = CompletableFuture.allOf(over, streams.shutdown());
    }
    // What the core took of the stop timeout (the goodbye, a save it waited for) is not given
    // again: all of stop takes one stop timeout from the first stop(), with a grace for what it
    // cancelled to unwind; all of it when the client stopped on its own.
    Long asked = stopAsked.get();
    long waitMs = asked == null ? stopTimeoutMs
        : Math.max(STOP_GRACE_MS, stopTimeoutMs - (System.nanoTime() - asked) / 1_000_000);
    // The bound runs on the client's thread (armed before its shutdown: a delayed task armed then
    // still runs), not on CompletableFuture.completeOnTimeout: from JDK 25 that is a ForkJoinPool
    // delay scheduler, a thread of the JDK's own that the Fray schedule tests can't control, and
    // where it ran, a schedule could never end (ShellFrayTest.stopWhileASaveIsWriting).
    CompletableFuture<Void> finishing = over;
    ScheduledFuture<?> bound = thread.schedule(() -> finishing.complete(null), waitMs, TimeUnit.MILLISECONDS);
    thread.shutdown();
    if (error != null && !toldStopped) {
      toldStopped = true;
      // The client stopped on its own: the application hears it now, not only from stop(). If its
      // callback is what failed, it may fail again: that is already the error reported.
      try {
        notify.accept(new Notification.Stopped(error));
      } catch (RuntimeException | LinkageError | AssertionError ignored) {
        // reported through stop()
      }
    }
    over.whenComplete((v, e) -> {
      bound.cancel(false); // the client's thread ends now, not when the bound would have run out
      // The tap closes once the work still finishing (a save) has been recorded, or given up on.
      core.close();
      if (error == null) {
        done.complete(null);
      } else {
        done.completeExceptionally(error);
      }
    });
  }

  private Notification notification(CoreProto.Notification n) {
    return switch (n.getKindCase()) {
      case CONNECTED -> new Notification.Connected();
      case CONNECT_FAILED -> new Notification.ConnectFailed(
          n.getConnectFailed().hasStatus() ? n.getConnectFailed().getStatus() : null, n.getConnectFailed().getDetail());
      case ERROR_RESPONSE -> new Notification.ErrorResponse(
          n.getErrorResponse().getKind(), n.getErrorResponse().getMessage());
      case REMOTE_CONFIG -> new Notification.RemoteConfig(n.getRemoteConfig());
      case CONNECTION_SETTINGS -> new Notification.ConnectionSettings(n.getConnectionSettings());
      case CUSTOM_MESSAGE -> new Notification.CustomMessage(n.getCustomMessage());
      case SERVER_CUSTOM_CAPABILITIES ->
          new Notification.ServerCustomCapabilities(java.util.List.copyOf(n.getServerCustomCapabilities().getCapabilitiesList()));
      case CUSTOM_MESSAGE_NOT_SENT -> new Notification.CustomMessageNotSent(
          n.getCustomMessageNotSent().getMessage(), n.getCustomMessageNotSent().getReason());
      case INSTALL_PACKAGE -> new Notification.InstallPackage(n.getInstallPackage().getName(),
          n.getInstallPackage().getPackage(), executor.downloadPath(n.getInstallPackage().getDownload()));
      case REMOVE_PACKAGE -> new Notification.RemovePackage(n.getRemovePackage().getName());
      case COMMAND -> new Notification.Command(n.getCommand());
      case INSTANCE_UID_CHANGED -> new Notification.InstanceUidChanged(n.getInstanceUidChanged().toByteArray());
      case HEARTBEAT_INTERVAL_MS -> new Notification.HeartbeatInterval(
          Duration.ofMillis(n.getHeartbeatIntervalMs() < 0 ? Long.MAX_VALUE : n.getHeartbeatIntervalMs()));
      case STATUS_TOO_LARGE -> new Notification.StatusTooLarge(n.getStatusTooLarge().getName(), n.getStatusTooLarge().getBytes());
      case BAD_MESSAGE -> new Notification.BadMessage(n.getBadMessage().getDetail());
      case SAVE_STALLED -> new Notification.SaveStalled(n.getSaveStalled().getStalled(),
          Duration.ofMillis(n.getSaveStalled().getWaitedMs() < 0 ? Long.MAX_VALUE : n.getSaveStalled().getWaitedMs()));
      case STOPPED -> new Notification.Stopped(null);
      default -> null;
    };
  }
}
