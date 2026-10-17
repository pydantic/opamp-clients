// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import dev.pydantic.opamp.TransportTest.HttpProxy;
import dev.pydantic.opamp.TransportTest.Origin;
import java.io.DataInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.TimeUnit;
import java.util.function.BooleanSupplier;
import javax.net.ssl.SSLContext;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;

/**
 * Rust mode's transport, through the whole client against real servers: TLS from the operator's
 * settings (the CAs given, a client certificate, the versions allowed, a certificate verified for
 * a name or an address), and every proxy the core speaks: HTTP with credentials, {@code https://}
 * (TLS to the proxy, TLS to the server inside it), SOCKS5 with credentials (the proxy resolving
 * the name, or the client), and the environment's by Rust mode's rule. The servers and the HTTP
 * proxy are {@link TransportTest}'s.
 */
class RustTransportTest {
  static final byte[] UID = new byte[16];

  static OpampClient.Config.Builder rust(String endpoint) {
    return OpampClient.Config.builder(endpoint, UID).mode(OpampClient.Mode.RUST).heartbeatInterval(Duration.ofSeconds(30))
        .connectTimeout(Duration.ofSeconds(5)).requestTimeout(Duration.ofSeconds(5));
  }

  /**
   * A Rust-mode client with these settings until {@code arrived} (the server has its request) or
   * {@code ms} have passed; then stopped. What it was told.
   */
  static List<Notification> run(OpampClient.Config.Builder b, BooleanSupplier arrived, long ms) throws Exception {
    List<Notification> told = new CopyOnWriteArrayList<>();
    OpampClient c = OpampClient.start(b.build(), OpampClient.Status.of(Opamp.AgentDescription.getDefaultInstance()), told::add);
    for (long until = System.nanoTime() + ms * 1_000_000; !arrived.getAsBoolean() && System.nanoTime() < until; ) {
      Thread.sleep(10);
    }
    c.stop().get(15, TimeUnit.SECONDS);
    return told;
  }

  /** The client reaches {@code endpoint} (the request is there within 15 s). */
  static void reaches(OpampClient.Config.Builder b, List<String> requests) throws Exception {
    int before = requests.size();
    List<Notification> told = run(b, () -> requests.size() > before, 15_000);
    assertTrue(requests.size() > before, "no request came; told " + told);
  }

  /** Nothing reaches the server in 2 s. */
  static void reachesNothing(OpampClient.Config.Builder b, List<String> requests) throws Exception {
    int before = requests.size();
    run(b, () -> requests.size() > before, 2_000);
    assertEquals(before, requests.size(), "nothing reaches the server: " + requests);
  }

  // covers: RULE-TLS-01
  @Test
  void trustsTheGivenCaForANameAndAnAddress() throws Exception {
    try (Origin origin = Origin.tls(false, null)) {
      for (String scheme : List.of("https", "wss")) {
        reaches(rust(origin.url(scheme) + "/v1/opamp").tls(TransportTest.trusting(TransportTest.CA)).directConnection(), origin.requests);
        // The certificate names localhost too.
        reaches(rust(scheme + "://localhost:" + origin.port() + "/v1/opamp").tls(TransportTest.trusting(TransportTest.CA))
            .directConnection(), origin.requests);
        reaches(rust(origin.url(scheme) + "/v1/opamp").tls(Tls.builder().trustedCa(TransportTest.CA).includeSystemRoots(true).build())
            .directConnection(), origin.requests);
      }
    }
  }

  // covers: RULE-TLS-01
  @Test
  void sendsNothingToAServerItCannotVerify() throws Exception {
    try (Origin origin = Origin.tls(false, null)) {
      for (String scheme : List.of("https", "wss")) {
        reachesNothing(rust(origin.url(scheme) + "/v1/opamp").directConnection(), origin.requests);
        reachesNothing(rust(origin.url(scheme) + "/v1/opamp").tls(TransportTest.trusting(TransportTest.OTHER_CA)).directConnection(),
            origin.requests);
      }
    }
  }

  // covers: RULE-TLS-02
  @Test
  void presentsTheClientCertificate() throws Exception {
    try (Origin origin = Origin.tls(true, null)) {
      for (String scheme : List.of("https", "wss")) {
        reaches(rust(origin.url(scheme) + "/v1/opamp").tls(TransportTest.withClientCert()).directConnection(), origin.requests);
        reachesNothing(rust(origin.url(scheme) + "/v1/opamp").tls(TransportTest.trusting(TransportTest.CA)).directConnection(),
            origin.requests);
      }
      assertFalse(origin.peers.isEmpty());
      for (String peer : origin.peers) {
        assertEquals("CN=opamp-test-agent", peer);
      }
    }
  }

  // covers: RULE-TLS-01
  @Test
  void keepsToTheTlsVersionsAllowed() throws Exception {
    try (Origin origin = Origin.tls(false, "TLSv1.2")) {
      reaches(rust(origin.url("https") + "/v1/opamp").tls(TransportTest.trusting(TransportTest.CA)).directConnection(), origin.requests);
      Tls only13 = Tls.builder().trustedCa(TransportTest.CA).minVersion(Tls.Version.TLS_1_3).build();
      reachesNothing(rust(origin.url("https") + "/v1/opamp").tls(only13).directConnection(), origin.requests);
    }
  }

  // covers: RULE-PROXY-01
  @Test
  void goesThroughAnHttpProxyWithCredentials() throws Exception {
    try (Origin plain = Origin.plain(); Origin tls = Origin.tls(false, null); HttpProxy proxy = new HttpProxy("agent", "p@ss:w")) {
      String via = "http://agent:p%40ss%3Aw@127.0.0.1:" + proxy.port();
      for (String scheme : List.of("http", "ws")) {
        reaches(rust(plain.url(scheme) + "/v1/opamp").proxy(via), plain.requests);
      }
      for (String scheme : List.of("https", "wss")) {
        reaches(rust(tls.url(scheme) + "/v1/opamp").proxy(via).tls(TransportTest.trusting(TransportTest.CA)), tls.requests);
      }
      assertTrue(proxy.seen.size() >= 4, proxy.seen.toString());
      for (String s : proxy.seen) {
        assertTrue(s.contains("127.0.0.1:" + plain.port()) || s.contains("127.0.0.1:" + tls.port()), s);
      }
      // The credentials are the proxy's: the server never sees them.
      for (Map<String, String> h : plain.headers) {
        assertNull(h.get("proxy-authorization"), h.toString());
      }
      // Wrong ones get no further.
      reachesNothing(rust(plain.url("http") + "/v1/opamp").proxy("http://agent:wrong@127.0.0.1:" + proxy.port()), plain.requests);
    }
  }

  /** An https:// proxy: TLS to the proxy, verified with the operator's CAs; TLS to the server inside it. */
  // covers: RULE-PROXY-01, RULE-TLS-01
  @Test
  void goesThroughAnHttpsProxyWithTlsInsideTls() throws Exception {
    SSLContext server = Tls.builder().trustedCa(TransportTest.CA)
        .clientCertificate(TransportTest.pem("server.pem"), TransportTest.pem("server-key.pem")).build().context();
    try (Origin plain = Origin.plain(); Origin tls = Origin.tls(false, null); HttpProxy proxy = new HttpProxy(null, null, server)) {
      String via = "https://localhost:" + proxy.port();
      reaches(rust(plain.url("http") + "/v1/opamp").proxy(via).tls(TransportTest.trusting(TransportTest.CA)), plain.requests);
      reaches(rust(plain.url("ws") + "/v1/opamp").proxy(via).tls(TransportTest.trusting(TransportTest.CA)), plain.requests);
      reaches(rust(tls.url("https") + "/v1/opamp").proxy(via).tls(TransportTest.trusting(TransportTest.CA)), tls.requests);
      reaches(rust(tls.url("wss") + "/v1/opamp").proxy(via).tls(TransportTest.trusting(TransportTest.CA)), tls.requests);
      // A proxy it can't verify gets nothing.
      int seen = proxy.seen.size();
      reachesNothing(rust(plain.url("http") + "/v1/opamp").proxy(via).tls(TransportTest.trusting(TransportTest.OTHER_CA)),
          plain.requests);
      assertEquals(seen, proxy.seen.size());
    }
  }

  // covers: RULE-PROXY-01
  @Test
  void goesThroughASocks5Proxy() throws Exception {
    try (Origin plain = Origin.plain(); Origin tls = Origin.tls(false, null); Socks5 socks = new Socks5("agent", "s3cret")) {
      String endpoint = "http://localhost:" + plain.port() + "/v1/opamp";
      for (String scheme : List.of("socks5", "socks5h")) {
        String via = scheme + "://agent:s3cret@127.0.0.1:" + socks.port();
        // The proxy resolves the name...
        reaches(rust(endpoint).proxy(via), plain.requests);
        assertEquals("name localhost:" + plain.port(), socks.seen.get(socks.seen.size() - 1));
        // ...unless the client does.
        reaches(rust(endpoint).proxy(via).proxyResolveLocally(true), plain.requests);
        assertEquals("address 127.0.0.1:" + plain.port(), socks.seen.get(socks.seen.size() - 1));
      }
      reaches(rust(tls.url("wss") + "/v1/opamp").proxy("socks5://agent:s3cret@127.0.0.1:" + socks.port())
          .tls(TransportTest.trusting(TransportTest.CA)), tls.requests);
      reachesNothing(rust(endpoint).proxy("socks5://agent:wrong@127.0.0.1:" + socks.port()), plain.requests);
    }
  }

  /** No proxy configured: the environment's, by Rust mode's rule; directConnection ignores it. */
  // covers: RULE-PROXY-01, RULE-PROXY-02
  @Test
  void takesTheProxyFromTheEnvironment() throws Exception {
    try (Origin plain = Origin.plain(); HttpProxy proxy = new HttpProxy(null, null)) {
      String endpoint = plain.url("http") + "/v1/opamp";
      String via = "http://127.0.0.1:" + proxy.port();
      assertTrue(throughProxy(rust(endpoint).environment(Map.of("http_proxy", via)), plain, proxy));
      assertFalse(throughProxy(rust(endpoint).environment(Map.of("HTTP_PROXY", via, "NO_PROXY", "127.0.0.0/8")), plain, proxy));
      assertFalse(throughProxy(rust(endpoint).environment(Map.of("HTTP_PROXY", via, "REQUEST_METHOD", "GET")), plain, proxy));
      assertFalse(throughProxy(rust(endpoint).environment(Map.of("HTTP_PROXY", via)).directConnection(), plain, proxy));
      assertFalse(throughProxy(rust(endpoint).environment(Map.of("HTTPS_PROXY", via)), plain, proxy));
      // NO_PROXY=* is for names: an address still goes through the proxy (the table's no-proxy-star-address).
      assertTrue(throughProxy(rust(endpoint).environment(Map.of("ALL_PROXY", via, "NO_PROXY", "*")), plain, proxy));
    }
  }

  /** Whether the client reached {@code origin} through {@code proxy}. */
  private static boolean throughProxy(OpampClient.Config.Builder b, Origin origin, HttpProxy proxy) throws Exception {
    int before = proxy.seen.size();
    reaches(b, origin.requests);
    return proxy.seen.size() > before;
  }

  /**
   * A SOCKS5 proxy (RFC 1928), username and password (RFC 1929) if given: CONNECT only, then the
   * bytes both ways. It records each destination ({@code name host:port} or {@code address
   * host:port}).
   */
  static final class Socks5 implements AutoCloseable {
    final List<String> seen = new CopyOnWriteArrayList<>();
    private final String user;
    private final String password;
    private final ServerSocket listener = new ServerSocket(0, 50, InetAddress.getByName("127.0.0.1"));
    private final Set<Socket> sockets = ConcurrentHashMap.newKeySet();

    Socks5(String user, String password) throws IOException {
      this.user = user;
      this.password = password;
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
        DataInputStream in = new DataInputStream(c.getInputStream());
        OutputStream out = c.getOutputStream();
        if (in.readUnsignedByte() != 5) {
          return;
        }
        byte[] methods = new byte[in.readUnsignedByte()];
        in.readFully(methods);
        int want = user == null ? 0 : 2;
        boolean offered = false;
        for (byte m : methods) {
          offered |= m == want;
        }
        if (!offered) {
          out.write(new byte[] {5, (byte) 0xff});
          return;
        }
        out.write(new byte[] {5, (byte) want});
        if (want == 2) {
          in.readUnsignedByte(); // the subnegotiation's version, 1
          byte[] u = new byte[in.readUnsignedByte()];
          in.readFully(u);
          byte[] p = new byte[in.readUnsignedByte()];
          in.readFully(p);
          boolean ok = user.equals(new String(u, StandardCharsets.UTF_8)) && password.equals(new String(p, StandardCharsets.UTF_8));
          out.write(new byte[] {1, (byte) (ok ? 0 : 1)});
          if (!ok) {
            return;
          }
        }
        in.readUnsignedByte(); // 5
        int cmd = in.readUnsignedByte();
        in.readUnsignedByte(); // reserved
        int type = in.readUnsignedByte();
        String host;
        String kind;
        switch (type) {
          case 1 -> {
            byte[] a = new byte[4];
            in.readFully(a);
            host = InetAddress.getByAddress(a).getHostAddress();
            kind = "address";
          }
          case 4 -> {
            byte[] a = new byte[16];
            in.readFully(a);
            host = InetAddress.getByAddress(a).getHostAddress();
            kind = "address";
          }
          default -> {
            byte[] name = new byte[in.readUnsignedByte()];
            in.readFully(name);
            host = new String(name, StandardCharsets.US_ASCII);
            kind = "name";
          }
        }
        int port = in.readUnsignedShort();
        seen.add(kind + " " + host + ":" + port);
        if (cmd != 1) {
          out.write(new byte[] {5, 7, 0, 1, 0, 0, 0, 0, 0, 0});
          return;
        }
        try (Socket up = new Socket(host, port)) {
          sockets.add(up);
          out.write(new byte[] {5, 0, 0, 1, 127, 0, 0, 1, 0, 0});
          out.flush();
          start(() -> {
            try {
              in.transferTo(up.getOutputStream());
            } catch (IOException e) {
              // gone
            }
          });
          InputStream from = up.getInputStream();
          from.transferTo(out);
        }
      } catch (IOException e) {
        // the connection went
      } finally {
        sockets.remove(c);
      }
    }

    private static void start(Runnable r) {
      Thread t = new Thread(r);
      t.setDaemon(true);
      t.start();
    }
  }
}
