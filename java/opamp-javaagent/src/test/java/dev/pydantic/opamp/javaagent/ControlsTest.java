// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import static dev.pydantic.opamp.javaagent.Vectors.exports;
import static dev.pydantic.opamp.javaagent.Vectors.override;
import static dev.pydantic.opamp.javaagent.Vectors.v1;
import static dev.pydantic.opamp.javaagent.Vectors.v2;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import org.junit.jupiter.api.Test;

class ControlsTest {
  /** A policy's settings, read by the shared layer, every setting declared. */
  static dev.pydantic.opamp.sdk.SettingsProto.Settings settings(String json) {
    dev.pydantic.opamp.sdk.SettingsProto.Loaded l =
        Vectors.LAYER.load(json.getBytes(java.nio.charset.StandardCharsets.UTF_8), Vectors.ALL, Vectors.NOW);
    assertEquals("", l.getWarning());
    return l.getSettings();
  }

  @Test
  void nextStartSettingsBecomeAgentProperties() {
    assertEquals(java.util.Map.of(
        "otel.instrumentation.jdbc.enabled", "false",
        "otel.instrumentation.kafka-clients.enabled", "false",
        "otel.instrumentation.common.default-enabled", "false"),
        Controls.startProperties(settings(
            v2("instrumentations", override("\"disabled\":[\"jdbc\",\"kafka-clients\"],\"all_disabled\":true")))));
    assertEquals(java.util.Map.of(), Controls.startProperties(settings(v2("instrumentations", override("\"all_disabled\":false")))));
    assertEquals(java.util.Map.of(), Controls.startProperties(settings(v1())));
    // The agent reads `-` and `.` as the same in property names, not `_`.
    assertEquals(java.util.Map.of("otel.instrumentation.spring-webmvc-6.0.enabled", "false"),
        Controls.startProperties(settings(v2("instrumentations", override("\"disabled\":[\"spring_webmvc-6.0\"]")))));
  }

  @Test
  void theAgentsSamplerUntilARateIsSetAndAgainWhenItIsWithdrawn() {
    Controls c = new Controls();
    Sampler agents = Sampler.alwaysOff();
    c.sampler.wrap(agents);
    assertSame(agents, c.sampler.current());
    c.apply(settings(v1("head_sampling", override("\"ratio\":0.25"))));
    assertEquals(Sampler.parentBased(Sampler.traceIdRatioBased(0.25)).getDescription(),
        c.sampler.current().getDescription());
    c.apply(settings(v1()));
    assertSame(agents, c.sampler.current());
  }

  @Test
  void aRateSavedBeforeTheAgentBuildsItsSamplerStillHolds() {
    Controls c = new Controls();
    c.apply(settings(v1("head_sampling", override("\"ratio\":0"))));
    c.sampler.wrap(Sampler.alwaysOn());
    assertEquals(Sampler.parentBased(Sampler.traceIdRatioBased(0)).getDescription(),
        c.sampler.current().getDescription());
  }

  @Test
  void spansStopAndStartWithSendTraces() {
    Controls c = new Controls();
    InMemorySpanExporter out = InMemorySpanExporter.create();
    SdkTracerProvider tp = SdkTracerProvider.builder()
        .setSampler(c.sampler.wrap(Sampler.alwaysOn()))
        .addSpanProcessor(SimpleSpanProcessor.create(c.spans(out))).build();
    var tracer = OpenTelemetrySdk.builder().setTracerProvider(tp).build().getTracer("t");
    tracer.spanBuilder("one").startSpan().end();
    c.apply(settings(v1("signal_exports", exports("off", "", ""))));
    tracer.spanBuilder("dropped").startSpan().end();
    c.apply(settings(v1("signal_exports", exports("on", "", ""))));
    tracer.spanBuilder("two").startSpan().end();
    assertEquals(java.util.List.of("one", "two"), out.getFinishedSpanItems().stream().map(s -> s.getName()).toList());
  }

  @Test
  void aPullReaderServesNothingWhileMetricsAreNotSent() {
    Controls c = new Controls();
    io.opentelemetry.sdk.testing.exporter.InMemoryMetricReader pull =
        io.opentelemetry.sdk.testing.exporter.InMemoryMetricReader.create();
    io.opentelemetry.sdk.metrics.SdkMeterProvider mp = io.opentelemetry.sdk.metrics.SdkMeterProvider.builder()
        .registerMetricReader(c.reader(pull)).build();
    mp.get("t").counterBuilder("c").build().add(1);
    assertEquals(1, pull.collectAllMetrics().size());
    c.apply(settings(v1("signal_exports", exports("", "off", ""))));
    assertTrue(pull.collectAllMetrics().isEmpty());
    c.apply(settings(v1()));
    assertEquals(1, pull.collectAllMetrics().size());
  }

  @Test
  void aPeriodicReaderIsLeftAsItIsItsExporterIsGated() {
    var periodic = io.opentelemetry.sdk.metrics.export.PeriodicMetricReader.create(
        io.opentelemetry.sdk.testing.exporter.InMemoryMetricExporter.create());
    assertSame(periodic, new Controls().reader(periodic));
  }

  @Test
  void aRateOf0SamplesNoNewTrace() {
    Controls c = new Controls();
    InMemorySpanExporter out = InMemorySpanExporter.create();
    SdkTracerProvider tp = SdkTracerProvider.builder()
        .setSampler(c.sampler.wrap(Sampler.alwaysOn()))
        .addSpanProcessor(SimpleSpanProcessor.create(c.spans(out))).build();
    var tracer = tp.get("t");
    c.apply(settings(v1("head_sampling", override("\"ratio\":0"))));
    for (int i = 0; i < 100; i++) {
      tracer.spanBuilder("s").startSpan().end();
    }
    assertTrue(out.getFinishedSpanItems().isEmpty());
    c.apply(settings(v1("head_sampling", override("\"ratio\":1"))));
    tracer.spanBuilder("s").startSpan().end();
    assertEquals(1, out.getFinishedSpanItems().size());
  }

  @Test
  void logsBelowTheLevelAreDroppedLiveAndComeBackWhenItIsWithdrawn() {
    Controls c = new Controls();
    var out = io.opentelemetry.sdk.testing.exporter.InMemoryLogRecordExporter.create();
    var lp = io.opentelemetry.sdk.logs.SdkLoggerProvider.builder()
        .addLogRecordProcessor(io.opentelemetry.sdk.logs.export.SimpleLogRecordProcessor.create(c.logs(out)))
        .build();
    var logger = lp.get("t");
    java.util.function.BiConsumer<String, io.opentelemetry.api.logs.Severity> emit =
        (body, sev) -> logger.logRecordBuilder().setBody(body).setSeverity(sev).emit();
    java.util.function.Supplier<java.util.List<String>> bodies = () -> {
      var b = out.getFinishedLogRecordItems().stream().map(r -> r.getBodyValue().asString()).toList();
      out.reset();
      return b;
    };
    var debug = io.opentelemetry.api.logs.Severity.DEBUG;
    var info = io.opentelemetry.api.logs.Severity.INFO;
    var warn = io.opentelemetry.api.logs.Severity.WARN;
    var warn2 = io.opentelemetry.api.logs.Severity.WARN2;
    var error = io.opentelemetry.api.logs.Severity.ERROR;
    var none = io.opentelemetry.api.logs.Severity.UNDEFINED_SEVERITY_NUMBER;

    emit.accept("debug", debug);
    assertEquals(java.util.List.of("debug"), bodies.get());

    c.apply(settings(v2("log_levels", override("\"level\":\"warn\""))));
    emit.accept("debug", debug);
    emit.accept("info", info);
    emit.accept("warn", warn);
    emit.accept("warn2", warn2);
    emit.accept("error", error);
    emit.accept("none", none);
    assertEquals(java.util.List.of("warn", "warn2", "error", "none"), bodies.get());

    // send_logs: false still drops everything.
    c.apply(settings(v2("signal_exports", exports("", "", "off"), "log_levels", override("\"level\":\"warn\""))));
    emit.accept("error", error);
    emit.accept("none", none);
    assertTrue(bodies.get().isEmpty());

    c.apply(settings(v1()));
    emit.accept("debug", debug);
    emit.accept("none", none);
    assertEquals(java.util.List.of("debug", "none"), bodies.get());
  }

  @Test
  void eachScopeHasItsOwnLevelLiveAndTheGlobalLevelForTheRest() {
    Controls c = new Controls();
    var out = io.opentelemetry.sdk.testing.exporter.InMemoryLogRecordExporter.create();
    var lp = io.opentelemetry.sdk.logs.SdkLoggerProvider.builder()
        .addLogRecordProcessor(io.opentelemetry.sdk.logs.export.SimpleLogRecordProcessor.create(c.logs(out)))
        .build();
    java.util.function.Supplier<java.util.List<String>> exported = () -> {
      var b = out.getFinishedLogRecordItems().stream()
          .map(r -> r.getInstrumentationScopeInfo().getName() + ":" + r.getSeverity().name()).toList();
      out.reset();
      return b;
    };
    java.util.List<String> scopes = java.util.List.of("com.acme.db", "com.acme.dbpool", "noisy.lib", "app");
    Runnable emitAll = () -> {
      for (String scope : scopes) {
        for (var sev : java.util.List.of(io.opentelemetry.api.logs.Severity.DEBUG,
            io.opentelemetry.api.logs.Severity.WARN, io.opentelemetry.api.logs.Severity.UNDEFINED_SEVERITY_NUMBER)) {
          lp.get(scope).logRecordBuilder().setSeverity(sev).setBody("x").emit();
        }
      }
    };

    // A scope's level is lower than the global one (debug under warn), off drops severity 0 too.
    c.apply(settings(v2("log_levels", override("\"level\":\"warn\",\"scopes\":{\"com.acme.db*\":\"debug\",\"noisy.lib\":\"off\"}"))));
    emitAll.run();
    assertEquals(java.util.List.of(
        "com.acme.db:DEBUG", "com.acme.db:WARN", "com.acme.db:UNDEFINED_SEVERITY_NUMBER",
        "com.acme.dbpool:DEBUG", "com.acme.dbpool:WARN", "com.acme.dbpool:UNDEFINED_SEVERITY_NUMBER",
        "app:WARN", "app:UNDEFINED_SEVERITY_NUMBER"), exported.get());

    // Changed live: an exact entry beats a prefix of the same name; no global level now.
    c.apply(settings(v2("log_levels", override("\"scopes\":{\"com.acme.db*\":\"error\",\"com.acme.db\":\"debug\",\"noisy.lib\":\"warn\"}"))));
    emitAll.run();
    assertEquals(java.util.List.of(
        "com.acme.db:DEBUG", "com.acme.db:WARN", "com.acme.db:UNDEFINED_SEVERITY_NUMBER",
        "com.acme.dbpool:UNDEFINED_SEVERITY_NUMBER",
        "noisy.lib:WARN", "noisy.lib:UNDEFINED_SEVERITY_NUMBER",
        "app:DEBUG", "app:WARN", "app:UNDEFINED_SEVERITY_NUMBER"), exported.get());

    // send_logs false still drops everything, whatever a scope's level.
    c.apply(settings(v2("signal_exports", exports("", "", "off"), "log_levels", override("\"scopes\":{\"*\":\"trace\"}"))));
    emitAll.run();
    assertTrue(exported.get().isEmpty());

    // Withdrawn: every record again.
    c.apply(settings(v1()));
    emitAll.run();
    assertEquals(12, exported.get().size());
  }

  @Test
  void theLogCapKeepsEachBatchsFirstRecordsRefillsChangesLiveAndIsRemovedAtOnce() {
    Controls c = new Controls();
    long[] now = {0};
    c.nanoTime = () -> now[0];
    var out = io.opentelemetry.sdk.testing.exporter.InMemoryLogRecordExporter.create();
    var gate = c.logs(out);
    java.util.function.IntFunction<java.util.List<String>> batch = n -> {
      java.util.List<io.opentelemetry.sdk.logs.data.LogRecordData> records = new java.util.ArrayList<>();
      for (int i = 0; i < n; i++) {
        records.add(io.opentelemetry.sdk.testing.logs.TestLogRecordData.builder().setBody("r" + i)
            .setInstrumentationScopeInfo(io.opentelemetry.sdk.common.InstrumentationScopeInfo.create(i % 2 == 0 ? "app" : "noisy"))
            .setSeverity(i % 2 == 0 ? io.opentelemetry.api.logs.Severity.INFO : io.opentelemetry.api.logs.Severity.DEBUG)
            .build());
      }
      out.reset();
      gate.export(records);
      return out.getFinishedLogRecordItems().stream().map(r -> r.getBodyValue().asString()).toList();
    };

    c.apply(settings(v2("log_rate_limit", override("\"records_per_second\":3"))));
    assertEquals(java.util.List.of("r0", "r1", "r2"), batch.apply(5));
    assertTrue(batch.apply(5).isEmpty());
    now[0] = 500_000_000L; // half a second: 1.5 tokens, so one record
    assertEquals(java.util.List.of("r0"), batch.apply(5));
    // The same cap again (a re-sent config) keeps the bucket; 0.5 token left.
    c.apply(settings(v2("log_rate_limit", override("\"records_per_second\":3"))));
    assertTrue(batch.apply(1).isEmpty());
    // A new cap starts a full bucket.
    c.apply(settings(v2("log_rate_limit", override("\"records_per_second\":2"))));
    assertEquals(java.util.List.of("r0", "r1"), batch.apply(5));
    // Records the level drops take no tokens: only the info ones (even numbers) count.
    c.apply(settings(v2("log_levels", override("\"scopes\":{\"noisy\":\"off\"}"), "log_rate_limit", override("\"records_per_second\":2"))));
    now[0] += 1_000_000_000L;
    assertEquals(java.util.List.of("r0", "r2"), batch.apply(8));
    // Removed: every record at once.
    c.apply(settings(v1()));
    assertEquals(8, batch.apply(8).size());
  }

  @Test
  void redactionChangesWhatIsExportedNotTheApplicationsSpansOrRecords() {
    Controls c = new Controls();
    var gated = InMemorySpanExporter.create();
    var plain = InMemorySpanExporter.create();
    var resource = io.opentelemetry.sdk.resources.Resource.create(io.opentelemetry.api.common.Attributes.of(
        io.opentelemetry.api.common.AttributeKey.stringKey("token.resource"), "kept"));
    SdkTracerProvider tp = SdkTracerProvider.builder().setResource(resource)
        .setSpanLimits(io.opentelemetry.sdk.trace.SpanLimits.builder().setMaxNumberOfAttributes(3).build())
        .addSpanProcessor(SimpleSpanProcessor.create(c.spans(gated)))
        .addSpanProcessor(SimpleSpanProcessor.create(plain)).build();
    var tracer = tp.get("token.scope");
    Runnable span = () -> {
      var other = tracer.spanBuilder("linked").startSpan();
      tracer.spanBuilder("secret token span")
          .addLink(other.getSpanContext(), io.opentelemetry.api.common.Attributes.of(
              io.opentelemetry.api.common.AttributeKey.stringKey("Authorization"), "Bearer l"))
          .setAttribute("http.request.header.authorization", "Bearer x")
          .setAttribute("card.number", 4111111111111111L)
          .setAttribute("user.id", "7")
          .setAttribute("over.the.limit", "dropped by the SDK")
          .startSpan()
          .addEvent("login", io.opentelemetry.api.common.Attributes.of(
              io.opentelemetry.api.common.AttributeKey.stringKey("user.password"), "p",
              io.opentelemetry.api.common.AttributeKey.stringKey("ok"), "yes"))
          .end();
    };

    c.apply(settings(v2("attribute_redaction", override("\"drop\":[\"http.request.header.*\",\"AUTHORIZATION\"],\"mask\":[\"card.number\",\"*password*\",\"*token*\"]"))));
    span.run();
    var out = gated.getFinishedSpanItems().get(0);
    var orig = plain.getFinishedSpanItems().get(0);
    assertEquals(io.opentelemetry.api.common.Attributes.builder().put("card.number", "[REDACTED]").put("user.id", "7").build(),
        out.getAttributes());
    // The SDK's own limit dropped one; the redaction's drop isn't counted with it.
    assertEquals(1, orig.getTotalAttributeCount() - orig.getAttributes().size());
    assertEquals(1, out.getTotalAttributeCount() - out.getAttributes().size());
    assertEquals(io.opentelemetry.api.common.Attributes.builder().put("user.password", "[REDACTED]").put("ok", "yes").build(),
        out.getEvents().get(0).getAttributes());
    assertTrue(out.getLinks().get(0).getAttributes().isEmpty());
    assertEquals(0, out.getLinks().get(0).getTotalAttributeCount());
    // Names, the resource and the scope are left alone; so are the trace and span ids.
    assertEquals("secret token span", out.getName());
    assertEquals(resource, out.getResource());
    assertEquals("token.scope", out.getInstrumentationScopeInfo().getName());
    assertEquals(orig.getSpanContext(), out.getSpanContext());
    // Another exporter, one the gate doesn't cover, gets the original.
    assertEquals(4111111111111111L, orig.getAttributes().get(io.opentelemetry.api.common.AttributeKey.longKey("card.number")));
    assertEquals("Bearer x", orig.getAttributes().get(
        io.opentelemetry.api.common.AttributeKey.stringKey("http.request.header.authorization")));
    assertEquals("p", orig.getEvents().get(0).getAttributes().get(
        io.opentelemetry.api.common.AttributeKey.stringKey("user.password")));

    // Changed live, then withdrawn: as the application made it.
    c.apply(settings(v2("attribute_redaction", override("\"mask\":[\"user.id\"]"))));
    gated.reset();
    span.run();
    assertEquals("[REDACTED]", gated.getFinishedSpanItems().get(0).getAttributes().get(
        io.opentelemetry.api.common.AttributeKey.stringKey("user.id")));
    assertEquals("Bearer x", gated.getFinishedSpanItems().get(0).getAttributes().get(
        io.opentelemetry.api.common.AttributeKey.stringKey("http.request.header.authorization")));
    c.apply(settings(v1()));
    gated.reset();
    plain.reset();
    span.run();
    assertSame(plain.getFinishedSpanItems().get(0).getAttributes().getClass(),
        gated.getFinishedSpanItems().get(0).getAttributes().getClass());
    assertEquals(plain.getFinishedSpanItems().get(0).getAttributes(), gated.getFinishedSpanItems().get(0).getAttributes());
  }

  @Test
  void redactionOfLogRecordsLeavesTheBodyAndTheApplicationsRecord() {
    Controls c = new Controls();
    var gated = io.opentelemetry.sdk.testing.exporter.InMemoryLogRecordExporter.create();
    var plain = io.opentelemetry.sdk.testing.exporter.InMemoryLogRecordExporter.create();
    var lp = io.opentelemetry.sdk.logs.SdkLoggerProvider.builder()
        .addLogRecordProcessor(io.opentelemetry.sdk.logs.export.SimpleLogRecordProcessor.create(c.logs(gated)))
        .addLogRecordProcessor(io.opentelemetry.sdk.logs.export.SimpleLogRecordProcessor.create(plain)).build();
    c.apply(settings(v2("attribute_redaction", override("\"drop\":[\"session.*\"],\"mask\":[\"user.email\"]"))));
    lp.get("app").logRecordBuilder().setBody("user.email=a@b.c logged in").setSeverity(io.opentelemetry.api.logs.Severity.INFO)
        .setAttribute(io.opentelemetry.api.common.AttributeKey.stringKey("user.email"), "a@b.c")
        .setAttribute(io.opentelemetry.api.common.AttributeKey.stringKey("Session.Id"), "s1")
        .setAttribute(io.opentelemetry.api.common.AttributeKey.longKey("n"), 1L).emit();
    var out = gated.getFinishedLogRecordItems().get(0);
    assertEquals(io.opentelemetry.api.common.Attributes.builder().put("user.email", "[REDACTED]").put("n", 1L).build(),
        out.getAttributes());
    assertEquals(2, out.getTotalAttributeCount());
    assertEquals("user.email=a@b.c logged in", out.getBodyValue().asString());
    assertEquals(io.opentelemetry.api.logs.Severity.INFO, out.getSeverity());
    var orig = plain.getFinishedLogRecordItems().get(0);
    assertEquals(3, orig.getAttributes().size());
    assertEquals(orig.getTimestampEpochNanos(), out.getTimestampEpochNanos());
    assertEquals(orig.getInstrumentationScopeInfo(), out.getInstrumentationScopeInfo());
    // A record nothing matches is passed on as it is.
    gated.reset();
    lp.get("app").logRecordBuilder().setBody("plain").setAttribute(
        io.opentelemetry.api.common.AttributeKey.stringKey("k"), "v").emit();
    assertTrue(gated.getFinishedLogRecordItems().get(0).getClass().getName().contains("SdkLogRecordData"));
  }

  /** A tracer over the controls, exporting through the span gate. */
  static io.opentelemetry.api.trace.Tracer tracer(Controls c, Sampler agents, InMemorySpanExporter out) {
    return SdkTracerProvider.builder().setSampler(c.sampler.wrap(agents))
        .addSpanProcessor(SimpleSpanProcessor.create(c.spans(out))).build().get("t");
  }

  @Test
  void rulesDecideNewTracesByPathKindAndNameAndTheirChildrenFollow() {
    Controls c = new Controls();
    InMemorySpanExporter out = InMemorySpanExporter.create();
    var tracer = tracer(c, Sampler.alwaysOff(), out);
    java.util.function.BiConsumer<String, String> request = (name, path) -> {
      var server = tracer.spanBuilder(name).setSpanKind(io.opentelemetry.api.trace.SpanKind.SERVER)
          .setAttribute("url.path", path).startSpan();
      try (var scope = server.makeCurrent()) {
        tracer.spanBuilder("child of " + path).startSpan().end();
      }
      server.end();
    };
    java.util.function.Supplier<java.util.List<String>> names = () -> {
      var n = out.getFinishedSpanItems().stream().map(s -> s.getName()).toList();
      out.reset();
      return n;
    };

    c.apply(settings(v2("head_sampling", override("\"ratio\":1"), "sampling_rules", override("\"rules\":[{\"rate\":0,\"kind\":\"server\",\"path\":\"/health*\"},{\"rate\":1,\"name\":\"checkout *\"}]"))));
    request.accept("GET", "/healthz");
    request.accept("GET", "/orders");
    // Rate 0 on the root: its child isn't sampled either; the trace is whole or not at all.
    assertEquals(java.util.List.of("child of /orders", "GET"), names.get());

    // A sampled caller wins over a rule of rate 0, and an unsampled one over a rule of rate 1.
    var sampled = io.opentelemetry.context.Context.root().with(io.opentelemetry.api.trace.Span.wrap(
        io.opentelemetry.api.trace.SpanContext.createFromRemoteParent(VectorsTest.TRACE_ID, VectorsTest.SPAN_ID,
            io.opentelemetry.api.trace.TraceFlags.getSampled(), io.opentelemetry.api.trace.TraceState.getDefault())));
    tracer.spanBuilder("GET").setParent(sampled).setSpanKind(io.opentelemetry.api.trace.SpanKind.SERVER)
        .setAttribute("url.path", "/health").startSpan().end();
    assertEquals(java.util.List.of("GET"), names.get());

    // http.target, its query removed, when there is no url.path.
    c.apply(settings(v2("head_sampling", override("\"ratio\":1"), "sampling_rules", override("\"rules\":[{\"rate\":0,\"path\":\"/health\"}]"))));
    tracer.spanBuilder("GET").setAttribute("http.target", "/health?verbose=1").startSpan().end();
    tracer.spanBuilder("GET").setAttribute("http.target", "/healthy").startSpan().end();
    assertEquals(1, names.get().size());

    // No rule matches and no sampling_rate: the agent's own sampler (always_off here).
    c.apply(settings(v2("sampling_rules", override("\"rules\":[{\"rate\":1,\"name\":\"checkout *\"}]"))));
    tracer.spanBuilder("checkout cart").startSpan().end();
    tracer.spanBuilder("GET").startSpan().end();
    assertEquals(java.util.List.of("checkout cart"), names.get());

    // Rules withdrawn: the agent's own sampler again, for everything.
    c.apply(settings(v1()));
    tracer.spanBuilder("checkout cart").startSpan().end();
    assertTrue(names.get().isEmpty());
    assertSame(Sampler.alwaysOff(), c.sampler.current());
  }

  @Test
  void aRuleOfRateOneHalfSamplesHalfOfTenThousandTraces() {
    Controls c = new Controls();
    c.sampler.wrap(Sampler.alwaysOff());
    c.apply(settings(v2("sampling_rules", override("\"rules\":[{\"rate\":0.5,\"kind\":\"client\"}]"))));
    var ids = io.opentelemetry.sdk.trace.IdGenerator.random();
    int sampled = 0;
    for (int i = 0; i < 10_000; i++) {
      if (c.sampler.shouldSample(io.opentelemetry.context.Context.root(), ids.generateTraceId(), "GET",
          io.opentelemetry.api.trace.SpanKind.CLIENT, io.opentelemetry.api.common.Attributes.empty(), java.util.List.of())
          .getDecision() == io.opentelemetry.sdk.trace.samplers.SamplingDecision.RECORD_AND_SAMPLE) {
        sampled++;
      }
    }
    assertTrue(Math.abs(sampled - 5_000) <= 300, sampled + " of 10000");
  }

  @Test
  void theTraceCapRefusesWholeTracesNotTheirChildren() {
    Controls c = new Controls();
    long[] now = {0};
    c.nanoTime = () -> now[0];
    InMemorySpanExporter out = InMemorySpanExporter.create();
    var tracer = tracer(c, Sampler.parentBased(Sampler.alwaysOn()), out);
    Runnable trace = () -> {
      var root = tracer.spanBuilder("root").startSpan();
      try (var scope = root.makeCurrent()) {
        for (int i = 0; i < 3; i++) {
          tracer.spanBuilder("child").startSpan().end();
        }
      }
      root.end();
    };
    java.util.function.IntSupplier traces = () -> {
      int n = (int) out.getFinishedSpanItems().stream().map(s -> s.getTraceId()).distinct().count();
      int spans = out.getFinishedSpanItems().size();
      assertEquals(4 * n, spans);
      out.reset();
      return n;
    };

    c.apply(settings(v2("trace_rate_limit", override("\"traces_per_second\":2"))));
    for (int i = 0; i < 5; i++) {
      trace.run();
    }
    assertEquals(2, traces.getAsInt());
    now[0] += 500_000_000L; // one token back
    for (int i = 0; i < 5; i++) {
      trace.run();
    }
    assertEquals(1, traces.getAsInt());
    // A sampled remote parent takes a token too: the cap holds behind a traced caller.
    now[0] += 1_000_000_000L;
    var caller = io.opentelemetry.context.Context.root().with(io.opentelemetry.api.trace.Span.wrap(
        io.opentelemetry.api.trace.SpanContext.createFromRemoteParent(VectorsTest.TRACE_ID, VectorsTest.SPAN_ID,
            io.opentelemetry.api.trace.TraceFlags.getSampled(), io.opentelemetry.api.trace.TraceState.getDefault())));
    for (int i = 0; i < 4; i++) {
      tracer.spanBuilder("server").setParent(caller).startSpan().end();
    }
    assertEquals(2, out.getFinishedSpanItems().size());
    out.reset();
    // With a rule of rate 0 for some, only those sampled take tokens.
    now[0] += 1_000_000_000L;
    c.apply(settings(v2("sampling_rules", override("\"rules\":[{\"rate\":0,\"name\":\"noise\"}]"), "trace_rate_limit", override("\"traces_per_second\":2"))));
    for (int i = 0; i < 5; i++) {
      tracer.spanBuilder("noise").startSpan().end();
    }
    trace.run();
    trace.run();
    assertEquals(2, traces.getAsInt());
    // Removed: no cap at once.
    c.apply(settings(v1()));
    for (int i = 0; i < 5; i++) {
      trace.run();
    }
    assertEquals(5, traces.getAsInt());
  }

  /** A span exporter whose next result the test chooses. */
  static final class Scripted implements io.opentelemetry.sdk.trace.export.SpanExporter {
    io.opentelemetry.sdk.common.CompletableResultCode next = io.opentelemetry.sdk.common.CompletableResultCode.ofSuccess();

    @Override
    public io.opentelemetry.sdk.common.CompletableResultCode export(
        java.util.Collection<io.opentelemetry.sdk.trace.data.SpanData> spans) {
      return next;
    }

    @Override
    public io.opentelemetry.sdk.common.CompletableResultCode flush() {
      return io.opentelemetry.sdk.common.CompletableResultCode.ofSuccess();
    }

    @Override
    public io.opentelemetry.sdk.common.CompletableResultCode shutdown() {
      return io.opentelemetry.sdk.common.CompletableResultCode.ofSuccess();
    }
  }

  @Test
  void theGatesCountExportsAfterTheExportersRetriesAndWhatTheyDropOnPurpose() {
    Controls c = new Controls();
    Scripted exporter = new Scripted();
    var gate = c.spans(exporter);
    var span = io.opentelemetry.sdk.testing.trace.TestSpanData.builder().setName("s")
        .setKind(io.opentelemetry.api.trace.SpanKind.INTERNAL).setStartEpochNanos(1).setEndEpochNanos(2)
        .setHasEnded(true).setStatus(io.opentelemetry.sdk.trace.data.StatusData.unset()).build();
    gate.export(java.util.List.of(span, span));
    exporter.next = io.opentelemetry.sdk.common.CompletableResultCode.ofExceptionalFailure(
        new java.io.IOException("connection refused"));
    gate.export(java.util.List.of(span, span, span));
    var t = c.counts().getTraces();
    assertEquals(1, t.getExportsOk());
    assertEquals(1, t.getExportsFailed());
    assertEquals(3, t.getItemsFailed());
    assertTrue(t.getLastFailed());
    assertEquals("connection refused", t.getLastError());
    // A result that comes later is counted when it comes; a failure with no error has no text.
    var pending = new io.opentelemetry.sdk.common.CompletableResultCode();
    exporter.next = pending;
    gate.export(java.util.List.of(span));
    assertEquals(1, c.counts().getTraces().getExportsFailed());
    pending.fail();
    assertEquals(2, c.counts().getTraces().getExportsFailed());
    assertEquals("", c.counts().getTraces().getLastError());
    // The next success clears the failure.
    exporter.next = io.opentelemetry.sdk.common.CompletableResultCode.ofSuccess();
    gate.export(java.util.List.of(span));
    assertFalse(c.counts().getTraces().getLastFailed());
    // send_traces false: dropped on purpose, not exported.
    c.apply(settings(v1("signal_exports", exports("off", "", ""))));
    gate.export(java.util.List.of(span, span));
    assertEquals(2, c.counts().getTraces().getItemsFiltered());
    assertEquals(2, c.counts().getTraces().getExportsOk());

    // Logs: the levels and the cap count as filtered; redaction drops attributes, not records.
    var logs = c.logs(io.opentelemetry.sdk.testing.exporter.InMemoryLogRecordExporter.create());
    c.apply(settings(v2("log_levels", override("\"level\":\"warn\""), "log_rate_limit", override("\"records_per_second\":1"), "attribute_redaction", override("\"drop\":[\"*\"]"))));
    c.nanoTime = () -> 0;
    java.util.function.Function<io.opentelemetry.api.logs.Severity, io.opentelemetry.sdk.logs.data.LogRecordData> rec =
        sev -> io.opentelemetry.sdk.testing.logs.TestLogRecordData.builder().setSeverity(sev).setBody("b")
            .setAttributes(io.opentelemetry.api.common.Attributes.of(
                io.opentelemetry.api.common.AttributeKey.stringKey("k"), "v")).build();
    logs.export(java.util.List.of(rec.apply(io.opentelemetry.api.logs.Severity.DEBUG),
        rec.apply(io.opentelemetry.api.logs.Severity.ERROR), rec.apply(io.opentelemetry.api.logs.Severity.ERROR)));
    var l = c.counts().getLogs();
    assertEquals(2, l.getItemsFiltered());
    assertEquals(1, l.getExportsOk());

    // Metrics: points, not metrics.
    var metricsOut = io.opentelemetry.sdk.testing.exporter.InMemoryMetricExporter.create();
    var mp = io.opentelemetry.sdk.metrics.SdkMeterProvider.builder().registerMetricReader(
        io.opentelemetry.sdk.metrics.export.PeriodicMetricReader.create(c.metrics(metricsOut))).build();
    var counter = mp.get("m").counterBuilder("c").build();
    counter.add(1, io.opentelemetry.api.common.Attributes.of(io.opentelemetry.api.common.AttributeKey.stringKey("a"), "1"));
    counter.add(1, io.opentelemetry.api.common.Attributes.of(io.opentelemetry.api.common.AttributeKey.stringKey("a"), "2"));
    c.apply(settings(v1("signal_exports", exports("", "off", ""))));
    mp.forceFlush().join(5, java.util.concurrent.TimeUnit.SECONDS);
    assertEquals(2, c.counts().getMetrics().getItemsFiltered());
    mp.close();
  }

  /**
   * Flips the controls between a policy that switches the signal's export off and one that switches
   * it on and drops {@code *password*}, on one thread, from the first,
   * while this one exports with {@code export} as fast as it can, for two seconds or until it says
   * user.password reached the exporter: which neither document would let through, but a gate that
   * read the second's send flag beside the first's redaction would. Whether it did.
   */
  static boolean aPasswordGetsThroughWhileFlipping(Controls c, String signal,
      java.util.function.BooleanSupplier export) throws InterruptedException {
    boolean traces = signal.equals("traces");
    var off = settings(v1("signal_exports", traces ? exports("off", "", "") : exports("", "", "off")));
    var on = settings(v2("signal_exports", traces ? exports("on", "", "") : exports("", "", "on"),
        "attribute_redaction", override("\"drop\":[\"*password*\"]")));
    c.apply(off);
    var stop = new java.util.concurrent.atomic.AtomicBoolean();
    Thread applier = new Thread(() -> {
      while (!stop.get()) {
        c.apply(on);
        c.apply(off);
      }
    }, "flipper");
    applier.start();
    boolean leaked = false;
    try {
      long end = System.nanoTime() + java.util.concurrent.TimeUnit.SECONDS.toNanos(2);
      while (!leaked && System.nanoTime() < end) {
        leaked = export.getAsBoolean();
      }
    } finally {
      stop.set(true);
      applier.join();
    }
    return leaked;
  }

  private static final io.opentelemetry.api.common.AttributeKey<String> PASSWORD_KEY =
      io.opentelemetry.api.common.AttributeKey.stringKey("user.password");
  private static final io.opentelemetry.api.common.Attributes PASSWORD =
      io.opentelemetry.api.common.Attributes.of(PASSWORD_KEY, "hunter2");

  @Test
  void aSpanExportedWhileTheSettingsChangeIsRedactedAsTheDocumentThatSentItSays() throws Exception {
    Controls c = new Controls();
    var leaked = new java.util.concurrent.atomic.AtomicBoolean();
    var gate = c.spans(new io.opentelemetry.sdk.trace.export.SpanExporter() {
      @Override
      public io.opentelemetry.sdk.common.CompletableResultCode export(
          java.util.Collection<io.opentelemetry.sdk.trace.data.SpanData> spans) {
        if (spans.stream().anyMatch(s -> s.getAttributes().get(PASSWORD_KEY) != null)) {
          leaked.set(true);
        }
        return io.opentelemetry.sdk.common.CompletableResultCode.ofSuccess();
      }

      @Override
      public io.opentelemetry.sdk.common.CompletableResultCode flush() {
        return io.opentelemetry.sdk.common.CompletableResultCode.ofSuccess();
      }

      @Override
      public io.opentelemetry.sdk.common.CompletableResultCode shutdown() {
        return io.opentelemetry.sdk.common.CompletableResultCode.ofSuccess();
      }
    });
    var span = io.opentelemetry.sdk.testing.trace.TestSpanData.builder().setName("s")
        .setKind(io.opentelemetry.api.trace.SpanKind.INTERNAL).setStartEpochNanos(1).setEndEpochNanos(2)
        .setHasEnded(true).setStatus(io.opentelemetry.sdk.trace.data.StatusData.unset())
        .setAttributes(PASSWORD).setTotalAttributeCount(1).build();
    assertFalse(aPasswordGetsThroughWhileFlipping(c, "traces", () -> {
      gate.export(java.util.List.of(span));
      return leaked.get();
    }), "a span exported with send_traces true and no redaction");
  }

  @Test
  void aLogRecordExportedWhileTheSettingsChangeIsRedactedAsTheDocumentThatSentItSays() throws Exception {
    Controls c = new Controls();
    var leaked = new java.util.concurrent.atomic.AtomicBoolean();
    var gate = c.logs(new io.opentelemetry.sdk.logs.export.LogRecordExporter() {
      @Override
      public io.opentelemetry.sdk.common.CompletableResultCode export(
          java.util.Collection<io.opentelemetry.sdk.logs.data.LogRecordData> logs) {
        if (logs.stream().anyMatch(r -> r.getAttributes().get(PASSWORD_KEY) != null)) {
          leaked.set(true);
        }
        return io.opentelemetry.sdk.common.CompletableResultCode.ofSuccess();
      }

      @Override
      public io.opentelemetry.sdk.common.CompletableResultCode flush() {
        return io.opentelemetry.sdk.common.CompletableResultCode.ofSuccess();
      }

      @Override
      public io.opentelemetry.sdk.common.CompletableResultCode shutdown() {
        return io.opentelemetry.sdk.common.CompletableResultCode.ofSuccess();
      }
    });
    var record = io.opentelemetry.sdk.testing.logs.TestLogRecordData.builder().setBody("b")
        .setSeverity(io.opentelemetry.api.logs.Severity.INFO).setAttributes(PASSWORD).build();
    assertFalse(aPasswordGetsThroughWhileFlipping(c, "logs", () -> {
      gate.export(java.util.List.of(record));
      return leaked.get();
    }), "a log record exported with send_logs true and no redaction");
  }
}
