// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertInstanceOf;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assertions.fail;

import java.io.BufferedInputStream;
import java.io.File;
import java.io.InputStream;
import java.lang.management.ManagementFactory;
import java.net.Socket;
import java.nio.file.Path;
import java.time.Duration;
import java.util.List;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.Semaphore;
import java.util.concurrent.TimeUnit;
import java.util.stream.Collectors;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.EnumSource;

/**
 * Lifecycle churn: an agent that reconfigures its client stops it and starts another, again and
 * again, in one JVM. Whatever a start leaves behind when its client stops (a core instance, a
 * thread, an HttpClient, a socket) adds up there. This starts and stops clients many times, at
 * every point a stop can come (at once, mid-handshake, with the connection up, with the callback
 * running, from the callback, with a state file, after the callback threw), over HTTP and
 * WebSocket, in both modes, against servers on loopback, and checks that the JVM's live threads,
 * open file descriptors and heap come back to what they were after a warm-up.
 */
class ChurnTest {
  /** Where a run's client is stopped. */
  enum Stop {
    /** Right after start. */
    AT_ONCE,
    /** The WebSocket handshake (or the request) under way: the server never answers it. */
    HELD,
    /** A config has come: the connection is up. */
    CONNECTED,
    /** The callback is running (it waits) when stop is called. */
    IN_CALLBACK,
    /** Stop from the callback that took the config. */
    FROM_CALLBACK,
    /** With a state file, once the config is saved. */
    STATE_FILE,
    /** The callback threw: the client stopped itself. */
    THREW
  }

  static final Opamp.AgentRemoteConfig CONFIG = Opamp.AgentRemoteConfig.newBuilder()
      .setConfig(Opamp.AgentConfigMap.newBuilder().putConfigMap("", Opamp.AgentConfigObject.newBuilder()
          .setBody(com.google.protobuf.ByteString.copyFromUtf8("churn")).build()))
      .setConfigHash(com.google.protobuf.ByteString.copyFromUtf8("churn"))
      .build();

  static byte[] reply(Opamp.AgentToServer m) {
    return Opamp.ServerToAgent.newBuilder().setInstanceUid(m.getInstanceUid()).setRemoteConfig(CONFIG).build().toByteArray();
  }

  /** The servers: OpAMP over HTTP, over WebSocket, and one that holds whatever it gets. */
  static final class Servers implements AutoCloseable {
    final PropertiesTest.HttpOpamp http = new PropertiesTest.HttpOpamp(ChurnTest::reply);
    final PropertiesTest.WsServer ws = new PropertiesTest.WsServer(ChurnTest::reply);
    /** One permit for each connection the holding server took. */
    final Semaphore held = new Semaphore(0);
    final Raw hold = new Raw(this::hold);

    Servers() throws Exception {}

    /** Read the request and hold the connection, answering nothing, until the client closes it. */
    private void hold(Socket s) throws Exception {
      try (s) {
        InputStream in = new BufferedInputStream(s.getInputStream());
        TransportTest.Head.read(in);
        held.release();
        while (in.read() >= 0) {
          // what comes after the head (a body), until the client goes
        }
      }
    }

    String endpoint(boolean websocket, Stop where) {
      if (where == Stop.HELD) {
        return hold.url(websocket ? "ws" : "http") + "/v1/opamp";
      }
      return websocket ? "ws://127.0.0.1:" + ws.port() + "/v1/opamp" : "http://127.0.0.1:" + http.port() + "/v1/opamp";
    }

    @Override
    public void close() throws Exception {
      http.close();
      ws.close();
      hold.close();
    }
  }

  /** Start one client, stop it at {@code where}, and check it stopped as it should. */
  static void run(Servers srv, Path dir, OpampClient.Mode mode, boolean websocket, Stop where) throws Exception {
    OpampClient.Config.Builder b = OpampClient.Config.builder(srv.endpoint(websocket, where)).mode(mode).directConnection()
        .heartbeatInterval(Duration.ofHours(1))
        .capabilities(Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE)
        // Over HTTP the goodbye waits for the request held: the stop gives up on both then.
        .stopTimeout(Duration.ofMillis(where == Stop.HELD ? 50 : 2000));
    if (where == Stop.STATE_FILE) {
      // The same file each time, as an agent that restarts its client keeps it.
      b.stateFile(dir.resolve("state"));
    }
    RuntimeException threw = new RuntimeException("churn");
    CompletableFuture<OpampClient> handle = new CompletableFuture<>();
    CountDownLatch configured = new CountDownLatch(1);
    CountDownLatch inside = new CountDownLatch(1); // the callback is running (IN_CALLBACK)
    CountDownLatch release = new CountDownLatch(1); // and may return
    CompletableFuture<Notification.Stopped> stopped = new CompletableFuture<>();
    CompletableFuture<CompletableFuture<Void>> fromCallback = new CompletableFuture<>();
    handle.complete(OpampClient.start(b.build(), OpampClient.Status.of(null), n -> {
      if (n instanceof Notification.Stopped s) {
        stopped.complete(s);
        return;
      }
      if (!(n instanceof Notification.RemoteConfig rc)) {
        return;
      }
      OpampClient c = handle.join();
      c.setRemoteConfigStatus(Opamp.RemoteConfigStatus.newBuilder().setLastRemoteConfigHash(rc.config().getConfigHash())
          .setStatus(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED).build());
      switch (where) {
        case IN_CALLBACK -> {
          if (inside.getCount() > 0) {
            inside.countDown();
            Raw.await(release);
          }
        }
        case FROM_CALLBACK -> fromCallback.complete(c.stop());
        case THREW -> throw threw;
        default -> {}
      }
      configured.countDown();
    }));
    OpampClient client = handle.join();
    switch (where) {
      case HELD -> assertTrue(srv.held.tryAcquire(10, TimeUnit.SECONDS), where + ": the held request never came");
      case CONNECTED, STATE_FILE -> assertTrue(configured.await(10, TimeUnit.SECONDS), where + ": no remote config");
      case IN_CALLBACK -> {
        assertTrue(inside.await(10, TimeUnit.SECONDS), where + ": the callback never ran");
        CompletableFuture.runAsync(() -> {
          try {
            Thread.sleep(1);
          } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
          }
          release.countDown();
        });
      }
      case FROM_CALLBACK -> fromCallback.get(10, TimeUnit.SECONDS).get(10, TimeUnit.SECONDS);
      case THREW -> stopped.get(10, TimeUnit.SECONDS);
      default -> {}
    }
    if (where == Stop.THREW) {
      ExecutionException e = assertThrows(ExecutionException.class, () -> client.stop().get(10, TimeUnit.SECONDS));
      assertSame(threw, e.getCause(), where + ": stop says why");
    } else {
      client.stop().get(10, TimeUnit.SECONDS);
    }
    assertTrue(stopped.isDone(), where + ": stop returned before Stopped was told");
    if (where == Stop.HELD) {
      srv.held.drainPermits(); // a request the core tried again before it stopped
    }
  }

  /** What the JVM holds that a client could leave behind. */
  record Usage(int threads, int fds, long heap) {
    @Override
    public String toString() {
      return threads + " threads, " + fds + " fds, " + (heap >> 10) + " KiB heap";
    }
  }

  static int openFds() {
    String[] fds = new File("/proc/self/fd").list();
    return fds == null ? -1 : fds.length;
  }

  static Usage measure() {
    for (int i = 0; i < 3; i++) {
      System.gc();
    }
    return new Usage(ManagementFactory.getThreadMXBean().getThreadCount(), openFds(), ManagementFactory.getMemoryMXBean().getHeapMemoryUsage().getUsed());
  }

  /**
   * Measure until threads and fds are within {@code slack} of {@code base} (what a client let go of
   * ends by itself: a thread finishing its last task, a socket the server closes), or 5 s have gone.
   */
  static Usage settle(Usage base, int slack) throws InterruptedException {
    for (long end = System.nanoTime() + TimeUnit.SECONDS.toNanos(5); ; Thread.sleep(20)) {
      Usage u = measure();
      if ((u.threads() <= base.threads() + slack && u.fds() <= base.fds() + slack) || System.nanoTime() > end) {
        return u;
      }
    }
  }

  static String threadNames() {
    return Thread.getAllStackTraces().keySet().stream().map(Thread::getName).sorted().collect(Collectors.joining(", "));
  }

  /**
   * Starting and stopping clients again and again leaves nothing behind: after a warm-up, rounds of
   * a client stopped at every point, over HTTP and WebSocket, leave the JVM with no more live
   * threads, open file descriptors or heap than before them (a client is a thread or more, an
   * HttpClient in platform mode, and a core instance of hundreds of KiB of heap).
   */
  @ParameterizedTest
  @EnumSource(OpampClient.Mode.class)
  void churnLeavesNothingBehind(OpampClient.Mode mode, @TempDir Path dir) throws Exception {
    int warmup = 2;
    int rounds = 10;
    try (Servers srv = new Servers()) {
      Runnable round = () -> {
        for (Stop where : Stop.values()) {
          for (boolean websocket : List.of(false, true)) {
            try {
              run(srv, dir, mode, websocket, where);
            } catch (Exception e) {
              throw new AssertionError(mode + " " + where + (websocket ? " over WebSocket" : " over HTTP"), e);
            }
          }
        }
      };
      for (int i = 0; i < warmup; i++) {
        round.run();
      }
      Usage base = settle(measure(), 0);
      long began = System.nanoTime();
      for (int i = 0; i < rounds; i++) {
        round.run();
      }
      long took = TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - began);
      Usage after = settle(base, 2);
      int runs = rounds * Stop.values().length * 2;
      System.out.println(mode + " mode: " + runs + " clients in " + took + " ms: before " + base + "; after " + after);
      assertTrue(after.threads() <= base.threads() + 2,
          "live threads grew from " + base.threads() + " to " + after.threads() + " over " + runs + " clients: " + threadNames());
      if (base.fds() >= 0) {
        assertTrue(after.fds() <= base.fds() + 2, "open fds grew from " + base.fds() + " to " + after.fds() + " over " + runs + " clients");
      }
      assertTrue(after.heap() <= base.heap() + (32L << 20),
          "heap grew from " + (base.heap() >> 10) + " KiB to " + (after.heap() >> 10) + " KiB over " + runs + " clients");
    }
  }
}
