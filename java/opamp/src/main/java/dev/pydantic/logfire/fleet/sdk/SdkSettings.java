// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.logfire.fleet.sdk;

import com.dylibso.chicory.runtime.ExportFunction;
import com.dylibso.chicory.runtime.Instance;
import com.dylibso.chicory.runtime.Memory;
import com.google.protobuf.InvalidProtocolBufferException;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;
import opamp.proto.v1.Anyvalue;
import opamp.proto.v1.Opamp;

/**
 * What an OpenTelemetry SDK does with the settings an OpAMP server sends it, decided the same way
 * in every language: the SDK settings layer (src/main/wasm/opamp_sdk_settings.wasm),
 * compiled to JVM classes when this jar is built and run with
 * Chicory. It keeps nothing between calls; the caller keeps the agent flow's state it is handed
 * and passes it back with the next step.
 * See docs/sdk-settings.md. Thread-safe.
 */
public final class SdkSettings {
  private final Memory memory;
  private final ExportFunction alloc;
  private final ExportFunction free;
  private final ExportFunction call;

  /** Why the layer can't be loaded, once asked: "" when it can. */
  private static volatile String unavailable;

  /**
   * Why the layer can't be loaded here (the classes compiled from its module missing or broken, or
   * a module that won't instantiate), or null when it can: found out once. An integration asks
   * before it makes one, and runs unmanaged when the layer can't be loaded.
   */
  public static String unavailable() {
    String why = unavailable;
    if (why == null) {
      String found = dev.pydantic.logfire.fleet.Setup.unavailable(SdkSettings::new);
      why = found == null ? "" : found;
      unavailable = why;
    }
    return why.isEmpty() ? null : why;
  }

  /** An instance of the layer. */
  public SdkSettings() {
    Instance instance =
        Instance.builder(SdkSettingsModule.load()).withMachineFactory(SdkSettingsModule::create).build();
    memory = instance.memory();
    alloc = instance.export("alloc");
    free = instance.export("free");
    call = instance.export("call");
  }

  /**
   * The agent flow's start, before the SDK is built: the agent to keep and pass to the next {@link
   * #step}, and what to do first (the settings to start with, from the last config applied in the
   * core's saved state). A host naming what is no setting throws {@link IllegalArgumentException}.
   */
  public SettingsProto.AgentOutput start(SettingsProto.AgentStart start) {
    return call(SettingsProto.Request.newBuilder().setAgentStart(start).build()).getAgentOutput();
  }

  /**
   * One input to the agent flow (a remote config, a connection, the timer, the exporters built...):
   * the agent to keep, the effects to carry out in order, and when to step the timer next.
   */
  public SettingsProto.AgentOutput step(SettingsProto.AgentStep step) {
    return call(SettingsProto.Request.newBuilder().setAgentStep(step).build()).getAgentOutput();
  }

  /**
   * The agent description, from the SDK's resource attributes. It adds the policy format the layer
   * reads and the controls {@code host} carries out at {@code nowUnixMs} (as for load), so that a
   * server knows which overrides this SDK can apply.
   */
  public Opamp.AgentDescription describe(List<Anyvalue.KeyValue> resource, SettingsProto.Host host, long nowUnixMs) {
    SettingsProto.Describe.Builder d = SettingsProto.Describe.newBuilder().addAllResource(resource)
        .setNowUnixMs(nowUnixMs);
    if (host != null) {
      d.setHost(host);
    }
    return call(SettingsProto.Request.newBuilder().setDescribe(d).build()).getDescription();
  }

  /**
   * The inventory as the agent's available components: what the integration lists of itself, its
   * SDK, the instrumentations it can run and the exporters it built, and the settings {@code host}
   * carries out at {@code nowUnixMs} (as for load). The hash is left empty, for the client to
   * compute. See docs/available-components.md#what-the-sdk-integrations-report.
   */
  public Opamp.AvailableComponents components(SettingsProto.Inventory inventory, SettingsProto.Host host,
      long nowUnixMs) {
    SettingsProto.Components.Builder c = SettingsProto.Components.newBuilder().setInventory(inventory)
        .setNowUnixMs(nowUnixMs);
    if (host != null) {
      c.setHost(host);
    }
    return call(SettingsProto.Request.newBuilder().setComponents(c).build()).getAvailableComponents();
  }

  /**
   * What Fleet is told of the exporters an integration found on its SDK's providers, decided the
   * same in every language: the name each is listed under, where it sends (its own endpoint, else
   * the {@code OTEL_EXPORTER_*} variables in {@code env}), the governed and ungoverned signals, and
   * a warning for each ungoverned exporter. At start and when the providers change; never per span.
   */
  public SettingsProto.ExportersDecided exporters(List<SettingsProto.FoundExporter> found, Map<String, String> env,
      String defaultProtocol) {
    SettingsProto.Exporters.Builder e = SettingsProto.Exporters.newBuilder().addAllFound(found)
        .setDefaultProtocol(defaultProtocol == null ? "" : defaultProtocol);
    new TreeMap<>(env).forEach((k, v) -> {
      if (k.startsWith("OTEL_EXPORTER_") && v != null) {
        e.addEnv(SettingsProto.EnvVar.newBuilder().setName(k).setValue(v));
      }
    });
    return call(SettingsProto.Request.newBuilder().setExporters(e).build()).getExportersDecided();
  }

  private synchronized SettingsProto.Reply call(SettingsProto.Request request) {
    byte[] data = request.toByteArray();
    long packed;
    try {
      int ptr = (int) alloc.apply(data.length)[0];
      memory.write(ptr, data);
      packed = call.apply(ptr, data.length)[0];
    } catch (RuntimeException e) {
      throw new IllegalStateException("the SDK settings layer trapped: " + e, e);
    }
    int outPtr = (int) (packed >>> 32);
    int outLen = (int) (packed & 0xffffffffL);
    byte[] raw = memory.readBytes(outPtr, outLen);
    free.apply(outPtr, outLen);
    SettingsProto.Reply reply;
    try {
      reply = SettingsProto.Reply.parseFrom(raw);
    } catch (InvalidProtocolBufferException e) {
      throw new IllegalStateException("the SDK settings layer's reply is unreadable", e);
    }
    if (reply.hasError()) {
      throw new IllegalArgumentException(reply.getError());
    }
    return reply;
  }
}
