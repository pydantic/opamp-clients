// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.javaagent.extension.instrumentation.InstrumentationModule;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import io.opentelemetry.sdk.resources.Resource;
import java.io.IOException;
import java.io.InputStream;
import java.util.ArrayList;
import java.util.Iterator;
import java.util.List;
import java.util.Map;
import java.util.Properties;
import java.util.ServiceConfigurationError;
import java.util.ServiceLoader;
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
  static final String PACKAGE = "dev.pydantic:opamp-javaagent-extension";
  /** Its version, as Maven built it ({@code version.properties}); empty if the jar doesn't say. */
  static final String VERSION = property(Inventory.class.getResourceAsStream("version.properties"));
  private static final Logger LOG = Logger.getLogger(Extension.class.getName());
  private static final AttributeKey<String> SDK_LANGUAGE = AttributeKey.stringKey("telemetry.sdk.language");
  private static final AttributeKey<String> SDK_VERSION = AttributeKey.stringKey("telemetry.sdk.version");

  /** The exporters the SDK was built with, by signal, as the customizers or the model named them. */
  private final Map<String, List<SettingsProto.Exporter>> exporters = new TreeMap<>();
  /** The instrumentations the agent can run: its InstrumentationModules, read once, at start. */
  Supplier<List<SettingsProto.Instrumentation>> instrumentations =
      () -> instrumentations(Inventory.class.getClassLoader());

  /**
   * Under autoconfigure, from the customizer an exporter of {@code signal} ({@code traces},
   * {@code metrics} or {@code logs}) went through: the SDK builds one for each name
   * {@code otel.<signal>.exporter} gives ({@code otlp} when unset; {@code none}: none), and every
   * one of them is in the extension's gate.
   */
  void configured(String signal, ConfigProperties config) {
    List<SettingsProto.Exporter> built = new ArrayList<>();
    for (String name : names(config, signal)) {
      built.add(exporter(signal, name, true));
    }
    exporters(signal, built);
  }

  /** The exporters of {@code otel.<signal>.exporter}, as the SDK reads it. */
  static List<String> names(ConfigProperties config, String signal) {
    List<String> names = config.getList("otel." + signal + ".exporter", List.of("otlp"));
    return names.contains("none") ? List.of() : names;
  }

  /** The exporters of one signal, in place of any listed before. */
  synchronized void exporters(String signal, List<SettingsProto.Exporter> built) {
    exporters.put(signal, List.copyOf(built));
  }

  static SettingsProto.Exporter exporter(String signal, String name, boolean gated) {
    return SettingsProto.Exporter.newBuilder().setSignal(signal).setName(name).setGated(gated).build();
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
    exporters.values().forEach(b::addAllExporters);
    return b.build();
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
