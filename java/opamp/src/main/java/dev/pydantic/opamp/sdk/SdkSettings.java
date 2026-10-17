// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.sdk;

import com.dylibso.chicory.runtime.ExportFunction;
import com.dylibso.chicory.runtime.Instance;
import com.dylibso.chicory.runtime.Memory;
import com.google.protobuf.ByteString;
import com.google.protobuf.InvalidProtocolBufferException;
import java.util.List;
import opamp.proto.v1.Anyvalue;
import opamp.proto.v1.Opamp;

/**
 * What an OpenTelemetry SDK does with the settings an OpAMP server sends it, decided the same way
 * in every language: the SDK settings layer (src/main/wasm/opamp_sdk_settings.wasm),
 * compiled to JVM classes when this jar is built and run with
 * Chicory. It keeps nothing between calls; the caller keeps the settings documents it is handed
 * ({@code current}, and the one it saves) and passes them back.
 * See docs/sdk-settings.md. Thread-safe.
 */
public final class SdkSettings {
  private final Memory memory;
  private final ExportFunction alloc;
  private final ExportFunction free;
  private final ExportFunction call;

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
   * At start, before the SDK is built: the settings to start with, from the document saved at the
   * last change ({@code null}: nothing saved). Put all of them in force, next-start ones too. With
   * no host, none is carried out: see {@link #load(byte[], SettingsProto.Host, long)}.
   */
  public SettingsProto.Loaded load(byte[] saved) {
    return load(saved, null, 0);
  }

  /**
   * As {@link #load(byte[])}, for {@code host}: the settings the SDK carries out, at once or at the
   * next start ({@code null}: none carried out; see docs/sdk-settings.md#the-host). At {@code
   * nowUnixMs}, the wall clock in milliseconds since the Unix epoch, which debug mode is decided by
   * (0: none, and the SDK does not carry out debug mode). A host naming what is no setting throws
   * {@link IllegalArgumentException}.
   */
  public SettingsProto.Loaded load(byte[] saved, SettingsProto.Host host, long nowUnixMs) {
    return loadFrom(null, saved, host, nowUnixMs);
  }

  /**
   * As {@link #load(byte[], SettingsProto.Host, long)}, with {@code coreState}: the OpAMP core's
   * saved state (what it asked the host to store; {@code null}: none). The settings then come from
   * the last config applied in it, and {@code saved} is read only when it holds none (a document
   * kept before the core kept the config applied).
   */
  public SettingsProto.Loaded loadFrom(byte[] coreState, byte[] saved, SettingsProto.Host host, long nowUnixMs) {
    SettingsProto.Load.Builder l = SettingsProto.Load.newBuilder().setNowUnixMs(nowUnixMs);
    if (saved != null) {
      l.setSaved(ByteString.copyFrom(saved));
    }
    if (coreState != null) {
      l.setCoreState(ByteString.copyFrom(coreState));
    }
    if (host != null) {
      l.setHost(host);
    }
    return call(SettingsProto.Request.newBuilder().setLoad(l).build()).getLoaded();
  }

  /** Keeps the document the layer asks to keep for the next start: whole or not at all. */
  @FunctionalInterface
  public interface Saver {
    /** Save {@code doc} where {@link #load} reads it at the next start; throws if it can't. */
    void save(byte[] doc) throws Exception;
  }

  /**
   * How to answer a remote config, for a host that carries out nothing and keeps no state: see
   * {@link #decide(Opamp.AgentRemoteConfig, ByteString, ByteString, SettingsProto.Host, long, Saver)}.
   */
  public SettingsProto.Decision decide(Opamp.AgentRemoteConfig config, ByteString current, ByteString atStart) {
    return decide(config, current, atStart, null, 0, null);
  }

  /**
   * How to answer a remote config. {@code current} is the document in force (from the last {@link
   * #load} or decision), {@code atStart} the one loaded at start, the host and the time as for load.
   *
   * <p>The layer decides what to keep for the next start; {@code save} keeps it, and throws if it
   * can't; the layer then answers from how that went: a config whose save failed is refused, saying
   * why. {@code null}: the SDK keeps no state across restarts, so a config that changes a next-start
   * setting is refused. A policy that overrides a control the host does not carry out is refused too.
   */
  public SettingsProto.Decision decide(Opamp.AgentRemoteConfig config, ByteString current, ByteString atStart,
      SettingsProto.Host host, long nowUnixMs, Saver save) {
    SettingsProto.Decide.Builder d = SettingsProto.Decide.newBuilder().setConfig(config).setCurrent(current)
        .setAtStart(atStart).setNowUnixMs(nowUnixMs);
    if (host != null) {
      d.setHost(host);
    }
    SettingsProto.Decide decide = d.build();
    SettingsProto.Plan plan = call(SettingsProto.Request.newBuilder().setDecide(decide).build()).getPlan();
    SettingsProto.Commit.Builder commit = SettingsProto.Commit.newBuilder().setDecide(decide);
    if (plan.hasSave() && save != null) {
      try {
        save.save(plan.getSave().toByteArray());
        commit.setSaved(SettingsProto.Commit.Saved.SAVED_DONE);
      } catch (Exception e) {
        // Whatever stopped the save, the config is refused for it.
        commit.setSaved(SettingsProto.Commit.Saved.SAVED_FAILED)
            .setError(e.getMessage() != null ? e.getMessage() : e.toString());
      }
    }
    return call(SettingsProto.Request.newBuilder().setCommit(commit).build()).getDecision();
  }

  /**
   * What is in force at {@code nowUnixMs}, at the time a load, decision or refresh said to ask again
   * ({@code refreshAtUnixMs}): the end of debug mode. {@code current} and {@code atStart} as for decide.
   */
  public SettingsProto.Refreshed refresh(ByteString current, ByteString atStart, SettingsProto.Host host,
      long nowUnixMs) {
    SettingsProto.Refresh.Builder r = SettingsProto.Refresh.newBuilder().setCurrent(current).setAtStart(atStart)
        .setNowUnixMs(nowUnixMs);
    if (host != null) {
      r.setHost(host);
    }
    return call(SettingsProto.Request.newBuilder().setRefresh(r).build()).getRefreshed();
  }

  /**
   * Export health, from the gates' counts since start: whether to send it now, and the entry for the
   * agent health's {@code component_health_map["exporters"]}. {@code previous}: what the last call
   * answered ({@code null} at the first); {@code settings}: those in force ({@code null}: none).
   */
  public SettingsProto.HealthReport health(SettingsProto.ExportCounts counts, SettingsProto.HealthReport previous,
      long nowUnixMs, SettingsProto.Settings settings) {
    SettingsProto.Health.Builder h = SettingsProto.Health.newBuilder().setCounts(counts).setNowUnixMs(nowUnixMs);
    if (previous != null) {
      h.setPrevious(previous);
    }
    if (settings != null) {
      h.setSettings(settings);
    }
    return call(SettingsProto.Request.newBuilder().setHealth(h).build()).getHealthReport();
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
