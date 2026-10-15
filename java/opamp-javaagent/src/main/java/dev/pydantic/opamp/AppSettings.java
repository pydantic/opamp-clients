// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import java.lang.reflect.Method;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.function.BiConsumer;
import java.util.logging.Level;
import java.util.logging.Logger;
import java.util.regex.Pattern;

/**
 * The application's own settings, as the OpAMP server sends them (docs/app-settings.md): the
 * {@code custom} map of the server's settings document, string keys to string values, which the
 * SDK doesn't read but hands on for the application to read here. A feature flag, a log
 * verbosity, a limit.
 *
 * <pre>{@code
 * if (AppSettings.get("checkout.new_flow", "off").equals("on")) { ... }
 * long limit = AppSettings.getLong("checkout.max_items", 50);
 * AutoCloseable subscription = AppSettings.onChange((settings, changed) -> queue.offer(changed));
 * }</pre>
 *
 * <p>The application compiles against this class alone (the extension's {@code app-settings} jar,
 * with nothing else in it); the Java agent's OpAMP extension fills it. Without the agent, without
 * the extension, without an OpAMP server configured, and until the server sends a config (unless
 * one was saved before a restart), every read is its default and no listener is called.
 *
 * <p>Any thread may read, subscribe and unsubscribe at any time; a reader sees the whole of one
 * map or the whole of the next, never part of each. No method throws, whatever the key.
 */
public final class AppSettings {
  private static final Logger LOG = Logger.getLogger(AppSettings.class.getName());
  /** The class the extension puts on the bootstrap class path, which every class loader reaches. */
  static final String BRIDGE = "dev.pydantic.opamp.internal.AppSettingsBridge";
  private static final State STATE = State.attach(bootstrapBridge());

  private AppSettings() {}

  /**
   * The value of the setting {@code key} in force, or {@code def} while there is none: before the
   * first config, when the config in force leaves the key out, and without the extension or an
   * OpAMP server.
   */
  public static String get(String key, String def) {
    return STATE.get(key, def);
  }

  /**
   * The setting {@code key} as {@code true} or {@code false} in any ASCII case, spaces round it
   * allowed; {@code def} for anything else ({@code on}, {@code 1} and {@code yes} too) and while
   * there is none.
   */
  public static boolean getBoolean(String key, boolean def) {
    return STATE.getBoolean(key, def);
  }

  /**
   * The setting {@code key} as a decimal integer (a sign and leading zeros allowed, spaces round
   * it) in the {@code long} range; {@code def} for anything else and while there is none.
   */
  public static long getLong(String key, long def) {
    return STATE.getLong(key, def);
  }

  /**
   * The setting {@code key} as a number as JSON writes one (a leading {@code +} allowed, spaces
   * round it), finite as a {@code double}; {@code def} for anything else and while there is none.
   */
  public static double getDouble(String key, double def) {
    return STATE.getDouble(key, def);
  }

  /** Every setting in force, as an unmodifiable map no later change alters. Empty while there are none. */
  public static Map<String, String> snapshot() {
    return STATE.snapshot();
  }

  /**
   * Calls {@code listener} after each change to the settings is put in force, with the settings
   * now in force and the keys added, changed or removed. It isn't called for the settings in force
   * when it is added (read those with {@link #snapshot()}), nor when a config leaves them as they
   * were (the same map again, another setting changed, a FAILED config).
   *
   * <p>Changes are told one at a time, in the order they happened, to each listener in the order
   * it was added; {@link #get} already returns the new values inside the listener. It runs on the
   * extension's thread (the OpAMP client's), so it should be quick: hand longer work to the
   * application's own threads. An exception it throws is caught and logged as an error; the other
   * listeners are still called and the config is still APPLIED. A listener may add or remove
   * listeners, itself included, while it runs: that takes effect from the next change.
   *
   * @return closing it removes the listener; closing it again does nothing. It never throws.
   */
  public static AutoCloseable onChange(AppSettingsListener listener) {
    return STATE.onChange(listener);
  }

  /** The bridge the extension put in the bootstrap class loader, or null when there is none. */
  private static Object bootstrapBridge() {
    try {
      return Class.forName(BRIDGE, true, null).getField("SHARED").get(null);
    } catch (ClassNotFoundException e) {
      return null; // no agent, or no extension: the defaults
    } catch (ReflectiveOperationException | RuntimeException | LinkageError e) {
      LOG.log(Level.WARNING, "OpAMP: can't read the application settings the extension holds; every read is its default", e);
      return null;
    }
  }

  /**
   * One class loader's view of the settings: what the static methods read. A bridge's receiver,
   * held weakly by the bridge and strongly by {@link #STATE}.
   */
  static final class State implements BiConsumer<Map<String, String>, Set<String>> {
    private volatile Map<String, String> values = Collections.emptyMap();
    private final List<Subscription> subs = new CopyOnWriteArrayList<>();
    /** Held while this one's listeners are told: one change at a time. */
    private final Object telling = new Object();
    /** Whether the bridge has told this one a change (under {@link #telling}): newer than what subscribe returned. */
    private boolean told;

    /** A state that reads {@code bridge} (an {@code AppSettingsBridge}, from any class loader), or the defaults when null. */
    static State attach(Object bridge) {
      State s = new State();
      if (bridge == null) {
        return s;
      }
      try {
        Method subscribe = bridge.getClass().getMethod("subscribe", BiConsumer.class);
        @SuppressWarnings("unchecked")
        Map<String, String> now = (Map<String, String>) subscribe.invoke(bridge, s);
        synchronized (s.telling) {
          // A change put once the receiver was added, before this, is already in force here and newer.
          if (!s.told) {
            s.values = now;
          }
        }
      } catch (ReflectiveOperationException | RuntimeException | LinkageError e) {
        LOG.log(Level.WARNING, "OpAMP: can't read the application settings the extension holds; every read is its default", e);
      }
      return s;
    }

    String get(String key, String def) {
      String v = key == null ? null : values.get(key);
      return v == null ? def : v;
    }

    boolean getBoolean(String key, boolean def) {
      String v = trimmed(key);
      return v == null ? def : parseBoolean(v, def);
    }

    long getLong(String key, long def) {
      String v = trimmed(key);
      return v == null ? def : parseLong(v, def);
    }

    double getDouble(String key, double def) {
      String v = trimmed(key);
      return v == null ? def : parseDouble(v, def);
    }

    private String trimmed(String key) {
      String v = get(key, null);
      return v == null ? null : trim(v);
    }

    Map<String, String> snapshot() {
      return values;
    }

    AutoCloseable onChange(AppSettingsListener listener) {
      if (listener == null) {
        return () -> {};
      }
      Subscription sub = new Subscription(listener);
      subs.add(sub);
      // By identity: the same listener added twice is two subscriptions, each closed on its own.
      return () -> subs.remove(sub);
    }

    /** The bridge's call: a change put in force, told to each listener on this (the extension's) thread. */
    @Override
    public void accept(Map<String, String> settings, Set<String> changed) {
      synchronized (telling) {
        // The bridge's map: unmodifiable, and never changed once in force.
        Map<String, String> now = settings;
        values = now;
        told = true;
        for (Subscription sub : subs) { // a snapshot: added or removed meanwhile, from the next change
          try {
            sub.listener.changed(now, changed);
          } catch (Throwable t) {
            if (t instanceof VirtualMachineError vm) {
              throw vm;
            }
            LOG.log(Level.SEVERE, "OpAMP: an application settings listener threw; the others are still told, "
                + "and the settings are in force (changed: " + changed + ")", t);
          }
        }
      }
    }
  }

  /** A listener as added: each addition its own. */
  private static final class Subscription {
    final AppSettingsListener listener;

    Subscription(AppSettingsListener listener) {
      this.listener = listener;
    }
  }

  // The typed reads: the same text reads the same in every language (custom.json's typed vectors).

  /** The spaces a typed read takes round a value, and only these. */
  static String trim(String v) {
    int start = 0;
    int end = v.length();
    while (start < end && isSpace(v.charAt(start))) {
      start++;
    }
    while (end > start && isSpace(v.charAt(end - 1))) {
      end--;
    }
    return v.substring(start, end);
  }

  private static boolean isSpace(char c) {
    return c == ' ' || c == '\t' || c == '\r' || c == '\n';
  }

  /** {@code true} or {@code false} in any ASCII case (equalsIgnoreCase folds more: "ſ" is no "s"), else def. */
  static boolean parseBoolean(String v, boolean def) {
    String lower = asciiLower(v);
    if (lower.equals("true")) {
      return true;
    }
    if (lower.equals("false")) {
      return false;
    }
    return def;
  }

  private static String asciiLower(String v) {
    char[] c = v.toCharArray();
    for (int i = 0; i < c.length; i++) {
      if (c[i] >= 'A' && c[i] <= 'Z') {
        c[i] = (char) (c[i] + ('a' - 'A'));
      }
    }
    return new String(c);
  }

  private static final Pattern INT = Pattern.compile("[+-]?[0-9]+");
  /** A JSON number, a leading + too. */
  private static final Pattern FLOAT = Pattern.compile("[+-]?(?:0|[1-9][0-9]*)(?:\\.[0-9]+)?(?:[eE][+-]?[0-9]+)?");

  /** Decimal digits with an optional sign in the long range (no _, no hex, ASCII digits only), else def. */
  static long parseLong(String v, long def) {
    if (!INT.matcher(v).matches()) {
      return def;
    }
    try {
      return Long.parseLong(v);
    } catch (NumberFormatException e) {
      return def;
    }
  }

  /** A JSON number with an optional leading +, finite as a double, else def. */
  static double parseDouble(String v, double def) {
    if (!FLOAT.matcher(v).matches()) {
      return def;
    }
    try {
      double d = Double.parseDouble(v);
      return Double.isFinite(d) ? d : def;
    } catch (NumberFormatException e) {
      return def;
    }
  }
}
