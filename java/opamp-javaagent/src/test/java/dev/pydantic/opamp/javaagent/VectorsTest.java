// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;

import com.fasterxml.jackson.databind.JsonNode;
import io.opentelemetry.api.logs.Severity;
import io.opentelemetry.sdk.common.InstrumentationScopeInfo;
import io.opentelemetry.sdk.testing.exporter.InMemoryLogRecordExporter;
import io.opentelemetry.sdk.testing.logs.TestLogRecordData;
import java.util.Arrays;
import java.util.List;
import java.util.stream.Stream;
import org.junit.jupiter.api.DynamicTest;
import org.junit.jupiter.api.TestFactory;

/**
 * Every shared vector that applies to the agent, run through the controls the agent builds its
 * SDK with: the same gates and sampler, not a copy of their logic.
 */
class VectorsTest {
  static Severity severity(int number) {
    return Arrays.stream(Severity.values()).filter(s -> s.getSeverityNumber() == number).findFirst().orElseThrow();
  }

  /** logs.json: whether the log gate passes a record on, by send_logs and the levels. */
  @TestFactory
  Stream<DynamicTest> logs() {
    return Vectors.cases("logs").stream().map(c -> DynamicTest.dynamicTest(c.get("name").asText(), () -> {
      Controls controls = new Controls();
      controls.apply(Vectors.settings(c));
      InMemoryLogRecordExporter out = InMemoryLogRecordExporter.create();
      JsonNode record = c.get("record");
      controls.logs(out).export(List.of(TestLogRecordData.builder()
          .setInstrumentationScopeInfo(InstrumentationScopeInfo.create(record.get("scope").asText()))
          .setSeverity(severity(record.get("severity").asInt())).setBody("x").build()));
      assertEquals(c.get("kept").asBoolean(), out.getFinishedLogRecordItems().size() == 1);
    }));
  }

  /** caps.json, its log runs: how many records of each batch go on, the clock injected. */
  @TestFactory
  Stream<DynamicTest> logCaps() {
    return Vectors.cases("caps").stream().filter(c -> c.get("signal").asText().equals("logs"))
        .map(c -> DynamicTest.dynamicTest(c.get("name").asText(), () -> {
          Controls controls = new Controls();
          controls.apply(Vectors.settings(c));
          InMemoryLogRecordExporter out = InMemoryLogRecordExporter.create();
          var gate = controls.logs(out);
          List<Long> passed = new java.util.ArrayList<>();
          for (JsonNode event : c.get("events")) {
            controls.nanoTime = () -> event.get("at_ns").asLong();
            int records = event.get("records").asInt();
            // Each record its number, to see which go on; a large batch is one record over and over.
            var same = TestLogRecordData.builder().setBody("0").setSeverity(Severity.INFO).build();
            List<io.opentelemetry.sdk.logs.data.LogRecordData> batch = new java.util.ArrayList<>();
            for (int i = 0; i < records; i++) {
              batch.add(records > 1000 ? same
                  : TestLogRecordData.builder().setBody(Integer.toString(i)).setSeverity(Severity.INFO).build());
            }
            out.reset();
            gate.export(batch);
            List<String> kept = out.getFinishedLogRecordItems().stream().map(r -> r.getBodyValue().asString()).toList();
            if (records <= 1000) {
              // The first ones, in order.
              assertEquals(java.util.stream.IntStream.range(0, kept.size()).mapToObj(Integer::toString).toList(), kept);
            }
            passed.add((long) kept.size());
          }
          List<Long> want = new java.util.ArrayList<>();
          c.get("want").forEach(w -> want.add(w.asLong()));
          assertEquals(want, passed);
        }));
  }

  /** A vector's attributes, as the agent's spans and records hold them. */
  static io.opentelemetry.api.common.Attributes attributes(JsonNode list) {
    var b = io.opentelemetry.api.common.Attributes.builder();
    java.util.Set<String> seen = new java.util.HashSet<>();
    for (JsonNode kv : list) {
      String key = kv.get("key").asText();
      if (!seen.add(key)) {
        continue; // a span holds one attribute a key: the first, as the sampler sees it
      }
      if (kv.has("string")) {
        b.put(io.opentelemetry.api.common.AttributeKey.stringKey(key), kv.get("string").asText());
      } else if (kv.has("int")) {
        b.put(io.opentelemetry.api.common.AttributeKey.longKey(key), kv.get("int").asLong());
      } else if (kv.has("bool")) {
        b.put(io.opentelemetry.api.common.AttributeKey.booleanKey(key), kv.get("bool").asBoolean());
      } else if (kv.has("double")) {
        b.put(io.opentelemetry.api.common.AttributeKey.doubleKey(key), kv.get("double").asDouble());
      } else {
        List<String> items = new java.util.ArrayList<>();
        kv.get("array").forEach(i -> items.add(i.asText()));
        b.put(io.opentelemetry.api.common.AttributeKey.stringArrayKey(key), items);
      }
    }
    return b.build();
  }

  /**
   * redact.json: what the span gate leaves of a span's, its event's and its link's attributes, and
   * the log gate of a record's. (A Java span holds one attribute per key, so a vector's repeated
   * key is one attribute on both sides.)
   */
  @TestFactory
  Stream<DynamicTest> redaction() {
    return Vectors.cases("redact").stream().map(c -> DynamicTest.dynamicTest(c.get("name").asText(), () -> {
      Controls controls = new Controls();
      controls.apply(Vectors.settings(c));
      var given = attributes(c.get("attributes"));
      var want = attributes(c.get("want"));
      var link = io.opentelemetry.sdk.trace.data.LinkData.create(io.opentelemetry.api.trace.SpanContext.create(
          "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331",
          io.opentelemetry.api.trace.TraceFlags.getSampled(), io.opentelemetry.api.trace.TraceState.getDefault()),
          given);
      var span = io.opentelemetry.sdk.testing.trace.TestSpanData.builder().setName("s")
          .setKind(io.opentelemetry.api.trace.SpanKind.INTERNAL)
          .setStartEpochNanos(1).setEndEpochNanos(2).setHasEnded(true)
          .setStatus(io.opentelemetry.sdk.trace.data.StatusData.unset())
          .setAttributes(given).setTotalAttributeCount(given.size())
          .setEvents(List.of(io.opentelemetry.sdk.trace.data.EventData.create(1, "e", given)))
          .setTotalRecordedEvents(1).setLinks(List.of(link)).setTotalRecordedLinks(1).build();
      var spans = io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter.create();
      controls.spans(spans).export(List.of(span));
      var exported = spans.getFinishedSpanItems().get(0);
      assertEquals(want, exported.getAttributes());
      assertEquals(want, exported.getEvents().get(0).getAttributes());
      assertEquals(want, exported.getLinks().get(0).getAttributes());
      // Dropped attributes are gone, not counted as dropped by the SDK's limits.
      assertEquals(0, exported.getTotalAttributeCount() - exported.getAttributes().size());
      assertEquals(0, exported.getEvents().get(0).getDroppedAttributesCount());
      assertEquals(0, exported.getLinks().get(0).getTotalAttributeCount() - exported.getLinks().get(0).getAttributes().size());
      // The application's span is as it was.
      assertEquals(given, span.getAttributes());

      InMemoryLogRecordExporter logs = InMemoryLogRecordExporter.create();
      var record = TestLogRecordData.builder().setBody("b").setSeverity(Severity.INFO).setAttributes(given)
          .setTotalAttributeCount(given.size()).build();
      controls.logs(logs).export(List.of(record));
      var out = logs.getFinishedLogRecordItems().get(0);
      assertEquals(want, out.getAttributes());
      assertEquals(out.getAttributes().size(), out.getTotalAttributeCount());
      assertEquals(given, record.getAttributes());
    }));
  }

  static final String TRACE_ID = "4bf92f3577b34da6a3ce929d0e0e4736";
  static final String SPAN_ID = "00f067aa0ba902b7";

  /** The parent context a vector names. */
  static io.opentelemetry.context.Context parent(String parent) {
    var root = io.opentelemetry.context.Context.root();
    var sampled = io.opentelemetry.api.trace.TraceFlags.getSampled();
    var unsampled = io.opentelemetry.api.trace.TraceFlags.getDefault();
    var state = io.opentelemetry.api.trace.TraceState.getDefault();
    io.opentelemetry.api.trace.SpanContext sc = switch (parent) {
      case "none" -> null;
      case "local_sampled" -> io.opentelemetry.api.trace.SpanContext.create(TRACE_ID, SPAN_ID, sampled, state);
      case "local_unsampled" -> io.opentelemetry.api.trace.SpanContext.create(TRACE_ID, SPAN_ID, unsampled, state);
      case "remote_sampled" -> io.opentelemetry.api.trace.SpanContext.createFromRemoteParent(TRACE_ID, SPAN_ID, sampled, state);
      case "remote_unsampled" ->
          io.opentelemetry.api.trace.SpanContext.createFromRemoteParent(TRACE_ID, SPAN_ID, unsampled, state);
      default -> throw new IllegalArgumentException(parent);
    };
    return sc == null ? root : root.with(io.opentelemetry.api.trace.Span.wrap(sc));
  }

  static io.opentelemetry.api.trace.SpanKind kind(String kind) {
    return io.opentelemetry.api.trace.SpanKind.valueOf(kind.toUpperCase(java.util.Locale.ROOT));
  }

  /** The application's own sampler in the tests: it says RECORD_ONLY, which no rate ever does. */
  static final io.opentelemetry.sdk.trace.samplers.Sampler APP = new io.opentelemetry.sdk.trace.samplers.Sampler() {
    @Override
    public io.opentelemetry.sdk.trace.samplers.SamplingResult shouldSample(io.opentelemetry.context.Context parent,
        String traceId, String name, io.opentelemetry.api.trace.SpanKind kind,
        io.opentelemetry.api.common.Attributes attributes, List<io.opentelemetry.sdk.trace.data.LinkData> links) {
      return io.opentelemetry.sdk.trace.samplers.SamplingResult.recordOnly();
    }

    @Override
    public String getDescription() {
      return "app";
    }
  };

  /**
   * sampling.json: how the live sampler decides a span at start, and what it then answers: the
   * application's sampler's answer, the parent's flag, or the SDK's ratio sampler at the rate.
   */
  @TestFactory
  Stream<DynamicTest> sampling() {
    var random = new java.util.Random(7);
    return Vectors.cases("sampling").stream().map(c -> DynamicTest.dynamicTest(c.get("name").asText(), () -> {
      Controls controls = new Controls();
      controls.sampler.wrap(APP);
      controls.apply(Vectors.settings(c));
      JsonNode span = c.get("span");
      JsonNode want = c.get("want");
      var context = parent(span.get("parent").asText());
      var attrs = attributes(span.get("attributes"));
      String name = span.get("name").asText();
      var kind = kind(span.get("kind").asText());
      LiveSampler.Decision d = controls.sampler.decide(
          io.opentelemetry.api.trace.Span.fromContext(context).getSpanContext(), name, kind, attrs);
      assertEquals(want.get("how").asText().toUpperCase(java.util.Locale.ROOT), d.how().name());
      for (int i = 0; i < 20; i++) {
        String traceId = String.format("%016x%016x", random.nextLong(), random.nextLong());
        var got = controls.sampler.shouldSample(context, traceId, name, kind, attrs, List.of()).getDecision();
        switch (d.how()) {
          case APP_SAMPLER -> assertEquals(io.opentelemetry.sdk.trace.samplers.SamplingDecision.RECORD_ONLY, got);
          case FOLLOW_PARENT -> {
            assertEquals(want.get("sampled").asBoolean(), d.parentSampled());
            assertEquals(d.parentSampled(), got == io.opentelemetry.sdk.trace.samplers.SamplingDecision.RECORD_AND_SAMPLE);
          }
          case RATE -> {
            assertEquals(want.get("rate").asDouble(), d.rate());
            assertEquals(want.get("rule").isNull() ? null : want.get("rule").asInt(), d.rule());
            assertEquals(io.opentelemetry.sdk.trace.samplers.Sampler.traceIdRatioBased(d.rate())
                .shouldSample(context, traceId, name, kind, attrs, List.of()).getDecision(), got);
          }
        }
      }
    }));
  }

  /** caps.json, its trace runs: which spans the sampler samples, the clock injected. */
  @TestFactory
  Stream<DynamicTest> traceCaps() {
    return Vectors.cases("caps").stream().filter(c -> c.get("signal").asText().equals("traces"))
        .map(c -> DynamicTest.dynamicTest(c.get("name").asText(), () -> {
          Controls controls = new Controls();
          // What the rules or rate decided, here the application's sampler, scripted per event.
          boolean[] decided = {false};
          controls.sampler.wrap(new io.opentelemetry.sdk.trace.samplers.Sampler() {
            @Override
            public io.opentelemetry.sdk.trace.samplers.SamplingResult shouldSample(io.opentelemetry.context.Context p,
                String t, String n, io.opentelemetry.api.trace.SpanKind k, io.opentelemetry.api.common.Attributes a,
                List<io.opentelemetry.sdk.trace.data.LinkData> l) {
              return decided[0] ? io.opentelemetry.sdk.trace.samplers.SamplingResult.recordAndSample()
                  : io.opentelemetry.sdk.trace.samplers.SamplingResult.drop();
            }

            @Override
            public String getDescription() {
              return "scripted";
            }
          });
          controls.apply(Vectors.settings(c));
          List<Long> passed = new java.util.ArrayList<>();
          for (JsonNode event : c.get("events")) {
            controls.nanoTime = () -> event.get("at_ns").asLong();
            decided[0] = event.get("sampled").asBoolean();
            var result = controls.sampler.shouldSample(parent(event.get("parent").asText()), TRACE_ID, "s",
                io.opentelemetry.api.trace.SpanKind.INTERNAL, io.opentelemetry.api.common.Attributes.empty(), List.of());
            passed.add(result.getDecision() == io.opentelemetry.sdk.trace.samplers.SamplingDecision.RECORD_AND_SAMPLE
                ? 1L : 0L);
          }
          List<Long> want = new java.util.ArrayList<>();
          c.get("want").forEach(w -> want.add(w.asLong()));
          assertEquals(want, passed);
        }));
  }
}
