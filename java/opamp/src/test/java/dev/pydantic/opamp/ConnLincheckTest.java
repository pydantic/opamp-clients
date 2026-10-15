// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentLinkedQueue;
import java.util.concurrent.atomic.AtomicInteger;
import org.jetbrains.lincheck.datastructures.ModelCheckingOptions;
import org.jetbrains.lincheck.datastructures.Operation;
import org.jetbrains.lincheck.datastructures.Validate;
import org.junit.jupiter.api.Test;

/**
 * A WebSocket connection's sends and Close ({@code Executor.Conn}: each takes its place in one
 * chain, after the last, as the JDK allows one outstanding send), under Lincheck's model checker.
 * Lincheck draws scenarios of sends (from up to three threads) and completions of the oldest write
 * by the JDK's side (which starts the next send in the chain, from that thread), and explores
 * their interleavings; then the Close, with every write completed. On a {@link FakeHttp} socket,
 * which records what the JDK would refuse. Whatever the interleaving: never two sends outstanding,
 * every message sent exactly once, each sender's in its order, the Close after them all, and no
 * send failing.
 *
 * <p>Lincheck's data-structure API ({@link ModelCheckingOptions}): its general one ({@code
 * Lincheck.runConcurrentTest}, 3.7) timed out ("has hung") on this code in every trial, with the
 * chain's lock or without. OPAMP_LINCHECK_ITERATIONS sets the scenarios (default 5, each with 100
 * interleavings). A failure prints the interleaving.
 */
class ConnLincheckTest {
  // covers: RULE-SHELL-04
  @Test
  void sendsAndCloseStayInOneChain() {
    String env = System.getenv("OPAMP_LINCHECK_ITERATIONS");
    int iterations = env == null || env.isEmpty() ? 5 : Integer.parseInt(env);
    ModelCheckingOptions options = new ModelCheckingOptions()
        .iterations(iterations)
        .invocationsPerIteration(100)
        .threads(3)
        .actorsPerThread(2)
        .actorsBefore(0)
        .actorsAfter(0);
    // Lincheck instruments a class when an interleaving first touches it, inside that interleaving
    // and against its timeout (30 s): on a loaded machine the first ones (the JDK's collections,
    // CompletableFuture's stages) ran past it and read as hangs. A deadlock or livelock is the
    // model checker's own finding, not this timeout's. (Internal to Lincheck: its Kotlin name.)
    ((ModelCheckingOptions) options.invocationTimeout$lincheck(600_000)).check(Chain.class);
  }

  /** One connection, open; the operations Lincheck draws on it, and the check after them. */
  public static class Chain {
    private final FakeHttp http = new FakeHttp();
    private final ConcurrentLinkedQueue<CoreProto.Input> got = new ConcurrentLinkedQueue<>();
    private final Executor x = new Executor(Transport.of(http), got::add, null);
    private final FakeHttp.FakeWs ws;
    private final AtomicInteger sends = new AtomicInteger();

    public Chain() {
      x.run(CoreProto.Effect.newBuilder()
          .setWsOpen(CoreProto.WsOpen.newBuilder().setId(1).setUrl("ws://fake/v1/opamp")).build());
      ws = ((FakeHttp.Open) http.calls.poll()).open();
    }

    /** The core's ws_send, from this thread: its message names the thread and the send's number. */
    @Operation
    public void send() {
      String tag = Thread.currentThread().getName() + "#" + sends.incrementAndGet();
      x.run(CoreProto.Effect.newBuilder()
          .setWsSend(CoreProto.WsMessage.newBuilder().setId(1).setData(ByteString.copyFromUtf8(tag))).build());
    }

    /** The JDK's side: the oldest write completes, and the next in the chain goes from here. */
    @Operation
    public void complete() {
      ws.completeWrite(false);
    }

    @Validate
    public void validate() {
      x.run(CoreProto.Effect.newBuilder().setWsClose(CoreProto.Id.newBuilder().setId(1)).build());
      for (int i = 0; i < 16 && ws.completeWrite(true); i++) {
        // each completion lets the next write go; our Close last, answered by the server's
      }
      // Plain loops, nothing more than the scenario needs: what a check touches first is
      // instrumented inside an interleaving, against Lincheck's timeout (streams and a protobuf's
      // text made it time out on a loaded machine).
      List<String> sent = new ArrayList<>();
      for (byte[] m : ws.sent) {
        sent.add(new String(m, StandardCharsets.UTF_8));
      }
      check(http.violations.isEmpty(), () -> "what the JDK would refuse: " + http.violations + "; sent " + sent);
      check(sent.size() == sends.get() && new HashSet<>(sent).size() == sent.size(),
          () -> "each of the " + sends.get() + " messages once: " + sent);
      Map<String, Integer> lastBy = new HashMap<>();
      for (String m : sent) {
        int hash = m.lastIndexOf('#');
        String who = m.substring(0, hash);
        int n = Integer.parseInt(m.substring(hash + 1));
        Integer before = lastBy.put(who, n);
        check(before == null || before < n, () -> "one sender's in its order: " + sent);
      }
      check(ws.closeSent() && ws.writes.isEmpty(), () -> "the Close went, after every send: " + sent);
      for (CoreProto.Input in : got) {
        check(!in.hasWsLost(), () -> "no send failed: " + in.getWsLost().getDetail());
      }
    }
  }

  /** The message is made only on a failure: a protobuf's text, made in every interleaving, is slow under Lincheck. */
  private static void check(boolean ok, java.util.function.Supplier<String> what) {
    if (!ok) {
      throw new AssertionError(what.get());
    }
  }
}
