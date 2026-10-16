// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.fasterxml.jackson.databind.JsonNode;
import io.opentelemetry.common.ComponentLoader;
import io.opentelemetry.context.Context;
import io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdk;
import io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdkBuilder;
import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizer;
import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.logs.LogRecordProcessor;
import io.opentelemetry.sdk.logs.ReadWriteLogRecord;
import io.opentelemetry.sdk.logs.export.SimpleLogRecordProcessor;
import io.opentelemetry.sdk.testing.exporter.InMemoryLogRecordExporter;
import io.opentelemetry.sdk.testing.exporter.InMemoryMetricReader;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.ReadWriteSpan;
import io.opentelemetry.sdk.trace.ReadableSpan;
import io.opentelemetry.sdk.trace.SpanProcessor;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeSet;
import java.util.function.Consumer;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * What another agent extension puts into the SDK past the gates (docs/javaagent.md): a processor
 * or reader of its own, or a sampler in place of the extension's. The settings it escapes are set
 * aside, named as unsupported, and never answered as carried out; everything else is carried out
 * as before.
 */
class OutsideTest {
  /**
   * The SDK autoconfigure builds from {@code props}, through {@code other}'s customizers (another
   * extension's, which run first) and then the extension's around {@code e}; then, as the agent's
   * listener does once the SDK is built, the extension is told of it.
   */
  static InventoryTest.Sdk sdk(Extension e, Map<String, String> props, Consumer<AutoConfigurationCustomizer> other) {
    ComponentLoader all = ComponentLoader.forClassLoader(OutsideTest.class.getClassLoader());
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
    other.accept(b);
    new OpampCustomizerProvider(e).customize(b);
    AutoConfiguredOpenTelemetrySdk built = b.build();
    e.built(built.getOpenTelemetrySdk());
    return new InventoryTest.Sdk(built);
  }

  static Map<String, String> props(Path dir) {
    return InventoryTest.props("http://127.0.0.1:1/v1/opamp", dir);
  }

  static Set<String> live(Extension e) {
    return new TreeSet<>(e.host().getLiveList());
  }

  /** The settings the effective config names as unsupported. */
  static Set<String> unsupported(Extension e) throws Exception {
    Set<String> out = new TreeSet<>();
    JsonNode aside = Vectors.JSON.readTree(ExtensionTest.setAside(e));
    if (aside != null && aside.has("unsupported")) {
      aside.get("unsupported").forEach(n -> out.add(n.asText()));
    }
    return out;
  }

  /** A processor that only adds an attribute as a span starts, as the agent's thread details does. */
  static final class OnStart implements SpanProcessor {
    @Override
    public void onStart(Context parent, ReadWriteSpan span) {
      span.setAttribute("enriched", true);
    }

    @Override
    public boolean isStartRequired() {
      return true;
    }

    @Override
    public void onEnd(ReadableSpan span) {}

    @Override
    public boolean isEndRequired() {
      return false;
    }
  }

  /** Another extension's wrapper around the processor the agent built: it sees every span. */
  record Wrapping(SpanProcessor to) implements SpanProcessor {
    @Override
    public void onStart(Context parent, ReadWriteSpan span) {
      to.onStart(parent, span);
    }

    @Override
    public boolean isStartRequired() {
      return to.isStartRequired();
    }

    @Override
    public void onEnd(ReadableSpan span) {
      to.onEnd(span);
    }

    @Override
    public boolean isEndRequired() {
      return true;
    }

    @Override
    public CompletableResultCode shutdown() {
      return to.shutdown();
    }
  }

  /** Another extension's wrapper around the log record processor the agent built. */
  record WrappingLogs(LogRecordProcessor to) implements LogRecordProcessor {
    @Override
    public void onEmit(Context context, ReadWriteLogRecord record) {
      to.onEmit(context, record);
    }

    @Override
    public CompletableResultCode shutdown() {
      return to.shutdown();
    }
  }

  @Test
  void anAgentAloneDeclaresEverySetting(@TempDir Path dir) {
    Extension e = Extension.forTest(null);
    try (InventoryTest.Sdk sdk = sdk(e, props(dir), b -> { })) {
      assertEquals(new TreeSet<>(Extension.HOST.getLiveList()), live(e));
    }
  }

  @Test
  void aProcessorOrReaderAnotherExtensionAddsSetsItsSignalsGateAside(@TempDir Path dir) throws Exception {
    Extension e = Extension.forTest(null);
    InMemorySpanExporter spans = InMemorySpanExporter.create();
    try (InventoryTest.Sdk sdk = sdk(e, props(dir), b -> b
        .addTracerProviderCustomizer((t, c) -> t.addSpanProcessor(SimpleSpanProcessor.create(spans)))
        .addLoggerProviderCustomizer((l, c) -> l.addLogRecordProcessor(
            SimpleLogRecordProcessor.create(InMemoryLogRecordExporter.create())))
        .addMeterProviderCustomizer((m, c) -> m.registerMetricReader(InMemoryMetricReader.create())))) {
      Set<String> gated = new TreeSet<>();
      Extension.GATED.values().forEach(gated::addAll);
      Set<String> declared = live(e);
      assertTrue(declared.stream().noneMatch(gated::contains), declared.toString());
      assertTrue(declared.containsAll(List.of("sampling_rate", "sampling_rules", "sampling_traces_per_second",
          "debug_until", "debug_sampling_rate")), declared.toString());

      // A config with a gate's settings: APPLIED, and they are named as set aside, not in force.
      Opamp.RemoteConfigStatus s = e.remoteConfig(ExtensionTest.config("elastic",
          "{\"sampling_rate\":\"0.5\",\"send_traces\":\"false\",\"send_logs_level\":\"warn\",\"send_metrics\":\"false\"}"));
      assertEquals(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, s.getStatus());
      assertEquals(Set.of("send_logs_level", "send_metrics", "send_traces"), unsupported(e));
      assertEquals("{\"sampling_rate\":\"0.5\"}", ExtensionTest.effective(e));
      assertTrue(e.controls.gates.sendTraces());

      // The inventory lists what the other extension added, ungated, beside the configured ones.
      Map<String, Map<String, Object>> listed = InventoryTest.exporters(
          Vectors.LAYER.components(e.inventory.build(), e.host(), Vectors.NOW));
      assertEquals(Map.of("console", true, "InMemorySpanExporter", false), listed.get("traces"));
      assertEquals(Map.of("console", true, "InMemoryLogRecordExporter", false), listed.get("logs"));
      assertEquals(Map.of("console", true, "InMemoryMetricReader", false), listed.get("metrics"));
    }
  }

  @Test
  void onlyTheSignalsAnotherExtensionsProcessorReachesAreSetAside(@TempDir Path dir) {
    Extension e = Extension.forTest(null);
    try (InventoryTest.Sdk sdk = sdk(e, props(dir), b -> b.addTracerProviderCustomizer(
        (t, c) -> t.addSpanProcessor(SimpleSpanProcessor.create(InMemorySpanExporter.create()))))) {
      Set<String> declared = live(e);
      assertFalse(declared.contains("send_traces"), declared.toString());
      // Redaction is at the span and log gates both: one of them escaped is enough.
      assertFalse(declared.contains("mask_attributes"), declared.toString());
      assertTrue(declared.containsAll(List.of("send_logs", "send_logs_level", "send_metrics", "sampling_rate")),
          declared.toString());
    }
  }

  @Test
  void processorsThatCantExportOrWrapTheAgentsOwnAreLeftAlone(@TempDir Path dir) {
    Extension e = Extension.forTest(null);
    try (InventoryTest.Sdk sdk = sdk(e, props(dir), b -> b
        .addTracerProviderCustomizer((t, c) -> t.addSpanProcessorFirst(new OnStart()))
        .addSpanProcessorCustomizer((p, c) -> new Wrapping(p))
        .addLogRecordProcessorCustomizer((p, c) -> new WrappingLogs(p)))) {
      assertEquals(new TreeSet<>(Extension.HOST.getLiveList()), live(e));
    }
  }

  @Test
  void aSamplerAnotherExtensionSetsSetsTheSamplersSettingsAside(@TempDir Path dir) throws Exception {
    Extension e = Extension.forTest(null);
    try (InventoryTest.Sdk sdk = sdk(e, props(dir), b -> b.addTracerProviderCustomizer(
        (t, c) -> t.setSampler(Sampler.alwaysOn())))) {
      Set<String> declared = live(e);
      assertTrue(declared.stream().noneMatch(Extension.SAMPLED::contains), declared.toString());
      assertTrue(declared.containsAll(List.of("send_traces", "mask_attributes", "debug_until", "send_logs_level")),
          declared.toString());

      Opamp.RemoteConfigStatus s = e.remoteConfig(ExtensionTest.config("elastic",
          "{\"sampling_rate\":\"0\",\"sampling_traces_per_second\":\"1\",\"send_traces\":\"false\"}"));
      assertEquals(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, s.getStatus());
      assertEquals(Set.of("sampling_rate", "sampling_traces_per_second"), unsupported(e));
      assertEquals("{\"send_traces\":\"false\"}", ExtensionTest.effective(e));
      assertFalse(e.controls.gates.sendTraces());
    }
  }

  @Test
  void aSamplerSetThroughTheSamplerCustomizerIsStillWrapped(@TempDir Path dir) {
    Extension e = Extension.forTest(null);
    try (InventoryTest.Sdk sdk = sdk(e, props(dir), b -> b.addSamplerCustomizer((s, c) -> Sampler.alwaysOn()))) {
      assertEquals(new TreeSet<>(Extension.HOST.getLiveList()), live(e));
      assertEquals(e.controls.sampler, sdk.built().getOpenTelemetrySdk().getSdkTracerProvider().getSampler());
    }
  }
}
