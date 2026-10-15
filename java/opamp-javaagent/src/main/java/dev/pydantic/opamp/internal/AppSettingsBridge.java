// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp.internal;

import java.lang.ref.WeakReference;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.Set;
import java.util.SortedMap;
import java.util.SortedSet;
import java.util.TreeMap;
import java.util.TreeSet;
import java.util.function.BiConsumer;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Where the extension and the application meet: the application's own settings in force (the
 * {@code custom} map of the server's settings document, docs/app-settings.md), and the
 * {@link dev.pydantic.opamp.AppSettings} of each class loader that reads them. Not an API: the
 * application uses {@code AppSettings}.
 *
 * <p>The agent's extension classes are in a class loader of the agent's own, which the
 * application's can't see, and the application's are out of the extension's sight too. So the
 * extension puts this one class on the bootstrap class path, which every class loader reaches
 * ({@code Class.forName(name, true, null)}), and both sides speak to it in the JDK's own types
 * only: a map, a set and a {@link BiConsumer}. That is also why this is one class, with no nested
 * class or lambda of its own and nothing but the JDK in its signatures: it is the only one put
 * there.
 *
 * <p>Each {@code AppSettings} (one per class loader that has the type: several in an application
 * server) subscribes once, as it is loaded, and is held weakly: a web application undeployed takes
 * its class loader with it.
 */
public final class AppSettingsBridge {
  /** The one the extension fills and every {@code AppSettings} reads, in the bootstrap's copy. */
  public static final AppSettingsBridge SHARED = new AppSettingsBridge();

  /** Guards {@link #values} and {@link #receivers}; never held while a receiver runs. */
  private final Object lock = new Object();
  /** Held while a change is put in force and told: one at a time, in order. */
  private final Object telling = new Object();
  private SortedMap<String, String> values = Collections.unmodifiableSortedMap(new TreeMap<>());
  /** Never changed once stored: a change being told goes on with the list it took. */
  private List<WeakReference<BiConsumer<Map<String, String>, Set<String>>>> receivers = List.of();

  /** A bridge of its own, for tests; the extension and the application use {@link #SHARED}. */
  public AppSettingsBridge() {}

  /**
   * Adds {@code receiver}, which is called after each change with the map in force (unmodifiable)
   * and the keys added, changed or removed (sorted, unmodifiable), on the thread that put it in
   * force. Returns the map in force now, which it isn't called for: from then on it misses no change
   * and is told none twice.
   */
  public Map<String, String> subscribe(BiConsumer<Map<String, String>, Set<String>> receiver) {
    Objects.requireNonNull(receiver, "receiver");
    synchronized (lock) {
      List<WeakReference<BiConsumer<Map<String, String>, Set<String>>>> next = new ArrayList<>(receivers.size() + 1);
      for (WeakReference<BiConsumer<Map<String, String>, Set<String>>> r : receivers) {
        if (r.get() != null) {
          next.add(r);
        }
      }
      next.add(new WeakReference<>(receiver));
      receivers = List.copyOf(next);
      return values;
    }
  }

  /** The map in force: unmodifiable, and no later change alters it. */
  public Map<String, String> current() {
    synchronized (lock) {
      return values;
    }
  }

  /**
   * Puts {@code next} in force (null: none) and, if it differs from the map in force, tells every
   * receiver, in the order they subscribed, on this thread, before it returns. Returns the keys
   * added, changed or removed (sorted; empty: nothing changed, and no one was told).
   */
  public Set<String> put(Map<String, String> next) {
    SortedMap<String, String> now = new TreeMap<>();
    if (next != null) {
      for (Map.Entry<String, String> e : next.entrySet()) {
        now.put(Objects.requireNonNull(e.getKey(), "key"), Objects.requireNonNull(e.getValue(), "value"));
      }
    }
    synchronized (telling) {
      SortedSet<String> changed = new TreeSet<>();
      List<WeakReference<BiConsumer<Map<String, String>, Set<String>>>> told;
      SortedMap<String, String> settings;
      synchronized (lock) {
        for (Map.Entry<String, String> e : values.entrySet()) {
          if (!e.getValue().equals(now.get(e.getKey()))) {
            changed.add(e.getKey());
          }
        }
        for (String k : now.keySet()) {
          if (!values.containsKey(k)) {
            changed.add(k);
          }
        }
        if (changed.isEmpty()) {
          return Collections.emptySortedSet();
        }
        settings = Collections.unmodifiableSortedMap(now);
        values = settings;
        told = receivers;
      }
      Set<String> keys = Collections.unmodifiableSortedSet(changed);
      for (WeakReference<BiConsumer<Map<String, String>, Set<String>>> r : told) {
        BiConsumer<Map<String, String>, Set<String>> receiver = r.get();
        if (receiver == null) {
          continue;
        }
        try {
          receiver.accept(settings, keys);
        } catch (RuntimeException | Error e) {
          // A receiver catches what the application's callbacks throw: this is its own bug, and
          // the others are still told.
          Logger.getLogger("dev.pydantic.opamp.AppSettings").log(Level.SEVERE,
              "OpAMP: telling the application its settings changed", e);
        }
      }
      return keys;
    }
  }
}
