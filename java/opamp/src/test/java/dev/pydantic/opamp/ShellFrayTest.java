// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Comparator;
import java.util.List;
import java.util.Map;
import java.util.SplittableRandom;
import java.util.TreeMap;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import java.util.concurrent.atomic.AtomicInteger;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;

/**
 * The client as a whole (the real core, in Chicory) under Fray: its loop, its saver, the
 * callback, the application's threads and a {@link FakeHttp} network (one thread answering
 * requests, downloads and connection attempts, one completing WebSocket writes and answering
 * them), with stop coming at any point Fray picks. Each schedule's plan (how the network answers,
 * what the application calls, where the callback fails) comes from its seed. Scenarios: stop while
 * a connection is being opened (S1's ground), a send racing the Close stop sends, a download still
 * coming as stop comes (S3's), a save still writing when stop's bound runs out, an {@code Error}
 * from the callback (S6's), and calls made from inside the callback while the server's answers
 * keep coming.
 *
 * <p>Checked against the shell contract (spec/shell-contract.md), the callback model of {@link
 * CallbackPropertiesTest} (RULE-SHELL-01, -02) and, at the network, what the shell must release:
 * callbacks one at a time, none after {@code stopped}, {@code stopped} once and last, with the
 * error the callback threw, which {@code stop} gives too ({@code stopped} last even when the core
 * emits a notification after it in one output, as Go's and Python's schedule tests found); {@code
 * stop} returning, though a slow save is writing and the goodbye is held; after it,
 * nothing started, every connection the network opened closed, every request and attempt given
 * up, and the downloads left as they were; never two sends outstanding, nor a send after our
 * Close; and each connection's messages in order; and, where the callback calls the client, each
 * of its calls the next thing the core is fed after the step that called the callback, but for the
 * application's calls queued ahead of it (RULE-SHELL-10, read from the core's tap: {@link
 * CallbackAnswerTest#check}). See {@link FrayRun} for the number of schedules,
 * the seeds and replay. The core runs interpreted under Fray's instrumentation: runs are kept short.
 *
 * <p>In core mode (the {@code ...InCoreMode} scenarios) the same plans run over core mode's streams
 * in memory ({@link MemoryNet}, through the {@link Streams} seam): the network accepts, refuses or
 * holds each connection as the plan says (and accepts one after the client gave up on it), and a
 * minimal HTTP/1.1 and WebSocket server on the far end of each answers requests, downloads and
 * messages as the plan says (at once, slowly, failing, holding until the client goes). The same
 * checks, and after stop every connection the client had is closed or reset, and none is still
 * being made.
 */
// covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-06, RULE-SHELL-07, RULE-SHELL-10
class ShellFrayTest {
  /** Schedules per scenario in a normal run (OPAMP_FRAY_RUNS overrides it). */
  static final int RUNS = 12;
  /** Seconds a scenario may explore in a normal run before it stops early. */
  static final int BUDGET_S = 300;
  /**
   * How long stop may take on Fray's clock before it counts as never returning. Not the contract's
   * bound (one stop timeout and a margin): Fray 0.9.0's mocked clock can leap far ahead when every
   * thread waits (a schedule whose stop took 5 s in real time read 32 hours), so the bound itself
   * is checked in real time by {@link StopFoundTest}; here, that stop returns at all.
   */
  static final long STOP_BOUND_MS = 3_600_000;
  static final String CAPABILITY = "io.example.tick";
  static final byte[] PACKAGE = "the package".repeat(8).getBytes(java.nio.charset.StandardCharsets.US_ASCII);

  @Test
  void stopWhileOpening() throws Exception {
    explore("stopWhileOpening", Scenario.OPEN);
  }

  @Test
  void aSendRacingTheClose() throws Exception {
    explore("aSendRacingTheClose", Scenario.SEND_CLOSE);
  }

  @Test
  void aDownloadFinishingAfterStop() throws Exception {
    explore("aDownloadFinishingAfterStop", Scenario.DOWNLOAD);
  }

  @Test
  void anErrorFromTheCallback() throws Exception {
    explore("anErrorFromTheCallback", Scenario.CALLBACK_ERROR);
  }

  /**
   * Stop while a save is writing, on a disk slower than the stop timeout: stop gives up on the
   * save when its bound runs out, on Fray's clock. The bound once ran on JDK 25's ForkJoinPool
   * delay scheduler (CompletableFuture.completeOnTimeout), a thread Fray doesn't schedule: the
   * schedule never ended, and the nightly ran for hours in aDownloadFinishingAfterStop.
   */
  @Test
  void stopWhileASaveIsWriting() throws Exception {
    warmUp();
    FrayRun.explore("ShellFrayTest.stopWhileASaveIsWriting", RUNS, BUDGET_S, seed -> {
      Run run = new Run(seed, Plan.saveWriting());
      run.run();
      if (!run.saveBegun.isDone()) {
        throw new AssertionError("no save was writing when stop came: not the scenario's ground");
      }
    });
  }

  @Test
  void callsFromTheCallback() throws Exception {
    explore("callsFromTheCallback", Scenario.CALLBACK_CALL);
  }

  /** Schedules per core-mode scenario in a normal run (OPAMP_FRAY_RUNS overrides it). */
  static final int RUST_RUNS = 8;

  @Test
  void stopWhileOpeningInCoreMode() throws Exception {
    exploreCore("stopWhileOpeningInCoreMode", Scenario.OPEN);
  }

  @Test
  void aSendRacingTheCloseInCoreMode() throws Exception {
    exploreCore("aSendRacingTheCloseInCoreMode", Scenario.SEND_CLOSE);
  }

  @Test
  void aDownloadFinishingAfterStopInCoreMode() throws Exception {
    exploreCore("aDownloadFinishingAfterStopInCoreMode", Scenario.DOWNLOAD);
  }

  @Test
  void anErrorFromTheCallbackInCoreMode() throws Exception {
    exploreCore("anErrorFromTheCallbackInCoreMode", Scenario.CALLBACK_ERROR);
  }

  @Test
  void callsFromTheCallbackInCoreMode() throws Exception {
    exploreCore("callsFromTheCallbackInCoreMode", Scenario.CALLBACK_CALL);
  }

  private static void exploreCore(String name, Scenario scenario) throws Exception {
    warmUp();
    FrayRun.explore("ShellFrayTest." + name, RUST_RUNS, BUDGET_S,
        seed -> new CoreRun(seed, Plan.of(scenario, new SplittableRandom(seed))).run());
  }

  private static void explore(String name, Scenario scenario) throws Exception {
    warmUp();
    FrayRun.explore("ShellFrayTest." + name, RUNS, BUDGET_S, seed -> new Run(seed, Plan.of(scenario, new SplittableRandom(seed))).run());
  }

  /** The core's module parsed, and run a little, before Fray: not under its instrumentation, each schedule. */
  private static void warmUp() {
    for (boolean streams : new boolean[] {false, true}) {
      try (Core core = new Core(CoreProto.Config.newBuilder().setEndpoint("http://fake/v1/opamp").setStreams(streams)
          .setInstanceUid(ByteString.copyFrom(new byte[16])).setEntropy(ByteString.copyFrom(new byte[32])).build())) {
        core.step(0, 0, CoreProto.Input.newBuilder().setStart(CoreProto.Empty.getDefaultInstance()).build());
      }
    }
  }

  enum Scenario { OPEN, SEND_CLOSE, DOWNLOAD, CALLBACK_ERROR, CALLBACK_CALL }

  /** In {@link Scenario#CALLBACK_CALL}: the callback calls the client on its first notifications, this many. */
  static final int CALLBACK_CALLS = 3;

  /** How the network answers a request, a download or an attempt to open. */
  enum Answer { NOW, FAIL, SLOW, HOLD, HOLD_THEN_ANSWER }

  /** What the application does before it stops the client (WAIT_SAVE, until a save is writing: {@link Run} only). */
  enum Step { CALL, CALL_THREAD, YIELD, WAIT_DOWNLOAD, WAIT_SAVE }

  /** What the server does when a write of ours arrives, before answering it. */
  enum Wire { ANSWER, SERVER_CLOSE, DROP }

  /**
   * A schedule's plan: the transport; how the k-th request, download and attempt to open are
   * answered (then at once); the server's act at the k-th write; the application's steps; the
   * callback call that throws (0: none), and whether it throws an {@code Error}.
   */
  record Plan(Scenario scenario, boolean ws, List<Answer> opens, List<Answer> posts, List<Answer> gets,
      List<Wire> wire, List<Step> steps, int throwAt, boolean throwError, int slowSaveS, boolean holdGoodbye) {
    static Plan of(Scenario s, SplittableRandom r) {
      Answer[] all = Answer.values();
      List<Answer> opens = new ArrayList<>();
      List<Answer> posts = new ArrayList<>();
      List<Answer> gets = new ArrayList<>();
      List<Wire> wire = new ArrayList<>();
      List<Step> steps = new ArrayList<>();
      boolean ws = switch (s) {
        case OPEN, SEND_CLOSE -> true;
        case DOWNLOAD -> false;
        case CALLBACK_ERROR, CALLBACK_CALL -> r.nextBoolean();
      };
      for (int i = 0; i < 3; i++) {
        opens.add(s == Scenario.OPEN
            ? new Answer[] {Answer.NOW, Answer.FAIL, Answer.HOLD, Answer.HOLD_THEN_ANSWER, Answer.HOLD_THEN_ANSWER}[r.nextInt(5)]
            : Answer.NOW);
        posts.add(s == Scenario.CALLBACK_ERROR || s == Scenario.CALLBACK_CALL || s == Scenario.DOWNLOAD ? Answer.NOW
            : all[r.nextInt(all.length)]);
        gets.add(new Answer[] {Answer.NOW, Answer.SLOW, Answer.SLOW, Answer.HOLD_THEN_ANSWER, Answer.FAIL}[r.nextInt(5)]);
      }
      for (int i = 0; i < 6; i++) {
        wire.add(s == Scenario.SEND_CLOSE && r.nextInt(4) == 0 ? (r.nextBoolean() ? Wire.SERVER_CLOSE : Wire.DROP) : Wire.ANSWER);
      }
      if (s == Scenario.DOWNLOAD && r.nextInt(3) != 0) {
        steps.add(Step.WAIT_DOWNLOAD);
      }
      for (int n = (s == Scenario.SEND_CLOSE ? 1 : s == Scenario.CALLBACK_ERROR || s == Scenario.CALLBACK_CALL ? 2 : 0)
          + r.nextInt(3); n > 0; n--) {
        steps.add(new Step[] {Step.CALL, Step.CALL_THREAD, Step.YIELD}[r.nextInt(3)]);
      }
      int throwAt = s == Scenario.CALLBACK_ERROR ? 1 + r.nextInt(2) : 0;
      // A slow disk: each save takes this long (a save writing as stop comes must not hold stop
      // up for a second stop timeout).
      int slowSaveS = s == Scenario.DOWNLOAD ? new int[] {0, 0, 2, 20}[r.nextInt(4)] : 0;
      // The server holds what comes once stop is asked (the goodbye): the core spends its stop
      // timeout on it, and what it left is all a save still writing may have.
      boolean holdGoodbye = s == Scenario.DOWNLOAD && r.nextBoolean();
      return new Plan(s, ws, opens, posts, gets, wire, steps, throwAt, throwAt > 0 && r.nextInt(3) != 0, slowSaveS,
          holdGoodbye);
    }

    /**
     * Stop once a save is writing, on a disk that answers only after stop has returned (slowSaveS
     * -1): stop returns only by giving up on the save when its bound runs out.
     */
    static Plan saveWriting() {
      List<Answer> now = List.of(Answer.NOW, Answer.NOW, Answer.NOW);
      return new Plan(Scenario.DOWNLOAD, false, now, now, now, List.of(), List.of(Step.WAIT_SAVE), 0, false, -1, false);
    }

    static <T> T at(List<T> list, int k, T otherwise) {
      return k < list.size() ? list.get(k) : otherwise;
    }
  }

  /** A sentinel: the network's threads end. */
  private static final Object LAST = new Object();

  /** Where the core is tapped to for {@code plan}: a file in {@code dir} where the callback's calls are checked, else as the environment says. */
  static String tapFor(Plan plan, Path dir) {
    return plan.scenario() == Scenario.CALLBACK_CALL ? dir.resolve("core.tap").toString() : null;
  }

  /** One schedule. */
  static final class Run {
    final long seed;
    final Plan plan;
    final FakeHttp http = new FakeHttp();
    final List<String> violations = new CopyOnWriteArrayList<>();
    final Callbacks callbacks;
    final CompletableFuture<Void> downloadAsked = new CompletableFuture<>();
    /** A save has begun (a slow one is writing from then on). */
    final CompletableFuture<Void> saveBegun = new CompletableFuture<>();
    /** Stop has returned (or given up: the run is over). */
    final CompletableFuture<Void> stopReturned = new CompletableFuture<>();
    final AtomicInteger opens = new AtomicInteger();
    final AtomicInteger posts = new AtomicInteger();
    final AtomicInteger gets = new AtomicInteger();
    final AtomicInteger writes = new AtomicInteger();
    final AtomicInteger next = new AtomicInteger();
    volatile boolean stopping;
    Path dir;
    Path state;
    /** The core's tap, where the callback's calls are checked against it; else null. */
    String tap;

    Run(long seed, Plan plan) {
      this.seed = seed;
      this.plan = plan;
      this.callbacks = new Callbacks(plan.throwAt(), plan.throwError(), violations);
    }

    void violation(String what) {
      violations.add(what);
    }

    void run() {
      List<Thread> app = new ArrayList<>();
      Thread calls = new Thread(this::calls, "network-calls");
      Thread wire = new Thread(this::wire, "network-wire");
      try {
        dir = Files.createTempDirectory("fray-shell");
        if (plan.slowSaveS() != 0) {
          StateFile.beforeSave = p -> {
            FrayRun.saw("a slow save");
            saveBegun.complete(null);
            if (plan.slowSaveS() < 0) {
              stopReturned.join(); // a disk that answers only once stop has given up on it
              return;
            }
            try {
              Thread.sleep(plan.slowSaveS() * 1000L);
            } catch (InterruptedException e) {
              Thread.currentThread().interrupt();
            }
          };
        }
        state = plan.scenario() == Scenario.DOWNLOAD ? dir.resolve("state") : null;
        calls.start();
        wire.start();
        OpampClient.Config.Builder b = OpampClient.Config
            .builder((plan.ws() ? "ws" : "http") + "://fake/v1/opamp", new byte[16])
            .httpClient(http)
            .heartbeatInterval(Duration.ofSeconds(30));
        if (state != null) {
          b.stateFile(state).acceptPackages(true);
        }
        OpampClient c = OpampClient.start(b.build(), new OpampClient.Status(null, null, null, null, null,
            Opamp.CustomCapabilities.newBuilder().addCapabilities(CAPABILITY).build()), callbacks, null, tapFor(plan, dir),
            d -> d);
        tap = callbacks.calling(plan, c);
        for (Step s : plan.steps()) {
          switch (s) {
            case CALL -> c.setHealth(health(next.incrementAndGet()));
            case CALL_THREAD -> {
              int n = next.incrementAndGet();
              Thread t = new Thread(() -> c.setHealth(health(n)), "application");
              app.add(t);
              t.start();
            }
            case YIELD -> Thread.yield();
            case WAIT_DOWNLOAD -> {
              try {
                downloadAsked.get(10, TimeUnit.SECONDS);
              } catch (TimeoutException e) {
                // the core decides when: stop comes anyway
              }
            }
            case WAIT_SAVE -> {
              try {
                saveBegun.get(60, TimeUnit.SECONDS);
              } catch (TimeoutException e) {
                // the scenario checks it came: stop comes anyway
              }
            }
          }
        }
        stopping = true;
        CompletableFuture<Void> stop = c.stop();
        Throwable gave = null;
        try {
          stop.get(STOP_BOUND_MS, TimeUnit.MILLISECONDS);
        } catch (TimeoutException e) {
          violation("RULE-SHELL-07: stop didn't return within " + STOP_BOUND_MS + " ms");
        } catch (ExecutionException e) {
          gave = e.getCause();
        }
        stopReturned.complete(null);
        Map<String, String> downloads = listing();
        if (!downloads.isEmpty()) {
          FrayRun.saw("a download file there at stop");
        }
        http.seal("stop returned");
        for (Thread t : app) {
          t.join();
        }
        // Calls after stop do nothing.
        c.setHealth(health(0));
        http.calls.add(new FakeHttp.Request(-1, null, null));
        http.wire.add(LAST);
        calls.join();
        wire.join();
        check(gave, downloads);
      } catch (Exception e) {
        throw new IllegalStateException(e);
      } finally {
        stopReturned.complete(null);
        if (state != null && plan.slowSaveS() < 0) {
          StateFile.settled(state); // the save let go is over before its directory goes
        }
        StateFile.beforeSave = p -> {};
        RegistryFrayTest.Run.delete(dir);
      }
      if (!violations.isEmpty()) {
        throw new AssertionError(violations + "\n  plan: " + plan + "\n  notifications: " + callbacks.names());
      }
    }

    void check(Throwable gave, Map<String, String> downloads) throws IOException {
      callbacks.finish(gave);
      callbacks.checkCalls(tap);
      for (FakeHttp.FakeWs w : http.sockets) {
        if (!w.ended()) {
          violation("RULE-SHELL-06/07: " + w.open + " still open after stop");
        }
        long last = -1;
        for (byte[] m : w.sent) {
          long seq = Opamp.AgentToServer.parseFrom(Arrays.copyOfRange(m, 1, m.length)).getSequenceNum();
          if (seq < last || (seq == last && seq != 0)) { // (0 again: RULE-CLIENT-20)
            violation("messages out of order on " + w.open + ": " + seq + " after " + last);
          }
          last = seq;
        }
      }
      for (FakeHttp.Call k : http.all) {
        boolean done = k instanceof FakeHttp.Request r ? r.future.isDone() : ((FakeHttp.Open) k).future.isDone();
        if (!done) {
          violation("RULE-SHELL-07: " + k + " still running after stop");
        }
      }
      if (!listing().equals(downloads)) {
        violation("RULE-SHELL-07: the downloads changed after stop returned (S3): " + downloads + " -> " + listing());
      }
      violations.addAll(http.violations);
    }

    static Opamp.ComponentHealth health(int n) {
      return Opamp.ComponentHealth.newBuilder().setHealthy(true).setStatus("s" + n).build();
    }

    /** The server's answer to a message: a numbered custom message, and the package on offer. */
    byte[] reply(Opamp.AgentToServer m) {
      Opamp.ServerToAgent.Builder s = Opamp.ServerToAgent.newBuilder().setInstanceUid(m.getInstanceUid());
      if (plan.scenario() == Scenario.CALLBACK_ERROR || plan.scenario() == Scenario.CALLBACK_CALL) {
        s.setCustomMessage(Opamp.CustomMessage.newBuilder().setCapability(CAPABILITY).setType("tick")
            .setData(ByteString.copyFromUtf8(Long.toString(m.getSequenceNum()))));
      }
      if (plan.scenario() == Scenario.DOWNLOAD) {
        try {
          s.setPackagesAvailable(Opamp.PackagesAvailable.newBuilder()
              .putPackages("p", Opamp.PackageAvailable.newBuilder()
                  .setType(Opamp.PackageType.PackageType_TopLevel).setVersion("1")
                  .setHash(ByteString.copyFromUtf8("p1"))
                  .setFile(Opamp.DownloadableFile.newBuilder().setDownloadUrl("http://fake/p")
                      .setContentHash(ByteString.copyFrom(MessageDigest.getInstance("SHA-256").digest(PACKAGE))))
                  .build())
              .setAllPackagesHash(ByteString.copyFromUtf8("all1")));
        } catch (java.security.NoSuchAlgorithmException e) {
          throw new IllegalStateException(e);
        }
      }
      return s.build().toByteArray();
    }

    /** The network's calls: requests, downloads and attempts to open, each as the plan says. */
    void calls() {
      try {
        while (true) {
          FakeHttp.Call c = http.calls.take();
          if (c instanceof FakeHttp.Request r && r.request == null) {
            return;
          }
          if (c instanceof FakeHttp.Open o) {
            Answer a = Plan.at(plan.opens(), opens.getAndIncrement(), Answer.NOW);
            FrayRun.saw("open " + a);
            switch (a) {
              case NOW, SLOW -> o.open();
              case FAIL -> o.fail("refused");
              case HOLD -> o.future.handle((v, e) -> null).join();
              case HOLD_THEN_ANSWER -> {
                o.future.handle((v, e) -> null).join();
                FrayRun.saw("opened after the shell gave up");
                o.open(); // after the shell gave up on it: it must close it
              }
            }
          } else if (c instanceof FakeHttp.Request r) {
            boolean download = r.request.method().equals("GET");
            if (download) {
              downloadAsked.complete(null);
            }
            Answer a = download ? Plan.at(plan.gets(), gets.getAndIncrement(), Answer.NOW)
                : stopping && plan.holdGoodbye() ? Answer.HOLD
                : Plan.at(plan.posts(), posts.getAndIncrement(), Answer.NOW);
            byte[] body = download ? PACKAGE : reply(Opamp.AgentToServer.parseFrom(r.body));
            FrayRun.saw((download ? "download " : "request ") + a);
            switch (a) {
              case NOW -> r.answer(200, body);
              case SLOW -> r.answer(200, body, true);
              case FAIL -> r.fail("refused");
              case HOLD -> r.future.handle((v, e) -> null).join();
              case HOLD_THEN_ANSWER -> {
                r.future.handle((v, e) -> null).join();
                r.answer(200, body, true); // after the shell gave up on it: goes nowhere
              }
            }
          }
        }
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
      } catch (IOException e) {
        throw new IllegalStateException(e);
      }
    }

    /** The network's side of the WebSockets: each write completed, a message answered; the plan's acts first. */
    void wire() {
      try {
        while (true) {
          Object o = http.wire.take();
          if (o == LAST) {
            return;
          }
          FakeHttp.FakeWs w = (FakeHttp.FakeWs) o;
          Wire act = Plan.at(plan.wire(), writes.getAndIncrement(), Wire.ANSWER);
          if (act != Wire.ANSWER) {
            FrayRun.saw("server " + act);
          }
          switch (act) {
            case SERVER_CLOSE -> w.serverClose(1000);
            case DROP -> w.drop("dropped");
            case ANSWER -> {}
          }
          FakeHttp.FakeWs.Write done = w.completeNext(true);
          if (done != null) {
            FrayRun.saw(done.close() ? "our Close went" : "a send went");
          }
          if (done != null && !done.close()) {
            byte[] m = done.data();
            byte[] answer = reply(Opamp.AgentToServer.parseFrom(Arrays.copyOfRange(m, 1, m.length)));
            byte[] framed = new byte[answer.length + 1];
            System.arraycopy(answer, 0, framed, 1, answer.length);
            w.receive(framed);
          }
        }
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
      } catch (IOException e) {
        throw new IllegalStateException(e);
      }
    }

    Map<String, String> listing() throws IOException {
      Map<String, String> out = new TreeMap<>();
      if (state == null || !Files.isDirectory(Executor.downloadsDir(state))) {
        return out;
      }
      try (var files = Files.list(Executor.downloadsDir(state))) {
        for (Path f : files.sorted(Comparator.naturalOrder()).toList()) {
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

  /** The network's last dial: its thread ends. */
  private static final MemoryNet.Dial LAST_DIAL = new MemoryNet.Dial(null, 0, 0);

  /** One schedule in core mode: the client over {@link MemoryNet}, a small server on each connection's far end. */
  static final class CoreRun {
    final long seed;
    final Plan plan;
    final MemoryNet net = new MemoryNet(4096);
    final List<String> violations = new CopyOnWriteArrayList<>();
    final Callbacks callbacks;
    final CompletableFuture<Void> downloadAsked = new CompletableFuture<>();
    final AtomicInteger opens = new AtomicInteger();
    final AtomicInteger posts = new AtomicInteger();
    final AtomicInteger gets = new AtomicInteger();
    final AtomicInteger writes = new AtomicInteger();
    final AtomicInteger next = new AtomicInteger();
    final List<Thread> servers = new CopyOnWriteArrayList<>();
    /**
     * The scenario's ground reached: a connection asked for (OPEN), a message on the WebSocket
     * (SEND_CLOSE), an answer sent (CALLBACK_ERROR, CALLBACK_CALL). Core mode takes more turns of
     * the loop to get there (the proxy's answer, the connection, the request): most schedules wait
     * for it before the application's steps, so that stop comes in the middle of things rather than
     * before them.
     */
    final CompletableFuture<Void> reached = new CompletableFuture<>();
    volatile boolean stopping;
    Path dir;
    Path state;

    /** A Run of the same plan, for its server's replies and the downloads' listing. */
    final Run like;

    CoreRun(long seed, Plan plan) {
      this.seed = seed;
      this.plan = plan;
      this.callbacks = new Callbacks(plan.throwAt(), plan.throwError(), violations);
      this.like = new Run(seed, plan);
    }

    void violation(String what) {
      violations.add(what);
    }

    void run() {
      List<Thread> app = new ArrayList<>();
      Thread network = new Thread(this::network, "network");
      try {
        dir = Files.createTempDirectory("fray-shell-core");
        if (plan.slowSaveS() > 0) {
          StateFile.beforeSave = p -> {
            FrayRun.saw("a slow save");
            try {
              Thread.sleep(plan.slowSaveS() * 1000L);
            } catch (InterruptedException e) {
              Thread.currentThread().interrupt();
            }
          };
        }
        state = plan.scenario() == Scenario.DOWNLOAD ? dir.resolve("state") : null;
        like.state = state;
        network.start();
        OpampClient.Config.Builder b = OpampClient.Config
            .builder((plan.ws() ? "ws" : "http") + "://fake/v1/opamp", new byte[16])
            .mode(OpampClient.Mode.CORE)
            .streamsPlatform(net)
            .environment(Map.of())
            .heartbeatInterval(Duration.ofSeconds(30));
        if (state != null) {
          b.stateFile(state).acceptPackages(true);
        }
        OpampClient c = OpampClient.start(b.build(), new OpampClient.Status(null, null, null, null, null,
            Opamp.CustomCapabilities.newBuilder().addCapabilities(CAPABILITY).build()), callbacks, null, tapFor(plan, dir),
            d -> d);
        String tap = callbacks.calling(plan, c);
        if (Math.floorMod(seed, 3) != 0 && plan.scenario() != Scenario.DOWNLOAD) {
          try {
            reached.get(10, TimeUnit.SECONDS);
          } catch (TimeoutException e) {
            // stop comes anyway
          }
        }
        for (Step s : plan.steps()) {
          switch (s) {
            case CALL -> c.setHealth(Run.health(next.incrementAndGet()));
            case CALL_THREAD -> {
              int n = next.incrementAndGet();
              Thread t = new Thread(() -> c.setHealth(Run.health(n)), "application");
              app.add(t);
              t.start();
            }
            case YIELD -> Thread.yield();
            case WAIT_DOWNLOAD -> {
              try {
                downloadAsked.get(10, TimeUnit.SECONDS);
              } catch (TimeoutException e) {
                // the core decides when: stop comes anyway
              }
            }
          }
        }
        stopping = true;
        CompletableFuture<Void> stop = c.stop();
        Throwable gave = null;
        try {
          stop.get(STOP_BOUND_MS, TimeUnit.MILLISECONDS);
        } catch (TimeoutException e) {
          violation("RULE-SHELL-07: stop didn't return within " + STOP_BOUND_MS + " ms");
        } catch (ExecutionException e) {
          gave = e.getCause();
        }
        Map<String, String> downloads = like.listing();
        if (!downloads.isEmpty()) {
          FrayRun.saw("core: a download file there at stop");
        }
        // Nothing left: every connection the client had closed or reset, none still being made.
        String left = net.leftOpen();
        if (!left.isEmpty()) {
          violation("RULE-SHELL-06/07: after stop returned: " + left);
        }
        for (Thread t : app) {
          t.join();
        }
        // Calls after stop do nothing.
        c.setHealth(Run.health(0));
        net.dials.add(LAST_DIAL);
        network.join();
        for (Thread t : servers) {
          t.join();
        }
        callbacks.finish(gave);
        callbacks.checkCalls(tap);
        if (!like.listing().equals(downloads)) {
          violation("RULE-SHELL-07: the downloads changed after stop returned (S3): " + downloads + " -> " + like.listing());
        }
      } catch (Exception e) {
        throw new IllegalStateException(e);
      } finally {
        StateFile.beforeSave = p -> {};
        RegistryFrayTest.Run.delete(dir);
      }
      if (!violations.isEmpty()) {
        throw new AssertionError(violations + "\n  plan: " + plan + "\n  notifications: " + callbacks.names());
      }
    }

    /** Each connection asked for, as the plan says: made (a server on its far end), refused, or held. */
    void network() {
      try {
        while (true) {
          MemoryNet.Dial d = net.dials.take();
          if (d == LAST_DIAL) {
            return;
          }
          Answer a = Plan.at(plan.opens(), opens.getAndIncrement(), Answer.NOW);
          FrayRun.saw("core open " + a);
          if (plan.scenario() == Scenario.OPEN) {
            reached.complete(null);
          }
          switch (a) {
            case NOW, SLOW -> serve(d.accept());
            case FAIL -> d.refuse("connection refused");
            case HOLD -> d.awaitAborted();
            case HOLD_THEN_ANSWER -> {
              d.awaitAborted();
              FrayRun.saw("core: connected after the shell gave up");
              d.accept(); // the client's end is reset at once
            }
          }
        }
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
      }
    }

    void serve(MemoryNet.End e) {
      Thread t = new Thread(() -> conn(e), "server");
      servers.add(t);
      t.start();
    }

    /** HTTP/1.1 requests on one connection, one after another, until the client goes; or a WebSocket. */
    void conn(MemoryNet.End e) {
      try {
        java.io.InputStream in = new java.io.BufferedInputStream(e.input(), 4096);
        java.io.OutputStream out = e.output();
        long last = -1;
        while (true) {
          String line = line(in);
          if (line == null) {
            e.close();
            return;
          }
          Map<String, String> headers = new java.util.HashMap<>();
          for (String h = line(in); h != null && !h.isEmpty(); h = line(in)) {
            int colon = h.indexOf(':');
            headers.put(h.substring(0, colon).trim().toLowerCase(java.util.Locale.ROOT), h.substring(colon + 1).trim());
          }
          byte[] body = in.readNBytes(Integer.parseInt(headers.getOrDefault("content-length", "0")));
          if ("websocket".equalsIgnoreCase(headers.get("upgrade"))) {
            websocket(e, in, out, headers.get("sec-websocket-key"));
            return;
          }
          boolean download = line.startsWith("GET ");
          if (download) {
            downloadAsked.complete(null);
          }
          Answer a = download ? Plan.at(plan.gets(), gets.getAndIncrement(), Answer.NOW)
              : stopping && plan.holdGoodbye() ? Answer.HOLD
              : Plan.at(plan.posts(), posts.getAndIncrement(), Answer.NOW);
          byte[] answer;
          if (download) {
            answer = PACKAGE;
          } else {
            Opamp.AgentToServer m = Opamp.AgentToServer.parseFrom(body);
            // (0 again is the core holding the run's number 0: RULE-CLIENT-20.)
            if (m.getSequenceNum() < last || (m.getSequenceNum() == last && last != 0)) {
              violation("messages out of order on a connection: " + m.getSequenceNum() + " after " + last);
            }
            last = m.getSequenceNum();
            answer = like.reply(m);
          }
          FrayRun.saw("core " + (download ? "download " : "request ") + a);
          if (plan.scenario() == Scenario.CALLBACK_ERROR || plan.scenario() == Scenario.CALLBACK_CALL) {
            reached.complete(null);
          }
          switch (a) {
            case NOW -> respond(out, answer, false);
            case SLOW -> respond(out, answer, true);
            case FAIL -> {
              e.abort();
              return;
            }
            case HOLD -> {
              drain(in);
              e.close();
              return;
            }
            case HOLD_THEN_ANSWER -> {
              drain(in);
              try {
                respond(out, answer, true); // after the client gave up on it: goes nowhere
              } catch (IOException gone) {
                // as expected
              }
              e.close();
              return;
            }
          }
        }
      } catch (IOException x) {
        e.abort(); // the client went
      }
    }

    /** The WebSocket's side: the handshake, then each message answered (the plan's act first). */
    void websocket(MemoryNet.End e, java.io.InputStream in, java.io.OutputStream out, String key) throws IOException {
      String accept;
      try {
        accept = java.util.Base64.getEncoder().encodeToString(MessageDigest.getInstance("SHA-1")
            .digest((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").getBytes(java.nio.charset.StandardCharsets.US_ASCII)));
      } catch (java.security.NoSuchAlgorithmException x) {
        throw new IllegalStateException(x);
      }
      out.write(("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "
          + accept + "\r\n\r\n").getBytes(java.nio.charset.StandardCharsets.US_ASCII));
      java.io.DataInputStream din = new java.io.DataInputStream(in);
      boolean closeSent = false;
      long last = -1;
      while (true) {
        int b0 = din.read();
        if (b0 < 0) {
          e.close();
          return;
        }
        int b1 = din.readUnsignedByte();
        long len = b1 & 0x7f;
        if (len == 126) {
          len = din.readUnsignedShort();
        } else if (len == 127) {
          len = din.readLong();
        }
        byte[] mask = new byte[4];
        if ((b1 & 0x80) != 0) {
          din.readFully(mask);
        }
        byte[] payload = new byte[(int) len];
        din.readFully(payload);
        for (int i = 0; i < payload.length; i++) {
          payload[i] ^= mask[i % 4];
        }
        switch (b0 & 0x0f) {
          case 0, 2 -> {
            long seq = Opamp.AgentToServer.parseFrom(Arrays.copyOfRange(payload, 1, payload.length)).getSequenceNum();
            if (seq < last || (seq == last && seq != 0)) { // (0 again: RULE-CLIENT-20)
              violation("messages out of order on a WebSocket: " + seq + " after " + last);
            }
            last = seq;
            Wire act = Plan.at(plan.wire(), writes.getAndIncrement(), Wire.ANSWER);
            if (plan.scenario() != Scenario.OPEN) {
              reached.complete(null);
            }
            if (act != Wire.ANSWER) {
              FrayRun.saw("core: server " + act);
            }
            switch (act) {
              case SERVER_CLOSE -> {
                frame(out, 8, new byte[] {0x03, (byte) 0xe8});
                closeSent = true;
              }
              case DROP -> {
                e.abort();
                return;
              }
              case ANSWER -> {
                if (!closeSent) {
                  byte[] answer = like.reply(Opamp.AgentToServer.parseFrom(Arrays.copyOfRange(payload, 1, payload.length)));
                  byte[] framed = new byte[answer.length + 1];
                  System.arraycopy(answer, 0, framed, 1, answer.length);
                  frame(out, 2, framed);
                }
              }
            }
          }
          case 8 -> {
            FrayRun.saw("core: our Close came");
            if (!closeSent) {
              frame(out, 8, payload);
              closeSent = true;
            }
            e.finish();
          }
          case 9 -> frame(out, 10, payload);
          default -> {}
        }
      }
    }

    static void respond(java.io.OutputStream out, byte[] body, boolean slow) throws IOException {
      byte[] head = ("HTTP/1.1 200 OK\r\nContent-Type: application/x-protobuf\r\nContent-Length: " + body.length + "\r\n\r\n")
          .getBytes(java.nio.charset.StandardCharsets.US_ASCII);
      out.write(head);
      if (!slow) {
        out.write(body);
        return;
      }
      int half = body.length / 2;
      Thread.yield();
      out.write(body, 0, half);
      Thread.yield();
      out.write(body, half, body.length - half);
    }

    static void frame(java.io.OutputStream out, int opcode, byte[] payload) throws IOException {
      java.io.ByteArrayOutputStream f = new java.io.ByteArrayOutputStream();
      f.write(0x80 | opcode);
      if (payload.length < 126) {
        f.write(payload.length);
      } else {
        f.write(126);
        f.write(payload.length >>> 8);
        f.write(payload.length);
      }
      f.writeBytes(payload);
      out.write(f.toByteArray());
    }

    /** Read until the client goes. */
    static void drain(java.io.InputStream in) {
      try {
        while (in.read() >= 0) {
          // what comes is dropped
        }
      } catch (IOException e) {
        // reset
      }
    }

    static String line(java.io.InputStream in) throws IOException {
      StringBuilder b = new StringBuilder();
      for (int c = in.read(); c != '\n'; c = in.read()) {
        if (c < 0) {
          return b.length() == 0 ? null : b.toString();
        }
        if (c != '\r') {
          b.append((char) c);
        }
      }
      return b.toString();
    }
  }

  /**
   * The callback, and the model it is checked against (CallbackPropertiesTest's, RULE-SHELL-01 and
   * -02): one at a time, none before start returned or after stopped; it throws at its k-th call.
   */
  static final class Callbacks implements java.util.function.Consumer<Notification> {
    final int throwAt;
    final boolean throwError;
    final List<String> violations;
    final AtomicInteger inProgress = new AtomicInteger();
    final AtomicInteger calls = new AtomicInteger();
    final List<Notification> seen = new CopyOnWriteArrayList<>();
    final AtomicInteger stopped = new AtomicInteger();
    volatile Notification.Stopped firstStopped;
    volatile Throwable thrown;
    /** The client, once start has returned it, where the callback calls it ({@link Scenario#CALLBACK_CALL}); else null. */
    volatile OpampClient client;
    /** The calls the callback made. */
    final List<CallbackAnswerTest.Asked> asked = new CopyOnWriteArrayList<>();

    Callbacks(int throwAt, boolean throwError, List<String> violations) {
      this.throwAt = throwAt;
      this.throwError = throwError;
      this.violations = violations;
    }

    /** Start has returned {@code c}: in {@link Scenario#CALLBACK_CALL} the callback calls it from now on. The core's tap then, else null. */
    String calling(Plan plan, OpampClient c) {
      if (plan.scenario() != Scenario.CALLBACK_CALL) {
        return null;
      }
      client = c;
      return c.tapPath();
    }

    /** RULE-SHELL-10, against the core's tap (written out by the time stop returned): each call the callback made. */
    void checkCalls(String tap) {
      if (tap == null) {
        return;
      }
      FrayRun.saw("calls from the callback checked: " + Math.min(asked.size(), CALLBACK_CALLS));
      try {
        String wrong = CallbackAnswerTest.check(Path.of(tap), asked, true);
        if (wrong != null) {
          violations.add(wrong);
        }
      } catch (Exception e) {
        violations.add("the core's tap: " + e);
      }
    }

    @Override
    public void accept(Notification n) {
      if (inProgress.incrementAndGet() > 1) {
        violations.add("RULE-SHELL-01: two callbacks at once (" + n + ")");
      }
      try {
        int k = calls.incrementAndGet();
        if (firstStopped != null) {
          violations.add("RULE-SHELL-01: a callback after stopped: " + n.getClass().getSimpleName());
        }
        seen.add(n);
        if (n instanceof Notification.Stopped s) {
          stopped.incrementAndGet();
          if (firstStopped == null) {
            firstStopped = s;
          }
        }
        OpampClient c = client;
        if (c != null && asked.size() < CALLBACK_CALLS && !(n instanceof Notification.Stopped)) {
          // A call from inside the callback: the server's answers keep coming meanwhile.
          String status = "callback " + k;
          c.setHealth(Opamp.ComponentHealth.newBuilder().setHealthy(true).setStatus(status).build());
          asked.add(CallbackAnswerTest.health(k, status));
        }
        if (thrown == null && k == throwAt && !(n instanceof Notification.Stopped)) {
          Throwable t = throwError ? new AssertionError("the application's error, on callback " + k)
              : new IllegalStateException("the application's bug, on callback " + k);
          thrown = t;
          FrayRun.saw("callback threw " + t.getClass().getSimpleName());
          if (t instanceof Error e) {
            throw e;
          }
          throw (RuntimeException) t;
        }
      } finally {
        inProgress.decrementAndGet();
      }
    }

    /** At the end: stopped once and last, with the error thrown, which stop gave too. */
    void finish(Throwable gave) {
      if (stopped.get() != 1) {
        violations.add("RULE-SHELL-02: stopped " + stopped.get() + " times");
      } else if (!(seen.get(seen.size() - 1) instanceof Notification.Stopped)) {
        violations.add("RULE-SHELL-01: stopped isn't last");
      }
      if (firstStopped != null && firstStopped.error() != thrown) {
        violations.add("RULE-SHELL-02: stopped with " + firstStopped.error() + ", the model expects " + thrown);
      }
      if (gave != thrown) {
        violations.add("RULE-SHELL-02: stop gave " + gave + ", the model expects " + thrown);
      }
    }

    List<String> names() {
      List<String> out = new ArrayList<>();
      for (Notification n : seen) {
        out.add(n instanceof Notification.Stopped s ? "Stopped(" + s.error() + ")" : n.getClass().getSimpleName());
      }
      return out;
    }
  }
}
