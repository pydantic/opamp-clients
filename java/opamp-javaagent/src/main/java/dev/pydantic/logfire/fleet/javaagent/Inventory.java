// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.logfire.fleet.javaagent;

import dev.pydantic.logfire.fleet.sdk.SdkSettings;
import dev.pydantic.logfire.fleet.sdk.SettingsProto;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.javaagent.extension.instrumentation.InstrumentationModule;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import io.opentelemetry.sdk.resources.Resource;
import java.io.IOException;
import java.io.InputStream;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.HashSet;
import java.util.Iterator;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Properties;
import java.util.ServiceConfigurationError;
import java.util.ServiceLoader;
import java.util.Set;
import java.util.TreeMap;
import java.util.function.Supplier;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * What the agent runs, for the layer's Components call (docs/available-components.md): the
 * extension's package and version, the SDK's, the agent's instrumentations under the names
 * {@code deactivate_instrumentations} takes, and the exporters the SDK was built with. Thread-safe.
 */
final class Inventory {
  /** The extension's package, as published: its Maven coordinates. */
  static final String PACKAGE = "dev.pydantic:logfire-fleet-javaagent-extension";
  /** Its version, as Maven built it ({@code version.properties}); empty if the jar doesn't say. */
  static final String VERSION = property(Inventory.class.getResourceAsStream("version.properties"));
  private static final Logger LOG = Logger.getLogger(Extension.class.getName());
  private static final AttributeKey<String> SDK_LANGUAGE = AttributeKey.stringKey("telemetry.sdk.language");
  private static final AttributeKey<String> SDK_VERSION = AttributeKey.stringKey("telemetry.sdk.version");

  /** The exporters the SDK was built with, by signal, as found (the customizers or the model). */
  private final Map<String, List<SettingsProto.FoundExporter>> exporters = new TreeMap<>();
  /** What another extension added past the gates, by signal ({@link #outside}). */
  private final Map<String, List<SettingsProto.FoundExporter>> outside = new TreeMap<>();
  /** The {@code OTEL_EXPORTER_*} settings autoconfigure read, by their variables' names ({@link #configured}). */
  private final Map<String, String> env = new TreeMap<>();
  /** What decides the listed exporters from the found ones: the extension's layer. */
  Supplier<SdkSettings> layer = Inventory::sharedLayer;
  /** Each change to what was found, and the change {@link #decided} was decided at. */
  private long changes;
  private long decidedAt = -1;
  private List<SettingsProto.Exporter> decided = List.of();
  /** Each exporter as the layer last listed it, by what was found: should a call fail, it keeps its name. */
  private final Map<SettingsProto.FoundExporter, SettingsProto.Exporter> listedAs = new HashMap<>();
  /** The layer's warnings said so far: each once. */
  private final Set<String> warned = new HashSet<>();
  /** The instrumentations the agent can run: its InstrumentationModules, read once, at start. */
  Supplier<List<SettingsProto.Instrumentation>> instrumentations =
      () -> instrumentations(Inventory.class.getClassLoader());

  /**
   * Under autoconfigure, from the customizer an exporter of {@code signal} ({@code traces},
   * {@code metrics} or {@code logs}) went through: the SDK builds one for each name
   * {@code otel.<signal>.exporter} gives ({@code otlp} when unset; {@code none}: none), and every
   * one of them is in the extension's gate. Each with where it sends ({@link #destination}).
   */
  void configured(String signal, ConfigProperties config) {
    List<SettingsProto.FoundExporter> built = new ArrayList<>();
    for (String name : names(config, signal)) {
      built.add(found(signal, name, true, "").toBuilder().setFromEnv(true).build());
    }
    synchronized (this) {
      // The settings the layer reads to say where they send: autoconfigure's, whether a variable
      // or a system property gave them, under the variable's name.
      for (String key : List.of("otel.exporter.otlp.endpoint", "otel.exporter.otlp.protocol",
          "otel.exporter.otlp." + signal + ".endpoint", "otel.exporter.otlp." + signal + ".protocol",
          "otel.exporter.zipkin.endpoint")) {
        String v = config.getString(key, "");
        if (!v.isBlank()) {
          env.put(key.toUpperCase(Locale.ROOT).replace('.', '_'), v);
        }
      }
      exporters(signal, built);
    }
  }

  /** The exporters of {@code otel.<signal>.exporter}, as the SDK reads it. */
  static List<String> names(ConfigProperties config, String signal) {
    List<String> names = config.getList("otel." + signal + ".exporter", List.of("otlp"));
    return names.contains("none") ? List.of() : names;
  }

  /**
   * What another extension added to the SDK's {@code signal} past the gate (a processor, by its
   * exporter's class or its own, or a metric reader), each ungoverned, beside the configured ones.
   */
  synchronized void outside(String signal, List<String> names) {
    List<SettingsProto.FoundExporter> added = new ArrayList<>(outside.getOrDefault(signal, List.of()));
    names.forEach(n -> added.add(found(signal, n, false, "")));
    outside.put(signal, List.copyOf(added));
    changes++;
  }

  /** The exporters of one signal as found, in place of any found before. */
  synchronized void exporters(String signal, List<SettingsProto.FoundExporter> built) {
    exporters.put(signal, List.copyOf(built));
    changes++;
  }

  /** An exporter as found: what it is known as ({@code otlp}, {@code otlp_http}, a class's name). */
  static SettingsProto.FoundExporter found(String signal, String kind, boolean gated, String endpoint) {
    return SettingsProto.FoundExporter.newBuilder().setSignal(signal).setKind(kind).setGated(gated)
        .setEndpoint(endpoint).build();
  }

  /**
   * Every exporter listed, governed or not, as the layer decides them in every SDK: what the
   * inventory and export health report. Decided again only after a change; each warning the layer
   * gives is logged once. The layer is called outside this lock (it takes the extension's). Never
   * throws: should the call fail, each exporter keeps the name last decided, a new one listed as found.
   */
  List<SettingsProto.Exporter> listed() {
    List<SettingsProto.FoundExporter> found = new ArrayList<>();
    Map<String, String> vars;
    long at;
    synchronized (this) {
      if (decidedAt == changes) {
        return decided;
      }
      exporters.values().forEach(found::addAll);
      outside.values().forEach(found::addAll);
      vars = Map.copyOf(env);
      at = changes;
    }
    SettingsProto.ExportersDecided d;
    try {
      // The SDK's own default; the agent sets http/protobuf, which autoconfigure then reads.
      d = layer.get().exporters(found, vars, "grpc");
    } catch (RuntimeException e) {
      // Reporting only: the gates are in place, and the list stays as it was. One new since is
      // listed as found, with no destination: only the layer strips an endpoint's credentials.
      LOG.log(Level.WARNING, "OpAMP: can't list the exporters; they are governed all the same", e);
      synchronized (this) {
        return found.stream().map(f -> listedAs.getOrDefault(f, SettingsProto.Exporter.newBuilder()
            .setSignal(f.getSignal()).setName(f.getKind()).setGated(f.getGated()).build())).toList();
      }
    }
    synchronized (this) {
      for (String w : d.getWarningsList()) {
        if (warned.add(w)) {
          LOG.warning("OpAMP: " + w);
        }
      }
      if (at == changes) {
        decided = List.copyOf(d.getExportersList());
        decidedAt = at;
      }
      if (d.getExportersCount() == found.size()) {
        for (int i = 0; i < found.size(); i++) {
          listedAs.put(found.get(i), d.getExporters(i));
        }
      }
    }
    return List.copyOf(d.getExportersList());
  }

  /** A layer of the inventory's own, until the extension gives it its own. */
  private static SdkSettings sharedLayer() {
    return Shared.LAYER;
  }

  /** Holds the inventory's own layer, made on first use. */
  private static final class Shared {
    static final SdkSettings LAYER = new SdkSettings();
  }

  /**
   * The inventory: the SDK's version and language as its own default resource gives them (the
   * application's {@code otel.resource.attributes} can't change them).
   */
  synchronized SettingsProto.Inventory build() {
    Resource sdk = Resource.getDefault();
    String language = sdk.getAttribute(SDK_LANGUAGE);
    String version = sdk.getAttribute(SDK_VERSION);
    SettingsProto.Inventory.Builder b = SettingsProto.Inventory.newBuilder().setIntegration(PACKAGE)
        .setIntegrationVersion(VERSION).setSdkLanguage(language == null ? "java" : language)
        .setSdkVersion(version == null ? "" : version).addAllInstrumentations(instrumentations.get());
    return b.addAllExporters(listed()).build();
  }

  /**
   * The agent's instrumentations, as {@code loader} finds its InstrumentationModules (the agent's
   * own and any extension's), each under every name it answers to in
   * {@code otel.instrumentation.<name>.enabled}: {@code spring-webmvc} and {@code spring-webmvc-6.0},
   * say. Switched off or not: whether one runs is the agent's choice at start. A module's version
   * is its library's, as the agent's own instrumentations embed it
   * ({@code META-INF/io/opentelemetry/instrumentation/io.opentelemetry.<name>.properties}, the
   * version its telemetry's scope carries); unknown for one that embeds none. Outside the agent:
   * none.
   */
  static List<SettingsProto.Instrumentation> instrumentations(ClassLoader loader) {
    List<SettingsProto.Instrumentation> out = new ArrayList<>();
    try {
      Iterator<InstrumentationModule> modules = ServiceLoader.load(InstrumentationModule.class, loader).iterator();
      while (true) {
        InstrumentationModule m;
        try {
          if (!modules.hasNext()) {
            break;
          }
          m = modules.next();
        } catch (ServiceConfigurationError | LinkageError e) {
          // One that can't be made here; the agent says so itself if it can't run it.
          LOG.log(Level.FINE, "OpAMP: an instrumentation module isn't readable", e);
          continue;
        }
        String version = version(m);
        for (String name : m.instrumentationNames()) {
          out.add(SettingsProto.Instrumentation.newBuilder().setName(name).setVersion(version).build());
        }
      }
    } catch (LinkageError | RuntimeException e) {
      // Not under the agent (no extension API to read them by): none.
      LOG.log(Level.FINE, "OpAMP: the agent's instrumentations aren't readable", e);
    }
    return out;
  }

  /** A module's version, from the first of its names that embeds one; empty when none does. */
  private static String version(InstrumentationModule m) {
    ClassLoader loader = m.getClass().getClassLoader();
    if (loader == null) {
      return "";
    }
    for (String name : m.instrumentationNames()) {
      String v = property(loader.getResourceAsStream(
          "META-INF/io/opentelemetry/instrumentation/io.opentelemetry." + name + ".properties"));
      if (!v.isEmpty()) {
        return v;
      }
    }
    return "";
  }

  /** The {@code version} of a properties file; empty without one. */
  private static String property(InputStream in) {
    if (in == null) {
      return "";
    }
    try (in) {
      Properties p = new Properties();
      p.load(in);
      return p.getProperty("version", "").trim();
    } catch (IOException | IllegalArgumentException e) {
      return "";
    }
  }
}
