// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.function.Predicate;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * What the application asks from inside the callback reaches the core before anything the client
 * takes in next (RULE-SHELL-10, spec/shell-contract.md), checked over a {@link FakeHttp} network
 * with the real core, and for the other tests against the core's tap ({@link #check}).
 */
class CallbackAnswerTest {
  static final int APPLIED = Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED_VALUE;
  static final int APPLYING = Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLYING_VALUE;

  /**
   * A call the callback made: which notification it was being told (the {@code told}-th the client
   * told, from 1), what it was, and how to know it among the core's inputs.
   */
  record Asked(int told, String what, Predicate<CoreProto.Input> is) {}

  /** The callback's answer to the remote config {@code hash}. */
  static Asked answered(int told, ByteString hash) {
    return new Asked(told, "setRemoteConfigStatus(" + hash.toStringUtf8() + ")",
        in -> in.hasSetRemoteConfigStatus() && in.getSetRemoteConfigStatus().getLastRemoteConfigHash().equals(hash));
  }

  /** The callback's setHealth with {@code status} (each test's are numbered: one of a kind). */
  static Asked health(int told, String status) {
    return new Asked(told, "setHealth(" + status + ")", in -> in.hasSetHealth() && in.getSetHealth().getStatus().equals(status));
  }

  /**
   * The core's tap at {@code tap} against RULE-SHELL-10: each of {@code calls}, made from inside
   * the callback, reached the core before anything the client took in after the step that called
   * the callback. Between that step and the call the core is fed only the application's calls
   * (those queued ahead of it), and in core mode the shell's own answer to a proxy query of that
   * step, if {@code known} lets it. A call that never reached the core (the client stopped first)
   * still lets nothing else in. What is wrong, or null.
   */
  static String check(Path tap, List<Asked> calls, boolean known) throws Exception {
    List<CoreProto.Input> inputs = new ArrayList<>();
    // For each notification the core emitted, in order: the step that emitted it.
    List<Integer> notified = new ArrayList<>();
    for (CoreTest.Rec r : CoreTest.records(tap)) {
      if (r.kind() == 3) {
        inputs.add(CoreProto.Step.parseFrom(r.bytes()).getInput());
      } else if (r.kind() == 4) {
        for (CoreProto.Effect e : CoreProto.Reply.parseFrom(r.bytes()).getOutput().getEffectsList()) {
          if (e.hasNotify()) {
            notified.add(inputs.size() - 1);
          }
        }
      }
    }
    for (Asked call : calls) {
      if (call.told() > notified.size()) {
        return "RULE-SHELL-10: " + call.what() + " was made in callback " + call.told() + ", and the core notified "
            + notified.size() + " times";
      }
      int at = notified.get(call.told() - 1);
      for (int i = at + 1; i < inputs.size() && !call.is().test(inputs.get(i)); i++) {
        boolean ahead = switch (inputs.get(i).getKindCase()) {
          case SET_DESCRIPTION, SET_HEALTH, SET_EFFECTIVE_CONFIG, SET_CUSTOM_CAPABILITIES, SEND_CUSTOM_MESSAGE,
              SET_AVAILABLE_COMPONENTS, SET_REMOTE_CONFIG_STATUS, SET_CONNECTION_SETTINGS_STATUS, PACKAGE_INSTALLED,
              PACKAGE_REMOVED, STOP -> true;
          case PROXY_ANSWER -> known;
          default -> false;
        };
        if (!ahead) {
          StringBuilder fed = new StringBuilder();
          for (int j = at; j <= i; j++) {
            fed.append("\n  step ").append(j + 1).append(": ").append(inputs.get(j).getKindCase());
          }
          return "RULE-SHELL-10: the callback's call " + call.what() + ", made in callback " + call.told() + " (of step "
              + (at + 1) + "), reached the core after step " + (i + 1) + ":" + fed;
        }
      }
    }
    return null;
  }

  /** A sentinel: the network's wire thread ends. */
  private static final Object LAST = new Object();

  /**
   * One life of a WebSocket client on a state file, over a {@link FakeHttp} network: its callback
   * answers every remote config APPLIED before it returns. In a restart's life the connection is
   * opened while the callback runs, before it answers: when the callback asks, the connection's
   * opening is already waiting for the client's thread.
   */
  static final class Life {
    final FakeHttp http = new FakeHttp();
    final boolean restart;
    /** The messages the server got, in order. */
    final List<Opamp.AgentToServer> got = new CopyOnWriteArrayList<>();
    final List<Asked> asked = new CopyOnWriteArrayList<>();
    final CompletableFuture<OpampClient> handle = new CompletableFuture<>();
    final CountDownLatch inCallback = new CountDownLatch(1);
    final CountDownLatch opened = new CountDownLatch(1);
    final CountDownLatch answeredOne = new CountDownLatch(1);
    final AtomicInteger told = new AtomicInteger();
    /** What the server offers, once, in its first answer (null: nothing). */
    volatile Opamp.AgentRemoteConfig offer;
    Path tap;

    Life(boolean restart, Opamp.AgentRemoteConfig offer) {
      this.restart = restart;
      this.offer = offer;
    }

    void notify(Notification n) {
      int k = told.incrementAndGet();
      if (!(n instanceof Notification.RemoteConfig rc)) {
        return;
      }
      try {
        if (restart) {
          // The network opens the connection now, and says when it has: its opening is waiting
          // for this thread before the answer is given.
          inCallback.countDown();
          assertTrue(opened.await(20, TimeUnit.SECONDS), "the connection opened while the callback ran");
        }
        handle.get(20, TimeUnit.SECONDS).setRemoteConfigStatus(Opamp.RemoteConfigStatus.newBuilder()
            .setLastRemoteConfigHash(rc.config().getConfigHash())
            .setStatus(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED).build());
        asked.add(answered(k, rc.config().getConfigHash()));
        answeredOne.countDown();
      } catch (InterruptedException | java.util.concurrent.ExecutionException | java.util.concurrent.TimeoutException e) {
        throw new IllegalStateException(e);
      }
    }

    /** The network's calls: each connection opened (in a restart's life, once the callback is running). */
    void calls() {
      try {
        while (true) {
          FakeHttp.Call c = http.calls.take();
          if (c instanceof FakeHttp.Request) {
            return;
          }
          if (restart) {
            inCallback.await();
          }
          ((FakeHttp.Open) c).open();
          opened.countDown();
        }
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
      }
    }

    /** The server's side of the connection: each write completed, each message recorded and answered. */
    void wire() {
      try {
        while (true) {
          Object o = http.wire.take();
          if (o == LAST) {
            return;
          }
          FakeHttp.FakeWs w = (FakeHttp.FakeWs) o;
          FakeHttp.FakeWs.Write done = w.completeNext(true);
          if (done == null || done.close()) {
            continue;
          }
          Opamp.AgentToServer m = Opamp.AgentToServer.parseFrom(Arrays.copyOfRange(done.data(), 1, done.data().length));
          got.add(m);
          Opamp.ServerToAgent.Builder answer = Opamp.ServerToAgent.newBuilder().setInstanceUid(m.getInstanceUid());
          Opamp.AgentRemoteConfig o1 = offer;
          if (o1 != null) {
            offer = null;
            answer.setRemoteConfig(o1);
          }
          byte[] body = answer.build().toByteArray();
          byte[] framed = new byte[body.length + 1];
          System.arraycopy(body, 0, framed, 1, body.length);
          w.receive(framed);
        }
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
      } catch (java.io.IOException e) {
        throw new IllegalStateException(e);
      }
    }

    /** The statuses the server heard about the remote config {@code hash}, in order. */
    List<Integer> statuses(ByteString hash) {
      return got.stream().filter(m -> m.hasRemoteConfigStatus() && m.getRemoteConfigStatus().getLastRemoteConfigHash().equals(hash))
          .map(m -> m.getRemoteConfigStatus().getStatusValue()).toList();
    }

    /** Whether every save the core asked for is on the disk (and there was one), by its tap so far. */
    boolean saved() {
      try {
        int saves = 0;
        int stored = 0;
        for (CoreTest.Rec r : CoreTest.records(tap)) {
          if (r.kind() == 3 && CoreProto.Step.parseFrom(r.bytes()).getInput().hasStored()) {
            stored++;
          } else if (r.kind() == 4) {
            saves += (int) CoreProto.Reply.parseFrom(r.bytes()).getOutput().getEffectsList().stream()
                .filter(CoreProto.Effect::hasStore).count();
          }
        }
        return saves > 0 && stored == saves;
      } catch (Exception e) {
        return false; // a record half written: read again
      }
    }

    void run(Path state, Path tapBase, ByteString hash) throws Exception {
      Thread calls = new Thread(this::calls, "network-calls");
      Thread wire = new Thread(this::wire, "network-wire");
      calls.setDaemon(true);
      wire.setDaemon(true);
      calls.start();
      wire.start();
      try {
        OpampClient c = OpampClient.start(OpampClient.Config.builder("ws://fake/v1/opamp", new byte[16])
            .httpClient(http).stateFile(state)
            .capabilities(Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE)
            .heartbeatInterval(Duration.ofSeconds(30)).build(),
            OpampClient.Status.of(Opamp.AgentDescription.getDefaultInstance()), this::notify, null, tapBase.toString(), d -> d);
        tap = Path.of(c.tapPath());
        handle.complete(c);
        assertTrue(answeredOne.await(20, TimeUnit.SECONDS), "the callback was handed the remote config");
        LifecycleTest.waitFor(() -> statuses(hash).contains(APPLIED), 20_000);
        if (!restart) {
          // What the next life starts on is saved before this one stops (a stop gives up on a
          // save still writing at its bound).
          LifecycleTest.waitFor(this::saved, 20_000);
          assertTrue(saved(), "the config was saved");
        }
        c.stop().get(20, TimeUnit.SECONDS);
      } finally {
        http.calls.add(new FakeHttp.Request(-1, null, null));
        http.wire.add(LAST);
        calls.join(10_000);
        wire.join(10_000);
      }
    }
  }

  /**
   * Found by the differential, in the Rust client (remote-config-restart-ws, on CI): after a
   * restart the core hands the saved remote config to the application again, in the step that
   * starts connecting, and reports it APPLYING until the application answers. The connection is
   * asked for before the callback is called, and here it opens while the callback runs: its
   * opening was in the client's queue before the callback's answer, was stepped first, and the
   * first message said APPLYING where it should say APPLIED.
   */
  // covers: RULE-SHELL-10
  @Test
  void aConfigAnsweredInTheCallbackAtARestartIsNeverReportedApplying(@TempDir Path dir) throws Exception {
    ByteString hash = ByteString.copyFromUtf8("c1");
    Path state = dir.resolve("state");
    // The first life: the server offers the config, the application applies it, and it is saved.
    // (Offered by the server, a config is APPLYING in the message that goes before the
    // application is told.)
    Life first = new Life(false, CallbackPropertiesTest.remoteConfig("c1"));
    first.run(state, dir.resolve("first.tap"), hash);
    assertEquals(List.of(APPLYING, APPLIED), first.statuses(hash), "the first life");
    assertNull(check(first.tap, first.asked, false));

    // The second, on what the first saved: its connection opens while the callback runs.
    Life second = new Life(true, null);
    second.run(state, dir.resolve("second.tap"), hash);
    assertEquals(1, second.asked.size(), "the callback answered the saved config");
    Opamp.AgentToServer firstMessage = second.got.get(0);
    assertTrue(firstMessage.hasRemoteConfigStatus(), "the first message after the restart reports on the saved config");
    assertEquals(hash, firstMessage.getRemoteConfigStatus().getLastRemoteConfigHash());
    assertEquals(APPLIED, firstMessage.getRemoteConfigStatus().getStatusValue(), "the first message after the restart");
    assertTrue(!second.statuses(hash).contains(APPLYING), "APPLYING reported after the restart: " + second.statuses(hash));
    assertNull(check(second.tap, second.asked, false));
  }
}
