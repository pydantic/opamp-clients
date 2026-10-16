// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;

import com.google.protobuf.ByteString;
import com.sun.net.httpserver.HttpServer;
import java.io.ByteArrayInputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;
import java.util.function.BooleanSupplier;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * The config applied through a server is where the next start begins: the client keeps it with its
 * state, and tells the server APPLIED once that is saved. The agent keeps no file of its own.
 */
class KeptAppliedTest {
  /**
   * An OpAMP server over plain HTTP that offers {@code offer} (an {@code elastic} settings
   * document), if given, in its first answer: whether it heard APPLIED, and the settings each first
   * message reported in force.
   */
  static final class ConfigServer implements AutoCloseable {
    final HttpServer http;
    final List<String> firstEffective = new ArrayList<>();
    int polls;
    boolean applied;

    ConfigServer(String offer) throws Exception {
      http = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
      http.createContext("/v1/opamp", x -> {
        Opamp.AgentToServer m = Opamp.AgentToServer.parseFrom(new ByteArrayInputStream(x.getRequestBody().readAllBytes()));
        Opamp.ServerToAgent.Builder r = Opamp.ServerToAgent.newBuilder().setInstanceUid(m.getInstanceUid());
        synchronized (this) {
          polls++;
          if (m.getRemoteConfigStatus().getStatus() == Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED) {
            applied = true;
          }
          var elastic = m.getEffectiveConfig().getConfigMap().getConfigMapMap().get("elastic");
          if (m.getSequenceNum() == 0 && elastic != null) {
            firstEffective.add(elastic.getBody().toString(StandardCharsets.UTF_8));
          }
          if (offer != null && polls == 1) {
            ByteString body = ByteString.copyFromUtf8(offer);
            r.setRemoteConfig(Opamp.AgentRemoteConfig.newBuilder().setConfigHash(body)
                .setConfig(Opamp.AgentConfigMap.newBuilder().putConfigMap("elastic",
                    Opamp.AgentConfigObject.newBuilder().setBody(body).setContentType("application/json").build())));
          }
        }
        byte[] body = r.build().toByteArray();
        x.getResponseHeaders().set("Content-Type", "application/x-protobuf");
        x.sendResponseHeaders(200, body.length);
        x.getResponseBody().write(body);
        x.close();
      });
      http.start();
    }

    String endpoint() {
      return "http://127.0.0.1:" + http.getAddress().getPort() + "/v1/opamp";
    }

    void await(String what, BooleanSupplier holds) throws InterruptedException {
      long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(20);
      while (true) {
        synchronized (this) {
          if (holds.getAsBoolean()) {
            return;
          }
        }
        if (System.nanoTime() > deadline) {
          throw new AssertionError(what);
        }
        Thread.sleep(20);
      }
    }

    @Override
    public void close() {
      http.stop(0);
    }
  }

  static void runAgent(ConfigServer server, Path dir, String what, BooleanSupplier until) throws Exception {
    Extension e = Extension.forTest(null);
    try (InventoryTest.Sdk sdk = InventoryTest.sdk(e, InventoryTest.props(server.endpoint(), dir,
        "otel.opamp.heartbeat.interval", "100ms"))) {
      e.start(null);
      server.await(what, until);
      e.stopForTest();
    }
  }

  @Test
  void aConfigAppliedThroughTheServerIsWhereTheNextStartBegins(@TempDir Path dir) throws Exception {
    try (ConfigServer first = new ConfigServer("{\"sampling_rate\":\"0.5\"}")) {
      runAgent(first, dir, "the config offered was never APPLIED", () -> first.applied);
    }
    assertFalse(Files.exists(dir.resolve("sdk-settings.json")));
    // The next start, against a server that sends nothing: the settings applied, in force from the first message.
    try (ConfigServer second = new ConfigServer(null)) {
      runAgent(second, dir, "the next start never reached the server", () -> !second.firstEffective.isEmpty());
      synchronized (second) {
        assertEquals("{\"sampling_rate\":\"0.5\"}", second.firstEffective.get(0));
      }
    }
  }
}
