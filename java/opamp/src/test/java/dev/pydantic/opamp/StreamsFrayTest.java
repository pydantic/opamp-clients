// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.SplittableRandom;
import java.util.TreeMap;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import org.junit.jupiter.api.Test;

/**
 * Rust mode's registry ({@link Streams.InFlight}) and executor under Fray: {@link RegistryFrayTest}
 * in Rust mode. A {@link Streams} on a loop of its own (as the client's thread has it), the test
 * playing the core (stream effects, closes, a download's file, then the end) and {@link MemoryNet}
 * the network on threads of its own: each connection made at once, refused, held until the shell
 * gives up and made anyway (then reset at once), its far end answering bytes and ending as the
 * plan says. Fray explores the interleavings; the plan comes from the schedule's seed.
 *
 * <p>Checked as a model of what reaches the core: an input only about a stream opened, of the
 * kind that can come then (data and an end only after it opened), at most one end each, nothing
 * about a stream after its close, nothing at all after the end (RULE-SHELL-04, -06); after the
 * end's wait (within the stop bound), every connection the client had is closed or reset and none
 * is still being made, the registry is empty, and no download file appears or changes after it
 * (S3) (RULE-SHELL-07). See {@link FrayRun} for the number of schedules, the seeds and replay.
 */
// covers: RULE-SHELL-04, RULE-SHELL-06, RULE-SHELL-07
class StreamsFrayTest {
  /** Schedules per scenario in a normal run (OPAMP_FRAY_RUNS overrides it). */
  static final int RUNS = 200;
  /** Seconds a scenario may explore in a normal run before it stops early. */
  static final int BUDGET_S = 300;
  /** How long the end may wait for what is finishing (OpampClient's stop timeout). */
  static final long STOP_MS = 5_000;

  /** Streams opened, written, closed (some while connecting) as the network answers: nothing about a closed one reaches the core. */
  @Test
  void nothingAboutAStreamAfterItsClose() throws Exception {
    FrayRun.explore("StreamsFrayTest.nothingAboutAStreamAfterItsClose", RUNS, BUDGET_S,
        seed -> new Run(seed, Scenario.CLOSE).run());
  }

  /** Streams being opened (or resolved) as the shell ends: one that connects after the end is reset. */
  @Test
  void aConnectionMadeLateIsReset() throws Exception {
    FrayRun.explore("StreamsFrayTest.aConnectionMadeLateIsReset", RUNS, BUDGET_S,
        seed -> new Run(seed, Scenario.LATE_OPEN).run());
  }

  /** A download spliced to its file as the shell ends: nothing of it is left, or changes, after. */
  @Test
  void aDownloadFinishingAfterTheEndLeavesNothing() throws Exception {
    FrayRun.explore("StreamsFrayTest.aDownloadFinishingAfterTheEndLeavesNothing", RUNS, BUDGET_S,
        seed -> new Run(seed, Scenario.DOWNLOAD).run());
  }

  enum Scenario { CLOSE, LATE_OPEN, DOWNLOAD }

  /** How the network answers an attempt to connect. */
  enum Answer { NOW, FAIL, HOLD_THEN_ANSWER }

  /** When the core ends the shell: at once, once the body is spliced, or once the file is downloaded. */
  enum Until { NOW, SPLICED, DOWNLOADED }

  /** What the far end of a connection does: answer and wait for the end, answer and FIN, or reset. */
  enum Peer { TALK, END, RESET }

  /**
   * One schedule's plan: the core's effects in order (closes among them), how each attempt is
   * answered and what each far end does, by stream id; the download (its stream, and its size).
   */
  record Plan(Scenario scenario, List<CoreProto.Effect> effects, Map<Long, Answer> answers, Map<Long, Peer> peers,
      int body, Until until) {
    static Plan of(Scenario scenario, SplittableRandom r) {
      List<CoreProto.Effect> effects = new ArrayList<>();
      Map<Long, Answer> answers = new TreeMap<>();
      Map<Long, Peer> peers = new TreeMap<>();
      int n = switch (scenario) {
        case CLOSE -> 2 + r.nextInt(3);
        case LATE_OPEN -> 1 + r.nextInt(2);
        case DOWNLOAD -> 1;
      };
      List<Long> open = new ArrayList<>();
      for (long id = 1; id <= n; id++) {
        if (scenario == Scenario.LATE_OPEN && r.nextInt(3) == 0) {
          effects.add(StreamsTest.resolve(id, "proxy.example"));
          continue;
        }
        effects.add(StreamsTest.open(id));
        open.add(id);
        answers.put(id, switch (scenario) {
          case CLOSE -> Answer.values()[r.nextInt(3)];
          case LATE_OPEN -> r.nextInt(4) == 0 ? Answer.NOW : Answer.HOLD_THEN_ANSWER;
          case DOWNLOAD -> Answer.NOW;
        });
        peers.put(id, scenario == Scenario.DOWNLOAD ? Peer.TALK : Peer.values()[r.nextInt(3)]);
        if (scenario != Scenario.DOWNLOAD && r.nextBoolean()) {
          effects.add(StreamsTest.write(id, ("req" + id).getBytes(java.nio.charset.StandardCharsets.US_ASCII)));
        }
        // The core gives up on something under way, now and then (the scenario's point).
        if (scenario == Scenario.CLOSE && r.nextInt(2) == 0) {
          long victim = open.get(r.nextInt(open.size()));
          effects.add(StreamsTest.close(victim, r.nextBoolean()));
        }
      }
      int body = 0;
      if (scenario == Scenario.DOWNLOAD) {
        body = 1 + r.nextInt(3 * WINDOW);
        effects.add(StreamsTest.splice(1, 9, body));
      }
      Until until = scenario == Scenario.DOWNLOAD ? Until.values()[r.nextInt(3)] : Until.NOW;
      return new Plan(scenario, effects, answers, peers, body, until);
    }
  }

  /** The window of the pipes: small, so that a body is many reads, and writes wait for them. */
  static final int WINDOW = 512;

  /** One schedule. */
  static final class Run {
    final long seed;
    final Plan plan;
    final MemoryNet net = new MemoryNet(WINDOW);
    final List<String> violations = new CopyOnWriteArrayList<>();
    final List<Thread> threads = new CopyOnWriteArrayList<>();
    // The model: streams opened by the core, their state, the closes; on the loop only.
    final Set<Long> issued = new HashSet<>();
    final Set<Long> resolving = new HashSet<>();
    final Set<Long> opened = new HashSet<>();
    final Set<Long> ended = new HashSet<>();
    final Set<Long> closed = new HashSet<>();
    boolean over;
    /** The body went to the file (1), and the file was answered (2). */
    final CompletableFuture<Void> spliced = new CompletableFuture<>();
    final CompletableFuture<Void> downloaded = new CompletableFuture<>();
    Streams x;
    Path dir;
    Path state;

    Run(long seed, Scenario scenario) {
      this.seed = seed;
      this.plan = Plan.of(scenario, new SplittableRandom(seed));
    }

    void violation(String what) {
      violations.add(what);
    }

    void run() {
      ScheduledExecutorService loop = Executors.newSingleThreadScheduledExecutor();
      Thread network = new Thread(this::network, "network");
      try {
        dir = Files.createTempDirectory("fray-streams");
        state = dir.resolve("state");
        net.names.put("proxy.example", List.of("192.0.2.1"));
        x = new Streams(loop, this::admitted, net, host -> {
          throw new IllegalStateException("no TLS here");
        }, url -> CoreProto.ProxyAnswer.newBuilder(), Disk.REAL, state, id -> Executor.downloadsDir(state).resolve("d" + id));
        network.start();
        // The core: its effects, in order, then the end.
        for (CoreProto.Effect e : plan.effects()) {
          loop.execute(() -> effect(e));
        }
        Thread.yield();
        try {
          switch (plan.until()) {
            case SPLICED -> spliced.get(10, TimeUnit.SECONDS);
            case DOWNLOADED -> downloaded.get(10, TimeUnit.SECONDS);
            case NOW -> {}
          }
        } catch (TimeoutException e) {
          // the end comes anyway
        }
        CompletableFuture<CompletableFuture<Void>> end = new CompletableFuture<>();
        loop.execute(() -> {
          over = true;
          end.complete(x.shutdown());
        });
        try {
          end.get().get(STOP_MS, TimeUnit.MILLISECONDS);
        } catch (TimeoutException e) {
          violation("RULE-SHELL-07: what the end waits for wasn't over within " + STOP_MS + " ms");
        }
        Map<String, String> downloads = listing();
        if (!downloads.isEmpty()) {
          FrayRun.saw("a download file there at the end");
        }
        String left = net.leftOpen();
        if (!left.isEmpty()) {
          violation("RULE-SHELL-06/07: after the end: " + left);
        }
        String registry = loop.submit(() -> x.in.left()).get();
        if (!registry.isEmpty()) {
          violation("RULE-SHELL-07: the registry after the end: " + registry);
        }
        net.dials.add(LAST);
        network.join();
        for (Thread t : threads) {
          t.join();
        }
        loop.shutdown();
        loop.awaitTermination(1, TimeUnit.MINUTES);
        if (!listing().equals(downloads)) {
          violation("RULE-SHELL-07: the downloads changed after the end's wait (S3): " + downloads + " -> " + listing());
        }
      } catch (InterruptedException | ExecutionException | IOException e) {
        throw new IllegalStateException(e);
      } finally {
        loop.shutdownNow();
        RegistryFrayTest.Run.delete(dir);
      }
      if (!violations.isEmpty()) {
        throw new AssertionError(violations + "\n  plan: " + plan);
      }
    }

    /** The core's effect, on the loop. */
    void effect(CoreProto.Effect e) {
      switch (e.getKindCase()) {
        case STREAM_OPEN -> issued.add(e.getStreamOpen().getId());
        case STREAM_RESOLVE -> resolving.add(e.getStreamResolve().getId());
        case STREAM_CLOSE -> closed.add(e.getStreamClose().getId());
        default -> {}
      }
      x.run(e);
    }

    /** What reached the core, on the loop: the model's checks, and the core's answers. */
    void admitted(CoreProto.Input in) {
      if (over) {
        violation("RULE-SHELL-07: " + in.getKindCase() + " after the end");
        return;
      }
      long id;
      switch (in.getKindCase()) {
        case STREAM_RESOLVED -> {
          id = in.getStreamResolved().getId();
          if (!resolving.remove(id) || closed.contains(id)) {
            violation("RULE-SHELL-04: resolved " + id + ", not being resolved");
          }
          return;
        }
        case STREAM_OPENED -> id = in.getStreamOpened().getId();
        case STREAM_DATA -> id = in.getStreamData().getId();
        case STREAM_EOF -> id = in.getStreamEof().getId();
        case STREAM_FAILED -> id = in.getStreamFailed().getId();
        case STREAM_SPLICED -> id = in.getStreamSpliced().getId();
        case DOWNLOADED, DOWNLOAD_FAILED -> {
          FrayRun.saw(in.getKindCase().toString());
          downloaded.complete(null);
          if (in.hasDownloaded()) {
            // The core has it: done with the file, now and then.
            if (seed % 2 == 0) {
              x.run(StreamsTest.discard(9));
            }
          }
          return;
        }
        default -> {
          violation("an input nothing asked for: " + in.getKindCase());
          return;
        }
      }
      if (resolving.contains(id) && in.hasStreamFailed()) {
        resolving.remove(id);
        return;
      }
      if (!issued.contains(id)) {
        violation("RULE-SHELL-04: " + in.getKindCase() + " about " + id + ", never opened");
      } else if (closed.contains(id)) {
        violation("RULE-SHELL-06: " + in.getKindCase() + " about " + id + " after its close");
      } else if (ended.contains(id)) {
        violation("RULE-SHELL-04: " + in.getKindCase() + " about " + id + " after its end");
      } else if (in.hasStreamOpened() && !opened.add(id)) {
        violation("RULE-SHELL-04: " + id + " opened twice");
      } else if (!in.hasStreamOpened() && !in.hasStreamFailed() && !opened.contains(id)) {
        violation("RULE-SHELL-04: " + in.getKindCase() + " about " + id + " before it opened");
      }
      if (in.hasStreamFailed()) {
        ended.add(id);
        FrayRun.saw("failed" + (opened.contains(id) ? "" : " connecting"));
      }
      if (in.hasStreamSpliced()) {
        // The core: the body is all in the file; then the shell closes the stream.
        FrayRun.saw("spliced");
        spliced.complete(null);
        x.run(StreamsTest.done(9));
        closed.add(id);
        x.run(StreamsTest.close(id, false));
      }
    }

    /** The network: each attempt answered as the plan says, until the end's last. */
    void network() {
      try {
        while (true) {
          MemoryNet.Dial d = net.dials.take();
          if (d == LAST) {
            return;
          }
          long id = net.all.indexOf(d) + 1L;
          long stream = streamOf(id);
          Answer a = plan.answers().getOrDefault(stream, Answer.NOW);
          FrayRun.saw("open " + a);
          switch (a) {
            case NOW -> peer(d.accept(), stream);
            case FAIL -> d.refuse("refused");
            case HOLD_THEN_ANSWER -> {
              d.awaitAborted();
              FrayRun.saw("connected after the shell gave up");
              d.accept(); // reset at once
            }
          }
        }
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
      }
    }

    /** The stream id of the k-th attempt (1-based): the plan's opens, in order. */
    long streamOf(long k) {
      long i = 0;
      for (CoreProto.Effect e : plan.effects()) {
        if (e.hasStreamOpen() && ++i == k) {
          return e.getStreamOpen().getId();
        }
      }
      return -1;
    }

    /** A far end: what it does, as the plan says, on a thread of its own. */
    void peer(MemoryNet.End e, long stream) {
      Peer p = plan.peers().getOrDefault(stream, Peer.TALK);
      Thread t = new Thread(() -> {
        try {
          if (p == Peer.RESET) {
            e.abort();
            return;
          }
          byte[] say;
          if (plan.scenario() == Scenario.DOWNLOAD) {
            say = new byte[plan.body()];
            new SplittableRandom(seed).nextBytes(say);
          } else {
            say = ("hello " + stream).getBytes(java.nio.charset.StandardCharsets.US_ASCII);
          }
          e.write(say);
          if (p == Peer.END) {
            e.finish();
          }
          // Read until the client goes.
          byte[] b = new byte[256];
          while (e.in.read(b, 0, b.length) >= 0) {
            // dropped
          }
          e.close();
        } catch (IOException x) {
          e.abort();
        }
      }, "peer-" + stream);
      threads.add(t);
      t.start();
    }

    Map<String, String> listing() throws IOException {
      Map<String, String> out = new TreeMap<>();
      if (!Files.isDirectory(Executor.downloadsDir(state))) {
        return out;
      }
      try (var files = Files.list(Executor.downloadsDir(state))) {
        for (Path f : files.toList()) {
          try {
            out.put(f.getFileName().toString(), Long.toString(Files.size(f)));
          } catch (IOException e) {
            out.put(f.getFileName().toString(), "gone");
          }
        }
      }
      return out;
    }
  }

  /** The network's last attempt: its thread ends. */
  private static final MemoryNet.Dial LAST = new MemoryNet.Dial(null, 0, 0);
}
