// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizer;
import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizerProvider;

/**
 * Puts the extension's controls into the SDK the agent builds: the saved next-start settings as
 * properties, the instance UID in the resource, and the live sampler and gated exporters (and pull
 * metric reader) around the agent's own, each listed in the inventory. With no
 * {@code otel.opamp.endpoint} it changes nothing.
 */
public final class OpampCustomizerProvider implements AutoConfigurationCustomizerProvider {
  private final Extension e;

  /** The agent's: the extension's one instance. */
  public OpampCustomizerProvider() {
    this(Extension.INSTANCE);
  }

  /** For tests: an SDK built around {@code e}. */
  OpampCustomizerProvider(Extension e) {
    this.e = e;
  }

  @Override
  public void customize(AutoConfigurationCustomizer customizer) {
    customizer
        .addPropertiesCustomizer(e::properties)
        .addResourceCustomizer((resource, config) -> e.resource(resource))
        .addSamplerCustomizer((sampler, config) -> e.on() ? e.controls.sampler.wrap(sampler) : sampler)
        .addSpanExporterCustomizer((exporter, config) -> {
          if (!e.on()) {
            return exporter;
          }
          e.inventory.configured("traces", config);
          return e.controls.spans(exporter);
        })
        .addMetricExporterCustomizer((exporter, config) -> {
          if (!e.on()) {
            return exporter;
          }
          e.inventory.configured("metrics", config);
          return e.controls.metrics(exporter);
        })
        .addMetricReaderCustomizer((reader, config) -> {
          if (!e.on()) {
            return reader;
          }
          e.inventory.configured("metrics", config);
          return e.controls.reader(reader);
        })
        .addLogRecordExporterCustomizer((exporter, config) -> {
          if (!e.on()) {
            return exporter;
          }
          e.inventory.configured("logs", config);
          return e.controls.logs(exporter);
        });
  }

  /** Last, so the controls wrap what every other customizer built. */
  @Override
  public int order() {
    return Integer.MAX_VALUE;
  }
}
