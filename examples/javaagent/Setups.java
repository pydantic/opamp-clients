// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

import io.opentelemetry.exporter.otlp.http.logs.OtlpHttpLogRecordExporter;
import io.opentelemetry.exporter.otlp.http.metrics.OtlpHttpMetricExporter;
import io.opentelemetry.exporter.otlp.http.trace.OtlpHttpSpanExporter;
import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizer;
import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizerProvider;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import io.opentelemetry.sdk.logs.export.BatchLogRecordProcessor;
import io.opentelemetry.sdk.metrics.export.PeriodicMetricReader;
import io.opentelemetry.sdk.trace.export.BatchSpanProcessor;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import java.time.Duration;
import java.util.Arrays;
import java.util.Set;
import java.util.stream.Collectors;

/**
 * A second agent extension, for the setups check_truth runs App under (examples/integrations.py).
 * It does what another vendor's extension may do to the SDK the agent builds, as EXAMPLE_SETUP
 * names it (comma-separated), and nothing without it:
 *
 * <ul>
 *   <li>{@code extension-processor}: a span processor, a log record processor and a metric reader
 *       of its own, each exporting over OTLP to the same endpoint as the agent's exporters.
 *   <li>{@code extension-sampler}: a sampler of its own, which samples every new trace.
 * </ul>
 *
 * <p>Both go in through the autoconfigure customizers, as an extension's do.
 */
public final class Setups implements AutoConfigurationCustomizerProvider {
  private static final Duration DELAY = Duration.ofMillis(500);

  @Override
  public void customize(AutoConfigurationCustomizer customizer) {
    Set<String> setups = Arrays.stream(System.getenv().getOrDefault("EXAMPLE_SETUP", "").split(","))
        .map(String::trim).filter(s -> !s.isEmpty()).collect(Collectors.toSet());
    if (setups.contains("extension-processor")) {
      customizer
          .addTracerProviderCustomizer((builder, config) -> builder.addSpanProcessor(BatchSpanProcessor.builder(
              OtlpHttpSpanExporter.builder().setEndpoint(endpoint(config, "traces")).build())
              .setScheduleDelay(DELAY).build()))
          .addLoggerProviderCustomizer((builder, config) -> builder.addLogRecordProcessor(BatchLogRecordProcessor
              .builder(OtlpHttpLogRecordExporter.builder().setEndpoint(endpoint(config, "logs")).build())
              .setScheduleDelay(DELAY).build()))
          .addMeterProviderCustomizer((builder, config) -> builder.registerMetricReader(PeriodicMetricReader
              .builder(OtlpHttpMetricExporter.builder().setEndpoint(endpoint(config, "metrics")).build())
              .setInterval(DELAY).build()));
    }
    if (setups.contains("extension-sampler")) {
      customizer.addTracerProviderCustomizer((builder, config) -> builder.setSampler(Sampler.parentBased(Sampler.alwaysOn())));
    }
  }

  /** Where the agent's OTLP exporters send this signal: the base endpoint and the signal's path. */
  private static String endpoint(ConfigProperties config, String signal) {
    String base = config.getString("otel.exporter.otlp.endpoint", "http://localhost:4318");
    return (base.endsWith("/") ? base : base + "/") + "v1/" + signal;
  }
}
