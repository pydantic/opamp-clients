// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assertions.fail;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.io.BufferedInputStream;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.URI;
import java.net.http.HttpRequest;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.TreeMap;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * The shells' shared tables (tests/shell-tables/), as this client reads them: every URL the
 * core accepts is taken by the JDK's HTTP stack (java.net.URI, the HTTP client, its WebSocket),
 * read alike, and sent as exactly its target; every TLS setting is taken or refused as the table
 * says. Where the JDK differs, the case is listed below with the reason, so the test still fails if
 * the behaviour changes. In platform mode there is no proxy table: the client takes no proxy from
 * the environment (HTTP_PROXY and the like), only the JDK's ProxySelector or one given ({@link
 * OpampClient.Config.Builder#proxy}). In Rust mode (the core speaks HTTP and WebSocket over this
 * client's sockets) every URL is sent by the core with exactly its target, the proxy comes from
 * the environment by Rust mode's rule with no difference from proxy.json, and the TLS settings
 * are taken or refused as the table says, for Rust mode's engines too.
 *
 * <p>The tables are found at OPAMP_SHELL_TABLES if set (a Docker run that mounts only java/opamp
 * mounts them there), else at tests/shell-tables in a directory above this one.
 */
class ShellTablesTest {
  /**
   * URLs the JDK sends with another request target than the core's (the reference's): its HTTP
   * client drops an empty query's "?" (jdk.internal.net.http.Http1Request: a path and query with an
   * empty query is sent as the path alone). Equivalent for any server we know of; the JDK can't be
   * told otherwise.
   */
  private static final String DROPS_EMPTY_QUERY = "the JDK's HTTP client drops the '?' of an empty query";

  // ---------------------------------------------------------------------------------------------
  // URLs.

  @Test
  void everyUrlTheCoreAcceptsIsTakenAndSentAsItsTarget(@TempDir Path dir) throws Exception {
    List<Object> urls = list(table("urls.json").get("urls"));
    assertTrue(urls.size() > 200, "the corpus");
    LinkedBlockingQueue<String> seen = new LinkedBlockingQueue<>();
    // A server that records each request's raw request-target, and answers 200 (a WebSocket
    // handshake too: the client then fails it, which is all that is needed here).
    try (Raw server = new Raw(s -> {
      InputStream in = new BufferedInputStream(s.getInputStream());
      String line = requestLine(in);
      String[] parts = line.split(" ", 3);
      seen.add(parts.length == 3 ? parts[1] : "?? " + line);
      int len = 0;
      for (String h = requestLine(in); !h.isEmpty(); h = requestLine(in)) {
        if (h.toLowerCase(Locale.ROOT).startsWith("content-length:")) {
          len = Integer.parseInt(h.substring(15).trim());
        }
      }
      in.readNBytes(len);
      OutputStream out = s.getOutputStream();
      out.write("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".getBytes(StandardCharsets.US_ASCII));
      out.flush();
      s.close();
    })) {
      String base = "127.0.0.1:" + server.url("http").substring("http://127.0.0.1:".length());
      LinkedBlockingQueue<CoreProto.Input> got = new LinkedBlockingQueue<>();
      Executor x = new Executor(Transport.build(null, null, true), got::add, dir.resolve("state"));
      Map<String, String> wrong = new TreeMap<>();
      long id = 0;
      for (Object o : urls) {
        Map<String, Object> e = map(o);
        String url = (String) e.get("url");
        String target = (String) e.get("target");
        String scheme = ((String) e.get("scheme")).toLowerCase(Locale.ROOT);
        boolean ws = scheme.startsWith("ws");
        List<String> problems = new ArrayList<>();
        // Read: the URI the executor makes of it, and the request API taking it.
        try {
          URI u = URI.create(url);
          if (!ws) {
            HttpRequest.newBuilder(u);
          } else if (u.getHost() == null || u.getRawFragment() != null) {
            problems.add("the WebSocket refuses it (no host, or a fragment)");
          }
          String read = describe(u.getHost() == null ? null : host(u.getHost()), port(u, scheme),
              u.getRawPath() == null || u.getRawPath().isEmpty() ? "/" : u.getRawPath(), u.getRawQuery());
          String want = describe(host((String) e.get("host")), ((Number) e.get("port")).intValue(),
              (String) e.get("path"), (String) e.get("query"));
          if (!read.equals(want)) {
            problems.add("read as " + read + ", the core as " + want);
          }
        } catch (IllegalArgumentException ex) {
          problems.add("refused: " + ex.getMessage());
        }
        // Sent: the same target, to this server, through the executor (a POST; for ws and wss, the
        // WebSocket handshake too).
        seen.clear();
        x.run(CoreProto.Effect.newBuilder().setHttpRequest(CoreProto.HttpRequest.newBuilder()
            .setId(++id).setUrl("http://" + base + target).setBody(ByteString.copyFromUtf8("x"))
            .setTimeoutMs(10_000).setMaxResponseBytes(1024)).build());
        sent("POST", got, seen, target, problems);
        if (ws) {
          x.run(CoreProto.Effect.newBuilder().setWsOpen(CoreProto.WsOpen.newBuilder()
              .setId(++id).setUrl("ws://" + base + target).setMaxMessageBytes(1024)).build());
          sent("WebSocket", got, seen, target, problems);
        }
        if (!problems.isEmpty()) {
          wrong.put(url, String.join("; ", problems));
        }
      }
      x.close();
      // The known differences, exactly: an empty query's "?" dropped, and nothing else.
      Map<String, String> known = new TreeMap<>();
      for (Object o : urls) {
        Map<String, Object> e = map(o);
        String target = (String) e.get("target");
        if ("".equals(e.get("query"))) {
          String sentAs = "sent \"" + target.substring(0, target.length() - 1) + "\", not \"" + target + "\"";
          boolean ws = ((String) e.get("scheme")).toLowerCase(Locale.ROOT).startsWith("ws");
          known.put((String) e.get("url"), "POST " + sentAs + (ws ? "; WebSocket " + sentAs : ""));
        }
      }
      Map<String, String> unexplained = new TreeMap<>(wrong);
      unexplained.entrySet().removeIf(w -> w.getValue().equals(known.get(w.getKey())));
      Map<String, String> gone = new TreeMap<>(known);
      gone.keySet().removeAll(wrong.keySet());
      assertEquals(Map.of(), unexplained, "URLs the JDK takes, reads or sends otherwise, " + unexplained.size() + " of " + urls.size());
      assertEquals(Map.of(), gone, "known differences that are gone (" + DROPS_EMPTY_QUERY + "): take them off");
    }
  }

  /** Rust mode: the core sends each URL's exact target over this client's streams, HTTP and WebSocket. */
  @Test
  void rustModeSendsEveryUrlAsItsTarget() throws Exception {
    List<Object> urls = list(table("urls.json").get("urls"));
    LinkedBlockingQueue<String> seen = new LinkedBlockingQueue<>();
    try (Raw server = new Raw(s -> {
      InputStream in = new BufferedInputStream(s.getInputStream());
      String line = requestLine(in);
      String[] parts = line.split(" ", 3);
      seen.add(parts.length == 3 ? parts[1] : "?? " + line);
      int len = 0;
      for (String h = requestLine(in); !h.isEmpty(); h = requestLine(in)) {
        if (h.toLowerCase(Locale.ROOT).startsWith("content-length:")) {
          len = Integer.parseInt(h.substring(15).trim());
        }
      }
      in.readNBytes(len);
      OutputStream out = s.getOutputStream();
      out.write("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".getBytes(StandardCharsets.US_ASCII));
      out.flush();
      s.close();
    })) {
      String base = "127.0.0.1:" + server.url("http").substring("http://127.0.0.1:".length());
      Map<String, String> wrong = new TreeMap<>();
      for (Object o : urls) {
        Map<String, Object> e = map(o);
        String target = (String) e.get("target");
        boolean ws = ((String) e.get("scheme")).toLowerCase(Locale.ROOT).startsWith("ws");
        String endpoint = (ws ? "ws://" : "http://") + base + target;
        seen.clear();
        OpampClient c = OpampClient.start(OpampClient.Config.builder(endpoint, new byte[16]).mode(OpampClient.Mode.RUST)
            .directConnection().heartbeatInterval(java.time.Duration.ofSeconds(60)).build(), OpampClient.Status.of(null), n -> {});
        String arrived;
        try {
          arrived = seen.poll(20, TimeUnit.SECONDS);
        } finally {
          c.stop().get(20, TimeUnit.SECONDS);
        }
        if (!target.equals(arrived)) {
          wrong.put((String) e.get("url"), "sent \"" + arrived + "\", not \"" + target + "\"");
        }
      }
      // No known differences: the core writes the request itself.
      assertEquals(Map.of(), wrong, "URLs Rust mode sends otherwise, " + wrong.size() + " of " + urls.size());
    }
  }

  private static void sent(String how, LinkedBlockingQueue<CoreProto.Input> got, LinkedBlockingQueue<String> seen,
      String target, List<String> problems) throws InterruptedException {
    CoreProto.Input in = got.poll(20, TimeUnit.SECONDS);
    String arrived = seen.poll(5, TimeUnit.SECONDS);
    if (arrived == null) {
      problems.add(how + " not sent: " + in);
    } else if (!arrived.equals(target)) {
      problems.add(how + " sent \"" + arrived + "\", not \"" + target + "\"");
    }
  }

  private static String describe(String host, int port, String path, String query) {
    return "host " + host + ", port " + port + ", path " + path + ", query " + query;
  }

  /** A host as compared: names ignoring case, IP addresses by address. */
  private static String host(String h) {
    String bare = h.startsWith("[") && h.endsWith("]") ? h.substring(1, h.length() - 1) : h;
    if (bare.contains(":") || bare.matches("[0-9]+(\\.[0-9]+){3}")) {
      try {
        return InetAddress.getByName(bare).getHostAddress();
      } catch (IOException e) {
        return "unparsable " + h;
      }
    }
    return h.toLowerCase(Locale.ROOT);
  }

  private static int port(URI u, String scheme) {
    if (u.getPort() >= 0) {
      return u.getPort();
    }
    return scheme.equals("http") || scheme.equals("ws") ? 80 : 443;
  }

  private static String requestLine(InputStream in) throws IOException {
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

  // ---------------------------------------------------------------------------------------------
  // Proxies (Rust mode: platform mode takes none from the environment).

  /**
   * Rust mode's proxy from the environment: the client hands the core the variables
   * ({@code Config.proxy}) and the core picks by the shared rule. Each case through this client's
   * reading of the environment and the core it ships: exactly proxy.json, no known differences.
   */
  // covers: RULE-PROXY-02
  @Test
  void rustModeTakesTheEnvironmentsProxyExactlyAsTheTableSays() throws Exception {
    List<Object> cases = list(table("proxy.json").get("cases"));
    assertTrue(cases.size() > 30, "the cases");
    Map<String, String> wrong = new TreeMap<>();
    for (Object o : cases) {
      Map<String, Object> c = map(o);
      Map<String, String> env = new java.util.HashMap<>();
      map(c.get("env")).forEach((k, v) -> env.put(k, (String) v));
      String url = (String) c.get("url");
      OpampClient.Config config = OpampClient.Config.builder(url).mode(OpampClient.Mode.RUST).environment(env).build();
      String got = coreRoute(OpampClient.proxyChoice(config), url);
      if (!java.util.Objects.equals(got, c.get("proxy"))) {
        wrong.put((String) c.get("id"), got + ", the table says " + c.get("proxy") + " (" + c.get("why") + ")");
      }
    }
    assertEquals(Map.of(), wrong, "Rust mode's proxy otherwise than proxy.json");
  }

  /**
   * What Rust mode tells the core: the configured proxy and no variables; none for a direct
   * connection; otherwise each variable the rule reads that is set, even empty, by its own name. A
   * proxy_query (only a core that wasn't told asks) hears the configured proxy or none.
   */
  // covers: RULE-PROXY-02
  @Test
  void rustModeTellsTheCoreItsProxy() throws Exception {
    Map<String, String> env = Map.of("HTTP_PROXY", "http://env:1", "no_proxy", "", "REQUEST_METHOD", "GET", "OTHER", "x");
    OpampClient.Config configured = OpampClient.Config.builder("http://x/").mode(OpampClient.Mode.RUST).environment(env)
        .proxy("socks5://u:p%40ss@configured:1080").proxyResolveLocally(true).build();
    assertEquals(CoreProto.ProxyChoice.newBuilder().setUrl("socks5://u:p%40ss@configured:1080").setResolveLocally(true).build(),
        OpampClient.proxyChoice(configured));
    assertEquals("socks5://u:p%40ss@configured:1080", OpampClient.proxies(configured).apply("http://x/").getProxy());
    OpampClient.Config direct = OpampClient.Config.builder("http://x/").mode(OpampClient.Mode.RUST).environment(env)
        .directConnection().build();
    assertEquals(CoreProto.ProxyChoice.newBuilder().setDirect(true).build(), OpampClient.proxyChoice(direct));
    OpampClient.Config fromEnv = OpampClient.Config.builder("http://x/").mode(OpampClient.Mode.RUST).environment(env).build();
    CoreProto.ProxyChoice choice = OpampClient.proxyChoice(fromEnv);
    List<String> got = new ArrayList<>();
    choice.getEnvironmentList().forEach(h -> got.add(h.getName() + "=" + h.getValue()));
    assertEquals(List.of("REQUEST_METHOD=GET", "HTTP_PROXY=http://env:1", "no_proxy="), got);
    assertEquals("", OpampClient.proxies(fromEnv).apply("http://x/").getProxy());
    // The core reads them by the rule: CGI, so none.
    assertEquals(null, coreRoute(choice, "http://example.com/"));
  }

  /**
   * Where Rust mode's core connects first for {@code url} when told {@code choice}: the proxy as
   * the table writes a route (scheme://host:port, credentials left out), or null for directly. The
   * core picks it itself: it asks no proxy_query.
   */
  private static String coreRoute(CoreProto.ProxyChoice choice, String url) {
    Core core = new Core(CoreProto.Config.newBuilder().setInstanceUid(ByteString.copyFrom(new byte[16])).setEndpoint(url)
        .setStreams(true).setEntropy(ByteString.copyFrom(new byte[32])).setProxy(choice).build());
    try {
      CoreProto.Output out = core.step(1, 1, CoreProto.Input.newBuilder().setStart(CoreProto.Empty.getDefaultInstance()).build());
      CoreProto.StreamOpen open = null;
      for (CoreProto.Effect e : out.getEffectsList()) {
        assertTrue(!e.hasProxyQuery(), url + ": the core asked which proxy");
        if (e.hasStreamOpen()) {
          open = e.getStreamOpen();
        }
      }
      assertTrue(open != null, url + ": no stream opened");
      URI u = URI.create(url);
      String scheme = u.getScheme().toLowerCase(Locale.ROOT);
      int port = u.getPort() >= 0 ? u.getPort() : scheme.equals("https") || scheme.equals("wss") ? 443 : 80;
      String host = u.getHost().startsWith("[") ? u.getHost().substring(1, u.getHost().length() - 1) : u.getHost();
      if (open.getHost().equalsIgnoreCase(host) && open.getPort() == port) {
        return null;
      }
      // Which kind of proxy: SOCKS5 greets with version 5, an https:// one is TLS first.
      CoreProto.Output next = core.step(2, 2, CoreProto.Input.newBuilder()
          .setStreamOpened(CoreProto.Id.newBuilder().setId(open.getId())).build());
      String kind = "http";
      for (CoreProto.Effect e : next.getEffectsList()) {
        if (e.hasStreamWrite() && !e.getStreamWrite().getData().isEmpty() && e.getStreamWrite().getData().byteAt(0) == 5) {
          kind = "socks5";
        }
        if (e.hasStreamStartTls()) {
          kind = "https";
        }
      }
      String at = open.getHost().contains(":") ? "[" + open.getHost() + "]" : open.getHost();
      return kind + "://" + at + ":" + open.getPort();
    } finally {
      core.close();
    }
  }

  // ---------------------------------------------------------------------------------------------
  // TLS.

  // A client key in each PEM form a key comes in (PKCS #8, PKCS #1 RSA, SEC 1 EC) is taken, and is
  // the same key: the JDK reads PKCS #8 only, so the other two are wrapped as PKCS #8 first.
  @Test
  void aClientKeyIsTakenInEachPemForm() throws Exception {
    for (String[] forms : new String[][] {
        {"tls/client-rsa.pem", "tls/client-rsa.key", "tls/client-rsa-pkcs1.key"},
        {"tls/client-ec.pem", "tls/client-ec.key", "tls/client-ec-sec1.key"}}) {
      java.security.PrivateKey pkcs8 = Tls.privateKey(read(forms[1]));
      java.security.PrivateKey other = Tls.privateKey(read(forms[2]));
      // The same key: an RSA key's numbers, an EC key's secret and curve.
      if (pkcs8 instanceof java.security.interfaces.ECPrivateKey ec) {
        java.security.interfaces.ECPrivateKey o = (java.security.interfaces.ECPrivateKey) other;
        assertEquals(ec.getS(), o.getS(), forms[2] + " is the same key as " + forms[1]);
        assertEquals(ec.getParams().getCurve(), o.getParams().getCurve(), forms[2] + "'s curve");
      } else {
        assertEquals(pkcs8, other, forms[2] + " is the same key as " + forms[1]);
      }
      Tls.builder().clientCertificate(read(forms[0]), read(forms[2])).build();
    }
  }

  @Test
  void everyTlsSettingIsTakenOrRefusedAsTheTableSays() throws Exception {
    List<Object> cases = list(table("tls.json").get("cases"));
    assertTrue(cases.size() > 20, "the cases");
    Map<String, String> wrong = new TreeMap<>();
    for (Object o : cases) {
      Map<String, Object> c = map(o);
      String id = (String) c.get("id");
      String got;
      String why = "";
      try {
        Tls.Builder b = Tls.builder();
        if (c.get("ca") != null) {
          b.trustedCa(read((String) c.get("ca")));
        }
        if (c.get("cert") != null) {
          b.clientCertificate(read((String) c.get("cert")), c.get("key") == null ? "" : read((String) c.get("key")));
        }
        if (c.get("min") != null) {
          b.minVersion(version((String) c.get("min")));
        }
        if (c.get("max") != null) {
          b.maxVersion(version((String) c.get("max")));
        }
        // The settings, and the transport built from them (HTTP and WebSocket share it); and Rust
        // mode's TLS from the same settings, an engine for a name and one for an address.
        Tls tls = b.build();
        Transport.build(tls, null, true);
        java.util.function.Function<String, javax.net.ssl.SSLEngine> engines = Streams.engines(tls);
        engines.apply("example.com");
        engines.apply("192.0.2.1");
        got = "ok";
      } catch (IllegalArgumentException | IllegalStateException e) {
        got = "refused";
        why = ": " + e.getMessage();
      }
      if (!got.equals(c.get("expect"))) {
        wrong.put(id, got + why);
      }
    }
    // No known differences: every case is as the table says.
    assertEquals(Map.of(), wrong, "TLS settings taken or refused otherwise than the table says");
  }

  /** A version this client's API names; one it can't (TLS 1.1, 1.0) is refused. */
  private static Tls.Version version(String v) {
    return switch (v) {
      case "1.2" -> Tls.Version.TLS_1_2;
      case "1.3" -> Tls.Version.TLS_1_3;
      default -> throw new IllegalArgumentException("TLS " + v + ": not a version this client can name");
    };
  }

  // ---------------------------------------------------------------------------------------------
  // The tables, and just enough JSON to read them.

  static Path tables() {
    String env = System.getenv("OPAMP_SHELL_TABLES");
    if (env != null && !env.isEmpty()) {
      return Path.of(env);
    }
    for (Path d = Path.of("").toAbsolutePath(); d != null; d = d.getParent()) {
      if (Files.isDirectory(d.resolve("tests/shell-tables"))) {
        return d.resolve("tests/shell-tables");
      }
    }
    return fail("the shell tables aren't there (no tests/shell-tables above " + Path.of("").toAbsolutePath()
        + "): set OPAMP_SHELL_TABLES (in Docker: -v $PWD/tests/shell-tables:/shell-tables:ro -e OPAMP_SHELL_TABLES=/shell-tables)");
  }

  private static String read(String rel) throws IOException {
    return Files.readString(tables().resolve(rel), StandardCharsets.UTF_8);
  }

  private static Map<String, Object> table(String name) throws IOException {
    return map(new Json(Files.readString(tables().resolve(name), StandardCharsets.UTF_8)).value());
  }

  @SuppressWarnings("unchecked")
  private static Map<String, Object> map(Object o) {
    return (Map<String, Object>) o;
  }

  @SuppressWarnings("unchecked")
  private static List<Object> list(Object o) {
    return (List<Object>) o;
  }

  /** A JSON reader: objects, arrays, strings, numbers, true, false, null. */
  private static final class Json {
    private final String s;
    private int i;

    Json(String s) {
      this.s = s;
    }

    Object value() {
      skip();
      char c = s.charAt(i);
      switch (c) {
        case '{' -> {
          Map<String, Object> m = new LinkedHashMap<>();
          i++;
          skip();
          if (s.charAt(i) == '}') {
            i++;
            return m;
          }
          while (true) {
            skip();
            String k = string();
            skip();
            expect(':');
            m.put(k, value());
            skip();
            if (s.charAt(i++) == '}') {
              return m;
            }
          }
        }
        case '[' -> {
          List<Object> l = new ArrayList<>();
          i++;
          skip();
          if (s.charAt(i) == ']') {
            i++;
            return l;
          }
          while (true) {
            l.add(value());
            skip();
            if (s.charAt(i++) == ']') {
              return l;
            }
          }
        }
        case '"' -> {
          return string();
        }
        default -> {
          int start = i;
          while (i < s.length() && ",]} \n\r\t".indexOf(s.charAt(i)) < 0) {
            i++;
          }
          String w = s.substring(start, i);
          return switch (w) {
            case "null" -> null;
            case "true" -> true;
            case "false" -> false;
            default -> Double.parseDouble(w);
          };
        }
      }
    }

    private String string() {
      expect('"');
      StringBuilder b = new StringBuilder();
      while (true) {
        char c = s.charAt(i++);
        if (c == '"') {
          return b.toString();
        }
        if (c != '\\') {
          b.append(c);
          continue;
        }
        char e = s.charAt(i++);
        switch (e) {
          case 'n' -> b.append('\n');
          case 't' -> b.append('\t');
          case 'r' -> b.append('\r');
          case 'b' -> b.append('\b');
          case 'f' -> b.append('\f');
          case 'u' -> {
            b.append((char) Integer.parseInt(s.substring(i, i + 4), 16));
            i += 4;
          }
          default -> b.append(e);
        }
      }
    }

    private void expect(char c) {
      if (s.charAt(i++) != c) {
        throw new IllegalArgumentException("JSON: expected " + c + " at " + (i - 1));
      }
    }

    private void skip() {
      while (i < s.length() && Character.isWhitespace(s.charAt(i))) {
        i++;
      }
    }
  }
}
