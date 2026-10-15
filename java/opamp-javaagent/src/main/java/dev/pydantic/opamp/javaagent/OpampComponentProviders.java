// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import io.opentelemetry.api.incubator.config.ConfigProvider;
import io.opentelemetry.api.incubator.config.DeclarativeConfigException;
import io.opentelemetry.api.incubator.config.DeclarativeConfigProperties;
import io.opentelemetry.common.ComponentLoader;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.DeclarativeConfiguration;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.YamlDeclarativeConfigProperties;
import io.opentelemetry.sdk.autoconfigure.spi.internal.ComponentProvider;
import io.opentelemetry.sdk.autoconfigure.spi.internal.ExtendedDeclarativeConfigProperties;
import io.opentelemetry.sdk.logs.export.LogRecordExporter;
import io.opentelemetry.sdk.metrics.export.MetricExporter;
import io.opentelemetry.sdk.metrics.export.MetricReader;
import io.opentelemetry.sdk.trace.export.SpanExporter;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import java.util.List;
import java.util.Set;

/**
 * The {@code opamp} components a configuration file's model is rewritten with
 * ({@link Declarative#wrap}): each builds the component it wraps, as the SDK would have, and puts
 * the extension's control around it. (Wrapping in the SDK's own exporter customizers won't do: the
 * SDK closes the exporter a customizer replaces.)
 */
public final class OpampComponentProviders {
  private OpampComponentProviders() {}

  /** {@code sampler: {opamp: {<the configured sampler>}}}: the live sampler around it. */
  public static final class LiveSampler implements ComponentProvider {
    @Override
    public Class<?> getType() {
      return Sampler.class;
    }

    @Override
    public String getName() {
      return Declarative.NAME;
    }

    @Override
    public Object create(DeclarativeConfigProperties config) {
      // Nothing configured: the SDK's default, which the live sampler starts as.
      Sampler configured = config.getPropertyKeys().isEmpty()
          ? Sampler.parentBased(Sampler.alwaysOn())
          : DeclarativeConfiguration.createSampler(
              YamlDeclarativeConfigProperties.create(DeclarativeConfigProperties.toMap(config), config.getComponentLoader()));
      return Extension.INSTANCE.controls.sampler.wrap(configured);
    }
  }

  /** {@code exporter: {opamp: {<the configured span exporter>}}}: gated by {@code send_traces}. */
  public static final class Spans implements ComponentProvider {
    @Override
    public Class<?> getType() {
      return SpanExporter.class;
    }

    @Override
    public String getName() {
      return Declarative.NAME;
    }

    @Override
    public Object create(DeclarativeConfigProperties config) {
      return Extension.INSTANCE.controls.spans((SpanExporter) inner(SpanExporter.class, config));
    }
  }

  /** A periodic reader's {@code exporter: {opamp: {...}}}: gated by {@code send_metrics}. */
  public static final class Metrics implements ComponentProvider {
    @Override
    public Class<?> getType() {
      return MetricExporter.class;
    }

    @Override
    public String getName() {
      return Declarative.NAME;
    }

    @Override
    public Object create(DeclarativeConfigProperties config) {
      return Extension.INSTANCE.controls.metrics((MetricExporter) inner(MetricExporter.class, config));
    }
  }

  /** A pull reader's {@code exporter: {opamp: {...}}} (Prometheus): serves nothing while metrics are off. */
  public static final class PullReader implements ComponentProvider {
    @Override
    public Class<?> getType() {
      return MetricReader.class;
    }

    @Override
    public String getName() {
      return Declarative.NAME;
    }

    @Override
    public Object create(DeclarativeConfigProperties config) {
      return Extension.INSTANCE.controls.reader((MetricReader) inner(MetricReader.class, config));
    }
  }

  /** {@code exporter: {opamp: {<the configured log record exporter>}}}: gated by {@code send_logs}. */
  public static final class Logs implements ComponentProvider {
    @Override
    public Class<?> getType() {
      return LogRecordExporter.class;
    }

    @Override
    public String getName() {
      return Declarative.NAME;
    }

    @Override
    public Object create(DeclarativeConfigProperties config) {
      return Extension.INSTANCE.controls.logs((LogRecordExporter) inner(LogRecordExporter.class, config));
    }
  }

  /**
   * The one component {@code config} names, built by its provider as the SDK would: found among
   * the providers the SDK sees, by type and name, and handed its configuration with the SDK's
   * extras (exporters read their component loader and the config provider from it).
   */
  static Object inner(Class<?> type, DeclarativeConfigProperties config) {
    Set<String> keys = config.getPropertyKeys();
    if (keys.size() != 1) {
      throw new DeclarativeConfigException("opamp wraps one " + type.getSimpleName() + ", not " + keys);
    }
    String name = keys.iterator().next();
    DeclarativeConfigProperties inner = config.getStructured(name, DeclarativeConfigProperties.empty());
    ConfigProvider provider = config instanceof ExtendedDeclarativeConfigProperties e
        ? e.getConfigProvider() : ConfigProvider.noop();
    for (ComponentProvider p : ComponentLoader.loadList(config.getComponentLoader(), ComponentProvider.class)) {
      if (p.getType() == type && name.equals(p.getName())) {
        return p.create(new Extended(inner, provider));
      }
    }
    throw new DeclarativeConfigException("no " + type.getSimpleName() + " named \"" + name + "\" to wrap");
  }

  /** Configuration with the SDK's extras, as the SDK hands it to a component. */
  private record Extended(DeclarativeConfigProperties delegate, ConfigProvider configProvider)
      implements ExtendedDeclarativeConfigProperties {
    @Override
    public ConfigProvider getConfigProvider() {
      return configProvider;
    }

    @Override
    public String getString(String name) {
      return delegate.getString(name);
    }

    @Override
    public Boolean getBoolean(String name) {
      return delegate.getBoolean(name);
    }

    @Override
    public Integer getInt(String name) {
      return delegate.getInt(name);
    }

    @Override
    public Long getLong(String name) {
      return delegate.getLong(name);
    }

    @Override
    public Double getDouble(String name) {
      return delegate.getDouble(name);
    }

    @Override
    public <T> List<T> getScalarList(String name, Class<T> scalarType) {
      return delegate.getScalarList(name, scalarType);
    }

    @Override
    public DeclarativeConfigProperties getStructured(String name) {
      return delegate.getStructured(name);
    }

    @Override
    public List<DeclarativeConfigProperties> getStructuredList(String name) {
      return delegate.getStructuredList(name);
    }

    @Override
    public Set<String> getPropertyKeys() {
      return delegate.getPropertyKeys();
    }

    @Override
    public ComponentLoader getComponentLoader() {
      return delegate.getComponentLoader();
    }
  }
}
