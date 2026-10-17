// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.common.AttributesBuilder;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanContext;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.TraceFlags;
import io.opentelemetry.api.trace.TraceState;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Context;
import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.logs.SdkLoggerProvider;
import io.opentelemetry.sdk.logs.data.LogRecordData;
import io.opentelemetry.sdk.logs.export.BatchLogRecordProcessor;
import io.opentelemetry.sdk.logs.export.LogRecordExporter;
import io.opentelemetry.sdk.logs.export.SimpleLogRecordProcessor;
import io.opentelemetry.sdk.testing.exporter.InMemoryLogRecordExporter;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.EventData;
import io.opentelemetry.sdk.trace.data.LinkData;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.BatchSpanProcessor;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import java.time.Duration;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.SplittableRandom;
import java.util.concurrent.ConcurrentLinkedQueue;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.locks.LockSupport;
import java.util.logging.Handler;
import java.util.logging.Level;
import java.util.logging.LogRecord;
import java.util.logging.Logger;
import java.util.regex.Pattern;
import java.util.stream.Collectors;
import org.junit.jupiter.api.Test;

/**
 * The controls under fire: one thread (the applier, as the agent's OpAMP callback and debug timer
 * are, one at a time) flips through a seeded sequence of settings documents as fast as it can,
 * while workers emit spans (roots, children, remote children, with attributes, events, links and
 * exceptions) and log records through the real SDK, simple and batch processors both, into
 * in-memory exporters behind the gates. The documents overlap rules and change their order, add,
 * change and withdraw caps, levels per scope, redaction patterns, the send_* switches and debug
 * windows, and the empty document turns up too (in runs whose redaction isn't pinned, below). The
 * first document is in force before anything is emitted, as the agent's saved settings are.
 *
 * <p>Whatever the interleaving, the run ends in time and nothing throws (in a worker, or in a gate,
 * which the SDK's processors would only log); a key every document that sends the signal drops is
 * never exported, on a span, event, link or record, whatever the documents that don't send it say
 * (an export sees one document's send_* switch and its redaction together, never one's switch
 * beside another's redaction); a key every document that sends the signal masks and none of them
 * drops is only ever {@code [REDACTED]}; every attribute list exported is the redaction of one
 * document of the run that sends its signal, never half of one and half of another, and a span's
 * lists all of the same one (the gate redacts a whole span at once); a child of a span sampled
 * here is sampled, whatever the cap; and with the logs export off in every document, no record is
 * exported. Then a final document is applied, and
 * all that is emitted after it is exactly what that document says, caps included (the clock is
 * frozen far ahead, so each bucket is full and stays as the emissions leave it).
 *
 * <p>What can't be checked while flipping: which rate or rule decided a root, which level kept a
 * record and how many the caps let through, since each depends on which document was in force at
 * that instant, and the exporters don't record that (each gate reads all its settings from one
 * document, but which one an export doesn't say). Those are held exactly in the final phase
 * instead, through the simple processors only (a batch processor would export some of it late, and
 * its log gate would share the log cap).
 *
 * <p>OPAMP_PROP_RUNS sets the number of runs (default 6, a couple of seconds); a failure prints its
 * run's seed, and OPAMP_PROP_SEED replays that one run: the same documents in the same order and
 * the same emissions, though not the same interleaving.
 */
class ControlsTortureTest {
  /** How long the applier flips, a run. */
  private static final long FLIP_NS = TimeUnit.MILLISECONDS.toNanos(250);
  /** A run that takes longer is stuck. */
  private static final Duration DEADLINE = Duration.ofSeconds(30);
  private static final int WORKERS = 4;

  // The attributes on every list emitted: their values, and the keys the patterns are about.
  private static final Map<String, Object> ATTRS = new LinkedHashMap<>();

  static {
    ATTRS.put("r.0", "v0");
    ATTRS.put("r.1", 1L);
    ATTRS.put("r.2", true);
    ATTRS.put("r.3", 3.5);
    ATTRS.put("r.4", List.of("a", "b"));
    ATTRS.put("r.5", "v5");
    ATTRS.put("r.6", 6L);
    ATTRS.put("r.7", "v7");
    ATTRS.put("secret.token", "s3cr3t");
    ATTRS.put("Secret.Key", 42L);
    ATTRS.put("user.password", "hunter2");
    ATTRS.put("DB.Password.Hash", "abc");
    ATTRS.put("x.PASSWORD", 7L);
  }

  /**
   * Dropped and masked in every document of a pinned run that sends traces or logs (none of the
   * pools touches these keys); a dark document, which sends neither, may leave them out.
   */
  private static final String PINNED_DROP = "*password*";
  private static final String PINNED_MASK = "secret.*";
  private static final List<String> DROP_POOL = List.of("r.0", "R.1", "r.2*", "*.3", "r.5");
  private static final List<String> MASK_POOL = List.of("r.4", "r.5", "R.6", "r.*", "*7", "SECRET.TOKEN");
  private static final List<String> SCOPE_PATTERNS = List.of("app.db*", "app.db", "app.web*", "lib", "*", "app*");
  private static final List<String> SCOPES = List.of("app.db", "app.db.pool", "app.web.x", "lib", "libx", "other", "app");
  private static final List<String> LEVELS = List.of("trace", "debug", "info", "warn", "error", "fatal");
  private static final int[] LEVEL_NUMBERS = {1, 5, 9, 13, 17, 21};
  private static final int[] SEVERITIES = {0, 1, 5, 9, 13, 17, 21};
  private static final List<String> CAPS = List.of("0.5", "2", "5", "1000");

  /** A sampling rule, as sent; {@code tenant}: a glob on the tenant attribute. */
  record Rule(double rate, String name, String kind, String path, String tenant) {
    String json() {
      StringBuilder b = new StringBuilder("{\"rate\":").append(rate);
      if (name != null) {
        b.append(",\"name\":\"").append(name).append('"');
      }
      if (kind != null) {
        b.append(",\"kind\":\"").append(kind).append('"');
      }
      if (path != null) {
        b.append(",\"path\":\"").append(path).append('"');
      }
      if (tenant != null) {
        b.append(",\"attributes\":{\"tenant\":\"").append(tenant).append("\"}");
      }
      return b.append('}').toString();
    }

    boolean matches(Root r) {
      return (name == null || glob(name, false).matcher(r.name()).matches())
          && (kind == null || kind.equals(r.kind().name().toLowerCase()))
          && (path == null || (r.path() != null && glob(path, false).matcher(r.path()).matches()))
          && (tenant == null || (r.tenant() != null && glob(tenant, false).matcher(r.tenant()).matches()));
    }
  }

  private static final List<Rule> RULES = List.of(
      new Rule(0, "health*", null, null, null),
      new Rule(1, null, "server", null, null),
      new Rule(0.5, "checkout *", null, null, null),
      new Rule(0, null, null, null, "acme"),
      new Rule(1, null, null, "/api/*", null),
      new Rule(0, null, "server", "/health*", null),
      new Rule(1, "checkout *", "internal", null, null));

  /**
   * A settings document, as the generator made it; the oracle reads this, the controls the JSON.
   * {@code debug}: 0 none, 1 a window in force, 2 one already over. Levels are SeverityNumbers,
   * {@link Controls.LogLevels#OFF} for off; null fields are left out.
   */
  record Doc(List<String> drop, List<String> mask, List<Rule> rules, Double rate, Boolean sendTraces,
      Boolean sendLogs, Integer level, Map<String, Integer> byScope, String traceCap, String logCap, int debug,
      Double debugRate, Integer debugLevel) {
    static final Doc EMPTY = new Doc(List.of(), List.of(), List.of(), null, null, null, null, Map.of(), null, null, 0,
        null, null);

    String json() {
      List<String> set = new ArrayList<>();
      List<String> redaction = new ArrayList<>();
      if (!drop.isEmpty()) {
        redaction.add("\"drop\":" + strings(drop));
      }
      if (!mask.isEmpty()) {
        redaction.add("\"mask\":" + strings(mask));
      }
      if (!redaction.isEmpty()) {
        set.addAll(List.of("attribute_redaction", Vectors.override(String.join(",", redaction))));
      }
      if (!rules.isEmpty()) {
        set.addAll(List.of("sampling_rules", Vectors.override(
            "\"rules\":[" + rules.stream().map(Rule::json).collect(Collectors.joining(",")) + "]")));
      }
      if (rate != null) {
        set.addAll(List.of("head_sampling", Vectors.override("\"ratio\":" + rate)));
      }
      if (sendTraces != null || sendLogs != null) {
        set.addAll(List.of("signal_exports", Vectors.exports(signal(sendTraces), "", signal(sendLogs))));
      }
      List<String> levels = new ArrayList<>();
      if (level != null) {
        levels.add("\"level\":\"" + levelName(level) + "\"");
      }
      if (!byScope.isEmpty()) {
        levels.add("\"scopes\":{" + byScope.entrySet().stream()
            .map(e -> "\"" + e.getKey() + "\":\"" + levelName(e.getValue()) + "\"").collect(Collectors.joining(",")) + "}");
      }
      if (!levels.isEmpty()) {
        set.addAll(List.of("log_levels", Vectors.override(String.join(",", levels))));
      }
      if (traceCap != null) {
        set.addAll(List.of("trace_rate_limit", Vectors.override("\"traces_per_second\":" + traceCap)));
      }
      if (logCap != null) {
        set.addAll(List.of("log_rate_limit", Vectors.override("\"records_per_second\":" + logCap)));
      }
      if (debug != 0) {
        // The layer decides debug mode at Vectors.NOW (12:00Z): an hour on, or an hour gone.
        List<String> d = new ArrayList<>(List.of(
            "\"until\":\"" + (debug == 1 ? "2026-10-14T13:00:00Z" : "2026-10-14T11:00:00Z") + "\""));
        if (debugRate != null) {
          d.add("\"sampling_ratio\":" + debugRate);
        }
        if (debugLevel != null) {
          d.add("\"log_level\":\"" + levelName(debugLevel) + "\"");
        }
        set.addAll(List.of("debug", Vectors.override(String.join(",", d))));
      }
      return Vectors.every(set.toArray(String[]::new));
    }

    private static String strings(List<String> values) {
      return values.stream().map(v -> "\"" + v + "\"").collect(Collectors.joining(",", "[", "]"));
    }

    private static String signal(Boolean on) {
      return on == null ? "" : on ? "on" : "off";
    }

    /** What happens to each of {@link #ATTRS}' keys on export. */
    Map<String, Fate> redaction() {
      Map<String, Fate> out = new LinkedHashMap<>();
      for (String k : ATTRS.keySet()) {
        out.put(k, drop.stream().anyMatch(p -> glob(p, true).matcher(k).matches()) ? Fate.DROPPED
            : mask.stream().anyMatch(p -> glob(p, true).matcher(k).matches()) ? Fate.MASKED : Fate.KEPT);
      }
      return out;
    }

    /** Whether a new trace's root is sampled, before the cap (rates are 0 or 1 here). */
    boolean samples(Root r) {
      if (debug == 1) {
        return (debugRate == null ? 1 : debugRate) == 1;
      }
      for (Rule rule : rules) {
        if (rule.matches(r)) {
          return rule.rate() == 1;
        }
      }
      return rate == null || rate == 1; // left out: the agent's own, parent-based always-on
    }

    /** Whether a record of the scope and SeverityNumber passes the levels. */
    boolean keeps(String scope, int severity) {
      int min = debug == 1 ? (debugLevel == null ? 5 : debugLevel) : level == null ? 0 : level;
      Map<String, Integer> scoped = debug == 1 ? Map.of() : byScope;
      Integer t = scoped.get(scope);
      if (t == null) {
        int longest = -1;
        for (var e : scoped.entrySet()) {
          String p = e.getKey();
          if (p.endsWith("*") && scope.startsWith(p.substring(0, p.length() - 1)) && p.length() > longest) {
            longest = p.length();
            t = e.getValue();
          }
        }
      }
      int threshold = t == null ? min : t;
      return threshold == 0 || (threshold != Controls.LogLevels.OFF && (severity == 0 || severity >= threshold));
    }

    /** A full bucket's whole tokens, or all of them without a cap. */
    static int tokens(String cap) {
      return cap == null ? Integer.MAX_VALUE : (int) Math.floor(Math.max(Double.parseDouble(cap), 1));
    }
  }

  enum Fate { KEPT, MASKED, DROPPED, WRONG }

  /** A root span as the sampler sees it start. */
  record Root(String name, SpanKind kind, String path, String tenant) {}

  private static final List<Root> ROOTS = List.of(
      new Root("health check", SpanKind.INTERNAL, null, null),
      new Root("GET", SpanKind.SERVER, "/health", null),
      new Root("GET", SpanKind.SERVER, "/api/orders", "acme"),
      new Root("checkout cart", SpanKind.INTERNAL, null, null),
      new Root("checkout cart", SpanKind.CLIENT, null, "acme"),
      new Root("job", SpanKind.CONSUMER, "/api/job", "other"),
      new Root("job", SpanKind.PRODUCER, null, null));

  private static String levelName(int n) {
    if (n == Controls.LogLevels.OFF) {
      return "off";
    }
    for (int i = 0; i < LEVEL_NUMBERS.length; i++) {
      if (LEVEL_NUMBERS[i] == n) {
        return LEVELS.get(i);
      }
    }
    throw new IllegalArgumentException("level " + n);
  }

  /** A settings glob as a regex: {@code *} any run, all else itself; ASCII case ignored if asked. */
  static Pattern glob(String g, boolean ignoreCase) {
    String re = java.util.Arrays.stream(g.split("\\*", -1)).map(Pattern::quote).collect(Collectors.joining(".*"));
    return Pattern.compile(re, ignoreCase ? Pattern.CASE_INSENSITIVE : 0);
  }

  /**
   * What a run is: its documents, the final one, and which invariants hold over them. A pinned
   * run's dark documents send neither traces nor logs, and redact nothing.
   */
  record Plan(List<Doc> docs, Doc last, boolean pinned, boolean logsOff) {
    List<Doc> all() {
      List<Doc> a = new ArrayList<>(docs);
      a.add(last);
      return a;
    }
  }

  /**
   * A document at random. {@code exact}: rates 0 or 1 only, for the final one. A pinned run's
   * documents all drop and mask the pinned patterns, but for a {@code dark} one: the traces and
   * logs exports off, and no redaction at all. A logs-off run's documents all switch the logs export off.
   */
  static Doc doc(SplittableRandom r, boolean pinned, boolean logsOff, boolean dark, boolean exact) {
    if (dark) {
      return new Doc(List.of(), List.of(), List.of(), null, false, false, null, Map.of(), null, null, 0, null,
          null);
    }
    if (!pinned && !logsOff && r.nextInt(6) == 0) {
      return Doc.EMPTY;
    }
    List<String> drop = new ArrayList<>(pinned ? List.of(PINNED_DROP) : List.of());
    List<String> mask = new ArrayList<>(pinned ? List.of(PINNED_MASK) : List.of());
    // Redaction lists grow and shrink: a random subset of each pool.
    DROP_POOL.stream().filter(p -> r.nextInt(3) == 0).forEach(drop::add);
    MASK_POOL.stream().filter(p -> r.nextInt(3) == 0).forEach(mask::add);
    // Rules overlap and change order: a shuffled subset.
    List<Rule> rules = new ArrayList<>(RULES.stream().filter(x -> r.nextInt(3) == 0)
        .filter(x -> !exact || x.rate() != 0.5).toList());
    for (int i = rules.size() - 1; i > 0; i--) {
      java.util.Collections.swap(rules, i, r.nextInt(i + 1));
    }
    double[] rates = exact ? new double[] {0, 1} : new double[] {0, 0.5, 1};
    Double rate = r.nextInt(3) == 0 ? null : rates[r.nextInt(rates.length)];
    Boolean sendTraces = r.nextInt(3) == 0 ? null : r.nextInt(4) != 0;
    Boolean sendLogs = logsOff ? Boolean.FALSE : r.nextInt(3) == 0 ? null : r.nextInt(4) != 0;
    Integer level = r.nextBoolean() ? null : LEVEL_NUMBERS[r.nextInt(LEVEL_NUMBERS.length)];
    Map<String, Integer> byScope = new LinkedHashMap<>();
    for (String p : SCOPE_PATTERNS) {
      if (r.nextInt(3) == 0) {
        byScope.put(p, r.nextInt(7) == 0 ? Controls.LogLevels.OFF : LEVEL_NUMBERS[r.nextInt(LEVEL_NUMBERS.length)]);
      }
    }
    String traceCap = r.nextBoolean() ? null : CAPS.get(r.nextInt(CAPS.size()));
    String logCap = r.nextBoolean() ? null : CAPS.get(r.nextInt(CAPS.size()));
    int debug = r.nextInt(5) == 0 ? 1 : r.nextInt(8) == 0 ? 2 : 0;
    Double debugRate = debug == 0 || r.nextBoolean() ? null : rates[r.nextInt(rates.length)];
    Integer debugLevel = debug == 0 || r.nextBoolean() ? null : LEVEL_NUMBERS[r.nextInt(LEVEL_NUMBERS.length)];
    return new Doc(drop, mask, rules, rate, sendTraces, sendLogs, level, byScope, traceCap, logCap, debug,
        debugRate, debugLevel);
  }

  static Plan plan(SplittableRandom r) {
    boolean pinned = r.nextInt(4) != 0;
    boolean logsOff = r.nextInt(4) == 0;
    // A third of the pinned runs have dark documents among the rest: a third of them.
    boolean darkToo = pinned && r.nextInt(3) == 0;
    List<Doc> docs = new ArrayList<>();
    for (int i = 8 + r.nextInt(25); i > 0; i--) {
      docs.add(doc(r, pinned, logsOff, darkToo && r.nextInt(3) == 0, false));
    }
    return new Plan(docs, doc(r, pinned, logsOff, false, true), pinned, logsOff);
  }

  @Test
  void settingsFlippedUnderLoadKeepEveryInvariantAndTheLastOneHoldsExactly() throws Exception {
    String runsEnv = System.getenv("OPAMP_PROP_RUNS");
    String seedEnv = System.getenv("OPAMP_PROP_SEED");
    int runs = seedEnv != null && !seedEnv.isEmpty() ? 1
        : runsEnv == null || runsEnv.isEmpty() ? 6 : Integer.parseInt(runsEnv);
    long base = System.nanoTime();
    for (int i = 0; i < runs; i++) {
      // Each run has a seed of its own, so one can be replayed alone.
      long seed = seedEnv != null && !seedEnv.isEmpty() ? Long.parseLong(seedEnv) : base + i * 0x9E3779B97F4A7C15L;
      System.out.println("ControlsTortureTest: run " + i + " of " + runs + ", OPAMP_PROP_SEED=" + seed);
      Plan plan = plan(new SplittableRandom(seed));
      try {
        run(plan, seed);
      } catch (Throwable e) {
        throw new AssertionError("run " + i + " of " + runs + " (replay: OPAMP_PROP_SEED=" + seed + "), "
            + plan.docs().size() + " documents, final " + plan.last().json() + ": " + e.getMessage(), e);
      }
    }
  }

  /** The SDK's processors log what a gate throws, and go on: caught here as a failure. */
  private static final class Thrown extends Handler {
    final ConcurrentLinkedQueue<String> seen = new ConcurrentLinkedQueue<>();

    @Override
    public void publish(LogRecord r) {
      // Only a throwable: a batch processor's full queue is a warning too, and no failure.
      if (r.getLevel().intValue() >= Level.WARNING.intValue() && r.getThrown() != null) {
        seen.add(r.getMessage() + ": " + r.getThrown());
      }
    }

    @Override
    public void flush() {}

    @Override
    public void close() {}
  }

  private static void run(Plan plan, long seed) throws Exception {
    // Every document read by the layer before the clock starts, so the applier only flips.
    List<SettingsProto.Settings> settings = plan.docs().stream()
        .map(d -> Vectors.settings(d.json(), Vectors.NOW)).toList();
    SettingsProto.Settings last = Vectors.settings(plan.last().json(), Vectors.NOW);

    // The first document is in force before anything is emitted, as the agent puts its saved
    // settings in force before its SDK starts: the controls' defaults are no document of the run.
    Controls c = new Controls();
    c.apply(settings.get(0));
    InMemorySpanExporter spans = InMemorySpanExporter.create();
    InMemorySpanExporter batchedSpans = InMemorySpanExporter.create();
    InMemoryLogRecordExporter logs = InMemoryLogRecordExporter.create();
    InMemoryLogRecordExporter batchedLogs = InMemoryLogRecordExporter.create();
    SdkTracerProvider tp = SdkTracerProvider.builder()
        .setSampler(c.sampler.wrap(Sampler.parentBased(Sampler.alwaysOn())))
        .addSpanProcessor(SimpleSpanProcessor.create(c.spans(spans)))
        .addSpanProcessor(BatchSpanProcessor.builder(c.spans(batchedSpans)).setScheduleDelay(Duration.ofMillis(5))
            .setMaxExportBatchSize(64).build())
        .build();
    // The batch log gate is closed for the final phase: it would take the log cap's tokens too.
    AtomicBoolean batching = new AtomicBoolean(true);
    SdkLoggerProvider lp = SdkLoggerProvider.builder()
        .addLogRecordProcessor(SimpleLogRecordProcessor.create(c.logs(logs)))
        .addLogRecordProcessor(BatchLogRecordProcessor.builder(whileOpen(batching, c.logs(batchedLogs)))
            .setScheduleDelay(Duration.ofMillis(5)).setMaxExportBatchSize(64).build())
        .build();
    Logger otel = Logger.getLogger("io.opentelemetry");
    Thrown thrown = new Thrown();
    otel.addHandler(thrown);
    ConcurrentLinkedQueue<String> broken = new ConcurrentLinkedQueue<>();
    try {
      AtomicBoolean stop = new AtomicBoolean();
      CountDownLatch done = new CountDownLatch(WORKERS + 1);
      List<Thread> threads = new ArrayList<>();
      SplittableRandom r = new SplittableRandom(seed ^ 0x5DEECE66DL);
      SplittableRandom ar = r.split();
      threads.add(new Thread(() -> {
        try {
          long end = System.nanoTime() + FLIP_NS;
          for (int i = 0; System.nanoTime() < end; i = (i + 1) % settings.size()) {
            c.apply(settings.get(i));
            if (ar.nextInt(4) == 0) {
              LockSupport.parkNanos(ar.nextInt(50_000));
            }
          }
        } catch (Throwable e) {
          broken.add("applier: " + e);
        } finally {
          stop.set(true);
          done.countDown();
        }
      }, "torture-applier"));
      for (int w = 0; w < WORKERS; w++) {
        SplittableRandom wr = r.split();
        int id = w;
        threads.add(new Thread(() -> {
          try {
            Tracer tracer = tp.get("torture");
            while (!stop.get()) {
              if (wr.nextInt(3) == 0) {
                String scope = SCOPES.get(wr.nextInt(SCOPES.size()));
                lp.get(scope).logRecordBuilder().setSeverity(VectorsTest.severity(SEVERITIES[wr.nextInt(SEVERITIES.length)]))
                    .setBody("w" + id).setAllAttributes(attributes()).emit();
              } else {
                String bad = emitTrace(tracer, ROOTS.get(wr.nextInt(ROOTS.size())), wr.nextInt(4) == 0);
                if (bad != null) {
                  broken.add(bad);
                }
              }
            }
          } catch (Throwable e) {
            broken.add("worker " + id + ": " + e);
          } finally {
            done.countDown();
          }
        }, "torture-worker-" + w));
      }
      threads.forEach(Thread::start);
      if (!done.await(DEADLINE.toMillis(), TimeUnit.MILLISECONDS)) {
        StringBuilder dump = new StringBuilder("stuck after " + DEADLINE + ":");
        for (Thread t : threads) {
          dump.append("\n").append(t.getName()).append(" ").append(t.getState());
          for (StackTraceElement e : t.getStackTrace()) {
            dump.append("\n  at ").append(e);
          }
        }
        stop.set(true);
        throw new AssertionError(dump.toString());
      }
      tp.forceFlush().join(DEADLINE.toMillis(), TimeUnit.MILLISECONDS);
      lp.forceFlush().join(DEADLINE.toMillis(), TimeUnit.MILLISECONDS);
      assertEquals(List.of(), List.copyOf(broken), "thrown, or a child of a sampled span not sampled");
      assertEquals(List.of(), List.copyOf(thrown.seen), "a gate threw");

      List<SpanData> flipped = new ArrayList<>(spans.getFinishedSpanItems());
      flipped.addAll(batchedSpans.getFinishedSpanItems());
      List<LogRecordData> flippedLogs = new ArrayList<>(logs.getFinishedLogRecordItems());
      flippedLogs.addAll(batchedLogs.getFinishedLogRecordItems());
      checkFlipped(plan, flipped, flippedLogs);

      // The final document, and with it a clock frozen an hour ahead: every bucket full at its
      // first take and none refilled, so the caps let through exactly a full bucket's worth.
      batching.set(false);
      c.apply(last);
      long frozen = System.nanoTime() + TimeUnit.HOURS.toNanos(1);
      c.nanoTime = () -> frozen;
      spans.reset();
      logs.reset();
      checkLast(plan.last(), c, tp, lp, spans, logs);
    } finally {
      otel.removeHandler(thrown);
      tp.shutdown().join(10, TimeUnit.SECONDS);
      lp.shutdown().join(10, TimeUnit.SECONDS);
    }
    System.out.println("ControlsTortureTest:   " + plan.docs().size() + " documents, pinned redaction " + plan.pinned()
        + ", logs off " + plan.logsOff());
  }

  /** {@code to}, while {@code open}; then nothing goes through. */
  static LogRecordExporter whileOpen(AtomicBoolean open, LogRecordExporter to) {
    return new LogRecordExporter() {
      @Override
      public CompletableResultCode export(java.util.Collection<LogRecordData> logs) {
        return open.get() ? to.export(logs) : CompletableResultCode.ofSuccess();
      }

      @Override
      public CompletableResultCode flush() {
        return to.flush();
      }

      @Override
      public CompletableResultCode shutdown() {
        return to.shutdown();
      }
    };
  }

  static Attributes attributes() {
    AttributesBuilder b = Attributes.builder();
    ATTRS.forEach((k, v) -> {
      if (v instanceof String s) {
        b.put(AttributeKey.stringKey(k), s);
      } else if (v instanceof Long l) {
        b.put(AttributeKey.longKey(k), l);
      } else if (v instanceof Boolean x) {
        b.put(AttributeKey.booleanKey(k), x);
      } else if (v instanceof Double d) {
        b.put(AttributeKey.doubleKey(k), d);
      } else {
        @SuppressWarnings("unchecked")
        List<String> list = (List<String>) v;
        b.put(AttributeKey.stringArrayKey(k), list);
      }
    });
    return b.build();
  }

  private static final SpanContext REMOTE = SpanContext.createFromRemoteParent("0af7651916cd43dd8448eb211c80319c",
      "b7ad6b7169203331", TraceFlags.getSampled(), TraceState.getDefault());
  private static final SpanContext REMOTE_UNSAMPLED = SpanContext.createFromRemoteParent(
      "0af7651916cd43dd8448eb211c80319d", "b7ad6b7169203332", TraceFlags.getDefault(), TraceState.getDefault());

  /**
   * A trace: the root (or a span under a remote parent, {@code remote}), a child and a grandchild,
   * each with every attribute, an event, a link and on the root an exception. What went wrong, or
   * null: a child of a span sampled here must be sampled.
   */
  static String emitTrace(Tracer tracer, Root root, boolean remote) {
    var b = tracer.spanBuilder(root.name()).setSpanKind(root.kind()).setAllAttributes(attributes())
        .addLink(REMOTE, attributes());
    if (root.path() != null) {
      b.setAttribute("url.path", root.path());
    }
    if (root.tenant() != null) {
      b.setAttribute("tenant", root.tenant());
    }
    if (remote) {
      b.setParent(Context.root().with(Span.wrap(REMOTE)));
    } else {
      b.setNoParent();
    }
    Span top = b.startSpan();
    top.addEvent("e", attributes());
    top.recordException(new IllegalStateException("boom"), attributes());
    String bad = null;
    Span child = tracer.spanBuilder("child").setParent(Context.root().with(top)).setAllAttributes(attributes())
        .addLink(top.getSpanContext(), attributes()).startSpan();
    Span grandchild = tracer.spanBuilder("grandchild").setParent(Context.root().with(child)).startSpan();
    if (top.getSpanContext().isSampled() && !(child.getSpanContext().isSampled()
        && grandchild.getSpanContext().isSampled())) {
      bad = "a child of sampled " + root + " wasn't sampled";
    }
    child.addEvent("ce", attributes());
    grandchild.end();
    child.end();
    top.end();
    return bad;
  }

  /** What became of each of {@link #ATTRS}' keys in an exported list. */
  static Map<String, Fate> fates(Attributes exported) {
    Map<String, Object> byName = new LinkedHashMap<>();
    exported.forEach((k, v) -> byName.put(k.getKey(), v));
    Map<String, Fate> out = new LinkedHashMap<>();
    ATTRS.forEach((k, v) -> {
      Object got = byName.get(k);
      out.put(k, got == null ? Fate.DROPPED : got.equals(v) ? Fate.KEPT
          : Redaction.MASK.equals(got) ? Fate.MASKED : Fate.WRONG);
    });
    return out;
  }

  /** The documents of the run whose redaction each list is, in common: none, a mixed one. */
  static Set<Integer> candidates(List<Map<String, Fate>> redactions, List<Attributes> lists) {
    Set<Integer> out = new HashSet<>();
    for (int i = 0; i < redactions.size(); i++) {
      out.add(i);
    }
    for (Attributes a : lists) {
      Map<String, Fate> f = fates(a);
      out.removeIf(i -> !redactions.get(i).equals(f));
    }
    return out;
  }

  static void checkFlipped(Plan plan, List<SpanData> spans, List<LogRecordData> logs) {
    List<Doc> all = plan.all();
    // Each signal is exported only as a document that sends it says: the redactions of those.
    List<Map<String, Fate>> traceRedactions = all.stream().filter(d -> d.sendTraces() != Boolean.FALSE)
        .map(Doc::redaction).toList();
    List<Map<String, Fate>> logRedactions = all.stream().filter(d -> d.sendLogs() != Boolean.FALSE)
        .map(Doc::redaction).toList();
    List<String> wrong = new ArrayList<>();
    java.util.function.BiConsumer<String, Attributes> spanCheck = always(plan, "traces", traceRedactions, wrong);
    java.util.function.BiConsumer<String, Attributes> logCheck = always(plan, "logs", logRedactions, wrong);
    for (SpanData s : spans) {
      List<Attributes> lists = new ArrayList<>();
      lists.add(s.getAttributes());
      s.getEvents().stream().map(EventData::getAttributes).forEach(lists::add);
      // The grandchild has no attributes of its own, nor events or links: nothing to redact.
      s.getLinks().stream().map(LinkData::getAttributes).forEach(lists::add);
      if (s.getName().equals("grandchild")) {
        continue;
      }
      for (Attributes a : lists) {
        spanCheck.accept("span " + s.getName(), a);
      }
      if (candidates(traceRedactions, lists).isEmpty()) {
        wrong.add("span " + s.getName() + " isn't redacted as any one document that sends traces says: "
            + lists.stream().map(ControlsTortureTest::fates).toList());
      }
    }
    for (LogRecordData r : logs) {
      logCheck.accept("log " + r.getInstrumentationScopeInfo().getName(), r.getAttributes());
      if (candidates(logRedactions, List.of(r.getAttributes())).isEmpty()) {
        wrong.add("log record isn't redacted as any one document that sends logs says: " + fates(r.getAttributes()));
      }
    }
    if (plan.logsOff() && !logs.isEmpty()) {
      wrong.add(logs.size() + " log records exported, with the logs export off in every document");
    }
    assertEquals(List.of(), wrong.stream().distinct().limit(20).toList());
  }

  /**
   * The check of a list of the signal's attributes against what all the documents that send it
   * ({@code redactions}) say: a key each of them drops is dropped; one each masks and none drops,
   * masked. A key no document of the run (sending or not) would let through is one of these, and
   * with no document that sends the signal, every key is: then nothing must be exported at all.
   */
  static java.util.function.BiConsumer<String, Attributes> always(Plan plan, String signal,
      List<Map<String, Fate>> redactions, List<String> wrong) {
    Set<String> dropped = new HashSet<>(ATTRS.keySet());
    Set<String> masked = new HashSet<>(ATTRS.keySet());
    for (Map<String, Fate> f : redactions) {
      dropped.removeIf(k -> f.get(k) != Fate.DROPPED);
      masked.removeIf(k -> f.get(k) != Fate.MASKED);
    }
    if (plan.pinned()) {
      assertTrue(dropped.containsAll(Set.of("user.password", "DB.Password.Hash", "x.PASSWORD")), signal);
      assertTrue(redactions.isEmpty() || masked.containsAll(Set.of("secret.token", "Secret.Key")), signal);
    }
    return (where, a) -> {
      Map<String, Fate> f = fates(a);
      for (String k : dropped) {
        if (f.get(k) != Fate.DROPPED) {
          wrong.add(where + ": " + k + " is dropped in every document that sends " + signal + ", exported "
              + f.get(k));
        }
      }
      for (String k : masked) {
        if (f.get(k) != Fate.MASKED && !redactions.isEmpty()) {
          wrong.add(where + ": " + k + " is masked in every document that sends " + signal + ", exported "
              + f.get(k));
        }
      }
    };
  }

  /** After the final document, emitted from this thread: exactly what it says, in order. */
  static void checkLast(Doc d, Controls c, SdkTracerProvider tp, SdkLoggerProvider lp, InMemorySpanExporter spans,
      InMemoryLogRecordExporter logs) {
    Tracer tracer = tp.get("torture");
    Map<String, Fate> redaction = d.redaction();
    List<String> want = new ArrayList<>();
    int traceTokens = Doc.tokens(d.traceCap());
    for (int round = 0; round < 3; round++) {
      for (Root root : ROOTS) {
        boolean sampled = d.samples(root) && traceTokens > 0;
        if (d.samples(root) && d.traceCap() != null) {
          traceTokens--;
        }
        if (sampled && d.sendTraces() != Boolean.FALSE) {
          want.addAll(List.of("grandchild", "child", root.name()));
        }
        assertEquals(null, emitTrace(tracer, root, false));
      }
      // Under a remote sampled parent: followed (or the agent's parent-based sampler), and capped.
      boolean sampled = traceTokens > 0;
      if (d.traceCap() != null) {
        traceTokens--;
      }
      if (sampled && d.sendTraces() != Boolean.FALSE) {
        want.addAll(List.of("grandchild", "child", "job"));
      }
      emitTrace(tracer, ROOTS.get(ROOTS.size() - 1), true);
      // Under a remote unsampled one: never.
      tracer.spanBuilder("unsampled").setParent(Context.root().with(Span.wrap(REMOTE_UNSAMPLED))).startSpan().end();
    }
    List<SpanData> got = spans.getFinishedSpanItems();
    assertEquals(want, got.stream().map(SpanData::getName).toList(), "spans after the final document");
    for (SpanData s : got) {
      List<Attributes> lists = new ArrayList<>();
      if (!s.getName().equals("grandchild")) {
        lists.add(s.getAttributes());
      }
      s.getEvents().forEach(e -> lists.add(e.getAttributes()));
      s.getLinks().forEach(l -> lists.add(l.getAttributes()));
      for (Attributes a : lists) {
        assertEquals(redaction, fates(a), "redaction of " + s.getName() + " after the final document");
      }
    }

    List<String> wantLogs = new ArrayList<>();
    int logTokens = Doc.tokens(d.logCap());
    for (int round = 0; round < 2; round++) {
      for (String scope : SCOPES) {
        for (int sev : SEVERITIES) {
          if (d.sendLogs() != Boolean.FALSE && d.keeps(scope, sev) && logTokens-- > 0) {
            wantLogs.add(scope + ":" + sev);
          }
          lp.get(scope).logRecordBuilder().setSeverity(VectorsTest.severity(sev)).setBody("last")
              .setAllAttributes(attributes()).emit();
        }
      }
    }
    List<LogRecordData> gotLogs = logs.getFinishedLogRecordItems();
    assertEquals(wantLogs, gotLogs.stream()
        .map(r -> r.getInstrumentationScopeInfo().getName() + ":" + r.getSeverity().getSeverityNumber()).toList(),
        "log records after the final document");
    for (LogRecordData r : gotLogs) {
      assertEquals(redaction, fates(r.getAttributes()), "redaction of a record after the final document");
    }
  }
}
