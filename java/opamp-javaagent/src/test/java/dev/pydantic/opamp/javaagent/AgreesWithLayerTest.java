// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;

import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.google.protobuf.ByteString;
import dev.pydantic.opamp.sdk.SdkSettings;
import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.common.AttributesBuilder;
import io.opentelemetry.api.logs.Severity;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.sdk.common.InstrumentationScopeInfo;
import io.opentelemetry.sdk.testing.exporter.InMemoryLogRecordExporter;
import io.opentelemetry.sdk.testing.logs.TestLogRecordData;
import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Random;
import java.util.Set;
import opamp.proto.v1.Anyvalue;
import org.junit.jupiter.api.Test;

/**
 * The agent's own matchers against the layer's reference ones (its test-only {@code Evaluate}
 * call), on generated settings and inputs: the sampler's decision, the log gate's levels and
 * redaction. The shared vectors pin the cases someone thought of; these look for the rest.
 */
class AgreesWithLayerTest {
  private static final int RUNS = 400;
  /** Characters the generated globs and texts are made of: `*`, literals that look special, and two past ASCII (one past the BMP). */
  private static final String[] CHARS = {"a", "b", "A", "*", "?", ".", "/", "é", "😀"};
  private static final String[] KINDS = {"server", "client", "internal", "producer", "consumer"};
  private static final String[] PARENTS = {"none", "local_sampled", "local_unsampled", "remote_sampled", "remote_unsampled"};

  /** The layer's test-only Evaluate call, which the wrapper doesn't expose: through its one entry point. */
  static SettingsProto.Evaluated evaluate(SettingsProto.Evaluate.Builder e) throws Exception {
    Method call = SdkSettings.class.getDeclaredMethod("call", SettingsProto.Request.class);
    call.setAccessible(true);
    try {
      return ((SettingsProto.Reply) call.invoke(Vectors.LAYER,
          SettingsProto.Request.newBuilder().setEvaluate(e).build())).getEvaluated();
    } catch (InvocationTargetException x) {
      throw (Exception) x.getCause();
    }
  }

  static SettingsProto.Evaluate.Builder request(ObjectNode settings) {
    return SettingsProto.Evaluate.newBuilder().setHost(Vectors.ALL).setNowUnixMs(Vectors.NOW)
        .setSettings(ByteString.copyFrom(policy(settings), StandardCharsets.UTF_8));
  }

  /**
   * The version 2 policy for the generated settings. The generators fill a flat object of the
   * fields they vary, and this puts each field under its control.
   */
  static String policy(ObjectNode settings) {
    List<String> set = new ArrayList<>();
    if (settings.has("rules") && !settings.get("rules").isEmpty()) {
      set.addAll(List.of("sampling_rules", Vectors.override("\"rules\":" + settings.get("rules"))));
    }
    if (settings.has("ratio")) {
      set.addAll(List.of("head_sampling", Vectors.override("\"ratio\":" + settings.get("ratio"))));
    }
    List<String> levels = new ArrayList<>();
    for (String field : List.of("level", "scopes")) {
      if (settings.has(field)) {
        levels.add("\"" + field + "\":" + settings.get(field));
      }
    }
    if (!levels.isEmpty()) {
      set.addAll(List.of("log_levels", Vectors.override(String.join(",", levels))));
    }
    if (settings.has("logs")) {
      set.addAll(List.of("signal_exports", Vectors.exports("", "", "off")));
    }
    List<String> redaction = new ArrayList<>();
    for (String field : List.of("drop", "mask")) {
      if (settings.has(field)) {
        redaction.add("\"" + field + "\":" + settings.get(field));
      }
    }
    if (!redaction.isEmpty()) {
      set.addAll(List.of("attribute_redaction", Vectors.override(String.join(",", redaction))));
    }
    return Vectors.v2(set.toArray(String[]::new));
  }

  static String text(Random r, int max) {
    StringBuilder b = new StringBuilder();
    for (int i = r.nextInt(max + 1); i > 0; i--) {
      b.append(CHARS[r.nextInt(CHARS.length)]);
    }
    return b.toString();
  }

  /** A generated attribute: its key from a few, so rules and redaction often meet it. */
  record Attr(String key, Object value) {}

  static List<Attr> attrs(Random r, String... keys) {
    List<Attr> out = new ArrayList<>();
    for (int i = r.nextInt(4); i > 0; i--) {
      String key = keys[r.nextInt(keys.length)];
      Object value = switch (r.nextInt(5)) {
        case 0 -> (long) new int[] {-12, 0, 7, 200}[r.nextInt(4)];
        case 1 -> r.nextBoolean();
        case 2 -> 0.5;
        case 3 -> List.of(text(r, 2));
        default -> text(r, 4);
      };
      out.add(new Attr(key, value));
    }
    return out;
  }

  /** As a span or record holds them: one attribute a key, the first. */
  @SuppressWarnings("unchecked")
  static Attributes java(List<Attr> attrs) {
    AttributesBuilder b = Attributes.builder();
    Set<String> seen = new HashSet<>();
    for (Attr a : attrs) {
      if (!seen.add(a.key())) {
        continue;
      }
      if (a.value() instanceof Long n) {
        b.put(AttributeKey.longKey(a.key()), n);
      } else if (a.value() instanceof Boolean v) {
        b.put(AttributeKey.booleanKey(a.key()), v);
      } else if (a.value() instanceof Double d) {
        b.put(AttributeKey.doubleKey(a.key()), d);
      } else if (a.value() instanceof List<?> l) {
        b.put(AttributeKey.stringArrayKey(a.key()), (List<String>) l);
      } else {
        b.put(AttributeKey.stringKey(a.key()), (String) a.value());
      }
    }
    return b.build();
  }

  static List<Anyvalue.KeyValue> proto(List<Attr> attrs) {
    return attrs.stream().map(a -> Anyvalue.KeyValue.newBuilder().setKey(a.key()).setValue(value(a.value())).build())
        .toList();
  }

  static Anyvalue.AnyValue value(Object v) {
    Anyvalue.AnyValue.Builder b = Anyvalue.AnyValue.newBuilder();
    if (v instanceof Long n) {
      b.setIntValue(n);
    } else if (v instanceof Boolean x) {
      b.setBoolValue(x);
    } else if (v instanceof Double d) {
      b.setDoubleValue(d);
    } else if (v instanceof List<?> l) {
      Anyvalue.ArrayValue.Builder a = Anyvalue.ArrayValue.newBuilder();
      l.forEach(i -> a.addValues(value(i)));
      b.setArrayValue(a);
    } else {
      b.setStringValue((String) v);
    }
    return b.build();
  }

  static Attributes fromProto(List<Anyvalue.KeyValue> kvs) {
    List<Attr> out = new ArrayList<>();
    for (Anyvalue.KeyValue kv : kvs) {
      Anyvalue.AnyValue v = kv.getValue();
      Object o = switch (v.getValueCase()) {
        case INT_VALUE -> v.getIntValue();
        case BOOL_VALUE -> v.getBoolValue();
        case DOUBLE_VALUE -> v.getDoubleValue();
        case ARRAY_VALUE -> v.getArrayValue().getValuesList().stream().map(Anyvalue.AnyValue::getStringValue).toList();
        default -> v.getStringValue();
      };
      out.add(new Attr(kv.getKey(), o));
    }
    return java(out);
  }

  @Test
  void theSamplerDecidesAsTheLayerDoes() throws Exception {
    Random r = new Random(1);
    for (int run = 0; run < RUNS; run++) {
      ObjectNode doc = Vectors.JSON.createObjectNode();
      ArrayNode rules = doc.putArray("rules");
      for (int i = r.nextInt(4); i > 0; i--) {
        ObjectNode rule = rules.addObject();
        rule.put("rate", new double[] {0, 0.25, 1}[r.nextInt(3)]);
        if (r.nextInt(3) == 0) {
          rule.put("name", text(r, 3));
        }
        if (r.nextInt(3) == 0) {
          rule.put("kind", KINDS[r.nextInt(KINDS.length)]);
        }
        if (r.nextInt(3) == 0) {
          rule.put("path", "/" + text(r, 2));
        }
        if (r.nextInt(3) == 0) {
          ObjectNode a = rule.putObject("attributes");
          a.put(new String[] {"k", "n"}[r.nextInt(2)], text(r, 2));
        }
      }
      if (r.nextBoolean()) {
        doc.put("ratio", 0.5);
      }
      String name = text(r, 3);
      String kind = KINDS[r.nextInt(KINDS.length)];
      String parent = PARENTS[r.nextInt(PARENTS.length)];
      List<Attr> attrs = attrs(r, "k", "n", "url.path", "http.target");
      for (Attr a : List.copyOf(attrs)) {
        if (a.key().equals("url.path") || a.key().equals("http.target")) {
          attrs.set(attrs.indexOf(a), new Attr(a.key(), "/" + text(r, 2) + (r.nextBoolean() ? "?q" : "")));
        }
      }
      SettingsProto.Evaluated want = evaluate(request(doc).setSpan(SettingsProto.SpanStart.newBuilder()
          .setName(name).setKind(SettingsProto.SpanKind.valueOf("SPAN_KIND_" + kind.toUpperCase(java.util.Locale.ROOT)))
          .addAllAttributes(proto(attrs)).setParent(SettingsProto.Parent.valueOf("PARENT_" + parent.toUpperCase(java.util.Locale.ROOT)))));
      assertEquals("", want.getRefused(), doc.toString());

      Controls c = new Controls();
      c.sampler.wrap(VectorsTest.APP);
      c.apply(Vectors.settings(policy(doc), Vectors.NOW));
      LiveSampler.Decision got = c.sampler.decide(Span.fromContext(VectorsTest.parent(parent)).getSpanContext(), name,
          VectorsTest.kind(kind), java(attrs));
      String what = doc + " " + name + " " + kind + " " + parent + " " + attrs;
      SettingsProto.SpanSampling w = want.getSampling();
      if (w.getHow() == SettingsProto.SpanSampling.How.APP_SAMPLER || doc.get("rules").isEmpty()
          && !doc.has("ratio")) {
        // With nothing set the agent's own sampler decides children too (as before these
        // settings); the layer's reference follows the parent. Anything set, they agree.
        assertEquals(LiveSampler.How.APP_SAMPLER, got.how(), what);
        continue;
      }
      assertEquals(w.getHow().name(), got.how().name(), what);
      switch (got.how()) {
        case FOLLOW_PARENT -> assertEquals(w.getParentSampled(), got.parentSampled(), what);
        case RATE -> {
          assertEquals(w.getRate(), got.rate(), what);
          assertEquals(w.hasRule() ? w.getRule() : null, got.rule(), what);
        }
        default -> { }
      }
    }
  }

  @Test
  void theLogGateKeepsWhatTheLayerKeeps() throws Exception {
    Random r = new Random(2);
    String[] patterns = {"a", "a*", "a.b", "a.b*", "*", "b", "é*"};
    String[] levels = {"trace", "debug", "info", "warn", "error", "fatal", "off"};
    String[] scopes = {"", "a", "a.b", "a.bc", "b", "c", "é.x"};
    for (int run = 0; run < RUNS; run++) {
      ObjectNode doc = Vectors.JSON.createObjectNode();
      ObjectNode entries = Vectors.JSON.createObjectNode();
      for (int i = r.nextInt(4); i > 0; i--) {
        String p = patterns[r.nextInt(patterns.length)];
        if (!entries.has(p)) {
          entries.put(p, levels[r.nextInt(levels.length)]);
        }
      }
      if (!entries.isEmpty()) {
        doc.set("scopes", entries);
      }
      if (r.nextBoolean()) {
        doc.put("level", levels[r.nextInt(levels.length - 1)]);
      }
      if (r.nextInt(8) == 0) {
        doc.put("logs", "off");
      }
      String scope = scopes[r.nextInt(scopes.length)];
      int severity = r.nextInt(25);
      SettingsProto.Evaluated want = evaluate(request(doc).setLog(
          SettingsProto.LogRecord.newBuilder().setScope(scope).setSeverity(severity)));
      assertEquals("", want.getRefused(), doc.toString());

      Controls c = new Controls();
      c.apply(Vectors.settings(policy(doc), Vectors.NOW));
      InMemoryLogRecordExporter out = InMemoryLogRecordExporter.create();
      c.logs(out).export(List.of(TestLogRecordData.builder().setBody("x")
          .setInstrumentationScopeInfo(InstrumentationScopeInfo.create(scope))
          .setSeverity(VectorsTest.severity(severity)).build()));
      assertEquals(want.getKept(), out.getFinishedLogRecordItems().size() == 1, doc + " " + scope + " " + severity);
    }
  }

  @Test
  void redactionLeavesWhatTheLayerLeaves() throws Exception {
    Random r = new Random(3);
    for (int run = 0; run < RUNS; run++) {
      ObjectNode doc = Vectors.JSON.createObjectNode();
      for (String key : List.of("drop", "mask")) {
        if (r.nextBoolean()) {
          ArrayNode ps = doc.putArray(key);
          // The policy refuses an empty glob.
          while (ps.size() < 1 || r.nextInt(3) == 0 && ps.size() < 3) {
            String p = text(r, 3).replace("?", "");
            if (!p.isEmpty()) {
              ps.add(p);
            }
          }
        }
      }
      List<Attr> attrs = new ArrayList<>();
      Set<String> keys = new HashSet<>();
      for (int i = r.nextInt(5); i > 0; i--) {
        String key = text(r, 3);
        if (keys.add(key)) {
          attrs.add(new Attr(key, attrs(r, key).stream().findFirst().map(Attr::value).orElse("v")));
        }
      }
      SettingsProto.Evaluated want = evaluate(request(doc).setAttributes(
          SettingsProto.Attributes.newBuilder().addAllAttributes(proto(attrs))));
      assertEquals("", want.getRefused(), doc.toString());

      Controls c = new Controls();
      c.apply(Vectors.settings(policy(doc), Vectors.NOW));
      InMemoryLogRecordExporter out = InMemoryLogRecordExporter.create();
      Attributes given = java(attrs);
      c.logs(out).export(List.of(TestLogRecordData.builder().setBody("x").setSeverity(Severity.INFO)
          .setAttributes(given).build()));
      assertEquals(fromProto(want.getRedactedList()), out.getFinishedLogRecordItems().get(0).getAttributes(),
          doc + " " + attrs);
    }
  }
}
