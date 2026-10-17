// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Instant;
import java.time.ZoneOffset;
import java.time.format.DateTimeFormatter;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

class CoreTest {
  private static final CoreProto.Input START =
      CoreProto.Input.newBuilder().setStart(CoreProto.Empty.getDefaultInstance()).build();
  private static final CoreProto.Input TIMER =
      CoreProto.Input.newBuilder().setTimer(CoreProto.Empty.getDefaultInstance()).build();

  private static ByteString uid(int b) {
    byte[] u = new byte[16];
    java.util.Arrays.fill(u, (byte) b);
    return ByteString.copyFrom(u);
  }

  @Test
  void startSendsTheFirstReportAndHonoursARetryAfterDate() throws Exception {
    Core core = new Core(CoreProto.Config.newBuilder()
        .setInstanceUid(uid(7))
        .setEndpoint("http://server/v1/opamp")
        .build());
    CoreProto.Output out = core.step(0, 0, START);
    assertEquals(1, out.getEffectsCount());
    CoreProto.HttpRequest req = out.getEffects(0).getHttpRequest();
    assertEquals("http://server/v1/opamp", req.getUrl());
    Opamp.AgentToServer msg = Opamp.AgentToServer.parseFrom(req.getBody());
    assertEquals(uid(7), msg.getInstanceUid());
    assertTrue((msg.getCapabilities() & 1) != 0, "ReportsStatus");
    assertTrue(out.hasWakeAtMs(), "a wake-up for the next poll");

    // A 503 with Retry-After as a date, 30 s after the response came: nothing before then, and the
    // next request within the client's own spread after it (a quarter of the wait: 7.5 s).
    Instant came = Instant.parse("2026-09-26T12:00:00Z");
    String date = DateTimeFormatter.RFC_1123_DATE_TIME.format(came.plusSeconds(30).atZone(ZoneOffset.UTC));
    core.step(10, 0, CoreProto.Input.newBuilder()
        .setHttpResponse(CoreProto.HttpResponse.newBuilder()
            .setId(req.getId())
            .setStatus(503)
            .addHeaders(CoreProto.Header.newBuilder().setName("Retry-After").setValue(date))
            .setUnixMs(came.toEpochMilli()))
        .build());
    CoreProto.Output last = out;
    for (long at : new long[] {30_000, 30_009}) {
      last = core.step(at, 0, TIMER);
      assertTrue(!sends(last), "at " + at + " ms");
    }
    long now = 30_009;
    for (int i = 0; i < 10 && !sends(last); i++) {
      long wake = last.getWakeAtMs();
      assertTrue(wake > now, "wake at " + wake + " ms");
      assertTrue(!sends(core.step(wake - 1, 0, TIMER)), "at " + (wake - 1) + " ms");
      last = core.step(wake, 0, TIMER);
      now = wake;
    }
    assertTrue(sends(last) && now >= 30_010 && now <= 30_010 + 7_500, "sent at " + now + " ms");
  }

  private static boolean sends(CoreProto.Output out) {
    return out.getEffectsList().stream().anyMatch(CoreProto.Effect::hasHttpRequest);
  }

  @Test
  void aBadConfigIsAnError() {
    assertThrows(Core.CoreException.class,
        () -> new Core(CoreProto.Config.newBuilder().setInstanceUid(ByteString.copyFrom(new byte[2])).build()));
  }

  @Test
  void coresAreIndependent() {
    Core a = new Core(CoreProto.Config.newBuilder().setInstanceUid(uid(1)).setEndpoint("http://server/v1/opamp").build());
    Core b = new Core(CoreProto.Config.newBuilder().setInstanceUid(uid(2)).setEndpoint("http://server/v1/opamp").build());
    assertEquals(1, a.step(0, 0, START).getInstanceUid().byteAt(0));
    assertEquals(2, b.step(0, 0, START).getInstanceUid().byteAt(0));
  }

  /** A tap record: its kind, its time and its bytes. */
  record Rec(int kind, long nanos, byte[] bytes) {}

  static java.util.List<Rec> records(Path file) throws Exception {
    ByteBuffer b = ByteBuffer.wrap(Files.readAllBytes(file)).order(ByteOrder.LITTLE_ENDIAN);
    java.util.List<Rec> out = new java.util.ArrayList<>();
    while (b.hasRemaining()) {
      int kind = b.get();
      byte[] bytes = new byte[b.getInt()];
      long nanos = b.getLong();
      b.get(bytes);
      out.add(new Rec(kind, nanos, bytes));
    }
    return out;
  }

  @Test
  void theTapRecordsEveryCallAsItCrossed(@TempDir Path dir) throws Exception {
    String base = dir.resolve("core.tap").toString();
    CoreProto.Config config = CoreProto.Config.newBuilder()
        .setInstanceUid(uid(7)).setEndpoint("http://server/v1/opamp").build();
    Core a = new Core(config, base);
    a.step(5, 1234, START);
    a.close();
    // The process's first tapped core writes the path itself (unless another test's came first).
    Path aPath = Path.of(a.tapPath());
    java.util.List<Rec> recs = records(aPath);
    assertEquals(java.util.List.of(1, 2, 3, 4), recs.stream().map(Rec::kind).toList());
    assertArrayEquals(config.toByteArray(), recs.get(0).bytes(), "init in: the Config as encoded");
    assertTrue(CoreProto.Reply.parseFrom(recs.get(1).bytes()).hasOutput(), "init out: a Reply");
    assertEquals(CoreProto.Step.newBuilder().setNowMs(5).setInput(START).setUnixMs(1234).build(),
        CoreProto.Step.parseFrom(recs.get(2).bytes()), "step in: the Step sent");
    assertEquals(1, CoreProto.Reply.parseFrom(recs.get(3).bytes()).getOutput().getEffectsCount(), "step out: its Reply");
    for (int i = 1; i < recs.size(); i++) {
      assertTrue(recs.get(i).nanos() >= recs.get(i - 1).nanos(), "a monotonic clock");
    }

    // A later core in the same process: <path>.<pid>.<n>.
    Core b = new Core(config, base);
    b.close();
    long pid = ProcessHandle.current().pid();
    long n = aPath.toString().equals(base) ? 1 : Long.parseLong(aPath.toString().substring(aPath.toString().lastIndexOf('.') + 1)) + 1;
    assertEquals(base + "." + pid + "." + n, b.tapPath());
    assertEquals(java.util.List.of(1, 2), records(Path.of(b.tapPath())).stream().map(Rec::kind).toList());

    // A tap that can't be opened: the core runs without it.
    Core c = new Core(config, dir.resolve("missing").resolve("core.tap").toString());
    assertNull(c.tapPath());
    c.step(0, 0, START);
    c.close();
  }

  // The tap holds the Config's headers (the enrollment key): a new file is the user's only.
  @Test
  void theTapIsTheUsersOnly(@TempDir Path dir) throws Exception {
    org.junit.jupiter.api.Assumptions.assumeTrue(
        dir.getFileSystem().supportedFileAttributeViews().contains("posix"));
    Core.Tap tap = Core.Tap.open(dir.resolve("core.tap").toString());
    tap.record(Core.Tap.INIT_IN, new byte[] {1});
    tap.close();
    assertEquals("rw-------", java.nio.file.attribute.PosixFilePermissions.toString(
        Files.getPosixFilePermissions(Path.of(tap.path))));
    assertEquals(14, Files.size(Path.of(tap.path)));
  }

  @Test
  void aTapAlreadyThereIsNarrowedToTheUsersOnly(@TempDir Path dir) throws Exception {
    org.junit.jupiter.api.Assumptions.assumeTrue(
        dir.getFileSystem().supportedFileAttributeViews().contains("posix"));
    Path file = dir.resolve("core.tap");
    Files.write(file, new byte[0]);
    Files.setPosixFilePermissions(file, java.nio.file.attribute.PosixFilePermissions.fromString("rw-r--r--"));
    Core.Tap.append(file).close();
    assertEquals("rw-------", java.nio.file.attribute.PosixFilePermissions.toString(
        Files.getPosixFilePermissions(file)));
  }

  @Test
  void withoutTheVariableThereIsNoTap(@TempDir Path dir) throws Exception {
    CoreProto.Config config = CoreProto.Config.newBuilder().setInstanceUid(uid(7)).setEndpoint("http://server/v1/opamp").build();
    for (String unset : new String[] {null, ""}) {
      Core c = new Core(config, unset);
      c.step(0, 0, START);
      c.close();
      assertNull(c.tapPath());
    }
    try (var files = Files.list(dir)) {
      assertFalse(files.findAny().isPresent(), "no file");
    }
  }

  @Test
  void theCoreMakesTheInstanceUidFromTheEntropyGiven() {
    byte[] entropy = new byte[32];
    java.util.Arrays.fill(entropy, (byte) 0x5a);
    long now = Instant.parse("2026-09-30T00:00:00Z").toEpochMilli();
    Core c = new Core(CoreProto.Config.newBuilder().setEntropy(ByteString.copyFrom(entropy)).setUnixMs(now).setEndpoint("http://server/v1/opamp").build(), null);
    ByteBuffer uid = c.initOutput().getInstanceUid().asReadOnlyByteBuffer();
    java.util.UUID id = new java.util.UUID(uid.getLong(), uid.getLong());
    assertEquals(7, id.version());
    assertEquals(now, id.getMostSignificantBits() >>> 16, "the UUIDv7's time is unix_ms");
    assertEquals(c.initOutput().getInstanceUid(), c.step(0, 0, START).getInstanceUid(), "every step says it too");
    assertThrows(Core.CoreException.class, () -> new Core(CoreProto.Config.getDefaultInstance(), null),
        "no UID and no entropy");
  }
}
