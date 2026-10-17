// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import com.google.protobuf.ByteString;
import java.util.regex.Pattern;
import opamp.proto.v1.Opamp;

/**
 * What the notifications print instead of the server's offered credentials: a notification's
 * toString is what {@code log.info("got " + n)} writes, so it must not hold a header's value
 * (Authorization, say), a private key or a URL's password. As .NET's Redacted.
 */
final class Redacted {
  static final String HIDDEN = "<redacted>";
  private static final ByteString HIDDEN_KEY = ByteString.copyFromUtf8(HIDDEN);
  private static final Pattern PASSWORD = Pattern.compile("(//[^/@:]*):[^/@]*@");

  private Redacted() {}

  /** {@code url} without the password in it. */
  static String url(String url) {
    return PASSWORD.matcher(url).replaceAll("$1:***@");
  }

  /** The offers, with every header's value, private key and URL's password hidden. */
  static Opamp.ConnectionSettingsOffers offers(Opamp.ConnectionSettingsOffers offers) {
    var b = offers.toBuilder();
    if (offers.hasOpamp()) {
      var s = offers.getOpamp();
      var c = s.toBuilder().setDestinationEndpoint(url(s.getDestinationEndpoint()));
      if (s.hasHeaders()) {
        c.setHeaders(headers(s.getHeaders()));
      }
      if (s.hasCertificate()) {
        c.setCertificate(certificate(s.getCertificate()));
      }
      if (s.hasProxy()) {
        c.setProxy(proxy(s.getProxy()));
      }
      b.setOpamp(c);
    }
    if (offers.hasOwnMetrics()) {
      b.setOwnMetrics(telemetry(offers.getOwnMetrics()));
    }
    if (offers.hasOwnTraces()) {
      b.setOwnTraces(telemetry(offers.getOwnTraces()));
    }
    if (offers.hasOwnLogs()) {
      b.setOwnLogs(telemetry(offers.getOwnLogs()));
    }
    for (var e : offers.getOtherConnectionsMap().entrySet()) {
      var s = e.getValue();
      var c = s.toBuilder().setDestinationEndpoint(url(s.getDestinationEndpoint()));
      if (s.hasHeaders()) {
        c.setHeaders(headers(s.getHeaders()));
      }
      if (s.hasCertificate()) {
        c.setCertificate(certificate(s.getCertificate()));
      }
      if (s.hasProxy()) {
        c.setProxy(proxy(s.getProxy()));
      }
      b.putOtherConnections(e.getKey(), c.build());
    }
    return b.build();
  }

  /** The package, with its download's headers' values and URL's password hidden. */
  static Opamp.PackageAvailable pkg(Opamp.PackageAvailable pkg) {
    if (!pkg.hasFile()) {
      return pkg;
    }
    var f = pkg.getFile();
    var c = f.toBuilder().setDownloadUrl(url(f.getDownloadUrl()));
    if (f.hasHeaders()) {
      c.setHeaders(headers(f.getHeaders()));
    }
    return pkg.toBuilder().setFile(c).build();
  }

  private static Opamp.TelemetryConnectionSettings telemetry(Opamp.TelemetryConnectionSettings s) {
    var c = s.toBuilder().setDestinationEndpoint(url(s.getDestinationEndpoint()));
    if (s.hasHeaders()) {
      c.setHeaders(headers(s.getHeaders()));
    }
    if (s.hasCertificate()) {
      c.setCertificate(certificate(s.getCertificate()));
    }
    if (s.hasProxy()) {
      c.setProxy(proxy(s.getProxy()));
    }
    return c.build();
  }

  private static Opamp.ProxyConnectionSettings proxy(Opamp.ProxyConnectionSettings p) {
    var c = p.toBuilder().setUrl(url(p.getUrl()));
    if (p.hasConnectHeaders()) {
      c.setConnectHeaders(headers(p.getConnectHeaders()));
    }
    return c.build();
  }

  private static Opamp.Headers headers(Opamp.Headers headers) {
    var c = Opamp.Headers.newBuilder();
    for (var h : headers.getHeadersList()) {
      c.addHeaders(h.toBuilder().setValue(HIDDEN));
    }
    return c.build();
  }

  private static Opamp.TLSCertificate certificate(Opamp.TLSCertificate t) {
    return t.getPrivateKey().isEmpty() ? t : t.toBuilder().setPrivateKey(HIDDEN_KEY).build();
  }
}
