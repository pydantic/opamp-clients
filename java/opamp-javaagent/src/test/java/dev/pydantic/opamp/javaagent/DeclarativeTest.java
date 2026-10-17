// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.DeclarativeConfigResult;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.DeclarativeConfiguration;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.AttributeNameValueModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.OpenTelemetryConfigurationModel;
import io.opentelemetry.sdk.autoconfigure.spi.internal.DefaultConfigProperties;
import java.io.ByteArrayInputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.logging.Handler;
import java.util.logging.LogRecord;
import java.util.logging.Logger;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

class DeclarativeTest {
  /** A configuration file: every signal to the console, as the SDK reads it. */
  static final String FILE = """
      file_format: "1.1"
      resource:
        attributes:
          - name: service.name
            value: checkout
          - name: service.instance.id
            value: from-the-file
      tracer_provider:
        sampler:
          parent_based:
            root:
              always_off:
        processors:
          - simple:
              exporter:
                console:
      meter_provider:
        readers:
          - periodic:
              interval: 60000
              exporter:
                console:
      logger_provider:
        processors:
          - simple:
              exporter:
                console:
      """;

  static OpenTelemetryConfigurationModel model(String yaml) {
    return DeclarativeConfiguration.parse(new ByteArrayInputStream(yaml.getBytes(StandardCharsets.UTF_8)));
  }

  static DefaultConfigProperties config(Path dir) {
    return DefaultConfigProperties.createFromMap(
        Map.of("otel.opamp.endpoint", "http://server/v1/opamp", "otel.opamp.state.dir", dir.toString()));
  }

  /** How many spans the console exporter logs while {@code run} runs. */
  static int exported(Runnable run) {
    Logger log = Logger.getLogger("io.opentelemetry.exporter.logging.LoggingSpanExporter");
    List<LogRecord> said = new ArrayList<>();
    Handler h = new Handler() {
      @Override
      public void publish(LogRecord r) {
        said.add(r);
      }

      @Override
      public void flush() {}

      @Override
      public void close() {}
    };
    log.addHandler(h);
    try {
      run.run();
    } finally {
      log.removeHandler(h);
    }
    return said.size();
  }

  @Test
  void aConfigurationFilesSdkIsBuiltWithTheControlsAroundWhatItNames() {
    OpenTelemetryConfigurationModel m = Declarative.wrap(model(FILE), new Inventory());
    DeclarativeConfigResult built = DeclarativeConfiguration.create(m);
    OpenTelemetrySdk sdk = built.getSdk();
    Controls controls = Extension.INSTANCE.controls;
    try {
      String tracing = sdk.getSdkTracerProvider().toString();
      // The file's sampler, inside the live one.
      assertTrue(tracing.contains("OpampLiveSampler{ParentBased{root:AlwaysOffSampler"), tracing);
      assertTrue(tracing.contains("OpampGated{LoggingSpanExporter"), tracing);
      assertTrue(sdk.getSdkMeterProvider().toString().contains("OpampGated{LoggingMetricExporter"),
          sdk.getSdkMeterProvider().toString());
      assertTrue(sdk.getSdkLoggerProvider().toString().contains("OpampGated{SystemOutLogRecordExporter"),
          sdk.getSdkLoggerProvider().toString());

      // The file's sampler is in force (always_off) until the server sets a rate, and the gate is
      // the extension's.
      Runnable span = () -> sdk.getTracer("t").spanBuilder("s").startSpan().end();
      assertEquals(0, exported(span));
      controls.sampler.apply(dev.pydantic.opamp.sdk.SettingsProto.Settings.newBuilder().setSamplingRate(1).build());
      assertEquals(1, exported(span));
      controls.gates = new Controls.Gates(false, true, true, Controls.LogLevels.NONE, Redaction.NONE, null);
      assertEquals(0, exported(span));
      controls.gates = Controls.Gates.NONE;
      assertEquals(1, exported(span));
      controls.sampler.apply(dev.pydantic.opamp.sdk.SettingsProto.Settings.getDefaultInstance());
      assertEquals(0, exported(span));
    } finally {
      controls.sampler.apply(dev.pydantic.opamp.sdk.SettingsProto.Settings.getDefaultInstance());
      controls.gates = Controls.Gates.NONE;
      sdk.close();
    }
  }

  @Test
  void aSignalTheFileLeavesOutStaysOutAndAMissingSamplerIsTheDefault() {
    Inventory inventory = new Inventory();
    OpenTelemetryConfigurationModel m = Declarative.wrap(model("""
        file_format: "1.1"
        tracer_provider:
          processors:
            - batch:
                exporter:
                  console:
        """), inventory);
    assertNull(m.getMeterProvider());
    assertNull(m.getLoggerProvider());
    assertEquals(List.of(Inventory.exporter("traces", "console", true)), inventory.build().getExportersList());
    OpenTelemetrySdk sdk = DeclarativeConfiguration.create(m).getSdk();
    try {
      String tracing = sdk.getSdkTracerProvider().toString();
      assertTrue(tracing.contains("OpampLiveSampler{ParentBased{root:AlwaysOnSampler"), tracing);
      assertTrue(tracing.contains("OpampGated{LoggingSpanExporter"), tracing);
    } finally {
      sdk.close();
    }
  }

  @Test
  void theUidGoesIntoTheResourceAndIsKeptAcrossStarts(@TempDir Path dir) {
    Extension first = Extension.forTest(null);
    String uid = attribute(Declarative.setUp(first, config(dir), model(FILE)), "service.instance.id");
    assertEquals(7, UUID.fromString(uid).version());
    Extension second = Extension.forTest(null);
    OpenTelemetryConfigurationModel m = Declarative.setUp(second, config(dir), model(FILE));
    assertEquals(uid, attribute(m, "service.instance.id"));
    assertEquals(1, m.getResource().getAttributes().stream()
        .filter(a -> a.getName().equals("service.instance.id")).count());
    assertEquals("checkout", attribute(m, "service.name"));
    // The SDK builds the resource with it.
    OpenTelemetrySdk sdk = DeclarativeConfiguration.create(m).getSdk();
    sdk.close();
    // Without a state directory a UUID the file names is the UID; anything else is replaced.
    Extension stateless = Extension.forTest(null);
    String given = UUID.randomUUID().toString();
    Map<String, String> props = Map.of("otel.opamp.endpoint", "http://server/v1/opamp", "otel.opamp.state.dir", "none");
    assertEquals(given, attribute(Declarative.setUp(stateless, DefaultConfigProperties.createFromMap(props),
        model(FILE.replace("from-the-file", given))), "service.instance.id"));
    assertNotEquals("from-the-file", attribute(Declarative.setUp(Extension.forTest(null),
        DefaultConfigProperties.createFromMap(props), model(FILE)), "service.instance.id"));
  }

  static String attribute(OpenTelemetryConfigurationModel m, String name) {
    return m.getResource().getAttributes().stream().filter(a -> a.getName().equals(name))
        .map(AttributeNameValueModel::getValue).map(String::valueOf).findFirst().orElse(null);
  }

  @Test
  void savedSettingsAreInForceFromTheStartTheNextStartOnesWhereTheAgentReadsThem(@TempDir Path dir) throws Exception {
    Files.writeString(dir.resolve("sdk-settings.json"),
        "{\"send_logs\":\"false\",\"deactivate_instrumentations\":\"java-http-client,jdbc\","
            + "\"deactivate_all_instrumentations\":\"true\"}");
    Extension e = Extension.forTest(null);
    OpenTelemetryConfigurationModel m = Declarative.setUp(e, config(dir), model(FILE + """
        distribution:
          javaagent:
            instrumentation:
              enabled: [jdbc]
              disabled: [kafka_clients]
            indy/development: true
        """));
    assertFalse(e.controls.gates.sendLogs());
    assertTrue(e.on());
    Map<String, Object> agent = m.getDistribution().getAdditionalProperties().get("javaagent").getAdditionalProperties();
    assertEquals(Map.of("enabled", List.of("jdbc"), "disabled", List.of("kafka_clients", "java_http_client", "jdbc"),
        "default_enabled", false), agent.get("instrumentation"));
    // The rest of the distribution is the file's.
    assertEquals(true, agent.get("indy/development"));
  }

  @Test
  void withoutNextStartSettingsTheDistributionIsTheFiles(@TempDir Path dir) {
    OpenTelemetryConfigurationModel m = Declarative.setUp(Extension.forTest(null), config(dir), model(FILE));
    assertNull(m.getDistribution());
  }

  @Test
  void withoutAnEndpointTheModelIsLeftAsItIs() {
    Extension e = Extension.forTest(null);
    OpenTelemetryConfigurationModel m = Declarative.setUp(e, DefaultConfigProperties.createFromMap(Map.of()), model(FILE));
    assertFalse(e.on());
    assertEquals("from-the-file", attribute(m, "service.instance.id"));
  }

  @Test
  void anEndpointTheAgentNeverHandedTheSdkForIsWarnedAbout(@TempDir Path dir) {
    Extension e = Extension.forTest(null);
    assertEquals("OpAMP: otel.opamp.endpoint is set, but the agent built its SDK without calling the extension's "
        + "autoconfigure or declarative configuration hooks, so it won't connect to http://server/v1/opamp and "
        + "the agent runs unmanaged", e.notHooked(config(dir)));
    assertNull(e.notHooked(DefaultConfigProperties.createFromMap(Map.of())));
    e.properties(config(dir));
    assertNull(e.notHooked(config(dir)));
  }

  /** {@code own_processor}: a span processor of its own, as an application might configure. */
  public static final class OwnProcessor implements io.opentelemetry.sdk.autoconfigure.spi.internal.ComponentProvider {
    @Override
    public Class<?> getType() {
      return io.opentelemetry.sdk.trace.SpanProcessor.class;
    }

    @Override
    public String getName() {
      return "own_processor";
    }

    @Override
    public Object create(io.opentelemetry.api.incubator.config.DeclarativeConfigProperties config) {
      return io.opentelemetry.sdk.trace.SpanProcessor.composite();
    }
  }

  @Test
  void aSignalThroughAProcessorOfItsOwnHasNoneOfItsSettingsCarriedOut(@TempDir Path dir) throws Exception {
    // Saved before: send_traces false. The file's spans go through a processor of its own, past the
    // gate, so the agent can't stop them: neither in force nor answered as done.
    Files.writeString(dir.resolve("sdk-settings.json"), "{\"send_traces\":\"false\",\"send_metrics\":\"false\"}");
    System.setProperty("otel.opamp.endpoint", "http://server/v1/opamp");
    System.setProperty("otel.opamp.state.dir", dir.toString());
    Extension e = Extension.INSTANCE;
    OpenTelemetrySdk sdk = null;
    try {
      sdk = DeclarativeConfiguration.create(model(FILE.replace("""
            processors:
              - simple:
                  exporter:
                    console:
          meter_provider:""", """
            processors:
              - own_processor:
          meter_provider:"""))).getSdk();
      assertTrue(e.on());
      assertTrue(e.controls.gates.sendTraces());
      assertFalse(e.controls.gates.sendMetrics()); // the reader is the agent's: gated
      assertEquals("{\"send_metrics\":\"false\"}", ExtensionTest.effective(e));
      e.remoteConfig(ExtensionTest.config("elastic", "{\"send_traces\":\"false\"}"));
      assertTrue(e.controls.gates.sendTraces());
      assertEquals("{}", ExtensionTest.effective(e));
    } finally {
      System.clearProperty("otel.opamp.endpoint");
      System.clearProperty("otel.opamp.state.dir");
      e.controls.gates = Controls.Gates.NONE;
      e.off();
      if (sdk != null) {
        sdk.close();
      }
    }
  }

  @Test
  void theSdkRunsTheExtensionsHooksFromAConfigurationFile(@TempDir Path dir) throws Exception {
    // Through the SPI, as in the agent: the customizers find the endpoint in system properties.
    Files.writeString(dir.resolve("sdk-settings.json"), "{\"send_metrics\":\"false\"}");
    System.setProperty("otel.opamp.endpoint", "http://server/v1/opamp");
    System.setProperty("otel.opamp.state.dir", dir.toString());
    Extension e = Extension.INSTANCE;
    OpenTelemetrySdk sdk = null;
    try {
      DeclarativeConfigResult built = DeclarativeConfiguration.create(model(FILE));
      sdk = built.getSdk();
      assertTrue(e.on());
      assertFalse(e.controls.gates.sendMetrics());
      String uid = built.getResource().getAttribute(AttributeKey.stringKey("service.instance.id"));
      assertEquals(Files.readString(dir.resolve("instance-uid")).trim(), uid);
      assertTrue(sdk.getSdkTracerProvider().toString().contains("OpampLiveSampler"));
      assertTrue(sdk.getSdkTracerProvider().toString().contains("OpampGated{LoggingSpanExporter"));
      assertNull(e.notHooked(config(dir)));
      // The SDK as built samples with the extension's sampler, around the file's: nothing set aside.
      var declared = e.host();
      e.built(sdk);
      assertEquals(declared, e.host());
    } finally {
      System.clearProperty("otel.opamp.endpoint");
      System.clearProperty("otel.opamp.state.dir");
      e.controls.gates = Controls.Gates.NONE;
      e.off();
      if (sdk != null) {
        sdk.close();
      }
    }
  }
}
