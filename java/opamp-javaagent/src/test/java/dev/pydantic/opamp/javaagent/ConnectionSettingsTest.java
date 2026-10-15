// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import com.sun.net.httpserver.HttpServer;
import java.io.ByteArrayInputStream;
import java.net.InetSocketAddress;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;
import java.util.function.BooleanSupplier;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * A key of the agent's own (docs/connection-settings.md): with a state directory the agent says it
 * takes OpAMP connection settings, takes the key the server offers, says APPLIED on it, and keeps
 * it for the next start; without one it says nothing of the kind.
 */
class ConnectionSettingsTest {
  static final long TAKES_KEYS = Opamp.AgentCapabilities.AgentCapabilities_AcceptsOpAMPConnectionSettings_VALUE
      | Opamp.AgentCapabilities.AgentCapabilities_ReportsConnectionSettingsStatus_VALUE;
  static final ByteString OFFER = ByteString.copyFromUtf8("own-key-offer");

  /**
   * An OpAMP server over plain HTTP that takes an enrollment key and, as the reference server does,
   * hands an agent that says it takes connection settings a key of its own in its answer to the
   * agent's first message with the enrollment key; once the agent uses its own key, the enrollment
   * key speaks for it no more (401).
   */
  static final class KeyServer implements AutoCloseable {
    final HttpServer http;
    final List<Long> caps = new ArrayList<>();
    final List<String> auth = new ArrayList<>();
    boolean applied;
    boolean ownUsed;

    KeyServer() throws Exception {
      http = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
      http.createContext("/v1/opamp", x -> {
        Opamp.AgentToServer m = Opamp.AgentToServer.parseFrom(new ByteArrayInputStream(x.getRequestBody().readAllBytes()));
        String a = x.getRequestHeaders().getFirst("Authorization");
        Opamp.ServerToAgent.Builder r = Opamp.ServerToAgent.newBuilder().setInstanceUid(m.getInstanceUid());
        synchronized (this) {
          auth.add(a);
          boolean enroll = "Bearer enroll".equals(a);
          if (!enroll && !"Bearer own".equals(a) || enroll && ownUsed) {
            x.sendResponseHeaders(401, -1);
            x.close();
            return;
          }
          ownUsed |= !enroll;
          caps.add(m.getCapabilities());
          Opamp.ConnectionSettingsStatus s = m.getConnectionSettingsStatus();
          if (!enroll && m.hasConnectionSettingsStatus()
              && s.getStatus() == Opamp.ConnectionSettingsStatuses.ConnectionSettingsStatuses_APPLIED
              && s.getLastConnectionSettingsHash().equals(OFFER)) {
            applied = true;
          }
          if (enroll && (m.getCapabilities() & TAKES_KEYS) == TAKES_KEYS) {
            r.setConnectionSettings(Opamp.ConnectionSettingsOffers.newBuilder().setHash(OFFER)
                .setOpamp(Opamp.OpAMPConnectionSettings.newBuilder().setDestinationEndpoint(endpoint())
                    .setHeaders(Opamp.Headers.newBuilder().addHeaders(
                        Opamp.Header.newBuilder().setKey("Authorization").setValue("Bearer own")))
                    .setHeartbeatIntervalSeconds(1)));
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

  /** An agent under {@code server} with the enrollment key, until {@code until} holds. */
  static void runAgent(KeyServer server, Path dir, String what, BooleanSupplier until, String... more) throws Exception {
    Extension e = Extension.forTest(null);
    List<String> props = new ArrayList<>(List.of("otel.opamp.headers", "Authorization=Bearer%20enroll",
        "otel.opamp.heartbeat.interval", "100ms"));
    props.addAll(List.of(more));
    try (InventoryTest.Sdk sdk = InventoryTest.sdk(e, InventoryTest.props(server.endpoint(), dir,
        props.toArray(String[]::new)))) {
      e.start(null);
      server.await(what, until);
      e.stopForTest();
    }
  }

  @Test
  void withAStateDirectoryTheAgentTakesAKeyOfItsOwnAndKeepsIt(@TempDir Path dir) throws Exception {
    try (KeyServer server = new KeyServer()) {
      runAgent(server, dir, "the key offered was never APPLIED on it", () -> server.applied);
      int before;
      synchronized (server) {
        assertEquals(TAKES_KEYS, server.caps.get(0) & TAKES_KEYS, "it says it takes connection settings");
        before = server.auth.size();
      }
      runAgent(server, dir, "the next start never reached the server", () -> server.auth.size() > before);
      synchronized (server) {
        assertEquals("Bearer own", server.auth.get(before), "the next start comes with the key it was given");
      }
    }
  }

  @Test
  void withoutAStateDirectoryTheAgentDoesntSayItTakesConnectionSettings(@TempDir Path dir) throws Exception {
    try (KeyServer server = new KeyServer()) {
      runAgent(server, dir, "no message reached the server", () -> server.caps.size() >= 2,
          "otel.opamp.state.dir", "none");
      synchronized (server) {
        assertTrue(server.caps.stream().allMatch(c -> (c & TAKES_KEYS) == 0), server.caps.toString());
        assertTrue(server.auth.stream().allMatch("Bearer enroll"::equals), server.auth.toString());
      }
    }
  }
}
