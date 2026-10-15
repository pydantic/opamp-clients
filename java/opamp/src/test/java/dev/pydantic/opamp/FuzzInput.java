// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import com.google.protobuf.ByteString;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import opamp.proto.v1.Anyvalue;
import opamp.proto.v1.Opamp;

/**
 * What the Jazzer targets ({@link ClientFuzz}, {@link CoreFuzz}) share, as python/tests/fuzz/_fuzzlib.py
 * does for Atheris: a reader of the fuzzer's bytes (0 past their end), the server that answers from
 * them (any bytes, or a ServerToAgent built from them and perhaps damaged, an error status, a
 * reset), and the application's calls a callback makes. The reader is shared by the client's thread
 * (the callback) and the server's: each read is one at a time.
 */
final class FuzzInput {
  private FuzzInput() {}

  static final String CAPABILITY = "io.example.fuzz";
  static final String FILES = "http://files.test";
  static final int[] STATUSES = {204, 301, 302, 400, 401, 403, 404, 429, 500, 503};
  static final long CAPABILITIES = Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE
      | Opamp.AgentCapabilities.AgentCapabilities_ReportsRemoteConfig_VALUE
      | Opamp.AgentCapabilities.AgentCapabilities_AcceptsPackages_VALUE
      | Opamp.AgentCapabilities.AgentCapabilities_ReportsPackageStatuses_VALUE
      | Opamp.AgentCapabilities.AgentCapabilities_AcceptsOpAMPConnectionSettings_VALUE
      | Opamp.AgentCapabilities.AgentCapabilities_AcceptsRestartCommand_VALUE
      | Opamp.AgentCapabilities.AgentCapabilities_ReportsHealth_VALUE
      | Opamp.AgentCapabilities.AgentCapabilities_ReportsEffectiveConfig_VALUE
      | Opamp.AgentCapabilities.AgentCapabilities_ReportsAvailableComponents_VALUE
      | Opamp.AgentCapabilities.AgentCapabilities_AcceptsOtherConnectionSettings_VALUE
      | Opamp.AgentCapabilities.AgentCapabilities_ReportsConnectionSettingsStatus_VALUE;

  /** The fuzzer's bytes, read front to back; 0 past the end. */
  static final class Reader {
    private final byte[] data;
    private int i;

    Reader(byte[] data) {
      this.data = data;
    }

    synchronized boolean empty() {
      return i >= data.length;
    }

    synchronized int u8() {
      return i < data.length ? data[i++] & 0xff : 0;
    }

    synchronized int u16() {
      return u8() | u8() << 8;
    }

    synchronized long u32() {
      return (u16() | (long) u16() << 16) & 0xffffffffL;
    }

    synchronized long u64() {
      return u32() | u32() << 32;
    }

    synchronized byte[] take(int n) {
      int k = Math.max(0, Math.min(n, data.length - i));
      byte[] out = java.util.Arrays.copyOfRange(data, i, i + k);
      i += k;
      return out;
    }

    /** A length (one byte, or two when {@code most} is over 255), then that many bytes. */
    synchronized byte[] blob(int most) {
      int n = most <= 255 ? u8() : u16();
      return take(Math.min(n, most));
    }

    ByteString bytes(int most) {
      return ByteString.copyFrom(blob(most));
    }

    /** protobuf strings are UTF-8: what isn't becomes U+FFFD. */
    String text(int most) {
      return new String(blob(most), StandardCharsets.UTF_8);
    }

    synchronized <T> T pick(List<T> choices) {
      return choices.get(u8() % choices.size());
    }

    boolean bit() {
      return (u8() & 1) != 0;
    }
  }

  static byte[] sha256(byte[] b) {
    try {
      return MessageDigest.getInstance("SHA-256").digest(b);
    } catch (NoSuchAlgorithmException e) {
      throw new AssertionError(e);
    }
  }

  /** Where a body of {@code size} bytes is cut as it arrives (up to three cuts). */
  static List<Integer> cuts(Reader r, int size) {
    List<Integer> at = new ArrayList<>();
    if (size > 0) {
      for (int n = r.u8() % 4; n > 0; n--) {
        at.add(r.u16() % (size + 1));
      }
    }
    return at;
  }

  /** {@code data} in pieces, cut at {@code at}. */
  static List<byte[]> pieces(byte[] data, List<Integer> at) {
    List<byte[]> out = new ArrayList<>();
    if (data.length == 0) {
      return out;
    }
    java.util.TreeSet<Integer> marks = new java.util.TreeSet<>();
    for (int a : at) {
      if (a > 0 && a < data.length) {
        marks.add(a);
      }
    }
    int from = 0;
    for (int m : marks) {
      out.add(java.util.Arrays.copyOfRange(data, from, m));
      from = m;
    }
    out.add(java.util.Arrays.copyOfRange(data, from, data.length));
    return out;
  }

  /** An answer: how ("ok", "reset", "garbage": all the server writes, status line included). */
  record Answer(String how, int status, List<String[]> headers, byte[] data, List<Integer> cuts) {}

  /**
   * What the server does with a WebSocket upgrade ("ok", "status": refused, "reset"), or with a
   * message on one ("message": a frame's payload; "raw": any bytes, as frames or not, where the
   * client reads frames itself, elsewhere a message's payload; "close", with {@code code}; "drop").
   */
  record Ws(String how, int status, List<String[]> headers, byte[] data, int code) {}

  /** {@code b} as a header name: no control characters, spaces or colons. */
  static String token(byte[] b) {
    StringBuilder s = new StringBuilder();
    for (byte x : b) {
      if (x > 32 && x < 127 && x != ':') {
        s.append((char) x);
      }
    }
    return s.toString();
  }

  /**
   * {@code b} as a header value HTTP/1.1 parsers take, read as Latin-1 (as the JDK reads headers):
   * no control characters but tab; bytes 128 to 255 (obs-text) are allowed.
   */
  static String text(byte[] b) {
    StringBuilder s = new StringBuilder();
    for (byte x : b) {
      int c = x & 0xff;
      if (c == 9 || (c >= 32 && c != 127)) {
        s.append((char) c);
      }
    }
    return s.toString().strip();
  }

  /** An OpAMP server whose every answer is the fuzzer's; package files it offers kept by URL. */
  static final class Server {
    final Reader r;
    final Map<String, byte[]> files = new ConcurrentHashMap<>();
    volatile int answers;

    Server(Reader r) {
      this.r = r;
    }

    /** Headers of the server's own, perhaps none (a Latin-1 Server or Set-Cookie, say). */
    synchronized List<String[]> headers() {
      List<String[]> out = new ArrayList<>();
      for (int n = (r.u8() & 1) != 0 ? r.u8() % 3 : 0; n > 0; n--) {
        String name = token(r.pick(List.of("Server".getBytes(StandardCharsets.US_ASCII),
            "Set-Cookie".getBytes(StandardCharsets.US_ASCII), "X-Fuzz".getBytes(StandardCharsets.US_ASCII), r.blob(8))));
        String value = text(r.blob(32));
        if (!name.isEmpty()) {
          out.add(new String[] {name, value});
        }
      }
      return out;
    }

    synchronized Ws upgrade() {
      int how = r.u8() % 6;
      if (how == 0) {
        return new Ws("reset", 0, List.of(), null, 0);
      }
      if (how == 1) {
        return new Ws("status", r.pick(List.of(200, 301, 400, 401, 403, 404, 426, 429, 500, 503)), headers(), null, 0);
      }
      return new Ws("ok", 101, List.of(), null, 0);
    }

    synchronized Ws wsAnswer(byte[] body) {
      byte[] msg = body.length > 0 && body[0] == 0 ? java.util.Arrays.copyOfRange(body, 1, body.length) : body;
      Answer a = answer(msg);
      if (a.how().equals("reset")) {
        return new Ws("drop", 0, List.of(), null, 0);
      }
      if (a.how().equals("garbage")) {
        return new Ws("raw", 0, List.of(), a.data(), 0);
      }
      if (a.status() != 200) {
        return new Ws("close", 0, List.of(), null, r.pick(List.of(1000, 1001, 1002, 1008, 1011, 3000, 4999, 0)));
      }
      byte[] framed = new byte[a.data().length + 1];
      System.arraycopy(a.data(), 0, framed, 1, a.data().length);
      return new Ws("message", 0, List.of(), framed, 0);
    }

    synchronized Answer answer(byte[] body) {
      answers++;
      ByteString uid;
      try {
        uid = Opamp.AgentToServer.parseFrom(body).getInstanceUid();
      } catch (com.google.protobuf.InvalidProtocolBufferException e) {
        uid = ByteString.EMPTY;
      }
      int kind = r.u8() % 10;
      int status = 200;
      List<String[]> headers = headers();
      byte[] data;
      switch (kind) {
        case 0 -> data = Opamp.ServerToAgent.newBuilder().setInstanceUid(uid).build().toByteArray();
        case 1 -> data = r.blob(4096);
        case 2 -> {
          return new Answer("reset", 0, headers, new byte[0], List.of());
        }
        case 3 -> {
          status = STATUSES[r.u8() % STATUSES.length];
          if (r.bit()) {
            headers.add(new String[] {"Retry-After", r.pick(List.of("0", "1", "120", "Wed, 21 Oct 2037 07:28:00 GMT",
                "soon", Long.toString(r.u32())))});
          }
          if (status == 301 || status == 302) {
            headers.add(new String[] {"Location", r.pick(List.of("http://opamp.test/v2", "/elsewhere", "https://x.test/", ""))});
          }
          data = r.blob(255);
        }
        case 4 -> {
          if (!r.empty()) {
            return new Answer("garbage", 0, headers, r.blob(1024), List.of());
          }
          data = message(uid).toByteArray();
        }
        default -> {
          data = message(uid).toByteArray();
          if (kind == 9 && data.length > 0) {
            for (int n = 1 + r.u8() % 3; n > 0; n--) {
              int at = r.u16() % data.length;
              int x = r.u8();
              data[at] ^= (byte) (x == 0 ? 1 : x);
            }
            if (r.bit()) {
              data = java.util.Arrays.copyOf(data, r.u16() % data.length);
            }
          }
        }
      }
      return new Answer("ok", status, headers, data, cuts(r, data.length));
    }

    synchronized Answer download(String url) {
      byte[] body = files.get(url);
      int how = r.u8() % 8;
      List<String[]> headers = headers();
      if (body == null || how == 1) {
        return new Answer("ok", 404, headers, "missing".getBytes(StandardCharsets.UTF_8), List.of());
      }
      if (how == 2) {
        return new Answer("reset", 0, List.of(), new byte[0], List.of());
      }
      if (how == 3) {
        // A redirect, to anywhere (a Location of any bytes but control characters).
        headers.add(new String[] {"Location", r.pick(List.of(FILES + "/moved", "/p1", "//other.test/p", "http://[bad",
            text(r.blob(40))))});
        return new Answer("ok", STATUSES[r.u8() % STATUSES.length], headers, new byte[0], List.of());
      }
      if (how == 4) {
        body = body.length == 0 ? new byte[] {'x'} : body.clone();
        body[body.length - 1] ^= 1; // not the file offered
      }
      return new Answer("ok", 200, headers, body, cuts(r, body.length));
    }

    Opamp.ServerToAgent message(ByteString uid) {
      int parts = r.u16();
      Opamp.ServerToAgent.Builder m = Opamp.ServerToAgent.newBuilder()
          .setInstanceUid((parts & 1) == 0 ? uid : r.bytes(20));
      if ((parts & 2) != 0) {
        Opamp.AgentConfigMap.Builder cm = Opamp.AgentConfigMap.newBuilder();
        for (int n = r.u8() % 3; n > 0; n--) {
          cm.putConfigMap(r.text(16), Opamp.AgentConfigObject.newBuilder().setBody(r.bytes(512)).setContentType(r.text(16)).build());
        }
        m.setRemoteConfig(Opamp.AgentRemoteConfig.newBuilder().setConfigHash(r.bytes(32)).setConfig(cm));
      }
      if ((parts & 4) != 0) {
        m.setPackagesAvailable(packages());
      }
      if ((parts & 8) != 0) {
        Opamp.ConnectionSettingsOffers.Builder cs = Opamp.ConnectionSettingsOffers.newBuilder().setHash(r.bytes(16));
        if (r.bit()) {
          Opamp.OpAMPConnectionSettings.Builder o = Opamp.OpAMPConnectionSettings.newBuilder()
              .setDestinationEndpoint(r.pick(List.of("http://opamp.test/v1/opamp", "http://other.test/opamp",
                  "ws://opamp.test/v1/opamp", r.text(40))))
              .setHeartbeatIntervalSeconds(r.pick(List.of(0L, 1L, 30L, 3600L, Long.MIN_VALUE, -1L, r.u32())));
          Opamp.Headers.Builder hs = Opamp.Headers.newBuilder();
          for (int n = r.u8() % 3; n > 0; n--) {
            hs.addHeaders(Opamp.Header.newBuilder().setKey(r.text(16)).setValue(r.text(32)));
          }
          cs.setOpamp(o.setHeaders(hs));
        }
        if (r.bit()) {
          cs.setOwnMetrics(Opamp.TelemetryConnectionSettings.newBuilder().setDestinationEndpoint(r.text(40))
              .setHeaders(Opamp.Headers.newBuilder().addHeaders(Opamp.Header.newBuilder().setKey(r.text(16)).setValue(r.text(32)))));
        }
        for (int n = r.u8() % 3; n > 0; n--) {
          cs.putOtherConnections(r.text(16), Opamp.OtherConnectionSettings.newBuilder().setDestinationEndpoint(r.text(40)).build());
        }
        m.setConnectionSettings(cs);
      }
      if ((parts & 16) != 0) {
        m.setFlags(r.pick(List.of(1L, 2L, 3L, r.u64())));
      }
      if ((parts & 32) != 0) {
        m.setCapabilities(r.u64());
      }
      if ((parts & 64) != 0) {
        m.setAgentIdentification(Opamp.AgentIdentification.newBuilder().setNewInstanceUid(
            r.pick(List.of(ByteString.copyFrom(new byte[16]), ByteString.copyFrom(new byte[] {0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}),
                r.bytes(20)))));
      }
      if ((parts & 128) != 0) {
        m.setCommand(Opamp.ServerToAgentCommand.newBuilder().setTypeValue(r.u8() % 3));
      }
      if ((parts & 256) != 0) {
        Opamp.CustomCapabilities.Builder cc = Opamp.CustomCapabilities.newBuilder();
        for (int n = r.u8() % 4; n > 0; n--) {
          cc.addCapabilities(r.text(24));
        }
        m.setCustomCapabilities(cc);
      }
      if ((parts & 512) != 0) {
        m.setCustomMessage(Opamp.CustomMessage.newBuilder().setCapability(r.pick(List.of(CAPABILITY, r.text(24))))
            .setType(r.text(16)).setData(r.bytes(256)));
      }
      if ((parts & 1024) != 0) {
        Opamp.ServerErrorResponse.Builder e = Opamp.ServerErrorResponse.newBuilder().setTypeValue(r.u8() % 5)
            .setErrorMessage(r.text(64));
        if (r.bit()) {
          e.setRetryInfo(Opamp.RetryInfo.newBuilder().setRetryAfterNanoseconds(r.u64()));
        }
        m.setErrorResponse(e);
      }
      return m.build();
    }

    private Opamp.PackagesAvailable packages() {
      Opamp.PackagesAvailable.Builder offer = Opamp.PackagesAvailable.newBuilder().setAllPackagesHash(r.bytes(16));
      for (int n = r.u8() % 3; n > 0; n--) {
        String name = r.pick(List.of("pkg", "other", "", "../up", "a/b", r.text(16)));
        Opamp.PackageAvailable.Builder p = Opamp.PackageAvailable.newBuilder().setTypeValue(r.u8() % 3)
            .setVersion(r.text(8)).setHash(r.bytes(16));
        if (r.u8() % 4 != 0) {
          int k = files.size() + 1;
          String url = r.pick(List.of(FILES + "/p" + k, FILES + "/p" + k + "?x=1", "", "ftp://files.test/p", r.text(40)));
          byte[] body = r.blob(2048);
          if (url.startsWith(FILES)) {
            files.put(url, body);
          }
          ByteString sum = ByteString.copyFrom(sha256(body));
          Opamp.DownloadableFile.Builder f = Opamp.DownloadableFile.newBuilder().setDownloadUrl(url)
              .setContentHash(r.pick(List.of(sum, sum, ByteString.EMPTY, r.bytes(32)))).setSignature(r.bytes(16));
          if (r.bit()) {
            f.setHeaders(Opamp.Headers.newBuilder().addHeaders(Opamp.Header.newBuilder().setKey(r.text(16)).setValue(r.text(32))));
          }
          p.setFile(f);
        }
        offer.putPackages(name, p.build());
      }
      return offer.build();
    }
  }

  static List<String> CALLS = List.of("setHealth", "setDescription", "setEffectiveConfig", "setRemoteConfigStatus",
      "packageInstalled", "packageRemoved", "setConnectionSettingsStatus", "sendCustomMessage",
      "setCustomCapabilities", "setAvailableComponents", "stop", "nothing");

  static List<Anyvalue.KeyValue> attributes(Reader r) {
    List<Anyvalue.KeyValue> out = new ArrayList<>();
    for (int n = r.u8() % 3; n > 0; n--) {
      Anyvalue.AnyValue.Builder v = Anyvalue.AnyValue.newBuilder();
      switch (r.u8() % 4) {
        case 0 -> v.setStringValue(r.text(32));
        case 1 -> v.setIntValue(r.u64());
        case 2 -> v.setBytesValue(r.bytes(32));
        default -> v.setKvlistValue(Anyvalue.KeyValueList.newBuilder().addValues(Anyvalue.KeyValue.newBuilder()
            .setKey(r.text(8)).setValue(Anyvalue.AnyValue.newBuilder().setBoolValue(r.bit()))));
      }
      out.add(Anyvalue.KeyValue.newBuilder().setKey(r.text(16)).setValue(v).build());
    }
    return out;
  }

  /**
   * The application's call {@code what}, its arguments from the fuzzer's bytes and the last
   * notification of each kind the callback was told ({@code seen}). True: stop was asked for (the
   * caller carries it out).
   */
  static boolean call(Reader r, OpampClient c, String what, Map<Class<?>, Notification> seen) {
    switch (what) {
      case "setHealth" -> c.setHealth(Opamp.ComponentHealth.newBuilder().setHealthy(r.bit()).setStatus(r.text(32))
          .setLastError(r.text(32)).setStartTimeUnixNano(r.u64()).setStatusTimeUnixNano(r.u64()).build());
      case "setDescription" -> c.setDescription(Opamp.AgentDescription.newBuilder()
          .addAllIdentifyingAttributes(attributes(r)).addAllNonIdentifyingAttributes(attributes(r)).build());
      case "setEffectiveConfig" -> c.setEffectiveConfig(Opamp.EffectiveConfig.newBuilder().setConfigMap(
          Opamp.AgentConfigMap.newBuilder().putConfigMap(r.text(8), Opamp.AgentConfigObject.newBuilder()
              .setBody(r.bytes(256)).build())).build());
      case "setRemoteConfigStatus" -> {
        ByteString h = seen.get(Notification.RemoteConfig.class) instanceof Notification.RemoteConfig rc && r.bit()
            ? rc.config().getConfigHash() : r.bytes(32);
        c.setRemoteConfigStatus(Opamp.RemoteConfigStatus.newBuilder().setLastRemoteConfigHash(h)
            .setStatusValue(r.u8() % 5).setErrorMessage(r.text(32)).build());
      }
      case "packageInstalled" -> {
        String name;
        byte[] hash;
        if (seen.get(Notification.InstallPackage.class) instanceof Notification.InstallPackage ip && r.u8() % 4 != 0) {
          name = ip.name();
          hash = r.u8() % 4 != 0 ? ip.pkg().getHash().toByteArray() : r.blob(16);
        } else {
          name = r.pick(List.of("pkg", "other", "", r.text(16)));
          hash = r.blob(16);
        }
        c.packageInstalled(name, hash, r.bit() ? null : r.text(32));
      }
      case "packageRemoved" -> c.packageRemoved(
          seen.get(Notification.RemovePackage.class) instanceof Notification.RemovePackage rp && r.bit() ? rp.name() : r.text(16));
      case "setConnectionSettingsStatus" -> {
        ByteString h = seen.get(Notification.ConnectionSettings.class) instanceof Notification.ConnectionSettings cs && r.bit()
            ? cs.settings().getHash() : r.bytes(16);
        c.setConnectionSettingsStatus(Opamp.ConnectionSettingsStatus.newBuilder().setLastConnectionSettingsHash(h)
            .setStatusValue(r.u8() % 4).setErrorMessage(r.text(32)).build());
      }
      case "sendCustomMessage" -> c.sendCustomMessage(Opamp.CustomMessage.newBuilder()
          .setCapability(r.pick(List.of(CAPABILITY, r.text(24)))).setType(r.text(16)).setData(r.bytes(512)).build());
      case "setCustomCapabilities" -> {
        Opamp.CustomCapabilities.Builder cc = Opamp.CustomCapabilities.newBuilder();
        for (int n = r.u8() % 3; n > 0; n--) {
          cc.addCapabilities(r.pick(List.of(CAPABILITY, r.text(24))));
        }
        c.setCustomCapabilities(cc.build());
      }
      case "setAvailableComponents" -> {
        Opamp.AvailableComponents.Builder ac = Opamp.AvailableComponents.newBuilder();
        for (int n = r.u8() % 3; n > 0; n--) {
          ac.putComponents(r.text(16), Opamp.ComponentDetails.newBuilder().addAllMetadata(attributes(r)).build());
        }
        c.setAvailableComponents(ac.build());
      }
      case "stop" -> {
        return true;
      }
      default -> {}
    }
    return false;
  }
}
