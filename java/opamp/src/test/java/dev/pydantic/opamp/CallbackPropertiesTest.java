// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.SplittableRandom;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ConcurrentLinkedQueue;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicReference;
import java.util.function.BooleanSupplier;
import java.util.function.Consumer;
import java.util.function.Function;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.EnumSource;

/**
 * The callback contract (spec/shell-contract.md), checked against a model over random runs of the
 * real core and the local test servers of {@link PropertiesTest}, over either transport. Each run
 * is a seeded sequence of steps: start (with a state file holding a saved remote config, so the
 * core hands it over on the first step, or without one); application calls from the test's
 * thread, from a thread of their own, or from inside the callback; a callback that throws on its
 * k-th call; stop from outside, or requested from inside the callback; the server offering a
 * remote config, a custom message (every answer carries a numbered one) or a heartbeat interval.
 * After each step the notifications seen so far are checked against the model: never two
 * callbacks at once, none before {@code start} returned, none after {@code stopped}, the server's
 * custom messages in its order and the saved config before them; and at the end, {@code stopped}
 * exactly once, with the error the callback threw, and every {@code stop} returning within its
 * bound with that error; and in the core's tap, each call the callback made (its answers to
 * remote configs, its numbered calls) the next thing the core was fed after the step that called
 * the callback, but for the application's calls queued ahead of it (RULE-SHELL-10, {@link
 * CallbackAnswerTest#check}).
 *
 * <p>Each test runs in both modes: platform mode (the JDK's HTTP client) and core mode (the core
 * speaking HTTP and WebSocket over this client's sockets, {@link Streams}), against the same
 * servers.
 *
 * <p>OPAMP_MODEL_RUNS sets the number of runs per mode (default 30); a failure prints its run's
 * seed, and OPAMP_MODEL_SEED replays that run alone.
 *
 * <p>Known breaks (spec/shell-contract.md): S5, a callback before {@code start} returns, is counted and logged
 * with the seed if it happens (the client holds its loop until start has returned). S6 and N1 are
 * fixed, and the seeds that showed them are replayed apart.
 */
class CallbackPropertiesTest {
  private static final byte[] UID = new byte[16];
  private static final String CAPABILITY = "io.example.tick";
  private static final String SAVED = "saved";
  /** stop's bound: the longest request timeout in flight (2 s, set below) plus a margin. */
  private static final long STOP_BOUND_MS = 10_000;
  /** How long a step waits for what it set off (soft: the core decides when, a miss isn't a failure). */
  private static final long STEP_WAIT_MS = 3_000;

  static {
    java.util.Arrays.fill(UID, (byte) 0x37);
  }

  enum Op { CALL_OUTSIDE, CALL_THREAD, CALL_INSIDE, OFFER_CONFIG, OFFER_CUSTOM, OFFER_HEARTBEAT, WAIT, STOP_OUTSIDE, STOP_INSIDE }

  /** A step, and its argument (how long WAIT waits, in ms). */
  record Step(Op op, int arg) {}

  /**
   * A run: the transport, whether it starts with a saved remote config, the callback call that
   * throws (1-based; 0: none; never a {@code stopped}), whether it throws an {@code Error} rather
   * than a {@code RuntimeException}, whether it throws again at the {@code stopped} that reports
   * it, whether it throws at the {@code stopped} of a stop instead, and the
   * steps. A final stop from outside always follows them.
   */
  record Plan(boolean ws, boolean saved, int throwAt, boolean throwError, boolean throwAgain, boolean throwAtStopped,
      List<Step> steps) {}

  private static final Op[] CALM = {Op.CALL_OUTSIDE, Op.CALL_THREAD, Op.CALL_INSIDE, Op.OFFER_CONFIG, Op.OFFER_CUSTOM,
      Op.OFFER_HEARTBEAT, Op.WAIT};

  static Plan plan(SplittableRandom r) {
    boolean ws = r.nextBoolean();
    boolean saved = r.nextBoolean();
    int throwAt = r.nextInt(3) == 0 ? 0 : 1 + r.nextInt(8);
    List<Step> steps = new ArrayList<>();
    for (int n = 1 + r.nextInt(10); n > 0; n--) {
      int pick = r.nextInt(2 * CALM.length + 2);
      Op op = pick < 2 * CALM.length ? CALM[pick / 2] : pick == 2 * CALM.length ? Op.STOP_OUTSIDE : Op.STOP_INSIDE;
      steps.add(new Step(op, r.nextInt(120)));
    }
    // An Error as well as a RuntimeException (S6, fixed); with no throw before it, a throw at the
    // stopped of the final stop (N1, fixed).
    return new Plan(ws, saved, throwAt, throwAt > 0 && r.nextBoolean(), r.nextBoolean(), throwAt == 0 && r.nextInt(3) == 0,
        steps);
  }

  // covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-07, RULE-SHELL-10
  @ParameterizedTest
  @EnumSource(OpampClient.Mode.class)
  void callbacksKeepTheContract(OpampClient.Mode mode, @TempDir Path dir) throws Exception {
    String runsEnv = System.getenv("OPAMP_MODEL_RUNS");
    String seedEnv = System.getenv("OPAMP_MODEL_SEED");
    boolean replay = seedEnv != null && !seedEnv.isEmpty();
    int runs = replay ? 1 : runsEnv == null || runsEnv.isEmpty() ? 30 : Integer.parseInt(runsEnv);
    SplittableRandom seeds = new SplittableRandom(System.nanoTime());
    try (Servers servers = new Servers(dir)) {
      for (int i = 0; i < runs; i++) {
        // Each run has a seed of its own, so one can be replayed alone.
        long seed = replay ? Long.parseLong(seedEnv) : seeds.nextLong();
        Plan plan = plan(new SplittableRandom(seed));
        try {
          new Run(seed, plan, servers, dir.resolve("run" + i), mode).run();
        } catch (Throwable e) {
          throw new AssertionError(mode + " mode, run " + i + " of " + runs + " (replay: OPAMP_MODEL_SEED=" + seed + "), " + plan
              + ": " + e.getMessage(), e);
        }
      }
    }
  }

  /**
   * S6, fixed: an {@code Error} from the callback (an AssertionError here) escaped the
   * client's catch, which took {@code RuntimeException} only: the rest of the step was skipped, and
   * the client neither stopped nor said why. The seeds that showed it.
   */
  // covers: RULE-SHELL-02
  @ParameterizedTest
  @EnumSource(OpampClient.Mode.class)
  void anErrorFromTheCallbackStopsTheClient(OpampClient.Mode mode, @TempDir Path dir) throws Exception {
    try (Servers servers = new Servers(dir)) {
      for (int i = 0; i < 2; i++) {
        long seed = 6_000 + i;
        // The first callback throws; the run waits for it, then stops.
        Plan plan = new Plan(i == 1, false, 1, true, false, false,
            List.of(new Step(Op.OFFER_CUSTOM, 0), new Step(Op.WAIT, 50)));
        new Run(seed, plan, servers, dir.resolve("s6-" + i), mode).run();
      }
    }
  }

  /**
   * N1, fixed: a callback that threw at the {@code stopped} of a stop was answered with a second
   * {@code stopped}, carrying that exception (OpampClient.feed caught it and called end(e), which
   * notified again). RULE-SHELL-01: nothing comes after {@code stopped}. The seeds that showed it.
   */
  // covers: RULE-SHELL-01, RULE-SHELL-02
  @ParameterizedTest
  @EnumSource(OpampClient.Mode.class)
  void aCallbackThatThrowsAtStoppedHearsNothingMore(OpampClient.Mode mode, @TempDir Path dir) throws Exception {
    try (Servers servers = new Servers(dir)) {
      for (int i = 0; i < 2; i++) {
        long seed = 1_000 + i;
        Plan plan = new Plan(i == 1, false, 0, false, false, true, List.of(new Step(Op.OFFER_CUSTOM, 0)));
        new Run(seed, plan, servers, dir.resolve("n1-" + i), mode).run();
      }
    }
  }

  /** The two local servers (HTTP and WebSocket), shared by the runs, answering for the current run. */
  static final class Servers implements AutoCloseable {
    final AtomicReference<ServerSide> current = new AtomicReference<>();
    final PropertiesTest.Server http;
    final PropertiesTest.Server ws;
    private final Path dir;
    private byte[] savedHttp;
    private byte[] savedWs;

    Servers(Path dir) throws Exception {
      this.dir = dir;
      Function<Opamp.AgentToServer, byte[]> reply = msg -> {
        ServerSide s = current.get();
        return s == null
            ? Opamp.ServerToAgent.newBuilder().setInstanceUid(msg.getInstanceUid()).build().toByteArray()
            : s.reply(msg);
      };
      http = new PropertiesTest.HttpOpamp(reply);
      ws = new PropertiesTest.WsServer(reply);
    }

    String endpoint(boolean overWs) {
      return overWs ? "ws://127.0.0.1:" + ws.port() + "/v1/opamp" : "http://127.0.0.1:" + http.port() + "/v1/opamp";
    }

    /**
     * A state file's contents holding the remote config {@value #SAVED}, for this endpoint (the
     * core ignores state saved for another): a client that was offered it, and stopped.
     */
    synchronized byte[] saved(boolean overWs) throws Exception {
      byte[] have = overWs ? savedWs : savedHttp;
      if (have != null) {
        return have;
      }
      ServerSide side = new ServerSide();
      side.offers.add(b -> b.setRemoteConfig(remoteConfig(SAVED)));
      current.set(side);
      Path file = dir.resolve("prep-" + (overWs ? "ws" : "http") + ".state");
      CompletableFuture<Void> got = new CompletableFuture<>();
      OpampClient c = OpampClient.start(config(endpoint(overWs), file), status(), n -> {
        if (n instanceof Notification.RemoteConfig) {
          got.complete(null);
        }
      });
      got.get(10, TimeUnit.SECONDS);
      c.stop().get(10, TimeUnit.SECONDS);
      current.set(null);
      have = Files.readAllBytes(file);
      assertTrue(have.length > 0, "the remote config was saved");
      if (overWs) {
        savedWs = have;
      } else {
        savedHttp = have;
      }
      return have;
    }

    @Override
    public void close() throws Exception {
      http.close();
      ws.close();
    }
  }

  /** The server's side of a run: a numbered custom message in every answer, and offers in turn. */
  static final class ServerSide {
    final AtomicLong tick = new AtomicLong();
    final ConcurrentLinkedQueue<Consumer<Opamp.ServerToAgent.Builder>> offers = new ConcurrentLinkedQueue<>();

    byte[] reply(Opamp.AgentToServer msg) {
      Opamp.ServerToAgent.Builder b = Opamp.ServerToAgent.newBuilder().setInstanceUid(msg.getInstanceUid());
      b.setCustomMessage(Opamp.CustomMessage.newBuilder().setCapability(CAPABILITY).setType("tick")
          .setData(ByteString.copyFromUtf8(Long.toString(tick.incrementAndGet()))));
      Consumer<Opamp.ServerToAgent.Builder> offer = offers.poll();
      if (offer != null) {
        offer.accept(b);
      }
      return b.build().toByteArray();
    }
  }

  static Opamp.AgentRemoteConfig remoteConfig(String hash) {
    return Opamp.AgentRemoteConfig.newBuilder()
        .setConfigHash(ByteString.copyFromUtf8(hash))
        .setConfig(Opamp.AgentConfigMap.newBuilder().putConfigMap("", Opamp.AgentConfigObject.newBuilder()
            .setBody(ByteString.copyFromUtf8("config " + hash)).setContentType("text/plain").build()))
        .build();
  }

  static OpampClient.Config config(String endpoint, Path stateFile) {
    return config(endpoint, stateFile, OpampClient.Mode.PLATFORM);
  }

  static OpampClient.Config config(String endpoint, Path stateFile, OpampClient.Mode mode) {
    return OpampClient.Config.builder(endpoint, UID)
        .mode(mode)
        // Core mode reads HTTP_PROXY and the like: the local servers are reached directly.
        .environment(java.util.Map.of())
        // The triggers are healths: a client that doesn't declare ReportsHealth sends none, and
        // each offer then waits for a heartbeat over HTTP.
        .capabilities(Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE
            | Opamp.AgentCapabilities.AgentCapabilities_ReportsHealth_VALUE)
        .heartbeatInterval(Duration.ofMillis(50))
        .requestTimeout(Duration.ofSeconds(2))
        .stateFile(stateFile)
        .build();
  }

  static OpampClient.Status status() {
    return new OpampClient.Status(null, health(0), null, null, null,
        Opamp.CustomCapabilities.newBuilder().addCapabilities(CAPABILITY).build());
  }

  static Opamp.ComponentHealth health(int n) {
    return Opamp.ComponentHealth.newBuilder().setHealthy(true).setStatus("s" + n).build();
  }

  /** One run: the client, the callback that records and checks what it sees, and the steps. */
  static final class Run {
    final long seed;
    final Plan plan;
    final Servers servers;
    final Path dir;
    final ServerSide side = new ServerSide();
    final AtomicReference<OpampClient> client = new AtomicReference<>();
    volatile boolean started;
    final AtomicInteger inProgress = new AtomicInteger();
    final AtomicInteger calls = new AtomicInteger();
    final List<String> violations = new CopyOnWriteArrayList<>();
    final List<Notification> seen = new CopyOnWriteArrayList<>();
    final AtomicInteger stoppedCount = new AtomicInteger();
    volatile Notification.Stopped firstStopped;
    volatile Throwable thrown;
    volatile Notification thrownOn;
    volatile long lastTick;
    volatile boolean savedSeen;
    final ConcurrentLinkedQueue<Consumer<OpampClient>> inside = new ConcurrentLinkedQueue<>();
    final AtomicInteger insideDone = new AtomicInteger();
    final List<CompletableFuture<Void>> stops = new CopyOnWriteArrayList<>();
    final AtomicInteger early = new AtomicInteger();
    /** A remote config handed over before the handle was there (S5): answered from outside. */
    final AtomicReference<Notification.RemoteConfig> unanswered = new AtomicReference<>();
    /**
     * The calls the callback made, and which callback (the k-th) is running: 0 on {@code stopped},
     * where a call does nothing.
     */
    final List<CallbackAnswerTest.Asked> asked = new CopyOnWriteArrayList<>();
    volatile int telling;
    /** The core's tap. */
    volatile String tap;
    int next = 1;

    final OpampClient.Mode mode;

    Run(long seed, Plan plan, Servers servers, Path dir, OpampClient.Mode mode) {
      this.mode = mode;
      this.seed = seed;
      this.plan = plan;
      this.servers = servers;
      this.dir = dir;
    }

    void violation(String what) {
      violations.add(what);
    }

    void notify(Notification n) {
      if (inProgress.incrementAndGet() > 1) {
        violation("RULE-SHELL-01: two callbacks at once (" + n + ")");
      }
      try {
        int k = calls.incrementAndGet();
        telling = n instanceof Notification.Stopped ? 0 : k;
        if (!started) {
          early.incrementAndGet();
        }
        if (firstStopped != null) {
          violation("RULE-SHELL-01: a callback after stopped: " + describe(n));
        }
        seen.add(n);
        if (n instanceof Notification.Stopped s) {
          stoppedCount.incrementAndGet();
          if (firstStopped == null) {
            firstStopped = s;
          }
        }
        if (n instanceof Notification.RemoteConfig rc && rc.config().getConfigHash().toStringUtf8().equals(SAVED)) {
          savedSeen = true;
        }
        if (n instanceof Notification.CustomMessage m && m.message().getType().equals("tick")) {
          long t = Long.parseLong(m.message().getData().toStringUtf8());
          if (t <= lastTick) {
            violation("RULE-SHELL-01: the server's custom message " + t + " after " + lastTick);
          }
          if (plan.saved() && !savedSeen && firstStopped == null) {
            violation("RULE-SHELL-01: the server's custom message " + t + " before the saved remote config");
          }
          lastTick = t;
        }
        OpampClient c = client.get();
        if (c == null && n instanceof Notification.RemoteConfig rc) {
          unanswered.set(rc);
        }
        if (c != null) { // null: S5, the handle isn't there yet
          if (n instanceof Notification.RemoteConfig rc) {
            answer(c, rc); // so the core hands over the next (a call from inside the callback)
            asked.add(CallbackAnswerTest.answered(k, rc.config().getConfigHash()));
          }
          Notification.RemoteConfig before = unanswered.getAndSet(null);
          if (before != null) {
            answer(c, before);
            asked.add(CallbackAnswerTest.answered(k, before.config().getConfigHash()));
          }
          for (Consumer<OpampClient> a; (a = inside.poll()) != null; ) {
            long t0 = System.nanoTime();
            a.accept(c);
            if (System.nanoTime() - t0 > 1_000_000_000L) {
              violation("RULE-SHELL-03: a call from the callback took over 1 s");
            }
            insideDone.incrementAndGet();
          }
        }
        if (thrown == null && (plan.throwAtStopped() ? n instanceof Notification.Stopped
            : k == plan.throwAt() && !(n instanceof Notification.Stopped))) {
          thrownOn = n;
          if (plan.throwError()) {
            AssertionError e = new AssertionError("the application's error, on callback " + k);
            thrown = e;
            throw e;
          }
          RuntimeException e = new IllegalStateException("the application's bug, on callback " + k);
          thrown = e;
          throw e;
        }
        if (thrown != null && plan.throwAgain() && n instanceof Notification.Stopped s && s.error() != null) {
          // Fails again on the stopped that reports the first: dropped, the first is the error.
          throw new IllegalStateException("the application's second bug, on callback " + k);
        }
      } finally {
        inProgress.decrementAndGet();
      }
    }

    static void answer(OpampClient c, Notification.RemoteConfig rc) {
      if (rc != null) {
        c.setRemoteConfigStatus(Opamp.RemoteConfigStatus.newBuilder()
            .setLastRemoteConfigHash(rc.config().getConfigHash())
            .setStatus(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED).build());
      }
    }

    static String describe(Notification n) {
      return n instanceof Notification.Stopped s ? "Stopped(" + s.error() + ")" : n.getClass().getSimpleName();
    }

    boolean ended() {
      return firstStopped != null;
    }

    void run() throws Exception {
      Files.createDirectories(dir);
      Path stateFile = dir.resolve("state");
      if (plan.saved()) {
        Files.write(stateFile, servers.saved(plan.ws()));
      }
      servers.current.set(side);
      try {
        OpampClient c = OpampClient.start(config(servers.endpoint(plan.ws()), stateFile, mode), status(), this::notify,
            null, dir.resolve("core.tap").toString(), d -> d);
        tap = c.tapPath();
        client.set(c);
        started = true;
        check("start");
        for (Step s : plan.steps()) {
          step(c, s);
          check(s.toString());
        }
        step(c, new Step(Op.STOP_OUTSIDE, 0));
        check("the final stop");
        finish(c);
      } finally {
        servers.current.set(null);
        if (early.get() > 0) {
          System.out.println("CallbackPropertiesTest: tolerated S5: " + early.get()
              + " callback(s) before start returned (seed " + seed + ")");
        }
      }
    }

    /** A status change: the core sends it, and the server's answer calls back. */
    void trigger(OpampClient c) {
      answer(c, unanswered.getAndSet(null));
      c.setHealth(health(next++));
    }

    void step(OpampClient c, Step s) throws Exception {
      switch (s.op()) {
        case CALL_OUTSIDE -> {
          long t0 = System.nanoTime();
          trigger(c);
          if (System.nanoTime() - t0 > 1_000_000_000L) {
            violation("RULE-SHELL-03: a call took over 1 s");
          }
        }
        case CALL_THREAD -> {
          int n = next++;
          Thread t = new Thread(() -> {
            c.setDescription(Opamp.AgentDescription.getDefaultInstance());
            c.setHealth(health(n));
          });
          t.setDaemon(true);
          t.start();
          t.join(5_000);
          if (t.isAlive()) {
            violation("RULE-SHELL-03: a call from another thread didn't return");
          }
        }
        case CALL_INSIDE -> {
          int want = insideDone.get() + 1;
          int n = next++;
          inside.add(cl -> {
            cl.setHealth(health(n));
            if (telling > 0) {
              asked.add(CallbackAnswerTest.health(telling, "s" + n));
            }
          });
          trigger(c);
          waitFor(() -> insideDone.get() >= want || ended(), STEP_WAIT_MS);
        }
        case OFFER_CONFIG -> {
          String hash = "cfg-" + next;
          side.offers.add(b -> b.setRemoteConfig(remoteConfig(hash)));
          trigger(c);
          waitFor(() -> ended() || seen.stream().anyMatch(n -> n instanceof Notification.RemoteConfig rc
              && rc.config().getConfigHash().toStringUtf8().equals(hash)), STEP_WAIT_MS);
        }
        case OFFER_CUSTOM -> {
          long before = lastTick;
          trigger(c);
          waitFor(() -> lastTick > before || ended(), STEP_WAIT_MS);
        }
        case OFFER_HEARTBEAT -> {
          // 1 s, then 0: over HTTP back to the configured interval, over WebSocket heartbeats off.
          long before = seen.stream().filter(n -> n instanceof Notification.HeartbeatInterval).count();
          side.offers.add(b -> b.setConnectionSettings(Opamp.ConnectionSettingsOffers.newBuilder()
              .setOpamp(Opamp.OpAMPConnectionSettings.newBuilder().setHeartbeatIntervalSeconds(1))));
          side.offers.add(b -> b.setConnectionSettings(Opamp.ConnectionSettingsOffers.newBuilder()
              .setOpamp(Opamp.OpAMPConnectionSettings.newBuilder().setHeartbeatIntervalSeconds(0))));
          trigger(c);
          waitFor(() -> ended() || seen.stream().filter(n -> n instanceof Notification.HeartbeatInterval).count() > before,
              STEP_WAIT_MS);
          trigger(c);
          waitFor(() -> ended() || side.offers.isEmpty(), STEP_WAIT_MS);
        }
        case WAIT -> Thread.sleep(s.arg());
        case STOP_OUTSIDE -> {
          CompletableFuture<Void> f = c.stop();
          stops.add(f);
          awaitStop(f, "stop from outside");
        }
        case STOP_INSIDE -> {
          int want = insideDone.get() + 1;
          inside.add(cl -> {
            // Requested, not waited for: the client is waiting for this callback.
            stops.add(cl.stop());
          });
          trigger(c);
          waitFor(() -> insideDone.get() >= want || ended(), STEP_WAIT_MS);
          for (CompletableFuture<Void> f : stops) {
            awaitStop(f, "stop requested from the callback");
          }
        }
      }
    }

    void awaitStop(CompletableFuture<Void> f, String what) throws InterruptedException {
      try {
        f.get(STOP_BOUND_MS, TimeUnit.MILLISECONDS);
      } catch (TimeoutException e) {
        violation("RULE-SHELL-07: " + what + " didn't return within " + STOP_BOUND_MS + " ms");
      } catch (ExecutionException e) {
        // its error: checked at the end, against the model
      }
    }

    /** After each step: what the callback recorded so far. */
    void check(String after) {
      if (!violations.isEmpty()) {
        throw new AssertionError("after " + after + ": " + violations + "; notifications: " + names());
      }
    }

    List<String> names() {
      return seen.stream().map(Run::describe).toList();
    }

    void finish(OpampClient c) throws Exception {
      for (CompletableFuture<Void> f : stops) {
        awaitStop(f, "stop");
      }
      Thread.sleep(150); // anything late would show now
      // The error the model expects: what the callback threw, unless it threw at stopped itself.
      boolean onStopped = thrownOn instanceof Notification.Stopped;
      Throwable expected = onStopped ? null : thrown;
      if (stoppedCount.get() != 1) {
        violation("RULE-SHELL-02: stopped " + stoppedCount.get() + " times");
      } else if (!(seen.get(seen.size() - 1) instanceof Notification.Stopped)) {
        violation("RULE-SHELL-01: stopped isn't last");
      }
      if (firstStopped != null && firstStopped.error() != expected) {
        violation("RULE-SHELL-02: stopped with " + firstStopped.error() + ", the model expects " + expected);
      }
      // stop, before or after, every time: that error (thrown at stopped itself: either).
      List<CompletableFuture<Void>> all = new ArrayList<>(stops);
      all.add(c.stop());
      for (CompletableFuture<Void> f : all) {
        Throwable got;
        try {
          f.get(STOP_BOUND_MS, TimeUnit.MILLISECONDS);
          got = null;
        } catch (ExecutionException e) {
          got = e.getCause();
        } catch (TimeoutException e) {
          violation("RULE-SHELL-07: stop didn't return within " + STOP_BOUND_MS + " ms");
          continue;
        }
        if (got != expected && !(onStopped && got == thrown)) {
          violation("RULE-SHELL-02: stop gave " + got + ", the model expects " + expected);
        }
      }
      // Calls after the client stopped do nothing.
      try {
        trigger(c);
        c.setDescription(Opamp.AgentDescription.getDefaultInstance());
      } catch (RuntimeException e) {
        violation("RULE-SHELL-03: a call after stop threw " + e);
      }
      Thread.sleep(20);
      if (stoppedCount.get() != 1 || !(seen.get(seen.size() - 1) instanceof Notification.Stopped)) {
        violation("RULE-SHELL-01: a callback after stopped (a call after stop)");
      }
      // RULE-SHELL-10, in the core's tap (written out by the time stop returned): what the
      // callback asked reached the core before anything the client took in next.
      try {
        String wrong = CallbackAnswerTest.check(Path.of(tap), asked, true);
        if (wrong != null) {
          violation(wrong);
        }
      } catch (Exception e) {
        violation("the core's tap: " + e);
      }
      check("the end (threw " + thrown + " on " + (thrownOn == null ? "nothing" : describe(thrownOn)) + ")");
    }
  }

  private static void waitFor(BooleanSupplier done, long ms) throws InterruptedException {
    for (long end = System.nanoTime() + ms * 1_000_000L; !done.getAsBoolean() && System.nanoTime() < end; ) {
      Thread.sleep(2);
    }
  }
}
