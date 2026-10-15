// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertInstanceOf;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.sun.net.httpserver.Headers;
import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.time.Duration;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.TimeUnit;
import opamp.proto.v1.Anyvalue;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;

class OpampClientTest {
  private static final byte[] UID = new byte[16];
  private static final String UID_TEXT = "42424242-4242-4242-4242-424242424242";

  static {
    java.util.Arrays.fill(UID, (byte) 0x42);
  }

  /** The messages' sequence numbers. */
  static List<Long> sequence(List<Opamp.AgentToServer> got) {
    return got.stream().map(Opamp.AgentToServer::getSequenceNum).toList();
  }

  /**
   * The sequence numbers a run should have: 0 until the server has answered a message (those sent
   * before its first answer: RULE-CLIENT-20), then 1, 2, ...
   */
  static List<Long> expectedSequence(List<Opamp.AgentToServer> got) {
    int first = 0;
    while (first < got.size() && got.get(first).getSequenceNum() == 0) {
      first++;
    }
    int zeros = Math.max(1, first);
    List<Long> out = new java.util.ArrayList<>();
    for (int i = 0; i < got.size(); i++) {
      out.add(i < zeros ? 0L : (long) (i - zeros + 1));
    }
    return out;
  }

  @Test
  void httpReportsThenSaysGoodbyeAfterTheRequestInFlight() throws Exception {
    List<Opamp.AgentToServer> got = new CopyOnWriteArrayList<>();
    List<Headers> headers = new CopyOnWriteArrayList<>();
    HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    server.setExecutor(java.util.concurrent.Executors.newCachedThreadPool());
    server.createContext("/v1/opamp", ex -> {
      headers.add(ex.getRequestHeaders());
      Opamp.AgentToServer msg = Opamp.AgentToServer.parseFrom(ex.getRequestBody().readAllBytes());
      got.add(msg);
      if (got.size() == 2) { // hold a poll, so stop comes while it is in flight
        try {
          Thread.sleep(200);
        } catch (InterruptedException e) {
          Thread.currentThread().interrupt();
        }
      }
      byte[] reply = Opamp.ServerToAgent.newBuilder().setInstanceUid(msg.getInstanceUid()).build().toByteArray();
      ex.getResponseHeaders().set("Content-Type", "application/x-protobuf");
      ex.sendResponseHeaders(200, reply.length);
      ex.getResponseBody().write(reply);
      ex.close();
    });
    server.start();
    List<Notification> notes = new CopyOnWriteArrayList<>();
    String endpoint = "http://127.0.0.1:" + server.getAddress().getPort() + "/v1/opamp";
    Opamp.AgentDescription description = Opamp.AgentDescription.newBuilder()
        .addIdentifyingAttributes(Anyvalue.KeyValue.newBuilder()
            .setKey("service.name")
            .setValue(Anyvalue.AnyValue.newBuilder().setStringValue("test")))
        .build();
    OpampClient client = OpampClient.start(
        OpampClient.Config.builder(endpoint, UID).mode(OpampClient.Mode.JAVA)
            .heartbeatInterval(Duration.ofMillis(50))
            .header("Authorization", "Bearer t")
            .build(),
        OpampClient.Status.of(description),
        notes::add);
    for (int i = 0; i < 1000 && got.size() < 2; i++) {
      Thread.sleep(10);
    }
    client.stop().get(10, TimeUnit.SECONDS);
    server.stop(0);

    assertTrue(got.get(0).hasAgentDescription(), "the first message is a full report");
    assertTrue(got.get(got.size() - 1).hasAgentDisconnect(), "the last message says goodbye");
    assertEquals(expectedSequence(got), sequence(got), "sequence numbers 0 until the first answer, then one apart");
    for (Headers h : headers) {
      assertEquals("application/x-protobuf", h.getFirst("Content-Type"));
      assertEquals(UID_TEXT, h.getFirst("OpAMP-Instance-UID"));
      assertEquals("Bearer t", h.getFirst("Authorization"));
    }
    assertInstanceOf(Notification.Stopped.class, notes.get(notes.size() - 1));
  }

  // covers: OPAMP-SIZE-02, OPAMP-SIZE-08
  @Test
  void maxSendBytesReachesTheCore() throws Exception {
    List<Opamp.AgentToServer> got = new CopyOnWriteArrayList<>();
    HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    server.createContext("/v1/opamp", ex -> {
      Opamp.AgentToServer msg = Opamp.AgentToServer.parseFrom(ex.getRequestBody().readAllBytes());
      got.add(msg);
      byte[] reply = Opamp.ServerToAgent.newBuilder().setInstanceUid(msg.getInstanceUid()).build().toByteArray();
      ex.getResponseHeaders().set("Content-Type", "application/x-protobuf");
      ex.sendResponseHeaders(200, reply.length);
      ex.getResponseBody().write(reply);
      ex.close();
    });
    server.start();
    List<Notification> notes = new CopyOnWriteArrayList<>();
    String endpoint = "http://127.0.0.1:" + server.getAddress().getPort() + "/v1/opamp";
    Opamp.AgentDescription big = Opamp.AgentDescription.newBuilder()
        .addIdentifyingAttributes(Anyvalue.KeyValue.newBuilder()
            .setKey("big")
            .setValue(Anyvalue.AnyValue.newBuilder().setStringValue("x".repeat(4096))))
        .build();
    OpampClient client = OpampClient.start(
        OpampClient.Config.builder(endpoint, UID).mode(OpampClient.Mode.JAVA).maxSendBytes(1024).build(),
        OpampClient.Status.of(big),
        notes::add);
    for (int i = 0; i < 1000 && got.isEmpty(); i++) {
      Thread.sleep(10);
    }
    client.stop().get(10, TimeUnit.SECONDS);
    server.stop(0);

    assertTrue(!got.get(0).hasAgentDescription(), "a description too large is left out");
    assertTrue(notes.stream().anyMatch(n -> n instanceof Notification.StatusTooLarge s && s.field().equals("agent_description")),
        notes.toString());
  }
}
