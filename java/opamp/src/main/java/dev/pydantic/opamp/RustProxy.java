// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import java.math.BigInteger;
import java.net.URI;
import java.net.URISyntaxException;
import java.util.Locale;
import java.util.Map;

/**
 * Rust mode's proxy: the answer to the core's {@code proxy_query} for a URL. A proxy the operator
 * configured is answered as given (its credentials with it, the core uses them); {@link
 * OpampClient.Config.Builder#directConnection} is answered with none; otherwise the environment
 * decides, by the one rule every SDK follows in Rust mode (tests/shell-tables/README.md, "Rust
 * mode's proxy rule", the reference's: the Rust client's), with no differences from
 * proxy.json. Platform mode keeps the JDK's convention ({@code ProxySelector}), and never reads
 * these variables.
 *
 * <p>Nothing here resolves a name: an IP address is recognised by its text alone.
 */
final class RustProxy {
  private RustProxy() {}

  /**
   * The proxy URL the environment names for {@code url} (a URL the core accepts), or null for
   * none: {@code env} is the process environment, or a test's.
   */
  static String fromEnvironment(Map<String, String> env, String url) {
    // 1. Under CGI, HTTP_PROXY may be a request's Proxy header (httpoxy): no variable is taken.
    if (env.containsKey("REQUEST_METHOD")) {
      return null;
    }
    String scheme = scheme(url);
    boolean secure = scheme.equals("https") || scheme.equals("wss");
    // 3. The destination's own variable, else ALL_PROXY.
    String named = proxy(env, secure ? "HTTPS_PROXY" : "HTTP_PROXY");
    if (named == null) {
      named = proxy(env, "ALL_PROXY");
    }
    if (named == null) {
      return null;
    }
    // 4. NO_PROXY.
    String noProxy = read(env, "NO_PROXY");
    return noProxy != null && bypassed(noProxy, host(url)) ? null : named;
  }

  /** 2. The upper-case name if it is set at all (even empty), else the lower-case one. */
  private static String read(Map<String, String> env, String upper) {
    String v = env.get(upper);
    return v != null ? v : env.get(upper.toLowerCase(Locale.ROOT));
  }

  /** A proxy variable, trimmed: empty (or spaces) is unset, with no fall back to the other case. */
  private static String proxy(Map<String, String> env, String upper) {
    String v = read(env, upper);
    if (v == null) {
      return null;
    }
    v = v.strip();
    return v.isEmpty() ? null : v;
  }

  /** The URL's scheme, lower case. */
  static String scheme(String url) {
    int colon = url.indexOf(':');
    return colon < 0 ? "" : url.substring(0, colon).toLowerCase(Locale.ROOT);
  }

  /** The URL's host: a name, or an address without its brackets. */
  static String host(String url) {
    int start = url.indexOf("://");
    start = start < 0 ? 0 : start + 3;
    int end = start;
    while (end < url.length() && "/?#".indexOf(url.charAt(end)) < 0) {
      end++;
    }
    String authority = url.substring(start, end);
    authority = authority.substring(authority.lastIndexOf('@') + 1);
    if (authority.startsWith("[")) {
      int close = authority.indexOf(']');
      return close < 0 ? authority.substring(1) : authority.substring(1, close);
    }
    int colon = authority.indexOf(':');
    return colon < 0 ? authority : authority.substring(0, colon);
  }

  /**
   * Whether NO_PROXY {@code list} sends {@code host} (a name, or an address without brackets)
   * directly: an address by the address and network entries, a name by the name entries.
   */
  static boolean bypassed(String list, String host) {
    Ip ip = Ip.parse(host);
    String name = host.toLowerCase(Locale.ROOT);
    for (String raw : list.split(",", -1)) {
      String entry = raw.strip();
      if (entry.isEmpty()) {
        continue;
      }
      // An address entry: a network (address/prefix) or an address.
      int slash = entry.indexOf('/');
      Ip net = Ip.parse(slash < 0 ? entry : entry.substring(0, slash));
      if (net != null) {
        int bits = net.v6 ? 128 : 32;
        int prefix = slash < 0 ? bits : decimal(entry.substring(slash + 1), bits);
        if (prefix >= 0) {
          if (ip != null && ip.v6 == net.v6 && ip.value.shiftRight(bits - prefix).equals(net.value.shiftRight(bits - prefix))) {
            return true;
          }
          continue;
        }
      }
      // A name entry: never an address's.
      if (ip != null) {
        continue;
      }
      String d = entry.toLowerCase(Locale.ROOT);
      if (d.equals(name) || (d.startsWith(".") && d.substring(1).equals(name))) {
        return true;
      }
      if (name.endsWith(d)) {
        if (d.startsWith(".") || (name.length() > d.length() && name.charAt(name.length() - d.length() - 1) == '.')) {
          return true;
        }
      } else if (d.equals("*")) {
        return true;
      }
    }
    return false;
  }

  /** A prefix length: decimal, no leading zeros, at most {@code max}; -1 if it isn't one. */
  private static int decimal(String s, int max) {
    if (!s.matches("0|[1-9][0-9]{0,2}")) {
      return -1;
    }
    int n = Integer.parseInt(s);
    return n <= max ? n : -1;
  }

  /** An IP address, from its text only: its family and value. */
  record Ip(boolean v6, BigInteger value) {
    /**
     * {@code text} as an address, or null for anything else (a name: nothing is resolved). IPv4:
     * four decimal parts, 0 to 255, without leading zeros; IPv6: its text forms (an IPv4 tail
     * too), without a zone or brackets.
     */
    static Ip parse(String text) {
      Long four = v4(text);
      if (four != null) {
        return new Ip(false, BigInteger.valueOf(four));
      }
      if (text.indexOf(':') < 0 || !text.matches("[0-9a-fA-F:.]+")) {
        return null;
      }
      String groups = text;
      int lastColon = text.lastIndexOf(':');
      if (text.substring(lastColon + 1).indexOf('.') >= 0) {
        // An IPv4 tail is two groups.
        Long tail = v4(text.substring(lastColon + 1));
        if (tail == null) {
          return null;
        }
        groups = text.substring(0, lastColon + 1) + Long.toHexString(tail >> 16) + ":" + Long.toHexString(tail & 0xffff);
      }
      String[] halves = groups.split("::", -1);
      if (halves.length > 2) {
        return null;
      }
      String[] head = side(halves[0]);
      String[] tail = halves.length == 2 ? side(halves[1]) : new String[0];
      if (head == null || tail == null) {
        return null;
      }
      int missing = 8 - head.length - tail.length;
      if (halves.length == 2 ? missing < 1 : missing != 0) {
        return null;
      }
      BigInteger n = BigInteger.ZERO;
      for (String g : head) {
        n = n.shiftLeft(16).or(BigInteger.valueOf(Integer.parseInt(g, 16)));
      }
      n = n.shiftLeft(16 * missing);
      for (String g : tail) {
        n = n.shiftLeft(16).or(BigInteger.valueOf(Integer.parseInt(g, 16)));
      }
      return new Ip(true, n);
    }

    /** Whether {@code host} is an address (IPv6 with or without brackets), not a name. */
    static boolean literal(String host) {
      String bare = host.startsWith("[") && host.endsWith("]") ? host.substring(1, host.length() - 1) : host;
      return parse(bare) != null;
    }

    private static String[] side(String h) {
      if (h.isEmpty()) {
        return new String[0];
      }
      String[] g = h.split(":", -1);
      for (String x : g) {
        if (!x.matches("[0-9a-fA-F]{1,4}")) {
          return null;
        }
      }
      return g;
    }

    private static Long v4(String t) {
      String[] parts = t.split("\\.", -1);
      if (parts.length != 4) {
        return null;
      }
      long n = 0;
      for (String p : parts) {
        if (!p.matches("0|[1-9][0-9]{0,2}") || Integer.parseInt(p) > 255) {
          return null;
        }
        n = (n << 8) | Integer.parseInt(p);
      }
      return n;
    }
  }

  /**
   * A proxy the operator configured for Rust mode, checked: {@code http://}, {@code https://},
   * {@code socks5://} or {@code socks5h://}, a host, an optional port and {@code user:password@},
   * nothing after the port. The URL is answered as it is (the core reads the credentials from it).
   */
  static String check(String url, boolean resolveLocally) {
    String given = url.strip();
    URI u;
    try {
      u = new URI(given);
    } catch (URISyntaxException e) {
      throw new IllegalArgumentException("bad proxy URL: " + e.getMessage(), e);
    }
    String scheme = u.getScheme() == null ? "" : u.getScheme().toLowerCase(Locale.ROOT);
    switch (scheme) {
      case "http", "https" -> {
        if (resolveLocally) {
          throw new IllegalArgumentException("proxyResolveLocally is for SOCKS5 proxies: an HTTP proxy resolves the server's name");
        }
      }
      case "socks5", "socks5h" -> {}
      case "socks", "socks4", "socks4a" -> throw new IllegalArgumentException("SOCKS4 proxies are not supported: use socks5:// or socks5h://");
      default -> throw new IllegalArgumentException(
          "the proxy URL must be http://, https://, socks5:// or socks5h://host:port");
    }
    if (u.getHost() == null) {
      throw new IllegalArgumentException("the proxy URL has no host");
    }
    if ((u.getRawPath() != null && !u.getRawPath().isEmpty() && !u.getRawPath().equals("/"))
        || u.getRawQuery() != null || u.getRawFragment() != null) {
      throw new IllegalArgumentException("the proxy URL must be scheme://host:port, with nothing after the port");
    }
    return given;
  }
}
