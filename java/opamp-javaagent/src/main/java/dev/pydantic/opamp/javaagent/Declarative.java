// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.api.incubator.config.DeclarativeConfigProperties;
import io.opentelemetry.common.ComponentLoader;
import io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdk;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.DeclarativeConfiguration;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.AttributeNameValueModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.AttributeTypeModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.DistributionModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.DistributionPropertyModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.LogRecordExporterModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.LogRecordExporterPropertyModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.LogRecordProcessorModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.MetricReaderModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.OpenTelemetryConfigurationModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.PullMetricExporterModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.PullMetricExporterPropertyModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.PushMetricExporterModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.PushMetricExporterPropertyModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.ResourceModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.SamplerModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.SamplerPropertyModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.SpanExporterModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.SpanExporterPropertyModel;
import io.opentelemetry.sdk.autoconfigure.declarativeconfig.model.SpanProcessorModel;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import io.opentelemetry.sdk.autoconfigure.spi.internal.DefaultConfigProperties;
import io.opentelemetry.sdk.resources.Resource;
import java.lang.reflect.Method;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * The extension under declarative configuration ({@code otel.config.file}), where the agent builds
 * its SDK from a configuration model and runs none of the autoconfigure customizers. The same
 * controls go in, by rewriting the model before the SDK is built: the sampler and each exporter
 * become an {@code opamp} component wrapping what was configured ({@link OpampComponentProviders}),
 * the instance UID goes into the resource, and the next-start settings into
 * {@code distribution.javaagent}, where the agent reads which instrumentations run.
 *
 * <p>Where the OpAMP server is, is read as without a configuration file: {@code otel.opamp.*}
 * system properties and {@code OTEL_OPAMP_*} environment variables.
 */
final class Declarative {
  /** The name of the extension's components in the model. */
  static final String NAME = "opamp";
  private static final Logger LOG = Logger.getLogger(Extension.class.getName());

  private Declarative() {}

  /** The {@code otel.opamp.*} settings, from system properties and environment variables. */
  static ConfigProperties config() {
    return DefaultConfigProperties.create(Map.of(), ComponentLoader.forClassLoader(Declarative.class.getClassLoader()));
  }

  /**
   * First, before the agent reads the model: load the saved settings, then put the instance UID in
   * the resource and the next-start settings in the distribution. With no endpoint, the model as
   * it is.
   */
  static OpenTelemetryConfigurationModel setUp(Extension e, ConfigProperties config, OpenTelemetryConfigurationModel m) {
    e.properties(config);
    if (!e.on()) {
      return m;
    }
    ResourceModel resource = m.getResource() == null ? new ResourceModel() : m.getResource();
    List<AttributeNameValueModel> attributes =
        resource.getAttributes() == null ? new ArrayList<>() : new ArrayList<>(resource.getAttributes());
    String given = null;
    for (AttributeNameValueModel a : attributes) {
      if ("service.instance.id".equals(a.getName()) && a.getValue() instanceof String s) {
        given = s;
      }
    }
    String uid = e.instanceId(given);
    attributes.removeIf(a -> "service.instance.id".equals(a.getName()));
    attributes.add(new AttributeNameValueModel().withName("service.instance.id").withValue(uid)
        .withType(AttributeTypeModel.STRING));
    m.withResource(resource.withAttributes(attributes));
    instrumentations(m, e.startSettings());
    return m;
  }

  /**
   * The next-start settings, as the agent reads them from the model:
   * {@code distribution.javaagent.instrumentation}'s {@code disabled} list (names with {@code _}
   * for {@code -}, as the agent compares them) and {@code default_enabled}. A name already enabled
   * there is still disabled: the agent checks {@code disabled} first.
   */
  @SuppressWarnings("unchecked")
  static void instrumentations(OpenTelemetryConfigurationModel m, SettingsProto.Settings s) {
    List<String> names = s.getDeactivateInstrumentations().getNamesList();
    boolean all = s.hasDeactivateAllInstrumentations() && s.getDeactivateAllInstrumentations();
    if (names.isEmpty() && !all) {
      return;
    }
    DistributionModel distribution = m.getDistribution() == null ? new DistributionModel() : m.getDistribution();
    DistributionPropertyModel agent = distribution.getAdditionalProperties().get("javaagent");
    if (agent == null) {
      agent = new DistributionPropertyModel();
    }
    Object was = agent.getAdditionalProperties().get("instrumentation");
    Map<String, Object> instrumentation = was instanceof Map<?, ?> map
        ? new LinkedHashMap<>((Map<String, Object>) map) : new LinkedHashMap<>();
    Set<String> disabled = new LinkedHashSet<>();
    if (instrumentation.get("disabled") instanceof List<?> list) {
      list.forEach(n -> disabled.add(String.valueOf(n)));
    }
    for (String name : names) {
      disabled.add(name.replace('-', '_'));
    }
    if (!disabled.isEmpty()) {
      instrumentation.put("disabled", new ArrayList<>(disabled));
    }
    if (all) {
      instrumentation.put("default_enabled", false);
    }
    m.withDistribution(distribution.withAdditionalProperty("javaagent",
        agent.withAdditionalProperty("instrumentation", instrumentation)));
  }

  /**
   * Last, so the controls wrap what every other customizer configured: the sampler, and every
   * exporter of the span and log processors and metric readers the model names, each in an
   * {@code opamp} component, and listed in {@code inventory}. A processor or reader of another kind
   * (a component of its own) isn't reached, so its signal's settings aren't declared ({@link
   * #ungated}): it is listed by its name, ungated.
   */
  static OpenTelemetryConfigurationModel wrap(OpenTelemetryConfigurationModel m, Inventory inventory) {
    if (m.getTracerProvider() != null) {
      SamplerModel sampler = m.getTracerProvider().getSampler();
      SamplerPropertyModel live = new SamplerPropertyModel();
      if (sampler != null) {
        inner(sampler).forEach(live::withAdditionalProperty);
      }
      m.getTracerProvider().withSampler(new SamplerModel().withAdditionalProperty(NAME, live));
      List<SettingsProto.Exporter> built = new ArrayList<>();
      for (SpanProcessorModel p : list(m.getTracerProvider().getProcessors())) {
        if (p.getBatch() != null) {
          p.getBatch().withExporter(spans(p.getBatch().getExporter(), built));
        }
        if (p.getSimple() != null) {
          p.getSimple().withExporter(spans(p.getSimple().getExporter(), built));
        }
        p.getAdditionalProperties().keySet().forEach(k -> built.add(Inventory.exporter("traces", k, false)));
      }
      inventory.exporters("traces", built);
    }
    if (m.getMeterProvider() != null) {
      List<SettingsProto.Exporter> built = new ArrayList<>();
      for (MetricReaderModel r : list(m.getMeterProvider().getReaders())) {
        if (r.getPeriodic() != null && r.getPeriodic().getExporter() != null) {
          PushMetricExporterPropertyModel gated = new PushMetricExporterPropertyModel();
          inner(r.getPeriodic().getExporter()).forEach(gated::withAdditionalProperty);
          listed("metrics", gated.getAdditionalProperties().keySet(), built);
          r.getPeriodic().withExporter(new PushMetricExporterModel().withAdditionalProperty(NAME, gated));
        }
        if (r.getPull() != null && r.getPull().getExporter() != null) {
          PullMetricExporterPropertyModel gated = new PullMetricExporterPropertyModel();
          inner(r.getPull().getExporter()).forEach(gated::withAdditionalProperty);
          listed("metrics", gated.getAdditionalProperties().keySet(), built);
          r.getPull().withExporter(new PullMetricExporterModel().withAdditionalProperty(NAME, gated));
        }
      }
      inventory.exporters("metrics", built);
    }
    if (m.getLoggerProvider() != null) {
      List<SettingsProto.Exporter> built = new ArrayList<>();
      for (LogRecordProcessorModel p : list(m.getLoggerProvider().getProcessors())) {
        if (p.getBatch() != null) {
          p.getBatch().withExporter(logs(p.getBatch().getExporter(), built));
        }
        if (p.getSimple() != null) {
          p.getSimple().withExporter(logs(p.getSimple().getExporter(), built));
        }
        p.getAdditionalProperties().keySet().forEach(k -> built.add(Inventory.exporter("logs", k, false)));
      }
      inventory.exporters("logs", built);
    }
    return m;
  }

  /**
   * The signals the model sends past the gates: a span or log processor that is neither batch nor
   * simple, or a metric reader neither periodic nor pull, is a component of its own, which {@link
   * #wrap} doesn't reach, so the server's settings for that signal can't be carried out.
   */
  static Set<String> ungated(OpenTelemetryConfigurationModel m) {
    Set<String> out = new LinkedHashSet<>();
    if (m.getTracerProvider() != null && list(m.getTracerProvider().getProcessors()).stream()
        .anyMatch(p -> p.getBatch() == null && p.getSimple() == null)) {
      out.add("traces");
    }
    if (m.getMeterProvider() != null && list(m.getMeterProvider().getReaders()).stream()
        .anyMatch(r -> r.getPeriodic() == null && r.getPull() == null)) {
      out.add("metrics");
    }
    if (m.getLoggerProvider() != null && list(m.getLoggerProvider().getProcessors()).stream()
        .anyMatch(p -> p.getBatch() == null && p.getSimple() == null)) {
      out.add("logs");
    }
    return out;
  }

  /**
   * An exporter the model names, as {@code OTEL_*_EXPORTER} names it: {@code otlp_http} and
   * {@code otlp_grpc} are {@code otlp}, {@code prometheus/development} is {@code prometheus}; any
   * other by its name in the model ({@code console}, {@code zipkin}, a component of its own).
   */
  static String exporterName(String model) {
    return switch (model) {
      case "otlp_http", "otlp_grpc" -> "otlp";
      case "prometheus/development" -> "prometheus";
      default -> model;
    };
  }

  /** The exporter a part of the model names (its one key), gated, onto {@code built}. */
  private static void listed(String signal, Set<String> keys, List<SettingsProto.Exporter> built) {
    keys.forEach(k -> built.add(Inventory.exporter(signal, exporterName(k), true)));
  }

  private static <T> List<T> list(List<T> l) {
    return l == null ? List.of() : l;
  }

  private static SpanExporterModel spans(SpanExporterModel exporter, List<SettingsProto.Exporter> built) {
    if (exporter == null) {
      return null;
    }
    SpanExporterPropertyModel gated = new SpanExporterPropertyModel();
    inner(exporter).forEach(gated::withAdditionalProperty);
    listed("traces", gated.getAdditionalProperties().keySet(), built);
    return new SpanExporterModel().withAdditionalProperty(NAME, gated);
  }

  private static LogRecordExporterModel logs(LogRecordExporterModel exporter, List<SettingsProto.Exporter> built) {
    if (exporter == null) {
      return null;
    }
    LogRecordExporterPropertyModel gated = new LogRecordExporterPropertyModel();
    inner(exporter).forEach(gated::withAdditionalProperty);
    listed("logs", gated.getAdditionalProperties().keySet(), built);
    return new LogRecordExporterModel().withAdditionalProperty(NAME, gated);
  }

  /** A part of the model as the plain map an {@code opamp} component is configured with. */
  static Map<String, Object> inner(Object model) {
    return DeclarativeConfigProperties.toMap(DeclarativeConfiguration.toConfigProperties(model));
  }

  /**
   * The resource the agent built, for the agent description; the SDK doesn't expose it, so it is
   * read reflectively, and without it the instance is described by its UID alone.
   */
  static Resource resource(AutoConfiguredOpenTelemetrySdk sdk) {
    try {
      Method get = AutoConfiguredOpenTelemetrySdk.class.getDeclaredMethod("getResource");
      get.setAccessible(true);
      Object r = get.invoke(sdk);
      if (r instanceof Resource resource) {
        return resource;
      }
    } catch (ReflectiveOperationException | RuntimeException err) {
      LOG.log(Level.FINE, "OpAMP: the agent's resource isn't readable", err);
    }
    return null;
  }
}
