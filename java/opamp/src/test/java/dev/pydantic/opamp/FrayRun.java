// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.SplittableRandom;
import java.util.function.LongConsumer;
import kotlin.Unit;
import org.pastalab.fray.core.TestRunner;
import org.pastalab.fray.core.command.Configuration;
import org.pastalab.fray.core.command.ExecutionInfo;
import org.pastalab.fray.core.command.LambdaExecutor;
import org.pastalab.fray.core.command.NetworkDelegateType;
import org.pastalab.fray.core.command.SystemTimeDelegateType;
import org.pastalab.fray.core.observers.TimelineCoverageType;
import org.pastalab.fray.core.randomness.ControlledRandomProvider;
import org.pastalab.fray.core.randomness.Randomness;
import org.pastalab.fray.core.randomness.RecordedRandomProvider;
import org.pastalab.fray.core.scheduler.POSScheduler;
import org.pastalab.fray.core.scheduler.Scheduler;
import org.junit.jupiter.api.Assumptions;

/**
 * Fray's engine (the one behind {@code @ExtendWith(FrayTestExtension.class)}), driven from a plain
 * test so that the number of schedules and the replay come from the environment rather than from
 * an annotation: each scenario explores {@code OPAMP_FRAY_RUNS} schedules (POS, Fray's default),
 * each with a plan of its own from a {@link SplittableRandom} seed, over threads only (no network:
 * {@link FakeHttp}), on Fray's mocked clock (time jumps to the next deadline when every thread
 * waits). Must run in Fray's JVM (the fray profile: {@code mvn -Pfray test}).
 *
 * <ul>
 *   <li>{@code OPAMP_FRAY_RUNS}: schedules per scenario (default: the scenario's own, small).
 *   <li>{@code OPAMP_FRAY_BUDGET_S}: seconds a scenario may explore before it stops, passing, with
 *       fewer schedules (default: the scenario's own).
 *   <li>{@code OPAMP_FRAY_SEED}: the first plan seed (default: from the clock, printed); schedule
 *       {@code i} uses that seed plus {@code i}.
 *   <li>{@code OPAMP_FRAY_REPLAY}: a recording a failure printed (a directory {@code
 *       target/fray/<scenario>/recording}); with {@code OPAMP_FRAY_SEED} set to the failure's plan
 *       seed, the scenario runs that one schedule again. Other scenarios are skipped.
 * </ul>
 */
final class FrayRun {
  private FrayRun() {}

  /** What the schedules reached (a scenario counts its events here): printed after the run. */
  private static final java.util.concurrent.ConcurrentHashMap<String, java.util.concurrent.atomic.AtomicInteger> SAW =
      new java.util.concurrent.ConcurrentHashMap<>();

  /** A schedule reached {@code event} (so the summary shows what the run explored). */
  static void saw(String event) {
    SAW.computeIfAbsent(event, k -> new java.util.concurrent.atomic.AtomicInteger()).incrementAndGet();
  }

  /** The plan seed of the schedule running now (the last one, once Fray has stopped on a failure). */
  private static volatile long current;

  /**
   * Explore {@code scenario}: Fray runs {@code body} with a plan seed, once per schedule, until a
   * schedule fails (an exception in any thread, a deadlock) or {@code runs} have passed or {@code
   * budgetS} seconds have gone. A failure throws, naming the seed and the recording to replay.
   */
  static void explore(String scenario, int runs, int budgetS, LongConsumer body) throws IOException {
    String replay = env("OPAMP_FRAY_REPLAY");
    Path base = Path.of(System.getProperty("fray.workDir", "target/fray/fray-report")).toAbsolutePath().getParent();
    Path report = base.resolve(scenario);
    boolean replaying = replay != null;
    if (replaying) {
      Assumptions.assumeTrue(Path.of(replay).toAbsolutePath().normalize().startsWith(report),
          "OPAMP_FRAY_REPLAY is another scenario's: " + replay);
      if (env("OPAMP_FRAY_SEED") == null) {
        throw new IllegalArgumentException("OPAMP_FRAY_REPLAY needs OPAMP_FRAY_SEED, the failure's plan seed");
      }
    }
    String seedEnv = env("OPAMP_FRAY_SEED");
    long first = seedEnv != null ? Long.parseLong(seedEnv) : new SplittableRandom().nextLong();
    String runsEnv = env("OPAMP_FRAY_RUNS");
    int n = replaying ? 1 : runsEnv != null ? Integer.parseInt(runsEnv) : runs;
    String budgetEnv = env("OPAMP_FRAY_BUDGET_S");
    int budget = budgetEnv != null ? Integer.parseInt(budgetEnv) : budgetS;
    Scheduler scheduler;
    if (replaying) {
      // A copy: Fray clears the report directory it is given.
      Path copy = Files.createTempDirectory("fray-replay");
      for (String f : new String[] {"scheduler", "random.json"}) {
        Files.copy(Path.of(replay, f), copy.resolve(f));
      }
      Randomness recorded = new RecordedRandomProvider(copy.resolve("random.json").toString()).getRandomness();
      try {
        scheduler = (Scheduler) Class.forName(Files.readString(copy.resolve("scheduler")).trim())
            .getConstructor(Randomness.class).newInstance(recorded);
      } catch (ReflectiveOperationException e) {
        throw new IllegalStateException("can't replay " + replay, e);
      }
    } else {
      scheduler = new POSScheduler();
    }
    System.out.println("FrayRun " + scenario + ": " + n + " schedule(s), plan seeds from " + first
        + (replaying ? ", replaying " + replay : "") + " (OPAMP_FRAY_SEED=" + first + ")");
    long[] next = {first};
    Configuration config = new Configuration(
        new ExecutionInfo(new LambdaExecutor(() -> {
          long seed = next[0]++;
          current = seed;
          body.accept(seed);
          return Unit.INSTANCE;
        }), false, false, -1),
        report,
        n,
        budget,
        scheduler,
        new ControlledRandomProvider(),
        true, // fullSchedule
        false, // exploreMode: stop at the first failure
        true, // noExitWhenBugFound
        replaying,
        false, // noFray
        false, // dummyRun
        NetworkDelegateType.NONE,
        SystemTimeDelegateType.MOCK,
        // Virtual ns per clock read (Fray reads it at every scheduling step too): the least, so that
        // a long computation costs no time to speak of (the core, interpreted under Fray, is
        // millions of steps on a cold JVM: at 0.1 ms each, a first step "took" seconds and stop
        // ran past its bound). Time moves by jumping to the next deadline when all threads wait.
        1L,
        false, // ignoreTimedBlock: timed waits end at their deadline, on the mocked clock
        false, // sleepAsYield
        false, // resetClassLoader
        false, // redirectStdout
        false, // abortThreadExecutionAfterMainExit: every thread must end
        false,
        TimelineCoverageType.RESOURCE_ORDERING);
    SAW.clear();
    long t0 = System.nanoTime();
    Throwable failure = new TestRunner(config).run();
    long ms = (System.nanoTime() - t0) / 1_000_000;
    System.out.println("FrayRun " + scenario + ": " + (config.getCurrentIteration() + (failure == null ? 0 : 1))
        + " schedule(s) in " + ms + " ms" + (failure == null ? "" : ", failed") + "; reached " + new java.util.TreeMap<>(SAW));
    if (failure != null) {
      long seed = current;
      throw new AssertionError(scenario + " failed on plan seed " + seed + ": " + failure
          + "\n  Fray's report: " + report.resolve("fray.log")
          + "\n  replay: OPAMP_FRAY_SEED=" + seed + " OPAMP_FRAY_REPLAY=" + report.resolve("recording")
          + " mvn -Pfray test -Dfray.only=true -Dfray.test=" + scenario.replace('.', '#'), failure);
    }
  }

  private static String env(String name) {
    String v = System.getenv(name);
    return v == null || v.isEmpty() ? null : v;
  }
}
