// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.fail;

import com.dylibso.chicory.compiler.MachineFactoryCompiler;
import com.dylibso.chicory.wasm.WasmModule;
import java.io.ByteArrayOutputStream;
import java.io.DataInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.net.SocketTimeoutException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.Base64;
import java.util.HashMap;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import java.util.TreeSet;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicReference;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.EnumSource;

/**
 * The adversary (testdata/core/opamp_core_adversary.wasm): a module with the core's exports and
 * ABI that plays a hostile core, run in place of the core through {@link
 * OpampClient#start(OpampClient.Config, OpampClient.Status, java.util.function.Consumer, WasmModule)},
 * against the local server it expects ({@link AdversaryServer}). Each seed is one client; the
 * harness is the one every SDK runs: it fails a seed on a violation the adversary reports, a missing or second verdict, a
 * verdict with violations, the client stopping with an error, stop not returning within 15 s, two
 * callbacks at once or one after stopped, notes out of order, a WebSocket still open or a request
 * after stop returned, a download that changed after stop, or a state file that doesn't hold what
 * was last answered stored; and the batch on a production enabled but never used.
 *
 * <p>Each test runs in both modes. In Rust mode the client tells the core it speaks streams, and
 * each run has a raw server of its own ({@link AdversaryRawTest.Server}, the adversary's raw
 * server; {@code raw=PORT} and {@code tls=NAME} in the endpoint's query): the adversary hands the
 * client the stream effects ({@link Streams} carries them out, to that server), and the platform's
 * as well (http_request, ws_open, download), which Rust mode passes to the platform executor: which
 * of the two takes an effect (a cancel above all), Rust mode's stop and its shutdown run with both
 * under way. The run then also fails on a stream's connection still open, or one made after stop
 * returned.
 *
 * <p>OPAMP_ADVERSARY_SEEDS (default 24) seeds per mode from OPAMP_ADVERSARY_BASE (default 1);
 * OPAMP_ADVERSARY_SEED replays one; OPAMP_ADVERSARY_WASM is the module (default
 * testdata/core/opamp_core_adversary.wasm in the repository).
 *
 * <p>Known breaks: {@link #KNOWN} (productions the main run passes in {@code off}, each run in
 * {@code focus} by {@link #knownBugsStillFail}): none now; the ones fixed are on {@link #FIXED},
 * replayed by {@link #fixedBugsStayFixed}. S5, a callback before start returns (the notes
 * of {@code notify_start}), is still counted and logged with the seed if it happens; the client
 * holds its loop until start has returned, so only the caller's own store of the handle can lag.
 */
class AdversaryPropertiesTest {
  /**
   * Productions that trigger a bug this SDK is known to have: the bug, and why. S-numbers are
   * spec/shell-contract.md's; N-numbers are new (found by this harness), still to fix.
   */
  static final Map<String, String> KNOWN = new TreeMap<>(Map.of());

  /**
   * Bugs this SDK had, fixed, each with the focus seeds that showed it (as {@link
   * #knownBugsStillFail} ran them, with the other entries of the list then in {@code off}): they
   * must pass now.
   */
  static final Map<String, List<Long>> FIXED = new TreeMap<>(Map.of(
      // S1 (a WebSocket that opened after stop stayed open), S2 (an upgrade held past its timeout
      // opened) and S3 (a download outlived stop) were replayed here by stop_while_opening,
      // ws_hold_past_timeout and stop_while_downloading: the adversary no longer has those (they
      // were a shell's duties without cancels); stop_cancel and the cancel productions, in
      // cancelsAreHonoured and the main run, hold the client to them now.
      // N3: a timeout_ms of 0 threw out of Executor.post and stopped the client.
      "timeout_zero", List.of(6001L, 6002L, 6003L),
      // N4: a timeout_ms of 2^63 or more was a negative Duration: Executor.post threw.
      "timeout_huge", List.of(5001L, 5002L, 5003L),
      // N5: a header the JDK refuses threw out of Executor.post; seed 1002 then found a connection
      // reported lost after the server's Close had ended it (RULE-SHELL-04, twice).
      "headers_odd", List.of(1001L, 1002L, 1003L),
      // N6: a limit of 2^63 or more was negative: every response and download failed as too large.
      "limit_huge", List.of(2001L, 2002L, 2003L),
      // N7: a wake_at_ms of 2^63 or more was negative: the timer fired at once.
      "wake_max", List.of(7001L, 7002L, 7003L),
      // N8 (Rust mode): a splice asked for in answer to stream_spliced didn't take the bytes
      // already read: they were handed over as stream_data (splice_leak). Seeds 25 and 26 were
      // the main run's focus on it, the production having failed in every seed of the batch.
      "stream_splice_chain", List.of(25L, 26L)));

  private static final int PARALLEL = 4;
  /** Seeds per known bug in {@link #knownBugsStillFail}. */
  private static final int FOCUS_SEEDS = 3;
  private static final long STOP_BOUND_MS = 15_000;
  private static final long AFTER_STOP_MS = 3_000;
  /**
   * How long after the harness's own marker a request or upgrade the client had already sent may
   * arrive. The marker ({@link Marker}) is a request the harness sends the moment stop returns, on
   * a connection of its own: it waits for the server's accept and read as whatever the client sent
   * before then does, so a server late to both is late to the marker too. Measured from the moment
   * stop returned, as it was, the 50 ms called that lateness the client's: on loaded CI runners
   * Node's server has taken 171 ms and 348 ms to get to a request already on its way.
   */
  private static final long ARRIVAL_GRACE_MS = 50;
  /** The marker is answered within this of being sent: a server that never answers it fails the seed, and doesn't hang it. */
  private static final int MARKER_BOUND_MS = 30_000;
  /** A seed's whole run, start to stop: generous (a lingering stop alone may take 30 s). */
  private static final long RUN_DEADLINE_MS = 90_000;
  private static final String PREFIX = "opamp-adversary ";
  /** How late (ms) the adversary lets a timer or an answer be: see {@link Seed#check}. */
  private static final long MARGIN_MS = 10_000;

  private static volatile WasmModule module;

  /** What the raw servers start TLS with (tls=1): the tests' server certificate, made once. */
  private static final class RawTls {
    static final javax.net.ssl.SSLContext CONTEXT = AdversaryRawTest.certificate();
  }

  // covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-04, RULE-SHELL-05, RULE-SHELL-07, RULE-SHELL-08, RULE-SHELL-09, RULE-SHELL-10
  @ParameterizedTest
  @EnumSource(OpampClient.Mode.class)
  void theAdversaryFindsNothing(OpampClient.Mode mode, @TempDir Path dir) throws Exception {
    String one = System.getenv("OPAMP_ADVERSARY_SEED");
    List<Long> seeds = new ArrayList<>();
    if (one != null && !one.isEmpty()) {
      seeds.add(Long.parseLong(one));
    } else {
      long base = longEnv("OPAMP_ADVERSARY_BASE", 1);
      long count = longEnv("OPAMP_ADVERSARY_SEEDS", 24);
      for (long s = base; s < base + count; s++) {
        seeds.add(s);
      }
    }
    String off = String.join(",", KNOWN.keySet());
    List<Result> results = batch(mode, dir, seeds, s -> "off=" + off);
    List<String> failures = new ArrayList<>();
    for (Result r : results) {
      if (!r.failures.isEmpty()) {
        failures.add(r.describe());
      }
    }
    // Every production enabled somewhere must have run somewhere (not for a replay of one seed).
    if (seeds.size() > 1) {
      Set<String> enabled = new TreeSet<>();
      Set<String> used = new TreeSet<>();
      for (Result r : results) {
        enabled.addAll(r.enabled);
        used.addAll(r.used);
      }
      enabled.removeAll(used);
      if (!enabled.isEmpty()) {
        // Drawn at random, a production can miss a batch by chance (a stop production ends each
        // run, so each is drawn by about one run in eight): run each missed one in focus, on seeds
        // past the batch's, before calling it one that can't run.
        long next = seeds.get(seeds.size() - 1) + 1;
        Map<Long, String> focus = new TreeMap<>();
        for (String prod : enabled) {
          for (int j = 0; j < 2; j++) {
            focus.put(next++, "focus=" + prod + (off.isEmpty() ? "" : "&off=" + off));
          }
        }
        for (Result r : batch(mode, dir.resolve("missed"), new ArrayList<>(focus.keySet()), focus::get)) {
          enabled.removeAll(r.used);
          if (!r.failures.isEmpty()) {
            failures.add(r.describe());
          }
        }
      }
      if (!enabled.isEmpty()) {
        failures.add("productions enabled but never used in the batch (they test nothing; more seeds, or a"
            + " production that can't run): " + enabled);
      }
    }
    if (!failures.isEmpty()) {
      fail(failures.size() + " of " + seeds.size() + " seeds failed:\n" + String.join("\n", failures));
    }
  }

  /**
   * Each known bug, in {@code focus} over a few seeds: it must still make a run fail. When none
   * does, the bug looks fixed: take its entry off {@link #KNOWN}.
   */
  // covers: RULE-SHELL-07
  @ParameterizedTest
  @EnumSource(OpampClient.Mode.class)
  void knownBugsStillFail(OpampClient.Mode mode, @TempDir Path dir) throws Exception {
    // FOCUS_SEEDS seeds per bug, all in one batch: bug i has seeds 1000 (i + 1) + j.
    List<String> prods = new ArrayList<>(KNOWN.keySet());
    List<Long> seeds = new ArrayList<>();
    for (int i = 0; i < prods.size(); i++) {
      for (int j = 1; j <= FOCUS_SEEDS; j++) {
        seeds.add(1000L * (i + 1) + j);
      }
    }
    java.util.function.LongFunction<String> prodOf = s -> prods.get((int) (s / 1000) - 1);
    List<Result> results = batch(mode, dir, seeds, s -> {
      String prod = prodOf.apply(s);
      List<String> others = new ArrayList<>(prods);
      others.remove(prod);
      return "focus=" + prod + (others.isEmpty() ? "" : "&off=" + String.join(",", others));
    });
    List<String> fixed = new ArrayList<>();
    for (String prod : prods) {
      boolean failed = false;
      List<String> drawn = new ArrayList<>();
      for (Result r : results) {
        if (prodOf.apply(r.seed).equals(prod)) {
          drawn.add(r.seed + (r.used.contains(prod) ? " drew it" : " didn't draw it") + (r.failures.isEmpty() ? ", passed" : ", failed"));
        }
        // A failure counts if the run drew the production (or never got to say what it drew).
        if (prodOf.apply(r.seed).equals(prod) && !r.failures.isEmpty() && (r.used.contains(prod) || r.verdicts == 0)) {
          failed = true;
          System.out.println("AdversaryPropertiesTest: " + prod + " still fails, as expected (" + KNOWN.get(prod)
              + "): " + r.describe());
        }
      }
      if (!failed) {
        fixed.add(prod + " (" + KNOWN.get(prod) + "): no seed in focus failed with it: " + drawn);
      }
    }
    if (!fixed.isEmpty()) {
      fail("known bugs that look fixed: take them off AdversaryPropertiesTest.KNOWN:\n" + String.join("\n", fixed));
    }
  }

  /** The bugs on {@link #FIXED}, each in focus with the seeds that showed it: they pass now. */
  // covers: RULE-SHELL-04, RULE-SHELL-05, RULE-SHELL-07, RULE-SHELL-08
  @ParameterizedTest
  @EnumSource(OpampClient.Mode.class)
  void fixedBugsStayFixed(OpampClient.Mode mode, @TempDir Path dir) throws Exception {
    Map<Long, String> queries = new TreeMap<>();
    for (Map.Entry<String, List<Long>> e : FIXED.entrySet()) {
      List<String> others = new ArrayList<>(FIXED.keySet());
      others.remove(e.getKey());
      for (long seed : e.getValue()) {
        queries.put(seed, "focus=" + e.getKey() + "&off=" + String.join(",", others));
      }
    }
    List<Result> results = batch(mode, dir, new ArrayList<>(queries.keySet()), queries::get);
    List<String> failures = results.stream().filter(r -> !r.failures.isEmpty()).map(Result::describe).toList();
    if (!failures.isEmpty()) {
      fail(String.join("\n", failures));
    }
  }

  /** The cancel productions (RULE-SHELL-06), in every run's grammar. */
  static final List<String> CANCELS = List.of("cancel_download", "cancel_http", "cancel_http_done", "cancel_store",
      "cancel_twice", "cancel_ws_closing", "cancel_ws_opening", "stop_cancel");
  /** And those of the streams and their files, in a Rust mode run's. */
  static final List<String> STREAM_CANCELS = List.of("cancel_file", "cancel_splice", "cancel_stream_open",
      "cancel_stream_opening", "cancel_stream_resolving");

  /** The cancel productions of a run in {@code mode}. */
  static List<String> cancels(OpampClient.Mode mode) {
    List<String> all = new ArrayList<>(CANCELS);
    if (mode == OpampClient.Mode.RUST) {
      all.addAll(STREAM_CANCELS);
    }
    return all;
  }

  /** RULE-SHELL-06, the cancels: every cancel production is enabled, and each runs in focus. */
  // covers: RULE-SHELL-06
  @ParameterizedTest
  @EnumSource(OpampClient.Mode.class)
  void cancelsAreHonoured(OpampClient.Mode mode, @TempDir Path dir) throws Exception {
    List<String> cancels = cancels(mode);
    List<Result> probe = batch(mode, dir.resolve("probe"), List.of(1L), s -> "steps=5");
    List<String> missing = cancels.stream().filter(p -> !probe.get(0).enabled.contains(p)).toList();
    if (!missing.isEmpty()) {
      fail("cancel productions not enabled: " + missing + " (enabled: " + probe.get(0).enabled + ")");
    }
    // Two seeds each (11 and 12 for the first, 21 and 22 for the next, ...), in focus.
    Map<Long, String> queries = new TreeMap<>();
    for (int i = 0; i < cancels.size(); i++) {
      for (long k = 1; k <= 2; k++) {
        queries.put(10L * (i + 1) + k, "focus=" + cancels.get(i) + "&off=" + String.join(",", KNOWN.keySet()));
      }
    }
    List<Result> results = batch(mode, dir, new ArrayList<>(queries.keySet()), queries::get);
    List<String> failures = results.stream().filter(r -> !r.failures.isEmpty()).map(Result::describe).toList();
    if (!failures.isEmpty()) {
      fail(String.join("\n", failures));
    }
  }

  /**
   * The server's own measure (seed 3 of cancelsAreHonoured failed on it, 326 ms late): a request
   * arrives when its line and headers have reached the server, however long the server then takes
   * to parse them; a head that comes in two parts arrives with the second.
   */
  // covers: RULE-SHELL-07
  @Test
  void anArrivalIsWhenItsHeadCame() throws Exception {
    try (AdversaryServer server = new AdversaryServer()) {
      server.afterFirstRead = () -> pause(2_000); // a loaded host, between the bytes and the parse
      long sent = request(server, "GET /d?run=whole HTTP/1.1\r\nHost: x\r\n\r\n", null);
      long late = (server.lastArrival("whole").at() - sent) / 1_000_000;
      if (late >= 1_000) {
        fail("a head sent whole counted as arriving " + late + " ms after it was sent: when the server parsed it");
      }
      server.afterFirstRead = () -> {};
      long second = request(server, "GET /d?run=split HTTP/1.1\r\n", "Host: x\r\n\r\n");
      if (server.lastArrival("split").at() < second) {
        fail("a head sent in two parts counted as arriving before its second part was sent");
      }
    }
  }

  /**
   * The harness's own measure of what arrived after stop (Node's seed 5 failed twice on loaded CI
   * runners, 171 ms and 348 ms "after stop returned", and passes elsewhere): a request sent before
   * stop returned isn't the client's to answer for however late the server takes it, and one sent
   * after stop returned still is.
   */
  // covers: RULE-SHELL-07
  @Test
  void anArrivalIsJudgedAgainstTheMarker() throws Exception {
    try (AdversaryServer server = new AdversaryServer()) {
      // Sent before stop returned, to a server 400 ms late to accept anything: by the moment stop
      // returned it arrived late, by the marker it didn't.
      server.lateMs = 400;
      try (Socket before = new Socket(InetAddress.getByName("127.0.0.1"), server.port())) {
        before.getOutputStream().write("GET /d?run=late HTTP/1.1\r\nHost: x\r\n\r\n".getBytes(StandardCharsets.US_ASCII));
        before.getOutputStream().flush();
        long stoppedAt = System.nanoTime();
        String error = new Marker(server.port(), "late").answered();
        if (error != null) {
          fail("the marker: " + error);
        }
        before.setSoTimeout(10_000);
        if (before.getInputStream().read() < 0) {
          fail("no answer");
        }
        long late = (server.lastArrival("late").at() - stoppedAt) / 1_000_000;
        if (late <= ARRIVAL_GRACE_MS) {
          fail("the server was to be late: it took the request " + late + " ms after stop returned");
        }
        String said = server.afterStop("late", stoppedAt);
        System.out.println("AdversaryPropertiesTest: a request sent before stop returned, taken " + late
            + " ms after it: " + (said == null ? "passes" : said));
        if (said != null) {
          fail("a request sent before stop returned counted as arriving after it: " + said);
        }
      }

      // Sent after stop returned: 100 ms after the marker sent then was answered.
      server.lateMs = 0;
      long stoppedAt = System.nanoTime();
      String error = new Marker(server.port(), "after").answered();
      if (error != null) {
        fail("the marker: " + error);
      }
      pause(100);
      request(server, "GET /d?run=after HTTP/1.1\r\nHost: x\r\n\r\n", null);
      String said = server.afterStop("after", stoppedAt);
      System.out.println("AdversaryPropertiesTest: a request sent 100 ms after stop returned: " + (said == null ? "passes" : said));
      if (said == null || !said.contains("RULE-SHELL-07")) {
        fail("a request sent 100 ms after stop returned passed");
      }
    }
  }

  /**
   * The harness's marker for a run: {@code GET /marker}, sent on a connection of its own as this is
   * made (the request is written before the constructor returns).
   */
  private static final class Marker {
    private Socket socket;
    private String error;

    Marker(int port, String run) {
      try {
        socket = new Socket(InetAddress.getByName("127.0.0.1"), port);
        socket.getOutputStream().write(("GET /marker?run=" + run + " HTTP/1.1\r\nHost: x\r\n\r\n").getBytes(StandardCharsets.US_ASCII));
        socket.getOutputStream().flush();
      } catch (IOException e) {
        error = e.toString();
      }
    }

    /** Wait for the answer: what went wrong, or null if it was 204. */
    String answered() {
      if (socket == null) {
        return error;
      }
      try (Socket s = socket) {
        if (error != null) {
          return error;
        }
        s.setSoTimeout(MARKER_BOUND_MS);
        ByteArrayOutputStream status = new ByteArrayOutputStream();
        InputStream in = s.getInputStream();
        for (int c = in.read(); c >= 0 && c != '\n'; c = in.read()) {
          status.write(c);
        }
        String line = status.toString(StandardCharsets.US_ASCII).trim();
        return line.startsWith("HTTP/1.1 204 ") ? null : "answered " + (line.isEmpty() ? "nothing" : line);
      } catch (SocketTimeoutException e) {
        return "not answered within " + MARKER_BOUND_MS + " ms";
      } catch (IOException e) {
        return e.toString();
      }
    }
  }

  /** Send a request's head (in two writes 300 ms apart if {@code rest}), read the answer: when the last part went. */
  private static long request(AdversaryServer server, String head, String rest) throws IOException {
    try (Socket c = new Socket(InetAddress.getByName("127.0.0.1"), server.port())) {
      c.setSoTimeout(10_000);
      OutputStream out = c.getOutputStream();
      long sent = System.nanoTime();
      out.write(head.getBytes(StandardCharsets.US_ASCII));
      out.flush();
      if (rest != null) {
        pause(300);
        sent = System.nanoTime();
        out.write(rest.getBytes(StandardCharsets.US_ASCII));
        out.flush();
      }
      if (c.getInputStream().read() < 0) {
        fail("no answer");
      }
      return sent;
    }
  }

  private static void pause(long ms) {
    try {
      Thread.sleep(ms);
    } catch (InterruptedException e) {
      Thread.currentThread().interrupt();
    }
  }

  private static long longEnv(String name, long dflt) {
    String v = System.getenv(name);
    return v == null || v.isEmpty() ? dflt : Long.parseLong(v);
  }

  /** The module: OPAMP_ADVERSARY_WASM, or the repository's (found walking up from here). */
  static WasmModule module() throws IOException {
    WasmModule m = module;
    if (m != null) {
      return m;
    }
    synchronized (AdversaryPropertiesTest.class) {
      if (module != null) {
        return module;
      }
      String env = System.getenv("OPAMP_ADVERSARY_WASM");
      Path wasm = null;
      if (env != null && !env.isEmpty()) {
        wasm = Path.of(env);
      } else {
        for (Path d = Path.of("").toAbsolutePath(); d != null; d = d.getParent()) {
          if (Files.isDirectory(d.resolve("testdata/core"))) {
            wasm = d.resolve("testdata/core/opamp_core_adversary.wasm");
            break;
          }
        }
      }
      if (wasm == null || !Files.isRegularFile(wasm)) {
        fail("the adversary module isn't there (" + (wasm == null ? "no testdata/core above "
            + Path.of("").toAbsolutePath() : wasm) + "): fetch it with scripts/fetch_core.py, or set OPAMP_ADVERSARY_WASM");
      }
      WasmModule parsed = Core.parse(Files.readAllBytes(wasm));
      // Compiled, as the embedded core is: interpreted, a step that hashes a few MB of spliced
      // file took seconds, and the shell's loop, which hands every stream's reads on, waited for
      // it (a splice made no progress for 15 s: RULE-SHELL-05 never_answered).
      Core.compiled(parsed, MachineFactoryCompiler.compile(parsed));
      warmUp(parsed);
      module = parsed;
      return module;
    }
  }

  /**
   * Run the module alone for a while, so that the first seeds don't run on a cold JVM:
   * there a step can take long enough to make a timer late by more than the adversary's margin.
   */
  private static void warmUp(WasmModule m) {
    byte[] uid = new byte[16];
    uid[0] = 1;
    Core core = new Core(m, dev.pydantic.opamp.core.CoreProto.Config.newBuilder()
        .setEndpoint("http://127.0.0.1:1/opamp?seed=0&steps=100000")
        .setInstanceUid(com.google.protobuf.ByteString.copyFrom(uid))
        .build());
    var timer = dev.pydantic.opamp.core.CoreProto.Input.newBuilder()
        .setTimer(dev.pydantic.opamp.core.CoreProto.Empty.getDefaultInstance()).build();
    try {
      core.step(0, 0, dev.pydantic.opamp.core.CoreProto.Input.newBuilder()
          .setStart(dev.pydantic.opamp.core.CoreProto.Empty.getDefaultInstance()).build());
      for (long end = System.nanoTime() + 3_000_000_000L, now = 1; System.nanoTime() < end; now += 200) {
        core.step(now, 0, timer);
      }
    } catch (RuntimeException e) {
      // only warming up
    }
  }

  /** The seeds, {@link #PARALLEL} at once, each with its query's extra parameters. */
  private static List<Result> batch(OpampClient.Mode mode, Path dir, List<Long> seeds,
      java.util.function.LongFunction<String> query)
      throws Exception {
    WasmModule m = module();
    Files.createDirectories(dir);
    ExecutorService pool = Executors.newFixedThreadPool(PARALLEL);
    try (AdversaryServer server = new AdversaryServer()) {
      List<Future<Result>> futures = new ArrayList<>();
      for (long seed : seeds) {
        futures.add(pool.submit(() -> new Seed(seed, query.apply(seed), server, m, dir.resolve("seed-" + seed), mode).run()));
      }
      List<Result> results = new ArrayList<>();
      for (Future<Result> f : futures) {
        results.add(f.get());
      }
      return results;
    } finally {
      pool.shutdownNow();
    }
  }

  /** What one seed's run found. */
  static final class Result {
    final long seed;
    final String query;
    final List<String> failures = new CopyOnWriteArrayList<>();
    final Set<String> used = new TreeSet<>();
    final Set<String> enabled = new TreeSet<>();
    int verdicts;

    Result(long seed, String query) {
      this.seed = seed;
      this.query = query;
    }

    String describe() {
      return "seed " + seed + " (" + query + "; replay: OPAMP_ADVERSARY_SEED=" + seed + "): " + failures;
    }
  }

  /** One seed: a client on the adversary, the harness's callback, and the checks. */
  static final class Seed {
    final long seed;
    final String query;
    final AdversaryServer server;
    final WasmModule module;
    final Path dir;
    final Result result;
    final AtomicReference<OpampClient> client = new AtomicReference<>();
    volatile boolean started;
    final AtomicInteger early = new AtomicInteger();
    final List<String> tolerated = new CopyOnWriteArrayList<>();
    final AtomicInteger inProgress = new AtomicInteger();
    final AtomicLong lastNote = new AtomicLong();
    final List<String> verdicts = new CopyOnWriteArrayList<>();
    final List<Notification.Stopped> stopped = new CopyOnWriteArrayList<>();
    /** Every stop: when it was asked for, and its future. */
    final List<Map.Entry<Long, CompletableFuture<Void>>> stops = new CopyOnWriteArrayList<>();

    final OpampClient.Mode mode;
    /** The run's raw server, which its streams connect to (Rust mode; null in Java mode). */
    AdversaryRawTest.Server raw;

    Seed(long seed, String query, AdversaryServer server, WasmModule module, Path dir, OpampClient.Mode mode) {
      this.mode = mode;
      this.seed = seed;
      this.query = query;
      this.server = server;
      this.module = module;
      this.dir = dir;
      this.result = new Result(seed, query);
    }

    void failure(String what) {
      if (!result.failures.contains(what)) {
        result.failures.add(what);
      }
    }

    void notify(Notification n) {
      if (inProgress.incrementAndGet() > 1) {
        failure("RULE-SHELL-01: two callbacks at once");
      }
      try {
        if (!started) {
          early.incrementAndGet();
        }
        if (!stopped.isEmpty()) {
          failure("RULE-SHELL-01: a callback after stopped: " + brief(n));
        }
        if (n instanceof Notification.Stopped s) {
          stopped.add(s);
          return;
        }
        if (!(n instanceof Notification.BadMessage b) || !b.detail().startsWith(PREFIX)) {
          return;
        }
        String text = b.detail().substring(PREFIX.length());
        OpampClient c = client.get();
        if (text.startsWith("note ")) {
          long note = Long.parseLong(text.substring(5).trim());
          long last = lastNote.getAndSet(note);
          if (note != last + 1) {
            failure("RULE-SHELL-01: note " + note + " after note " + last);
          }
        } else if (text.startsWith("violation RULE-SHELL-08 late_timer")) {
          // Tolerated: the rule is "never early"; lateness is the adversary's sign of a lost timer,
          // which would stall the run (no stop asked: a failure). Here a timer comes late when the
          // loop is busy: S4, saves (fsync) on the loop, and the adversary's slow steps on a
          // loaded machine.
          tolerated.add(text);
        } else if (text.startsWith("violation ")) {
          failure(b.detail());
        } else if (text.startsWith("verdict ")) {
          verdicts.add(text);
        } else if (text.equals("call inside")) {
          // "inside": the adversary checks this call reaches it before anything the client takes in
          // next (RULE-SHELL-10). With no client stored yet there is nothing to call: the adversary
          // owes itself no call until one from this application has shown it holds its client.
          if (c != null) {
            c.setHealth(Opamp.ComponentHealth.newBuilder().setHealthy(true).setStatus("inside").build());
          } else {
            early.incrementAndGet();
          }
        } else if (text.equals("call outside")) {
          Thread t = new Thread(() -> {
            OpampClient cl = awaitClient();
            if (cl != null) {
              cl.setHealth(Opamp.ComponentHealth.newBuilder().setHealthy(true).setStatus("outside").build());
            }
          }, "adversary-call-" + seed);
          t.setDaemon(true);
          t.start();
        } else if (text.equals("stop inside")) {
          if (c != null) {
            // Requested, not waited for: the client is waiting for this callback.
            stops.add(Map.entry(System.nanoTime(), c.stop()));
          } else {
            early.incrementAndGet();
            stopOutside();
          }
        } else if (text.equals("stop outside")) {
          stopOutside();
        }
      } finally {
        inProgress.decrementAndGet();
      }
    }

    /** Stop from another thread (its wait, with the deadline, is the run's). */
    void stopOutside() {
      CompletableFuture<Void> asked = new CompletableFuture<>();
      stops.add(Map.entry(System.nanoTime(), asked));
      Thread t = new Thread(() -> {
        OpampClient cl = awaitClient();
        if (cl != null) {
          cl.stop().whenComplete((v, e) -> {
            if (e == null) {
              asked.complete(null);
            } else {
              asked.completeExceptionally(e);
            }
          });
        }
      }, "adversary-stop-" + seed);
      t.setDaemon(true);
      t.start();
    }

    OpampClient awaitClient() {
      for (long end = System.nanoTime() + 10_000_000_000L; System.nanoTime() < end; ) {
        OpampClient c = client.get();
        if (c != null) {
          return c;
        }
        Thread.onSpinWait();
      }
      return null;
    }

    static String brief(Notification n) {
      if (n instanceof Notification.BadMessage b) {
        return "BadMessage(" + b.detail().substring(0, Math.min(80, b.detail().length())) + ")";
      }
      return n instanceof Notification.Stopped s ? "Stopped(" + s.error() + ")" : n.getClass().getSimpleName();
    }

    Result run() {
      try {
        check();
      } catch (Throwable e) {
        failure("the harness: " + e);
      } finally {
        if (raw != null) {
          try {
            raw.close();
          } catch (IOException e) {
            // it is going anyway
          }
        }
        if (early.get() > 0) {
          System.out.println("AdversaryPropertiesTest: tolerated S5: " + early.get()
              + " callback(s) before start returned (seed " + seed + ")");
        }
        for (String t : tolerated) {
          System.out.println("AdversaryPropertiesTest: tolerated a late timer (S4, or load; seed " + seed + "): " + t);
        }
      }
      return result;
    }

    private void check() throws Exception {
      Files.createDirectories(dir);
      Path state = dir.resolve("state");
      String run = Long.toString(seed);
      // The adversary runs several seeds at once: on a loaded host one of its own steps has taken
      // seconds (5 s when it ran interpreted), during which the shell can feed it nothing (one core call at a
      // time). Its margin for lateness covers that, not the shell's own timing.
      String endpoint = "http://127.0.0.1:" + server.port() + "/opamp?seed=" + seed + "&margin=" + MARGIN_MS
          + (query.isEmpty() ? "" : "&" + query);
      Tls tls = null;
      if (mode == OpampClient.Mode.RUST) {
        // Rust mode: the run has a raw server of its own, which the adversary opens streams to
        // (raw=PORT), some of them with TLS under the name its certificate is for (tls=NAME): the
        // client trusts the tests' CA for it.
        raw = new AdversaryRawTest.Server(RawTls.CONTEXT);
        endpoint += "&raw=" + raw.port() + "&tls=" + AdversaryRawTest.TLS_NAME;
        tls = TransportTest.trusting(TransportTest.CA);
      }
      OpampClient.Config.Builder config = OpampClient.Config.builder(endpoint).stateFile(state).mode(mode).environment(Map.of());
      if (tls != null) {
        config.tls(tls);
      }
      OpampClient c = OpampClient.start(config.build(),
          OpampClient.Status.of(Opamp.AgentDescription.getDefaultInstance()), this::notify, module);
      client.set(c);
      started = true;

      // Until stop is asked for, or the client stops on its own.
      long deadline = System.nanoTime() + RUN_DEADLINE_MS * 1_000_000L;
      while (stops.isEmpty() && stopped.isEmpty() && System.nanoTime() < deadline) {
        Thread.sleep(5);
      }
      if (stops.isEmpty() && stopped.isEmpty()) {
        failure("no stop asked for, and the client didn't stop, within " + RUN_DEADLINE_MS + " ms");
        stops.add(Map.entry(System.nanoTime(), c.stop()));
      }
      boolean returned = true;
      for (Map.Entry<Long, CompletableFuture<Void>> s : stops) {
        long left = STOP_BOUND_MS - (System.nanoTime() - s.getKey()) / 1_000_000;
        try {
          s.getValue().get(Math.max(left, 0), TimeUnit.MILLISECONDS);
        } catch (TimeoutException e) {
          failure("RULE-SHELL-07: stop didn't return within " + STOP_BOUND_MS + " ms");
          returned = false;
        } catch (ExecutionException e) {
          failure("RULE-SHELL-02: the client stopped with an error: " + e.getCause());
        }
      }
      if (stops.isEmpty()) { // it stopped on its own
        try {
          c.stop().get(STOP_BOUND_MS, TimeUnit.MILLISECONDS);
        } catch (TimeoutException e) {
          failure("RULE-SHELL-07: stop didn't return within " + STOP_BOUND_MS + " ms");
          returned = false;
        } catch (ExecutionException e) {
          failure("RULE-SHELL-02: the client stopped with an error: " + e.getCause());
        }
      }
      long stoppedAt = System.nanoTime();
      // Sent now, on a connection of its own: what the client sent before stop returned reaches the
      // server by the time this does (ARRIVAL_GRACE_MS).
      Marker marker = returned ? new Marker(server.port(), run) : null;
      AdversaryRawTest.Marker rawMarker = null;
      if (raw != null) {
        // The raw server's marker, at the same moment; and from here a connection it was told not
        // to read is read: only that tells it the connection has gone.
        rawMarker = returned ? new AdversaryRawTest.Marker(raw.port(), MARKER_BOUND_MS) : null;
        raw.release();
      }
      Path downloads = Executor.downloadsDir(state);
      Map<String, String> before = listing(downloads);
      if (returned) {
        long end = stoppedAt + AFTER_STOP_MS * 1_000_000L;
        while (server.open(run) > 0 && System.nanoTime() < end) {
          Thread.sleep(10);
        }
        long rest = (end - System.nanoTime()) / 1_000_000;
        if (rest > 0) {
          Thread.sleep(rest);
        }
        if (server.open(run) > 0) {
          failure("RULE-SHELL-07: " + server.open(run) + " WebSocket connection(s) still open " + AFTER_STOP_MS
              + " ms after stop returned");
        }
        // A connection the client gave up on at stop may still reach the server's accept after stop
        // returned (its connect was already sent), but not after the marker sent then has.
        String markerError = marker.answered();
        String late = markerError == null ? server.afterStop(run, stoppedAt) : null;
        if (markerError != null) {
          failure("the harness's marker request failed: " + markerError);
        } else if (late != null) {
          failure(late);
        }
        if (raw != null) {
          // The streams: every connection gone, and none made after stop returned.
          String rawError = rawMarker.answered();
          String rawLate = rawError == null ? raw.afterMark(ARRIVAL_GRACE_MS) : null;
          if (rawError != null) {
            failure("the harness's marker on the raw server failed: " + rawError);
          } else if (rawLate != null) {
            failure(rawLate);
          }
          int[] open = raw.open();
          if (open[0] > 0) {
            failure("RULE-SHELL-07: " + open[0] + " stream connection(s) still open " + AFTER_STOP_MS
                + " ms after stop returned (" + open[1] + " never said who they were)");
          }
        }
        Map<String, String> after = listing(downloads);
        if (!after.equals(before)) {
          failure("RULE-SHELL-07: the downloads changed after stop returned (S3): " + before + " -> " + after);
        }
      }
      Thread.sleep(20);
      if (stopped.size() != 1) {
        failure("RULE-SHELL-02: stopped " + stopped.size() + " times");
      }
      for (Notification.Stopped s : stopped) {
        if (s.error() != null) {
          failure("RULE-SHELL-02: the client stopped with an error: " + s.error());
        }
      }
      result.verdicts = verdicts.size();
      if (verdicts.size() != 1) {
        failure((verdicts.isEmpty() ? "no verdict" : verdicts.size() + " verdicts"));
      }
      if (!verdicts.isEmpty()) {
        Map<String, String> v = new HashMap<>();
        for (String kv : verdicts.get(0).substring("verdict ".length()).split(" ")) {
          int eq = kv.indexOf('=');
          if (eq > 0) {
            v.put(kv.substring(0, eq), kv.substring(eq + 1));
          }
        }
        for (String p : v.getOrDefault("used", "").split(",")) {
          if (!p.isEmpty()) {
            result.used.add(p);
          }
        }
        for (String p : v.getOrDefault("enabled", "").split(",")) {
          if (!p.isEmpty()) {
            result.enabled.add(p);
          }
        }
        if (!Long.toString(tolerated.size()).equals(v.get("violations"))) {
          failure("the verdict has violations: " + verdicts.get(0).substring(0, Math.min(300, verdicts.get(0).length())));
        }
        String stored = v.getOrDefault("stored", "-");
        if (!stored.equals("-")) {
          byte[] file = Files.exists(state) ? Files.readAllBytes(state) : new byte[0];
          // That data, or the data of a later store (RULE-SHELL-09): one that landed and was then
          // cancelled before its stored was fed (at stop, say) is never answered (RULE-SHELL-06).
          long last = storeId(HexFormat.of().parseHex(stored));
          long held = storeId(file);
          if (!HexFormat.of().formatHex(file).equals(stored) && !(last >= 0 && held > last)) {
            failure("RULE-SHELL-09: the state file holds " + file.length + " bytes that aren't the data last answered"
                + " stored (" + stored.length() / 2 + " bytes), nor a later store's");
          }
        }
      }
    }

    /** The id of the adversary's store whose data this is ("opamp-adversary seed S store N ..."), or -1. */
    long storeId(byte[] data) {
      String head = new String(data, 0, Math.min(data.length, 80), StandardCharsets.ISO_8859_1);
      java.util.regex.Matcher m = java.util.regex.Pattern.compile("^opamp-adversary +seed " + seed + " store (\\d+) ").matcher(head);
      return m.find() ? Long.parseLong(m.group(1)) : -1;
    }

    /** The downloads directory: each file's size and time, by name. */
    static Map<String, String> listing(Path dir) throws IOException {
      Map<String, String> out = new TreeMap<>();
      if (!Files.isDirectory(dir)) {
        return out;
      }
      try (var files = Files.list(dir)) {
        for (Path f : files.toList()) {
          try {
            out.put(f.getFileName().toString(), Files.size(f) + "@" + Files.getLastModifiedTime(f).toMillis());
          } catch (IOException e) {
            out.put(f.getFileName().toString(), "gone");
          }
        }
      }
      return out;
    }
  }

  /**
   * The adversary's local server, over plain sockets: {@code POST /h}, {@code GET
   * /d} and the {@code GET /w} WebSocket upgrade, each doing what its query says; per run (the
   * {@code run} parameter) the WebSocket connections open and when the last request or upgrade
   * arrived.
   */
  static final class AdversaryServer implements AutoCloseable {
    private final ServerSocket listener = new ServerSocket(0, 200, InetAddress.getByName("127.0.0.1"));
    private final Set<Socket> sockets = ConcurrentHashMap.newKeySet();
    private final Map<String, AtomicInteger> open = new ConcurrentHashMap<>();
    private final Map<String, Arrival> last = new ConcurrentHashMap<>();
    /** Per run, when (System.nanoTime) the harness's marker (GET /marker, sent as stop returned) arrived. */
    private final Map<String, Long> marker = new ConcurrentHashMap<>();
    private final ScheduledExecutorService timers = Executors.newScheduledThreadPool(2, r -> {
      Thread t = new Thread(r, "adversary-server-timer");
      t.setDaemon(true);
      return t;
    });

    AdversaryServer() throws IOException {
      Thread accept = new Thread(() -> {
        while (!listener.isClosed()) {
          try {
            Socket s = listener.accept();
            if (lateMs > 0) {
              pause(lateMs);
            }
            long accepted = System.nanoTime();
            sockets.add(s);
            Thread t = new Thread(() -> serve(s, accepted), "adversary-server");
            t.setDaemon(true);
            t.start();
          } catch (IOException e) {
            return;
          }
        }
      }, "adversary-server-accept");
      accept.setDaemon(true);
      accept.start();
    }

    int port() {
      return listener.getLocalPort();
    }

    int open(String run) {
      AtomicInteger n = open.get(run);
      return n == null ? 0 : n.get();
    }

    private static final byte[] HEAD_END = "\r\n\r\n".getBytes(StandardCharsets.US_ASCII);
    /** A test's pause in this server's own handling of a request, after its first read. */
    volatile Runnable afterFirstRead = () -> {};
    /** A test's: a loaded host's pause before this server accepts a connection that has come. */
    volatile long lateMs;

    /**
     * A request or upgrade: when (System.nanoTime) its line and headers had reached this server,
     * when its connection was accepted, and what it was.
     */
    record Arrival(long at, long accepted, String what) {}

    /** The last request or upgrade of {@code run} to arrive; null: none. */
    Arrival lastArrival(String run) {
      return last.get(run);
    }

    /**
     * What breaks RULE-SHELL-07 in when {@code run}'s requests and upgrades arrived, stop having
     * returned at {@code stoppedAt} (null: nothing does): one that arrived more than {@link
     * #ARRIVAL_GRACE_MS} after the marker the harness sent then.
     */
    String afterStop(String run, long stoppedAt) {
      Arrival arrival = last.get(run);
      Long marked = marker.get(run);
      if (arrival == null || marked == null || arrival.at() <= marked + ARRIVAL_GRACE_MS * 1_000_000L) {
        return null;
      }
      return "RULE-SHELL-07: a request or upgrade arrived " + (arrival.at() - stoppedAt) / 1_000_000
          + " ms after stop returned, " + (arrival.at() - marked) / 1_000_000 + " ms after a request the harness sent then: "
          + arrival.what() + " (its connection accepted " + (arrival.accepted() - stoppedAt) / 1_000_000
          + " ms after stop returned)";
    }

    @Override
    public void close() throws IOException {
      listener.close();
      for (Socket s : sockets) {
        s.close();
      }
      timers.shutdownNow();
    }

    private void serve(Socket s, long accepted) {
      try (s) {
        // A request arrives when its line and headers have reached this server, not when this
        // server has got round to parsing them: on a loaded host that has lagged the bytes by up to
        // 700 ms, and a request sent before stop returned was counted as arriving after it. When
        // the first read holds the whole head (the JDK writes it at once), it arrived by then.
        InputStream raw = s.getInputStream();
        byte[] first = new byte[8192];
        int got = raw.read(first);
        if (got < 0) {
          return;
        }
        long read = System.nanoTime();
        afterFirstRead.run();
        boolean whole = contains(first, got, HEAD_END);
        DataInputStream in = new DataInputStream(new java.io.BufferedInputStream(
            new java.io.SequenceInputStream(new java.io.ByteArrayInputStream(first, 0, got), raw)));
        String requestLine = line(in);
        String[] parts = requestLine.split(" ");
        if (parts.length < 2) {
          return;
        }
        Map<String, String> headers = new LinkedHashMap<>();
        for (String h = line(in); !h.isEmpty(); h = line(in)) {
          int colon = h.indexOf(':');
          if (colon > 0) {
            headers.put(h.substring(0, colon).trim().toLowerCase(Locale.ROOT), h.substring(colon + 1).trim());
          }
        }
        String target = parts[1];
        String path = target.contains("?") ? target.substring(0, target.indexOf('?')) : target;
        Map<String, String> q = new HashMap<>();
        if (target.contains("?")) {
          for (String kv : target.substring(target.indexOf('?') + 1).split("&")) {
            int eq = kv.indexOf('=');
            q.put(eq < 0 ? kv : kv.substring(0, eq), eq < 0 ? "" : kv.substring(eq + 1));
          }
        }
        String run = q.getOrDefault("run", "");
        Arrival arrival = new Arrival(whole ? read : System.nanoTime(), accepted, parts[0] + " " + target);
        if (path.equals("/marker")) {
          // The harness's own, not the client's: when what was sent as stop returned got here.
          marker.put(run, arrival.at());
          s.getOutputStream().write("HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n".getBytes(StandardCharsets.US_ASCII));
          s.getOutputStream().flush();
          return;
        }
        last.merge(run, arrival, (a, b) -> b.at() > a.at() ? b : a);
        // The request's body, if any.
        String length = headers.get("content-length");
        if (length != null) {
          in.readFully(new byte[Integer.parseInt(length)]);
        } else if ("chunked".equalsIgnoreCase(headers.get("transfer-encoding"))) {
          for (int n = Integer.parseInt(line(in).split(";")[0].trim(), 16); n > 0;
              n = Integer.parseInt(line(in).split(";")[0].trim(), 16)) {
            in.readFully(new byte[n]);
            line(in);
          }
          line(in);
        }
        long delay = num(q, "delay");
        OutputStream out = s.getOutputStream();
        if (path.equals("/w")) {
          websocket(s, in, out, headers, q, run, delay);
          return;
        }
        if (!hold(s, in, delay)) {
          return; // the client went
        }
        if (num(q, "drop") == 1) {
          return; // closed without an answer
        }
        int status = (int) (q.containsKey("status") ? num(q, "status") : 200);
        int size = (int) num(q, "size");
        out.write(("HTTP/1.1 " + status + " Adversary\r\nContent-Length: " + size
            + "\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\n").getBytes(StandardCharsets.US_ASCII));
        out.write(body(size));
        out.flush();
        s.shutdownOutput();
        // Let the client read it all before the connection goes.
        s.setSoTimeout(2_000);
        while (in.read() >= 0) {
          // drain
        }
      } catch (Exception e) {
        // the connection went
      } finally {
        sockets.remove(s);
      }
    }

    private static boolean contains(byte[] b, int n, byte[] what) {
      for (int i = 0; i + what.length <= n; i++) {
        if (java.util.Arrays.equals(b, i, i + what.length, what, 0, what.length)) {
          return true;
        }
      }
      return false;
    }

    private static long num(Map<String, String> q, String key) {
      String v = q.get(key);
      if (v == null || v.isEmpty()) {
        return 0;
      }
      try {
        return Long.parseLong(v);
      } catch (NumberFormatException e) {
        return 0;
      }
    }

    static byte[] body(int size) {
      byte[] b = new byte[size];
      for (int i = 0; i < size; i++) {
        b[i] = (byte) (i % 251);
      }
      return b;
    }

    /**
     * Wait {@code delay} ms ({@code -1}: until the client goes, up to 30 s): whether the client is
     * still there.
     */
    private static boolean hold(Socket s, InputStream in, long delay) throws IOException {
      if (delay == 0) {
        return true;
      }
      long ms = delay < 0 ? 30_000 : delay;
      long end = System.nanoTime() + ms * 1_000_000L;
      int was = s.getSoTimeout();
      try {
        while (System.nanoTime() < end) {
          s.setSoTimeout((int) Math.max(1, Math.min(50, (end - System.nanoTime()) / 1_000_000)));
          try {
            if (in.read() < 0) {
              return false;
            }
          } catch (SocketTimeoutException e) {
            // still there
          }
        }
        return delay >= 0;
      } finally {
        s.setSoTimeout(was);
      }
    }

    private void websocket(Socket s, DataInputStream in, OutputStream out, Map<String, String> headers,
        Map<String, String> q, String run, long delay) throws Exception {
      if (!hold(s, in, delay)) {
        return;
      }
      long status = num(q, "status");
      if (status != 0) {
        out.write(("HTTP/1.1 " + status + " Refused\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            .getBytes(StandardCharsets.US_ASCII));
        out.flush();
        return;
      }
      String key = headers.get("sec-websocket-key");
      if (key == null) {
        return;
      }
      String accept = Base64.getEncoder().encodeToString(MessageDigest.getInstance("SHA-1")
          .digest((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").getBytes(StandardCharsets.US_ASCII)));
      AtomicInteger count = open.computeIfAbsent(run, k -> new AtomicInteger());
      count.incrementAndGet();
      try {
        out.write(("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
            + "Sec-WebSocket-Accept: " + accept + "\r\n\r\n").getBytes(StandardCharsets.US_ASCII));
        out.flush();
        for (long i = num(q, "push"); i > 0; i--) {
          frame(out, 2, body(16));
        }
        java.util.concurrent.atomic.AtomicBoolean closeSent = new java.util.concurrent.atomic.AtomicBoolean();
        long close = num(q, "close");
        if (close > 0) {
          timers.schedule(() -> {
            try {
              if (closeSent.compareAndSet(false, true)) {
                frame(out, 8, new byte[] {0x03, (byte) 0xe8}); // 1000
              }
            } catch (IOException e) {
              // gone
            }
          }, close, TimeUnit.MILLISECONDS);
        }
        long drop = num(q, "drop");
        if (drop > 0) {
          timers.schedule(() -> {
            try {
              s.close();
            } catch (IOException e) {
              // gone
            }
          }, drop, TimeUnit.MILLISECONDS);
        }
        boolean echo = num(q, "echo") == 1;
        ByteArrayOutputStream message = new ByteArrayOutputStream();
        while (true) {
          int b0 = in.read();
          if (b0 < 0) {
            return;
          }
          int b1 = in.readUnsignedByte();
          long len = b1 & 0x7f;
          if (len == 126) {
            len = in.readUnsignedShort();
          } else if (len == 127) {
            len = in.readLong();
          }
          byte[] mask = new byte[4];
          if ((b1 & 0x80) != 0) {
            in.readFully(mask);
          }
          byte[] payload = new byte[(int) len];
          in.readFully(payload);
          for (int i = 0; i < payload.length; i++) {
            payload[i] ^= mask[i % 4];
          }
          switch (b0 & 0x0f) {
            case 0, 2 -> {
              message.writeBytes(payload);
              if ((b0 & 0x80) != 0) {
                byte[] data = message.toByteArray();
                message.reset();
                if (echo) {
                  frame(out, 2, data);
                }
              }
            }
            case 8 -> {
              if (closeSent.compareAndSet(false, true)) {
                frame(out, 8, payload.length >= 2 ? java.util.Arrays.copyOf(payload, 2) : payload);
              }
              return;
            }
            case 9 -> frame(out, 10, payload);
            default -> {}
          }
        }
      } finally {
        count.decrementAndGet();
      }
    }

    private static String line(InputStream in) throws IOException {
      ByteArrayOutputStream b = new ByteArrayOutputStream();
      for (int c = in.read(); c != '\n'; c = in.read()) {
        if (c < 0) {
          throw new IOException("closed");
        }
        if (c != '\r') {
          b.write(c);
        }
      }
      return b.toString(StandardCharsets.ISO_8859_1);
    }

    private static void frame(OutputStream out, int opcode, byte[] payload) throws IOException {
      ByteArrayOutputStream f = new ByteArrayOutputStream();
      f.write(0x80 | opcode);
      if (payload.length < 126) {
        f.write(payload.length);
      } else if (payload.length < 65536) {
        f.write(126);
        f.write(payload.length >>> 8);
        f.write(payload.length);
      } else {
        f.write(127);
        for (int i = 7; i >= 0; i--) {
          f.write((int) ((long) payload.length >>> (8 * i)));
        }
      }
      f.writeBytes(payload);
      synchronized (out) {
        out.write(f.toByteArray());
        out.flush();
      }
    }
  }
}
