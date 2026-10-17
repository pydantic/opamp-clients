// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.SplittableRandom;
import java.util.TreeMap;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import org.junit.jupiter.api.Test;

/**
 * The in-flight registry ({@link InFlight}) and the executor's async edges, under Fray: a
 * small shell around {@link Executor} with no core, its loop on a thread of its own as {@link
 * OpampClient#feed} has it (effects carried out in order, each input admitted or dropped there,
 * nothing after the end), the test playing the core (effects, cancels, then the end) and a
 * {@link FakeHttp} network answering on a thread of its own: at once, with a failure, slowly, or
 * holding until the shell gives up and then answering anyway (a result after its cancel, a
 * connection that opens after it). Fray explores the interleavings of the three; the plan (what
 * the core asks, what it cancels, how the network answers) comes from the schedule's seed.
 *
 * <p>Checked against the shell contract (spec/shell-contract.md) as a model of what reaches the
 * core: an input only about an effect issued, of its kind, at most one end each, and nothing about
 * an effect after its cancel (RULE-SHELL-04, -06); after the end, every connection the network
 * opened is closed, every request and attempt given up (RULE-SHELL-06, -07), the end's wait over
 * within the stop bound, and no download file appearing or changing after it (S3). See {@link
 * FrayRun} for the number of schedules, the seeds and replay.
 */
// covers: RULE-SHELL-04, RULE-SHELL-06, RULE-SHELL-07
class RegistryFrayTest {
  /** Schedules per scenario in a normal run (OPAMP_FRAY_RUNS overrides it). */
  static final int RUNS = 200;
  /** Seconds a scenario may explore in a normal run before it stops early. */
  static final int BUDGET_S = 300;
  /** How long the end may wait for what is finishing (OpampClient's stop timeout). */
  static final long STOP_MS = 5_000;

  /**
   * A request, a connection and a download (and with a state file, a save), some cancelled by the
   * core while the network answers: nothing about a cancelled effect reaches the core, and a
   * connection that opens after its cancel is closed.
   */
  @Test
  void aResultAfterItsCancelIsDropped() throws Exception {
    FrayRun.explore("RegistryFrayTest.aResultAfterItsCancelIsDropped", RUNS, BUDGET_S,
        seed -> new Run(seed, Scenario.CANCEL).run());
  }

  /** Connections being opened as the client ends: one that opens after the end is closed. */
  @Test
  void aConnectionOpenedLateIsClosed() throws Exception {
    FrayRun.explore("RegistryFrayTest.aConnectionOpenedLateIsClosed", RUNS, BUDGET_S,
        seed -> new Run(seed, Scenario.LATE_OPEN).run());
  }

  /** A download whose body is still coming as the client ends: nothing of it is left, or changes, after. */
  @Test
  void aDownloadFinishingAfterTheEndLeavesNothing() throws Exception {
    FrayRun.explore("RegistryFrayTest.aDownloadFinishingAfterTheEndLeavesNothing", RUNS, BUDGET_S,
        seed -> new Run(seed, Scenario.DOWNLOAD).run());
  }

  enum Scenario { CANCEL, LATE_OPEN, DOWNLOAD }

  /** How the network answers a call. */
  enum Answer { NOW, FAIL, SLOW, HOLD, HOLD_THEN_ANSWER }

  /** One schedule's plan: the effects in order (cancels among them), and how each call is answered. */
  record Plan(Scenario scenario, boolean stateFile, List<CoreProto.Effect> effects, Map<Long, Answer> answers) {
    static Plan of(Scenario scenario, SplittableRandom r) {
      List<CoreProto.Effect> effects = new ArrayList<>();
      Map<Long, Answer> answers = new TreeMap<>();
      Answer[] all = Answer.values();
      boolean state = scenario == Scenario.DOWNLOAD || r.nextBoolean();
      long id = 1;
      int n = switch (scenario) {
        case CANCEL -> 3 + r.nextInt(3);
        case LATE_OPEN -> 1 + r.nextInt(2);
        case DOWNLOAD -> 1 + r.nextInt(2);
      };
      List<Long> started = new ArrayList<>();
      for (int i = 0; i < n; i++, id++) {
        int kind = switch (scenario) {
          case CANCEL -> r.nextInt(state ? 4 : 2) == 0 ? 0 : r.nextInt(state ? 4 : 2);
          case LATE_OPEN -> r.nextInt(3) == 0 ? 0 : 1;
          case DOWNLOAD -> r.nextInt(3) == 0 ? 0 : 2;
        };
        effects.add(switch (kind) {
          case 0 -> CoreProto.Effect.newBuilder().setHttpRequest(CoreProto.HttpRequest.newBuilder()
              .setId(id).setUrl("http://fake/v1/opamp").setBody(ByteString.copyFromUtf8("req" + id))).build();
          case 1 -> CoreProto.Effect.newBuilder().setWsOpen(CoreProto.WsOpen.newBuilder()
              .setId(id).setUrl("ws://fake/v1/opamp")).build();
          case 2 -> CoreProto.Effect.newBuilder().setDownload(CoreProto.Download.newBuilder()
              .setId(id).setUrl("http://fake/p" + id).setMaxBytes(1 << 20)).build();
          default -> CoreProto.Effect.newBuilder().setStore(CoreProto.Store.newBuilder()
              .setId(id).setData(ByteString.copyFromUtf8("state " + id))).build();
        });
        if (kind != 3) {
          started.add(id);
          Answer a = scenario == Scenario.LATE_OPEN && kind == 1
              ? (r.nextInt(4) == 0 ? Answer.NOW : Answer.HOLD_THEN_ANSWER)
              : scenario == Scenario.DOWNLOAD && kind == 2
                  ? (r.nextInt(3) == 0 ? Answer.HOLD_THEN_ANSWER : Answer.SLOW)
                  : all[r.nextInt(all.length)];
          answers.put(id, a);
        }
        // The core gives up on something under way, now and then (the scenario's point).
        if (scenario == Scenario.CANCEL && !started.isEmpty() && r.nextInt(2) == 0) {
          long victim = started.get(r.nextInt(started.size()));
          effects.add(CoreProto.Effect.newBuilder().setCancel(CoreProto.Id.newBuilder().setId(victim)).build());
        }
      }
      return new Plan(scenario, state, effects, answers);
    }

    String describe() {
      List<String> es = new ArrayList<>();
      for (CoreProto.Effect e : effects) {
        es.add(e.getKindCase() + "(" + id(e) + ")");
      }
      return scenario + " state=" + stateFile + " effects=" + es + " answers=" + answers;
    }
  }

  static long id(CoreProto.Effect e) {
    return switch (e.getKindCase()) {
      case HTTP_REQUEST -> e.getHttpRequest().getId();
      case WS_OPEN -> e.getWsOpen().getId();
      case DOWNLOAD -> e.getDownload().getId();
      case STORE -> e.getStore().getId();
      case CANCEL -> e.getCancel().getId();
      default -> -1;
    };
  }

  /** The end, posted to the shell's loop. */
  private static final Object END = new Object();

  /** One schedule. */
  static final class Run {
    final long seed;
    final Plan plan;
    final FakeHttp http = new FakeHttp();
    final List<String> violations = new CopyOnWriteArrayList<>();
    final LinkedBlockingQueue<Object> loop = new LinkedBlockingQueue<>();
    final CompletableFuture<CompletableFuture<Void>> ended = new CompletableFuture<>();
    // The model: what the core issued, of which kind; what has ended; what it cancelled. Loop only.
    final Map<Long, CoreProto.Effect.KindCase> issued = new HashMap<>();
    final Set<Long> over = new HashSet<>();
    final Set<Long> cancelled = new HashSet<>();
    Path dir;
    Path state;
    Executor x;

    Run(long seed, Scenario scenario) {
      this.seed = seed;
      this.plan = Plan.of(scenario, new SplittableRandom(seed));
    }

    void violation(String what) {
      violations.add(what);
    }

    void run() {
      try {
        dir = Files.createTempDirectory("fray-registry");
        state = plan.stateFile() ? dir.resolve("state") : null;
        x = new Executor(Transport.of(http), loop::add, state);
        Thread shell = new Thread(this::shell, "shell-loop");
        Thread net = new Thread(this::network, "network");
        shell.start();
        net.start();
        // The core: its effects, in order, then the end.
        for (CoreProto.Effect e : plan.effects()) {
          loop.add(e);
        }
        loop.add(END);
        CompletableFuture<Void> finishing = ended.get();
        try {
          finishing.get(STOP_MS, TimeUnit.MILLISECONDS);
        } catch (TimeoutException e) {
          violation("RULE-SHELL-07: what the end waits for wasn't over within " + STOP_MS + " ms");
        }
        Map<String, String> downloads = listing();
        if (!downloads.isEmpty()) {
          FrayRun.saw("a download file there at the end");
        }
        http.seal("the end's wait was over");
        http.calls.add(new FakeHttp.Request(-1, null, null)); // the network's last call
        net.join();
        shell.join();
        // Every connection the network opened is closed; every call is over (answered or given up).
        for (FakeHttp.FakeWs w : http.sockets) {
          if (!w.ended()) {
            violation("RULE-SHELL-06/07: " + w.open + " still open after the end");
          }
        }
        for (FakeHttp.Call c : http.all) {
          boolean done = c instanceof FakeHttp.Request r ? r.future.isDone() : ((FakeHttp.Open) c).future.isDone();
          if (!done) {
            violation("RULE-SHELL-07: " + c + " still running after the end");
          }
        }
        if (!listing().equals(downloads)) {
          violation("RULE-SHELL-07: the downloads changed after the end's wait (S3): " + downloads + " -> " + listing());
        }
        violations.addAll(http.violations);
      } catch (Exception e) {
        throw new IllegalStateException(e);
      } finally {
        delete(dir);
      }
      if (!violations.isEmpty()) {
        throw new AssertionError(violations + "\n  plan: " + plan.describe());
      }
    }

    /** The shell's loop: effects carried out, inputs admitted (as OpampClient.feed), until the end. */
    void shell() {
      try {
        while (true) {
          Object o = loop.take();
          if (o == END) {
            ended.complete(x.close());
            return; // nothing more is read: nothing reaches the core after the end
          }
          if (o instanceof CoreProto.Effect e) {
            if (e.hasCancel()) {
              cancelled.add(e.getCancel().getId());
            } else {
              issued.put(RegistryFrayTest.id(e), e.getKindCase());
            }
            x.run(e);
          } else if (o instanceof CoreProto.Input in) {
            if (x.admit(in)) {
              admitted(in);
            } else if (in.getKindCase() != CoreProto.Input.KindCase.WS_MESSAGE) {
              FrayRun.saw("dropped " + in.getKindCase() + (cancelled.isEmpty() ? "" : " (with a cancel)"));
            }
          }
        }
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
      }
    }

    /** An input reached the core: the model's checks. */
    void admitted(CoreProto.Input in) {
      long id;
      CoreProto.Effect.KindCase of;
      boolean last = true;
      switch (in.getKindCase()) {
        case HTTP_RESPONSE -> { id = in.getHttpResponse().getId(); of = CoreProto.Effect.KindCase.HTTP_REQUEST; }
        case HTTP_FAILED -> { id = in.getHttpFailed().getId(); of = CoreProto.Effect.KindCase.HTTP_REQUEST; }
        case WS_OPENED -> { id = in.getWsOpened().getId(); of = CoreProto.Effect.KindCase.WS_OPEN; last = false; }
        case WS_FAILED -> { id = in.getWsFailed().getId(); of = CoreProto.Effect.KindCase.WS_OPEN; }
        case WS_LOST -> { id = in.getWsLost().getId(); of = CoreProto.Effect.KindCase.WS_OPEN; }
        case WS_MESSAGE -> { id = in.getWsMessage().getId(); of = CoreProto.Effect.KindCase.WS_OPEN; last = false; }
        case WS_CLOSED -> { id = in.getWsClosed().getId(); of = CoreProto.Effect.KindCase.WS_OPEN; last = false; }
        case DOWNLOADED, DOWNLOAD_RESPONSE, DOWNLOAD_FAILED -> {
          id = in.hasDownloaded() ? in.getDownloaded().getId()
              : in.hasDownloadResponse() ? in.getDownloadResponse().getId() : in.getDownloadFailed().getId();
          of = CoreProto.Effect.KindCase.DOWNLOAD;
        }
        case STORED -> { id = in.getStored().getId(); of = CoreProto.Effect.KindCase.STORE; }
        case STORE_FAILED -> { id = in.getStoreFailed().getId(); of = CoreProto.Effect.KindCase.STORE; }
        default -> {
          return;
        }
      }
      if (issued.get(id) != of) {
        violation("RULE-SHELL-04: " + in.getKindCase() + " names " + id + ", which is " + issued.get(id));
      } else if (cancelled.contains(id)) {
        violation("RULE-SHELL-06: " + in.getKindCase() + " of " + id + " after its cancel");
      } else if (over.contains(id)) {
        violation("RULE-SHELL-04: " + in.getKindCase() + " of " + id + " after its end");
      }
      if (last) {
        over.add(id);
      }
    }

    /** The network: each call answered as the plan says, until the end's last call. */
    void network() {
      try {
        while (true) {
          FakeHttp.Call c = http.calls.take();
          if (c instanceof FakeHttp.Request r && r.request == null) {
            return;
          }
          long id = callId(c);
          Answer a = plan.answers().getOrDefault(id, Answer.NOW);
          if (c instanceof FakeHttp.Request r) {
            byte[] body = ("body of " + id).repeat(4).getBytes(java.nio.charset.StandardCharsets.US_ASCII);
            switch (a) {
              case NOW -> r.answer(200, body);
              case SLOW -> r.answer(200, body, true);
              case FAIL -> r.fail("refused");
              case HOLD -> r.future.handle((v, e) -> null).join();
              case HOLD_THEN_ANSWER -> {
                r.future.handle((v, e) -> null).join();
                r.answer(200, body); // after the shell gave up on it: goes nowhere
              }
            }
          } else if (c instanceof FakeHttp.Open o) {
            switch (a) {
              case NOW, SLOW -> o.open();
              case FAIL -> o.fail("refused");
              case HOLD -> o.future.handle((v, e) -> null).join();
              case HOLD_THEN_ANSWER -> {
                o.future.handle((v, e) -> null).join();
                FrayRun.saw("opened after the shell gave up");
                o.open(); // opens after the shell gave up on it: it must close it
              }
            }
          }
        }
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
      }
    }

    /** The effect a call is for: its URL's id (the plan names them so), from the request's path or body. */
    long callId(FakeHttp.Call c) {
      if (c instanceof FakeHttp.Open o) {
        return wsIds().get(http.all.stream().filter(k -> k instanceof FakeHttp.Open).toList().indexOf(o));
      }
      FakeHttp.Request r = (FakeHttp.Request) c;
      String path = r.request.uri().getPath();
      if (path.startsWith("/p")) {
        return Long.parseLong(path.substring(2));
      }
      return Long.parseLong(new String(r.body, java.nio.charset.StandardCharsets.US_ASCII).substring(3));
    }

    /** The ids of the plan's ws_opens, in order (the shell opens them in that order). */
    List<Long> wsIds() {
      List<Long> ids = new ArrayList<>();
      for (CoreProto.Effect e : plan.effects()) {
        if (e.hasWsOpen()) {
          ids.add(e.getWsOpen().getId());
        }
      }
      return ids;
    }

    /** The downloads directory: each file's size, by name. */
    Map<String, String> listing() throws IOException {
      Map<String, String> out = new TreeMap<>();
      if (state == null || !Files.isDirectory(Executor.downloadsDir(state))) {
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

    static void delete(Path dir) {
      if (dir == null) {
        return;
      }
      try (var files = Files.walk(dir)) {
        files.sorted(Comparator.reverseOrder()).forEach(p -> p.toFile().delete());
      } catch (IOException e) {
        // a temporary directory
      }
    }
  }
}
