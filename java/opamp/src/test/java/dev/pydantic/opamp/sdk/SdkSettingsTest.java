// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.sdk;

import static dev.pydantic.opamp.sdk.Policy.every;
import static dev.pydantic.opamp.sdk.Policy.exports;
import static dev.pydantic.opamp.sdk.Policy.override;
import static dev.pydantic.opamp.sdk.Policy.v1;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
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
    return Policy.config(body);
  }

  /** The policy the effective config reports: the test fails if it holds another file. */
  static String effective(Opamp.EffectiveConfig e) {
    var files = e.getConfigMap().getConfigMapMap();
    assertEquals(List.of(Policy.FILE), List.copyOf(files.keySet()));
    assertEquals("application/json", files.get(Policy.FILE).getContentType());
    return files.get(Policy.FILE).getBody().toStringUtf8();
  }

  static final Opamp.RemoteConfigStatuses APPLIED = Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED;
  static final Opamp.RemoteConfigStatuses FAILED = Opamp.RemoteConfigStatuses.RemoteConfigStatuses_FAILED;

  @Test
  void loadsDecidesAndDescribes() {
    String saved = v1("signal_exports", exports("", "", "off"));
    SettingsProto.Loaded l = layer.load(saved.getBytes(StandardCharsets.UTF_8), DECLARING, 0);
    assertTrue(l.getSettings().hasSendLogs());
    assertFalse(l.getSettings().getSendLogs());
    assertEquals(Policy.full(saved), effective(l.getEffective()));

    List<String> kept = new ArrayList<>();
    String quarter = v1("head_sampling", override("\"ratio\":0.25"));
    SettingsProto.Decision d = layer.decide(config(quarter), l.getCurrent(), l.getCurrent(),
        DECLARING, 0, doc -> kept.add(new String(doc, StandardCharsets.UTF_8)));
    assertEquals(APPLIED, d.getStatus().getStatus());
    assertEquals(ByteString.copyFromUtf8("h"), d.getStatus().getLastRemoteConfigHash());
    assertEquals(0.25, d.getApply().getSamplingRate());
    assertEquals(List.of(quarter), kept);
    assertEquals(Policy.full(quarter), effective(d.getEffective()));

    SettingsProto.Decision bad = layer.decide(config(v1("head_sampling", override("\"ratio\":\"x\""))), d.getCurrent(),
        l.getCurrent(), DECLARING, 0, doc -> kept.add("again"));
    assertEquals(FAILED, bad.getStatus().getStatus());
    assertTrue(bad.getStatus().getErrorMessage().startsWith("head_sampling:"));
    assertFalse(bad.hasApply());
    assertEquals(d.getCurrent(), bad.getCurrent());
    assertEquals(1, kept.size()); // refused: nothing to keep

    // The policy is the remote config's only file: another beside it, or none, is refused.
    Opamp.AgentConfigObject other = Opamp.AgentConfigObject.newBuilder().setBody(ByteString.copyFromUtf8("{}"))
        .setContentType("application/json").build();
    Opamp.AgentRemoteConfig beside = config(quarter).toBuilder()
        .setConfig(config(quarter).getConfig().toBuilder().putConfigMap("other.json", other)).build();
    Opamp.AgentRemoteConfig none = config(quarter).toBuilder()
        .setConfig(Opamp.AgentConfigMap.newBuilder().putConfigMap("other.json", other)).build();
    for (Opamp.AgentRemoteConfig c : List.of(beside, none)) {
      SettingsProto.Decision r = layer.decide(c, d.getCurrent(), l.getCurrent(), DECLARING, 0, KEEP);
      assertEquals(FAILED, r.getStatus().getStatus());
      assertTrue(r.getStatus().getErrorMessage().startsWith("config_map:"), r.getStatus().getErrorMessage());
    }
  }

  /** A save that keeps nothing, for a host that has somewhere to keep state. */
  static final SdkSettings.Saver KEEP = doc -> { };

  /**
   * Decide, then commit (#215): the integration saves what the layer asks it to keep, and the layer
   * answers from how that went, the same in every language.
   */
  @Test
  void theSaveTheLayerAsksForDecidesTheAnswer() {
    String half = v1("head_sampling", override("\"ratio\":0.5"));
    String quarter = v1("head_sampling", override("\"ratio\":0.25"));
    ByteString current = ByteString.copyFromUtf8(half);
    SettingsProto.Decision failed = layer.decide(config(quarter), current, current, DECLARING,
        0, doc -> {
          throw new java.io.IOException("No space left on device");
        });
    assertEquals(FAILED, failed.getStatus().getStatus());
    assertEquals("can't save the settings: No space left on device", failed.getStatus().getErrorMessage());
    assertFalse(failed.hasApply());
    assertEquals(current, failed.getCurrent());
    assertEquals(Policy.full(half), effective(failed.getEffective()));
    // Whatever stops the save refuses the config.
    SettingsProto.Decision odd = layer.decide(config(quarter), current, current, DECLARING, 0,
        doc -> {
          throw new IllegalStateException();
        });
    assertEquals("can't save the settings: java.lang.IllegalStateException", odd.getStatus().getErrorMessage());
    // Nowhere to keep state: a live change is applied, a next-start one refused.
    SettingsProto.Decision live = layer.decide(config(quarter), current, current, DECLARING, 0, null);
    assertEquals(APPLIED, live.getStatus().getStatus());
    String minute = every("polling_interval", override("\"milliseconds\":60000"));
    SettingsProto.Decision later = layer.decide(config(minute), current, current, DECLARING, 0, null);
    assertEquals("polling_interval: takes effect at the next start, which needs somewhere to keep state",
        later.getStatus().getErrorMessage());
    SettingsProto.Decision saved = layer.decide(config(minute), current, current, DECLARING, 0, KEEP);
    assertEquals(APPLIED, saved.getStatus().getStatus());
    assertEquals(List.of("opamp_polling_interval"), saved.getPendingRestartList());
  }

  /** A non-identifying attribute of the description: a string as itself, an array joined by commas. */
  static String attribute(Opamp.AgentDescription d, String key) {
    for (Anyvalue.KeyValue kv : d.getNonIdentifyingAttributesList()) {
      if (kv.getKey().equals(key)) {
        if (kv.getValue().hasArrayValue()) {
          return String.join(",", kv.getValue().getArrayValue().getValuesList().stream()
              .map(Anyvalue.AnyValue::getStringValue).toList());
        }
        return kv.getValue().getStringValue();
      }
    }
    return null;
  }

  /**
   * The description carries the resource, and tells a server the policy format the layer reads and
   * the controls the host carries out: debug only with a clock, and none for no host.
   */
  @Test
  void theDescriptionNamesTheFormatAndTheControls() {
    Opamp.AgentDescription desc = layer.describe(List.of(
        Anyvalue.KeyValue.newBuilder().setKey("service.name")
            .setValue(Anyvalue.AnyValue.newBuilder().setStringValue("checkout")).build(),
        Anyvalue.KeyValue.newBuilder().setKey("host.name")
            .setValue(Anyvalue.AnyValue.newBuilder().setStringValue("h")).build()), EVERY, NOW);
    assertEquals(1, desc.getIdentifyingAttributesCount());
    assertEquals("h", attribute(desc, "host.name"));
    String every = "head_sampling,signal_exports.traces,signal_exports.metrics,signal_exports.logs,sampling_rules,"
        + "trace_rate_limit,log_levels,log_rate_limit,attribute_redaction,debug,app_settings,instrumentations,"
        + "polling_interval";
    record Case(SettingsProto.Host host, long now, String controls) {}
    for (Case c : List.of(
        new Case(EVERY, NOW, every),
        new Case(EVERY, 0, every.replace(",debug", "")),
        new Case(DECLARING, NOW, every.replace(",app_settings", "")),
        new Case(SettingsProto.Host.newBuilder().addLive("sampling_rate").addLive("send_logs").build(), NOW,
            "head_sampling,signal_exports.logs"),
        new Case(null, NOW, ""))) {
      Opamp.AgentDescription d = layer.describe(List.of(), c.host(), c.now());
      assertEquals("logfire-sdk-policy/v1", attribute(d, "logfire.opamp.config_format"));
      assertEquals(c.controls(), attribute(d, "logfire.opamp.supported_controls"), String.valueOf(c));
    }
  }

  @Test
  void aLogLevelArrivesAsTheLowestSeverityNumberToExport() {
    String doc = every("log_levels", override("\"level\":\"warn\""));
    SettingsProto.Decision d = layer.decide(config(doc), ByteString.EMPTY, ByteString.EMPTY, DECLARING, 0, null);
    assertEquals(APPLIED, d.getStatus().getStatus());
    assertTrue(d.getApply().hasSendLogsMinSeverity());
    assertEquals(13, d.getApply().getSendLogsMinSeverity());
    assertEquals(Policy.sorted(doc), effective(d.getEffective()));
    // Local: every severity.
    assertFalse(layer.load(every().getBytes(StandardCharsets.UTF_8), DECLARING, 0).getSettings()
        .hasSendLogsMinSeverity());
  }

  /** A host that declares every setting: each live one at once, each next-start one at the next start. */
  static final SettingsProto.Host DECLARING = SettingsProto.Host.newBuilder().addAllLive(List.of(
      "debug_sampling_rate", "debug_send_logs_level", "debug_until", "drop_attributes", "mask_attributes",
      "sampling_rate", "sampling_rules", "sampling_traces_per_second", "send_logs", "send_logs_level",
      "send_logs_level_by_scope", "send_logs_per_second", "send_metrics", "send_traces"))
      .addAllNextStart(List.of("deactivate_all_instrumentations", "deactivate_instrumentations",
          "opamp_polling_interval")).build();
  /** {@link #DECLARING} with the application's own settings ({@code custom}) too. */
  static final SettingsProto.Host EVERY = DECLARING.toBuilder().addLive("custom").build();
  static final long NOW = 1_791_979_200_000L; // 2026-10-14T12:00:00Z

  @Test
  void theSettingsThatMatchTelemetryArriveTypedWhereTheHostDeclaresThem() {
    String doc = every("sampling_rules", override("\"rules\":[{\"rate\":0,\"kind\":\"server\",\"path\":\"/health*\"}]"),
        "log_levels", override("\"scopes\":{\"com.acme.db*\":\"debug\"}"),
        "attribute_redaction", override("\"mask\":[\"User.Email\"],\"drop\":[\"*token*\"]"),
        "trace_rate_limit", override("\"traces_per_second\":50"),
        "log_rate_limit", override("\"records_per_second\":0.5"));
    SettingsProto.Decision d = layer.decide(config(doc), ByteString.EMPTY, ByteString.EMPTY, DECLARING, NOW, null);
    assertEquals(APPLIED, d.getStatus().getStatus());
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
    // Undeclared: refused, and nothing is in force.
    SettingsProto.Decision plain = layer.decide(config(doc), ByteString.EMPTY, ByteString.EMPTY,
        SettingsProto.Host.newBuilder().addLive("sampling_rate").build(), NOW, null);
    assertEquals(FAILED, plain.getStatus().getStatus());
    assertFalse(plain.hasApply());
    assertTrue(plain.getStatus().getErrorMessage().endsWith(": this SDK cannot apply the requested override"),
        plain.getStatus().getErrorMessage());
  }

  @Test
  void anOverrideTheHostDoesNotCarryOutIsRefused() {
    SettingsProto.Host host = SettingsProto.Host.newBuilder().addLive("sampling_rate").build();
    String in = v1("head_sampling", override("\"ratio\":0.5"));
    SettingsProto.Decision d = layer.decide(config(in), ByteString.EMPTY, ByteString.EMPTY, host, NOW, null);
    assertEquals(APPLIED, d.getStatus().getStatus());
    // A refusal names the control, or the control and its field.
    SettingsProto.Decision v1 = layer.decide(config(v1("head_sampling", override("\"ratio\":0.5"),
        "signal_exports", exports("", "", "off"))), d.getCurrent(), ByteString.EMPTY, host, NOW, null);
    assertEquals(FAILED, v1.getStatus().getStatus());
    assertEquals("signal_exports.logs: this SDK cannot apply the requested override", v1.getStatus().getErrorMessage());
    assertFalse(v1.hasApply());
    assertEquals(d.getCurrent(), v1.getCurrent());
    assertEquals(Policy.full(in), effective(v1.getEffective()));
    SettingsProto.Decision field = layer.decide(config(every("instrumentations", override("\"disabled\":[\"otelhttp\"]"))),
        d.getCurrent(), ByteString.EMPTY, host, NOW, KEEP);
    assertEquals(FAILED, field.getStatus().getStatus());
    assertEquals("instrumentations: disabled: this SDK cannot apply the requested override",
        field.getStatus().getErrorMessage());
  }

  @Test
  void debugModeArrivesFoldedInAndARefreshAtItsEndEndsIt() {
    String doc = every("head_sampling", override("\"ratio\":0.1"),
        "debug", override("\"until\":\"2026-10-14T12:30:00Z\",\"log_level\":\"info\""));
    SettingsProto.Decision d = layer.decide(config(doc), ByteString.EMPTY, ByteString.EMPTY, DECLARING, NOW, KEEP);
    assertEquals(1.0, d.getApply().getSamplingRate());
    assertEquals(9, d.getApply().getSendLogsMinSeverity());
    long end = NOW + 30 * 60_000;
    assertEquals(end, d.getRefreshAtUnixMs());
    SettingsProto.Refreshed r = layer.refresh(d.getCurrent(), ByteString.EMPTY, DECLARING, end);
    assertEquals(0.1, r.getSettings().getSamplingRate());
    assertFalse(r.getSettings().hasSendLogsMinSeverity());
    assertEquals(0, r.getRefreshAtUnixMs());
    assertEquals(Policy.sorted(every("head_sampling", override("\"ratio\":0.1"))), effective(r.getEffective()));
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

  /**
   * testdata/start.json: the policy to start with comes from the last config applied in the core's
   * saved state, else from the policy kept before. A config applied without a policy starts from the
   * SDK's own settings, with a warning.
   */
  @TestFactory
  Stream<DynamicTest> theStartVectors() throws IOException {
    List<DynamicTest> tests = new ArrayList<>();
    for (JsonNode c : JSON.readTree(testdata().resolve("start.json").toFile()).get("cases")) {
      tests.add(DynamicTest.dynamicTest(c.get("name").asText(), () -> {
        byte[] coreState = c.has("core_state") ? HexFormat.of().parseHex(c.get("core_state").asText()) : null;
        byte[] saved = c.has("saved") ? c.get("saved").asText().getBytes(StandardCharsets.UTF_8) : null;
        SettingsProto.Loaded loaded = layer.loadFrom(coreState, saved, EVERY, 0);
        assertEquals(c.get("want").asText(), loaded.getCurrent().toStringUtf8());
        assertEquals(c.get("warns").asBoolean(), !loaded.getWarning().isEmpty(), loaded.getWarning());
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
      return EVERY;
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
