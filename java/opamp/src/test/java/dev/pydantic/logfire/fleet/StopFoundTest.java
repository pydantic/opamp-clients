// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.logfire.fleet;

import static org.junit.jupiter.api.Assertions.assertTrue;

import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * A break the other SDKs' schedule tests found, checked here in real time over a {@link FakeHttp}
 * network with the real core: stop held up by a save for a second stop timeout.
 */
class StopFoundTest {
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
          .mode(OpampClient.Mode.PLATFORM).httpClient(http).stateFile(dir.resolve("state"))
          .capabilities(Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE)
          .stopTimeout(Duration.ofSeconds(2)).heartbeatInterval(Duration.ofSeconds(30)).build(),
          OpampClient.Status.of(Opamp.AgentDescription.getDefaultInstance()), n -> {});
      assertTrue(saving.await(20, TimeUnit.SECONDS), "a save is writing");
      stopping.set(true); // the server holds what comes now: the goodbye
      long t0 = System.nanoTime();
      c.stop().get(20, TimeUnit.SECONDS);
      long ms = (System.nanoTime() - t0) / 1_000_000;
      assertTrue(ms < 2_000 + 1_000 + 500, "stop took " + ms + " ms: more than its one stop timeout (2 s) and the grace (1 s)");
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

  /** The server: answers each POST with {@code reply} (holds it once {@code stopping}). */
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
          if (stopping.get()) {
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
