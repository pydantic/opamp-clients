// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import java.nio.file.Path;
import java.util.Random;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;

/**
 * Noninterference of secrets, as .NET's RedactionTests: the notifications that carry what the
 * server offers (a ConnectionSettings and an InstallPackage) print the same whatever the secrets'
 * values, each as long in both. No header's value, private key, or password in an endpoint's,
 * proxy's or download's URL.
 */
class RedactionTest {
  // A secret's characters: those a URL's user-info holds as they are.
  private static final String CHARS =
      "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~!$&'()*+,;=";

  private static Opamp.Headers headers(String token) {
    return Opamp.Headers.newBuilder()
        .addHeaders(Opamp.Header.newBuilder().setKey("Authorization").setValue(token))
        .build();
  }

  private static Opamp.TLSCertificate certificate(String key) {
    return Opamp.TLSCertificate.newBuilder()
        .setCert(ByteString.copyFromUtf8("CERT"))
        .setPrivateKey(ByteString.copyFromUtf8(key))
        .setCaCert(ByteString.copyFromUtf8("CA"))
        .build();
  }

  private static Opamp.ProxyConnectionSettings proxy(String token, String password) {
    return Opamp.ProxyConnectionSettings.newBuilder()
        .setUrl("http://agent:" + password + "@proxy.example:3128")
        .setConnectHeaders(headers(token))
        .build();
  }

  private static String shown(String token, String password, String key) {
    var offers =
        Opamp.ConnectionSettingsOffers.newBuilder()
            .setOpamp(
                Opamp.OpAMPConnectionSettings.newBuilder()
                    .setDestinationEndpoint("https://agent:" + password + "@opamp.example/v1/opamp")
                    .setHeaders(headers(token))
                    .setCertificate(certificate(key))
                    .setProxy(proxy(token, password)))
            .setOwnLogs(
                Opamp.TelemetryConnectionSettings.newBuilder()
                    .setDestinationEndpoint("https://agent:" + password + "@otlp.example/v1/logs")
                    .setHeaders(headers(token))
                    .setCertificate(certificate(key))
                    .setProxy(proxy(token, password)))
            .putOtherConnections(
                "db",
                Opamp.OtherConnectionSettings.newBuilder()
                    .setDestinationEndpoint("postgres://agent:" + password + "@db.example:5432")
                    .setHeaders(headers(token))
                    .setCertificate(certificate(key))
                    .setProxy(proxy(token, password))
                    .build())
            .build();
    var pkg =
        Opamp.PackageAvailable.newBuilder()
            .setVersion("1.0")
            .setFile(
                Opamp.DownloadableFile.newBuilder()
                    .setDownloadUrl("https://agent:" + password + "@cdn.example/pkg")
                    .setHeaders(headers(token)))
            .build();
    return new Notification.ConnectionSettings(offers)
        + "\n"
        + new Notification.InstallPackage("pkg", pkg, Path.of("/downloads/pkg"));
  }

  /** Two different secrets of one length. */
  private static String[] pair(Random random) {
    int length = 1 + random.nextInt(40);
    String a = one(random, length);
    String b = one(random, length);
    while (b.equals(a)) {
      b = one(random, length);
    }
    return new String[] {a, b};
  }

  private static String one(Random random, int length) {
    var s = new StringBuilder();
    for (int i = 0; i < length; i++) {
      s.append(CHARS.charAt(random.nextInt(CHARS.length())));
    }
    return s.toString();
  }

  @Test
  void secretsChangeNothingShown() {
    var random = new Random(120);
    for (int i = 0; i < 200; i++) {
      String[] token = pair(random);
      String[] password = pair(random);
      String[] key = pair(random);
      assertEquals(shown(token[0], password[0], key[0]), shown(token[1], password[1], key[1]));
    }
  }

  @Test
  void whatIsNotSecretIsStillShown() {
    String shown = shown("tok", "pw", "key");
    for (String part :
        new String[] {
          "opamp.example/v1/opamp", "Authorization", "proxy.example:3128", "agent", "CERT", "CA",
          "otlp.example/v1/logs", "db.example:5432", "cdn.example/pkg", "/downloads/pkg",
          "value: \"<redacted>\"", "private_key: \"<redacted>\"", "agent:***@"
        }) {
      assertTrue(shown.contains(part), part + " in " + shown);
    }
  }

  /** Each record prints all its components, so one added later is printed too, or this fails. */
  @Test
  void everyComponentIsPrinted() {
    for (Record value :
        new Record[] {
          new Notification.ConnectionSettings(Opamp.ConnectionSettingsOffers.getDefaultInstance()),
          new Notification.InstallPackage(
              "pkg", Opamp.PackageAvailable.getDefaultInstance(), Path.of("/downloads/pkg"))
        }) {
      String shown = value.toString();
      for (var component : value.getClass().getRecordComponents()) {
        assertTrue(shown.contains(component.getName() + "="), component.getName() + " in " + shown);
      }
    }
  }
}
