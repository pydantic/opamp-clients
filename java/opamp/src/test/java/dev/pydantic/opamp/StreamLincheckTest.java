// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicInteger;
import org.jetbrains.lincheck.datastructures.ModelCheckingOptions;
import org.jetbrains.lincheck.datastructures.Operation;
import org.jetbrains.lincheck.datastructures.Validate;
import org.junit.jupiter.api.Test;

/**
 * Core mode's counterpart of {@link ConnLincheckTest}: a stream's writes ({@code Streams.Stream}:
 * the core's bytes queued under the stream's lock, past {@link Streams#WRITE_HIGH} holding its
 * reader; its writer taking the oldest and, once it has gone, the next), under Lincheck's model
 * checker. Lincheck draws scenarios of writes (from up to three threads: stronger than the one
 * loop that writes in the client) and of the writer's steps (one writer thread a stream, as in
 * the client: never two at once), and explores their interleavings;
 * then the close (the FIN goes once everything queued has gone), with the writer draining the rest.
 * Whatever the interleaving: every write goes to the socket exactly once, each writer's in its
 * order, nothing after the FIN, and the reader is held exactly while more than WRITE_HIGH waits
 * (and released once it is down to WRITE_LOW).
 *
 * <p>OPAMP_LINCHECK_ITERATIONS sets the scenarios (default 5, each with 100 interleavings).
 */
class StreamLincheckTest {
  /** Each write's size: two of them are past WRITE_HIGH. */
  static final int SIZE = Streams.WRITE_HIGH / 2 + 1;

  // covers: RULE-SHELL-04
  @Test
  void writesGoOnceInOrderAndHoldTheReaderWhileTheyWait() {
    String env = System.getenv("OPAMP_LINCHECK_ITERATIONS");
    int iterations = env == null || env.isEmpty() ? 5 : Integer.parseInt(env);
    ModelCheckingOptions options = new ModelCheckingOptions()
        .iterations(iterations)
        .invocationsPerIteration(100)
        .threads(3)
        .actorsPerThread(2)
        .actorsBefore(0)
        .actorsAfter(0);
    // As in ConnLincheckTest: the first interleavings instrument the JDK's classes inside
    // Lincheck's timeout; a hang is the model checker's own finding, not this timeout's.
    ((ModelCheckingOptions) options.invocationTimeout$lincheck(600_000)).check(Queue.class);
  }

  /** One stream, open, without TLS; the operations Lincheck draws on it, and the check after them. */
  public static class Queue {
    private final Streams.Stream s = new Streams.Stream(1);
    private final AtomicInteger writes = new AtomicInteger();
    /** What reached the socket, in order (the writer's steps record it under the stream's lock). */
    private final List<byte[]> socket = new ArrayList<>();

    /** The core's stream_write: a tagged write of SIZE bytes. */
    @Operation
    public void write() throws Exception {
      String tag = Thread.currentThread().getName() + "#" + writes.incrementAndGet() + ";";
      byte[] data = Arrays.copyOf(tag.getBytes(StandardCharsets.US_ASCII), SIZE);
      synchronized (s) {
        s.write(data);
      }
    }

    /**
     * The writer's step: the oldest goes to the socket, then it is taken off the queue. One
     * writer a stream: never two of these at once.
     */
    @Operation(nonParallelGroup = "writer")
    public void complete() {
      byte[] chunk;
      synchronized (s) {
        chunk = s.nextOut();
      }
      if (chunk == null) {
        return;
      }
      synchronized (s) {
        socket.add(chunk);
        s.wentOut(chunk);
      }
    }

    @Validate
    public void validate() {
      // Closed: the writer drains what is left, then the FIN.
      synchronized (s) {
        s.finishing = true;
        check(s.blocked == (s.holds > 0), () -> "held exactly while blocked: holds " + s.holds + ", blocked " + s.blocked);
        check(!s.blocked || s.queued > Streams.WRITE_LOW, () -> "blocked with " + s.queued + " waiting");
      }
      for (int i = 0; i < 16; i++) {
        complete();
      }
      List<String> sent = new ArrayList<>();
      for (byte[] m : socket) {
        String t = new String(m, StandardCharsets.US_ASCII);
        sent.add(t.substring(0, t.indexOf(';')));
      }
      synchronized (s) {
        check(s.out.isEmpty() && s.queued == 0, () -> "everything went before the FIN: " + s.queued + " left");
        check(s.holds == 0 && !s.blocked, () -> "the reader released once drained: holds " + s.holds);
      }
      check(sent.size() == writes.get() && new HashSet<>(sent).size() == sent.size(),
          () -> "each of the " + writes.get() + " writes once: " + sent);
      Map<String, Integer> lastBy = new HashMap<>();
      for (String m : sent) {
        int hash = m.lastIndexOf('#');
        String who = m.substring(0, hash);
        int n = Integer.parseInt(m.substring(hash + 1));
        Integer before = lastBy.put(who, n);
        check(before == null || before < n, () -> "one writer's in its order: " + sent);
      }
    }
  }

  private static void check(boolean ok, java.util.function.Supplier<String> what) {
    if (!ok) {
      throw new AssertionError(what.get());
    }
  }
}
