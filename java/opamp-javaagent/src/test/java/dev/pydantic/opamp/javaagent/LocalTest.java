// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.sdk.autoconfigure.spi.internal.DefaultConfigProperties;
import java.nio.file.Path;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * The local report (contracts' logfire-sdk-local/v1): what the SDK autoconfigure builds does
 * without the server, as the host's {@code local} carries it to the layer; and each
 * instrumentation's scope in the inventory.
 */
class LocalTest {
  private static SettingsProto.Local local(String... props) {
    Map<String, String> p = new HashMap<>();
    for (int i = 0; i < props.length; i += 2) {
      p.put(props[i], props[i + 1]);
    }
    return Local.of(DefaultConfigProperties.createFromMap(p));
  }

  @Test
  void theSamplingRateIsTheRatioOfTheSamplerThePropertiesName() {
    assertEquals("1", local().getSamplingRate()); // parentbased_always_on, the SDK's default
    assertEquals("1", local("otel.traces.sampler", "always_on").getSamplingRate());
    assertEquals("0", local("otel.traces.sampler", "always_off").getSamplingRate());
    assertEquals("0", local("otel.traces.sampler", "parentbased_always_off").getSamplingRate());
    assertEquals("1", local("otel.traces.sampler", "traceidratio").getSamplingRate()); // its argument's default
    assertEquals("0.25", local("otel.traces.sampler", "traceidratio", "otel.traces.sampler.arg", "0.25")
        .getSamplingRate());
    assertEquals("0.1", local("otel.traces.sampler", "parentbased_traceidratio", "otel.traces.sampler.arg", "0.1")
        .getSamplingRate());
    // Not a ratio, or an argument the SDK can't build one from: not told.
    for (String[] props : List.of(new String[] {"otel.traces.sampler", "jaeger_remote"},
        new String[] {"otel.traces.sampler", "traceidratio", "otel.traces.sampler.arg", "2"},
        new String[] {"otel.traces.sampler", "traceidratio", "otel.traces.sampler.arg", "half"})) {
      assertFalse(local(props).hasSamplingRate(), String.join(" ", props));
    }
  }

  @Test
  void aRatioIsDecimalTextWithoutAnExponent() {
    assertEquals("1", Local.decimal(1));
    assertEquals("0", Local.decimal(0));
    assertEquals("0.25", Local.decimal(0.25));
    assertEquals("0.00001", Local.decimal(0.00001));
    assertEquals("0.0000001", Local.decimal(1e-7));
  }

  @Test
  void eachSignalIsExportedWhereThePropertiesNameAnExporterForIt() {
    SettingsProto.Local defaults = local();
    assertTrue(defaults.getSendTraces() && defaults.getSendMetrics() && defaults.getSendLogs());
    SettingsProto.Local some = local("otel.traces.exporter", "console", "otel.metrics.exporter", "none",
        "otel.logs.exporter", "none");
    assertTrue(some.hasSendTraces() && some.hasSendMetrics() && some.hasSendLogs());
    assertEquals(List.of(true, false, false), List.of(some.getSendTraces(), some.getSendMetrics(), some.getSendLogs()));
    // The SDK has no level of its own.
    assertFalse(some.hasSendLogsLevel());
  }

  @Test
  void theHostCarriesTheLocalValuesUnderAutoconfigureAndNoneUnderAFile(@TempDir Path dir) {
    Extension e = Extension.forTest(null);
    e.properties(DefaultConfigProperties.createFromMap(InventoryTest.props("http://server/v1/opamp", dir,
        "otel.traces.sampler", "parentbased_traceidratio", "otel.traces.sampler.arg", "0.5",
        "otel.logs.exporter", "none")));
    assertTrue(e.on());
    assertEquals(SettingsProto.Local.newBuilder().setSamplingRate("0.5").setSendTraces(true).setSendMetrics(true)
        .setSendLogs(false).build(), e.host().getLocal());
    Extension file = Extension.forTest(null);
    file.propertiesUnderFile(DefaultConfigProperties.createFromMap(InventoryTest.props("http://server/v1/opamp",
        dir.resolve("file"))));
    assertTrue(file.on());
    assertFalse(file.host().hasLocal());
  }

  @Test
  void eachInstrumentationIsListedWithTheScopeItsVersionIsEmbeddedFor() {
    Map<String, String> scopes = new HashMap<>();
    for (SettingsProto.Instrumentation i : Inventory.instrumentations(LocalTest.class.getClassLoader())) {
      scopes.put(i.getName(), i.getScope());
    }
    // Every name of the module carries its telemetry's one scope; none embedded: not known.
    assertEquals(Map.of("spring-webmvc", "io.opentelemetry.spring-webmvc-6.0",
        "spring-webmvc-6.0", "io.opentelemetry.spring-webmvc-6.0", "jdbc", "", "example-kafka", ""), scopes);
  }

  private static List<String> names(List<SettingsProto.Instrumentation> listed) {
    return listed.stream().map(SettingsProto.Instrumentation::getName).sorted().toList();
  }

  @Test
  void onlyTheInstrumentationsWhoseLibraryTheApplicationHasAreListedAndOneThatComesLaterIsAdded(@TempDir Path dir)
      throws Exception {
    Inventory inventory = new Inventory();
    List<ClassLoader> loaders = new java.util.ArrayList<>(List.of(LocalTest.class.getClassLoader()));
    inventory.loaders = () -> loaders;
    // The Kafka module's class isn't on the class path: every module is there, it isn't listed.
    assertEquals(List.of("example-kafka", "jdbc", "spring-webmvc", "spring-webmvc-6.0"),
        names(Inventory.instrumentations(LocalTest.class.getClassLoader())));
    assertEquals(List.of("jdbc", "spring-webmvc", "spring-webmvc-6.0"), names(inventory.applying()));
    assertFalse(inventory.look()); // looked there already: nothing new
    // The library comes later, in a class loader the application makes (a class file is what the
    // agent looks for).
    java.nio.file.Files.createDirectories(dir.resolve("com/example/kafka"));
    java.nio.file.Files.write(dir.resolve("com/example/kafka/Producer.class"), new byte[0]);
    loaders.add(new java.net.URLClassLoader(new java.net.URL[] {dir.toUri().toURL()}, null));
    assertTrue(inventory.look());
    assertEquals(List.of("example-kafka", "jdbc", "spring-webmvc", "spring-webmvc-6.0"), names(inventory.applying()));
    assertFalse(inventory.look());
  }

  @Test
  void theClassesATypeMatcherNamesAreReadFromItsDescription() {
    assertEquals(List.of("akka.dispatch.Dispatcher"), Inventory.Type.classesNamed("name(equals(akka.dispatch.Dispatcher))"));
    assertEquals(List.of("reactor.core.publisher.Flux", "reactor.core.publisher.Hooks"),
        Inventory.Type.classesNamed("name(in(reactor.core.publisher.Flux, reactor.core.publisher.Hooks))"));
    assertEquals(List.of("java.sql.Connection"),
        Inventory.Type.classesNamed("safeHasSuperType(safeErasure(name(equals(java.sql.Connection))))"));
    assertEquals(List.of("okhttp3.OkHttpClient$Builder"), Inventory.Type.classesNamed("name(equals(okhttp3.OkHttpClient$Builder))"));
    // A method's name, a pattern, a word ending in "in": no class.
    assertEquals(List.of(), Inventory.Type.classesNamed("named(equals(execute))"));
    assertEquals(List.of(), Inventory.Type.classesNamed("name(startsWith(java.net.))"));
    assertEquals(List.of(), Inventory.Type.classesNamed("join(a.b)"));
    // One that matches nothing never applies; one that names no class, by its class loader matcher.
    ClassLoader here = LocalTest.class.getClassLoader();
    assertFalse(new Inventory.Type(net.bytebuddy.matcher.ElementMatchers.any(), List.of(), true).appliesIn(here));
    assertTrue(new Inventory.Type(net.bytebuddy.matcher.ElementMatchers.any(), List.of(), false).appliesIn(here));
    assertTrue(new Inventory.Type(net.bytebuddy.matcher.ElementMatchers.any(), List.of("java.sql.Connection"), false)
        .appliesIn(here));
    assertFalse(new Inventory.Type(net.bytebuddy.matcher.ElementMatchers.none(), List.of("java.sql.Connection"), false)
        .appliesIn(here));
  }
}
