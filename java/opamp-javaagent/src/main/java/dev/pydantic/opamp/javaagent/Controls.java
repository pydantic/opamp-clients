// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.common.export.MemoryMode;
import io.opentelemetry.sdk.logs.data.LogRecordData;
import io.opentelemetry.sdk.logs.export.LogRecordExporter;
import io.opentelemetry.sdk.metrics.Aggregation;
import io.opentelemetry.sdk.metrics.InstrumentType;
import io.opentelemetry.sdk.metrics.data.AggregationTemporality;
import io.opentelemetry.sdk.metrics.data.MetricData;
import io.opentelemetry.sdk.metrics.export.CollectionRegistration;
import io.opentelemetry.sdk.metrics.export.MetricExporter;
import io.opentelemetry.sdk.metrics.export.MetricReader;
import io.opentelemetry.sdk.metrics.export.PeriodicMetricReader;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SpanExporter;
import java.util.Collection;
import java.util.List;

/**
 * What the server changes while the agent runs. The SDK is built once, with these in place of its
 * own sampler and exporters; each delegates to what the agent configured until a setting says
 * otherwise, and again once the setting is withdrawn.
 */
final class Controls {
  final LiveSampler sampler = new LiveSampler(() -> this.nanoTime.getAsLong());
  /**
   * All the gates decide by, from one document: replaced whole by each {@link #apply}, and read
   * once by each export, so an export never sees one document's send_traces beside another's
   * redaction.
   */
  volatile Gates gates = Gates.NONE;
  /** What each gate counted, for export health. */
  final GateCounts traceCounts = new GateCounts();
  final GateCounts metricCounts = new GateCounts();
  final GateCounts logCounts = new GateCounts();
  /** send_logs_per_second's decision on each record, shared by every log gate. */
  final LogDecisions logDecisions = new LogDecisions();
  /** The monotonic clock the rate caps (the sampler's too) refill by. */
  java.util.function.LongSupplier nanoTime = System::nanoTime;

  /**
   * Where the application's own settings go ({@code custom}, read with
   * {@code dev.pydantic.opamp.AppSettings}); null: nowhere, and {@code custom} isn't declared.
   */
  volatile AppSettingsSink app;

  /**
   * Carry out the live settings; a setting left out is the agent's own. The application's own
   * settings ({@code custom}) are put in force last, so a listener sees the SDK's settings already
   * in force; the listeners run on the calling thread. Both go in under one lock, so the SDK's
   * settings and the application's always come from the same document.
   */
  synchronized void apply(SettingsProto.Settings s) {
    applySdk(s);
    AppSettingsSink to = app;
    if (to != null) {
      putApp(to, s.getCustomMap());
    }
  }

  /** Put the application's own settings in force (the lock held), saying which keys changed. */
  static void putApp(AppSettingsSink to, java.util.Map<String, String> values) {
    java.util.Set<String> changed = to.put(values);
    if (!changed.isEmpty()) {
      java.util.logging.Logger.getLogger(Extension.class.getName())
          .info("OpAMP: application settings changed: " + String.join(", ", changed));
    }
  }

  /** The application's own settings back to {@code values}, under the same lock as {@link #apply}. */
  synchronized void restoreApp(java.util.Map<String, String> values) {
    AppSettingsSink to = app;
    if (to != null) {
      putApp(to, values);
    }
  }

  private void applySdk(SettingsProto.Settings s) {
    sampler.apply(s);
    // One write: the gates see this document's settings all together, or the last one's.
    gates = new Gates(
        !s.hasSendTraces() || s.getSendTraces(),
        !s.hasSendMetrics() || s.getSendMetrics(),
        !s.hasSendLogs() || s.getSendLogs(),
        new LogLevels(s.hasSendLogsMinSeverity() ? s.getSendLogsMinSeverity() : 0,
            List.copyOf(s.getSendLogsScopeLevelsList())),
        Redaction.of(s),
        TokenBucket.of(s.hasSendLogsPerSecond() ? s.getSendLogsPerSecond() : null, gates.logCap()));
  }

  /**
   * What the gates decide by, all from one document: the send_* switches; {@code logLevels}, which
   * log records to export by severity (send_logs_level, and send_logs_level_by_scope);
   * {@code redaction}, drop_attributes, mask_attributes and scrubbing, for spans and log records; and
   * {@code logCap}, send_logs_per_second's bucket (null: no cap).
   */
  record Gates(boolean sendTraces, boolean sendMetrics, boolean sendLogs, LogLevels logLevels,
      Redaction redaction, TokenBucket logCap) {
    static final Gates NONE = new Gates(true, true, true, LogLevels.NONE, Redaction.NONE, null);
  }

  /** What the gates counted since start, for the layer's Health call. */
  SettingsProto.ExportCounts counts() {
    return SettingsProto.ExportCounts.newBuilder().setTraces(traceCounts.snapshot())
        .setMetrics(metricCounts.snapshot()).setLogs(logCounts.snapshot()).build();
  }

  /**
   * The agent configuration properties the next-start settings stand for, merged over the agent's
   * own: the server's choice wins for these keys; the rest are the agent's.
   */
  static java.util.Map<String, String> startProperties(SettingsProto.Settings s) {
    java.util.Map<String, String> props = new java.util.LinkedHashMap<>();
    for (String name : s.getDeactivateInstrumentations().getNamesList()) {
      // The agent reads `-` and `.` in a property name as the same, but not `_`: spring_webmvc
      // would name nothing.
      props.put("otel.instrumentation." + name.replace('_', '-') + ".enabled", "false");
    }
    if (s.hasDeactivateAllInstrumentations() && s.getDeactivateAllInstrumentations()) {
      props.put("otel.instrumentation.common.default-enabled", "false");
    }
    return props;
  }

  /**
   * One of the extension's gates: how a processor around one is told apart from a processor
   * another extension added ({@link Outside}).
   */
  interface Gate {}

  private abstract static class SpanGate implements SpanExporter, Gate {}

  private abstract static class LogGate implements LogRecordExporter, Gate {}

  /**
   * The agent's span exporter, or nothing while the server says not to send traces; with
   * redaction, each span as {@link Redaction} leaves it.
   */
  SpanExporter spans(SpanExporter to) {
    return new SpanGate() {
      @Override
      public CompletableResultCode export(Collection<SpanData> spans) {
        Gates g = gates;
        if (!g.sendTraces()) {
          traceCounts.filtered(spans.size());
          return CompletableResultCode.ofSuccess();
        }
        Redaction r = g.redaction();
        if (r.any()) {
          spans = spans.stream().map(r::span).toList();
        }
        return traceCounts.exported(to.export(spans), spans.size());
      }

      @Override
      public CompletableResultCode flush() {
        return to.flush();
      }

      @Override
      public CompletableResultCode shutdown() {
        return to.shutdown();
      }

      @Override
      public String toString() {
        return "OpampGated{" + to + "}";
      }
    };
  }

  /** The agent's metric exporter, or nothing while the server says not to send metrics. */
  MetricExporter metrics(MetricExporter to) {
    return new MetricExporter() {
      @Override
      public CompletableResultCode export(Collection<MetricData> metrics) {
        long points = metrics.stream().mapToLong(m -> m.getData().getPoints().size()).sum();
        if (!gates.sendMetrics()) {
          metricCounts.filtered(points);
          return CompletableResultCode.ofSuccess();
        }
        return metricCounts.exported(to.export(metrics), points);
      }

      @Override
      public AggregationTemporality getAggregationTemporality(InstrumentType type) {
        return to.getAggregationTemporality(type);
      }

      @Override
      public Aggregation getDefaultAggregation(InstrumentType type) {
        return to.getDefaultAggregation(type);
      }

      @Override
      public MemoryMode getMemoryMode() {
        return to.getMemoryMode();
      }

      @Override
      public CompletableResultCode flush() {
        return to.flush();
      }

      @Override
      public CompletableResultCode shutdown() {
        return to.shutdown();
      }

      @Override
      public String toString() {
        return "OpampGated{" + to + "}";
      }
    };
  }

  /**
   * A pull reader (Prometheus: {@code otel.metrics.exporter=prometheus}) has no exporter to gate:
   * it serves nothing while the server says not to send metrics. A periodic reader is left as it
   * is (the SDK needs it as built), since its exporter is gated.
   */
  MetricReader reader(MetricReader to) {
    if (to instanceof PeriodicMetricReader) {
      return to;
    }
    return new MetricReader() {
      @Override
      public void register(CollectionRegistration registration) {
        to.register(new CollectionRegistration() {
          @Override
          public Collection<MetricData> collectAllMetrics() {
            return gates.sendMetrics() ? registration.collectAllMetrics() : List.of();
          }
        });
      }

      @Override
      public AggregationTemporality getAggregationTemporality(InstrumentType type) {
        return to.getAggregationTemporality(type);
      }

      @Override
      public Aggregation getDefaultAggregation(InstrumentType type) {
        return to.getDefaultAggregation(type);
      }

      @Override
      public MemoryMode getMemoryMode() {
        return to.getMemoryMode();
      }

      @Override
      public CompletableResultCode forceFlush() {
        return to.forceFlush();
      }

      @Override
      public CompletableResultCode shutdown() {
        return to.shutdown();
      }

      @Override
      public String toString() {
        return "OpampGated{" + to + "}";
      }
    };
  }

  /**
   * The log levels in force: {@code min}, send_logs_level's SeverityNumber (0: none), and
   * {@code byScope}, send_logs_level_by_scope's entries as the layer sorts them (the longest name
   * first, an exact name before a prefix of it), so the first that matches is the one.
   */
  record LogLevels(int min, List<SettingsProto.ScopeLevel> byScope) {
    static final LogLevels NONE = new LogLevels(0, List.of());
    /** A scope whose records are all dropped, severity 0 too. */
    static final int OFF = 0xFFFF;

    /**
     * The lowest SeverityNumber exported for the scope named {@code scope}: its exact entry, else
     * the longest prefix ({@code name*}) that matches, else send_logs_level; 0: every record.
     */
    int threshold(String scope) {
      for (SettingsProto.ScopeLevel l : byScope) {
        String p = l.getPattern();
        boolean prefix = p.endsWith("*");
        if (prefix ? scope.startsWith(p.substring(0, p.length() - 1)) : scope.equals(p)) {
          return l.getMinSeverity();
        }
      }
      return min;
    }

    /** Whether any level is set: without one, every record goes on. */
    boolean any() {
      return min > 0 || !byScope.isEmpty();
    }

    /** Whether a record of this scope and SeverityNumber is exported; 0 (unspecified) is, but under off. */
    boolean keeps(String scope, int severity) {
      int t = threshold(scope);
      return t == 0 || (t != OFF && (severity == 0 || severity >= t));
    }
  }

  /**
   * The agent's log record exporter, or nothing while the server says not to send logs. With
   * {@code send_logs_level} and {@code send_logs_level_by_scope}, only records at their scope's
   * severity or above go on; a record with no severity (SeverityNumber 0) can't be judged and goes
   * on, unless its scope's level is {@code off}. The scope is the one the record is exported
   * under: the agent's logging bridges name it after the logger. Then {@code send_logs_per_second}:
   * of a batch, the first records the bucket has tokens for go on, the rest are dropped; a record
   * another log gate already decided keeps its decision ({@link LogDecisions}). Last, redaction:
   * each record as {@link Redaction} leaves it.
   */
  LogRecordExporter logs(LogRecordExporter to) {
    logDecisions.gates.incrementAndGet();
    return new LogGate() {
      @Override
      public CompletableResultCode export(Collection<LogRecordData> logs) {
        int given = logs.size();
        Gates g = gates;
        if (!g.sendLogs()) {
          logCounts.filtered(given);
          return CompletableResultCode.ofSuccess();
        }
        LogLevels levels = g.logLevels();
        if (levels.any()) {
          logs = logs.stream().filter(r -> levels.keeps(r.getInstrumentationScopeInfo().getName(),
              r.getSeverity().getSeverityNumber())).toList();
        }
        TokenBucket cap = g.logCap();
        if (cap != null) {
          logs = logDecisions.admitted(List.copyOf(logs), this, n -> cap.take(nanoTime.getAsLong(), n));
        }
        logCounts.filtered(given - logs.size());
        if (logs.isEmpty()) {
          return CompletableResultCode.ofSuccess();
        }
        Redaction r = g.redaction();
        if (r.any()) {
          logs = logs.stream().map(r::log).toList();
        }
        return logCounts.exported(to.export(logs), logs.size());
      }

      @Override
      public CompletableResultCode flush() {
        return to.flush();
      }

      @Override
      public CompletableResultCode shutdown() {
        return to.shutdown();
      }

      @Override
      public String toString() {
        return "OpampGated{" + to + "}";
      }
    };
  }

  /**
   * send_logs_per_second's decision on each record, so that every exporter sends the same records
   * and the process spends one budget: the first log gate to see a record takes a token for it,
   * and the others reuse that decision. Only kept while there are several log gates. The SDK hands
   * each processor its own copy of a record, so a record is known by what it holds ({@link Key}).
   * A gate that sees the same record again decides afresh. Bounded: an entry a gate never sees
   * (its processor dropped the record) goes once {@link #KEPT} newer ones are kept.
   */
  static final class LogDecisions {
    static final int KEPT = 8192;
    /** How many log gates there are. */
    final java.util.concurrent.atomic.AtomicInteger gates = new java.util.concurrent.atomic.AtomicInteger();
    private final java.util.Map<Key, Decision> decided = new java.util.LinkedHashMap<>() {
      @Override
      protected boolean removeEldestEntry(java.util.Map.Entry<Key, Decision> eldest) {
        return size() > KEPT;
      }
    };

    /** A record as every processor's copy of it holds it: its observed time is set once, as it is emitted. */
    record Key(long observedNs, long timestampNs, String scope, int severity, String traceId, String spanId,
        Object body, io.opentelemetry.api.common.Attributes attributes) {
      static Key of(LogRecordData r) {
        return new Key(r.getObservedTimestampEpochNanos(), r.getTimestampEpochNanos(),
            r.getInstrumentationScopeInfo().getName(), r.getSeverity().getSeverityNumber(),
            r.getSpanContext().getTraceId(), r.getSpanContext().getSpanId(), r.getBodyValue(), r.getAttributes());
      }
    }

    /** Whether a record goes on, and the gates that have seen it. */
    private record Decision(boolean admitted, java.util.Set<Object> seen) {}

    /** Number of known decisions, for tests. */
    synchronized int size() {
      return decided.size();
    }

    /**
     * The records of {@code records} {@code gate} sends on: each reuses another gate's decision, or
     * is decided now, the first of those {@code take} (a count of records to the tokens granted)
     * has tokens for going on.
     */
    synchronized List<LogRecordData> admitted(List<LogRecordData> records, Object gate,
        java.util.function.LongUnaryOperator take) {
      int n = gates.get();
      if (n < 2) {
        return records.subList(0, (int) take.applyAsLong(records.size()));
      }
      List<Key> keys = records.stream().map(Key::of).toList();
      boolean[] fresh = new boolean[keys.size()];
      int asked = 0;
      for (int i = 0; i < fresh.length; i++) {
        Decision d = decided.get(keys.get(i));
        fresh[i] = d == null || d.seen().contains(gate);
        asked += fresh[i] ? 1 : 0;
      }
      long tokens = asked == 0 ? 0 : take.applyAsLong(asked);
      List<LogRecordData> out = new java.util.ArrayList<>();
      for (int i = 0; i < fresh.length; i++) {
        Key k = keys.get(i);
        Decision d = fresh[i] ? null : decided.get(k);
        if (d == null) {
          d = new Decision(tokens-- > 0, java.util.Collections.newSetFromMap(new java.util.IdentityHashMap<>()));
          decided.put(k, d);
        }
        d.seen().add(gate);
        if (d.seen().size() >= n) {
          decided.remove(k); // every gate has it
        }
        if (d.admitted()) {
          out.add(records.get(i));
        }
      }
      return out;
    }
  }
}
