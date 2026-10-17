// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import com.sun.net.httpserver.HttpServer;
import java.io.ByteArrayInputStream;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;
import java.util.function.BooleanSupplier;
import java.util.function.Consumer;
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
   * key speaks for it no more (401). Once the agent has said APPLIED of its own key, it offers
   * {@link #next} (if set) in answer to each message with it: another key, here or at another
   * server's endpoint.
   */
  static final class KeyServer implements AutoCloseable {
    final HttpServer http;
    final List<Long> caps = new ArrayList<>();
    final List<String> auth = new ArrayList<>();
    /** Each status reported: its status and the hash it names. */
    final List<Map.Entry<Opamp.ConnectionSettingsStatuses, ByteString>> statuses = new ArrayList<>();
    boolean applied;
    boolean ownUsed;
    /** The keys it takes besides the enrollment key, which it takes only with {@link #enroll}. */
    List<String> keys = List.of("own");
    boolean enroll = true;
    /** Offered in answer to each message with "own" once the agent said APPLIED of it. */
    Opamp.ConnectionSettingsOffers next;
    /** Once a request comes with this key, "own" speaks for the agent no more. */
    String retires;
    boolean ownGone;

    KeyServer() throws Exception {
      http = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
      http.createContext("/v1/opamp", x -> {
        Opamp.AgentToServer m = Opamp.AgentToServer.parseFrom(new ByteArrayInputStream(x.getRequestBody().readAllBytes()));
        String a = x.getRequestHeaders().getFirst("Authorization");
        Opamp.ServerToAgent.Builder r = Opamp.ServerToAgent.newBuilder().setInstanceUid(m.getInstanceUid());
        synchronized (this) {
          auth.add(a);
          boolean enrolling = "Bearer enroll".equals(a);
          String key = a != null && a.startsWith("Bearer ") ? a.substring("Bearer ".length()) : "";
          boolean taken = enrolling && enroll && !ownUsed || keys.contains(key) && !(key.equals("own") && ownGone);
          if (!taken) {
            x.sendResponseHeaders(401, -1);
            x.close();
            return;
          }
          ownUsed |= key.equals("own");
          ownGone |= key.equals(retires);
          caps.add(m.getCapabilities());
          Opamp.ConnectionSettingsStatus s = m.getConnectionSettingsStatus();
          if (m.hasConnectionSettingsStatus()) {
            statuses.add(Map.entry(s.getStatus(), s.getLastConnectionSettingsHash()));
          }
          if (key.equals("own") && m.hasConnectionSettingsStatus()
              && s.getStatus() == Opamp.ConnectionSettingsStatuses.ConnectionSettingsStatuses_APPLIED
              && s.getLastConnectionSettingsHash().equals(OFFER)) {
            applied = true;
          }
          if (enrolling && (m.getCapabilities() & TAKES_KEYS) == TAKES_KEYS) {
            r.setConnectionSettings(Opamp.ConnectionSettingsOffers.newBuilder().setHash(OFFER)
                .setOpamp(Opamp.OpAMPConnectionSettings.newBuilder().setDestinationEndpoint(endpoint())
                    .setHeaders(Opamp.Headers.newBuilder().addHeaders(
                        Opamp.Header.newBuilder().setKey("Authorization").setValue("Bearer own")))
                    .setHeartbeatIntervalSeconds(1)));
          } else if (key.equals("own") && next != null && applied) {
            r.setConnectionSettings(next);
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

    /** Whether the agent reported {@code status} of the offer {@code hash} here (under the lock). */
    boolean said(Opamp.ConnectionSettingsStatuses status, String hash) {
      return statuses.contains(Map.entry(status, ByteString.copyFromUtf8(hash)));
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
    runAgent(server, server, dir, what, until, e -> { }, more);
  }

  /**
   * An agent configured with {@code server} and the enrollment key, until {@code until} holds of
   * {@code watched} (under its lock); {@code then} is given the running agent.
   */
  static void runAgent(KeyServer server, KeyServer watched, Path dir, String what, BooleanSupplier until,
      Consumer<Extension> then, String... more) throws Exception {
    Extension e = Extension.forTest(null);
    List<String> props = new ArrayList<>(List.of("otel.opamp.headers", "Authorization=Bearer%20enroll",
        "otel.opamp.heartbeat.interval", "100ms"));
    props.addAll(List.of(more));
    try (InventoryTest.Sdk sdk = InventoryTest.sdk(e, InventoryTest.props(server.endpoint(), dir,
        props.toArray(String[]::new)))) {
      e.start(null);
      watched.await(what, until);
      then.accept(e);
      e.stopForTest();
    }
  }

  static Opamp.ConnectionSettingsOffers offer(String hash, String endpoint, String key) {
    return Opamp.ConnectionSettingsOffers.newBuilder().setHash(ByteString.copyFromUtf8(hash))
        .setOpamp(Opamp.OpAMPConnectionSettings.newBuilder().setDestinationEndpoint(endpoint)
            .setHeaders(Opamp.Headers.newBuilder().addHeaders(
                Opamp.Header.newBuilder().setKey("Authorization").setValue("Bearer " + key))))
        .build();
  }

  static final Opamp.ConnectionSettingsStatuses APPLIED = Opamp.ConnectionSettingsStatuses.ConnectionSettingsStatuses_APPLIED;
  static final Opamp.ConnectionSettingsStatuses FAILED = Opamp.ConnectionSettingsStatuses.ConnectionSettingsStatuses_FAILED;

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

  /**
   * Once it has its own key, A offers another key at server B's endpoint: the agent goes to B with
   * it, says APPLIED of the move there, and the next start goes to B with it first.
   */
  @Test
  void theAgentFollowsAMoveToAnotherServerAndKeepsIt(@TempDir Path dir) throws Exception {
    try (KeyServer a = new KeyServer(); KeyServer b = new KeyServer()) {
      synchronized (b) {
        b.keys = List.of("moved");
        b.enroll = false;
      }
      synchronized (a) {
        a.next = offer("move-to-b", b.endpoint(), "moved");
      }
      runAgent(a, b, dir, "the move was never APPLIED at B", () -> b.said(APPLIED, "move-to-b"),
          // What the agent says of reaching its server names B from now on.
          e -> assertEquals(b.endpoint(), e.endpointSaidForTest()));
      int atA;
      int atB;
      synchronized (b) {
        assertTrue(b.auth.stream().allMatch("Bearer moved"::equals), b.auth.toString());
        atB = b.auth.size();
      }
      synchronized (a) {
        atA = a.auth.size();
      }
      runAgent(a, b, dir, "the next start never reached B", () -> b.auth.size() > atB, e -> { });
      synchronized (a) {
        assertEquals(atA, a.auth.size(), "the next start went to A, not to B");
      }
      synchronized (b) {
        assertEquals("Bearer moved", b.auth.get(atB), "the next start comes to B with the key it was moved with");
      }
    }
  }

  /** A second key at the same endpoint: the agent takes it, keeps it across a restart, and never sends "own" again. */
  @Test
  void theAgentTakesASecondKeyAndKeepsIt(@TempDir Path dir) throws Exception {
    try (KeyServer server = new KeyServer()) {
      synchronized (server) {
        server.keys = List.of("own", "own2");
        server.retires = "own2";
        server.next = offer("second-key", server.endpoint(), "own2");
      }
      int[] since = {-1};
      runAgent(server, dir, "the second key was never APPLIED", () -> {
        if (since[0] < 0 && server.said(APPLIED, "second-key")) {
          since[0] = server.auth.size();
        }
        return since[0] >= 0 && server.auth.size() > since[0] + 2;
      });
      int before;
      synchronized (server) {
        before = server.auth.size();
      }
      runAgent(server, dir, "the next start never reached the server", () -> server.auth.size() > before + 2);
      synchronized (server) {
        List<String> after = server.auth.subList(since[0], server.auth.size());
        assertTrue(after.stream().allMatch("Bearer own2"::equals), after.toString());
      }
    }
  }

  /** A move nobody answers: the agent goes back with the key it has, says FAILED of it, and carries on. */
  @Test
  void aMoveNobodyAnswersFailsAndTheAgentStaysWhereItWas(@TempDir Path dir) throws Exception {
    String nowhere;
    try (ServerSocket closed = new ServerSocket(0, 1, InetAddress.getByName("127.0.0.1"))) {
      nowhere = "http://127.0.0.1:" + closed.getLocalPort() + "/v1/opamp";
    }
    try (KeyServer server = new KeyServer()) {
      synchronized (server) {
        server.next = offer("move-nowhere", nowhere, "moved");
      }
      int[] since = {-1};
      runAgent(server, dir, "the move was never FAILED, or the agent did not carry on", () -> {
        if (since[0] < 0 && server.said(FAILED, "move-nowhere")) {
          since[0] = server.auth.size();
        }
        return since[0] >= 0 && server.auth.size() > since[0] + 2;
      });
      synchronized (server) {
        List<String> after = server.auth.subList(since[0], server.auth.size());
        assertTrue(after.stream().allMatch("Bearer own"::equals), after.toString());
        assertFalse(server.said(APPLIED, "move-nowhere"));
      }
    }
  }
}
