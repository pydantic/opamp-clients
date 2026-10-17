// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.lang.instrument.Instrumentation;
import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Method;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Map;
import java.util.Set;
import java.util.jar.JarEntry;
import java.util.jar.JarFile;
import java.util.jar.JarOutputStream;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Where the extension puts the application's own settings ({@code custom}, docs/app-settings.md)
 * for {@code dev.pydantic.opamp.AppSettings} to read: an {@code AppSettingsBridge}, which may be
 * the bootstrap class loader's (in the agent) or any other's (in tests), spoken to in the JDK's
 * types only.
 *
 * <p>Why the bootstrap: the agent loads this extension in a class loader of its own, out of the
 * application's sight, and the application's class loaders are out of the extension's. The one
 * class loader both reach is the bootstrap, which the agent itself puts its shared classes in
 * ({@code io.opentelemetry.javaagent.bootstrap}). So at start the extension writes the bridge
 * class (one class, JDK types only) into a jar of its own and appends that to the bootstrap class
 * path, through the {@link Instrumentation} the agent keeps ({@code InstrumentationHolder}); the
 * application's {@code AppSettings} finds it with {@code Class.forName(name, true, null)},
 * whatever its own class loader delegates to.
 */
final class AppSettingsSink {
  private static final Logger LOG = Logger.getLogger(Extension.class.getName());
  static final String BRIDGE = "dev.pydantic.opamp.internal.AppSettingsBridge";
  private static final String HOLDER = "io.opentelemetry.javaagent.bootstrap.InstrumentationHolder";

  private final Object bridge;
  private final Method put;
  private final Method current;

  /** {@code bridge}: an {@code AppSettingsBridge} from any class loader. */
  AppSettingsSink(Object bridge) throws NoSuchMethodException {
    this.bridge = bridge;
    this.put = bridge.getClass().getMethod("put", Map.class);
    this.current = bridge.getClass().getMethod("current");
  }

  /**
   * Puts {@code values} in force and tells the application's listeners, on this thread, if they
   * changed anything. Returns the keys added, changed or removed (empty: none).
   */
  @SuppressWarnings("unchecked")
  Set<String> put(Map<String, String> values) {
    return (Set<String>) call(put, values);
  }

  /** The values in force. */
  @SuppressWarnings("unchecked")
  Map<String, String> current() {
    return (Map<String, String>) call(current);
  }

  private Object call(Method m, Object... args) {
    try {
      return m.invoke(bridge, args);
    } catch (InvocationTargetException e) {
      if (e.getCause() instanceof RuntimeException r) {
        throw r;
      }
      if (e.getCause() instanceof Error err) {
        throw err;
      }
      throw new IllegalStateException(e.getCause());
    } catch (IllegalAccessException e) {
      throw new IllegalStateException(e);
    }
  }

  /**
   * The bootstrap's bridge, put there first if it isn't yet; null (logged) when it can't be: then
   * the application reads its defaults, and the extension does not declare {@code custom}. So a
   * policy that overrides app_settings is refused, not answered APPLIED for nothing.
   */
  static AppSettingsSink install() {
    try {
      Class<?> c;
      try {
        c = Class.forName(BRIDGE, true, null);
      } catch (ClassNotFoundException absent) {
        Object inst = Class.forName(HOLDER, true, null).getMethod("getInstrumentation").invoke(null);
        if (!(inst instanceof Instrumentation instrumentation)) {
          throw new IllegalStateException("the agent keeps no Instrumentation");
        }
        instrumentation.appendToBootstrapClassLoaderSearch(new JarFile(bridgeJar().toFile()));
        c = Class.forName(BRIDGE, true, null);
      }
      return new AppSettingsSink(c.getField("SHARED").get(null));
    } catch (Exception | LinkageError e) {
      LOG.log(Level.WARNING, "OpAMP: can't hand the application its own settings (dev.pydantic.opamp.AppSettings "
          + "reads its defaults); a policy that overrides app_settings is refused", e);
      return null;
    }
  }

  /**
   * A jar holding the bridge class alone, as this extension's jar holds it: in a directory of the
   * extension's own (readable by this user only), removed when the JVM exits.
   */
  static Path bridgeJar() throws IOException {
    String entry = BRIDGE.replace('.', '/') + ".class";
    Path dir = Files.createTempDirectory("opamp-app-settings");
    dir.toFile().deleteOnExit();
    Path jar = dir.resolve("opamp-app-settings-bridge.jar");
    try (InputStream in = AppSettingsSink.class.getClassLoader().getResourceAsStream(entry)) {
      if (in == null) {
        throw new IOException(entry + " isn't in the extension's jar");
      }
      try (OutputStream file = Files.newOutputStream(jar); JarOutputStream out = new JarOutputStream(file)) {
        out.putNextEntry(new JarEntry(entry));
        in.transferTo(out);
        out.closeEntry();
      }
    }
    jar.toFile().deleteOnExit();
    return jar;
  }
}
