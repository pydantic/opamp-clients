// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.Notification;
import dev.pydantic.opamp.OpampClient;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Context;
import io.opentelemetry.exporter.otlp.http.trace.OtlpHttpSpanExporter;
import io.opentelemetry.exporter.otlp.http.trace.OtlpHttpSpanExporterBuilder;
import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.resources.Resource;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.LinkData;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.BatchSpanProcessor;
import io.opentelemetry.sdk.trace.export.SpanExporter;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import io.opentelemetry.sdk.trace.samplers.SamplingResult;
import java.nio.ByteBuffer;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.Collection;
import java.util.List;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.regex.Matcher;
import java.util.regex.Pattern;
import opamp.proto.v1.Anyvalue;
import opamp.proto.v1.Opamp;

public final class Example {
  private static final Set<String> IDENTIFYING = Set.of("service.name", "service.namespace", "service.instance.id");

  /** A sampler the server can change while the program runs. */
  static final class LiveSampler implements Sampler {
    volatile Sampler current;

    LiveSampler(double ratio) {
      current = Sampler.traceIdRatioBased(ratio);
    }

    @Override
    public SamplingResult shouldSample(Context parent, String traceId, String name, SpanKind kind,
        Attributes attributes, List<LinkData> links) {
      return current.shouldSample(parent, traceId, name, kind, attributes, links);
    }

    @Override
    public String getDescription() {
      return current.getDescription();
    }
  }

  /**
   * An exporter the server can point elsewhere while the program runs. Until it says where, spans
   * go nowhere.
   */
  static final class LiveExporter implements SpanExporter {
    private volatile SpanExporter current;

    /** Point it at {@code to}, and shut down the exporter it replaces. */
    void set(SpanExporter to) {
      SpanExporter old = current;
      current = to;
      if (old != null) {
        old.shutdown();
      }
    }

    @Override
    public CompletableResultCode export(Collection<SpanData> spans) {
      SpanExporter c = current;
      return c == null ? CompletableResultCode.ofSuccess() : c.export(spans);
    }

    @Override
    public CompletableResultCode flush() {
      SpanExporter c = current;
      return c == null ? CompletableResultCode.ofSuccess() : c.flush();
    }

    @Override
    public CompletableResultCode shutdown() {
      SpanExporter c = current;
      return c == null ? CompletableResultCode.ofSuccess() : c.shutdown();
    }
  }

  /**
   * An OTLP/HTTP exporter for the traces part of telemetry settings: the endpoint is the full URL
   * (.../v1/traces), with the headers offered.
   */
  static SpanExporter exporter(Opamp.TelemetryConnectionSettings t) {
    OtlpHttpSpanExporterBuilder b = OtlpHttpSpanExporter.builder().setEndpoint(t.getDestinationEndpoint());
    t.getHeaders().getHeadersList().forEach(h -> b.addHeader(h.getKey(), h.getValue()));
    return b.build();
  }

  /** The agent description, from the SDK's resource. */
  static Opamp.AgentDescription description(Resource resource) {
    Opamp.AgentDescription.Builder d = Opamp.AgentDescription.newBuilder();
    resource.getAttributes().forEach((key, value) -> {
      Anyvalue.KeyValue kv = Anyvalue.KeyValue.newBuilder().setKey(key.getKey())
          .setValue(Anyvalue.AnyValue.newBuilder().setStringValue(String.valueOf(value))).build();
      if (IDENTIFYING.contains(key.getKey())) {
        d.addIdentifyingAttributes(kv);
      } else {
        d.addNonIdentifyingAttributes(kv);
      }
    });
    return d.build();
  }

  static Opamp.EffectiveConfig effective(double ratio) {
    Opamp.AgentConfigObject file = Opamp.AgentConfigObject.newBuilder()
        .setBody(ByteString.copyFromUtf8("{\"sampling_ratio\": " + ratio + "}"))
        .setContentType("application/json").build();
    return Opamp.EffectiveConfig.newBuilder()
        .setConfigMap(Opamp.AgentConfigMap.newBuilder().putConfigMap("", file)).build();
  }

  /** The ratio in a remote config of the form {"sampling_ratio": 0.25} (use your JSON library). */
  static Double samplingRatio(Opamp.AgentRemoteConfig config) {
    Opamp.AgentConfigObject file = config.getConfig().getConfigMapMap().get("");
    Matcher m = Pattern.compile("\"sampling_ratio\"\\s*:\\s*([0-9.eE+-]+)")
        .matcher(file == null ? "" : file.getBody().toString(StandardCharsets.UTF_8));
    if (!m.find()) {
      return null;
    }
    try {
      double ratio = Double.parseDouble(m.group(1));
      return ratio >= 0 && ratio <= 1 ? ratio : null;
    } catch (NumberFormatException e) {
      return null;
    }
  }

  public static void main(String[] args) throws Exception {
    // One UUID is both the OpAMP instance UID and service.instance.id, so the fleet view and the
    // telemetry name the same instance.
    UUID uid = UUID.randomUUID();
    Resource resource = Resource.getDefault().merge(Resource.create(Attributes.of(
        AttributeKey.stringKey("service.name"), "checkout",
        AttributeKey.stringKey("service.instance.id"), uid.toString())));
    LiveSampler sampler = new LiveSampler(1.0);
    // Spans go wherever the server says (telemetry settings), through a live exporter.
    LiveExporter live = new LiveExporter();
    SdkTracerProvider tracerProvider = SdkTracerProvider.builder()
        .setResource(resource).setSampler(Sampler.parentBased(sampler))
        .addSpanProcessor(BatchSpanProcessor.builder(live).setScheduleDelay(Duration.ofMillis(500)).build())
        .build();
    OpenTelemetrySdk.builder().setTracerProvider(tracerProvider).buildAndRegisterGlobal();

    byte[] instanceUid = ByteBuffer.allocate(16)
        .putLong(uid.getMostSignificantBits()).putLong(uid.getLeastSignificantBits()).array();
    OpampClient.Config config = OpampClient.Config
        .builder(System.getenv("OPAMP_ENDPOINT"), instanceUid) // https://.../v1/opamp, or wss://...
        .capabilities(Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE
            | Opamp.AgentCapabilities.AgentCapabilities_ReportsOwnTraces_VALUE)
        // .header("Authorization", "Bearer " + token)
        .build();
    OpampClient.Status status = new OpampClient.Status(
        description(resource), Opamp.ComponentHealth.newBuilder().setHealthy(true).build(), effective(1.0), null);
    // Notifications arrive on the client's thread; the first may come before start returns.
    CompletableFuture<OpampClient> started = new CompletableFuture<>();
    OpampClient client = OpampClient.start(config, status, n -> {
      if (n instanceof Notification.Stopped s && s.error() != null) {
        // The client stopped on its own (a bug): don't carry on unmanaged.
        System.err.println("OpAMP client stopped: " + s.error());
        System.exit(1);
      }
      if (n instanceof Notification.ConnectionSettings cs) {
        // Where to send our own telemetry: all the settings, not only what changed.
        Opamp.ConnectionSettingsStatus.Builder st = Opamp.ConnectionSettingsStatus.newBuilder()
            .setLastConnectionSettingsHash(cs.settings().getHash())
            .setStatus(Opamp.ConnectionSettingsStatuses.ConnectionSettingsStatuses_APPLIED);
        if (cs.settings().hasOwnTraces()) {
          try {
            live.set(exporter(cs.settings().getOwnTraces()));
          } catch (RuntimeException e) {
            st.setStatus(Opamp.ConnectionSettingsStatuses.ConnectionSettingsStatuses_FAILED)
                .setErrorMessage(e.toString());
          }
        }
        started.join().setConnectionSettingsStatus(st.build());
        return;
      }
      if (!(n instanceof Notification.RemoteConfig rc)) {
        return;
      }
      Opamp.RemoteConfigStatus.Builder s = Opamp.RemoteConfigStatus.newBuilder()
          .setLastRemoteConfigHash(rc.config().getConfigHash());
      Double ratio = samplingRatio(rc.config());
      if (ratio == null) {
        s.setStatus(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_FAILED)
            .setErrorMessage("want {\"sampling_ratio\": 0..1}");
      } else {
        sampler.current = Sampler.traceIdRatioBased(ratio);
        s.setStatus(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
        started.join().setEffectiveConfig(effective(ratio));
      }
      started.join().setRemoteConfigStatus(s.build());
    });
    started.complete(client);

    // The application's work: a span every 100 ms.
    Tracer tracer = tracerProvider.get("checkout");
    ScheduledExecutorService work = Executors.newSingleThreadScheduledExecutor();
    work.scheduleAtFixedRate(() -> tracer.spanBuilder("checkout").startSpan().end(), 0, 100, TimeUnit.MILLISECONDS);

    // The program runs until it is told to stop. Then the server hears the agent go
    // (agent_disconnect), and the SDK flushes.
    CountDownLatch stopped = new CountDownLatch(1);
    Runtime.getRuntime().addShutdownHook(new Thread(() -> {
      work.shutdownNow();
      client.stop().join();
      tracerProvider.shutdown().join(10, TimeUnit.SECONDS);
      stopped.countDown();
    }));
    stopped.await();
  }
}
