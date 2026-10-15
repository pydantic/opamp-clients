// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import io.opentelemetry.javaagent.extension.instrumentation.InstrumentationModule;
import io.opentelemetry.javaagent.extension.instrumentation.TypeInstrumentation;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import io.opentelemetry.sdk.autoconfigure.spi.traces.ConfigurableSpanExporterProvider;
import io.opentelemetry.sdk.trace.export.SpanExporter;
import java.util.List;

/**
 * What the agent's jar provides, as the tests' class path provides it (src/test/resources): two
 * instrumentation modules, as the agent's are named, one with its library's version embedded, and
 * an exporter of a name of its own.
 */
public final class TestInstrumentations {
  private TestInstrumentations() {}

  /** As the agent's Spring Web MVC 6 module: a family name and its own, a version embedded. */
  public static final class SpringWebmvc extends InstrumentationModule {
    public SpringWebmvc() {
      super("spring-webmvc", "spring-webmvc-6.0");
    }

    @Override
    public List<TypeInstrumentation> typeInstrumentations() {
      return List.of();
    }
  }

  /** As the agent's JDBC module, but with no version embedded. */
  public static final class Jdbc extends InstrumentationModule {
    public Jdbc() {
      super("jdbc");
    }

    @Override
    public boolean defaultEnabled(ConfigProperties config) {
      // Off by default: listed all the same.
      return false;
    }

    @Override
    public List<TypeInstrumentation> typeInstrumentations() {
      return List.of();
    }
  }

  /** {@code otel.traces.exporter=recording}: an exporter of another library's. */
  public static final class Recording implements ConfigurableSpanExporterProvider {
    @Override
    public SpanExporter createExporter(ConfigProperties config) {
      return io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter.create();
    }

    @Override
    public String getName() {
      return "recording";
    }
  }
}
