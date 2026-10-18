// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import java.net.InetSocketAddress;
import java.net.ProxySelector;
import java.net.URI;
import java.net.URISyntaxException;
import java.net.URLDecoder;
import java.net.http.HttpClient;
import java.nio.charset.StandardCharsets;
import java.util.Base64;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.atomic.AtomicBoolean;
import javax.net.ssl.SSLContext;
import javax.net.ssl.SSLParameters;

/**
 * What the client reaches servers with: one {@link HttpClient}, used for OpAMP over HTTP and
 * WebSocket and for package downloads, and the headers every request carries (the proxy's
 * credentials). Built from the options, or the application's own client as it is.
 */
final class Transport {
  /** The JDK property listing the schemes it refuses to send to a proxy for a CONNECT tunnel. */
  static final String TUNNEL_DISABLED_SCHEMES = "jdk.http.auth.tunneling.disabledSchemes";
  private static final AtomicBoolean WARNED = new AtomicBoolean();

  final HttpClient http;
  /** Headers for every request, upgrade and download: name, value. */
  final List<Map.Entry<String, String>> headers;
  /** The client is ours (built here), not the application's: it goes with the OpAMP client. */
  final boolean owned;

  private Transport(HttpClient http, List<Map.Entry<String, String>> headers, boolean owned) {
    this.http = http;
    this.headers = headers;
    this.owned = owned;
  }

  /** The application's client, as it is. */
  static Transport of(HttpClient http) {
    return new Transport(http, List.of(), false);
  }

  /**
   * Shut our own client down at once, every exchange and connection with it, where the JDK can
   * (HttpClient.shutdownNow, Java 21 and later; looked up, as this is built for 17): a request or a
   * connection still being made when the OpAMP client ends goes no further. The application's
   * client is left alone. The future completes when the JDK says the client has terminated (the
   * caller bounds the wait), or at once where it can't say.
   */
  CompletableFuture<Void> shutdown() {
    if (!owned || (http instanceof Lazy l && !l.made())) {
      return CompletableFuture.completedFuture(null);
    }
    try {
      HttpClient.class.getMethod("shutdownNow").invoke(http);
      java.lang.reflect.Method await = HttpClient.class.getMethod("awaitTermination", java.time.Duration.class);
      return CompletableFuture.runAsync(() -> {
        try {
          await.invoke(http, java.time.Duration.ofSeconds(30));
        } catch (ReflectiveOperationException e) {
          // gone as far as it goes
        }
      });
    } catch (ReflectiveOperationException | RuntimeException e) {
      // an older JDK: the aborts already done are what there is
      return CompletableFuture.completedFuture(null);
    }
  }

  /**
   * A client with these options (either may be null). With no proxy it uses the platform's proxy
   * settings, {@link ProxySelector#getDefault()} (http.proxyHost, https.proxyHost,
   * http.nonProxyHosts, java.net.useSystemProxies), unless {@code direct}. It follows no
   * redirects: every 3xx goes to the core as it is, a download's too.
   */
  static Transport build(Tls tls, Proxy proxy, boolean direct) {
    if (proxy != null && direct) {
      throw new IllegalArgumentException("a proxy and a direct connection: choose one");
    }
    HttpClient.Builder b = HttpClient.newBuilder().followRedirects(HttpClient.Redirect.NEVER);
    SSLContext ctx;
    String[] protocols;
    if (tls != null) {
      ctx = tls.context();
      protocols = tls.protocols();
    } else {
      try {
        ctx = SSLContext.getDefault();
      } catch (java.security.NoSuchAlgorithmException e) {
        throw new IllegalStateException(e);
      }
      protocols = Tls.builder().build().protocols();
    }
    SSLParameters params = ctx.getDefaultSSLParameters();
    params.setProtocols(protocols);
    b.sslContext(ctx).sslParameters(params);
    List<Map.Entry<String, String>> headers = List.of();
    if (direct) {
      b.proxy(HttpClient.Builder.NO_PROXY);
    }
    // With neither, the builder's proxy is left unset: the JDK then uses ProxySelector.getDefault()
    // for requests and WebSocket alike.
    if (proxy != null) {
      b.proxy(ProxySelector.of(InetSocketAddress.createUnresolved(proxy.host(), proxy.port())));
      if (proxy.user() != null) {
        // Sent with every request rather than on a 407 through an Authenticator: with one, the
        // JDK fails any 401 without a WWW-Authenticate header, which the core must see as a 401.
        headers = List.of(Map.entry("Proxy-Authorization", proxy.authorization()));
        if (tunnelBasicDisabled(System.getProperty(TUNNEL_DISABLED_SCHEMES)) && WARNED.compareAndSet(false, true)) {
          System.getLogger("dev.pydantic.opamp").log(System.Logger.Level.WARNING,
              "the proxy has credentials, but the JDK sends none through a CONNECT tunnel (https://, ws://, wss://)"
                  + " unless the JVM runs with -D" + TUNNEL_DISABLED_SCHEMES + "=");
        }
      }
    }
    return new Transport(b.build(), headers, true);
  }

  /**
   * {@link #build}'s transport, its HTTP client built only when first used: core mode's, for
   * platform effects (http_request, ws_open, download) a core may still hand it (the adversary
   * does), which its own core never does: then no HTTP client, and none of its threads, is ever
   * made. The proxy's credentials are checked now, as build does.
   */
  static Transport lazy(Tls tls, Proxy proxy, boolean direct) {
    if (proxy != null && direct) {
      throw new IllegalArgumentException("a proxy and a direct connection: choose one");
    }
    List<Map.Entry<String, String>> headers = proxy != null && proxy.user() != null
        ? List.of(Map.entry("Proxy-Authorization", proxy.authorization())) : List.of();
    return new Transport(new Lazy(() -> build(tls, proxy, direct).http), headers, true);
  }

  /** An HTTP client made when first used, then used as it is. */
  static final class Lazy extends HttpClient {
    private final java.util.function.Supplier<HttpClient> make;
    private HttpClient made;

    Lazy(java.util.function.Supplier<HttpClient> make) {
      this.make = make;
    }

    private synchronized HttpClient get() {
      if (made == null) {
        made = make.get();
      }
      return made;
    }

    /** Whether it was made (tests). */
    synchronized boolean made() {
      return made != null;
    }

    @Override
    public java.util.Optional<java.net.CookieHandler> cookieHandler() {
      return get().cookieHandler();
    }

    @Override
    public java.util.Optional<java.time.Duration> connectTimeout() {
      return get().connectTimeout();
    }

    @Override
    public Redirect followRedirects() {
      return HttpClient.Redirect.NEVER;
    }

    @Override
    public java.util.Optional<ProxySelector> proxy() {
      return get().proxy();
    }

    @Override
    public SSLContext sslContext() {
      return get().sslContext();
    }

    @Override
    public SSLParameters sslParameters() {
      return get().sslParameters();
    }

    @Override
    public java.util.Optional<java.net.Authenticator> authenticator() {
      return get().authenticator();
    }

    @Override
    public Version version() {
      return get().version();
    }

    @Override
    public java.util.Optional<java.util.concurrent.Executor> executor() {
      return get().executor();
    }

    @Override
    public <T> java.net.http.HttpResponse<T> send(java.net.http.HttpRequest req,
        java.net.http.HttpResponse.BodyHandler<T> handler) throws java.io.IOException, InterruptedException {
      return get().send(req, handler);
    }

    @Override
    public <T> CompletableFuture<java.net.http.HttpResponse<T>> sendAsync(java.net.http.HttpRequest req,
        java.net.http.HttpResponse.BodyHandler<T> handler) {
      return get().sendAsync(req, handler);
    }

    @Override
    public <T> CompletableFuture<java.net.http.HttpResponse<T>> sendAsync(java.net.http.HttpRequest req,
        java.net.http.HttpResponse.BodyHandler<T> handler, java.net.http.HttpResponse.PushPromiseHandler<T> push) {
      return get().sendAsync(req, handler, push);
    }

    @Override
    public java.net.http.WebSocket.Builder newWebSocketBuilder() {
      return get().newWebSocketBuilder();
    }

    // Java 21's shutdown (Transport.shutdown looks them up): the client made, if one was.

    public void shutdownNow() throws ReflectiveOperationException {
      HttpClient c;
      synchronized (this) {
        c = made;
      }
      if (c != null) {
        HttpClient.class.getMethod("shutdownNow").invoke(c);
      }
    }

    public boolean awaitTermination(java.time.Duration d) throws ReflectiveOperationException {
      HttpClient c;
      synchronized (this) {
        c = made;
      }
      return c == null || (Boolean) HttpClient.class.getMethod("awaitTermination", java.time.Duration.class).invoke(c, d);
    }
  }

  /**
   * Whether the JDK refuses Basic proxy credentials for CONNECT tunnels, given the property's value
   * (null: unset, and the JDK's default is {@code Basic}).
   */
  static boolean tunnelBasicDisabled(String property) {
    if (property == null) {
      return true;
    }
    for (String s : property.split(",")) {
      if (s.trim().equalsIgnoreCase("Basic")) {
        return true;
      }
    }
    return false;
  }

  /**
   * An HTTP proxy, {@code http://[user:password@]host[:port]}. The JDK's HTTP client speaks plain
   * HTTP to proxies only: no SOCKS and no {@code https://} proxies.
   */
  record Proxy(String host, int port, String user, String password) {
    static Proxy parse(String url) {
      URI u;
      try {
        u = new URI(url);
      } catch (URISyntaxException e) {
        throw new IllegalArgumentException("bad proxy URL: " + e.getMessage(), e);
      }
      String scheme = u.getScheme() == null ? "" : u.getScheme().toLowerCase(Locale.ROOT);
      switch (scheme) {
        case "http" -> {}
        case "https" -> throw new IllegalArgumentException(
            "https:// proxies are not supported: the JDK's HTTP client talks to proxies in plain HTTP; use http://");
        case "socks", "socks4", "socks4a", "socks5", "socks5h" -> throw new IllegalArgumentException(
            "SOCKS proxies are not supported: the JDK's HTTP client only uses HTTP proxies (http://)");
        default -> throw new IllegalArgumentException("the proxy URL must be http://host:port");
      }
      if (u.getHost() == null) {
        throw new IllegalArgumentException("the proxy URL has no host");
      }
      if ((u.getRawPath() != null && !u.getRawPath().isEmpty() && !u.getRawPath().equals("/"))
          || u.getRawQuery() != null || u.getRawFragment() != null) {
        throw new IllegalArgumentException("the proxy URL must be http://host:port, with nothing after the port");
      }
      String user = null;
      String password = null;
      String info = u.getRawUserInfo();
      if (info != null) {
        int colon = info.indexOf(':');
        user = decode(colon < 0 ? info : info.substring(0, colon));
        password = colon < 0 ? "" : decode(info.substring(colon + 1));
      }
      String host = u.getHost().startsWith("[") ? u.getHost().substring(1, u.getHost().length() - 1) : u.getHost();
      return new Proxy(host, u.getPort() < 0 ? 80 : u.getPort(), user, password);
    }

    private static String decode(String s) {
      return URLDecoder.decode(s.replace("+", "%2B"), StandardCharsets.UTF_8);
    }

    /** The Proxy-Authorization value: Basic. */
    String authorization() {
      return "Basic " + Base64.getEncoder().encodeToString((user + ":" + password).getBytes(StandardCharsets.UTF_8));
    }

    @Override
    public String toString() {
      return "http://" + (user == null ? "" : user + ":<redacted>@") + host + ":" + port;
    }
  }
}
