// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

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
   * An OpAMP server over plain HTTP that offers {@code offer} (a policy), if given, in its first
   * answer: whether it heard APPLIED, the policy each first message reported in force, and the
   * description each first message carried.
   */
  static final class ConfigServer implements AutoCloseable {
    final HttpServer http;
    final List<String> firstEffective = new ArrayList<>();
    final List<Opamp.AgentDescription> descriptions = new ArrayList<>();
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
          var policy = m.getEffectiveConfig().getConfigMap().getConfigMapMap().get(Vectors.POLICY_FILE);
          if (m.getSequenceNum() == 0 && policy != null) {
            firstEffective.add(policy.getBody().toString(StandardCharsets.UTF_8));
          }
          if (m.getSequenceNum() == 0 && m.hasAgentDescription()) {
            descriptions.add(m.getAgentDescription());
          }
          if (offer != null && polls == 1) {
            ByteString body = ByteString.copyFromUtf8(offer);
            r.setRemoteConfig(Opamp.AgentRemoteConfig.newBuilder().setConfigHash(body)
                .setConfig(Opamp.AgentConfigMap.newBuilder().putConfigMap(Vectors.POLICY_FILE,
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
    String half = Vectors.v1("head_sampling", Vectors.override("\"ratio\":0.5"));
    try (ConfigServer first = new ConfigServer(half)) {
      runAgent(first, dir, "the config offered was never APPLIED", () -> first.applied);
    }
    assertFalse(Files.exists(dir.resolve("sdk-settings.json")));
    // The next start, against a server that sends nothing: the settings applied, in force from the first message.
    try (ConfigServer second = new ConfigServer(null)) {
      runAgent(second, dir, "the next start never reached the server", () -> !second.firstEffective.isEmpty());
      synchronized (second) {
        assertEquals(Vectors.full(half), second.firstEffective.get(0));
      }
    }
  }

  /**
   * The description the agent sends at start names the policy format the layer reads and the
   * controls the agent carries out.
   */
  @Test
  void theDescriptionNamesThePolicyAndTheControls(@TempDir Path dir) throws Exception {
    try (ConfigServer server = new ConfigServer(null)) {
      runAgent(server, dir, "no description sent", () -> !server.descriptions.isEmpty());
      java.util.Map<String, String> attrs = new java.util.HashMap<>();
      synchronized (server) {
        for (var kv : server.descriptions.get(0).getNonIdentifyingAttributesList()) {
          attrs.put(kv.getKey(), kv.getValue().hasArrayValue()
              ? String.join(",", kv.getValue().getArrayValue().getValuesList().stream()
                  .map(opamp.proto.v1.Anyvalue.AnyValue::getStringValue).toList())
              : kv.getValue().getStringValue());
        }
      }
      assertEquals("logfire-sdk-policy/v1", attrs.get("logfire.opamp.config_format"));
      assertEquals("head_sampling,signal_exports.traces,signal_exports.metrics,signal_exports.logs,sampling_rules,"
          + "trace_rate_limit,log_levels,log_rate_limit,attribute_redaction,debug,instrumentations",
          attrs.get("logfire.opamp.supported_controls"));
    }
  }
}
