// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import io.opentelemetry.sdk.logs.LogRecordProcessor;
import io.opentelemetry.sdk.logs.SdkLoggerProviderBuilder;
import io.opentelemetry.sdk.logs.export.BatchLogRecordProcessor;
import io.opentelemetry.sdk.logs.export.SimpleLogRecordProcessor;
import io.opentelemetry.sdk.metrics.SdkMeterProviderBuilder;
import io.opentelemetry.sdk.trace.SdkTracerProviderBuilder;
import io.opentelemetry.sdk.trace.SpanProcessor;
import io.opentelemetry.sdk.trace.export.BatchSpanProcessor;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.lang.reflect.Field;
import java.util.ArrayList;
import java.util.Collection;
import java.util.Collections;
import java.util.IdentityHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * What another agent extension puts into the SDK past the extension's gates, under autoconfigure. An
 * extension's tracer, logger or meter provider customizer can add a span processor, a log record
 * processor or a metric reader of its own, and what it exports never goes through a gate. The
 * extension's provider customizers run last, so they see every processor and reader the SDK is
 * built with. The SDK's builders don't expose them: they are read reflectively.
 *
 * <p>The processors and readers the agent builds from {@code otel.*.exporter} are told to {@link
 * #built} as the SDK makes them, after every other extension's processor and reader customizers:
 * each holds a gated exporter, whatever another customizer wrapped around it. Of the rest, one
 * that can't export is left alone: a span processor that isn't told when a span ends (the agent's
 * thread details, or a baggage processor), the agent's baggage log record processor, and a batch
 * or simple processor around one of the extension's gates, and EDOT Java's span logger while
 * nothing can switch it on ({@link #ELASTIC_DEBUG_LOG}). Every other one is outside, as the
 * extension can't tell what it does with the telemetry.
 */
final class Outside {
  private static final Logger LOG = Logger.getLogger(Extension.class.getName());
  /** Log record processors of the agent's own that only add attributes to a record. */
  static final Set<String> ENRICHING_LOGS = Set.of("io.opentelemetry.contrib.baggage.processor.BaggageLogRecordProcessor");

  /** The agent's own processors and readers, by identity. */
  private final Set<Object> agents = Collections.synchronizedSet(Collections.newSetFromMap(new IdentityHashMap<>()));

  /** A processor or reader the agent built from its exporters, as the last customizer left it. */
  <T> T built(T processorOrReader) {
    agents.add(processorOrReader);
    return processorOrReader;
  }

  /**
   * The span logger Elastic's distribution of the agent (EDOT Java) always adds: a simple processor
   * around an exporter that writes spans to the agent's own log, and does nothing until debug
   * logging is switched on. EDOT switches it on only from Elastic's central configuration, its own
   * OpAMP client ({@code elastic.otel.opamp.endpoint}); not at start, whatever the log level.
   */
  static final String ELASTIC_DEBUG_LOG = "co.elastic.otel.logging.AgentLog$DebugLogSpanExporter";

  /** The span processors in {@code b} that export spans past the gate, by name. */
  List<String> spans(SdkTracerProviderBuilder b) {
    return spans(b, false);
  }

  /**
   * The span processors in {@code b} that export spans past the gate, by name. With {@code
   * elasticCentralConfig} off, EDOT's span logger ({@link #ELASTIC_DEBUG_LOG}) is left alone, as
   * nothing can switch it on; with it on, it is outside like any other.
   */
  List<String> spans(SdkTracerProviderBuilder b, boolean elasticCentralConfig) {
    List<String> out = new ArrayList<>();
    for (Object o : held(b, "spanProcessors")) {
      if (o instanceof SpanProcessor p && !agents.contains(p) && p.isEndRequired() && !gated(p)
          && (elasticCentralConfig || !elasticDebugLog(p))) {
        out.add(name(p));
      }
    }
    return out;
  }

  private static boolean elasticDebugLog(Object p) {
    Object exporter = p instanceof SimpleSpanProcessor ? exporter(p) : null;
    return exporter != null && exporter.getClass().getName().equals(ELASTIC_DEBUG_LOG);
  }

  /** The log record processors in {@code b} that export log records past the gate, by name. */
  List<String> logs(SdkLoggerProviderBuilder b) {
    List<String> out = new ArrayList<>();
    for (Object o : held(b, "logRecordProcessors")) {
      if (o instanceof LogRecordProcessor p && !agents.contains(p) && !ENRICHING_LOGS.contains(p.getClass().getName())
          && !gated(p)) {
        out.add(name(p));
      }
    }
    return out;
  }

  /** The metric readers in {@code b} that the agent didn't build, by name. */
  List<String> metrics(SdkMeterProviderBuilder b) {
    Object readers = field(b, "metricReaders");
    List<String> out = new ArrayList<>();
    if (readers instanceof Map<?, ?> m) {
      for (Object r : m.keySet()) {
        if (!agents.contains(r)) {
          out.add(simpleName(r));
        }
      }
    }
    return out;
  }

  /** A batch or simple processor whose exporter is one of the extension's gates. */
  private static boolean gated(Object p) {
    return exporter(p) instanceof Controls.Gate;
  }

  /** The exporter of a batch or simple processor; null for any other. */
  private static Object exporter(Object p) {
    if (p instanceof BatchSpanProcessor b) {
      return b.getSpanExporter();
    }
    if (p instanceof SimpleSpanProcessor s) {
      return s.getSpanExporter();
    }
    if (p instanceof BatchLogRecordProcessor b) {
      return b.getLogRecordExporter();
    }
    if (p instanceof SimpleLogRecordProcessor s) {
      return s.getLogRecordExporter();
    }
    return null;
  }

  /** What the inventory lists a processor as: its exporter's class, else its own. */
  private static String name(Object p) {
    Object exporter = exporter(p);
    return simpleName(exporter == null ? p : exporter);
  }

  private static String simpleName(Object o) {
    String simple = o.getClass().getSimpleName();
    return simple.isEmpty() ? o.getClass().getName() : simple;
  }

  /** The collection a builder keeps in {@code field}; empty, with a warning, when it can't be read. */
  private static Collection<?> held(Object builder, String field) {
    return field(builder, field) instanceof Collection<?> c ? c : List.of();
  }

  private static Object field(Object builder, String name) {
    try {
      Field f = builder.getClass().getDeclaredField(name);
      f.setAccessible(true);
      return f.get(builder);
    } catch (ReflectiveOperationException | RuntimeException err) {
      LOG.log(Level.WARNING, "OpAMP: can't read what the SDK's " + builder.getClass().getSimpleName() + " holds, so a "
          + "processor or reader another extension adds isn't seen, and the settings it escapes are still answered", err);
      return null;
    }
  }
}
