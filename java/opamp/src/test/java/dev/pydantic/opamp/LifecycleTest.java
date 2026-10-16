// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertInstanceOf;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import com.sun.net.httpserver.HttpServer;
import java.io.BufferedInputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.time.Duration;
import java.util.List;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import java.util.function.Function;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * The client's lifetime around the core: stop ends when the core says, nothing it started
 * outlives it, and what the shell itself could get wrong (the audit's S1 to S6) doesn't happen.
 */
class LifecycleTest {
  /** An OpAMP server over plain HTTP whose answer to each message is {@code answer}'s. */
  static HttpServer opamp(List<Opamp.AgentToServer> got, Function<Opamp.AgentToServer, Opamp.ServerToAgent.Builder> answer)
      throws Exception {
    HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    server.setExecutor(java.util.concurrent.Executors.newCachedThreadPool());
    server.createContext("/v1/opamp", ex -> {
      Opamp.AgentToServer msg = Opamp.AgentToServer.parseFrom(ex.getRequestBody().readAllBytes());
      got.add(msg);
      byte[] body = answer.apply(msg).setInstanceUid(msg.getInstanceUid()).build().toByteArray();
      ex.getResponseHeaders().set("Content-Type", "application/x-protobuf");
      ex.sendResponseHeaders(200, body.length);
      ex.getResponseBody().write(body);
      ex.close();
    });
    server.start();
    return server;
  }

  static String endpoint(HttpServer s) {
    return "http://127.0.0.1:" + s.getAddress().getPort() + "/v1/opamp";
  }

  static void waitFor(java.util.function.BooleanSupplier done, long ms) throws InterruptedException {
    for (long end = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(ms); !done.getAsBoolean() && System.nanoTime() < end; ) {
      Thread.sleep(10);
    }
  }

  // Stop: the core waits for the request in flight (the goodbye is owed), up to its stop timeout,
  // then cancels it and says it has stopped. The client neither ends sooner nor waits longer.
  @Test
  void stopEndsWhenTheCoreSays() throws Exception {
    try (Raw server = new Raw(Raw::hold)) {
      List<Notification> notes = new CopyOnWriteArrayList<>();
      OpampClient client = OpampClient.start(
          OpampClient.Config.builder(server.url("http") + "/v1/opamp").mode(OpampClient.Mode.JAVA).stopTimeout(Duration.ofMillis(500)).build(),
          OpampClient.Status.of(null), notes::add);
      Socket s = server.held();
      long t0 = System.nanoTime();
      client.stop().get(10, TimeUnit.SECONDS);
      long ms = TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - t0);
      assertTrue(ms >= 400 && ms < 3000, "stop took " + ms + " ms");
      assertTrue(Raw.closedWithin(s, 5000), "the request is cancelled");
      assertInstanceOf(Notification.Stopped.class, notes.get(notes.size() - 1));
    }
  }

  // A negative duration is refused at start by the core, naming the field and the number meant, as
  // in every SDK. (It reached the core as an unsigned number once, some 584 million years: a stop
  // that never returned; then it was taken as the default here, while other SDKs threw.) Less
  // than a millisecond is 0, the default. A duration past a long's milliseconds is the longest
  // there is, not an ArithmeticException from start.
  @Test
  void aNegativeDurationIsRefused() throws Exception {
    assertEquals(0, OpampClient.ms(Duration.ofNanos(-500_000)));
    assertEquals(-30_000, OpampClient.ms(Duration.ofSeconds(-30)));
    assertEquals(1500, OpampClient.ms(Duration.ofMillis(1500)));
    assertEquals(Long.MAX_VALUE, OpampClient.ms(Duration.ofSeconds(Long.MAX_VALUE)));
    assertEquals(Long.MIN_VALUE, OpampClient.ms(Duration.ofSeconds(Long.MIN_VALUE)));
    String url = "http://127.0.0.1:9/v1/opamp";
    for (var c : java.util.Map.of(
        "stop_timeout_ms is -5000: negative", OpampClient.Config.builder(url).stopTimeout(Duration.ofSeconds(-5)),
        "request_timeout_ms is -1: negative", OpampClient.Config.builder(url).requestTimeout(Duration.ofMillis(-1)),
        "heartbeat_interval_ms is -30000: negative",
        OpampClient.Config.builder(url).heartbeatInterval(Duration.ofSeconds(-30))).entrySet()) {
      RuntimeException e = assertThrows(RuntimeException.class,
          () -> OpampClient.start(c.getValue().build(), OpampClient.Status.of(null), n -> {}));
      assertTrue(e.getMessage().contains(c.getKey()), e.getMessage());
    }
  }

  // All of stop takes the stop timeout the core goes by: its start reports it, the default (5 s)
  // too. A server that never answers holds the request until then.
  @Test
  void stopTakesTheCoresDefaultStopTimeout() throws Exception {
    try (Raw server = new Raw(Raw::hold)) {
      OpampClient client = OpampClient.start(
          OpampClient.Config.builder(server.url("http") + "/v1/opamp").build(),
          OpampClient.Status.of(null), n -> {});
      server.held();
      long t0 = System.nanoTime();
      client.stop().get(20, TimeUnit.SECONDS);
      long ms = TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - t0);
      assertTrue(ms >= 4000 && ms < 8000, "stop took " + ms + " ms, want the default bound (5 s)");
    }
  }

  // S1: stop while the WebSocket is still being opened. The server answers the upgrade 2 s later:
  // stop returns, and that connection is closed at once, not kept.
  @Test
  void aWebSocketThatOpensAfterStopIsClosed() throws Exception {
    try (Raw server = new Raw(s -> {
      InputStream in = new BufferedInputStream(s.getInputStream());
      TransportTest.Head h = TransportTest.Head.read(in);
      Thread.sleep(2000);
      Raw.HELD.add(s);
      Raw.upgrade(s.getOutputStream(), h);
    })) {
      OpampClient client = OpampClient.start(
          OpampClient.Config.builder(server.url("ws") + "/v1/opamp", new byte[16]).mode(OpampClient.Mode.JAVA).build(), OpampClient.Status.of(null), n -> {});
      Thread.sleep(300);
      client.stop().get(5, TimeUnit.SECONDS);
      assertTrue(Raw.closedWithin(server.held(), 5000), "the late connection is closed");
    }
  }

  // S2: a server that takes the connection and never answers the upgrade. The core's connect
  // deadline gives up on it (and the cancel really drops it), and the client tries again.
  @Test
  void aHandshakeThatNeverAnswersIsGivenUp() throws Exception {
    try (Raw server = new Raw(Raw::hold)) {
      OpampClient client = OpampClient.start(
          OpampClient.Config.builder(server.url("ws") + "/v1/opamp", new byte[16]).mode(OpampClient.Mode.JAVA)
              .connectTimeout(Duration.ofMillis(CONNECT_TIMEOUT_MS)).build(),
          OpampClient.Status.of(null), n -> {});
      try {
        Socket first = server.held();
        assertTrue(Raw.closedWithin(first, CONNECT_TIMEOUT_MS + 5000), "the first attempt is dropped");
        assertNotNull(Raw.HELD.poll(SECOND_ATTEMPT_S, TimeUnit.SECONDS), "a second attempt");
      } finally {
        client.stop().get(10, TimeUnit.SECONDS);
      }
    }
  }

  static final long CONNECT_TIMEOUT_MS = 500;
  static final long SECOND_ATTEMPT_S = 20;

  // S3: a download of a client stopped while it waits for the file's headers, then a new client
  // on the same state file downloading the same package (with the same effect id). The old
  // download's response comes after: the file the new client hands over is still its own.
  @Test
  void anOldClientsDownloadLeavesTheNewClientsFileAlone(@TempDir Path dir) throws Exception {
    byte[] file = "the package".repeat(20).getBytes(StandardCharsets.US_ASCII);
    CountDownLatch release = new CountDownLatch(1);
    CountDownLatch firstAsked = new CountDownLatch(1);
    try (Raw files = new Raw(s -> {
      InputStream in = new BufferedInputStream(s.getInputStream());
      TransportTest.Head.read(in);
      OutputStream out = s.getOutputStream();
      byte[] body = file;
      if (firstAsked.getCount() > 0) { // the first client's: held, then the wrong bytes
        firstAsked.countDown();
        release.await();
        body = "WRONG".repeat(40).getBytes(StandardCharsets.US_ASCII);
      }
      out.write(("HTTP/1.1 200 OK\r\nContent-Length: " + body.length + "\r\nConnection: close\r\n\r\n").getBytes(StandardCharsets.US_ASCII));
      out.write(body);
      out.flush();
      s.close();
    })) {
      Opamp.PackagesAvailable offer = Opamp.PackagesAvailable.newBuilder()
          .putPackages("p", Opamp.PackageAvailable.newBuilder()
              .setType(Opamp.PackageType.PackageType_TopLevel).setVersion("1")
              .setHash(ByteString.copyFromUtf8("p1"))
              .setFile(Opamp.DownloadableFile.newBuilder()
                  .setDownloadUrl(files.url("http") + "/p")
                  .setContentHash(ByteString.copyFrom(MessageDigest.getInstance("SHA-256").digest(file))))
              .build())
          .setAllPackagesHash(ByteString.copyFromUtf8("all1"))
          .build();
      HttpServer server = opamp(new CopyOnWriteArrayList<>(), m -> Opamp.ServerToAgent.newBuilder().setPackagesAvailable(offer));
      Path state = dir.resolve("state");
      OpampClient.Config config = OpampClient.Config.builder(endpoint(server)).mode(OpampClient.Mode.JAVA)
          .heartbeatInterval(Duration.ofMillis(100)).stateFile(state).acceptPackages(true).build();
      OpampClient a = OpampClient.start(config, OpampClient.Status.of(null), n -> {});
      assertTrue(firstAsked.await(20, TimeUnit.SECONDS), "the first client downloads");
      a.stop().get(10, TimeUnit.SECONDS);

      CompletableFuture<byte[]> handed = new CompletableFuture<>();
      OpampClient b = OpampClient.start(config, OpampClient.Status.of(null), n -> {
        if (n instanceof Notification.InstallPackage ip) {
          release.countDown(); // the old download's response comes now
          try {
            Thread.sleep(500);
            handed.complete(Files.readAllBytes(ip.path()));
          } catch (Exception e) {
            handed.completeExceptionally(e);
          }
        }
      });
      try {
        assertArrayEquals(file, handed.get(20, TimeUnit.SECONDS), "the file handed over");
      } finally {
        release.countDown();
        b.stop().get(10, TimeUnit.SECONDS);
        server.stop(0);
      }
    }
  }

  // S4: a slow disk. A save that takes its time holds up neither heartbeats nor stop, and the
  // next client on the same state file starts only once it is over (here: given up on at stop,
  // so it doesn't land).
  @Test
  void aSlowSaveHoldsUpNothing(@TempDir Path dir) throws Exception {
    Path state = dir.resolve("state");
    CountDownLatch saving = new CountDownLatch(1);
    CountDownLatch release = new CountDownLatch(1);
    StateFile.beforeSave = p -> {
      if (p.equals(state) && saving.getCount() > 0) {
        saving.countDown();
        Raw.await(release);
      }
    };
    List<Opamp.AgentToServer> got = new CopyOnWriteArrayList<>();
    HttpServer server = opamp(got, m -> Opamp.ServerToAgent.newBuilder());
    try {
      OpampClient.Config config = OpampClient.Config.builder(endpoint(server)).mode(OpampClient.Mode.JAVA)
          .heartbeatInterval(Duration.ofMillis(50)).stateFile(state).stopTimeout(Duration.ofMillis(500)).build();
      OpampClient a = OpampClient.start(config, OpampClient.Status.of(null), n -> {});
      assertTrue(saving.await(10, TimeUnit.SECONDS), "a save");
      int before = got.size();
      waitFor(() -> got.size() >= before + 3, 3000);
      assertTrue(got.size() >= before + 3, "heartbeats go on while the save writes: " + (got.size() - before));
      long t0 = System.nanoTime();
      a.stop().get(10, TimeUnit.SECONDS);
      assertTrue(TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - t0) < 3000, "stop isn't held up by the save");

      CompletableFuture<OpampClient> b = CompletableFuture.supplyAsync(
          () -> OpampClient.start(config, OpampClient.Status.of(null), n -> {}));
      Thread.sleep(300);
      assertFalse(b.isDone(), "the next client started while a save was still writing");
      release.countDown();
      b.get(10, TimeUnit.SECONDS).stop().get(10, TimeUnit.SECONDS);
      assertFalse(Files.exists(state.resolveSibling("state.tmp")), "nothing half-written left");
    } finally {
      release.countDown();
      StateFile.beforeSave = p -> {};
      server.stop(0);
    }
  }

  // S5: a remote config saved by an earlier run is handed over on the first step. The callback
  // answers through the client start returned: start has returned by then, though the caller may
  // not have stored it yet (start's javadoc), so the callback takes it from a future completed with
  // it, not from a reference that can still be empty.
  @Test
  void notificationsComeAfterStartReturns(@TempDir Path dir) throws Exception {
    Opamp.AgentRemoteConfig remote = Opamp.AgentRemoteConfig.newBuilder()
        .setConfig(Opamp.AgentConfigMap.newBuilder().putConfigMap("", Opamp.AgentConfigObject.newBuilder()
            .setBody(ByteString.copyFromUtf8("{}")).setContentType("application/json").build()))
        .setConfigHash(ByteString.copyFromUtf8("h1"))
        .build();
    HttpServer server = opamp(new CopyOnWriteArrayList<>(), m -> Opamp.ServerToAgent.newBuilder().setRemoteConfig(remote));
    Path state = dir.resolve("state");
    OpampClient.Config config = OpampClient.Config.builder(endpoint(server)).mode(OpampClient.Mode.JAVA)
        .capabilities(Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE).stateFile(state).build();
    try {
      CountDownLatch told = new CountDownLatch(1);
      OpampClient first = OpampClient.start(config, OpampClient.Status.of(null), n -> {
        if (n instanceof Notification.RemoteConfig) {
          told.countDown();
        }
      });
      assertTrue(told.await(20, TimeUnit.SECONDS));
      Thread.sleep(200);
      first.stop().get(10, TimeUnit.SECONDS);
      // A full status: start posts several inputs, and the first step hands the config over.
      OpampClient.Status status = new OpampClient.Status(
          Opamp.AgentDescription.getDefaultInstance(), Opamp.ComponentHealth.newBuilder().setHealthy(true).build(),
          Opamp.EffectiveConfig.getDefaultInstance(), Opamp.RemoteConfigStatus.getDefaultInstance(),
          Opamp.AvailableComponents.getDefaultInstance(), Opamp.CustomCapabilities.getDefaultInstance());
      for (int i = 0; i < 10; i++) {
        CompletableFuture<OpampClient> handle = new CompletableFuture<>();
        List<Throwable> errors = new CopyOnWriteArrayList<>();
        CountDownLatch configured = new CountDownLatch(1);
        handle.complete(OpampClient.start(config, status, n -> {
          if (n instanceof Notification.RemoteConfig rc) {
            handle.join().setRemoteConfigStatus(Opamp.RemoteConfigStatus.newBuilder()
                .setLastRemoteConfigHash(rc.config().getConfigHash())
                .setStatus(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED).build());
            configured.countDown();
          } else if (n instanceof Notification.Stopped st && st.error() != null) {
            errors.add(st.error());
          }
        }));
        assertTrue(configured.await(20, TimeUnit.SECONDS), "the saved config is handed over: " + errors);
        handle.join().stop().get(10, TimeUnit.SECONDS);
        assertTrue(errors.isEmpty(), "the callback found no client: " + errors);
      }
    } finally {
      server.stop(0);
    }
  }

  // S6: an Error from the callback (not only a RuntimeException) stops the client, which says so.
  @Test
  void anErrorFromTheCallbackStopsTheClient() throws Exception {
    AssertionError bug = new AssertionError("the application's bug");
    CompletableFuture<Notification.Stopped> stopped = new CompletableFuture<>();
    OpampClient client = OpampClient.start(OpampClient.Config.builder("http://127.0.0.1:1", new byte[16]).mode(OpampClient.Mode.JAVA).build(),
        OpampClient.Status.of(null), n -> {
          if (n instanceof Notification.Stopped s) {
            stopped.complete(s);
            return;
          }
          throw bug;
        });
    assertSame(bug, stopped.get(10, TimeUnit.SECONDS).error());
    ExecutionException e = assertThrows(ExecutionException.class, () -> client.stop().get(10, TimeUnit.SECONDS));
    assertSame(bug, e.getCause());
  }
}
