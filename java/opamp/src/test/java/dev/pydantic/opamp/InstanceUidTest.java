// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.nio.ByteBuffer;
import java.time.Duration;
import java.util.Arrays;
import java.util.List;
import java.util.UUID;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.TimeUnit;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;

class InstanceUidTest {
  // No UID chosen: the core makes a UUIDv7 from the clock and the platform's randomness, and the
  // client says which (the UID in the first message too).
  // covers: RULE-CLIENT-19
  @Test
  void withoutAUidTheCoreMakesOne() throws Exception {
    assertNull(OpampClient.Config.builder("http://x").build().instanceUid, "the client makes none");
    List<Opamp.AgentToServer> got = new CopyOnWriteArrayList<>();
    HttpServer server = server(got, null);
    String endpoint = "http://127.0.0.1:" + server.getAddress().getPort() + "/v1/opamp";
    OpampClient a = OpampClient.start(OpampClient.Config.builder(endpoint).mode(OpampClient.Mode.JAVA).build(), OpampClient.Status.of(null), n -> {});
    OpampClient b = OpampClient.start(OpampClient.Config.builder("http://127.0.0.1:1").mode(OpampClient.Mode.JAVA).build(), OpampClient.Status.of(null), n -> {});
    for (int i = 0; i < 1000 && got.isEmpty(); i++) {
      Thread.sleep(10);
    }
    a.stop().get(10, TimeUnit.SECONDS);
    b.stop().get(10, TimeUnit.SECONDS);
    server.stop(0);
    assertFalse(Arrays.equals(a.instanceUid(), b.instanceUid()));
    for (byte[] u : new byte[][] {a.instanceUid(), b.instanceUid()}) {
      ByteBuffer buf = ByteBuffer.wrap(u);
      UUID id = new UUID(buf.getLong(), buf.getLong());
      assertEquals(7, id.version());
      assertEquals(2, id.variant());
      long ms = id.getMostSignificantBits() >>> 16;
      assertTrue(Math.abs(System.currentTimeMillis() - ms) < 60_000);
    }
    assertArrayEquals(a.instanceUid(), got.get(0).getInstanceUid().toByteArray(), "the one the server hears");
    // Chosen: the application's, as it is.
    byte[] chosen = new byte[16];
    OpampClient c = OpampClient.start(OpampClient.Config.builder("http://127.0.0.1:1", chosen).mode(OpampClient.Mode.JAVA).build(),
        OpampClient.Status.of(null), n -> {});
    assertArrayEquals(chosen, c.instanceUid());
    c.stop().get(10, TimeUnit.SECONDS);
  }

  /** A server that answers every message, assigning {@code assigned} when asked to. */
  private static HttpServer server(List<Opamp.AgentToServer> got, byte[] assigned) throws java.io.IOException {
    HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    server.setExecutor(java.util.concurrent.Executors.newCachedThreadPool());
    server.createContext("/v1/opamp", ex -> {
      Opamp.AgentToServer msg = Opamp.AgentToServer.parseFrom(ex.getRequestBody().readAllBytes());
      got.add(msg);
      Opamp.ServerToAgent.Builder reply = Opamp.ServerToAgent.newBuilder().setInstanceUid(msg.getInstanceUid());
      if (assigned != null && (msg.getFlags() & Opamp.AgentToServerFlags.AgentToServerFlags_RequestInstanceUid_VALUE) != 0) {
        reply.setAgentIdentification(
            Opamp.AgentIdentification.newBuilder().setNewInstanceUid(ByteString.copyFrom(assigned)));
      }
      byte[] body = reply.build().toByteArray();
      ex.getResponseHeaders().set("Content-Type", "application/x-protobuf");
      ex.sendResponseHeaders(200, body.length);
      ex.getResponseBody().write(body);
      ex.close();
    });
    server.start();
    return server;
  }

  // requestInstanceUid reaches the core: the client asks until the server assigns a UID, then
  // uses it; without the option it never asks.
  // covers: OPAMP-UID-04
  @ParameterizedTest
  @ValueSource(booleans = {true, false})
  void requestInstanceUidAsksTheServer(boolean on) throws Exception {
    byte[] assigned = new byte[16];
    Arrays.fill(assigned, (byte) 0x77);
    List<Opamp.AgentToServer> got = new CopyOnWriteArrayList<>();
    HttpServer server = server(got, assigned);
    List<Notification> notes = new CopyOnWriteArrayList<>();
    String endpoint = "http://127.0.0.1:" + server.getAddress().getPort() + "/v1/opamp";
    OpampClient client = OpampClient.start(
        OpampClient.Config.builder(endpoint).mode(OpampClient.Mode.JAVA)
            .heartbeatInterval(Duration.ofMillis(50))
            .requestInstanceUid(on)
            .build(),
        OpampClient.Status.of(null),
        notes::add);
    for (int i = 0; i < 1000 && got.size() < 3; i++) {
      Thread.sleep(10);
    }
    client.stop().get(10, TimeUnit.SECONDS);
    server.stop(0);

    assertArrayEquals(on ? assigned : got.get(0).getInstanceUid().toByteArray(), client.instanceUid(),
        "the client says the UID in use");
    assertEquals(on, (got.get(0).getFlags() & 1) != 0, "the first message asks for a UID");
    for (Opamp.AgentToServer m : got.subList(1, got.size())) {
      assertEquals(0, m.getFlags() & 1, "later messages don't ask");
    }
    byte[] last = got.get(got.size() - 1).getInstanceUid().toByteArray();
    if (on) {
      assertArrayEquals(assigned, last, "the assigned UID is used");
      assertTrue(notes.stream().anyMatch(n -> n instanceof Notification.InstanceUidChanged),
          "the application hears of it");
    } else {
      assertFalse(Arrays.equals(assigned, last));
    }
  }
}
