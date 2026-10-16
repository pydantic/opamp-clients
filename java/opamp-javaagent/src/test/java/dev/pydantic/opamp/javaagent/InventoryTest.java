// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import com.sun.net.httpserver.HttpServer;
import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.common.ComponentLoader;
import io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdk;
import io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdkBuilder;
import io.opentelemetry.sdk.resources.Resource;
import java.io.ByteArrayInputStream;
import java.net.InetSocketAddress;
import java.net.URL;
import java.net.URLClassLoader;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;
import java.util.function.Predicate;
import opamp.proto.v1.Anyvalue;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * The inventory (docs/javaagent.md#inventory): what the extension lists of the SDK the agent
 * built, and that the client sends it.
 */
class InventoryTest {
  /** Every setting the agent carries out, and when: its host declares all the newer ones. */
  static final Map<String, String> SETTINGS = settings();

  private static Map<String, String> settings() {
    Map<String, String> m = new TreeMap<>();
    for (String k : List.of("debug_sampling_rate", "debug_send_logs_level", "debug_until", "drop_attributes",
        "mask_attributes", "sampling_rate", "sampling_rules", "sampling_traces_per_second", "send_logs",
        "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second", "send_metrics", "send_traces")) {
      m.put(k, "at once");
    }
    for (String k : List.of("deactivate_all_instrumentations", "deactivate_instrumentations", "opamp_polling_interval")) {
      m.put(k, "next start");
    }
    return m;
  }

  /**
   * The SDK autoconfigure builds from {@code props}, through the extension's customizers around
   * {@code e} (and not the agent's instance, which the class path's provider would use).
   */
  static Sdk sdk(Extension e, Map<String, String> props) {
    ComponentLoader all = ComponentLoader.forClassLoader(InventoryTest.class.getClassLoader());
    ComponentLoader others = new ComponentLoader() {
      @Override
      public <T> Iterable<T> load(Class<T> type) {
        List<T> out = new ArrayList<>();
        all.load(type).forEach(p -> {
          if (!(p instanceof OpampCustomizerProvider)) {
            out.add(p);
          }
        });
        return out;
      }
    };
    AutoConfiguredOpenTelemetrySdkBuilder b = AutoConfiguredOpenTelemetrySdk.builder().setComponentLoader(others)
        .disableShutdownHook().addPropertiesSupplier(() -> props);
    new OpampCustomizerProvider(e).customize(b);
    return new Sdk(b.build());
  }

  /** An SDK, shut down at the end of a try block. */
  record Sdk(AutoConfiguredOpenTelemetrySdk built) implements AutoCloseable {
    @Override
    public void close() {
      built.getOpenTelemetrySdk().close();
    }
  }

  /** The properties of an agent under {@code server}, every signal to the console, then {@code more}. */
  static Map<String, String> props(String server, Path dir, String... more) {
    Map<String, String> p = new HashMap<>(Map.of("otel.opamp.endpoint", server, "otel.opamp.state.dir",
        dir.toString(), "otel.service.name", "checkout", "otel.traces.exporter", "console",
        "otel.metrics.exporter", "console", "otel.logs.exporter", "console"));
    for (int i = 0; i < more.length; i += 2) {
      p.put(more[i], more[i + 1]);
    }
    return p;
  }

  /** The inventory the extension reports for an SDK built from {@code props}. */
  static Opamp.AvailableComponents components(Map<String, String> props) {
    Extension e = Extension.forTest(null);
    try (Sdk sdk = sdk(e, props)) {
      assertTrue(e.on());
      return Vectors.LAYER.components(e.inventory.build(), e.host(), Vectors.NOW);
    }
  }

  /** A sub-component's metadata, by key. */
  static Map<String, Object> metadata(Opamp.ComponentDetails d) {
    Map<String, Object> m = new LinkedHashMap<>();
    for (Anyvalue.KeyValue kv : d.getMetadataList()) {
      m.put(kv.getKey(), kv.getValue().hasBoolValue() ? kv.getValue().getBoolValue() : kv.getValue().getStringValue());
    }
    return m;
  }

  /** The exporters listed: signal to name to gated. */
  static Map<String, Map<String, Object>> exporters(Opamp.AvailableComponents c) {
    Map<String, Map<String, Object>> out = new TreeMap<>();
    c.getComponentsMap().get("exporters").getSubComponentMapMap().forEach((signal, d) -> {
      Map<String, Object> names = new TreeMap<>();
      d.getSubComponentMapMap().forEach((name, x) -> names.put(name, metadata(x).get("gated")));
      out.put(signal, names);
    });
    return out;
  }

  /** The instrumentations listed: name to version ("" when unknown). */
  static Map<String, Object> instrumentations(Opamp.AvailableComponents c) {
    Map<String, Object> out = new TreeMap<>();
    c.getComponentsMap().get("instrumentations").getSubComponentMapMap()
        .forEach((name, d) -> out.put(name, metadata(d).getOrDefault("version", "")));
    return out;
  }

  @Test
  void aTypicalAgentListsItsPackageTheSdkItsInstrumentationsAndItsGatedExporters(@TempDir Path dir) {
    Opamp.AvailableComponents c = components(props("http://server/v1/opamp", dir));
    assertTrue(c.getHash().isEmpty(), "the client computes the hash");

    Map<String, Object> sdk = metadata(c.getComponentsMap().get("sdk"));
    assertEquals("dev.pydantic:opamp-javaagent-extension", sdk.get("integration.name"));
    String version = (String) sdk.get("integration.version");
    assertTrue(version.matches("\\d+\\.\\d+\\.\\d+.*"), "the version Maven built: " + version);
    assertEquals("java", sdk.get("telemetry.sdk.language"));
    assertEquals(Resource.getDefault().getAttribute(io.opentelemetry.api.common.AttributeKey.stringKey(
        "telemetry.sdk.version")), sdk.get("telemetry.sdk.version"));
    assertTrue(((String) sdk.get("telemetry.sdk.version")).startsWith("1.66."), sdk.toString());

    // Every name each module answers to, off by default or not; a version where one is embedded.
    // The module the agent can't make is left out.
    assertEquals(Map.of("jdbc", "", "spring-webmvc", "2.31.1-alpha", "spring-webmvc-6.0", "2.31.1-alpha"),
        instrumentations(c));

    // Each exporter the SDK built, as otel.*.exporter names it, in the extension's gate.
    assertEquals(Map.of("traces", Map.of("console", true), "metrics", Map.of("console", true),
        "logs", Map.of("console", true)), exporters(c));

    // The settings it carries out, as its host declares them.
    Map<String, Object> settings = new TreeMap<>();
    c.getComponentsMap().get("settings").getSubComponentMapMap()
        .forEach((k, d) -> settings.put(k, metadata(d).get("applies")));
    assertEquals(SETTINGS, settings);
  }

  @Test
  void theInstrumentationsAreListedByTheNamesTheInstrumentationsControlTakes(@TempDir Path dir) {
    Opamp.AvailableComponents c = components(props("http://server/v1/opamp", dir));
    List<String> names = new ArrayList<>(instrumentations(c).keySet());
    // Loaded at start, as a saved document: next-start settings are in force there.
    String disabled = names.stream().map(n -> "\"" + n + "\"").collect(java.util.stream.Collectors.joining(","));
    SettingsProto.Settings s = Vectors.LAYER.load(Vectors.every("instrumentations",
        Vectors.override("\"disabled\":[" + disabled + "]")).getBytes(StandardCharsets.UTF_8), Extension.HOST,
        Vectors.NOW).getSettings();
    // Each name switches off exactly that instrumentation, as the agent reads its property.
    Map<String, String> want = new LinkedHashMap<>();
    names.forEach(n -> want.put("otel.instrumentation." + n + ".enabled", "false"));
    assertEquals(want, Controls.startProperties(s));
  }

  @Test
  void anExporterSetToNoneIsLeftOutAndOneOfAnotherLibraryIsListedByItsName(@TempDir Path dir) {
    // (The SDK refuses a name given twice, so autoconfigure never builds duplicates.)
    Opamp.AvailableComponents c = components(props("http://server/v1/opamp", dir, "otel.traces.exporter",
        "console,recording", "otel.metrics.exporter", "none", "otel.logs.exporter", "none"));
    assertEquals(Map.of("traces", Map.of("console", true, "recording", true)), exporters(c));
  }

  @Test
  void withNoExporterConfiguredTheSdksDefaultOtlpIsListed() {
    Inventory inventory = new Inventory();
    inventory.configured("traces", io.opentelemetry.sdk.autoconfigure.spi.internal.DefaultConfigProperties
        .createFromMap(Map.of()));
    assertEquals(List.of(Inventory.exporter("traces", "otlp", true)), inventory.build().getExportersList());
  }

  @Test
  void outsideTheAgentThereAreNoInstrumentations() {
    assertEquals(List.of(), Inventory.instrumentations(new URLClassLoader(new URL[0], null)));
    Inventory inventory = new Inventory();
    inventory.instrumentations = List::of;
    Opamp.AvailableComponents c = Vectors.LAYER.components(inventory.build(), Extension.HOST, Vectors.NOW);
    assertEquals(0, c.getComponentsMap().get("instrumentations").getSubComponentMapCount());
    assertEquals(0, c.getComponentsMap().get("exporters").getSubComponentMapCount());
    // The sdk entry all the same.
    assertEquals("dev.pydantic:opamp-javaagent-extension",
        metadata(c.getComponentsMap().get("sdk")).get("integration.name"));
  }

  @Test
  void underAConfigurationFileEachExporterIsListedOnceByItsNameAndAProcessorOfItsOwnIsUngated() {
    Inventory inventory = new Inventory();
    Declarative.wrap(DeclarativeTest.model("""
        file_format: "1.1"
        tracer_provider:
          processors:
            - batch:
                exporter:
                  otlp_http:
            - simple:
                exporter:
                  console:
            - kafka_processor:
                topic: spans
        meter_provider:
          readers:
            - periodic:
                exporter:
                  otlp_grpc:
            - pull:
                exporter:
                  prometheus/development:
        logger_provider:
          processors:
            - batch:
                exporter:
                  otlp_http:
            - batch:
                exporter:
                  otlp_grpc:
        """), inventory);
    Opamp.AvailableComponents c = Vectors.LAYER.components(inventory.build(), Extension.HOST, Vectors.NOW);
    assertEquals(Map.of("traces", Map.of("otlp", true, "console", true, "kafka_processor", false),
        "metrics", Map.of("otlp", true, "prometheus", true), "logs", Map.of("otlp", true)), exporters(c));
  }

  @Test
  void aSignalTheFileLeavesOutHasNoExporters() {
    Inventory inventory = new Inventory();
    Declarative.wrap(DeclarativeTest.model("""
        file_format: "1.1"
        tracer_provider:
          processors: []
        """), inventory);
    assertEquals(List.of(), inventory.build().getExportersList());
  }

  /**
   * A server of OpAMP over plain HTTP: each AgentToServer, and the answer it gets (the first asks for
   * the components in full).
   */
  static final class Server implements AutoCloseable {
    final HttpServer http;
    final LinkedBlockingQueue<Opamp.AgentToServer> got = new LinkedBlockingQueue<>();
    private boolean asked;

    Server() throws Exception {
      http = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
      http.createContext("/v1/opamp", x -> {
        Opamp.AgentToServer m = Opamp.AgentToServer.parseFrom(new ByteArrayInputStream(x.getRequestBody().readAllBytes()));
        got.add(m);
        Opamp.ServerToAgent.Builder r = Opamp.ServerToAgent.newBuilder().setInstanceUid(m.getInstanceUid());
        synchronized (this) {
          if (!asked) {
            asked = true;
            r.setFlags(Opamp.ServerToAgentFlags.ServerToAgentFlags_ReportAvailableComponents_VALUE);
          }
        }
        byte[] body = r.build().toByteArray();
        x.getResponseHeaders().set("Content-Type", "application/x-protobuf");
        x.sendResponseHeaders(200, body.length);
        x.getResponseBody().write(body);
        x.close();
      });
      http.start();
    }

    String endpoint() {
      return "http://127.0.0.1:" + http.getAddress().getPort() + "/v1/opamp";
    }

    /** The first message from now that {@code p} holds for. */
    Opamp.AgentToServer next(Predicate<Opamp.AgentToServer> p) throws InterruptedException {
      long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(20);
      while (System.nanoTime() < deadline) {
        Opamp.AgentToServer m = got.poll(100, TimeUnit.MILLISECONDS);
        if (m != null && p.test(m)) {
          return m;
        }
      }
      throw new AssertionError("no such message from the agent");
    }

    @Override
    public void close() {
      http.stop(0);
    }
  }

  @Test
  void theClientSendsTheHashAtOnceAndTheComponentsInFullWhenAsked(@TempDir Path dir) throws Exception {
    try (Server server = new Server()) {
      Extension e = Extension.forTest(null, new AppSettingsExtensionTest.App().sink); // the application reads its settings
      try (Sdk sdk = sdk(e, props(server.endpoint(), dir, "otel.opamp.heartbeat.interval", "200ms"))) {
        e.start(null);
        Opamp.AvailableComponents sent = e.components();
        assertEquals(Vectors.LAYER.components(e.inventory.build(), e.host(), Vectors.NOW).getComponentsMap(),
            sent.getComponentsMap());
        // The application's own settings, read through the agent, are listed with the rest.
        assertTrue(sent.getComponentsMap().get("settings").containsSubComponentMap("custom"));

        // The first message: the capability, and the hash alone.
        Opamp.AgentToServer first = server.next(m -> true);
        assertTrue((first.getCapabilities() & Opamp.AgentCapabilities.AgentCapabilities_ReportsAvailableComponents_VALUE)
            != 0, "declares ReportsAvailableComponents");
        assertTrue(first.hasAvailableComponents());
        ByteString hash = first.getAvailableComponents().getHash();
        assertFalse(hash.isEmpty(), "the hash is set");
        assertEquals(0, first.getAvailableComponents().getComponentsCount());

        // Asked, the components in full, under the same hash: what the extension built.
        Opamp.AgentToServer full = server.next(m -> m.getAvailableComponents().getComponentsCount() > 0);
        assertEquals(hash, full.getAvailableComponents().getHash());
        assertEquals(sent.getComponentsMap(), full.getAvailableComponents().getComponentsMap());
        assertEquals(Map.of("console", true), exporters(full.getAvailableComponents()).get("traces"));
        assertEquals("dev.pydantic:opamp-javaagent-extension",
            metadata(full.getAvailableComponents().getComponentsMap().get("sdk")).get("integration.name"));
      }
    }
  }

  @Test
  void theSameAgentHasTheSameHashAndAnotherExporterAnother(@TempDir Path dir) throws Exception {
    List<ByteString> hashes = new ArrayList<>();
    for (String traces : List.of("console", "console", "console,recording")) {
      try (Server server = new Server()) {
        Extension e = Extension.forTest(null);
        try (Sdk sdk = sdk(e, props(server.endpoint(), dir.resolve(String.valueOf(hashes.size())),
            "otel.traces.exporter", traces))) {
          e.start(null);
          hashes.add(server.next(Opamp.AgentToServer::hasAvailableComponents).getAvailableComponents().getHash());
        }
      }
    }
    assertEquals(hashes.get(0), hashes.get(1));
    assertNotEquals(hashes.get(0), hashes.get(2));
  }
}
