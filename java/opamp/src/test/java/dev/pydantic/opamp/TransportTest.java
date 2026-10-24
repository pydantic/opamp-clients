// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.io.BufferedInputStream;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.net.URI;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.security.cert.X509Certificate;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Objects;
import java.util.Set;
import java.util.SplittableRandom;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;
import javax.net.ssl.SSLContext;
import javax.net.ssl.SSLServerSocket;
import javax.net.ssl.SSLSocket;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * The transport built from the options: TLS with the CAs given, a client certificate, TLS versions,
 * and an HTTP proxy with credentials, over plain HTTP, WebSocket and downloads alike. The
 * certificates are src/test/resources/tls (made by gen.sh there). The proxy tests need Basic
 * credentials through CONNECT tunnels, which the JDK allows only with
 * -Djdk.http.auth.tunneling.disabledSchemes= (pom.xml sets it for the tests).
 */
class TransportTest {
  static String pem(String name) {
    try (InputStream in = TransportTest.class.getResourceAsStream("/tls/" + name)) {
      return new String(Objects.requireNonNull(in, name).readAllBytes(), StandardCharsets.US_ASCII);
    } catch (IOException e) {
      throw new IllegalStateException(e);
    }
  }

  static final String CA = pem("ca.pem");
  static final String OTHER_CA = pem("other-ca.pem");

  static Tls trusting(String ca) {
    return Tls.builder().trustedCa(ca).build();
  }

  static Tls withClientCert() {
    return Tls.builder().trustedCa(CA).clientCertificate(pem("client.pem"), pem("client-key.pem")).build();
  }

  // covers: RULE-TLS-01
  @Test
  void trustsTheGivenCaOverHttpWebSocketAndDownloads(@TempDir Path dir) throws Exception {
    try (Origin origin = Origin.tls(false, null)) {
      Probe p = new Probe(Transport.build(trusting(CA), null, false), dir);
      assertEquals(CoreProto.Input.KindCase.HTTP_RESPONSE, p.post(origin.url("https")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.WS_OPENED, p.open(origin.url("wss")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.DOWNLOAD_RESPONSE, p.download(origin.url("https") + "/file").getKindCase());
      // The system's CAs as well: still the given one.
      Probe both = new Probe(Transport.build(Tls.builder().trustedCa(CA).includeSystemCas(true).build(), null, false), dir);
      assertEquals(CoreProto.Input.KindCase.HTTP_RESPONSE, both.post(origin.url("https")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.WS_OPENED, both.open(origin.url("wss")).getKindCase());
    }
  }

  // covers: RULE-TLS-01
  @Test
  void sendsNothingToAServerItCannotVerify(@TempDir Path dir) throws Exception {
    try (Origin origin = Origin.tls(false, null)) {
      for (Transport t : List.of(Transport.build(null, null, false), Transport.build(trusting(OTHER_CA), null, false),
          Transport.build(Tls.builder().trustedCa(OTHER_CA).includeSystemCas(true).build(), null, false))) {
        Probe p = new Probe(t, dir);
        CoreProto.Input http = p.post(origin.url("https"));
        assertEquals(CoreProto.Input.KindCase.HTTP_FAILED, http.getKindCase());
        CoreProto.Input ws = p.open(origin.url("wss"));
        assertEquals(CoreProto.Input.KindCase.WS_FAILED, ws.getKindCase());
        assertFalse(ws.getWsFailed().hasStatus());
        assertEquals(CoreProto.Input.KindCase.DOWNLOAD_FAILED, p.download(origin.url("https") + "/file").getKindCase());
      }
      assertTrue(origin.requests.isEmpty(), origin.requests.toString());
    }
  }

  // covers: RULE-TLS-02
  @Test
  void presentsTheClientCertificate(@TempDir Path dir) throws Exception {
    try (Origin origin = Origin.tls(true, null)) {
      Probe p = new Probe(Transport.build(withClientCert(), null, false), dir);
      assertEquals(CoreProto.Input.KindCase.HTTP_RESPONSE, p.post(origin.url("https")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.WS_OPENED, p.open(origin.url("wss")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.DOWNLOAD_RESPONSE, p.download(origin.url("https") + "/file").getKindCase());
      assertEquals(3, origin.peers.size(), origin.peers.toString());
      for (String peer : origin.peers) {
        assertEquals("CN=opamp-test-agent", peer);
      }
      // Without one, the server refuses the handshake.
      Probe none = new Probe(Transport.build(trusting(CA), null, false), dir);
      assertEquals(CoreProto.Input.KindCase.HTTP_FAILED, none.post(origin.url("https")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.WS_FAILED, none.open(origin.url("wss")).getKindCase());
      // The client certificate goes only to a file at the server's own origin, as the core says
      // (Download.server_origin): told otherwise, the client presents none, and is refused.
      assertEquals(CoreProto.Input.KindCase.DOWNLOAD_FAILED, p.download(origin.url("https") + "/file", false).getKindCase());
      assertEquals(3, origin.requests.size());
    }
  }

  // covers: RULE-TLS-01
  @Test
  void keepsToTheTlsVersionsAllowed(@TempDir Path dir) throws Exception {
    try (Origin origin = Origin.tls(false, "TLSv1.2")) {
      assertEquals(CoreProto.Input.KindCase.HTTP_RESPONSE,
          new Probe(Transport.build(trusting(CA), null, false), dir).post(origin.url("https")).getKindCase());
      Tls only13 = Tls.builder().trustedCa(CA).minVersion(Tls.Version.TLS_1_3).build();
      Probe p = new Probe(Transport.build(only13, null, false), dir);
      assertEquals(CoreProto.Input.KindCase.HTTP_FAILED, p.post(origin.url("https")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.WS_FAILED, p.open(origin.url("wss")).getKindCase());
    }
    assertArrayEquals(new String[] {"TLSv1.3", "TLSv1.2"}, Tls.builder().build().protocols());
    assertArrayEquals(new String[] {"TLSv1.2"}, Tls.builder().maxVersion(Tls.Version.TLS_1_2).build().protocols());
    assertThrows(IllegalArgumentException.class,
        () -> Tls.builder().minVersion(Tls.Version.TLS_1_3).maxVersion(Tls.Version.TLS_1_2).build());
  }

  // covers: RULE-PROXY-01
  @Test
  void goesThroughAnHttpProxyWithCredentials(@TempDir Path dir) throws Exception {
    try (Origin plain = Origin.plain(); Origin tls = Origin.tls(false, null); HttpProxy proxy = new HttpProxy("agent", "p@ss:w")) {
      Transport.Proxy via = Transport.Proxy.parse("http://agent:p%40ss%3Aw@127.0.0.1:" + proxy.port());
      Probe p = new Probe(Transport.build(trusting(CA), via, false), dir);
      assertEquals(CoreProto.Input.KindCase.HTTP_RESPONSE, p.post(plain.url("http")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.WS_OPENED, p.open(plain.url("ws")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.HTTP_RESPONSE, p.post(tls.url("https")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.WS_OPENED, p.open(tls.url("wss")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.DOWNLOAD_RESPONSE, p.download(plain.url("http") + "/file").getKindCase());
      assertEquals(CoreProto.Input.KindCase.DOWNLOAD_RESPONSE, p.download(tls.url("https") + "/file").getKindCase());
      String plainHost = "127.0.0.1:" + plain.port();
      String tlsHost = "127.0.0.1:" + tls.port();
      assertEquals(List.of(
          "POST http://" + plainHost + "/v1/opamp", // absolute form for http://
          "CONNECT " + plainHost, // a tunnel for ws://
          "CONNECT " + tlsHost,
          "CONNECT " + tlsHost,
          "GET http://" + plainHost + "/file",
          "CONNECT " + tlsHost), proxy.seen, "every connection through the proxy");
      // The credentials are the proxy's: the server never sees them.
      for (Map<String, String> h : plain.headers) {
        assertNull(h.get("proxy-authorization"), h.toString());
      }
      for (Map<String, String> h : tls.headers) {
        assertNull(h.get("proxy-authorization"), h.toString());
      }
      assertEquals(3, plain.requests.size(), plain.requests.toString());
      assertEquals(3, tls.requests.size(), tls.requests.toString());
    }
  }

  // covers: RULE-PROXY-01
  @Test
  void wrongProxyCredentialsGetNoFurther(@TempDir Path dir) throws Exception {
    try (Origin plain = Origin.plain(); HttpProxy proxy = new HttpProxy("agent", "secret")) {
      Probe p = new Probe(Transport.build(null, Transport.Proxy.parse("http://agent:wrong@127.0.0.1:" + proxy.port()), false), dir);
      CoreProto.Input http = p.post(plain.url("http"));
      assertEquals(407, http.getHttpResponse().getStatus());
      CoreProto.Input ws = p.open(plain.url("ws"));
      assertEquals(CoreProto.Input.KindCase.WS_FAILED, ws.getKindCase());
      Probe none = new Probe(Transport.build(null, Transport.Proxy.parse("http://127.0.0.1:" + proxy.port()), false), dir);
      assertEquals(407, none.post(plain.url("http")).getHttpResponse().getStatus());
      assertTrue(plain.requests.isEmpty(), plain.requests.toString());
    }
  }

  @Test
  void proxyUrls() {
    Transport.Proxy p = Transport.Proxy.parse("http://us%3Aer:pa%40ss+w@proxy.example:3128");
    assertEquals(new Transport.Proxy("proxy.example", 3128, "us:er", "pa@ss+w"), p);
    assertEquals("http://us:er:<redacted>@proxy.example:3128", p.toString());
    assertEquals(new Transport.Proxy("proxy.example", 80, null, null), Transport.Proxy.parse("http://proxy.example"));
    assertEquals(new Transport.Proxy("::1", 8080, "u", ""), Transport.Proxy.parse("HTTP://u@[::1]:8080/"));
    for (String bad : List.of("socks5://u:p@proxy:1080", "socks5h://proxy:1080", "https://proxy:443", "proxy:3128",
        "http://proxy:3128/path", "http://:3128", "ftp://proxy")) {
      IllegalArgumentException e = assertThrows(IllegalArgumentException.class, () -> Transport.Proxy.parse(bad), bad);
      assertFalse(e.getMessage().contains("u:p"), e.getMessage());
    }
    // Checked at build, as the mode reads it (core mode takes SOCKS5: CoreModeTest).
    assertTrue(assertThrows(IllegalArgumentException.class, () -> OpampClient.Config.builder("ws://x").mode(OpampClient.Mode.PLATFORM).proxy("socks5://proxy:1080").build())
        .getMessage().contains("SOCKS"));
    assertThrows(IllegalStateException.class, () -> OpampClient.Config.builder("ws://x")
        .httpClient(java.net.http.HttpClient.newHttpClient()).proxy("http://proxy:3128").build());
    assertThrows(IllegalStateException.class, () -> OpampClient.Config.builder("ws://x")
        .httpClient(java.net.http.HttpClient.newHttpClient()).tls(trusting(CA)).build());
    assertThrows(IllegalStateException.class, () -> OpampClient.Config.builder("ws://x")
        .httpClient(java.net.http.HttpClient.newHttpClient()).directConnection().build());
    assertThrows(IllegalStateException.class, () -> OpampClient.Config.builder("ws://x")
        .proxy("http://proxy:3128").directConnection().build());
  }

  // covers: RULE-TLS-02
  @Test
  void aKeyThatIsNotTheCertificatesIsRefused() throws Exception {
    // The server's EC key with the client's certificate: same algorithm, another key.
    IllegalArgumentException e = assertThrows(IllegalArgumentException.class,
        () -> Tls.builder().clientCertificate(pem("client.pem"), pem("server-key.pem")));
    assertTrue(e.getMessage().contains("does not match"), e.getMessage());
    assertTrue(e.getMessage().contains("opamp-test-agent"), e.getMessage());
    // An RSA key with an EC certificate.
    java.security.KeyPairGenerator g = java.security.KeyPairGenerator.getInstance("RSA");
    g.initialize(2048);
    String rsa = "-----BEGIN PRIVATE KEY-----\n"
        + Base64.getMimeEncoder().encodeToString(g.generateKeyPair().getPrivate().getEncoded())
        + "\n-----END PRIVATE KEY-----\n";
    e = assertThrows(IllegalArgumentException.class, () -> Tls.builder().clientCertificate(pem("client.pem"), rsa));
    assertTrue(e.getMessage().contains("the key is RSA"), e.getMessage());
    // The certificate's own key is fine, and so is its chain after it.
    Tls.builder().clientCertificate(pem("client.pem") + CA, pem("client-key.pem")).build();
  }

  /**
   * Any user and password, percent-encoded into the URL, come back as they were, and as the Basic
   * credentials (a seeded generator: OPAMP_PROP_SEED replays one run).
   */
  @Test
  void proxyCredentialsRoundTrip() {
    long first = Long.parseLong(Objects.requireNonNullElse(System.getenv("OPAMP_PROP_SEED"), "1"));
    int runs = System.getenv("OPAMP_PROP_SEED") != null ? 1 : 500;
    for (long seed = first; seed < first + runs; seed++) {
      SplittableRandom r = new SplittableRandom(seed);
      String user = text(r, 1);
      String password = text(r, 0);
      String url = "http://" + enc(user) + ":" + enc(password) + "@proxy:1";
      Transport.Proxy p = Transport.Proxy.parse(url);
      String why = "seed " + seed + ": " + url;
      assertEquals(user, p.user(), why);
      assertEquals(password, p.password(), why);
      assertEquals(user + ":" + password,
          new String(Base64.getDecoder().decode(p.authorization().substring("Basic ".length())), StandardCharsets.UTF_8), why);
      assertEquals("http://" + user + ":<redacted>@proxy:1", p.toString(), why);
    }
  }

  private static final String CHARS = "abcXYZ019 :@/?#[]%+&=~!$'()*,;é€\u00e0-_.";

  private static String text(SplittableRandom r, int min) {
    StringBuilder b = new StringBuilder();
    for (int i = min + r.nextInt(12); i > 0; i--) {
      b.append(CHARS.charAt(r.nextInt(CHARS.length())));
    }
    return b.toString();
  }

  private static String enc(String s) {
    return URLEncoder.encode(s, StandardCharsets.UTF_8).replace("+", "%20");
  }

  @Test
  void tunnelPropertyDecidesBasic() {
    assertTrue(Transport.tunnelBasicDisabled(null));
    assertTrue(Transport.tunnelBasicDisabled("Basic"));
    assertTrue(Transport.tunnelBasicDisabled("Digest, basic"));
    assertFalse(Transport.tunnelBasicDisabled(""));
    assertFalse(Transport.tunnelBasicDisabled("Digest"));
    // The tests run with it cleared (pom.xml): the proxy tests need it.
    assertFalse(Transport.tunnelBasicDisabled(System.getProperty(Transport.TUNNEL_DISABLED_SCHEMES)));
  }

  @Test
  void pemMistakesAreNamed() {
    // A PKCS #1 or SEC 1 key is taken (ShellTablesTest), but not a broken one.
    String rsa1 = "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n";
    assertTrue(assertThrows(IllegalArgumentException.class, () -> Tls.builder().clientCertificate(pem("client.pem"), rsa1))
        .getMessage().contains("RSA private key"));
    String sec1 = "-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n";
    assertThrows(IllegalArgumentException.class, () -> Tls.builder().clientCertificate(pem("client.pem"), sec1));
    String enc = "-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY-----\n";
    assertTrue(assertThrows(IllegalArgumentException.class, () -> Tls.builder().clientCertificate(pem("client.pem"), enc))
        .getMessage().contains("encrypted"));
    assertThrows(IllegalArgumentException.class, () -> Tls.builder().clientCertificate(pem("client.pem"), pem("client.pem")));
    assertThrows(IllegalArgumentException.class, () -> Tls.builder().clientCertificate(pem("client-key.pem"), pem("client-key.pem")));
    assertThrows(IllegalArgumentException.class, () -> Tls.builder().trustedCa("not a certificate"));
    assertThrows(IllegalArgumentException.class,
        () -> Tls.builder().trustedCa("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"));
    // Text around the blocks, and several certificates in one PEM, are fine.
    assertEquals(2, Tls.certificates("subject=CN=ca\n" + CA + "\nissuer=x\n" + OTHER_CA).size());
    assertNotNull(Tls.privateKey("# the key\n" + pem("client-key.pem")));
  }

  /**
   * Downloads follow no redirect: each is reported with its Location resolved, through the
   * transport's proxy, with the headers the core gave (the core decides where they go).
   */
  @Test
  void downloadRedirectsAreReportedNotFollowed(@TempDir Path dir) throws Exception {
    try (Origin a = Origin.plain(); Origin b = Origin.plain(); HttpProxy proxy = new HttpProxy(null, null)) {
      Probe p = new Probe(Transport.build(trusting(CA), Transport.Proxy.parse("http://127.0.0.1:" + proxy.port()), false), dir);
      a.redirect("/here", "/file");
      a.redirect("/there", b.url("http") + "/file");
      CoreProto.Input here = p.download(a.url("http") + "/here");
      assertEquals(302, here.getDownloadResponse().getStatus());
      assertEquals(a.url("http") + "/file", here.getDownloadResponse().getLocation());
      assertEquals(a.port(), here.getDownloadResponse().getLocationOrigin().getPort());
      CoreProto.Input there = p.download(a.url("http") + "/there");
      assertEquals(b.url("http") + "/file", there.getDownloadResponse().getLocation());
      CoreProto.Input file = p.download(b.url("http") + "/file");
      assertEquals(200, file.getDownloadResponse().getStatus());
      assertArrayEquals(MessageDigest.getInstance("SHA-256").digest(Origin.FILE), file.getDownloadResponse().getSha256().toByteArray());
      assertEquals(2, a.requests.size(), a.requests.toString());
      assertEquals(1, b.requests.size(), b.requests.toString());
      assertTrue(proxy.seen.stream().allMatch(s -> s.startsWith("GET http://")), proxy.seen.toString());
    }
  }

  /** Runs one effect at a time on an {@link Executor} over a transport, and returns what came of it. */
  static final class Probe {
    private final LinkedBlockingQueue<CoreProto.Input> got = new LinkedBlockingQueue<>();
    private final Executor x;
    private long id;

    Probe(Transport t, Path dir) {
      x = new Executor(t, got::add, dir.resolve("state"));
    }

    CoreProto.Input post(String url) throws InterruptedException {
      x.run(CoreProto.Effect.newBuilder().setHttpRequest(CoreProto.HttpRequest.newBuilder()
          .setId(++id).setUrl(url + "/v1/opamp").setBody(ByteString.copyFromUtf8("hello"))
          .setTimeoutMs(5000).setMaxResponseBytes(1 << 20)).build());
      return next();
    }

    CoreProto.Input open(String url) throws InterruptedException {
      long i = ++id;
      x.run(CoreProto.Effect.newBuilder().setWsOpen(CoreProto.WsOpen.newBuilder()
          .setId(i).setUrl(url + "/v1/opamp").setMaxMessageBytes(1 << 20)).build());
      CoreProto.Input in = next();
      if (in.getKindCase() == CoreProto.Input.KindCase.WS_OPENED) {
        // Closed: reported when the closing handshake is done.
        x.run(CoreProto.Effect.newBuilder().setWsClose(CoreProto.Id.newBuilder().setId(i)).build());
        assertEquals(CoreProto.Input.KindCase.WS_CLOSED, next().getKindCase());
      }
      return in;
    }

    CoreProto.Input download(String url) throws InterruptedException {
      return download(url, true);
    }

    CoreProto.Input download(String url, boolean serverOrigin) throws InterruptedException {
      x.run(CoreProto.Effect.newBuilder().setDownload(CoreProto.Download.newBuilder()
          .setId(++id).setUrl(url).setMaxBytes(1 << 20).setTimeoutMs(5000).setServerOrigin(serverOrigin)
          .addHeaders(CoreProto.Header.newBuilder().setName("Authorization").setValue("t"))).build());
      return next();
    }

    private CoreProto.Input next() throws InterruptedException {
      CoreProto.Input in = got.poll(20, TimeUnit.SECONDS);
      assertNotNull(in, "nothing came");
      return in;
    }
  }

  /**
   * A server, plain or TLS, just enough for the client: a request gets 200 and a small body (a
   * redirect where one is set), a WebSocket upgrade gets 101 and the connection held until the
   * client goes. It records each request line, its headers, and the client certificate's subject.
   */
  static final class Origin implements AutoCloseable {
    static final byte[] FILE = "package".repeat(50).getBytes(StandardCharsets.US_ASCII);

    final List<String> requests = new CopyOnWriteArrayList<>();
    final List<Map<String, String>> headers = new CopyOnWriteArrayList<>();
    final List<String> peers = new CopyOnWriteArrayList<>();
    private final Map<String, String> redirects = new ConcurrentHashMap<>();
    private final ServerSocket listener;
    private final Set<Socket> sockets = ConcurrentHashMap.newKeySet();

    private Origin(ServerSocket listener) {
      this.listener = listener;
      start(this::accept);
    }

    static Origin plain() throws IOException {
      return new Origin(new ServerSocket(0, 50, InetAddress.getByName("127.0.0.1")));
    }

    /** With the server certificate the CA signed; {@code clientAuth}: require a client's. */
    static Origin tls(boolean clientAuth, String onlyProtocol) throws IOException {
      SSLContext ctx = Tls.builder().trustedCa(CA).clientCertificate(pem("server.pem"), pem("server-key.pem")).build().context();
      SSLServerSocket s = (SSLServerSocket) ctx.getServerSocketFactory().createServerSocket(0, 50, InetAddress.getByName("127.0.0.1"));
      s.setNeedClientAuth(clientAuth);
      if (onlyProtocol != null) {
        s.setEnabledProtocols(new String[] {onlyProtocol});
      }
      return new Origin(s);
    }

    int port() {
      return listener.getLocalPort();
    }

    String url(String scheme) {
      return scheme + "://127.0.0.1:" + port();
    }

    void redirect(String path, String location) {
      redirects.put(path, location);
    }

    @Override
    public void close() throws IOException {
      listener.close();
      for (Socket s : sockets) {
        s.close();
      }
    }

    private void accept() {
      while (!listener.isClosed()) {
        try {
          Socket s = listener.accept();
          sockets.add(s);
          start(() -> serve(s));
        } catch (IOException e) {
          return;
        }
      }
    }

    private void serve(Socket s) {
      try (s) {
        InputStream in = new BufferedInputStream(s.getInputStream());
        OutputStream out = s.getOutputStream();
        Head h = Head.read(in);
        if (h == null) {
          return;
        }
        if (s instanceof SSLSocket ssl && ssl.getNeedClientAuth()) {
          peers.add(((X509Certificate) ssl.getSession().getPeerCertificates()[0]).getSubjectX500Principal().getName());
        }
        requests.add(h.line);
        headers.add(h.headers);
        in.readNBytes(Integer.parseInt(h.headers.getOrDefault("content-length", "0")));
        String path = URI.create(h.line.split(" ")[1]).getPath();
        if ("websocket".equalsIgnoreCase(h.headers.get("upgrade"))) {
          String accept = Base64.getEncoder().encodeToString(MessageDigest.getInstance("SHA-1")
              .digest((h.headers.get("sec-websocket-key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
                  .getBytes(StandardCharsets.US_ASCII)));
          out.write(("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
              + "Sec-WebSocket-Accept: " + accept + "\r\n\r\n").getBytes(StandardCharsets.US_ASCII));
          out.flush();
          // Hold it; answer the client's close frame with an empty close, and go.
          while (in.read() >= 0) {
            out.write(new byte[] {(byte) 0x88, 0});
            out.flush();
          }
          return;
        }
        String location = redirects.get(path);
        byte[] body = path.equals("/file") ? FILE : new byte[] {1, 2, 3};
        // One request a connection: the proxy passes on one too.
        String status = location != null ? "302 Found\r\nLocation: " + location : "200 OK";
        out.write(("HTTP/1.1 " + status + "\r\nContent-Length: " + (location != null ? 0 : body.length)
            + "\r\nConnection: close\r\n\r\n")
            .getBytes(StandardCharsets.US_ASCII));
        if (location == null) {
          out.write(body);
        }
        out.flush();
      } catch (Exception e) {
        // the connection went, or the handshake failed
      } finally {
        sockets.remove(s);
      }
    }
  }

  /**
   * An HTTP proxy: CONNECT tunnels, and requests in absolute form passed on to the server. With a
   * user, it wants Basic Proxy-Authorization, and answers 407 without it. It records what it was
   * asked for ({@code METHOD target}).
   */
  static final class HttpProxy implements AutoCloseable {
    final List<String> seen = new CopyOnWriteArrayList<>();
    private final String expected;
    private final ServerSocket listener;
    private final Set<Socket> sockets = ConcurrentHashMap.newKeySet();

    HttpProxy(String user, String password) throws IOException {
      this(user, password, null);
    }

    /** {@code tls}: an {@code https://} proxy, TLS with this server context (core mode's). */
    HttpProxy(String user, String password, SSLContext tls) throws IOException {
      listener = tls == null ? new ServerSocket(0, 50, InetAddress.getByName("127.0.0.1"))
          : tls.getServerSocketFactory().createServerSocket(0, 50, InetAddress.getByName("127.0.0.1"));
      expected = user == null ? null
          : "Basic " + Base64.getEncoder().encodeToString((user + ":" + password).getBytes(StandardCharsets.UTF_8));
      start(() -> {
        while (!listener.isClosed()) {
          try {
            Socket s = listener.accept();
            sockets.add(s);
            start(() -> serve(s));
          } catch (IOException e) {
            return;
          }
        }
      });
    }

    int port() {
      return listener.getLocalPort();
    }

    @Override
    public void close() throws IOException {
      listener.close();
      for (Socket s : sockets) {
        s.close();
      }
    }

    private void serve(Socket c) {
      try (c) {
        InputStream in = new BufferedInputStream(c.getInputStream());
        OutputStream out = c.getOutputStream();
        Head h = Head.read(in);
        if (h == null) {
          return;
        }
        if (expected != null && !expected.equals(h.headers.get("proxy-authorization"))) {
          out.write(("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"test\"\r\n"
              + "Content-Length: 0\r\nConnection: close\r\n\r\n").getBytes(StandardCharsets.US_ASCII));
          out.flush();
          return;
        }
        String[] parts = h.line.split(" ");
        seen.add(parts[0] + " " + parts[1]);
        if (parts[0].equals("CONNECT")) {
          int colon = parts[1].lastIndexOf(':');
          try (Socket up = new Socket(parts[1].substring(0, colon), Integer.parseInt(parts[1].substring(colon + 1)))) {
            sockets.add(up);
            out.write("HTTP/1.1 200 Connection established\r\n\r\n".getBytes(StandardCharsets.US_ASCII));
            out.flush();
            pipe(in, out, up);
          }
          return;
        }
        URI target = URI.create(parts[1]);
        try (Socket up = new Socket(target.getHost(), target.getPort())) {
          sockets.add(up);
          StringBuilder head = new StringBuilder(parts[0] + " " + target.getRawPath() + " " + parts[2] + "\r\n");
          h.headers.forEach((k, v) -> {
            if (!k.startsWith("proxy-") && !k.equals("connection")) {
              head.append(k).append(": ").append(v).append("\r\n");
            }
          });
          head.append("connection: close\r\n\r\n");
          up.getOutputStream().write(head.toString().getBytes(StandardCharsets.US_ASCII));
          pipe(in, out, up);
        }
      } catch (Exception e) {
        // the connection went
      } finally {
        sockets.remove(c);
      }
    }

    /** Bytes both ways until the server's side ends. */
    private static void pipe(InputStream in, OutputStream out, Socket up) throws IOException {
      start(() -> {
        try {
          in.transferTo(up.getOutputStream());
        } catch (IOException e) {
          // gone
        }
      });
      up.getInputStream().transferTo(out);
    }
  }

  /** A request line and its headers (names in lower case). */
  record Head(String line, Map<String, String> headers) {
    static Head read(InputStream in) throws IOException {
      String line = line(in);
      if (line == null || line.isEmpty()) {
        return null;
      }
      Map<String, String> headers = new LinkedHashMap<>();
      for (String l = line(in); l != null && !l.isEmpty(); l = line(in)) {
        int colon = l.indexOf(':');
        headers.put(l.substring(0, colon).trim().toLowerCase(Locale.ROOT), l.substring(colon + 1).trim());
      }
      return new Head(line, headers);
    }

    private static String line(InputStream in) throws IOException {
      ByteArrayOutputStream b = new ByteArrayOutputStream();
      for (int c = in.read(); c != '\n'; c = in.read()) {
        if (c < 0) {
          return b.size() == 0 ? null : b.toString(StandardCharsets.US_ASCII);
        }
        if (c != '\r') {
          b.write(c);
        }
      }
      return b.toString(StandardCharsets.US_ASCII);
    }
  }

  private static void start(Runnable r) {
    Thread t = new Thread(r);
    t.setDaemon(true);
    t.start();
  }
}
