// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import io.opentelemetry.javaagent.extension.instrumentation.InstrumentationModule;
import io.opentelemetry.javaagent.extension.instrumentation.TypeInstrumentation;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import io.opentelemetry.sdk.autoconfigure.spi.traces.ConfigurableSpanExporterProvider;
import io.opentelemetry.sdk.trace.export.SpanExporter;
import java.util.List;

/**
 * What the agent's jar provides, as the tests' class path provides it (src/test/resources): three
 * instrumentation modules, as the agent's are named, one with its library's version embedded and one
 * whose library isn't there, and an exporter of a name of its own.
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

  /**
   * As the agent's Kafka module: its type instrumentation names the library's class, and the class
   * loader matcher is left to any. The tests' class path hasn't the library: it doesn't apply.
   */
  public static final class Kafka extends InstrumentationModule {
    public Kafka() {
      super("example-kafka");
    }

    @Override
    public List<TypeInstrumentation> typeInstrumentations() {
      return List.of(new TypeInstrumentation() {
        @Override
        public net.bytebuddy.matcher.ElementMatcher<net.bytebuddy.description.type.TypeDescription> typeMatcher() {
          return net.bytebuddy.matcher.ElementMatchers.named("com.example.kafka.Producer");
        }

        @Override
        public void transform(io.opentelemetry.javaagent.extension.instrumentation.TypeTransformer transformer) {}
      });
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
