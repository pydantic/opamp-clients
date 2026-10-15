// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import dev.pydantic.opamp.core.CoreProto;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.SplittableRandom;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CyclicBarrier;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.jupiter.api.Test;

/**
 * {@link InFlight}'s invariants over random schedules (a seeded generator; OPAMP_PROP_SEED replays
 * one): a late result is dropped, a late resource is closed, nothing registers after close, every
 * aborted effect is aborted once, and close completes when the tasks are over.
 */
class InFlightTest {
  private static final int RUNS = Integer.getInteger("opamp.prop.runs", 300);

  /** One effect's record: how often it was aborted, and its resource closed. */
  static final class Effect {
    final AtomicInteger aborted = new AtomicInteger();
    final AtomicInteger closed = new AtomicInteger();
    final CompletableFuture<Void> task = new CompletableFuture<>();
    boolean resource;
  }

  static CoreProto.Input result(long id, int kind) {
    CoreProto.Input.Builder in = CoreProto.Input.newBuilder();
    return switch (kind) {
      case 0 -> in.setHttpResponse(CoreProto.HttpResponse.newBuilder().setId(id)).build();
      case 1 -> in.setWsMessage(CoreProto.WsMessage.newBuilder().setId(id)).build();
      case 2 -> in.setWsClosed(CoreProto.WsClosed.newBuilder().setId(id)).build();
      case 3 -> in.setStored(CoreProto.Id.newBuilder().setId(id)).build();
      default -> in.setWsOpened(CoreProto.Id.newBuilder().setId(id)).build();
    };
  }

  @Test
  void invariantsHoldOverRandomSchedules() {
    String fixed = System.getenv("OPAMP_PROP_SEED");
    long base = fixed != null ? Long.parseLong(fixed) : new SplittableRandom().nextLong();
    for (int run = 0; run < (fixed != null ? 1 : RUNS); run++) {
      long seed = base + run;
      try {
        schedule(new SplittableRandom(seed));
      } catch (AssertionError e) {
        throw new AssertionError("seed " + seed + " (OPAMP_PROP_SEED=" + seed + "): " + e.getMessage(), e);
      }
    }
  }

  /** One schedule, checked against a model of which entries are present. */
  private static void schedule(SplittableRandom r) {
    InFlight f = new InFlight();
    Map<Long, Effect> effects = new HashMap<>();
    Set<Long> present = new HashSet<>();
    Set<Long> closingIds = new HashSet<>();
    Set<Long> ended = new HashSet<>(); // by a last result: never aborted
    boolean closed = false;
    CompletableFuture<Void> over = null;
    long next = 1;
    for (int step = 0, n = 5 + r.nextInt(40); step < n; step++) {
      int op = r.nextInt(7);
      List<Long> ids = new ArrayList<>(effects.keySet());
      long id = ids.isEmpty() || r.nextInt(4) == 0 ? next : ids.get(r.nextInt(ids.size()));
      switch (op) {
        case 0 -> { // the core starts an effect
          long nid = next++;
          Effect e = new Effect();
          effects.put(nid, e);
          boolean added = f.add(nid, e.aborted::incrementAndGet, e.task);
          assertEquals(!closed, added, "registers only before close");
          if (added) {
            present.add(nid);
          } else {
            assertEquals(1, e.aborted.get(), "added after close: aborted at once");
          }
        }
        case 1 -> { // a resource becomes ready
          Effect e = effects.get(id);
          if (e == null || e.resource) {
            continue;
          }
          e.resource = true;
          boolean kept = f.attach(id, e.closed::incrementAndGet);
          assertEquals(present.contains(id), kept);
          assertEquals(kept ? 0 : 1, e.closed.get(), "a late resource is closed at once");
        }
        case 2 -> { // a result comes
          int kind = r.nextInt(5);
          boolean admitted = f.admit(result(id, kind));
          assertEquals(present.contains(id), admitted, "a result goes only while its entry is there");
          boolean last = kind == 0 || kind == 3 || (kind == 2 && closingIds.contains(id));
          if (admitted && last) {
            present.remove(id);
            ended.add(id);
          }
        }
        case 3 -> { // the core closes it
          f.closing(id);
          if (present.contains(id)) {
            closingIds.add(id);
          }
        }
        case 4 -> { // the core cancels it
          boolean was = present.remove(id);
          f.cancel(id);
          Effect e = effects.get(id);
          if (e != null && was) {
            assertEquals(1, e.aborted.get(), "a cancel aborts it");
            assertEquals(e.resource ? 1 : 0, e.closed.get(), "and closes its resource");
          }
          assertFalse(f.admit(result(id, r.nextInt(5))), "nothing after a cancel");
        }
        case 5 -> { // a task finishes
          Effect e = effects.get(id);
          if (e != null) {
            e.task.complete(null);
          }
        }
        default -> { // the client ends
          if (!closed) {
            closed = true;
            over = f.close();
            for (long p : present) {
              assertEquals(1, effects.get(p).aborted.get(), "close aborts what is in flight");
            }
            present.clear();
          }
        }
      }
    }
    for (Map.Entry<Long, Effect> e : effects.entrySet()) {
      assertTrue(e.getValue().aborted.get() <= 1, "aborted once at most");
      assertTrue(e.getValue().closed.get() <= 1, "closed once at most");
      if (ended.contains(e.getKey())) {
        assertEquals(0, e.getValue().aborted.get(), "an effect that ended with its result isn't aborted");
      }
    }
    if (over != null) {
      // Close is over when the tasks it waits for are.
      effects.values().forEach(e -> e.task.complete(null));
      assertTrue(over.isDone());
    }
  }

  /** A resource ready on one thread as the effect is cancelled on another: closed exactly once. */
  @Test
  void aResourceRacingItsCancelIsClosedOnce() throws Exception {
    SplittableRandom r = new SplittableRandom(Long.getLong("opamp.prop.seed", 42));
    for (int i = 0; i < 200; i++) {
      InFlight f = new InFlight();
      Effect e = new Effect();
      f.add(1, e.aborted::incrementAndGet, e.task);
      CyclicBarrier go = new CyclicBarrier(2);
      boolean closeInstead = r.nextBoolean();
      int spin = r.nextInt(200);
      CompletableFuture<Boolean> attached = CompletableFuture.supplyAsync(() -> {
        await(go);
        for (int s = 0; s < spin; s++) {
          Thread.onSpinWait();
        }
        return f.attach(1, e.closed::incrementAndGet);
      });
      await(go);
      if (closeInstead) {
        f.close();
      } else {
        f.cancel(1);
      }
      attached.get(10, TimeUnit.SECONDS);
      assertEquals(1, e.closed.get(), "the resource is closed once, whoever came first");
      assertEquals(1, e.aborted.get());
      assertFalse(f.admit(result(1, 4)), "never reported opened");
    }
  }

  private static void await(CyclicBarrier b) {
    try {
      b.await(10, TimeUnit.SECONDS);
    } catch (Exception ex) {
      throw new IllegalStateException(ex);
    }
  }
}
