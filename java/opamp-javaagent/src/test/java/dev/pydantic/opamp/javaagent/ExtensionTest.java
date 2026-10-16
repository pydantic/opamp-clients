// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.Notification;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.sdk.autoconfigure.spi.internal.DefaultConfigProperties;
import io.opentelemetry.sdk.resources.Resource;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.logging.Handler;
import java.util.logging.Level;
import java.util.logging.LogRecord;
import java.util.logging.Logger;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

class ExtensionTest {
  private static final Opamp.RemoteConfigStatuses APPLIED = Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED;
  private static final Opamp.RemoteConfigStatuses FAILED = Opamp.RemoteConfigStatuses.RemoteConfigStatuses_FAILED;

  static Opamp.AgentRemoteConfig config(String key, String body) {
    Opamp.AgentConfigMap.Builder map = Opamp.AgentConfigMap.newBuilder();
    if (key != null) {
      map.putConfigMap(key, Opamp.AgentConfigObject.newBuilder()
          .setBody(ByteString.copyFromUtf8(body)).setContentType("application/json").build());
    }
    return Opamp.AgentRemoteConfig.newBuilder().setConfig(map)
        .setConfigHash(ByteString.copyFromUtf8(key + body)).build();
  }

  static String effective(Extension e) {
    return e.effective().getConfigMap().getConfigMapMap().get("elastic").getBody().toString(StandardCharsets.UTF_8);
  }

  /** What the effective config names as set aside (its elastic.set_aside entry): "null" without one. */
  static String setAside(Extension e) {
    var f = e.effective().getConfigMap().getConfigMapMap().get("elastic.set_aside");
    return f == null ? "null" : f.getBody().toString(StandardCharsets.UTF_8);
  }

  static Extension.Setup setup(Path dir) {
    return new Extension.Setup("http://server/v1/opamp", Map.of(), dir, Duration.ZERO);
  }

  @Test
  void liveSettingsAreAppliedAtOnceAndAnsweredWithTheConfigsHash(@TempDir Path dir) throws Exception {
    Extension e = Extension.forTest(setup(dir));
    Opamp.AgentRemoteConfig c = config("elastic", "{\"sampling_rate\":\"0.5\",\"send_logs\":\"false\"}");
    Opamp.RemoteConfigStatus s = e.remoteConfig(c);
    assertEquals(APPLIED, s.getStatus());
    assertEquals(c.getConfigHash(), s.getLastRemoteConfigHash());
    assertFalse(e.controls.gates.sendLogs());
    assertEquals("{\"sampling_rate\":\"0.5\",\"send_logs\":\"false\"}", effective(e));
    // The client keeps the config applied, in its own state: nothing of the agent's own.
    assertFalse(Files.exists(dir.resolve("sdk-settings.json")));
  }

  @Test
  void aLogLevelIsAppliedAtOnceAndReportedByItsLowercaseName(@TempDir Path dir) {
    Extension e = Extension.forTest(setup(dir));
    assertEquals(APPLIED, e.remoteConfig(config("elastic", "{\"send_logs_level\":\"WARNING\"}")).getStatus());
    assertEquals(13, e.controls.gates.logLevels().min());
    assertEquals("{\"send_logs_level\":\"warn\"}", effective(e));
    e.remoteConfig(config("elastic", "{}"));
    assertEquals(0, e.controls.gates.logLevels().min());
  }

  @Test
  void levelsPerScopeAreDeclaredAppliedAtOnceAndReportedSorted(@TempDir Path dir) {
    assertTrue(Extension.HOST.getLiveList().contains("send_logs_level_by_scope"));
    Extension e = Extension.forTest(setup(dir));
    assertEquals(APPLIED, e.remoteConfig(config("elastic",
        "{\"send_logs_level_by_scope\":\"noisy.lib=OFF, com.acme.db*=debug, com.acme.db=Warning\"}")).getStatus());
    // As the layer sorts them for matching: the longest name first, an exact name before its prefix.
    assertEquals(List.of("com.acme.db", "com.acme.db*", "noisy.lib"),
        e.controls.gates.logLevels().byScope().stream().map(l -> l.getPattern()).toList());
    assertEquals("{\"send_logs_level_by_scope\":\"com.acme.db=warn,com.acme.db*=debug,noisy.lib=off\"}", effective(e));
    // A level that isn't one: FAILED, naming the setting, and the levels stay.
    Opamp.RemoteConfigStatus bad = e.remoteConfig(config("elastic", "{\"send_logs_level_by_scope\":\"a=loud\"}"));
    assertEquals(FAILED, bad.getStatus());
    assertTrue(bad.getErrorMessage().startsWith("send_logs_level_by_scope:"), bad.getErrorMessage());
    assertEquals(3, e.controls.gates.logLevels().byScope().size());
    // Left out: none.
    e.remoteConfig(config("elastic", "{}"));
    assertFalse(e.controls.gates.logLevels().any());
  }

  @Test
  void theLogCapIsDeclaredAppliedAtOnceAndZeroIsRefused(@TempDir Path dir) {
    assertTrue(Extension.HOST.getLiveList().contains("send_logs_per_second"));
    Extension e = Extension.forTest(setup(dir));
    assertEquals(APPLIED, e.remoteConfig(config("elastic", "{\"send_logs_per_second\":\"0.5\"}")).getStatus());
    assertEquals(0.5, e.controls.gates.logCap().perSecond);
    assertEquals("{\"send_logs_per_second\":\"0.5\"}", effective(e));
    Opamp.RemoteConfigStatus zero = e.remoteConfig(config("elastic", "{\"send_logs_per_second\":\"0\"}"));
    assertEquals(FAILED, zero.getStatus());
    assertTrue(zero.getErrorMessage().contains("send_logs false"), zero.getErrorMessage());
    assertEquals(0.5, e.controls.gates.logCap().perSecond);
    e.remoteConfig(config("elastic", "{}"));
    assertEquals(null, e.controls.gates.logCap());
  }

  @Test
  void redactionIsDeclaredAppliedAtOnceAndReportedLowercaseAndSorted(@TempDir Path dir) {
    assertTrue(Extension.HOST.getLiveList().containsAll(List.of("drop_attributes", "mask_attributes")));
    Extension e = Extension.forTest(setup(dir));
    assertEquals(APPLIED, e.remoteConfig(config("elastic",
        "{\"drop_attributes\":\"Cookie, Authorization, cookie\",\"mask_attributes\":\"*Token*\"}")).getStatus());
    assertTrue(e.controls.gates.redaction().any());
    assertEquals("{\"drop_attributes\":\"authorization,cookie\",\"mask_attributes\":\"*token*\"}", effective(e));
    e.remoteConfig(config("elastic", "{\"drop_attributes\":\" , \"}"));
    assertFalse(e.controls.gates.redaction().any());
  }

  @Test
  void samplingRulesAndTheTraceCapAreDeclaredAppliedAtOnceAndReportedCanonically(@TempDir Path dir) {
    assertTrue(Extension.HOST.getLiveList().containsAll(List.of("sampling_rules", "sampling_traces_per_second")));
    Extension e = Extension.forTest(setup(dir));
    // Sent in a string, as a server that only sends strings does.
    assertEquals(APPLIED, e.remoteConfig(config("elastic", "{\"sampling_rules\":\"[{\\\"path\\\": \\\"/health*\\\", "
        + "\\\"rate\\\": 0.0, \\\"kind\\\": \\\"SERVER\\\"}]\",\"sampling_traces_per_second\":\"50\"}")).getStatus());
    assertEquals(50.0, e.controls.sampler.cap().perSecond);
    assertEquals("{\"sampling_rules\":\"[{\\\"kind\\\":\\\"server\\\",\\\"path\\\":\\\"/health*\\\",\\\"rate\\\":0}]\","
        + "\"sampling_traces_per_second\":\"50\"}", effective(e));
    // route isn't a matcher: FAILED, naming the rule, and nothing changes.
    Opamp.RemoteConfigStatus route = e.remoteConfig(config("elastic",
        "{\"sampling_rules\":[{\"rate\":1},{\"rate\":0,\"route\":\"/users/:id\"}]}"));
    assertEquals(FAILED, route.getStatus());
    assertTrue(route.getErrorMessage().startsWith("sampling_rules[1].route:"), route.getErrorMessage());
    assertEquals(50.0, e.controls.sampler.cap().perSecond);
    e.remoteConfig(config("elastic", "{}"));
    assertEquals(null, e.controls.sampler.cap());
  }

  /**
   * debug.json and saving.json, through the extension: each decide a remote config (kept applied
   * as the client keeps it, where there is a state directory), each load a restart (what the client
   * kept read back from the state directory), each refresh what the timer does, at the step's clock.
   * saving.json's cases whose save fails are the layer's: here the client keeps the config applied,
   * and its save failing holds APPLIED back rather than refusing the config (the client's tests).
   */
  @org.junit.jupiter.api.TestFactory
  java.util.stream.Stream<org.junit.jupiter.api.DynamicTest> debugModeAndSavingOverTime(@TempDir Path root) {
    var cases = new java.util.ArrayList<>(Vectors.cases("debug"));
    for (var c : Vectors.cases("saving")) {
      boolean fails = false;
      for (var step : c.get("steps")) {
        fails |= "failed".equals(step.path("save").asText());
      }
      if (!fails) {
        cases.add(c);
      }
    }
    return cases.stream().map(c -> org.junit.jupiter.api.DynamicTest.dynamicTest(
        c.get("name").asText(), () -> {
          Path dir = Files.createTempDirectory(root, "state");
          long[] now = {0};
          Extension e = null;
          boolean canSave = true;
          for (var step : c.get("steps")) {
            canSave &= !"nowhere".equals(step.path("save").asText());
          }
          for (var step : c.get("steps")) {
            now[0] = step.get("now_unix_ms").asLong();
            var want = step.get("want");
            switch (step.get("call").asText()) {
              case "decide" -> {
                if (e == null) {
                  e = Extension.forTest(setup(canSave ? dir : null));
                  e.wallClock = () -> now[0];
                }
                Opamp.RemoteConfigStatus st = e.remoteConfig(config("elastic", step.get("document").asText()));
                if (st.getStatus() == APPLIED && canSave) {
                  Kept.keepApplied(dir, step.get("document").asText()); // as the client does, before APPLIED goes out
                }
                assertEquals(want.get("status").asText(), st.getStatus() == APPLIED ? "applied" : "failed");
                assertTrue(st.getErrorMessage().startsWith(want.path("error").asText()), st.getErrorMessage());
              }
              case "load" -> {
                e = Extension.forTest(null);
                e.wallClock = () -> now[0];
                e.properties(DefaultConfigProperties.createFromMap(Map.of("otel.opamp.endpoint",
                    "http://server/v1/opamp", "otel.opamp.state.dir", canSave ? dir.toString() : "none")));
              }
              case "refresh" -> e.refresh(now[0]);
              default -> throw new IllegalArgumentException(step.toString());
            }
            if (want.has("sampling_rate")) {
              var rate = want.get("sampling_rate");
              assertEquals(rate.isNull() ? null : rate.asDouble(),
                  e.inForce().hasSamplingRate() ? e.inForce().getSamplingRate() : null, step.toString());
            }
            if (want.has("refresh_at_unix_ms")) {
              assertEquals(want.get("refresh_at_unix_ms").asLong(), e.armedForTest()[1], step.toString());
            }
            if (want.has("effective")) {
              assertEquals(want.get("effective"), Vectors.JSON.readTree(effective(e)), step.toString());
            }
            if (want.has("set_aside")) {
              assertEquals(want.get("set_aside"), Vectors.JSON.readTree(setAside(e)), step.toString());
            }
          }
          e.refresh(0); // disarm
        }));
  }

  @Test
  void debugModeFoldsInTheDebugRateAndLevelAndKeepsTheCapsAndRedaction(@TempDir Path dir) {
    assertTrue(Extension.HOST.getLiveList().containsAll(List.of("debug_until", "debug_sampling_rate",
        "debug_send_logs_level")));
    Extension e = Extension.forTest(setup(dir));
    e.wallClock = () -> Vectors.NOW;
    assertEquals(APPLIED, e.remoteConfig(config("elastic", "{\"sampling_rate\":\"0.01\",\"sampling_rules\":"
        + "[{\"rate\":0,\"name\":\"GET\"}],\"send_logs_level\":\"error\",\"send_logs_level_by_scope\":\"noisy=off\","
        + "\"send_logs_per_second\":\"100\",\"sampling_traces_per_second\":\"10\",\"mask_attributes\":\"user.*\","
        + "\"debug_until\":\"2026-10-14T12:30:00Z\",\"debug_sampling_rate\":\"0.5\"}")).getStatus());
    // The debug rate in place of the rate and the rules, the debug level in place of the levels.
    var root = io.opentelemetry.api.trace.SpanContext.getInvalid();
    var d = e.controls.sampler.decide(root, "GET", io.opentelemetry.api.trace.SpanKind.SERVER,
        io.opentelemetry.api.common.Attributes.empty());
    assertEquals(LiveSampler.How.RATE, d.how());
    assertEquals(0.5, d.rate());
    assertEquals(new Controls.LogLevels(5, List.of()), e.controls.gates.logLevels());
    // The caps and redaction stay.
    assertEquals(10.0, e.controls.sampler.cap().perSecond);
    assertEquals(100.0, e.controls.gates.logCap().perSecond);
    assertTrue(e.controls.gates.redaction().any());
    assertEquals(Vectors.NOW + 30 * 60_000, e.armedForTest()[1]);

    // The timer ends it: the rules and levels again, reported without debug_until.
    long[] armed = e.armedForTest();
    e.fire(armed[0], armed[1]);
    d = e.controls.sampler.decide(root, "GET", io.opentelemetry.api.trace.SpanKind.SERVER,
        io.opentelemetry.api.common.Attributes.empty());
    assertEquals(0.0, d.rate());
    assertEquals(0, d.rule());
    assertEquals(17, e.controls.gates.logLevels().min());
    assertEquals(0, e.armedForTest()[1]);
    assertFalse(effective(e).contains("debug_until"), effective(e));
  }

  @Test
  void aTimerArmedBeforeANewerDecisionDoesNothing(@TempDir Path dir) {
    Extension e = Extension.forTest(setup(dir));
    long[] now = {Vectors.NOW};
    e.wallClock = () -> now[0];
    e.remoteConfig(config("elastic", "{\"debug_until\":\"2026-10-14T12:10:00Z\"}"));
    long[] stale = e.armedForTest();
    // A newer window, sent before the first ends.
    e.remoteConfig(config("elastic", "{\"debug_until\":\"2026-10-14T12:20:00Z\",\"sampling_rate\":\"0.1\"}"));
    now[0] = Vectors.NOW + 10 * 60_000;
    e.fire(stale[0], stale[1]);
    assertEquals(1.0, e.inForce().getSamplingRate());
    assertTrue(effective(e).contains("debug_until"));
    // Its own timer ends it, even if the wall clock went back meanwhile: the time waited counts.
    now[0] = Vectors.NOW;
    long[] armed = e.armedForTest();
    e.fire(armed[0], armed[1]);
    assertEquals(0.1, e.inForce().getSamplingRate());
    assertEquals("{\"sampling_rate\":\"0.1\"}", effective(e));
  }

  /**
   * A FAILED config after the wall clock passed debug_until, before the timer went off (a machine
   * that slept: the timer's clock doesn't count it), ends debug mode, as the effective config it
   * reports says.
   */
  @Test
  void aFailedConfigAfterTheWallClockPassedDebugUntilEndsDebugMode(@TempDir Path dir) {
    Extension e = Extension.forTest(setup(dir));
    long[] now = {Vectors.NOW};
    e.wallClock = () -> now[0];
    e.remoteConfig(config("elastic", "{\"sampling_rate\":\"0.1\",\"debug_until\":\"2026-10-14T12:10:00Z\"}"));
    assertEquals(1.0, e.inForce().getSamplingRate());
    now[0] = Vectors.NOW + 25 * 60_000;
    assertEquals(FAILED, e.remoteConfig(config("elastic", "{\"sampling_rate\":\"7\"}")).getStatus());
    assertEquals("{\"sampling_rate\":\"0.1\"}", effective(e));
    assertEquals(0.1, e.inForce().getSamplingRate());
    assertEquals(0, e.armedForTest()[1]);
  }

  @Test
  void aWindowMoreThanADayAheadIsRefused(@TempDir Path dir) {
    Extension e = Extension.forTest(setup(dir));
    e.wallClock = () -> Vectors.NOW;
    Opamp.RemoteConfigStatus s = e.remoteConfig(config("elastic", "{\"debug_until\":\"2026-10-17T00:00:00Z\"}"));
    assertEquals(FAILED, s.getStatus());
    assertTrue(s.getErrorMessage().contains("24h"), s.getErrorMessage());
    assertEquals(0, e.armedForTest()[1]);
  }

  @Test
  void debugModeEndsOnItsOwnOnTheRealClock(@TempDir Path dir) throws Exception {
    Extension e = Extension.forTest(setup(dir));
    String until = java.time.Instant.ofEpochMilli(System.currentTimeMillis() + 1500).toString();
    assertEquals(APPLIED, e.remoteConfig(config("elastic",
        "{\"sampling_rate\":\"0.2\",\"debug_until\":\"" + until + "\"}")).getStatus());
    assertEquals(1.0, e.inForce().getSamplingRate());
    long deadline = System.nanoTime() + 10_000_000_000L;
    while (e.inForce().getSamplingRate() != 0.2 && System.nanoTime() < deadline) {
      Thread.sleep(50);
    }
    assertEquals(0.2, e.inForce().getSamplingRate());
    assertEquals("{\"sampling_rate\":\"0.2\"}", effective(e));
  }

  /** health.json, through the extension's health step: what it sends, and the agent health it sends it in. */
  @org.junit.jupiter.api.TestFactory
  java.util.stream.Stream<org.junit.jupiter.api.DynamicTest> exportHealthOverTime() {
    return Vectors.cases("health").stream().map(c -> org.junit.jupiter.api.DynamicTest.dynamicTest(
        c.get("name").asText(), () -> {
          Extension e = Extension.forTest(setup(null));
          for (var step : c.get("steps")) {
            var counts = dev.pydantic.opamp.sdk.SettingsProto.ExportCounts.newBuilder();
            var given = step.has("counts") ? step.get("counts") : Vectors.JSON.createObjectNode();
            for (String signal : List.of("traces", "metrics", "logs")) {
              var n = given.has(signal) ? given.get(signal) : Vectors.JSON.createObjectNode();
              var sc = dev.pydantic.opamp.sdk.SettingsProto.SignalCounts.newBuilder()
                  .setExportsOk(n.path("exports_ok").asLong()).setExportsFailed(n.path("exports_failed").asLong())
                  .setItemsFailed(n.path("items_failed").asLong()).setItemsFiltered(n.path("items_filtered").asLong())
                  .setLastFailed(n.path("last_failed").asBoolean()).setLastError(n.path("last_error").asText()).build();
              switch (signal) {
                case "traces" -> counts.setTraces(sc);
                case "metrics" -> counts.setMetrics(sc);
                default -> counts.setLogs(sc);
              }
            }
            var settings = Vectors.settings(step.has("settings") ? step.get("settings").toString() : "{}", Vectors.NOW);
            Opamp.ComponentHealth before = e.health();
            Opamp.ComponentHealth sent = e.health(counts.build(), settings, step.get("now_unix_ms").asLong());
            var want = step.get("want");
            if (want.has("send")) {
              assertEquals(want.get("send").asBoolean(), sent != null, step.toString());
            }
            // Sent or not, the health in force: the top level the application's, the exporters under it.
            Opamp.ComponentHealth h = sent != null ? sent : e.health();
            if (sent == null) {
              assertEquals(before, h);
            }
            assertTrue(h.getHealthy());
            Opamp.ComponentHealth x = h.getComponentHealthMapMap().get("exporters");
            if (want.has("healthy")) {
              assertEquals(want.get("healthy").asBoolean(), x.getHealthy(), step.toString());
            }
            if (want.has("status")) {
              assertEquals(want.get("status").asText(), x.getStatus(), step.toString());
            }
            if (want.has("last_error")) {
              assertEquals(want.get("last_error").asText(), x.getLastError(), step.toString());
            }
            if (want.has("signals")) {
              want.get("signals").fields().forEachRemaining(sig -> {
                Opamp.ComponentHealth one = x.getComponentHealthMapMap().get(sig.getKey());
                var w = sig.getValue();
                if (w.has("healthy")) {
                  assertEquals(w.get("healthy").asBoolean(), one.getHealthy(), step.toString());
                }
                if (w.has("status")) {
                  assertEquals(w.get("status").asText(), one.getStatus(), step.toString());
                }
                if (w.has("last_error")) {
                  assertEquals(w.get("last_error").asText(), one.getLastError(), step.toString());
                }
                if (w.has("counters") && sent != null) {
                  w.get("counters").fields().forEachRemaining(kv -> assertEquals(kv.getValue().asLong(),
                      one.getAttributesList().stream().filter(a -> a.getKey().equals(kv.getKey())).findFirst()
                          .orElseThrow().getValue().getIntValue(), step.toString()));
                }
              });
            }
          }
        }));
  }

  @Test
  void aFailingExporterShowsInTheHealthAtOnceAndClearsOnTheNextSuccess(@TempDir Path dir) {
    Extension e = Extension.forTest(setup(dir));
    long[] now = {Vectors.NOW};
    e.wallClock = () -> now[0];
    ControlsTest.Scripted exporter = new ControlsTest.Scripted();
    var gate = e.controls.spans(exporter);
    var span = io.opentelemetry.sdk.testing.trace.TestSpanData.builder().setName("s")
        .setKind(io.opentelemetry.api.trace.SpanKind.INTERNAL).setStartEpochNanos(1).setEndEpochNanos(2)
        .setHasEnded(true).setStatus(io.opentelemetry.sdk.trace.data.StatusData.unset()).build();
    java.util.function.Supplier<Opamp.ComponentHealth> tick = () -> {
      now[0] += Extension.HEALTH_EVERY.toMillis();
      return e.health(e.controls.counts(), e.inForce(), now[0]);
    };
    assertEquals("ok", tick.get().getComponentHealthMapMap().get("exporters").getStatus());
    gate.export(List.of(span));
    assertEquals(null, tick.get()); // only a counter moved, 30 s after the last send
    exporter.next = io.opentelemetry.sdk.common.CompletableResultCode.ofExceptionalFailure(
        new java.net.ConnectException("connection refused"));
    gate.export(List.of(span, span));
    Opamp.ComponentHealth failing = tick.get();
    assertTrue(failing.getHealthy()); // the application's
    Opamp.ComponentHealth exporters = failing.getComponentHealthMapMap().get("exporters");
    assertFalse(exporters.getHealthy());
    assertEquals("traces: connection refused", exporters.getLastError());
    exporter.next = io.opentelemetry.sdk.common.CompletableResultCode.ofSuccess();
    gate.export(List.of(span));
    assertEquals("ok", tick.get().getComponentHealthMapMap().get("exporters").getStatus());
    // send_traces false: off, not failing.
    e.remoteConfig(config("elastic", "{\"send_traces\":\"false\"}"));
    assertEquals("off", tick.get().getComponentHealthMapMap().get("exporters").getComponentHealthMapMap()
        .get("traces").getStatus());
  }

  @Test
  void theAgentDeclaresEverySettingItCarriesOut() {
    assertEquals(new java.util.TreeSet<>(Vectors.ALL.getLiveList()), new java.util.TreeSet<>(Extension.HOST.getLiveList()));
    assertEquals(new java.util.TreeSet<>(Vectors.ALL.getNextStartList()),
        new java.util.TreeSet<>(Extension.HOST.getNextStartList()));
    // Where a signal's exporters pass no gate of the agent's, what the gate holds isn't declared.
    dev.pydantic.opamp.sdk.SettingsProto.Host ungated = Extension.host(java.util.Set.of("metrics", "logs"));
    assertFalse(ungated.getLiveList().stream().anyMatch(k -> k.startsWith("send_logs") || k.equals("send_metrics")
        || k.endsWith("_attributes") || k.equals("debug_send_logs_level")), ungated.toString());
    assertTrue(ungated.getLiveList().containsAll(List.of("send_traces", "sampling_rate", "debug_until")));
  }

  @Test
  void aWrongConfigIsFailedAndChangesNothing(@TempDir Path dir) {
    Extension e = Extension.forTest(setup(dir));
    e.remoteConfig(config("elastic", "{\"send_traces\":\"false\"}"));
    Opamp.RemoteConfigStatus s = e.remoteConfig(config("elastic", "{\"send_traces\":\"true\",\"sampling_rate\":\"2\"}"));
    assertEquals(FAILED, s.getStatus());
    assertTrue(s.getErrorMessage().startsWith("sampling_rate:"), s.getErrorMessage());
    assertFalse(e.controls.gates.sendTraces());
    assertEquals("{\"send_traces\":\"false\"}", effective(e));
  }

  @Test
  void noSettingsEntryMeansTheAgentsOwnValues(@TempDir Path dir) {
    Extension e = Extension.forTest(setup(dir));
    e.remoteConfig(config("elastic", "{\"send_traces\":\"false\"}"));
    assertEquals(APPLIED, e.remoteConfig(config(null, "")).getStatus());
    assertTrue(e.controls.gates.sendTraces());
    assertEquals("{}", effective(e));
  }

  @Test
  void nextStartSettingsWaitForTheRestartAndShowAsPendingUntilThen(@TempDir Path dir) {
    Extension e = Extension.forTest(setup(dir));
    Opamp.RemoteConfigStatus s = e.remoteConfig(config("elastic", "{\"deactivate_instrumentations\":\"jdbc\"}"));
    assertEquals(APPLIED, s.getStatus());
    // Not in force yet: the effective config still has the agent's own.
    assertEquals("{}", effective(e));
    Kept.keepApplied(dir, "{\"deactivate_instrumentations\":\"jdbc\"}"); // as the client does, before APPLIED goes out

    // The next start reads them, as agent properties, and they are in force.
    Extension next = Extension.forTest(null);
    Map<String, String> props = next.properties(DefaultConfigProperties.createFromMap(
        Map.of("otel.opamp.endpoint", "http://server/v1/opamp", "otel.opamp.state.dir", dir.toString())));
    assertEquals(Map.of("otel.instrumentation.jdbc.enabled", "false"), props);
    assertEquals("{\"deactivate_instrumentations\":\"jdbc\"}", effective(next));
  }

  @Test
  void withoutAStateDirectoryNextStartSettingsAreFailed() {
    Extension e = Extension.forTest(setup(null));
    Opamp.RemoteConfigStatus s = e.remoteConfig(config("elastic", "{\"sampling_rate\":\"0.5\",\"deactivate_all_instrumentations\":\"true\"}"));
    assertEquals(FAILED, s.getStatus());
    assertTrue(s.getErrorMessage().contains("take effect at the next start"), s.getErrorMessage());
    // Nothing of it applied, the live setting included.
    assertEquals("{}", effective(e));
    // Live settings alone are fine.
    assertEquals(APPLIED, e.remoteConfig(config("elastic", "{\"sampling_rate\":\"0.5\"}")).getStatus());
  }

  @Test
  void theSettingsToStartWithAreTheConfigTheClientKeptAppliedElseTheFileKeptBefore(@TempDir Path dir) throws Exception {
    // Kept before the client kept the config applied (an earlier release): it carries over.
    Files.writeString(dir.resolve("sdk-settings.json"), "{\"sampling_rate\":\"0.25\"}");
    assertEquals(0.25, startedIn(dir).inForce().getSamplingRate());
    // Once the client keeps a config applied, that is where a start begins: the old file is ignored.
    Kept.keepApplied(dir, "{\"sampling_rate\":\"0.5\"}");
    assertEquals(0.5, startedIn(dir).inForce().getSamplingRate());
    // A config applied with no settings entry: every setting the SDK's own, not the old file's.
    Kept.keepApplied(dir, "{}");
    assertFalse(startedIn(dir).inForce().hasSamplingRate());
    // A client state that doesn't read: the old file again.
    Files.write(dir.resolve("opamp-state"), new byte[] {(byte) 0xff, (byte) 0xff});
    assertEquals(0.25, startedIn(dir).inForce().getSamplingRate());
  }

  static Extension startedIn(Path dir) {
    Extension e = Extension.forTest(null);
    e.properties(DefaultConfigProperties.createFromMap(
        Map.of("otel.opamp.endpoint", "http://server/v1/opamp", "otel.opamp.state.dir", dir.toString())));
    return e;
  }

  @Test
  void savedSettingsAreInForceFromTheStart(@TempDir Path dir) throws Exception {
    Kept.keepApplied(dir, "{\"send_metrics\":\"false\",\"sampling_rate\":\"0\"}");
    Extension e = Extension.forTest(null);
    e.properties(DefaultConfigProperties.createFromMap(
        Map.of("otel.opamp.endpoint", "http://server/v1/opamp", "otel.opamp.state.dir", dir.toString())));
    assertFalse(e.controls.gates.sendMetrics());
    assertEquals("{\"sampling_rate\":\"0\",\"send_metrics\":\"false\"}", effective(e));
  }

  @Test
  void withoutAnEndpointNothingChanges() {
    Extension e = Extension.forTest(null);
    assertEquals(Map.of(), e.properties(DefaultConfigProperties.createFromMap(Map.of())));
    assertFalse(e.on());
    Resource r = Resource.empty();
    assertEquals(r, e.resource(r));
  }

  @Test
  void oneUidNamesTheInstanceAndIsKeptAcrossStarts(@TempDir Path dir) {
    AttributeKey<String> id = AttributeKey.stringKey("service.instance.id");
    Map<String, String> props = Map.of("otel.opamp.endpoint", "http://s/v1/opamp", "otel.opamp.state.dir", dir.toString());
    Extension first = Extension.forTest(null);
    first.properties(DefaultConfigProperties.createFromMap(props));
    String uid = first.resource(Resource.empty()).getAttribute(id);
    assertEquals(7, java.util.UUID.fromString(uid).version());
    Extension second = Extension.forTest(null);
    second.properties(DefaultConfigProperties.createFromMap(props));
    assertEquals(uid, second.resource(Resource.create(io.opentelemetry.api.common.Attributes.of(id, "other"))).getAttribute(id));
  }

  /**
   * Issue #218: a state directory the UID can't be saved in keeps none, so a UUID
   * service.instance.id names the instance at every start, as without one; not a new UID each
   * start. Whether the directory can't be written at all, or only the UID's file can't be.
   */
  @Test
  void aStateDirectoryItCantSaveInKeepsAGivenUuid(@TempDir Path dir) throws Exception {
    AttributeKey<String> id = AttributeKey.stringKey("service.instance.id");
    String given = "0190a3b2-7c4d-7e8f-9a0b-1c2d3e4f5a6b";
    Path file = Files.writeString(dir.resolve("file"), "");
    Path stuck = dir.resolve("stuck");
    Files.createDirectories(stuck.resolve("instance-uid.tmp"));
    Files.writeString(stuck.resolve("instance-uid.tmp").resolve("x"), "");
    for (Path state : List.of(file.resolve("state"), stuck)) {
      Map<String, String> props = Map.of("otel.opamp.endpoint", "http://s/v1/opamp", "otel.opamp.state.dir", state.toString());
      for (int i = 0; i < 2; i++) {
        Extension e = Extension.forTest(null);
        e.properties(DefaultConfigProperties.createFromMap(props));
        Resource r = Resource.create(io.opentelemetry.api.common.Attributes.of(id, given));
        assertEquals(given, e.resource(r).getAttribute(id), state.toString());
      }
      Extension bare = Extension.forTest(null);
      bare.properties(DefaultConfigProperties.createFromMap(props));
      assertEquals(7, java.util.UUID.fromString(bare.resource(Resource.empty()).getAttribute(id)).version());
    }
  }

  @Test
  void aUidTheServerAssignsNamesTheInstanceFromTheNextStart(@TempDir Path dir) throws Exception {
    AttributeKey<String> id = AttributeKey.stringKey("service.instance.id");
    Map<String, String> props = Map.of("otel.opamp.endpoint", "http://s/v1/opamp", "otel.opamp.state.dir", dir.toString());
    Extension first = Extension.forTest(null);
    first.properties(DefaultConfigProperties.createFromMap(props));
    String before = first.resource(Resource.empty()).getAttribute(id);
    java.util.UUID assigned = Extension.uuidV7();
    first.notified(new dev.pydantic.opamp.Notification.InstanceUidChanged(
        java.nio.ByteBuffer.allocate(16).putLong(assigned.getMostSignificantBits())
            .putLong(assigned.getLeastSignificantBits()).array()));
    Extension second = Extension.forTest(null);
    second.properties(DefaultConfigProperties.createFromMap(props));
    String after = second.resource(Resource.empty()).getAttribute(id);
    assertEquals(assigned.toString(), after);
    assertFalse(after.equals(before));
  }

  @Test
  void aStateDirectoryThatCantBeWrittenIsTheClientsToKeepTrying(@TempDir Path tmp) throws Exception {
    // A directory that can't be made, whoever runs the test (root writes anywhere else).
    Path dir = Files.writeString(tmp.resolve("a-file"), "").resolve("state");
    Extension e = Extension.forTest(null);
    e.properties(DefaultConfigProperties.createFromMap(
        Map.of("otel.opamp.endpoint", "http://server/v1/opamp", "otel.opamp.state.dir", dir.toString())));
    // The agent saves nothing: the client keeps the config applied with its state, and until that
    // save lands the server hears APPLYING, not APPLIED (the client's own tests). The same in
    // every language.
    Opamp.RemoteConfigStatus live = e.remoteConfig(config("elastic", "{\"send_logs\":\"false\"}"));
    assertEquals(APPLIED, live.getStatus());
    assertFalse(e.controls.gates.sendLogs());
  }

  @Test
  void headersAreReadAsTheOtlpExporterReadsThem() {
    assertEquals(Map.of("Authorization", "Bearer a b", "x", "1=2"),
        Extension.headers("Authorization=Bearer%20a%20b, x=1=2,broken"));
    // '+' is a plus, as every other SDK reads it: a base64 token keeps it.
    assertEquals(Map.of("Authorization", "Bearer abc+def=="), Extension.headers("Authorization=Bearer abc+def=="));
  }

  @Test
  void aHeaderItCantDecodeIsSentAsWrittenWithAWarningThatNamesOnlyTheHeader() {
    List<String> said = logged(Level.WARNING, () -> assertEquals(
        Map.of("Authorization", "Bearer 100%", "x", "%zz", "y", "ok"),
        Extension.headers("Authorization=Bearer 100%,x=%zz,y=ok")));
    assertEquals(List.of(
        "WARNING OpAMP: otel.opamp.headers: the value of 'Authorization' has a percent escape that can't be "
            + "decoded: it is sent as written",
        "WARNING OpAMP: otel.opamp.headers: the value of 'x' has a percent escape that can't be decoded: it is "
            + "sent as written"), said);
  }

  @Test
  void aHeartbeatItCantReadIsWarnedAboutAndTheDefaultUsed(@TempDir Path dir) {
    List<String> said = logged(Level.WARNING, () -> assertEquals(Duration.ZERO, Extension.Setup.from(
        DefaultConfigProperties.createFromMap(Map.of("otel.opamp.endpoint", "http://server/v1/opamp",
            "otel.opamp.state.dir", dir.toString(), "otel.opamp.heartbeat.interval", "thirty")),
        Map.of(), null).heartbeat()));
    assertEquals(List.of("WARNING OpAMP: otel.opamp.heartbeat.interval 'thirty' is not a duration (as 1500ms, "
        + "30s or 2m): using the default, 30s"), said);
  }

  @Test
  void aValueItCantUseNeverStopsTheSdk(@TempDir Path dir) {
    // Under autoconfigure a properties customizer that throws fails the whole SDK, not only OpAMP.
    for (Map.Entry<String, String> bad : Map.of("otel.opamp.headers", "Authorization=Bearer 100%",
        "otel.opamp.heartbeat.interval", "thirty").entrySet()) {
      Extension e = Extension.forTest(null);
      logged(Level.WARNING, () -> e.properties(DefaultConfigProperties.createFromMap(Map.of(
          "otel.opamp.endpoint", "http://127.0.0.1:1/v1/opamp", "otel.opamp.state.dir", dir.toString(),
          bad.getKey(), bad.getValue())), Map.of()));
      assertTrue(e.on(), bad.getKey());
    }
  }

  /** What the extension logs at {@code level} or above while {@code run} runs, as level and message. */
  static List<String> logged(Level level, Runnable run) {
    Logger log = Logger.getLogger(Extension.class.getName());
    List<String> said = new ArrayList<>();
    Handler h = new Handler() {
      @Override
      public void publish(LogRecord r) {
        if (r.getLevel().intValue() >= level.intValue()) {
          said.add(r.getLevel() + " " + r.getMessage());
        }
      }

      @Override
      public void flush() {}

      @Override
      public void close() {}
    };
    Level was = log.getLevel();
    log.setLevel(Level.ALL);
    log.addHandler(h);
    try {
      run.run();
    } finally {
      log.removeHandler(h);
      log.setLevel(was);
    }
    return said;
  }

  @Test
  void connectingFailingAndRecoveringAreEachSaidOnce() {
    UUID uid = new UUID(0, 7);
    Extension.Reachability r = new Extension.Reachability(
        "https://user:secret@opamp.example.com/v1/opamp?token=x#frag", "checkout", uid);
    List<String> all = logged(Level.ALL, () -> {
      r.notified(new Notification.ConnectFailed(401, "HTTP 401"));
      r.notified(new Notification.ConnectFailed(401, "HTTP 401"));
      r.notified(new Notification.Connected());
      r.notified(new Notification.Connected());
      r.notified(new Notification.ConnectFailed(null, "connection refused"));
      r.notified(new Notification.Connected());
    });
    List<String> said = all.stream().filter(m -> !m.startsWith("FINE")).toList();
    assertEquals(List.of(
        "WARNING OpAMP: can't reach https://opamp.example.com/v1/opamp (HTTP 401): the server refused this agent's "
            + "credentials: check otel.opamp.headers / OTEL_OPAMP_HEADERS (Authorization=Bearer <token>). "
            + "Retrying in the background; the SDK keeps its current settings meanwhile",
        "INFO OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance "
            + uid,
        "WARNING OpAMP: can't reach https://opamp.example.com/v1/opamp (connection refused): check "
            + "otel.opamp.endpoint / OTEL_OPAMP_ENDPOINT and that the server is up. Retrying in the background; "
            + "the SDK keeps its current settings meanwhile",
        "INFO OpAMP: connected to https://opamp.example.com/v1/opamp again"), said);
    assertEquals(1, all.stream().filter(m -> m.startsWith("FINE")).count());
    for (String m : all) {
      assertFalse(m.contains("secret") || m.contains("token=x") || m.contains("frag"), m);
    }
  }

  /**
   * After the server moved the agent to another endpoint, what it says names the one the client uses
   * now (logged without credentials), not the one configured.
   */
  @Test
  void whatIsSaidNamesTheEndpointInUse() {
    String[] inUse = {null}; // none yet: the configured one
    Extension.Reachability r = new Extension.Reachability("https://a.example.com/v1/opamp", null, new UUID(0, 1));
    r.inUse = () -> inUse[0];
    List<String> said = logged(Level.INFO, () -> {
      r.notified(new Notification.Connected());
      inUse[0] = "https://user:secret@b.example.com/v1/opamp?token=x";
      r.notified(new Notification.ConnectFailed(null, "connection refused"));
      r.notified(new Notification.Connected());
    });
    assertEquals(3, said.size(), said.toString());
    assertTrue(said.get(0).startsWith("INFO OpAMP: connected to https://a.example.com/v1/opamp;"), said.get(0));
    assertTrue(said.get(1).startsWith("WARNING OpAMP: can't reach https://b.example.com/v1/opamp (connection refused)"),
        said.get(1));
    assertEquals("INFO OpAMP: connected to https://b.example.com/v1/opamp again", said.get(2));
  }

  @Test
  void theHintFollowsTheStatusAndTheServiceIsLeftOutWithoutOne() {
    assertTrue(Extension.hint(403).startsWith("the server refused this agent's credentials"));
    assertTrue(Extension.hint(404).startsWith("nothing serves OpAMP at that path"));
    assertEquals("the server answered with an error", Extension.hint(500));
    UUID uid = new UUID(0, 1);
    Extension.Reachability r = new Extension.Reachability("http://[::1]:4320/v1/opamp", null, uid);
    assertEquals(List.of("INFO OpAMP: connected to http://[::1]:4320/v1/opamp; this SDK is managed, instance " + uid),
        logged(Level.INFO, () -> r.notified(new Notification.Connected())));
  }

  @Test
  void theEndpointIsCutForTheLogNeverParsed() {
    assertEquals("https://opamp.example.com:notaport/v1/opamp",
        Extension.where("https://u:p@opamp.example.com:notaport/v1/opamp?token=x#f"));
    assertEquals("https://[::1/v1/opamp", Extension.where("https://u:p@[::1/v1/opamp?k=v"));
    assertEquals("https://[::1/v1/opamp", Extension.where("https://[::1/v1/opamp?k=v"));
    assertEquals("https://opamp.example.com", Extension.where("https://a@b@opamp.example.com#x"));
    assertEquals("opamp.example.com/v1/opamp", Extension.where("u:p@opamp.example.com/v1/opamp?k=v"));
  }

  @Test
  void anEndpointWithoutASchemeIsWarnedAboutAndTakenAsHttp() {
    // Taken as http://, as the warning says: the client refuses an endpoint without a scheme.
    List<String> said = logged(Level.WARNING, () -> assertEquals("http://u:p@opamp.example.com:4320/v1/opamp?k=v",
        Extension.Setup.from(DefaultConfigProperties.createFromMap(
            Map.of("otel.opamp.endpoint", " u:p@opamp.example.com:4320/v1/opamp?k=v ")), Map.of(), null).endpoint()));
    assertEquals(List.of("WARNING OpAMP: otel.opamp.endpoint 'opamp.example.com:4320/v1/opamp' has no scheme, so "
        + "it is taken as http://; name it in full, as https://host/v1/opamp or wss://host/v1/opamp"), said);
  }

  /** A user's state directory under {@code base}, whatever the OS; never the real home. */
  static Map<String, String> home(Path base) {
    return Map.of("XDG_STATE_HOME", base.toString(), "HOME", base.toString(), "LOCALAPPDATA", base.toString());
  }

  static Extension.Setup setupFrom(Map<String, String> props, Map<String, String> env) {
    return Extension.Setup.from(DefaultConfigProperties.createFromMap(props), env, "com.example.Checkout");
  }

  @Test
  void byDefaultAnInstanceKeepsItsUidAcrossRestarts(@TempDir Path base) throws Exception {
    AttributeKey<String> id = AttributeKey.stringKey("service.instance.id");
    Map<String, String> props = Map.of("otel.opamp.endpoint", "http://s/v1/opamp", "otel.service.name", "checkout");
    Extension first = Extension.forTest(null);
    first.properties(DefaultConfigProperties.createFromMap(props), home(base));
    String uid = first.resource(Resource.empty()).getAttribute(id);
    Path dir = first.setupForTest().stateDir();
    assertTrue(dir.startsWith(base.resolve("opamp")) && dir.getParent().getFileName().toString().startsWith("checkout-"),
        dir.toString());
    if (dir.getFileSystem().supportedFileAttributeViews().contains("posix")) {
      assertEquals("rwx------", java.nio.file.attribute.PosixFilePermissions.toString(Files.getPosixFilePermissions(dir)));
    }
    // Another process of it, running at once: a slot of its own, so an identity of its own.
    Extension other = Extension.forTest(null);
    other.properties(DefaultConfigProperties.createFromMap(props), home(base));
    String otherUid = other.resource(Resource.empty()).getAttribute(id);
    assertFalse(other.setupForTest().stateDir().equals(dir));
    assertFalse(otherUid.equals(uid));
    Extension.release(first.setupForTest());
    Extension.release(other.setupForTest());
    Extension again = Extension.forTest(null);
    again.properties(DefaultConfigProperties.createFromMap(props), home(base));
    assertEquals(dir, again.setupForTest().stateDir());
    assertEquals(uid, again.resource(Resource.empty()).getAttribute(id));
    Extension.release(again.setupForTest());
    // Another service is another instance.
    Extension elsewhere = Extension.forTest(null);
    Map<String, String> billing = new java.util.HashMap<>(props);
    billing.put("otel.service.name", "billing");
    elsewhere.properties(DefaultConfigProperties.createFromMap(billing), home(base));
    assertFalse(elsewhere.resource(Resource.empty()).getAttribute(id).equals(uid));
    Extension.release(elsewhere.setupForTest());
  }

  /**
   * A service.instance.id that isn't a UUID can't be the instance UID, so it is replaced by one:
   * kept in the default state directory, the same at every start, rather than a new one each time.
   */
  @Test
  void aServiceInstanceIdThatIsntAUuidKeepsTheDefaultStateDirectory(@TempDir Path base) throws Exception {
    AttributeKey<String> id = AttributeKey.stringKey("service.instance.id");
    Map<String, String> props = Map.of("otel.opamp.endpoint", "http://s/v1/opamp", "otel.service.name", "web",
        "otel.resource.attributes", "service.instance.id=web-1");
    List<String> uids = new java.util.ArrayList<>();
    for (int i = 0; i < 2; i++) {
      Extension e = Extension.forTest(null);
      e.properties(DefaultConfigProperties.createFromMap(props), home(base));
      Path dir = e.setupForTest().stateDir();
      assertTrue(dir != null && dir.startsWith(base), String.valueOf(dir));
      uids.add(e.resource(Resource.create(io.opentelemetry.api.common.Attributes.of(id, "web-1"))).getAttribute(id));
      Extension.release(e.setupForTest()); // as the process exiting would
    }
    assertEquals(uids.get(0), uids.get(1));
  }

  @Test
  void theDefaultStateDirectoryGivesWay(@TempDir Path base) throws Exception {
    Map<String, String> on = Map.of("otel.opamp.endpoint", "http://s");
    // Turned off, or a service.instance.id the application chose (a UUID, so the instance UID): none.
    assertEquals(null, setupFrom(Map.of("otel.opamp.endpoint", "http://s", "otel.opamp.state.dir", "none"),
        home(base)).stateDir());
    assertEquals(null, setupFrom(Map.of("otel.opamp.endpoint", "http://s", "otel.resource.attributes",
        "service.instance.id=" + UUID.randomUUID()), home(base)).stateDir());
    // Named: that one, unlocked as before.
    Extension.Setup named = setupFrom(Map.of("otel.opamp.endpoint", "http://s", "otel.opamp.state.dir",
        base.resolve("mine").toString()), home(base));
    assertEquals(base.resolve("mine"), named.stateDir());
    assertEquals(null, named.stateLock());
    // Nowhere to keep it: none, and no error.
    assertEquals(null, setupFrom(on, Map.of()).stateDir());
    Path file = Files.writeString(base.resolve("file"), "");
    assertEquals(null, setupFrom(on, home(file)).stateDir());
    // Every slot taken: none.
    List<Extension.Setup> held = new ArrayList<>();
    for (int i = 0; i < Extension.SLOTS; i++) {
      held.add(setupFrom(on, home(base)));
    }
    assertTrue(held.stream().allMatch(h -> h.stateDir() != null));
    assertEquals(Extension.SLOTS, held.stream().map(Extension.Setup::stateDir).distinct().count());
    List<String> said = logged(Level.INFO, () -> assertEquals(null, setupFrom(on, home(base)).stateDir()));
    assertEquals(1, said.size(), said.toString());
    assertTrue(said.get(0).startsWith("INFO OpAMP: 16 processes of com.example.Checkout already"), said.get(0));
    for (Extension.Setup h : held) {
      Extension.release(h);
    }
  }

  @Test
  void theProgramIsTheMainClassOrTheJar() {
    assertEquals("com.example.Main", Extension.programName("com.example.Main --port 8080"));
    assertEquals("checkout", Extension.programName("/opt/app/checkout.jar --port 8080"));
    assertEquals("checkout", Extension.programName("C:\\app\\checkout.jar"));
    assertEquals("java", Extension.programName(null));
    assertEquals("java", Extension.programName("  "));
  }
}
