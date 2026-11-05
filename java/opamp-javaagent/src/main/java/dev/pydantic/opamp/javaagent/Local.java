// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import java.math.BigDecimal;

/**
 * What the SDK autoconfigure builds does without the server, for the local report (contracts'
 * logfire-sdk-local/v1), as far as its properties tell it: the ratio of the sampler
 * {@code otel.traces.sampler} names, and whether each signal is exported at all
 * ({@code otel.<signal>.exporter}, as {@link Inventory#names} reads it). Left out: a sampler that
 * isn't a ratio, and the log level, as the SDK has none of its own for the application's records.
 */
final class Local {
  private Local() {}

  /** The local values {@code config} tells; never throws, as the report never fails. */
  static SettingsProto.Local of(ConfigProperties config) {
    SettingsProto.Local.Builder b = SettingsProto.Local.newBuilder();
    String rate = samplingRate(config);
    if (rate != null) {
      b.setSamplingRate(rate);
    }
    try {
      b.setSendTraces(!Inventory.names(config, "traces").isEmpty())
          .setSendMetrics(!Inventory.names(config, "metrics").isEmpty())
          .setSendLogs(!Inventory.names(config, "logs").isEmpty());
    } catch (RuntimeException e) {
      // A list it can't read: the SDK can't be built from it either. Not told.
      return rate == null ? SettingsProto.Local.getDefaultInstance() : SettingsProto.Local.newBuilder()
          .setSamplingRate(rate).build();
    }
    return b.build();
  }

  /**
   * The ratio of new traces the sampler {@code otel.traces.sampler} names samples, as the SDK builds
   * it from its argument ({@code otel.traces.sampler.arg}, 1 by default): always on is 1, always
   * off 0. Null for any other sampler (jaeger_remote, one of an extension's), or an argument the
   * SDK can't build one from.
   */
  static String samplingRate(ConfigProperties config) {
    double ratio;
    try {
      switch (config.getString("otel.traces.sampler", "parentbased_always_on")) {
        case "always_on", "parentbased_always_on" -> ratio = 1;
        case "always_off", "parentbased_always_off" -> ratio = 0;
        case "traceidratio", "parentbased_traceidratio" -> ratio = config.getDouble("otel.traces.sampler.arg", 1.0d);
        default -> {
          return null;
        }
      }
    } catch (RuntimeException e) {
      return null; // ConfigurationException: the SDK refuses it too
    }
    return ratio >= 0 && ratio <= 1 ? decimal(ratio) : null;
  }

  /** A ratio as decimal text, as the local report takes it: "1", "0.25", "0.00001" (no exponent). */
  static String decimal(double x) {
    return BigDecimal.valueOf(x).stripTrailingZeros().toPlainString();
  }
}
