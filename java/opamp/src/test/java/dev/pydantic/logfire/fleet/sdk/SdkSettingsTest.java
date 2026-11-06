// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.logfire.fleet.sdk;

import static dev.pydantic.logfire.fleet.sdk.Policy.every;
import static dev.pydantic.logfire.fleet.sdk.Policy.exports;
import static dev.pydantic.logfire.fleet.sdk.Policy.override;
import static dev.pydantic.logfire.fleet.sdk.Policy.v1;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
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
import java.util.Map;
import java.util.TreeMap;
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

  /** The layer's agent flow, stepped by a test, on clocks that move together from {@link #NOW}. */
  static final class Flow {
    final SdkSettings layer = new SdkSettings();
    SettingsProto.AgentOutput out;

    Flow(SettingsProto.Host host) {
      this(host, true, null);
    }

    Flow(SettingsProto.Host host, boolean stateDir, byte[] coreState) {
      SettingsProto.AgentStart.Builder start = SettingsProto.AgentStart.newBuilder().setStateDir(stateDir)
          .setEndpoint("http://127.0.0.1:1").setInstanceUid(ByteString.copyFrom(new byte[16]))
          .setNowUnixMs(NOW).setNowMonoMs(NOW);
      if (host != null) {
        start.setHost(host);
      }
      if (coreState != null) {
        start.setCoreState(ByteString.copyFrom(coreState));
      }
      out = layer.start(start.build());
    }

    Flow step(long ms, SettingsProto.AgentStep.Builder input) {
      out = layer.step(input.setAgent(out.getAgent()).setNowUnixMs(NOW + ms).setNowMonoMs(NOW + ms).build());
      return this;
    }

    Flow decide(Opamp.AgentRemoteConfig c, long ms) {
      return step(ms, SettingsProto.AgentStep.newBuilder().setRemoteConfig(c));
    }

    Flow decide(Opamp.AgentRemoteConfig c) {
      return decide(c, 1);
    }

    private SettingsProto.AgentEffect last(SettingsProto.AgentEffect.EffectCase kind) {
      SettingsProto.AgentEffect found = null;
      for (SettingsProto.AgentEffect e : out.getEffectsList()) {
        if (e.getEffectCase() == kind) {
          found = e;
        }
      }
      return found;
    }

    Opamp.RemoteConfigStatus status() {
      SettingsProto.AgentEffect e = last(SettingsProto.AgentEffect.EffectCase.STATUS);
      return e == null ? Opamp.RemoteConfigStatus.getDefaultInstance() : e.getStatus();
    }

    /** The settings to put in force, or null for none. */
    SettingsProto.Settings apply() {
      SettingsProto.AgentEffect e = last(SettingsProto.AgentEffect.EffectCase.APPLY);
      return e == null ? null : e.getApply();
    }

    Opamp.ComponentHealth health() {
      SettingsProto.AgentEffect e = last(SettingsProto.AgentEffect.EffectCase.HEALTH);
      return e == null ? null : e.getHealth();
    }

    Opamp.EffectiveConfig effective() {
      return out.getEffective();
    }

    boolean warned() {
      return out.getEffectsList().stream().anyMatch(e -> e.hasLog()
          && e.getLog().getLevelValue() >= SettingsProto.AgentLog.Level.WARN_VALUE);
    }

    SettingsProto.AgentState state() {
      try {
        return SettingsProto.AgentState.parseFrom(out.getAgent());
      } catch (com.google.protobuf.InvalidProtocolBufferException e) {
        throw new AssertionError(e);
      }
    }
  }

  @Test
  void startsDecidesAndDescribes() {
    Flow f = new Flow(DECLARING);
    assertEquals(Policy.full(v1()), effective(f.effective()));
    String quarter = v1("head_sampling", override("\"ratio\":0.25"));
    f.decide(config(quarter));
    assertEquals(APPLIED, f.status().getStatus());
    assertEquals(ByteString.copyFromUtf8("h"), f.status().getLastRemoteConfigHash());
    assertEquals(0.25, f.apply().getSamplingRate());
    assertEquals(Policy.full(quarter), effective(f.effective()));

    f.decide(config(v1("head_sampling", override("\"ratio\":\"x\""))), 2);
    assertEquals(FAILED, f.status().getStatus());
    assertTrue(f.status().getErrorMessage().startsWith("head_sampling:"));
    assertNull(f.apply());
    assertEquals(Policy.full(quarter), effective(f.effective()));
    // A step without an input, or on a state that isn't one, is an error.
    assertThrows(IllegalArgumentException.class,
        () -> f.layer.step(SettingsProto.AgentStep.newBuilder().setAgent(f.out.getAgent()).build()));
    assertThrows(IllegalArgumentException.class, () -> f.layer.step(SettingsProto.AgentStep.newBuilder()
        .setAgent(ByteString.copyFrom(new byte[] {(byte) 0xff})).setTimer(SettingsProto.ExportCounts.getDefaultInstance())
        .build()));

    // The policy is the remote config's only file: another beside it, or none, is refused.
    Opamp.AgentConfigObject other = Opamp.AgentConfigObject.newBuilder().setBody(ByteString.copyFromUtf8("{}"))
        .setContentType("application/json").build();
    Opamp.AgentRemoteConfig beside = config(quarter).toBuilder()
        .setConfig(config(quarter).getConfig().toBuilder().putConfigMap("other.json", other)).build();
    Opamp.AgentRemoteConfig none = config(quarter).toBuilder()
        .setConfig(Opamp.AgentConfigMap.newBuilder().putConfigMap("other.json", other)).build();
    for (Opamp.AgentRemoteConfig c : List.of(beside, none)) {
      f.decide(c, 3);
      assertEquals(FAILED, f.status().getStatus());
      assertTrue(f.status().getErrorMessage().startsWith("config_map:"), f.status().getErrorMessage());
    }
  }

  @Test
  void aNextStartChangeNeedsSomewhereToKeepState() {
    String quarter = v1("head_sampling", override("\"ratio\":0.25"));
    // Nowhere to keep state: a live change is applied, a next-start one refused.
    assertEquals(APPLIED, new Flow(DECLARING, false, null).decide(config(quarter)).status().getStatus());
    String redis = every("instrumentations", override("\"disabled\":[\"redis\"]"));
    assertEquals("instrumentations: takes effect at the next start, which needs somewhere to keep state",
        new Flow(DECLARING, false, null).decide(config(redis)).status().getErrorMessage());
    assertEquals(APPLIED, new Flow(DECLARING).decide(config(redis)).status().getStatus());
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
        + "scrubbing,self_monitoring";
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
    Flow f = new Flow(DECLARING);
    assertFalse(f.apply().hasSendLogsMinSeverity()); // local: every severity
    String doc = every("log_levels", override("\"level\":\"warn\""));
    f.decide(config(doc));
    assertEquals(APPLIED, f.status().getStatus());
    assertTrue(f.apply().hasSendLogsMinSeverity());
    assertEquals(13, f.apply().getSendLogsMinSeverity());
    assertEquals(Policy.sorted(doc), effective(f.effective()));
  }

  /** A host that declares every setting: each live one at once, each next-start one at the next start. */
  static final SettingsProto.Host DECLARING = SettingsProto.Host.newBuilder().addAllLive(List.of(
      "debug_sampling_rate", "debug_send_logs_level", "debug_until", "drop_attributes", "mask_attributes",
      "sampling_rate", "sampling_rules", "sampling_traces_per_second", "scrub_defaults", "scrub_patterns",
      "send_logs", "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second", "send_metrics",
      "send_self_metrics", "send_traces"))
      .addAllNextStart(List.of("deactivate_all_instrumentations", "deactivate_instrumentations")).build();
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
    Flow d = new Flow(DECLARING).decide(config(doc));
    assertEquals(APPLIED, d.status().getStatus());
    SettingsProto.Settings s = d.apply();
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
    Flow plain = new Flow(SettingsProto.Host.newBuilder().addLive("sampling_rate").build()).decide(config(doc));
    assertEquals(FAILED, plain.status().getStatus());
    assertNull(plain.apply());
    assertTrue(plain.status().getErrorMessage().endsWith(": this SDK cannot apply the requested override"),
        plain.status().getErrorMessage());
  }

  @Test
  void anOverrideTheHostDoesNotCarryOutIsRefused() {
    SettingsProto.Host host = SettingsProto.Host.newBuilder().addLive("sampling_rate").build();
    String in = v1("head_sampling", override("\"ratio\":0.5"));
    Flow f = new Flow(host).decide(config(in));
    assertEquals(APPLIED, f.status().getStatus());
    // A refusal names the control, or the control and its field.
    f.decide(config(v1("head_sampling", override("\"ratio\":0.5"), "signal_exports", exports("", "", "off"))), 2);
    assertEquals(FAILED, f.status().getStatus());
    assertEquals("signal_exports.logs: this SDK cannot apply the requested override", f.status().getErrorMessage());
    assertNull(f.apply());
    assertEquals(Policy.full(in), effective(f.effective()));
    f.decide(config(every("instrumentations", override("\"disabled\":[\"otelhttp\"]"))), 3);
    assertEquals(FAILED, f.status().getStatus());
    assertEquals("instrumentations: disabled: this SDK cannot apply the requested override",
        f.status().getErrorMessage());
    // A host naming what is no setting has a bug: an error, saying which.
    IllegalArgumentException bad = assertThrows(IllegalArgumentException.class,
        () -> new Flow(SettingsProto.Host.newBuilder().addLive("send_metric").build()));
    assertTrue(bad.getMessage().contains("send_metric"), bad.getMessage());
  }

  @Test
  void debugModeArrivesFoldedInAndTheTimerAtItsEndEndsIt() {
    String doc = every("head_sampling", override("\"ratio\":0.1"),
        "debug", override("\"until\":\"2026-10-14T12:30:00Z\",\"log_level\":\"info\""));
    Flow f = new Flow(DECLARING).decide(config(doc), 0);
    assertEquals(1.0, f.apply().getSamplingRate());
    assertEquals(9, f.apply().getSendLogsMinSeverity());
    long end = 30 * 60_000;
    assertEquals(NOW + end, f.state().getRefreshAtUnixMs());
    assertTrue(f.out.getWakeAtMonoMs() > 0 && f.out.getWakeAtMonoMs() <= NOW + end);
    f.step(end, SettingsProto.AgentStep.newBuilder().setTimer(SettingsProto.ExportCounts.getDefaultInstance()));
    assertEquals(0.1, f.apply().getSamplingRate());
    assertFalse(f.apply().hasSendLogsMinSeverity());
    assertEquals(0, f.state().getRefreshAtUnixMs());
    assertEquals(Policy.sorted(every("head_sampling", override("\"ratio\":0.1"))), effective(f.effective()));
  }

  @Test
  void exportHealthIsSentWhenBuiltAndOnAFailureAndHeldWhileNothingChanges() {
    Flow f = new Flow(DECLARING);
    f.step(0, SettingsProto.AgentStep.newBuilder().setBuilt(SettingsProto.AgentBuilt.newBuilder().setService("checkout")));
    f.step(0, exporters(SettingsProto.ExportCounts.getDefaultInstance()));
    Opamp.ComponentHealth first = f.health().getComponentHealthMapMap().get("exporters");
    assertEquals(3, first.getComponentHealthMapCount());
    assertEquals("ok", first.getStatus());
    assertNull(f.step(30_000, exporters(SettingsProto.ExportCounts.getDefaultInstance())).health());
    f.step(60_000, SettingsProto.AgentStep.newBuilder().setExporters(SettingsProto.AgentExporters.newBuilder()
        .setCounts(SettingsProto.ExportCounts.newBuilder()
            .setTraces(SettingsProto.SignalCounts.newBuilder().setExportsFailed(1).setItemsFailed(512)
                .setLastFailed(true).setLastError("connection refused")))
        .addExporters(SettingsProto.Exporter.newBuilder().setSignal("traces").setName("zipkin"))));
    Opamp.ComponentHealth failed = f.health().getComponentHealthMapMap().get("exporters");
    // Each exporter is a child of its signal: one the gates don't reach is ungoverned, healthy all the same.
    Opamp.ComponentHealth zipkin = failed.getComponentHealthMapMap().get("traces")
        .getComponentHealthMapMap().get("zipkin");
    assertEquals("ungoverned", zipkin.getStatus());
    assertTrue(zipkin.getHealthy());
    assertFalse(failed.getHealthy());
    assertEquals("connection refused", failed.getComponentHealthMapMap().get("traces").getLastError());
  }

  private static SettingsProto.AgentStep.Builder exporters(SettingsProto.ExportCounts counts) {
    return SettingsProto.AgentStep.newBuilder().setExporters(SettingsProto.AgentExporters.newBuilder().setCounts(counts));
  }

  static final ObjectMapper JSON = new ObjectMapper();

  /** testdata/exporters.json, each case through Exporters: what is decided, compared whole. */
  @TestFactory
  Stream<DynamicTest> theExportersVectors() throws IOException {
    List<DynamicTest> tests = new ArrayList<>();
    for (JsonNode c : JSON.readTree(testdata().resolve("exporters.json").toFile()).get("cases")) {
      tests.add(DynamicTest.dynamicTest(c.get("name").asText(), () -> {
        List<SettingsProto.FoundExporter> found = new ArrayList<>();
        for (JsonNode f : c.get("found")) {
          found.add(SettingsProto.FoundExporter.newBuilder().setSignal(f.path("signal").asText())
              .setKind(f.path("kind").asText()).setGated(f.path("gated").asBoolean())
              .setEndpoint(f.path("endpoint").asText()).setFromEnv(f.path("from_env").asBoolean()).build());
        }
        Map<String, String> env = new TreeMap<>();
        c.path("env").fields().forEachRemaining(v -> env.put(v.getKey(), v.getValue().asText()));
        SettingsProto.ExportersDecided got = layer.exporters(found, env, c.path("default_protocol").asText());
        var out = JSON.createObjectNode();
        var exporters = out.putArray("exporters");
        got.getExportersList().forEach(e -> exporters.addObject().put("signal", e.getSignal()).put("name", e.getName())
            .put("gated", e.getGated()).put("destination", e.getDestination()));
        var governed = out.putArray("governed");
        got.getGovernedList().forEach(governed::add);
        var ungoverned = out.putArray("ungoverned");
        got.getUngovernedList().forEach(ungoverned::add);
        var warnings = out.putArray("warnings");
        got.getWarningsList().forEach(warnings::add);
        assertEquals(c.get("want"), out);
      }));
    }
    return tests.stream();
  }

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
   * saved state, else the SDK's own. A config applied without a policy starts from the
   * SDK's own settings, with a warning.
   */
  @TestFactory
  Stream<DynamicTest> theStartVectors() throws IOException {
    List<DynamicTest> tests = new ArrayList<>();
    for (JsonNode c : JSON.readTree(testdata().resolve("start.json").toFile()).get("cases")) {
      tests.add(DynamicTest.dynamicTest(c.get("name").asText(), () -> {
        byte[] coreState = c.has("core_state") ? HexFormat.of().parseHex(c.get("core_state").asText()) : null;
        Flow f = new Flow(EVERY, true, coreState);
        assertEquals(c.get("want").asText(), f.state().getCurrent().toStringUtf8());
        assertEquals(c.get("warns").asBoolean(), f.warned());
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
        .setSignal(x.path("signal").asText()).setName(x.path("name").asText()).setGated(x.path("gated").asBoolean())
        .setDestination(x.path("destination").asText())));
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
   * null when none), exporters (signal to identity to gated), destinations (signal to identity to
   * destination, for those with one) and settings (key to applies).
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
    ObjectNode destinations = out.putObject("destinations");
    c.getComponentsMap().get("exporters").getSubComponentMapMap().forEach((signal, d) -> {
      ObjectNode names = exporters.putObject(signal);
      d.getSubComponentMapMap().forEach((name, e) -> {
        Anyvalue.KeyValue gated = e.getMetadata(0);
        assertEquals("gated", gated.getKey());
        assertTrue(gated.getValue().hasBoolValue(), signal + "/" + name + ": gated is a boolean");
        names.put(name, gated.getValue().getBoolValue());
        // Then where it sends, for one that says: nothing else.
        e.getMetadataList().stream().skip(1).forEach(kv -> {
          assertEquals("destination", kv.getKey(), signal + "/" + name);
          ObjectNode sent = destinations.has(signal) ? (ObjectNode) destinations.get(signal)
              : destinations.putObject(signal);
          sent.put(name, text(kv));
        });
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
