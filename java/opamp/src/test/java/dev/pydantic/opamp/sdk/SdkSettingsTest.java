// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.sdk;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.google.protobuf.ByteString;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.List;
import java.util.stream.Stream;
import opamp.proto.v1.Anyvalue;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.DynamicTest;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.TestFactory;

/** The layer through the module: what the Rust tests hold it to, reached from Java. */
class SdkSettingsTest {
  private final SdkSettings layer = new SdkSettings();

  static Opamp.AgentRemoteConfig config(String body) {
    return Opamp.AgentRemoteConfig.newBuilder().setConfigHash(ByteString.copyFromUtf8("h"))
        .setConfig(Opamp.AgentConfigMap.newBuilder().putConfigMap("elastic", Opamp.AgentConfigObject.newBuilder()
            .setBody(ByteString.copyFromUtf8(body)).setContentType("application/json").build()))
        .build();
  }

  static String effective(Opamp.EffectiveConfig e) {
    return e.getConfigMap().getConfigMapMap().get("elastic").getBody().toStringUtf8();
  }

  @Test
  void loadsDecidesAndDescribes() {
    SettingsProto.Loaded l = layer.load("{\"send_logs\":\"false\"}".getBytes(), DECLARING, 0);
    assertFalse(l.getSettings().getSendLogs());
    assertEquals("{\"send_logs\":\"false\"}", effective(l.getEffective()));

    List<String> kept = new java.util.ArrayList<>();
    SettingsProto.Decision d = layer.decide(config("{\"sampling_rate\":\"0.25\"}"), l.getCurrent(), l.getCurrent(),
        DECLARING, 0, doc -> kept.add(new String(doc, java.nio.charset.StandardCharsets.UTF_8)));
    assertEquals(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, d.getStatus().getStatus());
    assertEquals(ByteString.copyFromUtf8("h"), d.getStatus().getLastRemoteConfigHash());
    assertEquals(0.25, d.getApply().getSamplingRate());
    assertEquals(List.of("{\"sampling_rate\":\"0.25\"}"), kept);
    assertEquals("{\"sampling_rate\":\"0.25\"}", effective(d.getEffective()));

    SettingsProto.Decision bad = layer.decide(config("{\"sampling_rate\":\"x\"}"), d.getCurrent(), l.getCurrent(),
        DECLARING, 0, doc -> kept.add("again"));
    assertEquals(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_FAILED, bad.getStatus().getStatus());
    assertTrue(bad.getStatus().getErrorMessage().startsWith("sampling_rate:"));
    assertFalse(bad.hasApply());
    assertEquals(d.getCurrent(), bad.getCurrent());
    assertEquals(1, kept.size()); // refused: nothing to keep
  }

  /** A save that keeps nothing, for a host that has somewhere to keep state. */
  static final SdkSettings.Saver KEEP = doc -> { };

  /**
   * Decide, then commit (#215): the integration saves what the layer asks it to keep, and the layer
   * answers from how that went, the same in every language.
   */
  @Test
  void theSaveTheLayerAsksForDecidesTheAnswer() {
    ByteString current = ByteString.copyFromUtf8("{\"sampling_rate\":\"0.5\"}");
    SettingsProto.Decision failed = layer.decide(config("{\"sampling_rate\":\"0.25\"}"), current, current, DECLARING,
        0, doc -> {
          throw new java.io.IOException("No space left on device");
        });
    assertEquals(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_FAILED, failed.getStatus().getStatus());
    assertEquals("can't save the settings: No space left on device", failed.getStatus().getErrorMessage());
    assertFalse(failed.hasApply());
    assertEquals(current, failed.getCurrent());
    assertEquals("{\"sampling_rate\":\"0.5\"}", effective(failed.getEffective()));
    // Whatever stops the save refuses the config.
    SettingsProto.Decision odd = layer.decide(config("{\"sampling_rate\":\"0.25\"}"), current, current, DECLARING, 0,
        doc -> {
          throw new IllegalStateException();
        });
    assertEquals("can't save the settings: java.lang.IllegalStateException", odd.getStatus().getErrorMessage());
    // Nowhere to keep state: a live change is applied, a next-start one refused.
    SettingsProto.Decision live = layer.decide(config("{\"sampling_rate\":\"0.25\"}"), current, current, DECLARING, 0,
        null);
    assertEquals(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, live.getStatus().getStatus());
    SettingsProto.Decision later = layer.decide(config("{\"opamp_polling_interval\":\"1m\"}"), current, current,
        DECLARING, 0, null);
    assertEquals("opamp_polling_interval take effect at the next start, which needs somewhere to keep state",
        later.getStatus().getErrorMessage());
    SettingsProto.Decision saved = layer.decide(config("{\"opamp_polling_interval\":\"1m\"}"), current, current,
        DECLARING, 0, KEEP);
    assertEquals(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, saved.getStatus().getStatus());
    assertEquals(List.of("opamp_polling_interval"), saved.getPendingRestartList());

    Opamp.AgentDescription desc = layer.describe(List.of(
        Anyvalue.KeyValue.newBuilder().setKey("service.name")
            .setValue(Anyvalue.AnyValue.newBuilder().setStringValue("checkout")).build(),
        Anyvalue.KeyValue.newBuilder().setKey("host.name")
            .setValue(Anyvalue.AnyValue.newBuilder().setStringValue("h")).build()));
    assertEquals(1, desc.getIdentifyingAttributesCount());
    assertEquals(1, desc.getNonIdentifyingAttributesCount());
  }

  @Test
  void aLogLevelArrivesAsTheLowestSeverityNumberToExport() {
    SettingsProto.Decision d = layer.decide(config("{\"send_logs_level\":\"Warning\"}"), ByteString.EMPTY,
        ByteString.EMPTY, DECLARING, 0, null);
    assertEquals(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, d.getStatus().getStatus());
    assertTrue(d.getApply().hasSendLogsMinSeverity());
    assertEquals(13, d.getApply().getSendLogsMinSeverity());
    assertEquals("{\"send_logs_level\":\"warn\"}", effective(d.getEffective()));
    // Unset: every severity.
    assertFalse(layer.load("{}".getBytes(), DECLARING, 0).getSettings().hasSendLogsMinSeverity());
  }

  /** A host that declares every setting: each live one at once, each next-start one at the next start. */
  static final SettingsProto.Host DECLARING = SettingsProto.Host.newBuilder().addAllLive(List.of(
      "debug_sampling_rate", "debug_send_logs_level", "debug_until", "drop_attributes", "mask_attributes",
      "sampling_rate", "sampling_rules", "sampling_traces_per_second", "send_logs", "send_logs_level",
      "send_logs_level_by_scope", "send_logs_per_second", "send_metrics", "send_traces"))
      .addAllNextStart(List.of("deactivate_all_instrumentations", "deactivate_instrumentations",
          "opamp_polling_interval")).build();
  static final long NOW = 1_791_979_200_000L; // 2026-10-14T12:00:00Z

  @Test
  void theSettingsThatMatchTelemetryArriveTypedWhereTheHostDeclaresThem() {
    String doc = "{\"sampling_rules\":[{\"rate\":0,\"kind\":\"server\",\"path\":\"/health*\"}],"
        + "\"send_logs_level_by_scope\":\"com.acme.db*=debug\",\"drop_attributes\":\"*token*\","
        + "\"mask_attributes\":\"User.Email\",\"sampling_traces_per_second\":\"50\",\"send_logs_per_second\":\"0.5\"}";
    SettingsProto.Decision d = layer.decide(config(doc), ByteString.EMPTY, ByteString.EMPTY, DECLARING, NOW, null);
    assertEquals(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, d.getStatus().getStatus());
    SettingsProto.Settings s = d.getApply();
    assertEquals(1, s.getSamplingRulesCount());
    SettingsProto.SamplingRule rule = s.getSamplingRules(0);
    assertEquals(0.0, rule.getRate());
    assertEquals(SettingsProto.SpanKind.SPAN_KIND_SERVER, rule.getKind());
    assertEquals("/health*", rule.getPath());
    assertEquals("com.acme.db*", s.getSendLogsScopeLevels(0).getPattern());
    assertEquals(5, s.getSendLogsScopeLevels(0).getMinSeverity());
    assertEquals(List.of("*token*"), s.getDropAttributesList());
    assertEquals(List.of("user.email"), s.getMaskAttributesList());
    assertEquals(50.0, s.getSamplingTracesPerSecond());
    assertEquals(0.5, s.getSendLogsPerSecond());
    // Undeclared: ignored, never in force.
    SettingsProto.Decision plain = layer.decide(config(doc), ByteString.EMPTY, ByteString.EMPTY,
        SettingsProto.Host.newBuilder().addLive("sampling_rate").build(), NOW, null);
    assertEquals(0, plain.getApply().getSamplingRulesCount());
    assertEquals(6, plain.getApply().getUnsupportedCount());
    assertEquals("{}", effective(plain.getEffective()));
  }

  /** What the effective config names as set aside (its elastic.set_aside entry); null without one. */
  static String setAside(Opamp.EffectiveConfig e) {
    var files = e.getConfigMap().getConfigMapMap();
    for (String name : files.keySet()) {
      assertTrue(name.equals("elastic") || name.equals("elastic.set_aside"), name);
    }
    Opamp.AgentConfigObject f = files.get("elastic.set_aside");
    if (f == null) {
      return null;
    }
    assertEquals("application/json", f.getContentType());
    return f.getBody().toStringUtf8();
  }

  @Test
  void whatIsSetAsideIsNamedInTheEffectiveConfigBesideWhatIsInForce() {
    // #215: by Decide, Load and Refresh alike, and still after a FAILED, which leaves the
    // document in force; a setting nothing knows is told apart from one the host doesn't declare.
    SettingsProto.Host host = SettingsProto.Host.newBuilder().addLive("sampling_rate").build();
    String names = "{\"ignored\":[\"x\"],\"unsupported\":[\"send_logs\"]}";
    SettingsProto.Decision d = layer.decide(config("{\"sampling_rate\":\"0.5\",\"send_logs\":\"false\",\"x\":\"1\"}"),
        ByteString.EMPTY, ByteString.EMPTY, host, NOW, null);
    assertEquals(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, d.getStatus().getStatus());
    assertEquals("", d.getStatus().getErrorMessage());
    assertEquals("{\"sampling_rate\":\"0.5\"}", effective(d.getEffective()));
    assertEquals(names, setAside(d.getEffective()));
    assertEquals(names, setAside(layer.load(d.getCurrent().toByteArray(), host, NOW).getEffective()));
    assertEquals(names, setAside(layer.refresh(d.getCurrent(), ByteString.EMPTY, host, NOW).getEffective()));
    SettingsProto.Decision failed = layer.decide(config("{\"sampling_rate\":\"7\"}"), d.getCurrent(),
        ByteString.EMPTY, host, NOW, null);
    assertEquals(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_FAILED, failed.getStatus().getStatus());
    assertEquals(names, setAside(failed.getEffective()));
    // Nothing set aside: no entry.
    SettingsProto.Decision plain = layer.decide(config("{\"sampling_rate\":\"0.5\"}"), d.getCurrent(),
        ByteString.EMPTY, host, NOW, null);
    assertNull(setAside(plain.getEffective()));
  }

  @Test
  void debugModeArrivesFoldedInAndARefreshAtItsEndEndsIt() {
    String doc = "{\"sampling_rate\":\"0.1\",\"debug_until\":\"2026-10-14T12:30:00Z\",\"debug_send_logs_level\":\"info\"}";
    SettingsProto.Decision d = layer.decide(config(doc), ByteString.EMPTY, ByteString.EMPTY, DECLARING, NOW, KEEP);
    assertEquals(1.0, d.getApply().getSamplingRate());
    assertEquals(9, d.getApply().getSendLogsMinSeverity());
    long end = NOW + 30 * 60_000;
    assertEquals(end, d.getRefreshAtUnixMs());
    SettingsProto.Refreshed r = layer.refresh(d.getCurrent(), ByteString.EMPTY, DECLARING, end);
    assertEquals(0.1, r.getSettings().getSamplingRate());
    assertEquals(0, r.getRefreshAtUnixMs());
    assertEquals("{\"sampling_rate\":\"0.1\"}", effective(r.getEffective()));
    assertEquals(end, layer.load(d.getCurrent().toByteArray(), DECLARING, NOW).getRefreshAtUnixMs());
  }

  @Test
  void exportHealthIsSentAtTheFirstCallAndOnAFailureAndHeldWhileNothingChanges() {
    SettingsProto.HealthReport first = layer.health(SettingsProto.ExportCounts.getDefaultInstance(), null, NOW, null);
    assertTrue(first.getSend());
    assertEquals(3, first.getExporters().getComponentHealthMapCount());
    assertEquals("ok", first.getExporters().getStatus());
    assertFalse(layer.health(SettingsProto.ExportCounts.getDefaultInstance(), first, NOW + 30_000, null).getSend());
    SettingsProto.HealthReport failed = layer.health(SettingsProto.ExportCounts.newBuilder()
        .setTraces(SettingsProto.SignalCounts.newBuilder().setExportsFailed(1).setItemsFailed(512).setLastFailed(true)
            .setLastError("connection refused")).build(),
        first, NOW + 30_000, SettingsProto.Settings.newBuilder().setSendLogs(false).build());
    assertTrue(failed.getSend());
    assertFalse(failed.getExporters().getHealthy());
    assertEquals("connection refused", failed.getExporters().getComponentHealthMapMap().get("traces").getLastError());
    assertEquals("off", failed.getExporters().getComponentHealthMapMap().get("logs").getStatus());
  }

  static final ObjectMapper JSON = new ObjectMapper();

  /** testdata/components.json, each case through Components, compared as the vectors' about says. */
  @TestFactory
  Stream<DynamicTest> theComponentsVectors() throws IOException {
    Path file = testdata().resolve("components.json");
    List<DynamicTest> tests = new ArrayList<>();
    for (JsonNode c : JSON.readTree(file.toFile()).get("cases")) {
      tests.add(DynamicTest.dynamicTest(c.get("name").asText(), () -> {
        Opamp.AvailableComponents got = layer.components(inventory(c.path("inventory")), host(c.path("host")),
            c.path("now_unix_ms").asLong(0));
        assertTrue(got.getHash().isEmpty(), "the client computes the hash");
        assertEquals(List.of("exporters", "instrumentations", "sdk", "settings"),
            got.getComponentsMap().keySet().stream().sorted().toList());
        JsonNode json = components(got);
        c.get("want").fields().forEachRemaining(want -> assertEquals(want.getValue(), json.get(want.getKey()),
            want.getKey()));
      }));
    }
    return tests.stream();
  }

  /** The layer's test vectors, from the repository this client is in. */
  /**
   * testdata/start.json: the settings to start with come from the last config applied in the core's
   * saved state, else from the document kept before.
   */
  @TestFactory
  Stream<DynamicTest> theStartVectors() throws IOException {
    List<DynamicTest> tests = new ArrayList<>();
    for (JsonNode c : JSON.readTree(testdata().resolve("start.json").toFile()).get("cases")) {
      tests.add(DynamicTest.dynamicTest(c.get("name").asText(), () -> {
        byte[] coreState = c.has("core_state") ? HexFormat.of().parseHex(c.get("core_state").asText()) : null;
        byte[] saved = c.has("saved") ? c.get("saved").asText().getBytes(StandardCharsets.UTF_8) : null;
        SettingsProto.Loaded loaded = layer.loadFrom(coreState, saved, null, 0);
        String want = c.get("want").isNull() ? "{}" : c.get("want").asText();
        assertEquals(want, loaded.getCurrent().toStringUtf8());
      }));
    }
    return tests.stream();
  }

  static Path testdata() {
    for (Path d = Path.of("").toAbsolutePath(); d != null; d = d.getParent()) {
      Path t = d.resolve("testdata/sdk-settings");
      if (Files.isDirectory(t)) {
        return t;
      }
    }
    throw new IllegalStateException("no testdata/sdk-settings above " + Path.of("").toAbsolutePath());
  }

  /** A case's inventory: each field left out is empty. */
  static SettingsProto.Inventory inventory(JsonNode i) {
    SettingsProto.Inventory.Builder b = SettingsProto.Inventory.newBuilder()
        .setIntegration(i.path("integration").asText()).setIntegrationVersion(i.path("integration_version").asText())
        .setSdkLanguage(i.path("sdk_language").asText()).setSdkVersion(i.path("sdk_version").asText());
    i.path("instrumentations").forEach(x -> b.addInstrumentations(SettingsProto.Instrumentation.newBuilder()
        .setName(x.path("name").asText()).setVersion(x.path("version").asText())));
    i.path("exporters").forEach(x -> b.addExporters(SettingsProto.Exporter.newBuilder()
        .setSignal(x.path("signal").asText()).setName(x.path("name").asText()).setGated(x.path("gated").asBoolean())));
    return b.build();
  }

  /**
   * A case's host: its live and next_start lists, as Host declares them; left out, one that declares
   * every setting, each live one in live ({@link #DECLARING}'s and the application's own, {@code
   * custom}) and each next-start one in next_start.
   */
  static SettingsProto.Host host(JsonNode h) {
    if (!h.isObject()) {
      return DECLARING.toBuilder().addLive("custom").build();
    }
    SettingsProto.Host.Builder b = SettingsProto.Host.newBuilder();
    h.path("live").forEach(k -> b.addLive(k.asText()));
    h.path("next_start").forEach(k -> b.addNextStart(k.asText()));
    return b.build();
  }

  /**
   * The components as the vectors give them: sdk (key to text), instrumentations (name to version,
   * null when none), exporters (signal to name to gated) and settings (key to applies).
   */
  static JsonNode components(Opamp.AvailableComponents c) {
    ObjectNode out = JSON.createObjectNode();
    ObjectNode sdk = out.putObject("sdk");
    c.getComponentsMap().get("sdk").getMetadataList().forEach(kv -> sdk.put(kv.getKey(), text(kv)));
    ObjectNode instrumentations = out.putObject("instrumentations");
    c.getComponentsMap().get("instrumentations").getSubComponentMapMap().forEach((name, d) ->
        instrumentations.put(name, d.getMetadataList().stream().filter(kv -> kv.getKey().equals("version"))
            .map(SdkSettingsTest::text).findFirst().orElse(null)));
    ObjectNode exporters = out.putObject("exporters");
    c.getComponentsMap().get("exporters").getSubComponentMapMap().forEach((signal, d) -> {
      ObjectNode names = exporters.putObject(signal);
      d.getSubComponentMapMap().forEach((name, e) -> {
        assertEquals(1, e.getMetadataCount(), signal + "/" + name);
        Anyvalue.KeyValue gated = e.getMetadata(0);
        assertEquals("gated", gated.getKey());
        assertTrue(gated.getValue().hasBoolValue(), signal + "/" + name + ": gated is a boolean");
        names.put(name, gated.getValue().getBoolValue());
      });
    });
    ObjectNode settings = out.putObject("settings");
    c.getComponentsMap().get("settings").getSubComponentMapMap().forEach((key, d) ->
        settings.put(key, d.getMetadataList().stream().filter(kv -> kv.getKey().equals("applies"))
            .map(SdkSettingsTest::text).findFirst().orElse(null)));
    return out;
  }

  private static String text(Anyvalue.KeyValue kv) {
    assertTrue(kv.getValue().hasStringValue(), kv.getKey() + " is text");
    return kv.getValue().getStringValue();
  }
}
