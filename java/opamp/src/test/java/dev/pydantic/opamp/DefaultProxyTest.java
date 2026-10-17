// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import dev.pydantic.opamp.TransportTest.HttpProxy;
import dev.pydantic.opamp.TransportTest.Origin;
import dev.pydantic.opamp.TransportTest.Probe;
import dev.pydantic.opamp.core.CoreProto;
import java.nio.file.Path;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * With no proxy configured, the client uses the platform's proxy settings (here the JDK's
 * http.proxyHost and http.nonProxyHosts properties, which ProxySelector.getDefault() reads) on
 * HTTP, WebSocket and downloads alike; directConnection ignores them. The properties are the
 * JVM's: each test puts back what was there.
 */
// covers: RULE-PROXY-01
class DefaultProxyTest {
  private static final List<String> PROPS =
      List.of("http.proxyHost", "http.proxyPort", "https.proxyHost", "https.proxyPort", "http.nonProxyHosts");
  private final Map<String, String> saved = new HashMap<>();

  @BeforeEach
  void save() {
    for (String p : PROPS) {
      saved.put(p, System.getProperty(p));
    }
  }

  @AfterEach
  void restore() {
    for (String p : PROPS) {
      if (saved.get(p) == null) {
        System.clearProperty(p);
      } else {
        System.setProperty(p, saved.get(p));
      }
    }
  }

  /** Point the platform's settings at {@code proxy}, for 127.0.0.1 too (excluded by default). */
  private static void platformProxy(HttpProxy proxy) {
    System.setProperty("http.proxyHost", "127.0.0.1");
    System.setProperty("http.proxyPort", Integer.toString(proxy.port()));
    System.setProperty("http.nonProxyHosts", "");
  }

  @Test
  void usesThePlatformsProxyWhenNoneIsConfigured(@TempDir Path dir) throws Exception {
    try (Origin origin = Origin.plain(); HttpProxy proxy = new HttpProxy(null, null)) {
      platformProxy(proxy);
      Probe p = new Probe(Transport.build(null, null, false), dir);
      assertEquals(CoreProto.Input.KindCase.HTTP_RESPONSE, p.post(origin.url("http")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.WS_OPENED, p.open(origin.url("ws")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.DOWNLOAD_RESPONSE, p.download(origin.url("http") + "/file").getKindCase());
      String host = "127.0.0.1:" + origin.port();
      assertEquals(List.of("POST http://" + host + "/v1/opamp", "CONNECT " + host, "GET http://" + host + "/file"),
          proxy.seen);
    }
  }

  @Test
  void directConnectionIgnoresIt(@TempDir Path dir) throws Exception {
    try (Origin origin = Origin.plain(); HttpProxy proxy = new HttpProxy(null, null)) {
      platformProxy(proxy);
      Probe p = new Probe(Transport.build(null, null, true), dir);
      assertEquals(CoreProto.Input.KindCase.HTTP_RESPONSE, p.post(origin.url("http")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.WS_OPENED, p.open(origin.url("ws")).getKindCase());
      assertEquals(CoreProto.Input.KindCase.DOWNLOAD_RESPONSE, p.download(origin.url("http") + "/file").getKindCase());
      assertTrue(proxy.seen.isEmpty(), proxy.seen.toString());
      assertEquals(3, origin.requests.size(), origin.requests.toString());
    }
  }
}
