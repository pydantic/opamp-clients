// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import dev.pydantic.opamp.internal.AppSettingsBridge;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicReference;
import java.util.logging.Handler;
import java.util.logging.Level;
import java.util.logging.LogRecord;
import java.util.logging.Logger;
import java.util.stream.Stream;
import org.junit.jupiter.api.DynamicTest;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.TestFactory;

/**
 * The application's side of its own settings (docs/app-settings.md): {@link AppSettings} over a
 * bridge of the test's own, as the extension fills the bootstrap's.
 */
class AppSettingsTest {
  private static final Path CUSTOM = Path.of("../../testdata/sdk-settings/custom.json");

  /** A class loader's state over a bridge of its own. */
  private static AppSettings.State attached(AppSettingsBridge bridge) {
    return AppSettings.State.attach(bridge);
  }

  /** What each listener call got. */
  record Told(Map<String, String> settings, Set<String> changed) {}

  @Test
  void withoutTheExtensionEveryReadIsItsDefault() throws Exception {
    // This JVM has no agent: the class finds no bridge in the bootstrap class loader.
    assertEquals("off", AppSettings.get("example.feature", "off"));
    assertEquals(null, AppSettings.get("example.feature", null));
    assertTrue(AppSettings.getBoolean("b", true));
    assertEquals(7, AppSettings.getLong("n", 7));
    assertEquals(0.5, AppSettings.getDouble("f", 0.5));
    assertEquals(Map.of(), AppSettings.snapshot());
    assertThrows(UnsupportedOperationException.class, () -> AppSettings.snapshot().put("a", "b"));
    AutoCloseable sub = AppSettings.onChange((s, c) -> {});
    sub.close();
    sub.close();
    AppSettings.onChange(null).close();
    assertEquals("d", AppSettings.get(null, "d"));
    assertThrows(ClassNotFoundException.class, () -> Class.forName(AppSettings.BRIDGE, true, null));
  }

  @Test
  void readsBeforeAnyConfigAndAfterOne() {
    AppSettingsBridge bridge = new AppSettingsBridge();
    AppSettings.State s = attached(bridge);
    assertEquals("off", s.get("example.feature", "off"));
    bridge.put(Map.of("example.feature", "on", "limit", "25"));
    assertEquals("on", s.get("example.feature", "off"));
    assertEquals(25, s.getLong("limit", 0));
    assertEquals("x", s.get("other", "x"));
    assertEquals("x", s.get(null, "x"));
    // A config that leaves a key out: its default again.
    bridge.put(Map.of("limit", "25"));
    assertEquals("off", s.get("example.feature", "off"));
  }

  @Test
  void aStateAttachedLaterStartsFromWhatIsInForceAndIsToldOnlyLaterChanges() {
    AppSettingsBridge bridge = new AppSettingsBridge();
    bridge.put(Map.of("a", "1"));
    AppSettings.State s = attached(bridge);
    List<Told> told = new ArrayList<>();
    s.onChange((settings, changed) -> told.add(new Told(settings, changed)));
    assertEquals(Map.of("a", "1"), s.snapshot());
    assertEquals(List.of(), told);
    bridge.put(Map.of("a", "2"));
    assertEquals(List.of(new Told(Map.of("a", "2"), Set.of("a"))), told);
  }

  /**
   * A bridge whose subscribe returns what was in force, with another thread's change put after the
   * receiver was added but before attach stored that return: the interleaving at start, when the
   * application first reads its settings as the server's first config arrives.
   */
  public static final class RacingBridge {
    final AppSettingsBridge real = new AppSettingsBridge();

    public Map<String, String> subscribe(java.util.function.BiConsumer<Map<String, String>, Set<String>> receiver)
        throws InterruptedException {
      Map<String, String> old = real.subscribe(receiver);
      Thread t = new Thread(() -> real.put(Map.of("flag", "on")));
      t.start();
      t.join();
      return old;
    }
  }

  @Test
  void aChangePutWhileAttachingIsNotLost() {
    RacingBridge bridge = new RacingBridge();
    AppSettings.State s = AppSettings.State.attach(bridge);
    assertEquals(Map.of("flag", "on"), bridge.real.current());
    assertEquals(Map.of("flag", "on"), s.snapshot());
    assertEquals("on", s.get("flag", "off"));
  }

  @Test
  void aBridgeOfAnotherClassLoaderIsSpokenToInTheJdksTypes() throws Exception {
    // As in the agent: the bridge's class is another class loader's (here one that sees the JDK
    // alone, as the bootstrap does), and the state reaches it by reflection.
    Path jar = dev.pydantic.opamp.javaagent.AppSettingsSinkAccess.bridgeJar();
    try (java.net.URLClassLoader alone = new java.net.URLClassLoader(new java.net.URL[] {jar.toUri().toURL()}, null)) {
      Class<?> c = Class.forName(AppSettings.BRIDGE, true, alone);
      assertFalse(c == AppSettingsBridge.class);
      Object bridge = c.getConstructor().newInstance();
      AppSettings.State s = AppSettings.State.attach(bridge);
      List<Told> told = new ArrayList<>();
      s.onChange((settings, changed) -> told.add(new Told(settings, changed)));
      c.getMethod("put", Map.class).invoke(bridge, Map.of("example.feature", "on"));
      assertEquals("on", s.get("example.feature", "off"));
      assertEquals(List.of(new Told(Map.of("example.feature", "on"), Set.of("example.feature"))), told);
    }
  }

  @Test
  void anUnusableBridgeIsTheDefaults() {
    AppSettings.State s = AppSettings.State.attach(new Object());
    assertEquals("d", s.get("a", "d"));
    assertEquals(Map.of(), s.snapshot());
  }

  /** custom.json's typed vectors: the same text reads the same in every language. */
  @TestFactory
  Stream<DynamicTest> typedReadsKeepTheSharedVectors() throws Exception {
    JsonNode typed = new ObjectMapper().readTree(CUSTOM.toFile()).get("typed");
    List<DynamicTest> tests = new ArrayList<>();
    for (JsonNode v : typed) {
      String value = v.get("value").asText();
      tests.add(DynamicTest.dynamicTest(new ObjectMapper().writeValueAsString(value), () -> {
        AppSettingsBridge bridge = new AppSettingsBridge();
        AppSettings.State s = attached(bridge);
        bridge.put(Map.of("k", value));
        // Each read twice, with two defaults: a value equal to the default can't pass for one.
        JsonNode b = v.get("bool");
        assertEquals(b.isNull() ? true : b.asBoolean(), s.getBoolean("k", true), "bool");
        assertEquals(b.isNull() ? false : b.asBoolean(), s.getBoolean("k", false), "bool");
        JsonNode i = v.get("int");
        assertEquals(i.isNull() ? 99L : i.asLong(), s.getLong("k", 99L), "int");
        assertEquals(i.isNull() ? -99L : i.asLong(), s.getLong("k", -99L), "int");
        JsonNode f = v.get("float");
        assertEquals(f.isNull() ? 9.5 : f.asDouble(), s.getDouble("k", 9.5), "float");
        assertEquals(f.isNull() ? -9.5 : f.asDouble(), s.getDouble("k", -9.5), "float");
      }));
    }
    assertTrue(tests.size() > 20);
    return tests.stream();
  }

  @Test
  void typedReadsTheirEdges() {
    // ASCII case only: Java's equalsIgnoreCase would take a long s (U+017F) for an s.
    assertTrue(AppSettings.parseBoolean("tRuE", false));
    assertFalse(AppSettings.parseBoolean("faLSE", true));
    assertTrue(AppSettings.parseBoolean("fal\u017Fe", true));
    assertFalse(AppSettings.parseBoolean("true\u0000", false));
    // Only space, tab, CR and LF round a value.
    assertEquals("x", AppSettings.trim(" \t\r\nx\n\r\t "));
    assertEquals("\u000Bx", AppSettings.trim("\u000Bx"));
    assertEquals(3, AppSettings.parseLong("\u000B5", 3));
    assertEquals("", AppSettings.trim("   "));
    // Non-ASCII digits, which Long.parseLong would take.
    assertEquals(-1, AppSettings.parseLong("١٢", -1));
    assertEquals(-1, AppSettings.parseDouble("١", -1));
    assertEquals(-1, AppSettings.parseLong("+", -1));
    assertEquals(-1, AppSettings.parseLong("--1", -1));
    assertEquals(Long.MIN_VALUE, AppSettings.parseLong("-9223372036854775808", -1));
    assertEquals(-1, AppSettings.parseLong("-9223372036854775809", -1));
    // A JSON number, a leading + too; what Double.parseDouble takes beyond that isn't one.
    assertEquals(-1, AppSettings.parseDouble("1d", -1));
    assertEquals(-1, AppSettings.parseDouble("0x1p3", -1));
    assertEquals(-1, AppSettings.parseDouble("01.5", -1));
    assertEquals(-1, AppSettings.parseDouble("-Infinity", -1));
    assertEquals(-1, AppSettings.parseDouble("-1e400", -1));
    assertEquals(0.0, AppSettings.parseDouble("1e-400", -1));
    assertEquals(-0.0, AppSettings.parseDouble("-0", -1));
    assertEquals(150.0, AppSettings.parseDouble("+1.5E+2", -1));
    assertEquals(Double.MAX_VALUE, AppSettings.parseDouble("1.7976931348623157e308", -1));
  }

  @Test
  void aListenerIsToldOncePerChangeAndNeverForTheSameMap() {
    AppSettingsBridge bridge = new AppSettingsBridge();
    AppSettings.State s = attached(bridge);
    List<Told> told = new ArrayList<>();
    s.onChange((settings, changed) -> told.add(new Told(settings, changed)));
    assertEquals(Set.of("a", "b"), bridge.put(Map.of("a", "1", "b", "2")));
    assertEquals(Set.of(), bridge.put(new TreeMap<>(Map.of("b", "2", "a", "1")))); // the same map
    assertEquals(Set.of("b", "c"), bridge.put(Map.of("a", "1", "b", "3", "c", "")));
    assertEquals(Set.of("a", "b", "c"), bridge.put(null)); // every key back to its default
    assertEquals(Set.of(), bridge.put(Map.of()));
    assertEquals(List.of(
        new Told(Map.of("a", "1", "b", "2"), Set.of("a", "b")),
        new Told(Map.of("a", "1", "b", "3", "c", ""), Set.of("b", "c")),
        new Told(Map.of(), Set.of("a", "b", "c"))), told);
  }

  @Test
  void theListenerGetsAnUnmodifiableMapAndSortedKeysAndGetAlreadyReadsTheNewValues() {
    AppSettingsBridge bridge = new AppSettingsBridge();
    AppSettings.State s = attached(bridge);
    AtomicReference<String> inside = new AtomicReference<>();
    AtomicReference<Told> got = new AtomicReference<>();
    s.onChange((settings, changed) -> {
      inside.set(s.get("z", "none"));
      got.set(new Told(settings, changed));
    });
    bridge.put(Map.of("z", "1", "m", "2", "a", "3"));
    assertEquals("1", inside.get());
    assertEquals(List.of("a", "m", "z"), new ArrayList<>(got.get().changed()));
    assertEquals(List.of("a", "m", "z"), new ArrayList<>(got.get().settings().keySet()));
    assertThrows(UnsupportedOperationException.class, () -> got.get().settings().put("b", "1"));
    assertThrows(UnsupportedOperationException.class, () -> got.get().changed().add("b"));
    assertSame(got.get().settings(), s.snapshot());
  }

  @Test
  void aSnapshotIsUnmodifiableAndNoLaterChangeAltersIt() {
    AppSettingsBridge bridge = new AppSettingsBridge();
    AppSettings.State s = attached(bridge);
    Map<String, String> sent = new HashMap<>(Map.of("a", "1"));
    bridge.put(sent);
    sent.put("a", "changed by the sender"); // the bridge took a copy
    Map<String, String> before = s.snapshot();
    bridge.put(Map.of("a", "2", "b", "1"));
    assertEquals(Map.of("a", "1"), before);
    assertEquals(Map.of("a", "2", "b", "1"), s.snapshot());
    assertThrows(UnsupportedOperationException.class, () -> before.remove("a"));
  }

  @Test
  void listenersAreToldInTheOrderTheyWereAdded() {
    AppSettingsBridge bridge = new AppSettingsBridge();
    AppSettings.State s = attached(bridge);
    List<String> order = new ArrayList<>();
    for (String name : List.of("first", "second", "third")) {
      s.onChange((settings, changed) -> order.add(name + ":" + settings.get("k")));
    }
    bridge.put(Map.of("k", "1"));
    bridge.put(Map.of("k", "2"));
    assertEquals(List.of("first:1", "second:1", "third:1", "first:2", "second:2", "third:2"), order);
  }

  @Test
  void aListenerThatThrowsIsLoggedAndTheOthersAreStillTold() {
    AppSettingsBridge bridge = new AppSettingsBridge();
    AppSettings.State s = attached(bridge);
    List<LogRecord> logged = Collections.synchronizedList(new ArrayList<>());
    Logger log = Logger.getLogger(AppSettings.class.getName());
    Handler h = new Handler() {
      @Override
      public void publish(LogRecord r) {
        logged.add(r);
      }

      @Override
      public void flush() {}

      @Override
      public void close() {}
    };
    log.addHandler(h);
    try {
      List<String> told = new ArrayList<>();
      s.onChange((settings, changed) -> told.add("before"));
      s.onChange((settings, changed) -> {
        throw new IllegalStateException("the application's bug");
      });
      s.onChange((settings, changed) -> {
        throw new AssertionError("an error too");
      });
      s.onChange((settings, changed) -> told.add("after"));
      assertEquals(Set.of("a"), bridge.put(Map.of("a", "1")));
      assertEquals(List.of("before", "after"), told);
      assertEquals("1", s.get("a", null));
      List<LogRecord> severe = logged.stream().filter(r -> r.getLevel() == Level.SEVERE).toList();
      assertEquals(2, severe.size());
      assertEquals("the application's bug", severe.get(0).getThrown().getMessage());
      assertTrue(severe.get(0).getMessage().contains("[a]"), severe.get(0).getMessage());
      // And the next change is told as usual.
      bridge.put(Map.of("a", "2"));
      assertEquals(List.of("before", "after", "before", "after"), told);
    } finally {
      log.removeHandler(h);
    }
  }

  @Test
  void closingUnsubscribesOnceAndTheSameListenerAddedTwiceIsTwoSubscriptions() throws Exception {
    AppSettingsBridge bridge = new AppSettingsBridge();
    AppSettings.State s = attached(bridge);
    List<String> told = new ArrayList<>();
    AppSettingsListener l = (settings, changed) -> told.add(settings.get("k"));
    AutoCloseable one = s.onChange(l);
    AutoCloseable two = s.onChange(l);
    bridge.put(Map.of("k", "1"));
    assertEquals(List.of("1", "1"), told);
    one.close();
    one.close(); // again: nothing, and the other stays
    bridge.put(Map.of("k", "2"));
    assertEquals(List.of("1", "1", "2"), told);
    two.close();
    bridge.put(Map.of("k", "3"));
    assertEquals(List.of("1", "1", "2"), told);
  }

  @Test
  void aListenerMaySubscribeAndUnsubscribeWhileItRunsFromTheNextChange() throws Exception {
    AppSettingsBridge bridge = new AppSettingsBridge();
    AppSettings.State s = attached(bridge);
    List<String> told = new ArrayList<>();
    AtomicReference<AutoCloseable> self = new AtomicReference<>();
    self.set(s.onChange((settings, changed) -> {
      told.add("once:" + settings.get("k"));
      try {
        self.get().close();
      } catch (Exception e) {
        throw new IllegalStateException(e);
      }
      s.onChange((settings2, changed2) -> told.add("added:" + settings2.get("k")));
    }));
    AutoCloseable[] later = new AutoCloseable[1];
    s.onChange((settings, changed) -> {
      told.add("second:" + settings.get("k"));
      if (later[0] != null) {
        try {
          later[0].close();
        } catch (Exception e) {
          throw new IllegalStateException(e);
        }
      }
    });
    later[0] = s.onChange((settings, changed) -> told.add("third:" + settings.get("k")));
    bridge.put(Map.of("k", "1"));
    // The third was removed by the second during this change, yet this change's list stands.
    assertEquals(List.of("once:1", "second:1", "third:1"), told);
    told.clear();
    bridge.put(Map.of("k", "2"));
    assertEquals(List.of("second:2", "added:2"), told);
  }

  @Test
  void severalClassLoadersEachHaveTheirOwnAndAreAllTold() {
    // An application server: AppSettings in each web application's class loader, one bridge.
    AppSettingsBridge bridge = new AppSettingsBridge();
    AppSettings.State one = attached(bridge);
    AppSettings.State two = attached(bridge);
    List<String> told = new ArrayList<>();
    one.onChange((settings, changed) -> told.add("one"));
    two.onChange((settings, changed) -> told.add("two"));
    bridge.put(Map.of("a", "1"));
    assertEquals(List.of("one", "two"), told);
    assertEquals("1", two.get("a", null));
  }

  @Test
  void aClassLoaderThatIsGoneIsNoLongerHeld() throws Exception {
    AppSettingsBridge bridge = new AppSettingsBridge();
    java.lang.ref.WeakReference<AppSettings.State> gone = new java.lang.ref.WeakReference<>(attached(bridge));
    for (int i = 0; i < 50 && gone.get() != null; i++) {
      System.gc();
      Thread.sleep(10);
    }
    assertEquals(null, gone.get());
    AppSettings.State kept = attached(bridge);
    List<String> told = new ArrayList<>();
    kept.onChange((settings, changed) -> told.add("kept"));
    bridge.put(Map.of("a", "1"));
    assertEquals(List.of("kept"), told);
  }

  @Test
  void aBadMapIsRefusedWhole() {
    AppSettingsBridge bridge = new AppSettingsBridge();
    bridge.put(Map.of("a", "1"));
    Map<String, String> bad = new HashMap<>();
    bad.put("a", "2");
    bad.put("b", null);
    assertThrows(NullPointerException.class, () -> bridge.put(bad));
    assertEquals(Map.of("a", "1"), bridge.current());
  }

  @Test
  void readersOnOtherThreadsSeeOneWholeMapAtATime() throws Exception {
    AppSettingsBridge bridge = new AppSettingsBridge();
    AppSettings.State s = attached(bridge);
    AtomicBoolean stop = new AtomicBoolean();
    AtomicReference<String> torn = new AtomicReference<>();
    CountDownLatch started = new CountDownLatch(4);
    List<Thread> threads = new ArrayList<>();
    for (int t = 0; t < 4; t++) {
      Thread th = new Thread(() -> {
        started.countDown();
        while (!stop.get()) {
          Map<String, String> m = s.snapshot();
          if (!m.isEmpty() && new java.util.HashSet<>(m.values()).size() != 1) {
            torn.set(m.toString());
          }
          String a = m.get("a");
          String z = m.get("z");
          if (a != null && !a.equals(z)) {
            torn.set(m.toString());
          }
        }
      });
      threads.add(th);
      th.start();
    }
    // Subscribers coming and going meanwhile.
    Thread churn = new Thread(() -> {
      while (!stop.get()) {
        try (AutoCloseable c = s.onChange((settings, changed) -> {
          if (changed.size() != settings.size()) {
            torn.set("changed " + changed + " of " + settings);
          }
        })) {
          Thread.onSpinWait();
        } catch (Exception e) {
          torn.set(e.toString());
        }
      }
    });
    churn.start();
    started.await();
    List<Integer> seen = Collections.synchronizedList(new ArrayList<>());
    s.onChange((settings, changed) -> seen.add(Integer.parseInt(settings.get("a"))));
    for (int gen = 0; gen < 2000; gen++) {
      Map<String, String> m = new HashMap<>();
      for (char k = 'a'; k <= 'z'; k++) {
        m.put(String.valueOf(k), Integer.toString(gen));
      }
      bridge.put(m);
    }
    stop.set(true);
    for (Thread th : threads) {
      th.join();
    }
    churn.join();
    assertEquals(null, torn.get());
    List<Integer> want = new ArrayList<>();
    for (int gen = 0; gen < 2000; gen++) {
      want.add(gen);
    }
    assertEquals(want, seen);
  }

  @Test
  void changesPutFromSeveralThreadsAreToldOneAtATimeEachOnce() throws Exception {
    AppSettingsBridge bridge = new AppSettingsBridge();
    AppSettings.State s = attached(bridge);
    AtomicBoolean inside = new AtomicBoolean();
    AtomicReference<String> wrong = new AtomicReference<>();
    List<Map<String, String>> told = Collections.synchronizedList(new ArrayList<>());
    s.onChange((settings, changed) -> {
      if (!inside.compareAndSet(false, true)) {
        wrong.set("two at once");
      }
      told.add(settings);
      Thread.yield();
      inside.set(false);
    });
    List<Thread> threads = new ArrayList<>();
    for (int t = 0; t < 4; t++) {
      int id = t;
      Thread th = new Thread(() -> {
        for (int i = 0; i < 500; i++) {
          bridge.put(Map.of("k", id + ":" + i));
        }
      });
      threads.add(th);
      th.start();
    }
    for (Thread th : threads) {
      th.join();
    }
    assertEquals(null, wrong.get());
    // Each put a new value, so each was a change, told once; consecutive tellings differ.
    assertEquals(2000, told.size());
    for (int i = 1; i < told.size(); i++) {
      assertFalse(told.get(i).equals(told.get(i - 1)));
    }
    assertEquals(told.get(told.size() - 1), s.snapshot());
  }
}
