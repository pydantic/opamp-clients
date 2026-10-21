// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertSame;

import com.fasterxml.jackson.databind.JsonNode;
import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.common.KeyValue;
import io.opentelemetry.api.common.Value;
import io.opentelemetry.api.logs.Severity;
import io.opentelemetry.api.trace.SpanContext;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.TraceFlags;
import io.opentelemetry.api.trace.TraceState;
import io.opentelemetry.sdk.logs.data.LogRecordData;
import io.opentelemetry.sdk.testing.exporter.InMemoryLogRecordExporter;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.testing.logs.TestLogRecordData;
import io.opentelemetry.sdk.testing.trace.TestSpanData;
import io.opentelemetry.sdk.trace.data.EventData;
import io.opentelemetry.sdk.trace.data.LinkData;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.data.StatusData;
import java.util.ArrayList;
import java.util.List;
import java.util.Random;
import java.util.stream.Stream;
import opamp.proto.v1.Anyvalue;
import org.junit.jupiter.api.DynamicTest;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.TestFactory;

/**
 * scrub_defaults and scrub_patterns, through the gates the agent builds its SDK with: the shared
 * vectors, live changes, and the layer's reference (Evaluate) on generated keys, values and bodies.
 */
class ScrubbingTest {
  static SpanData span(Attributes given) {
    LinkData link = LinkData.create(SpanContext.create("0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331",
        TraceFlags.getSampled(), TraceState.getDefault()), given);
    return TestSpanData.builder().setName("s").setKind(SpanKind.INTERNAL)
        .setStartEpochNanos(1).setEndEpochNanos(2).setHasEnded(true).setStatus(StatusData.unset())
        .setAttributes(given).setTotalAttributeCount(given.size())
        .setEvents(List.of(EventData.create(1, "e", given))).setTotalRecordedEvents(1)
        .setLinks(List.of(link)).setTotalRecordedLinks(1).build();
  }

  static LogRecordData record(Attributes given, Value<?> body) {
    var b = TestLogRecordData.builder().setSeverity(Severity.INFO).setAttributes(given)
        .setTotalAttributeCount(given.size());
    if (body != null) {
      b.setBodyValue(body);
    }
    return b.build();
  }

  /** A vector's log body: a string (or another typed value), or a map of members, nested. */
  static Value<?> body(JsonNode b) {
    if (b.has("map")) {
      List<KeyValue> members = new ArrayList<>();
      b.get("map").forEach(m -> members.add(KeyValue.of(m.get("key").asText(), body(m))));
      return Value.of(members.toArray(KeyValue[]::new));
    }
    if (b.has("string")) {
      return Value.of(b.get("string").asText());
    }
    if (b.has("int")) {
      return Value.of(b.get("int").asLong());
    }
    if (b.has("bool")) {
      return Value.of(b.get("bool").asBoolean());
    }
    if (b.has("double")) {
      return Value.of(b.get("double").asDouble());
    }
    List<Value<?>> items = new ArrayList<>();
    b.get("array").forEach(i -> items.add(Value.of(i.asText())));
    return Value.of(items);
  }

  /**
   * scrub.json: what the span gate leaves of a span's, its event's and its link's attributes, and
   * the log gate of a record's attributes and body.
   */
  @TestFactory
  Stream<DynamicTest> vectors() {
    List<JsonNode> cases = Vectors.cases("scrub");
    if (System.getenv("OPAMP_SETTINGS_VECTORS") == null) {
      assertEquals(true, cases.size() >= 30, cases.size() + " cases: scrub.json was cut short");
    }
    return cases.stream().map(c -> DynamicTest.dynamicTest(c.get("name").asText(), () -> {
      Controls controls = new Controls();
      controls.apply(Vectors.settings(c));
      Attributes given = VectorsTest.attributes(c.get("attributes"));
      Attributes want = VectorsTest.attributes(c.get("want"));
      SpanData span = span(given);
      InMemorySpanExporter spans = InMemorySpanExporter.create();
      controls.spans(spans).export(List.of(span));
      SpanData exported = spans.getFinishedSpanItems().get(0);
      assertEquals(want, exported.getAttributes());
      assertEquals(want, exported.getEvents().get(0).getAttributes());
      assertEquals(want, exported.getLinks().get(0).getAttributes());
      assertEquals(given, span.getAttributes());

      JsonNode b = c.get("body");
      Value<?> givenBody = b == null || b.isNull() ? Value.of("b") : body(b);
      InMemoryLogRecordExporter logs = InMemoryLogRecordExporter.create();
      LogRecordData r = record(given, givenBody);
      controls.logs(logs).export(List.of(r));
      LogRecordData out = logs.getFinishedLogRecordItems().get(0);
      assertEquals(want, out.getAttributes());
      assertEquals(b == null || b.isNull() ? givenBody : body(c.get("want_body")), out.getBodyValue());
      assertEquals(givenBody, r.getBodyValue());
    }));
  }

  @Test
  void scrubbingChangesLiveAndRevertsAndTheApplicationsOwnIsLeftAlone() {
    Controls c = new Controls();
    Attributes asIs = Attributes.of(AttributeKey.stringKey("api_key"), "k1", AttributeKey.stringKey("note"),
        "my secret is x", AttributeKey.stringKey("iban"), "DE89");
    InMemorySpanExporter out = InMemorySpanExporter.create();
    var gate = c.spans(out);
    SpanData span = span(asIs);
    gate.export(List.of(span));
    assertSame(span, out.getFinishedSpanItems().get(0));

    c.apply(Vectors.settings(Vectors.every("scrubbing", Vectors.override("\"defaults\":true")), Vectors.NOW));
    out.reset();
    gate.export(List.of(span));
    assertEquals(Attributes.of(AttributeKey.stringKey("api_key"), "[Scrubbed due to 'api_key']",
        AttributeKey.stringKey("note"), "[Scrubbed due to 'secret']", AttributeKey.stringKey("iban"), "DE89"),
        out.getFinishedSpanItems().get(0).getAttributes());

    c.apply(Vectors.settings(Vectors.every("scrubbing", Vectors.override("\"patterns\":[\"IBAN\"]")), Vectors.NOW));
    out.reset();
    gate.export(List.of(span));
    assertEquals(asIs.toBuilder().put("iban", "[Scrubbed due to 'iban']").build(),
        out.getFinishedSpanItems().get(0).getAttributes());

    c.apply(Vectors.settings(Vectors.every(), Vectors.NOW));
    out.reset();
    gate.export(List.of(span));
    assertSame(span, out.getFinishedSpanItems().get(0));

    // A map body: each member as an attribute; a list's strings one by one, its other items kept.
    c.apply(Vectors.settings(Vectors.every("scrubbing", Vectors.override("\"defaults\":true")), Vectors.NOW));
    Value<?> body = Value.of(KeyValue.of("token", Value.of("t")), KeyValue.of("cookie", Value.of("c")),
        KeyValue.of("lines", Value.of(List.<Value<?>>of(Value.of("wrong password"), Value.of(3L)))),
        KeyValue.of("n", Value.of(1L)));
    InMemoryLogRecordExporter logs = InMemoryLogRecordExporter.create();
    LogRecordData r = record(Attributes.empty(), body);
    c.logs(logs).export(List.of(r));
    LogRecordData got = logs.getFinishedLogRecordItems().get(0);
    assertEquals(Value.of(KeyValue.of("token", Value.of("t")), KeyValue.of("cookie", Value.of("[Scrubbed due to 'cookie']")),
        KeyValue.of("lines", Value.of(List.<Value<?>>of(Value.of("[Scrubbed due to 'password']"), Value.of(3L)))),
        KeyValue.of("n", Value.of(1L))), got.getBodyValue());
    assertSame(body, r.getBodyValue());
    // The deprecated Body of a scrubbed record is its body as a string, as the SDK's own is.
    @SuppressWarnings("deprecation")
    String text = got.getBody().asString();
    assertEquals(got.getBodyValue().asString(), text);
  }

  @Test
  void theDefaultPatternsAtTheirEdges() {
    Scrubbing s = Scrubbing.of(true, List.of());
    String[][] cases = {
        {"", null}, {"password", null}, {"a password", "password"}, {"PassWord!", "PassWord"},
        {"author", null}, {"authors", null}, {"authorx", "auth"}, {"auth", null}, {"oauth x", "auth"},
        {"authors_x", "auth"}, {"api_key=1", "api_key"}, {"api  key", null}, {"apikey!", "apikey"},
        {"my_jwt_", "_jwt_"}, {"xjwt", null}, {"jwt_x", "jwt_"}, {"pylf_v1_x", "pylf_v1_"}, {"pylf_v_", null},
        {"ſecret!", "ſecret"}, {"apiKey!", "apiKey"}, {"İ éssn", "ssn"},
        {"kéjwté", "jwt"}, {"x 😀secret", "secret"}, {"session cookie", "session"},
    };
    for (String[] c : cases) {
      assertEquals(c[1] == null ? null : Scrubbing.scrubbed(c[1]), s.text(c[0]), c[0]);
    }
    assertEquals(Scrubbing.VALUES, s.key("Exception.Message"));
    assertEquals(Scrubbing.KEEP, s.key("exception.message"));
    assertEquals(Scrubbing.KEEP, Scrubbing.of(false, List.of("*")).key("url.full"));
    assertEquals(Scrubbing.KEEP, Scrubbing.NONE.key("password"));
    // A key a pattern matches: a value of any type becomes the string.
    Redaction r = Redaction.of(SettingsProto.Settings.newBuilder().addScrubPatterns("n*").build());
    assertEquals(Attributes.of(AttributeKey.stringKey("n"), "[Scrubbed due to 'n*']", AttributeKey.longKey("m"), 1L),
        r.apply(Attributes.of(AttributeKey.longArrayKey("n"), List.of(1L), AttributeKey.longKey("m"), 1L)).attributes());
  }

  private static final String[] PIECES = {"pass", "word", "auth", "or", "s", "api", "key", "_", "-", ".", " ", "jwt",
      "ssn", "pylf_v", "1", "x", "K", "K", "ſ", "İ", "é", "session", "safe", "😀"};

  static String text(Random r) {
    StringBuilder b = new StringBuilder();
    for (int i = r.nextInt(6); i > 0; i--) {
      b.append(PIECES[r.nextInt(PIECES.length)]);
    }
    return b.toString();
  }

  /** The gates' scrubbing against the layer's (Evaluate), on generated keys, values and bodies. */
  @Test
  void scrubbingLeavesWhatTheLayerLeaves() throws Exception {
    Random r = new Random(11);
    for (int run = 0; run < 400; run++) {
      List<String> patterns = new ArrayList<>();
      for (int i = r.nextInt(3); i > 0; i--) {
        String p = text(r).strip() + "*";
        if (!p.equals("*") && !p.contains(",")) {
          patterns.add(Vectors.JSON.writeValueAsString(p));
        }
      }
      List<String> fields = new ArrayList<>();
      if (r.nextInt(4) != 0) {
        fields.add("\"defaults\":true");
      }
      if (!patterns.isEmpty()) {
        fields.add("\"patterns\":[" + String.join(",", patterns) + "]");
      }
      String doc = fields.isEmpty() ? Vectors.every() : Vectors.every("scrubbing", Vectors.override(String.join(",", fields)));
      List<AgreesWithLayerTest.Attr> attrs = new ArrayList<>();
      java.util.Set<String> keys = new java.util.HashSet<>();
      for (int i = r.nextInt(5); i > 0; i--) {
        String key = text(r);
        if (!key.isEmpty() && keys.add(key)) {
          attrs.add(new AgreesWithLayerTest.Attr(key, r.nextInt(4) == 0 ? List.of(text(r)) : text(r)));
        }
      }
      String bodyText = text(r);
      SettingsProto.Evaluated want = AgreesWithLayerTest.evaluate(SettingsProto.Evaluate.newBuilder()
          .setHost(Vectors.ALL).setNowUnixMs(Vectors.NOW)
          .setSettings(com.google.protobuf.ByteString.copyFromUtf8(doc))
          .setAttributes(SettingsProto.Attributes.newBuilder().addAllAttributes(AgreesWithLayerTest.proto(attrs))
              .setBody(Anyvalue.AnyValue.newBuilder().setStringValue(bodyText))));
      assertEquals("", want.getRefused(), doc);

      Controls c = new Controls();
      c.apply(Vectors.settings(doc, Vectors.NOW));
      InMemoryLogRecordExporter out = InMemoryLogRecordExporter.create();
      c.logs(out).export(List.of(record(AgreesWithLayerTest.java(attrs), Value.of(bodyText))));
      LogRecordData got = out.getFinishedLogRecordItems().get(0);
      assertEquals(AgreesWithLayerTest.fromProto(want.getRedactedList()), got.getAttributes(), doc + " " + attrs);
      assertEquals(want.getBody().getStringValue(), got.getBodyValue().asString(), doc + " " + bodyText);
    }
  }
}
