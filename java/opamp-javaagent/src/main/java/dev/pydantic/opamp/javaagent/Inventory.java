// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp.javaagent;

import dev.pydantic.opamp.sdk.SdkSettings;
import dev.pydantic.opamp.sdk.SettingsProto;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.javaagent.extension.instrumentation.InstrumentationModule;
import io.opentelemetry.javaagent.extension.instrumentation.TypeInstrumentation;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import io.opentelemetry.sdk.resources.Resource;
import java.io.IOException;
import java.io.InputStream;
import java.util.ArrayList;
import java.util.Collection;
import java.util.Collections;
import java.util.HashMap;
import java.util.HashSet;
import java.util.IdentityHashMap;
import java.util.Iterator;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Properties;
import java.util.ServiceConfigurationError;
import java.util.ServiceLoader;
import java.util.Set;
import java.util.TreeMap;
import java.util.WeakHashMap;
import java.util.function.Supplier;
import java.util.logging.Level;
import java.util.logging.Logger;
import net.bytebuddy.matcher.ElementMatcher;

/**
 * What the agent runs, for the layer's Components call (docs/available-components.md): the
 * extension's package and version, the SDK's, the agent's instrumentations under the names
 * {@code deactivate_instrumentations} takes, and the exporters the SDK was built with. Thread-safe.
 */
final class Inventory {
  /** The extension's package, as published: its Maven coordinates. */
  static final String PACKAGE = "dev.pydantic:opamp-javaagent-extension";
  /** Its version, as Maven built it ({@code version.properties}); empty if the jar doesn't say. */
  static final String VERSION = property(Inventory.class.getResourceAsStream("version.properties"));
  private static final Logger LOG = Logger.getLogger(Extension.class.getName());
  private static final AttributeKey<String> SDK_LANGUAGE = AttributeKey.stringKey("telemetry.sdk.language");
  private static final AttributeKey<String> SDK_VERSION = AttributeKey.stringKey("telemetry.sdk.version");

  /** The exporters the SDK was built with, by signal, as found (the customizers or the model). */
  private final Map<String, List<SettingsProto.FoundExporter>> exporters = new TreeMap<>();
  /** What another extension added past the gates, by signal ({@link #outside}). */
  private final Map<String, List<SettingsProto.FoundExporter>> outside = new TreeMap<>();
  /** The {@code OTEL_EXPORTER_*} settings autoconfigure read, by their variables' names ({@link #configured}). */
  private final Map<String, String> env = new TreeMap<>();
  /** What decides the listed exporters from the found ones: the extension's layer. */
  Supplier<SdkSettings> layer = Inventory::sharedLayer;
  /** Each change to what was found, and the change {@link #decided} was decided at. */
  private long changes;
  private long decidedAt = -1;
  private List<SettingsProto.Exporter> decided = List.of();
  /** Each exporter as the layer last listed it, by what was found: should a call fail, it keeps its name. */
  private final Map<SettingsProto.FoundExporter, SettingsProto.Exporter> listedAs = new HashMap<>();
  /** The layer's warnings said so far: each once. */
  private final Set<String> warned = new HashSet<>();
  /**
   * The instrumentations listed: those that apply ({@link #applying}), the agent's
   * InstrumentationModules read once, at start.
   */
  Supplier<List<SettingsProto.Instrumentation>> instrumentations = this::applying;
  /** The agent's instrumentation modules (the agent's own and any extension's). */
  Supplier<List<InstrumentationModule>> modulesFrom = () -> modules(Inventory.class.getClassLoader());
  /** The class loaders an application's library is looked for in ({@link #loaders()}). */
  Supplier<Collection<ClassLoader>> loaders = Inventory::loaders;
  /** The modules, once read, each with its matchers; and which apply. */
  private List<Module> modules;
  /** The class loaders looked in already: a module's library is looked for once in each. */
  private final Set<ClassLoader> looked = Collections.newSetFromMap(new WeakHashMap<>());

  /**
   * Under autoconfigure, from the customizer an exporter of {@code signal} ({@code traces},
   * {@code metrics} or {@code logs}) went through: the SDK builds one for each name
   * {@code otel.<signal>.exporter} gives ({@code otlp} when unset; {@code none}: none), and every
   * one of them is in the extension's gate. Each with where it sends ({@link #destination}).
   */
  void configured(String signal, ConfigProperties config) {
    List<SettingsProto.FoundExporter> built = new ArrayList<>();
    for (String name : names(config, signal)) {
      built.add(found(signal, name, true, "").toBuilder().setFromEnv(true).build());
    }
    synchronized (this) {
      // The settings the layer reads to say where they send: autoconfigure's, whether a variable
      // or a system property gave them, under the variable's name.
      for (String key : List.of("otel.exporter.otlp.endpoint", "otel.exporter.otlp.protocol",
          "otel.exporter.otlp." + signal + ".endpoint", "otel.exporter.otlp." + signal + ".protocol",
          "otel.exporter.zipkin.endpoint")) {
        String v = config.getString(key, "");
        if (!v.isBlank()) {
          env.put(key.toUpperCase(Locale.ROOT).replace('.', '_'), v);
        }
      }
      exporters(signal, built);
    }
  }

  /** The exporters of {@code otel.<signal>.exporter}, as the SDK reads it. */
  static List<String> names(ConfigProperties config, String signal) {
    List<String> names = config.getList("otel." + signal + ".exporter", List.of("otlp"));
    return names.contains("none") ? List.of() : names;
  }

  /**
   * What another extension added to the SDK's {@code signal} past the gate (a processor, by its
   * exporter's class or its own, or a metric reader), each ungoverned, beside the configured ones.
   */
  synchronized void outside(String signal, List<String> names) {
    List<SettingsProto.FoundExporter> added = new ArrayList<>(outside.getOrDefault(signal, List.of()));
    names.forEach(n -> added.add(found(signal, n, false, "")));
    outside.put(signal, List.copyOf(added));
    changes++;
  }

  /** The exporters of one signal as found, in place of any found before. */
  synchronized void exporters(String signal, List<SettingsProto.FoundExporter> built) {
    exporters.put(signal, List.copyOf(built));
    changes++;
  }

  /** An exporter as found: what it is known as ({@code otlp}, {@code otlp_http}, a class's name). */
  static SettingsProto.FoundExporter found(String signal, String kind, boolean gated, String endpoint) {
    return SettingsProto.FoundExporter.newBuilder().setSignal(signal).setKind(kind).setGated(gated)
        .setEndpoint(endpoint).build();
  }

  /**
   * Every exporter listed, governed or not, as the layer decides them in every SDK: what the
   * inventory and export health report. Decided again only after a change; each warning the layer
   * gives is logged once. The layer is called outside this lock (it takes the extension's). Never
   * throws: should the call fail, each exporter keeps the name last decided, a new one listed as found.
   */
  List<SettingsProto.Exporter> listed() {
    List<SettingsProto.FoundExporter> found = new ArrayList<>();
    Map<String, String> vars;
    long at;
    synchronized (this) {
      if (decidedAt == changes) {
        return decided;
      }
      exporters.values().forEach(found::addAll);
      outside.values().forEach(found::addAll);
      vars = Map.copyOf(env);
      at = changes;
    }
    SettingsProto.ExportersDecided d;
    try {
      // The SDK's own default; the agent sets http/protobuf, which autoconfigure then reads.
      d = layer.get().exporters(found, vars, "grpc");
    } catch (RuntimeException e) {
      // Reporting only: the gates are in place, and the list stays as it was. One new since is
      // listed as found, with no destination: only the layer strips an endpoint's credentials.
      LOG.log(Level.WARNING, "OpAMP: can't list the exporters; they are governed all the same", e);
      synchronized (this) {
        return found.stream().map(f -> listedAs.getOrDefault(f, SettingsProto.Exporter.newBuilder()
            .setSignal(f.getSignal()).setName(f.getKind()).setGated(f.getGated()).build())).toList();
      }
    }
    synchronized (this) {
      for (String w : d.getWarningsList()) {
        if (warned.add(w)) {
          LOG.warning("OpAMP: " + w);
        }
      }
      if (at == changes) {
        decided = List.copyOf(d.getExportersList());
        decidedAt = at;
      }
      if (d.getExportersCount() == found.size()) {
        for (int i = 0; i < found.size(); i++) {
          listedAs.put(found.get(i), d.getExporters(i));
        }
      }
    }
    return List.copyOf(d.getExportersList());
  }

  /** A layer of the inventory's own, until the extension gives it its own. */
  private static SdkSettings sharedLayer() {
    return Shared.LAYER;
  }

  /** Holds the inventory's own layer, made on first use. */
  private static final class Shared {
    static final SdkSettings LAYER = new SdkSettings();
  }

  /**
   * The inventory: the SDK's version and language as its own default resource gives them (the
   * application's {@code otel.resource.attributes} can't change them).
   */
  synchronized SettingsProto.Inventory build() {
    Resource sdk = Resource.getDefault();
    String language = sdk.getAttribute(SDK_LANGUAGE);
    String version = sdk.getAttribute(SDK_VERSION);
    SettingsProto.Inventory.Builder b = SettingsProto.Inventory.newBuilder().setIntegration(PACKAGE)
        .setIntegrationVersion(VERSION).setSdkLanguage(language == null ? "java" : language)
        .setSdkVersion(version == null ? "" : version).addAllInstrumentations(instrumentations.get());
    return b.addAllExporters(listed()).build();
  }

  /**
   * The agent's instrumentations, as {@code loader} finds its InstrumentationModules (the agent's
   * own and any extension's), each under every name it answers to in
   * {@code otel.instrumentation.<name>.enabled}: {@code spring-webmvc} and {@code spring-webmvc-6.0},
   * say. Every one, whether its library is there or not ({@link #applying} lists those that apply).
   * A module's version is its library's, as the agent's own instrumentations embed it
   * ({@code META-INF/io/opentelemetry/instrumentation/io.opentelemetry.<name>.properties}, the
   * version its telemetry's scope carries), with that scope; both unknown for one that embeds
   * none. Outside the agent: none.
   */
  static List<SettingsProto.Instrumentation> instrumentations(ClassLoader loader) {
    List<SettingsProto.Instrumentation> out = new ArrayList<>();
    for (InstrumentationModule m : modules(loader)) {
      out.addAll(listed(m));
    }
    return out;
  }

  /** The InstrumentationModules {@code loader} finds; outside the agent, none. */
  static List<InstrumentationModule> modules(ClassLoader loader) {
    List<InstrumentationModule> out = new ArrayList<>();
    try {
      Iterator<InstrumentationModule> modules = ServiceLoader.load(InstrumentationModule.class, loader).iterator();
      while (true) {
        try {
          if (!modules.hasNext()) {
            break;
          }
          out.add(modules.next());
        } catch (ServiceConfigurationError | LinkageError e) {
          // One that can't be made here; the agent says so itself if it can't run it.
          LOG.log(Level.FINE, "OpAMP: an instrumentation module isn't readable", e);
        }
      }
    } catch (LinkageError | RuntimeException e) {
      // Not under the agent (no extension API to read them by): none.
      LOG.log(Level.FINE, "OpAMP: the agent's instrumentations aren't readable", e);
    }
    return out;
  }

  /** A module as the inventory lists it: under every name it answers to, with its version and scope. */
  static List<SettingsProto.Instrumentation> listed(InstrumentationModule m) {
    Embedded embedded = embedded(m);
    List<SettingsProto.Instrumentation> out = new ArrayList<>();
    for (String name : m.instrumentationNames()) {
      out.add(SettingsProto.Instrumentation.newBuilder().setName(name).setVersion(embedded.version())
          .setScope(embedded.scope()).build());
    }
    return out;
  }

  /**
   * The instrumentations that apply: each module whose library a class loader of the
   * application's has, as the agent decides it can apply there (its {@code classLoaderMatcher}, and
   * one of its type instrumentations' {@code classLoaderOptimization}). One that matches only the
   * JDK, as the agent's JDK instrumentations do, applies wherever the JDK is. A library that comes
   * later (a class loader made once the application runs) is found at the next look ({@link #look}).
   */
  synchronized List<SettingsProto.Instrumentation> applying() {
    look();
    List<SettingsProto.Instrumentation> out = new ArrayList<>();
    for (Module m : modules) {
      if (m.applies) {
        out.addAll(m.listed);
      }
    }
    return out;
  }

  /**
   * Look for each module's library in the class loaders not looked in yet. Whether a module applies
   * now that didn't before: the inventory changed, and is to be sent again.
   */
  synchronized boolean look() {
    if (modules == null) {
      modules = new ArrayList<>();
      for (InstrumentationModule m : modulesFrom.get()) {
        modules.add(Module.of(m));
      }
    }
    boolean more = false;
    for (ClassLoader loader : loaders.get()) {
      if (loader == null || !looked.add(loader)) {
        continue;
      }
      for (Module m : modules) {
        if (!m.applies && m.appliesIn(loader)) {
          m.applies = true;
          more = true;
        }
      }
    }
    return more;
  }

  /** A module, with the matchers the agent decides by, and whether it applies. */
  private static final class Module {
    final List<SettingsProto.Instrumentation> listed;
    final ElementMatcher<ClassLoader> loader;
    final List<Type> types;
    boolean applies;

    private Module(List<SettingsProto.Instrumentation> listed, ElementMatcher<ClassLoader> loader, List<Type> types) {
      this.listed = listed;
      this.loader = loader;
      this.types = types;
    }

    /** A module read: one whose matchers can't be made never applies. */
    static Module of(InstrumentationModule m) {
      List<SettingsProto.Instrumentation> listed = listed(m);
      try {
        List<Type> types = new ArrayList<>();
        for (TypeInstrumentation t : m.typeInstrumentations()) {
          types.add(Type.of(t));
        }
        return new Module(listed, m.classLoaderMatcher(), types);
      } catch (RuntimeException | LinkageError e) {
        LOG.log(Level.FINE, "OpAMP: an instrumentation module's matchers aren't readable", e);
        return new Module(listed, null, List.of());
      }
    }

    /** Whether the agent can apply it in {@code loader}: one of its types' library is there. */
    boolean appliesIn(ClassLoader loader) {
      if (this.loader == null) {
        return false;
      }
      try {
        if (!this.loader.matches(loader)) {
          return false;
        }
        if (types.isEmpty()) {
          return true;
        }
        for (Type t : types) {
          if (t.appliesIn(loader)) {
            return true;
          }
        }
        return false;
      } catch (RuntimeException | LinkageError e) {
        return false;
      }
    }
  }

  /**
   * A type instrumentation, by what says where it can apply: its class loader matcher, and the
   * classes its type matcher names. Most of the agent's name the library's classes (an akka or a
   * Kafka class) and leave the class loader matcher to any: a class named is looked for, as the
   * agent's {@code hasClassesNamed} looks (a class file the loader has), so one whose library
   * isn't there doesn't apply. One that names none (a pattern only) applies by its class loader
   * matcher alone; one that matches nothing ({@code false}: the methods instrumentation with no
   * method configured) never applies.
   */
  record Type(ElementMatcher<ClassLoader> loader, List<String> classes, boolean never) {
    /** A qualified class name its matcher names: in {@code equals(...)} or {@code in(...)}. */
    private static final java.util.regex.Pattern NAMED =
        java.util.regex.Pattern.compile("\\b(?:equals|in)\\(([^()]*)\\)");

    static Type of(TypeInstrumentation t) {
      String matcher = String.valueOf(t.typeMatcher());
      return new Type(t.classLoaderOptimization(), classesNamed(matcher), matcher.equals("false"));
    }

    /** The qualified class names a type matcher's description names (a method's has no dot). */
    static List<String> classesNamed(String matcher) {
      List<String> out = new ArrayList<>();
      java.util.regex.Matcher m = NAMED.matcher(matcher);
      while (m.find()) {
        for (String name : m.group(1).split(",")) {
          String n = name.trim();
          if (n.indexOf('.') > 0 && n.chars().allMatch(c -> Character.isJavaIdentifierPart(c) || c == '.')) {
            out.add(n);
          }
        }
      }
      return out;
    }

    boolean appliesIn(ClassLoader cl) {
      if (never || !loader.matches(cl)) {
        return false;
      }
      if (classes.isEmpty()) {
        return true;
      }
      for (String c : classes) {
        if (cl.getResource(c.replace('.', '/') + ".class") != null) {
          return true;
        }
      }
      return false;
    }
  }

  /**
   * The class loaders an application's library can be in: the system class loader (the class path,
   * and through it the JDK's), and every other that has loaded a class (an application server's, a
   * Spring Boot jar's), but the agent's own and those that delegate to it (ByteBuddy's, under the
   * agent's loader, see the libraries the agent bundles for itself). All loaded classes, through the
   * JVM's instrumentation, which the agent's bootstrap holds; without it, the system class loader
   * alone.
   */
  static Collection<ClassLoader> loaders() {
    Set<ClassLoader> out = Collections.newSetFromMap(new IdentityHashMap<>());
    out.add(ClassLoader.getSystemClassLoader());
    java.lang.instrument.Instrumentation jvm = jvmInstrumentation();
    if (jvm != null) {
      Set<ClassLoader> agents = Collections.newSetFromMap(new IdentityHashMap<>());
      for (Class<?> c : jvm.getAllLoadedClasses()) {
        ClassLoader l = c.getClassLoader();
        if (l != null && !out.contains(l) && !agents.contains(l)) {
          (agents(l) ? agents : out).add(l);
        }
      }
    }
    return out;
  }

  /** Whether {@code loader} is the agent's, or delegates to the agent's (a parent of it is). */
  static boolean agents(ClassLoader loader) {
    for (ClassLoader l = loader; l != null; l = l.getParent()) {
      if (l.getClass().getName().startsWith("io.opentelemetry.javaagent.") || l == Inventory.class.getClassLoader()) {
        return true;
      }
    }
    return false;
  }

  /** The JVM's instrumentation, as the agent's bootstrap holds it; null outside the agent. */
  private static java.lang.instrument.Instrumentation jvmInstrumentation() {
    try {
      Class<?> holder = Class.forName("io.opentelemetry.javaagent.bootstrap.InstrumentationHolder", false, null);
      return (java.lang.instrument.Instrumentation) holder.getMethod("getInstrumentation").invoke(null);
    } catch (ReflectiveOperationException | RuntimeException | LinkageError e) {
      return null;
    }
  }

  /** What a module embeds of its telemetry's scope: its version, and the scope's name. */
  private record Embedded(String version, String scope) {}

  /**
   * A module's version, from the first of its names that embeds one, and the scope its telemetry
   * carries: that name's, {@code io.opentelemetry.<name>}, which the file is named for. Both empty
   * when none does.
   */
  private static Embedded embedded(InstrumentationModule m) {
    ClassLoader loader = m.getClass().getClassLoader();
    if (loader == null) {
      return new Embedded("", "");
    }
    for (String name : m.instrumentationNames()) {
      String scope = "io.opentelemetry." + name;
      String v = property(loader.getResourceAsStream("META-INF/io/opentelemetry/instrumentation/" + scope + ".properties"));
      if (!v.isEmpty()) {
        return new Embedded(v, scope);
      }
    }
    return new Embedded("", "");
  }

  /** The {@code version} of a properties file; empty without one. */
  private static String property(InputStream in) {
    if (in == null) {
      return "";
    }
    try (in) {
      Properties p = new Properties();
      p.load(in);
      return p.getProperty("version", "").trim();
    } catch (IOException | IllegalArgumentException e) {
      return "";
    }
  }
}
