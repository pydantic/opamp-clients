// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import static dev.pydantic.opamp.javaagent.Vectors.every;
import static dev.pydantic.opamp.javaagent.Vectors.exports;
import static dev.pydantic.opamp.javaagent.Vectors.full;
import static dev.pydantic.opamp.javaagent.Vectors.override;
import static dev.pydantic.opamp.javaagent.Vectors.v1;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.fasterxml.jackson.databind.JsonNode;
import dev.pydantic.opamp.internal.AppSettingsBridge;
import io.opentelemetry.sdk.autoconfigure.spi.internal.DefaultConfigProperties;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.atomic.AtomicReference;
import java.util.stream.Stream;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.DynamicTest;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.TestFactory;
import org.junit.jupiter.api.io.TempDir;

/**
 * The application's own settings (docs/app-settings.md) through the extension: what the layer
 * hands over in {@code Settings.custom} reaches the bridge {@code AppSettings} reads, at start,
 * on an APPLIED config and on a refresh, and nothing else does.
 */
class AppSettingsExtensionTest {
  private static final Opamp.RemoteConfigStatuses APPLIED = Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED;
  private static final Opamp.RemoteConfigStatuses FAILED = Opamp.RemoteConfigStatuses.RemoteConfigStatuses_FAILED;

  record Told(Map<String, String> settings, Set<String> changed) {}

  /** A bridge of the test's own, and what each change told. */
  static final class App {
    final AppSettingsBridge bridge = new AppSettingsBridge();
    final List<Told> told = Collections.synchronizedList(new ArrayList<>());
    final java.util.function.BiConsumer<Map<String, String>, Set<String>> receiver =
        (settings, changed) -> told.add(new Told(settings, changed));
    final AppSettingsSink sink;

    App() throws Exception {
      bridge.subscribe(receiver);
      sink = new AppSettingsSink(bridge);
    }
  }

  static Opamp.AgentRemoteConfig config(String document) {
    return ExtensionTest.config(document);
  }

  static Extension extension(Path dir, App app) {
    return Extension.forTest(ExtensionTest.setup(dir), app.sink);
  }

  static Map<String, String> props(Path dir) {
    return Map.of("otel.opamp.endpoint", "http://server/v1/opamp", "otel.opamp.state.dir",
        dir == null ? "none" : dir.toString());
  }

  @Test
  void customIsDeclaredOnlyWhereTheApplicationCanReadIt(@TempDir Path dir) throws Exception {
    assertFalse(Extension.HOST.getLiveList().contains("custom"));
    assertTrue(Extension.HOST_WITH_APP.getLiveList().contains("custom"));
    assertTrue(Extension.HOST_WITH_APP.getLiveList().containsAll(Extension.HOST.getLiveList()));
    App app = new App();
    assertEquals(Extension.HOST_WITH_APP, extension(dir, app).host());

    // Without the bridge (the agent could not put it in place): a policy that overrides app_settings
    // is FAILED, saying so, and nothing of it is in force.
    Extension without = Extension.forTest(ExtensionTest.setup(dir));
    assertEquals(Extension.HOST, without.host());
    Opamp.RemoteConfigStatus s = without.remoteConfig(config(every("signal_exports", exports("off", "", ""),
        "app_settings", override("\"values\":{\"a\":\"1\"}"))));
    assertEquals(FAILED, s.getStatus());
    assertEquals("app_settings: values: this SDK cannot apply the requested override", s.getErrorMessage());
    assertTrue(without.controls.gates.sendTraces());
    assertEquals(full(v1()), ExtensionTest.effective(without));
  }

  /** custom.json's cases, through the extension's own Decide: the map handed over, or FAILED with nothing changed. */
  @TestFactory
  Stream<DynamicTest> theSharedCases() {
    return Vectors.cases("custom").stream().map(c -> DynamicTest.dynamicTest(c.get("name").asText(), () -> {
      App app = new App();
      Extension e = extension(null, app);
      if (c.has("host")) {
        // A host of the case's own: through the layer's Decide, as the extension calls it.
        var host = dev.pydantic.opamp.sdk.SettingsProto.Host.newBuilder();
        c.get("host").path("live").forEach(n -> host.addLive(n.asText()));
        c.get("host").path("next_start").forEach(n -> host.addNextStart(n.asText()));
        var d = Vectors.LAYER.decide(config(c.get("document").asText()), com.google.protobuf.ByteString.EMPTY,
            com.google.protobuf.ByteString.EMPTY, host.build(), Vectors.NOW, null);
        if (c.has("refused")) {
          assertEquals(FAILED, d.getStatus().getStatus());
          assertEquals(c.get("refused").asText(), d.getStatus().getErrorMessage());
          assertFalse(d.hasApply());
          return;
        }
        assertEquals(APPLIED, d.getStatus().getStatus());
        e.controls.apply(d.getApply());
        assertEquals(map(c.get("custom")), app.bridge.current());
        return;
      }
      // Something in force first, so a FAILED case can show it changed nothing.
      assertEquals(APPLIED, e.remoteConfig(config(every("app_settings", override("\"values\":{\"before\":\"1\"}")))).getStatus());
      app.told.clear();
      Opamp.RemoteConfigStatus s = e.remoteConfig(config(c.get("document").asText()));
      if (c.has("refused")) {
        assertEquals(FAILED, s.getStatus());
        assertEquals(c.get("refused").asText(), s.getErrorMessage());
        assertEquals(Map.of("before", "1"), app.bridge.current());
        assertEquals(List.of(), app.told);
        return;
      }
      assertEquals(APPLIED, s.getStatus(), s.getErrorMessage());
      Map<String, String> want = map(c.get("custom"));
      assertEquals(want, app.bridge.current());
      assertEquals(want, e.inForce().getCustomMap());
      assertEquals(1, app.told.size());
      // Reported as the layer reports it: the policy in force, compact, keys sorted.
      if (c.has("effective")) {
        assertEquals(c.get("effective").asText(), ExtensionTest.effective(e));
      }
    }));
  }

  private static Map<String, String> map(JsonNode n) {
    Map<String, String> m = new LinkedHashMap<>();
    n.fields().forEachRemaining(f -> m.put(f.getKey(), f.getValue().asText()));
    return m;
  }

  @Test
  void aChangeIsToldOnceAndTheSameMapNeverAgain(@TempDir Path dir) throws Exception {
    App app = new App();
    Extension e = extension(dir, app);
    assertEquals(APPLIED, e.remoteConfig(config(every("app_settings", override("\"values\":{\"example.feature\":\"on\",\"limit\":\"25\"}")))).getStatus());
    assertEquals(List.of(new Told(Map.of("example.feature", "on", "limit", "25"), Set.of("example.feature", "limit"))),
        app.told);
    // The same map again, another way each time: keys in another order, 25 for "25", another
    // control beside it.
    for (String same : List.of(every("app_settings", override("\"values\":{\"example.feature\":\"on\",\"limit\":\"25\"}")),
        every("app_settings", override("\"values\":{\"limit\":\"25\",\"example.feature\":\"on\"}")),
        every("app_settings", override("\"values\":{\"limit\":25,\"example.feature\":\"on\"}")),
        every("head_sampling", override("\"ratio\":0.5"), "app_settings", override("\"values\":{\"limit\":\"25\",\"example.feature\":\"on\"}")))) {
      assertEquals(APPLIED, e.remoteConfig(config(same)).getStatus(), same);
    }
    assertEquals(1, app.told.size());
    // A value changed: that key alone.
    e.remoteConfig(config(every("app_settings", override("\"values\":{\"example.feature\":\"off\",\"limit\":\"25\"}"))));
    assertEquals(new Told(Map.of("example.feature", "off", "limit", "25"), Set.of("example.feature")), app.told.get(1));
    // A policy that leaves app_settings out (local): every key back to the application's default.
    e.remoteConfig(config(v1("head_sampling", override("\"ratio\":0.5"))));
    assertEquals(new Told(Map.of(), Set.of("example.feature", "limit")), app.told.get(2));
    // A config without a policy: FAILED, so nothing to tell.
    assertEquals(FAILED, e.remoteConfig(ExtensionTest.config(null, "")).getStatus());
    assertEquals(3, app.told.size());
  }

  @Test
  void aFailedConfigChangesNothingTheApplicationReads(@TempDir Path dir) throws Exception {
    App app = new App();
    Extension e = extension(dir, app);
    e.remoteConfig(config(every("signal_exports", exports("off", "", ""), "app_settings", override("\"values\":{\"example.feature\":\"on\"}"))));
    app.told.clear();
    for (String bad : List.of(every("app_settings", override("\"values\":{\"example.feature\":[\"on\"]}")),
        every("head_sampling", override("\"ratio\":7"), "app_settings", override("\"values\":{\"example.feature\":\"off\"}")),
        every("signal_exports", exports("on", "", ""), "app_settings", override("\"values\":{\"a b\":\"x\"}")))) {
      assertEquals(FAILED, e.remoteConfig(config(bad)).getStatus(), bad);
    }
    assertEquals(Map.of("example.feature", "on"), app.bridge.current());
    assertFalse(e.controls.gates.sendTraces());
    assertEquals(List.of(), app.told);
  }

  @Test
  void theListenerSeesTheSdksSettingsOfTheSameDocumentAlreadyInForce(@TempDir Path dir) throws Exception {
    App app = new App();
    Extension e = extension(dir, app);
    List<String> seen = new ArrayList<>();
    // The bridge holds its receivers weakly (an undeployed application's go with its class loader):
    // the test holds this one until the configs are through, or a GC between them would drop it.
    java.util.function.BiConsumer<Map<String, String>, Set<String>> listener =
        (settings, changed) -> seen.add(settings.get("traces") + "/" + e.controls.gates.sendTraces());
    app.bridge.subscribe(listener);
    e.remoteConfig(config(every("signal_exports", exports("off", "", ""), "app_settings", override("\"values\":{\"traces\":\"false\"}"))));
    e.remoteConfig(config(every("signal_exports", exports("on", "", ""), "app_settings", override("\"values\":{\"traces\":\"true\"}"))));
    java.lang.ref.Reference.reachabilityFence(listener);
    assertEquals(List.of("false/false", "true/true"), seen);
  }

  @Test
  void documentsAppliedFromSeveralThreadsGoInWhole(@TempDir Path dir) throws Exception {
    // The SDK's settings and the application's of one document go in under one lock: a listener
    // never sees the SDK's settings of one document beside the application's of another.
    App app = new App();
    Extension e = extension(null, app);
    AtomicReference<String> torn = new AtomicReference<>();
    java.util.concurrent.atomic.AtomicInteger heard = new java.util.concurrent.atomic.AtomicInteger();
    // Held until the threads are done: the bridge holds its receivers weakly.
    java.util.function.BiConsumer<Map<String, String>, Set<String>> listener = (settings, changed) -> {
      heard.incrementAndGet();
      boolean want = Boolean.parseBoolean(settings.get("logs"));
      if (e.controls.gates.sendLogs() != want) {
        torn.set(settings + " beside send_logs " + e.controls.gates.sendLogs());
      }
    };
    app.bridge.subscribe(listener);
    List<Thread> threads = new ArrayList<>();
    for (int t = 0; t < 4; t++) {
      int id = t;
      Thread th = new Thread(() -> {
        for (int i = 0; i < 100; i++) {
          boolean logs = (i + id) % 2 == 0;
          e.controls.apply(settings(every("signal_exports", exports("", "", logs ? "on" : "off"),
              "app_settings", override("\"values\":{\"logs\":\"" + logs + "\",\"n\":\"" + id + ":" + i + "\"}"))));
        }
      });
      threads.add(th);
      th.start();
    }
    for (Thread th : threads) {
      th.join();
    }
    java.lang.ref.Reference.reachabilityFence(listener);
    assertNull(torn.get());
    assertEquals(400, app.told.size());
    assertEquals(400, heard.get(), "the listener heard every document");
  }

  @Test
  void theEndOfDebugModeTellsNothingWhenTheMapStaysTheSame(@TempDir Path dir) throws Exception {
    App app = new App();
    Extension e = extension(dir, app);
    e.wallClock = () -> Vectors.NOW;
    assertEquals(APPLIED, e.remoteConfig(config(every("head_sampling", override("\"ratio\":0.1"), "debug", override("\"until\":\"2026-10-14T12:30:00Z\""), "app_settings", override("\"values\":{\"example.feature\":\"on\"}")))).getStatus());
    assertEquals(1, app.told.size());
    long[] armed = e.armedForTest();
    e.fire(armed[0], armed[1]);
    assertEquals(0.1, e.inForce().getSamplingRate());
    assertEquals(Map.of("example.feature", "on"), app.bridge.current());
    assertEquals(1, app.told.size());
  }

  @Test
  void withAStateDirectoryTheValuesInForceAtExitAreInForceFromTheNextStart(@TempDir Path dir) throws Exception {
    App first = new App();
    Extension e = Extension.forTest(null, first.sink);
    e.properties(DefaultConfigProperties.createFromMap(props(dir)));
    assertEquals(Map.of(), first.bridge.current());
    assertEquals(APPLIED, e.remoteConfig(config(every("app_settings", override("\"values\":{\"example.feature\":\"on\"}")))).getStatus());
    Kept.keepApplied(dir, every("app_settings", override("\"values\":{\"example.feature\":\"on\"}"))); // as the client does, before APPLIED goes out

    // The next start: in force at Load, before any config arrives, and told to whoever listened.
    App second = new App();
    Extension next = Extension.forTest(null, second.sink);
    next.properties(DefaultConfigProperties.createFromMap(props(dir)));
    assertEquals(Map.of("example.feature", "on"), second.bridge.current());
    assertEquals(List.of(new Told(Map.of("example.feature", "on"), Set.of("example.feature"))), second.told);
    assertTrue(ExtensionTest.effective(next).contains(
        "\"app_settings\":{\"mode\":\"override\",\"values\":{\"example.feature\":\"on\"}}"),
        ExtensionTest.effective(next));
    // And the same map from the server again tells nothing.
    assertEquals(APPLIED, next.remoteConfig(config(every("app_settings", override("\"values\":{\"example.feature\":\"on\"}")))).getStatus());
    assertEquals(1, second.told.size());
  }

  @Test
  void savedValuesTheServerLaterLeavesOutGoBackToTheDefaults(@TempDir Path dir) throws Exception {
    Files.writeString(dir.resolve("sdk-settings.json"), every("app_settings", override("\"values\":{\"a\":\"1\"}")));
    App app = new App();
    Extension e = Extension.forTest(null, app.sink);
    e.properties(DefaultConfigProperties.createFromMap(props(dir)));
    assertEquals(Map.of("a", "1"), app.bridge.current());
    e.remoteConfig(config(v1()));
    assertEquals(Map.of(), app.bridge.current());
    // The file kept before is left as it was: the client keeps the config applied now.
    assertEquals(every("app_settings", override("\"values\":{\"a\":\"1\"}")), Files.readString(dir.resolve("sdk-settings.json"), StandardCharsets.UTF_8));
  }

  @Test
  void withoutAStateDirectoryEachStartBeginsFromTheDefaults(@TempDir Path dir) throws Exception {
    App app = new App();
    Extension e = Extension.forTest(null, app.sink);
    e.properties(DefaultConfigProperties.createFromMap(props(null)));
    // Live, so applied without a state directory too.
    assertEquals(APPLIED, e.remoteConfig(config(every("app_settings", override("\"values\":{\"a\":\"1\"}")))).getStatus());
    assertEquals(Map.of("a", "1"), app.bridge.current());
    App again = new App();
    Extension next = Extension.forTest(null, again.sink);
    next.properties(DefaultConfigProperties.createFromMap(props(null)));
    assertEquals(Map.of(), again.bridge.current());
    assertEquals(List.of(), again.told);
  }

  @Test
  void aFailedSetupPutsBackWhatWasInForceBefore(@TempDir Path dir) throws Exception {
    Files.writeString(dir.resolve("sdk-settings.json"), every("app_settings", override("\"values\":{\"a\":\"saved\"}")));
    App app = new App();
    app.bridge.put(Map.of("a", "before"));
    app.told.clear();
    Extension e = Extension.forTest(null, app.sink);
    e.properties(DefaultConfigProperties.createFromMap(props(dir)));
    assertEquals(Map.of("a", "saved"), app.bridge.current());
    // The controls couldn't be put in (as under a configuration file the extension can't wrap).
    e.off();
    assertFalse(e.on());
    assertEquals(Map.of("a", "before"), app.bridge.current());
    assertEquals(List.of(new Told(Map.of("a", "saved"), Set.of("a")), new Told(Map.of("a", "before"), Set.of("a"))),
        app.told);
  }

  @Test
  void withoutAnEndpointTheBridgeIsntPutInPlace() {
    Extension e = Extension.forTest(null);
    int[] asked = {0};
    e.appSettings = () -> {
      asked[0]++;
      return null;
    };
    e.properties(DefaultConfigProperties.createFromMap(Map.of()));
    assertEquals(0, asked[0]);
    assertNull(e.controls.app);
  }

  @Test
  void withAnEndpointTheBridgeIsPutInPlaceOnce(@TempDir Path dir) throws Exception {
    App app = new App();
    Extension e = Extension.forTest(null);
    int[] asked = {0};
    e.appSettings = () -> {
      asked[0]++;
      return app.sink;
    };
    e.properties(DefaultConfigProperties.createFromMap(props(dir)));
    e.properties(DefaultConfigProperties.createFromMap(props(dir)));
    assertEquals(1, asked[0]);
    assertEquals(Extension.HOST_WITH_APP, e.host());
  }

  @Test
  void outsideTheAgentTheBridgeCantBePutInPlaceAndCustomIsntDeclared() {
    // No agent in this JVM: no Instrumentation to append to the bootstrap class path with.
    assertNull(AppSettingsSink.install());
  }

  @Test
  void theBridgeJarHoldsOneClassThatNeedsNothingButTheJdk() throws Exception {
    Path jar = AppSettingsSink.bridgeJar();
    try (java.util.jar.JarFile f = new java.util.jar.JarFile(jar.toFile())) {
      assertEquals(List.of(AppSettingsSink.BRIDGE.replace('.', '/') + ".class"),
          f.stream().map(java.util.zip.ZipEntry::getName).toList());
    }
    // A class loader that sees the JDK alone, as the bootstrap does: the class loads, links and
    // works there, its every signature the JDK's.
    try (java.net.URLClassLoader alone = new java.net.URLClassLoader(new java.net.URL[] {jar.toUri().toURL()}, null)) {
      Class<?> c = Class.forName(AppSettingsSink.BRIDGE, true, alone);
      AppSettingsSink sink = new AppSettingsSink(c.getField("SHARED").get(null));
      assertEquals(Set.of("a"), sink.put(Map.of("a", "1")));
      assertEquals(Map.of("a", "1"), sink.current());
      assertEquals(Set.of(), sink.put(Map.of("a", "1")));
      for (var m : c.getDeclaredMethods()) {
        for (Class<?> p : m.getParameterTypes()) {
          assertNull(p.getClassLoader(), m + ": " + p);
        }
        assertTrue(m.getReturnType().isPrimitive() || m.getReturnType().getClassLoader() == null, m.toString());
      }
    }
    Files.deleteIfExists(jar);
  }

  @Test
  void theExampleRequestsTheFeatureWhileOnAndChangedOncePerChange(@TempDir Path dir) throws Exception {
    // check_app_settings's scenario (examples/integrations.py), told as the example counts it.
    App app = new App();
    Extension e = extension(dir, app);
    List<String> statuses = new ArrayList<>();
    for (String doc : List.of(every("app_settings", override("\"values\":{\"example.feature\":\"on\"}")), every("app_settings", override("\"values\":{\"example.feature\":[\"on\"]}")),
        v1("head_sampling", override("\"ratio\":1")), every("app_settings", override("\"values\":{\"example.feature\":\"on\"}")),
        every("head_sampling", override("\"ratio\":1"), "app_settings", override("\"values\":{\"example.feature\":\"on\"}")))) {
      statuses.add(e.remoteConfig(config(doc)).getStatus().name().replace("RemoteConfigStatuses_", ""));
    }
    assertEquals(List.of("APPLIED", "FAILED", "APPLIED", "APPLIED", "APPLIED"), statuses);
    assertEquals(3, app.told.size());
    assertEquals(Map.of("example.feature", "on"), app.bridge.current());
    // The restart, from the config the client kept applied (the last one): on from the start.
    Kept.keepApplied(dir, every("head_sampling", override("\"ratio\":1"), "app_settings", override("\"values\":{\"example.feature\":\"on\"}")));
    App next = new App();
    Extension again = Extension.forTest(null, next.sink);
    again.properties(DefaultConfigProperties.createFromMap(props(dir)));
    assertEquals("on", next.bridge.current().get("example.feature"));
  }

  /** The settings a policy puts in force, custom declared, as Decide hands them over. */
  static dev.pydantic.opamp.sdk.SettingsProto.Settings settings(String document) {
    var d = Vectors.LAYER.decide(config(document), com.google.protobuf.ByteString.EMPTY,
        com.google.protobuf.ByteString.EMPTY, Extension.HOST_WITH_APP, Vectors.NOW, null);
    assertEquals(APPLIED, d.getStatus().getStatus(), d.getStatus().getErrorMessage());
    return d.getApply();
  }
}
