// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.time.Duration;
import java.time.Instant;
import java.time.ZoneOffset;
import java.time.format.DateTimeFormatter;
import java.util.List;
import java.util.Locale;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicLong;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;

/**
 * Core mode as a whole (the core speaking HTTP and WebSocket over the JDK's sockets): it reaches
 * real servers over plain HTTP and WebSocket, stops within its bound leaving nothing open, takes a
 * Retry-After date against the wall clock, and refuses the settings it can't apply.
 */
class CoreModeTest {
  static OpampClient.Config.Builder coreMode(String endpoint) {
    return OpampClient.Config.builder(endpoint, new byte[16]).mode(OpampClient.Mode.CORE).directConnection()
        .heartbeatInterval(Duration.ofSeconds(30));
  }

  @Test
  void reachesAServerOverHttpAndWebSocket() throws Exception {
    List<Opamp.AgentToServer> got = new CopyOnWriteArrayList<>();
    try (PropertiesTest.HttpOpamp http = new PropertiesTest.HttpOpamp(m -> {
      got.add(m);
      return Opamp.ServerToAgent.newBuilder().setInstanceUid(m.getInstanceUid()).build().toByteArray();
    }); PropertiesTest.WsServer ws = new PropertiesTest.WsServer(m -> {
      got.add(m);
      return Opamp.ServerToAgent.newBuilder().setInstanceUid(m.getInstanceUid()).build().toByteArray();
    })) {
      for (String endpoint : List.of("http://127.0.0.1:" + http.port() + "/v1/opamp", "ws://127.0.0.1:" + ws.port() + "/v1/opamp")) {
        got.clear();
        List<Notification> told = new CopyOnWriteArrayList<>();
        OpampClient c = OpampClient.start(coreMode(endpoint).build(), OpampClient.Status.of(
            Opamp.AgentDescription.getDefaultInstance()), told::add);
        for (long until = System.nanoTime() + 20_000_000_000L; got.isEmpty() && System.nanoTime() < until; ) {
          Thread.sleep(10);
        }
        assertTrue(!got.isEmpty(), endpoint + ": no message came: " + told);
        c.stop().get(10, TimeUnit.SECONDS);
        if (endpoint.startsWith("ws")) {
          assertTrue(told.get(0) instanceof Notification.Connected, told.toString());
        }
        assertTrue(got.size() >= 2, endpoint + ": the first message and the goodbye: " + got.size());
        assertTrue(got.get(got.size() - 1).hasAgentDisconnect(), endpoint + ": the goodbye last");
      }
    }
  }

  /**
   * A 503 whose Retry-After is an HTTP-date a few seconds ahead holds the next attempt (a
   * heartbeat's, every 100 ms) for about that long: the core reads the date against the wall
   * clock each step carries (not 1970, which makes it the hour's cap).
   */
  @Test
  void waitsUntilARetryAfterDate() throws Exception {
    AtomicLong refused = new AtomicLong();
    CompletableFuture<Long> again = new CompletableFuture<>();
    HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    server.createContext("/v1/opamp", ex -> {
      ex.getRequestBody().readAllBytes();
      long now = System.currentTimeMillis();
      if (refused.compareAndSet(0, now)) {
        ex.getResponseHeaders().set("Retry-After",
            DateTimeFormatter.ofPattern("EEE, dd MMM yyyy HH:mm:ss 'GMT'", Locale.US)
                .format(Instant.ofEpochMilli(now + 3_000).atZone(ZoneOffset.UTC)));
      } else {
        again.complete(now);
      }
      ex.sendResponseHeaders(503, -1);
      ex.close();
    });
    server.start();
    OpampClient c = OpampClient.start(coreMode("http://127.0.0.1:" + server.getAddress().getPort() + "/v1/opamp")
        .heartbeatInterval(Duration.ofMillis(100)).build(),
        OpampClient.Status.of(Opamp.AgentDescription.getDefaultInstance()), n -> { });
    try {
      long at;
      try {
        at = again.get(15, TimeUnit.SECONDS);
      } catch (java.util.concurrent.TimeoutException e) {
        throw new AssertionError("not tried again within 15 s of a Retry-After 3 s ahead");
      }
      // The date has whole seconds: 2 to 3 s after the 503.
      long waited = at - refused.get();
      assertTrue(waited >= 1_500, "tried again " + waited + " ms after a Retry-After 3 s ahead");
    } finally {
      c.stop().get(10, TimeUnit.SECONDS);
      server.stop(0);
    }
  }

  @Test
  void refusesWhatItCannotApply() {
    assertThrows(IllegalStateException.class, () -> coreMode("ws://x").httpClient(java.net.http.HttpClient.newHttpClient()).build());
    // SOCKS5 and https:// proxies are core mode's (named, or the default); the JDK's client
    // takes http:// only.
    OpampClient.Config.builder("ws://x").mode(OpampClient.Mode.CORE).proxy("socks5://u:p@proxy:1080").build();
    OpampClient.Config.builder("ws://x").mode(OpampClient.Mode.CORE).proxy("socks5h://proxy:1080").proxyResolveLocally(true).build();
    OpampClient.Config.builder("ws://x").mode(OpampClient.Mode.CORE).proxy("https://proxy:443").build();
    OpampClient.Config.builder("ws://x").proxy("socks5h://proxy:1080").proxyResolveLocally(true).build();
    assertThrows(IllegalArgumentException.class,
        () -> OpampClient.Config.builder("ws://x").mode(OpampClient.Mode.PLATFORM).proxy("socks5://proxy:1080").build());
    assertThrows(IllegalStateException.class, () -> OpampClient.Config.builder("ws://x").mode(OpampClient.Mode.PLATFORM)
        .proxy("socks5://proxy:1080").proxyResolveLocally(true).build());
    assertThrows(IllegalArgumentException.class, () -> OpampClient.Config.builder("ws://x").mode(OpampClient.Mode.CORE)
        .proxy("http://proxy:3128").proxyResolveLocally(true).build());
    for (String bad : List.of("socks4://proxy:1080", "ftp://proxy", "proxy:3128", "http://proxy:3128/path", "http://:3128")) {
      assertThrows(IllegalArgumentException.class, () -> OpampClient.Config.builder("ws://x").mode(OpampClient.Mode.CORE)
          .proxy(bad).build(), bad);
    }
  }

  /**
   * The mode a client runs in: core mode with none named, platform mode when asked for, by {@code
   * mode} or by an HTTP client of the application's (which only platform mode can use). Nothing one
   * mode alone honours is ignored: {@code build} refuses it, saying what to set.
   */
  @Test
  void theModeAClientRunsIn() {
    java.net.http.HttpClient own = java.net.http.HttpClient.newHttpClient();
    assertEquals(OpampClient.Mode.CORE, OpampClient.Config.builder("ws://x").build().mode);
    assertEquals(OpampClient.Mode.CORE, OpampClient.Config.builder("ws://x").mode(OpampClient.Mode.CORE).build().mode);
    assertEquals(OpampClient.Mode.CORE, OpampClient.Config.builder("ws://x").spliceDownloads(false).build().mode);
    assertEquals(OpampClient.Mode.PLATFORM, OpampClient.Config.builder("ws://x").mode(OpampClient.Mode.PLATFORM).build().mode);
    assertEquals(OpampClient.Mode.PLATFORM, OpampClient.Config.builder("ws://x").httpClient(own).build().mode);
    assertEquals(OpampClient.Mode.PLATFORM,
        OpampClient.Config.builder("ws://x").mode(OpampClient.Mode.PLATFORM).httpClient(own).build().mode);
    assertTrue(assertThrows(IllegalStateException.class,
        () -> OpampClient.Config.builder("ws://x").mode(OpampClient.Mode.CORE).httpClient(own).build())
        .getMessage().contains("httpClient"));
    assertTrue(assertThrows(IllegalStateException.class,
        () -> OpampClient.Config.builder("ws://x").mode(OpampClient.Mode.PLATFORM).spliceDownloads(false).build())
        .getMessage().contains("spliceDownloads is core mode's"));
    assertTrue(assertThrows(IllegalStateException.class,
        () -> OpampClient.Config.builder("ws://x").httpClient(own).spliceDownloads(false).build())
        .getMessage().contains("spliceDownloads is core mode's"));
    assertTrue(assertThrows(IllegalStateException.class,
        () -> OpampClient.Config.builder("ws://x").httpClient(own).proxyResolveLocally(true).build())
        .getMessage().contains("proxyResolveLocally is core mode's"));
  }

  /**
   * With no mode named the client's connection is core mode's: the proxy the environment names
   * (which core mode's rule reads, and the JDK's client never does) hears from it.
   */
  @Test
  void withNoModeNamedTheClientRunsInCoreMode() throws Exception {
    try (java.net.ServerSocket proxy = new java.net.ServerSocket(0, 50, java.net.InetAddress.getLoopbackAddress())) {
      java.util.Map<String, String> env = java.util.Map.of("HTTP_PROXY", "http://127.0.0.1:" + proxy.getLocalPort());
      for (OpampClient.Mode mode : new OpampClient.Mode[] {null, OpampClient.Mode.CORE, OpampClient.Mode.PLATFORM}) {
        OpampClient.Config.Builder b = OpampClient.Config.builder("http://opamp.invalid/v1/opamp", new byte[16])
            .environment(env).heartbeatInterval(Duration.ofSeconds(30));
        if (mode != null) {
          b.mode(mode);
        }
        proxy.setSoTimeout(mode == OpampClient.Mode.PLATFORM ? 1_500 : 20_000);
        OpampClient c = OpampClient.start(b.build(), OpampClient.Status.of(null), n -> {});
        boolean through = true;
        try (java.net.Socket s = proxy.accept()) {
          s.getInputStream().read();
        } catch (java.net.SocketTimeoutException none) {
          through = false;
        }
        c.stop().get(10, TimeUnit.SECONDS);
        assertEquals(mode != OpampClient.Mode.PLATFORM, through, "mode " + mode + ": through the environment's proxy");
      }
    }
  }
}
