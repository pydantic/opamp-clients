// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

import dev.pydantic.opamp.Notification;
import dev.pydantic.opamp.OpampClient;
import dev.pydantic.opamp.Tls;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.List;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import opamp.proto.v1.Anyvalue;
import opamp.proto.v1.Opamp;

/**
 * Runs our Java client (java/opamp: the shared core in Chicory) under the conformance driver
 * contract (conformance/DRIVERS.md). Run as a single source file on the client's classpath.
 * The mode is always asked for by name, never left to the client's default: java-core is Java
 * mode (the JDK's HTTP client), java-core-rust ({@code OPAMP_JAVA_MODE=rust}) Rust mode (the core
 * speaks HTTP and WebSocket over the JDK's sockets), where DRIVER_PROXY may be a SOCKS5 proxy too.
 */
public class Driver {
  /** The client stopped on its own: the driver no longer stops it, and exits 1. */
  static final AtomicBoolean FAILED = new AtomicBoolean();

  static synchronized void emit(String event, Object... kv) {
    StringBuilder b = new StringBuilder("{\"event\":\"").append(event).append('"');
    for (int i = 0; i + 1 < kv.length; i += 2) {
      b.append(",\"").append(kv[i]).append("\":");
      Object v = kv[i + 1];
      if (v == null) {
        b.append("null");
      } else if (v instanceof Number) {
        b.append(v);
      } else {
        b.append('"').append(v.toString().replace("\\", "\\\\").replace("\"", "\\\"")).append('"');
      }
    }
    b.append(",\"wall\":").append(System.currentTimeMillis() / 1000.0).append('}');
    System.out.println(b);
    System.out.flush();
  }

  static double envDouble(String key, double def) {
    try {
      return Double.parseDouble(System.getenv(key));
    } catch (RuntimeException e) {
      return def;
    }
  }

  /** All zeros, or the 32 hex characters given; null ({@code random}, {@code none}): the core makes one. */
  static byte[] instanceUid() {
    String v = System.getenv().getOrDefault("DRIVER_UID", "random");
    if (v.equals("zero")) {
      return new byte[16];
    }
    if (!v.isEmpty() && !v.equals("random") && !v.equals("none")) {
      return HexFormat.of().parseHex(v);
    }
    return null;
  }

  static Anyvalue.KeyValue kv(String k, String v) {
    return Anyvalue.KeyValue.newBuilder().setKey(k).setValue(Anyvalue.AnyValue.newBuilder().setStringValue(v)).build();
  }

  /** The custom capability DRIVERS.md names: ping is answered with pong. */
  static final String ECHO = "io.opentelemetry.echo";

  /** The components DRIVERS.md names: an OTLP receiver (with its version) and a debug exporter. */
  static Opamp.AvailableComponents components() {
    Opamp.ComponentDetails otlp = Opamp.ComponentDetails.newBuilder().addMetadata(kv("version", "1.0.0")).build();
    return Opamp.AvailableComponents.newBuilder()
        .putComponents("receivers", Opamp.ComponentDetails.newBuilder().putSubComponentMap("otlp", otlp).build())
        .putComponents("exporters",
            Opamp.ComponentDetails.newBuilder().putSubComponentMap("debug", Opamp.ComponentDetails.getDefaultInstance()).build())
        .build();
  }

  static Opamp.AgentDescription description(int change) {
    Opamp.AgentDescription.Builder d = Opamp.AgentDescription.newBuilder()
        .addIdentifyingAttributes(kv("service.name", "conformance-java-core"))
        .addNonIdentifyingAttributes(kv("driver.change", Integer.toString(change)));
    // DRIVER_DESCRIPTION_PAD: a description that long (with DRIVER_MAX_SEND_BYTES, too large to send).
    String pad = System.getenv().getOrDefault("DRIVER_DESCRIPTION_PAD", "");
    if (!pad.isEmpty()) {
      d.addNonIdentifyingAttributes(kv("driver.pad", "x".repeat(Integer.parseInt(pad))));
    }
    return d.build();
  }

  public static void main(String[] args) throws Exception {
    boolean rust = "rust".equals(System.getenv("OPAMP_JAVA_MODE"));
    byte[] uid = instanceUid();
    // The callback runs on the client's own thread, from before start returns (a remote config
    // saved by an earlier run is handed over on its first step): it waits for the client.
    CompletableFuture<OpampClient> client = new CompletableFuture<>();
    String endpoint = System.getenv().getOrDefault("OPAMP_ENDPOINT", "");
    OpampClient.Config.Builder builder = (uid == null
        ? OpampClient.Config.builder(endpoint)
        : OpampClient.Config.builder(endpoint, uid))
        .capabilities(Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE)
        .heartbeatInterval(Duration.ofMillis((long) (envDouble("DRIVER_POLL_S", 2) * 1000)))
        .mode(rust ? OpampClient.Mode.RUST : OpampClient.Mode.JAVA);
    String maxSend = System.getenv().getOrDefault("DRIVER_MAX_SEND_BYTES", "");
    if (!maxSend.isEmpty()) {
      builder.maxSendBytes(Long.parseLong(maxSend));
    }
    String token = System.getenv().getOrDefault("DRIVER_TOKEN", "");
    if (!token.isEmpty()) {
      builder.header("Authorization", "Bearer " + token);
    }
    String stateFile = System.getenv().getOrDefault("DRIVER_STATE_FILE", "");
    if (!stateFile.isEmpty()) {
      builder.stateFile(Path.of(stateFile));
    }
    // TLS: the CA to trust instead of the system's, and a client certificate when the server asks.
    String ca = System.getenv().getOrDefault("DRIVER_CA_FILE", "");
    String cert = System.getenv().getOrDefault("DRIVER_CLIENT_CERT", "");
    String key = System.getenv().getOrDefault("DRIVER_CLIENT_KEY", "");
    if (!ca.isEmpty() || !cert.isEmpty()) {
      Tls.Builder tls = Tls.builder();
      if (!ca.isEmpty()) {
        tls.trustedCa(Files.readString(Path.of(ca)));
      }
      if (!cert.isEmpty()) {
        tls.clientCertificate(Files.readString(Path.of(cert)), Files.readString(Path.of(key)));
      }
      builder.tls(tls.build());
    }
    // Package files come the same way as messages: the same TLS (DRIVER_CA_FILE) and proxy, and no
    // redirect followed (the core decides).
    // An HTTP proxy. Its credentials go through CONNECT tunnels only because clients.toml runs
    // the JVM with -Djdk.http.auth.tunneling.disabledSchemes= (java/opamp/README.md). In Rust mode
    // the core speaks to the proxy itself: http://, https://, socks5:// or socks5h://, with
    // credentials (no JVM property needed).
    String proxy = System.getenv().getOrDefault("DRIVER_PROXY", "");
    if (!proxy.isEmpty()) {
      builder.proxy(proxy);
    }
    builder.acceptConnectionSettings("1".equals(System.getenv("DRIVER_ACCEPT_CONNECTION_SETTINGS")));
    builder.acceptPackages("1".equals(System.getenv("DRIVER_PACKAGES")));
    builder.requestInstanceUid("1".equals(System.getenv("DRIVER_REQUEST_UID")));
    if ("1".equals(System.getenv("DRIVER_OWN_TELEMETRY"))) {
      builder.capabilities(Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE
          | Opamp.AgentCapabilities.AgentCapabilities_ReportsOwnMetrics_VALUE
          | Opamp.AgentCapabilities.AgentCapabilities_ReportsOwnTraces_VALUE
          | Opamp.AgentCapabilities.AgentCapabilities_ReportsOwnLogs_VALUE);
    }
    OpampClient.Config config = builder.build();
    // DRIVER_THROW_ON_CONFIG: the callback itself throws on the first remote config (RULE-SHELL-02).
    boolean throwOnConfig = "1".equals(System.getenv("DRIVER_THROW_ON_CONFIG"));
    AtomicBoolean thrown = new AtomicBoolean();
    // The status to start from, a restart after a crash going on from where the application was
    // (DRIVER_FIRST_CHANGE): the changes count on from it.
    String firstEnv = System.getenv().getOrDefault("DRIVER_FIRST_CHANGE", "").trim();
    int first = firstEnv.isEmpty() ? 0 : Integer.parseInt(firstEnv);
    OpampClient.Status status = new OpampClient.Status(
        description(first),
        Opamp.ComponentHealth.newBuilder()
            .setHealthy(true)
            .setStartTimeUnixNano(System.currentTimeMillis() * 1_000_000L)
            .build(),
        Opamp.EffectiveConfig.newBuilder().setConfigMap(Opamp.AgentConfigMap.getDefaultInstance()).build(),
        null,
        "1".equals(System.getenv("DRIVER_COMPONENTS")) ? components() : null,
        "1".equals(System.getenv("DRIVER_CUSTOM"))
            ? Opamp.CustomCapabilities.newBuilder().addCapabilities(ECHO).build()
            : null);
    try {
      client.complete(OpampClient.start(config, status, n -> {
        switch (n) {
          case Notification.Stopped s when s.error() != null -> {
            // The client stopped on its own. Stay DRIVER_LINGER_S, still reporting what the client
            // says (nothing, if it keeps RULE-SHELL-02: a second stopped is a second fatal), then
            // exit 1.
            emit("fatal", "error", s.error().toString());
            if (FAILED.compareAndSet(false, true)) {
              Thread exit = new Thread(() -> {
                try {
                  Thread.sleep((long) (envDouble("DRIVER_LINGER_S", 0) * 1000));
                } catch (InterruptedException e) {
                  Thread.currentThread().interrupt();
                }
                System.exit(1);
              });
              exit.start();
            }
          }
          case Notification.RemoteConfig r when throwOnConfig && thrown.compareAndSet(false, true) -> {
            emit("throwing");
            throw new RuntimeException("callback-throws: the driver's callback threw on the first remote config");
          }
          case Notification.Connected c -> emit("connect");
          case Notification.ConnectFailed f -> emit("connect_failed", "error", f.detail(), "status", f.status());
          case Notification.ErrorResponse e -> emit("error_response", "type", e.type(), "message", e.message());
          case Notification.RemoteConfig r -> {
            emit("message", "remote_config_hash", HexFormat.of().formatHex(r.config().getConfigHash().toByteArray()));
            client.join().setRemoteConfigStatus(Opamp.RemoteConfigStatus.newBuilder()
                .setLastRemoteConfigHash(r.config().getConfigHash())
                .setStatus(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED)
                .build());
          }
          case Notification.CustomMessage cm -> {
            Opamp.CustomMessage m = cm.message();
            emit("custom_message", "capability", m.getCapability(), "type", m.getType(),
                "data", HexFormat.of().formatHex(m.getData().toByteArray()));
            if (m.getType().equals("ping")) {
              client.join().sendCustomMessage(
                  Opamp.CustomMessage.newBuilder().setCapability(ECHO).setType("pong").setData(m.getData()).build());
            }
          }
          case Notification.CustomMessageNotSent ns -> emit("custom_message_not_sent", "reason", ns.reason());
          case Notification.InstallPackage ip -> {
            String sha = null;
            String error = null;
            try {
              sha = HexFormat.of().formatHex(
                  java.security.MessageDigest.getInstance("SHA-256").digest(java.nio.file.Files.readAllBytes(ip.path())));
            } catch (java.io.IOException | java.security.NoSuchAlgorithmException e) {
              error = String.valueOf(e);
            }
            emit("install_package", "name", ip.name(), "version", ip.pkg().getVersion(), "sha256", sha, "error", error);
            client.join().packageInstalled(ip.name(), ip.pkg().getHash().toByteArray(), error);
          }
          case Notification.RemovePackage rp -> {
            emit("remove_package", "name", rp.name());
            client.join().packageRemoved(rp.name());
          }
          case Notification.ServerCustomCapabilities sc ->
              emit("server_custom_capabilities", "capabilities", String.join(",", sc.capabilities()));
          case Notification.ConnectionSettings s -> {
            Opamp.ConnectionSettingsOffers offers = s.settings();
            emit("telemetry_settings", "hash", HexFormat.of().formatHex(offers.getHash().toByteArray()),
                "metrics_endpoint", offers.hasOwnMetrics() ? offers.getOwnMetrics().getDestinationEndpoint() : null);
            client.join().setConnectionSettingsStatus(Opamp.ConnectionSettingsStatus.newBuilder()
                .setLastConnectionSettingsHash(offers.getHash())
                .setStatus(Opamp.ConnectionSettingsStatuses.ConnectionSettingsStatuses_APPLIED)
                .build());
          }
          case Notification.InstanceUidChanged u ->
              emit("message", "new_instance_uid", HexFormat.of().formatHex(u.instanceUid()));
          case Notification.HeartbeatInterval h ->
              emit("connection_settings", "heartbeat_interval_seconds", h.interval().toSeconds());
          default -> {}
        }
      }));
    } catch (RuntimeException e) {
      emit("fatal", "error", e.toString());
      System.exit(1);
    }
    // The UID in use: the one given, or the one the core made.
    emit("started", "uid", HexFormat.of().formatHex(client.join().instanceUid()), "client", "opamp-java-core (ours" + (rust ? ", rust mode" : "") + ")");

    List<Double> changes = new ArrayList<>();
    for (String s : System.getenv().getOrDefault("DRIVER_CHANGES_AT_S", "").split(",")) {
      try {
        changes.add(Double.parseDouble(s.trim()));
      } catch (NumberFormatException e) {
        // not a number: skip
      }
    }
    long started = System.nanoTime();
    long runUntil = started + (long) (envDouble("DRIVER_RUN_S", 30) * 1e9);
    CountDownLatch signal = new CountDownLatch(1);
    Runtime.getRuntime().addShutdownHook(new Thread(signal::countDown));
    int next = 0;
    while (System.nanoTime() < runUntil && signal.getCount() > 0 || FAILED.get()) {
      if (next < changes.size() && System.nanoTime() >= started + (long) (changes.get(next) * 1e9)) {
        next++;
        emit("status_change", "change", first + next);
        client.join().setDescription(description(first + next));
      }
      signal.await(20, TimeUnit.MILLISECONDS);
    }
    emit("stopping");
    String error = null;
    try {
      // Bounded by the core (its stop timeout): no race of our own.
      client.join().stop().get();
    } catch (Exception e) {
      error = e.toString();
    }
    emit("stopped", "error", error);
    // Stay a while, if asked: anything the client left running would still show (DRIVER_LINGER_S).
    double linger = envDouble("DRIVER_LINGER_S", 0);
    if (linger > 0) {
      Thread.sleep((long) (linger * 1000));
    }
    System.exit(0);
  }
}
