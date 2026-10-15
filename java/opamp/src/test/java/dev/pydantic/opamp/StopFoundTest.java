// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * Two breaks the other SDKs' schedule tests found, checked here in real time over a
 * {@link FakeHttp} network with the real core: stop held up by a save for a second stop timeout,
 * and a notification told after {@code stopped} when the core emits one after it in one output.
 */
class StopFoundTest {
  static final byte[] PACKAGE = "the package".repeat(8).getBytes(java.nio.charset.StandardCharsets.US_ASCII);

  /**
   * A save still writing when stop is called, and the goodbye held by the server: the core spends
   * the stop timeout on them, and the shell then waited a whole stop timeout more for the save
   * writer. All of stop shares one stop timeout, from the first stop() (Go's stopBy, Python's
   * 7cf9ab7): it returns within it and a small grace.
   */
  // covers: RULE-SHELL-07
  @Test
  void stopTakesOneStopTimeoutWithASaveWriting(@TempDir Path dir) throws Exception {
    CountDownLatch saving = new CountDownLatch(1);
    CountDownLatch release = new CountDownLatch(1);
    AtomicBoolean stopping = new AtomicBoolean();
    StateFile.beforeSave = p -> {
      saving.countDown();
      try {
        release.await(30, TimeUnit.SECONDS); // a disk that takes its time
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
      }
    };
    FakeHttp http = new FakeHttp();
    Thread net = network(http, stopping, m -> Opamp.ServerToAgent.newBuilder().setInstanceUid(m.getInstanceUid())
        .setRemoteConfig(CallbackPropertiesTest.remoteConfig("c1")));
    try {
      OpampClient c = OpampClient.start(OpampClient.Config.builder("http://fake/v1/opamp", new byte[16])
          .httpClient(http).stateFile(dir.resolve("state"))
          .capabilities(Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE)
          .stopTimeout(Duration.ofSeconds(2)).heartbeatInterval(Duration.ofSeconds(30)).build(),
          OpampClient.Status.of(Opamp.AgentDescription.getDefaultInstance()), n -> {});
      assertTrue(saving.await(20, TimeUnit.SECONDS), "a save is writing");
      stopping.set(true); // the server holds what comes now: the goodbye
      long t0 = System.nanoTime();
      c.stop().get(20, TimeUnit.SECONDS);
      long ms = (System.nanoTime() - t0) / 1_000_000;
      assertTrue(ms < 2_000 + 900, "stop took " + ms + " ms: more than its one stop timeout (2 s) and a grace");
    } finally {
      release.countDown();
      // The save let go writes on after stop returned, and JUnit deletes the directory when this
      // returns: wait for the writer, or the delete finds a file appearing under it ("Failed to
      // delete temp directory", on a loaded CI runner).
      StateFile.settled(dir.resolve("state"));
      StateFile.beforeSave = p -> {};
      http.calls.add(new FakeHttp.Request(-1, null, null));
      net.join(10_000);
    }
  }

  /**
   * The core's output that answers a save during stop says {@code stopped} and then {@code
   * install_package} (a core bug, fixed on another branch): the shell told them in that order, so
   * the application heard a notification after {@code stopped}. It tells {@code stopped} last
   * within an output now (Python's 0ca039a), whatever the core emits after it.
   */
  // covers: RULE-SHELL-01
  @Test
  void stoppedIsTheLastNotification(@TempDir Path dir) throws Exception {
    List<String> heard = new CopyOnWriteArrayList<>();
    List<String> after = new CopyOnWriteArrayList<>();
    AtomicBoolean stopped = new AtomicBoolean();
    CountDownLatch saving = new CountDownLatch(1);
    CountDownLatch release = new CountDownLatch(1);
    AtomicBoolean holdSaves = new AtomicBoolean();
    StateFile.beforeSave = p -> {
      if (holdSaves.get()) {
        saving.countDown();
        try {
          release.await(30, TimeUnit.SECONDS);
        } catch (InterruptedException e) {
          Thread.currentThread().interrupt();
        }
      }
    };
    FakeHttp http = new FakeHttp();
    AtomicBoolean stopping = new AtomicBoolean();
    Thread net = network(http, stopping, m -> Opamp.ServerToAgent.newBuilder().setInstanceUid(m.getInstanceUid())
        .setPackagesAvailable(offer()));
    try {
      OpampClient c = OpampClient.start(OpampClient.Config.builder("http://fake/v1/opamp", new byte[16])
          .httpClient(http).stateFile(dir.resolve("state")).acceptPackages(true)
          .stopTimeout(Duration.ofSeconds(2)).heartbeatInterval(Duration.ofSeconds(30)).build(),
          OpampClient.Status.of(Opamp.AgentDescription.getDefaultInstance()), n -> {
            String name = n.getClass().getSimpleName();
            if (stopped.get()) {
              after.add(name);
            }
            heard.add(name);
            if (n instanceof Notification.Stopped) {
              stopped.set(true);
            }
          });
      // The saves after the download is asked for are held: the one that records it among them.
      http.downloadAsked.get(20, TimeUnit.SECONDS);
      holdSaves.set(true);
      assertTrue(saving.await(20, TimeUnit.SECONDS), "a save is writing");
      CompletableFuture<Void> stop = c.stop();
      Thread.sleep(300);
      release.countDown(); // the save lands: the core's answer to it says stopped
      try {
        stop.get(20, TimeUnit.SECONDS);
      } catch (java.util.concurrent.ExecutionException e) {
        // its error is not the point here
      }
      Thread.sleep(100);
      assertEquals(List.of(), after, "nothing after stopped (RULE-SHELL-01); heard " + heard);
      assertEquals("Stopped", heard.get(heard.size() - 1), "stopped is last; heard " + heard);
    } finally {
      release.countDown();
      // The save let go writes on after stop returned, and JUnit deletes the directory when this
      // returns: wait for the writer, or the delete finds a file appearing under it ("Failed to
      // delete temp directory", on a loaded CI runner).
      StateFile.settled(dir.resolve("state"));
      StateFile.beforeSave = p -> {};
      http.calls.add(new FakeHttp.Request(-1, null, null));
      net.join(10_000);
    }
  }

  static Opamp.PackagesAvailable offer() {
    try {
      return Opamp.PackagesAvailable.newBuilder()
          .putPackages("p", Opamp.PackageAvailable.newBuilder()
              .setType(Opamp.PackageType.PackageType_TopLevel).setVersion("1")
              .setHash(ByteString.copyFromUtf8("p1"))
              .setFile(Opamp.DownloadableFile.newBuilder().setDownloadUrl("http://fake/p")
                  .setContentHash(ByteString.copyFrom(MessageDigest.getInstance("SHA-256").digest(PACKAGE))))
              .build())
          .setAllPackagesHash(ByteString.copyFromUtf8("all1"))
          .build();
    } catch (java.security.NoSuchAlgorithmException e) {
      throw new IllegalStateException(e);
    }
  }

  /** The server: answers each POST with {@code reply} (holds it once {@code stopping}), a GET with the package. */
  static Thread network(FakeHttp http, AtomicBoolean stopping,
      java.util.function.Function<Opamp.AgentToServer, Opamp.ServerToAgent.Builder> reply) {
    Thread t = new Thread(() -> {
      List<FakeHttp.Request> held = new ArrayList<>();
      try {
        while (true) {
          FakeHttp.Call c = http.calls.take();
          if (!(c instanceof FakeHttp.Request r)) {
            continue;
          }
          if (r.request == null) {
            return;
          }
          if (r.request.method().equals("GET")) {
            http.downloadAsked.complete(null);
            r.answer(200, PACKAGE);
          } else if (stopping.get()) {
            held.add(r); // never answered: the core gives up on it at its stop bound
          } else {
            r.answer(200, reply.apply(Opamp.AgentToServer.parseFrom(r.body)).build().toByteArray());
          }
        }
      } catch (Exception e) {
        // the test ends
      }
    }, "network");
    t.setDaemon(true);
    t.start();
    return t;
  }
}
