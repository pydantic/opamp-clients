// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import dev.pydantic.opamp.sdk.SdkSettings;
import java.util.List;
import opamp.proto.v1.Anyvalue;
import opamp.proto.v1.Opamp;

/**
 * Jazzer target: the buffers at the WebAssembly boundary ({@link Core} and {@link SdkSettings}: each
 * call writes its message into the module's memory, and reads the reply back from where the module
 * says), with any bytes on both sides, as python/tests/fuzz/fuzz_native.py does for the native core.
 * One input is a session: a Config (any saved state), then steps whose inputs carry any
 * server-to-agent bytes (a response, a WebSocket message, a stream's data, up to megabytes: the
 * module's memory grows under the client's buffers), and calls into the SDK settings layer (load
 * any saved document, decide on any remote config, describe). What must hold: every call returns
 * an Output or, for an input the core refuses, a CoreException with its reason; the core never traps,
 * and neither does the SDK settings layer (a refusal is an IllegalArgumentException); nothing else
 * escapes.
 *
 * <p>scripts/java_fuzz.sh runs it under Jazzer; {@link FuzzInputsTest} runs {@link #SEEDS} and every
 * input under src/test/resources/fuzz/CoreFuzz/ with every {@code mvn test}.
 */
public final class CoreFuzz {
  private CoreFuzz() {}

  private static final SdkSettings LAYER = new SdkSettings();

  public static void fuzzerTestOneInput(byte[] data) {
    FuzzInput.Reader r = new FuzzInput.Reader(data);
    CoreProto.Config.Builder c = CoreProto.Config.newBuilder()
        .setEndpoint(r.pick(List.of("http://s.test/v1/opamp", "ws://s.test/v1/opamp", r.text(40))))
        .setEntropy(ByteString.copyFrom(new byte[32])).setUnixMs(1_790_000_000_000L).setSeed(r.u64())
        .setCapabilities(r.u64()).setPersistence(r.bit()).setAcceptPackages(r.bit())
        .setAcceptConnectionSettings(r.bit()).setStreams(r.bit())
        .setMaxReceiveBytes(r.pick(List.of(0L, 1L, 1024L, -1L)));
    if (c.getPersistence()) {
      c.setSavedState(r.bytes(1024));
    }
    Core core;
    try {
      core = new Core(c.build(), "");
    } catch (Core.CoreException e) {
      if (e.getMessage().startsWith("trap")) {
        throw new AssertionError("init trapped", e);
      }
      return; // a config it refuses, with the reason
    }
    long now = 0;
    while (!r.empty()) {
      if (r.u8() % 5 == 0) {
        sdk(r);
        continue;
      }
      now += r.u8() * 100L;
      CoreProto.Input in = input(r);
      try {
        core.step(now, 1_790_000_000_000L + now, in);
      } catch (Core.CoreException e) {
        if (e.getMessage().startsWith("trap")) {
          throw new AssertionError("step trapped on " + in.getKindCase(), e);
        }
      }
    }
    core.close();
  }

  /** Bytes a server may send: any, or one piece repeated up to megabytes. */
  private static ByteString payload(FuzzInput.Reader r) {
    byte[] b = r.blob(4096);
    int times = r.u8() % 4 == 0 ? 1 + r.u16() % 512 : 1;
    if (times == 1 || b.length == 0) {
      return ByteString.copyFrom(b);
    }
    ByteString.Output out = ByteString.newOutput(b.length * times);
    for (int i = 0; i < times; i++) {
      out.write(b, 0, b.length);
    }
    return out.toByteString();
  }

  private static CoreProto.Input input(FuzzInput.Reader r) {
    CoreProto.Input.Builder i = CoreProto.Input.newBuilder();
    switch (r.u8() % 8) {
      case 0 -> i.setStart(CoreProto.Empty.getDefaultInstance());
      case 1 -> i.setTimer(CoreProto.Empty.getDefaultInstance());
      case 2 -> i.setHttpResponse(CoreProto.HttpResponse.newBuilder().setId(r.u8())
          .setStatus(r.pick(List.of(200, 200, 204, 404, 503, (int) r.u32()))).setBody(payload(r)));
      case 3 -> i.setWsMessage(CoreProto.WsMessage.newBuilder().setId(r.u8()).setData(payload(r)));
      case 4 -> i.setStop(CoreProto.Empty.getDefaultInstance());
      case 5 -> i.setStreamData(CoreProto.WsMessage.newBuilder().setId(r.u8()).setData(payload(r)));
      case 6 -> i.setWsOpened(CoreProto.Id.newBuilder().setId(r.u8()));
      default -> {
        // Any bytes that decode as an input.
        try {
          return CoreProto.Input.parseFrom(r.blob(1024));
        } catch (com.google.protobuf.InvalidProtocolBufferException e) {
          i.setTimer(CoreProto.Empty.getDefaultInstance());
        }
      }
    }
    return i.build();
  }

  private static void sdk(FuzzInput.Reader r) {
    try {
      switch (r.u8() % 3) {
        case 0 -> LAYER.load(r.bit() ? r.blob(1024) : null);
        case 1 -> {
          Opamp.AgentConfigMap.Builder m = Opamp.AgentConfigMap.newBuilder();
          for (int n = r.u8() % 3; n > 0; n--) {
            m.putConfigMap(r.text(16), Opamp.AgentConfigObject.newBuilder().setBody(r.bytes(1024))
                .setContentType(r.pick(List.of("", "application/json", "text/yaml", r.text(16)))).build());
          }
          LAYER.decide(Opamp.AgentRemoteConfig.newBuilder().setConfigHash(r.bytes(32)).setConfig(m).build(),
              r.bytes(512), r.bytes(512), null, 0, r.bit() ? doc -> { } : null);
        }
        default -> {
          List<Anyvalue.KeyValue> resource = new java.util.ArrayList<>();
          for (int n = r.u8() % 3; n > 0; n--) {
            resource.add(Anyvalue.KeyValue.newBuilder().setKey(r.text(16))
                .setValue(Anyvalue.AnyValue.newBuilder().setStringValue(r.text(32))).build());
          }
          LAYER.describe(resource);
        }
      }
    } catch (IllegalArgumentException e) {
      // refused, with the reason
    } catch (IllegalStateException e) {
      throw new AssertionError("the SDK settings layer trapped", e);
    }
  }

  private static byte[] seed(int... b) {
    byte[] out = new byte[b.length];
    for (int i = 0; i < b.length; i++) {
      out[i] = (byte) b[i];
    }
    return out;
  }

  static final List<byte[]> SEEDS = List.of(
      seed(),
      seed(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 0, 1, 1, 2, 1, 0, 3, 'a', 'b', 'c', 1),
      seed(1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 0, 5, 'h', 'e', 'l', 'l', 'o', 1, 0, 2, 0, 1,
          3, 0, 3, 0, 0, 0, 1, 8, 7, 0x0a, 3, 'x', 'y', 'z'));
}
