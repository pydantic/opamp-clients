// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.io.IOException;
import java.io.InputStream;
import java.net.InetAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.Executors;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.Semaphore;
import java.util.concurrent.TimeUnit;
import java.util.function.Consumer;
import javax.net.ssl.SSLContext;
import javax.net.ssl.SSLSocket;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * Core mode's executor ({@link Streams}) on its own, the test playing the core: its effects go in
 * on a loop of its own, what it reports comes out. Over {@link MemoryNet} (the test chooses when
 * each connection is made, and how fast the peer reads) and, for TLS, real sockets. The registry: a
 * close, a cancel and shutdown end what they name at once; a connection, a handshake or a
 * resolution that completes after its close is closed and reported to no one; nothing is reported
 * after a close or after shutdown. Backpressure both ways; splices; download files made new,
 * written in order, durable before downloaded, deleted on discard, cancel and failure.
 */
// covers: RULE-SHELL-04, RULE-SHELL-06, RULE-SHELL-07
class StreamsTest {
  /** A Streams on a loop of its own, and what it reports. */
  static final class Harness implements AutoCloseable {
    final ScheduledExecutorService loop = Executors.newSingleThreadScheduledExecutor();
    final LinkedBlockingQueue<CoreProto.Input> got = new LinkedBlockingQueue<>();
    final Streams x;
    /** Run on the loop for each input, after it is recorded (the core's answer, say). */
    volatile Consumer<CoreProto.Input> core = in -> {};

    Harness(Streams.Platform platform, Disk disk, Path state) {
      this(platform, disk, state, TransportTest.trusting(TransportTest.CA));
    }

    /** With the operator's TLS settings {@code tls}. */
    Harness(Streams.Platform platform, Disk disk, Path state, Tls tls) {
      x = new Streams(loop, in -> {
        got.add(in);
        core.accept(in);
      }, platform, Streams.originEngines(tls),
          url -> CoreProto.ProxyAnswer.newBuilder(), disk, state, id -> Executor.downloadsDir(state).resolve("d" + id));
    }

    void run(CoreProto.Effect e) throws Exception {
      loop.submit(() -> x.run(e)).get(10, TimeUnit.SECONDS);
    }

    CoreProto.Input next() throws InterruptedException {
      CoreProto.Input in = got.poll(10, TimeUnit.SECONDS);
      assertNotNull(in, "an input");
      return in;
    }

    /** Nothing reported for {@code ms}. */
    void quiet(long ms) throws Exception {
      CoreProto.Input in = got.poll(ms, TimeUnit.MILLISECONDS);
      assertNull(in, () -> "reported: " + in);
    }

    CompletableFuture<Void> shutdown() throws Exception {
      return loop.submit(x::shutdown).get(10, TimeUnit.SECONDS);
    }

    boolean empty() throws Exception {
      return left().isEmpty();
    }

    String left() throws Exception {
      return loop.submit(() -> x.in.left()).get(10, TimeUnit.SECONDS);
    }

    @Override
    public void close() throws Exception {
      loop.submit(x::shutdown).get(10, TimeUnit.SECONDS);
      loop.shutdownNow();
    }
  }

  static CoreProto.Effect open(long id) {
    return CoreProto.Effect.newBuilder().setStreamOpen(CoreProto.StreamOpen.newBuilder().setId(id).setHost("server").setPort(80)).build();
  }

  static CoreProto.Effect write(long id, byte[] data) {
    return CoreProto.Effect.newBuilder().setStreamWrite(CoreProto.WsMessage.newBuilder().setId(id).setData(ByteString.copyFrom(data))).build();
  }

  static CoreProto.Effect close(long id, boolean abort) {
    return CoreProto.Effect.newBuilder().setStreamClose(CoreProto.StreamClose.newBuilder().setId(id).setAbort(abort)).build();
  }

  static CoreProto.Effect startTls(long id, String host) {
    return startTls(id, host, true);
  }

  /** TLS, at the server's own origin or not. */
  static CoreProto.Effect startTls(long id, String host, boolean serverOrigin) {
    return CoreProto.Effect.newBuilder()
        .setStreamStartTls(CoreProto.StreamName.newBuilder().setId(id).setHost(host).setServerOrigin(serverOrigin)).build();
  }

  static CoreProto.Effect resolve(long id, String host) {
    return CoreProto.Effect.newBuilder().setStreamResolve(CoreProto.StreamName.newBuilder().setId(id).setHost(host)).build();
  }

  static CoreProto.Effect splice(long id, long download, long bytes) {
    return CoreProto.Effect.newBuilder().setStreamSplice(CoreProto.StreamSplice.newBuilder().setId(id).setDownload(download).setBytes(bytes)).build();
  }

  static CoreProto.Effect data(long download, byte[] data) {
    return CoreProto.Effect.newBuilder().setDownloadData(CoreProto.WsMessage.newBuilder().setId(download).setData(ByteString.copyFrom(data))).build();
  }

  static CoreProto.Effect done(long download) {
    return CoreProto.Effect.newBuilder().setDownloadDone(CoreProto.Id.newBuilder().setId(download)).build();
  }

  static CoreProto.Effect discard(long download) {
    return CoreProto.Effect.newBuilder().setDiscard(CoreProto.Id.newBuilder().setId(download)).build();
  }

  static CoreProto.Effect cancel(long download) {
    return CoreProto.Effect.newBuilder().setCancel(CoreProto.Id.newBuilder().setId(download)).build();
  }

  static byte[] ascii(String s) {
    return s.getBytes(StandardCharsets.US_ASCII);
  }

  static byte[] sha(byte[] b) throws Exception {
    return MessageDigest.getInstance("SHA-256").digest(b);
  }

  /** A connection opened and accepted: the far end. */
  static MemoryNet.End opened(Harness h, MemoryNet net, long id) throws Exception {
    h.run(open(id));
    MemoryNet.End peer = net.dials.poll(10, TimeUnit.SECONDS).accept();
    assertEquals(CoreProto.Input.KindCase.STREAM_OPENED, h.next().getKindCase());
    return peer;
  }

  // ---------------------------------------------------------------------------------------------
  // Streams.

  @Test
  void aStreamCarriesBytesBothWaysAndACloseLetsWrittenBytesGo(@TempDir Path dir) throws Exception {
    MemoryNet net = new MemoryNet();
    try (Harness h = new Harness(net, Disk.REAL, dir.resolve("state"))) {
      MemoryNet.End peer = opened(h, net, 1);
      h.run(write(1, ascii("hello")));
      assertArrayEquals(ascii("hello"), peer.read(5));
      peer.write(ascii("world"));
      CoreProto.Input in = h.next();
      assertEquals("world", in.getStreamData().getData().toStringUtf8());
      // Written, then closed: the bytes go, then the FIN; nothing more is reported.
      h.run(write(1, ascii("last")));
      h.run(close(1, false));
      assertArrayEquals(ascii("last"), peer.read(10));
      try {
        peer.write(ascii("unheard"));
      } catch (IOException e) {
        // it reads nothing more: as good as unheard
      }
      h.quiet(200);
      assertTrue(net.dials.isEmpty());
      for (int i = 0; i < 100 && !h.empty(); i++) {
        Thread.sleep(10);
      }
      assertTrue(h.empty(), "the linger is over once the FIN went");
      assertEquals("", net.leftOpen());
    }
  }

  @Test
  void aRefusedConnectionAPeerThatClosesAndOneThatResetsAreReported(@TempDir Path dir) throws Exception {
    MemoryNet net = new MemoryNet();
    try (Harness h = new Harness(net, Disk.REAL, dir.resolve("state"))) {
      h.run(open(1));
      net.dials.poll(10, TimeUnit.SECONDS).refuse("connection refused");
      CoreProto.Input failed = h.next();
      assertEquals(1, failed.getStreamFailed().getId());
      assertEquals("connection refused", failed.getStreamFailed().getDetail());

      MemoryNet.End peer = opened(h, net, 2);
      peer.write(ascii("bye"));
      peer.finish();
      assertEquals("bye", h.next().getStreamData().getData().toStringUtf8());
      assertEquals(2, h.next().getStreamEof().getId());

      MemoryNet.End reset = opened(h, net, 3);
      reset.abort();
      CoreProto.Input lost = h.next();
      assertEquals(3, lost.getStreamFailed().getId(), lost.toString());
      h.quiet(100);
    }
  }

  /**
   * A stream the core closed lingers while what was written goes; its peer resets meanwhile, and
   * its writer is slow to see the close (a thread not yet run): the reset ends the linger, so it
   * must end the connection too, or nothing closes it before the end (StreamsFrayTest seed
   * 6806190798101515795 left it open after shutdown).
   */
  @Test
  void aLingeringStreamWhosePeerResetsIsClosedAtOnce(@TempDir Path dir) throws Exception {
    MemoryNet net = new MemoryNet();
    CountDownLatch writers = new CountDownLatch(1);
    Streams.Platform slowWriters = new Streams.Platform() {
      @Override
      public Streams.Conn connect(String host, int port, Streams.Attempt attempt) throws IOException {
        return net.connect(host, port, attempt);
      }

      @Override
      public List<String> resolve(String host) throws IOException {
        return net.resolve(host);
      }

      @Override
      public void start(String name, Runnable r) {
        Streams.Platform.super.start(name, !name.endsWith("-write") ? r : () -> {
          try {
            writers.await();
          } catch (InterruptedException e) {
            return;
          }
          r.run();
        });
      }
    };
    try (Harness h = new Harness(slowWriters, Disk.REAL, dir.resolve("state"))) {
      MemoryNet.End peer = opened(h, net, 1);
      h.run(close(1, false));
      peer.abort();
      for (int i = 0; i < 500 && !h.empty(); i++) {
        Thread.sleep(10);
      }
      assertTrue(h.empty(), "the reset ended the linger: " + h.left());
      assertEquals("", net.leftOpen(), "a linger over is a connection over");
      h.shutdown().get(5, TimeUnit.SECONDS);
      assertEquals("", net.leftOpen());
      h.quiet(100);
    } finally {
      writers.countDown();
    }
  }

  @Test
  void aStreamThatConnectsAfterItsCloseIsAbortedAtOnce(@TempDir Path dir) throws Exception {
    MemoryNet net = new MemoryNet();
    try (Harness h = new Harness(net, Disk.REAL, dir.resolve("state"))) {
      for (boolean abort : new boolean[] {true, false}) {
        h.run(open(1));
        MemoryNet.Dial d = net.dials.poll(10, TimeUnit.SECONDS);
        h.run(close(1, abort));
        assertTrue(d.aborted(), "the attempt is given up at once");
        MemoryNet.End peer = d.accept();
        assertTrue(d.client().aborted(), "a connection made after the close is reset");
        assertEquals(-1, readOrEnd(peer));
        h.quiet(100);
      }
      assertTrue(h.empty());
    }
  }

  /** -1 at the end or on a reset. */
  static int readOrEnd(MemoryNet.End peer) {
    try {
      return peer.in.read(new byte[1], 0, 1);
    } catch (IOException e) {
      return -1;
    }
  }

  @Test
  void namesAreResolvedForAStreamAndACloseStopsAResolution(@TempDir Path dir) throws Exception {
    MemoryNet net = new MemoryNet();
    net.names.put("proxy.example", List.of("192.0.2.1", "2001:db8::1"));
    try (Harness h = new Harness(net, Disk.REAL, dir.resolve("state"))) {
      h.run(resolve(1, "proxy.example"));
      CoreProto.Input in = h.next();
      assertEquals(List.of("192.0.2.1", "2001:db8::1"), in.getStreamResolved().getAddressesList());
      h.run(resolve(2, "nowhere.example"));
      assertEquals(2, h.next().getStreamFailed().getId());
      // A resolution closed while it runs: its answer goes nowhere.
      net.holdResolutions(true);
      h.run(resolve(3, "proxy.example"));
      h.run(close(3, true));
      net.holdResolutions(false);
      h.quiet(200);
      assertTrue(h.empty());
    }
  }

  @Test
  void shutdownEndsConnectsStreamsResolutionsAndLingeringClosesAtOnce(@TempDir Path dir) throws Exception {
    MemoryNet net = new MemoryNet(16);
    try (Harness h = new Harness(net, Disk.REAL, dir.resolve("state"))) {
      MemoryNet.End lingering = opened(h, net, 1);
      // Closed with more written than the peer takes: it lingers.
      h.run(write(1, new byte[1000]));
      h.run(close(1, false));
      MemoryNet.End open = opened(h, net, 2);
      h.run(open(3));
      MemoryNet.Dial connecting = net.dials.poll(10, TimeUnit.SECONDS);
      net.holdResolutions(true);
      h.run(resolve(4, "x"));
      assertFalse(h.empty());
      long t0 = System.nanoTime();
      h.shutdown().get(5, TimeUnit.SECONDS);
      assertTrue((System.nanoTime() - t0) / 1_000_000 < 1_000, "at once");
      assertTrue(h.empty());
      assertTrue(connecting.aborted());
      assertEquals("", net.leftOpen(), "every connection reset, the lingering one too");
      assertTrue(lingering.in.waiting() < 1000, "what was still to go was dropped");
      net.holdResolutions(false);
      try {
        open.write(ascii("late"));
      } catch (IOException e) {
        // reset
      }
      h.quiet(200);
    }
  }

  // ---------------------------------------------------------------------------------------------
  // Backpressure.

  @Test
  void aPeerThatStopsReadingIsNotReadUntilItDrainsAndPastABoundFails(@TempDir Path dir) throws Exception {
    MemoryNet net = new MemoryNet(1024);
    try (Harness h = new Harness(net, Disk.REAL, dir.resolve("state"))) {
      MemoryNet.End peer = opened(h, net, 1);
      // The stream is in its first read before anything is written: on a loaded machine its
      // reader had not got there when the write below passed WRITE_HIGH, so no read was waiting
      // and "ping" was never read.
      for (int i = 0; i < 5_000 && !peer.out.readerWaiting(); i++) {
        Thread.sleep(2);
      }
      assertTrue(peer.out.readerWaiting(), "the stream's reader is waiting for a byte");
      // The peer reads nothing: what is written waits; past WRITE_HIGH the stream reads nothing.
      byte[] big = new byte[Streams.WRITE_HIGH + 1];
      h.run(write(1, big));
      Thread.sleep(100);
      // A read already waiting completes (a blocking read can't be taken back); the next waits.
      peer.write(ascii("ping"));
      assertEquals("ping", h.next().getStreamData().getData().toStringUtf8());
      peer.write(ascii("pong"));
      h.quiet(300);
      // It reads: the stream drains, and reads again.
      assertEquals(big.length, peer.read(big.length).length);
      assertEquals("pong", h.next().getStreamData().getData().toStringUtf8());
      // Past MAX_UNSENT: the next write fails the stream.
      h.run(write(1, new byte[(int) Streams.MAX_UNSENT + 1]));
      Thread.sleep(100);
      h.run(write(1, ascii("x")));
      CoreProto.Input failed = h.next();
      assertTrue(failed.getStreamFailed().getDetail().contains("has not taken"), failed.toString());
      assertEquals("", net.leftOpen());
    }
  }

  /**
   * A stream this gives up from within an effect (a write past the bound of what waits unsent)
   * tells the core from the loop's next turn: if the core closes the stream first (a close in the
   * same output, or in the output of an input that was waiting ahead of it), nothing is said.
   */
  // covers: RULE-SHELL-06
  @Test
  void aFailureOnItsWayWhenItsStreamIsClosedIsDropped(@TempDir Path dir) throws Exception {
    MemoryNet net = new MemoryNet(1024);
    try (Harness h = new Harness(net, Disk.REAL, dir.resolve("state"))) {
      opened(h, net, 1);
      // The peer reads nothing: more than MAX_UNSENT waits.
      h.run(write(1, new byte[(int) Streams.MAX_UNSENT + 1]));
      Thread.sleep(100);
      // One turn of the loop, as the effects of one output: the write that fails it, and its close.
      h.loop.submit(() -> {
        h.x.run(write(1, ascii("x")));
        h.x.run(close(1, true));
      }).get(10, TimeUnit.SECONDS);
      h.quiet(300);
      assertEquals("", net.leftOpen());
      assertTrue(h.empty(), h.left());
    }
  }

  /** A disk whose download writes wait for permits (and are counted). */
  static final class SlowDisk implements Disk {
    final Semaphore permits = new Semaphore(0);
    final List<String> ops = new CopyOnWriteArrayList<>();
    volatile boolean failWrites;

    @Override
    public Handle create(Path path, boolean inherit) throws IOException {
      Handle h = REAL.create(path, inherit);
      ops.add("create " + path.getFileName());
      return new Handle() {
        @Override
        public void write(byte[] b, int off, int len) throws IOException {
          ops.add("writing " + path.getFileName());
          try {
            permits.acquire();
          } catch (InterruptedException e) {
            throw new java.io.InterruptedIOException();
          }
          if (failWrites) {
            throw new IOException("no space left");
          }
          h.write(b, off, len);
          ops.add("write " + path.getFileName() + " " + len);
        }

        @Override
        public void sync() throws IOException {
          h.sync();
          ops.add("sync " + path.getFileName());
        }

        @Override
        public void close() throws IOException {
          h.close();
          ops.add("close " + path.getFileName());
        }
      };
    }

    @Override
    public boolean remove(Path path) throws IOException {
      boolean was = REAL.remove(path);
      ops.add("remove " + path.getFileName() + (was ? "" : " (none)"));
      return was;
    }

    @Override
    public void rename(Path from, Path to) throws IOException {
      REAL.rename(from, to);
    }

    @Override
    public boolean syncDir(Path d) throws IOException {
      return REAL.syncDir(d);
    }

    @Override
    public boolean mkdir(Path d) throws IOException {
      return REAL.mkdir(d);
    }

    @Override
    public boolean clear(Path d) throws IOException {
      return REAL.clear(d);
    }
  }

  /**
   * Socket to file: bytes the file can't take as fast as they come stop the stream reading (past
   * HIGH_WATER) until the writes catch up; nothing is dropped or reordered; spliced, or as the
   * core's download_data of the bytes read.
   */
  @Test
  void aSocketFasterThanItsFileIsPausedNothingReordered(@TempDir Path dir) throws Exception {
    for (boolean splice : new boolean[] {true, false}) {
      MemoryNet net = new MemoryNet(64 << 10);
      SlowDisk disk = new SlowDisk();
      Path state = dir.resolve("state-" + splice);
      try (Harness h = new Harness(net, disk, state)) {
        java.util.concurrent.atomic.AtomicLong read = new java.util.concurrent.atomic.AtomicLong();
        if (!splice) {
          // The core: each chunk read becomes the download's data.
          h.core = in -> {
            if (in.hasStreamData()) {
              read.addAndGet(in.getStreamData().getData().size());
              h.x.run(data(7, in.getStreamData().getData().toByteArray()));
            }
          };
        }
        MemoryNet.End peer = opened(h, net, 1);
        byte[] body = new byte[12 << 20];
        new java.util.SplittableRandom(splice ? 1 : 2).nextBytes(body);
        if (splice) {
          h.run(splice(1, 7, body.length));
        }
        Thread writer = new Thread(() -> {
          try {
            peer.write(body);
          } catch (IOException e) {
            // closed
          }
        });
        writer.start();
        // The file takes nothing yet: the stream stops reading once HIGH_WATER waits.
        long waiting = 0;
        for (int i = 0; i < 200; i++) {
          Thread.sleep(10);
          waiting = h.loop.submit(h.x::unwritten).get();
          if (waiting > Streams.HIGH_WATER) {
            break;
          }
        }
        Thread.sleep(300);
        long held = h.loop.submit(h.x::unwritten).get();
        assertTrue(held > Streams.HIGH_WATER && held < Streams.HIGH_WATER + 2 * Streams.CHUNK + (64 << 10),
            "held at " + held + " (spliced: " + splice + ")");
        assertTrue(writer.isAlive(), "the peer waits for its window");
        disk.permits.release(Integer.MAX_VALUE / 2);
        writer.join(20_000);
        if (splice) {
          assertEquals(1, h.next().getStreamSpliced().getId());
        } else {
          for (int i = 0; i < 2000 && read.get() < body.length; i++) {
            Thread.sleep(10);
          }
          assertEquals(body.length, read.get());
        }
        h.run(done(7));
        CoreProto.Input in;
        do {
          in = h.next();
        } while (in.hasStreamData());
        assertArrayEquals(sha(body), in.getDownloaded().getSha256().toByteArray(), "downloaded, its digest");
        Path file = Executor.downloadsDir(state).resolve("d7");
        assertArrayEquals(body, Files.readAllBytes(file), "every byte, in order");
        int synced = disk.ops.indexOf("sync d7");
        assertTrue(synced > 0 && disk.ops.indexOf("close d7") > synced, disk.ops.toString());
      }
    }
  }

  @Test
  void aStreamTheShellClosesOnHearingItsSpliceDoneSaysNothingMore(@TempDir Path dir) throws Exception {
    MemoryNet net = new MemoryNet();
    Path state = dir.resolve("state");
    try (Harness h = new Harness(net, Disk.REAL, state)) {
      MemoryNet.End peer = opened(h, net, 1);
      h.core = in -> {
        if (in.hasStreamSpliced()) {
          h.x.run(close(1, false));
        }
      };
      h.run(splice(1, 3, 4));
      peer.write(ascii("bodyAFTER"));
      assertEquals(1, h.next().getStreamSpliced().getId());
      h.quiet(200);
      h.run(done(3));
      assertTrue(h.next().hasDownloaded());
      assertArrayEquals(ascii("body"), Files.readAllBytes(Executor.downloadsDir(state).resolve("d3")));
    }
  }

  /**
   * What the core asks in answer to an input about a stream applies to the very next bytes: a
   * splice it asks for as it hears the one before is done (the adversary's stream_splice_chain)
   * takes the bytes already read past the first, and only what is past the second is the core's.
   */
  // covers: RULE-SHELL-05
  @Test
  void aSpliceAskedForAsTheOneBeforeEndsTakesTheBytesAlreadyRead(@TempDir Path dir) throws Exception {
    MemoryNet net = new MemoryNet();
    Path state = dir.resolve("state");
    try (Harness h = new Harness(net, Disk.REAL, state)) {
      MemoryNet.End peer = opened(h, net, 1);
      // The core: the first splice's end is answered with a second, of the next three bytes.
      java.util.concurrent.atomic.AtomicInteger splices = new java.util.concurrent.atomic.AtomicInteger();
      h.core = in -> {
        if (in.hasStreamSpliced() && splices.incrementAndGet() == 1) {
          h.x.run(splice(1, 3, 3));
        }
      };
      h.run(splice(1, 3, 4));
      // One write, so one read: the bytes of both splices and those after them, in one chunk.
      peer.write(ascii("bodyandAFTER"));
      assertEquals(1, h.next().getStreamSpliced().getId());
      CoreProto.Input second = h.next();
      assertTrue(second.hasStreamSpliced(), "the second splice's bytes were to go to the file: " + second);
      assertEquals("AFTER", h.next().getStreamData().getData().toStringUtf8());
      h.run(done(3));
      assertTrue(h.next().hasDownloaded());
      assertArrayEquals(ascii("bodyand"), Files.readAllBytes(Executor.downloadsDir(state).resolve("d3")));
    }
  }

  // ---------------------------------------------------------------------------------------------
  // Files.

  @Test
  void aFileIsWrittenInOrderSyncedAndHashedAndAnEmptyOneIsAFile(@TempDir Path dir) throws Exception {
    SlowDisk disk = new SlowDisk();
    disk.permits.release(1000);
    Path state = dir.resolve("state");
    try (Harness h = new Harness(new MemoryNet(), disk, state)) {
      h.run(data(1, ascii("one ")));
      h.run(data(1, ascii("two ")));
      h.run(data(1, ascii("three")));
      h.run(done(1));
      CoreProto.Input in = h.next();
      assertArrayEquals(sha(ascii("one two three")), in.getDownloaded().getSha256().toByteArray());
      assertArrayEquals(ascii("one two three"), Files.readAllBytes(Executor.downloadsDir(state).resolve("d1")));
      assertTrue(disk.ops.indexOf("sync d1") < disk.ops.indexOf("close d1"), disk.ops.toString());
      // Made new: a file already at its path (a crashed run's) is not overwritten.
      h.run(done(2));
      assertArrayEquals(sha(new byte[0]), h.next().getDownloaded().getSha256().toByteArray());
      assertEquals(0, Files.size(Executor.downloadsDir(state).resolve("d2")));
      Files.write(Executor.downloadsDir(state).resolve("d3"), ascii("someone else's"));
      h.run(data(3, ascii("x")));
      h.run(done(3));
      assertTrue(h.next().hasDownloadFailed());
      assertArrayEquals(ascii("someone else's"), Files.readAllBytes(Executor.downloadsDir(state).resolve("d3")),
          "a file it didn't make is not its to delete");
    }
  }

  @Test
  void aDiscardOrCancelWhileAWriteIsPendingLeavesNoFileAndSaysNothing(@TempDir Path dir) throws Exception {
    for (boolean isCancel : new boolean[] {false, true}) {
      SlowDisk disk = new SlowDisk();
      Path state = dir.resolve("state-" + isCancel);
      try (Harness h = new Harness(new MemoryNet(), disk, state)) {
        h.run(data(1, ascii("partial")));
        for (int i = 0; i < 100 && !disk.ops.contains("create d1"); i++) {
          Thread.sleep(10);
        }
        h.run(isCancel ? cancel(1) : discard(1));
        h.run(done(1));
        disk.permits.release(10);
        for (int i = 0; i < 100 && !disk.ops.contains("remove d1"); i++) {
          Thread.sleep(10);
        }
        h.quiet(200);
        assertFalse(Files.exists(Executor.downloadsDir(state).resolve("d1")));
        assertTrue(disk.ops.contains("remove d1"), disk.ops.toString());
      }
    }
  }

  @Test
  void aDiscardOfAFileAlreadyAnsweredDeletesIt(@TempDir Path dir) throws Exception {
    Path state = dir.resolve("state");
    try (Harness h = new Harness(new MemoryNet(), Disk.REAL, state)) {
      h.run(data(1, ascii("package")));
      h.run(done(1));
      assertTrue(h.next().hasDownloaded());
      Path file = Executor.downloadsDir(state).resolve("d1");
      assertTrue(Files.exists(file));
      CoreProto.Effect d = discard(1);
      assertTrue(h.loop.submit(() -> h.x.run(d)).get(), "Streams' to delete");
      for (int i = 0; i < 100 && Files.exists(file); i++) {
        Thread.sleep(10);
      }
      assertFalse(Files.exists(file));
      // One it never wrote is the platform executor's.
      assertFalse(h.loop.submit(() -> h.x.run(discard(9))).get());
    }
  }

  /**
   * A file's answer on its way to the core when the core cancels or discards the file (its
   * download_done and the cancel in one output, the file made durable in between: the adversary's
   * seed 130, after_cancel): the answer is dropped, and the file deleted.
   */
  // covers: RULE-SHELL-06
  @Test
  void anAnswerOnItsWayWhenItsFileIsCancelledOrDiscardedIsDropped(@TempDir Path dir) throws Exception {
    for (boolean isCancel : new boolean[] {false, true}) {
      for (boolean fails : new boolean[] {false, true}) {
        SlowDisk disk = new SlowDisk();
        disk.failWrites = fails;
        disk.permits.release(10);
        Path state = dir.resolve("state-" + isCancel + "-" + fails);
        try (Harness h = new Harness(new MemoryNet(), disk, state)) {
          h.run(data(1, ascii("package")));
          // One turn of the loop, as the effects of one output: the end, then (the file durable,
          // or deleted, and its answer made meanwhile) the cancel.
          h.loop.submit(() -> {
            h.x.run(done(1));
            String last = fails ? "remove d1" : "close d1";
            for (int i = 0; i < 500 && !disk.ops.contains(last); i++) {
              Thread.sleep(10);
            }
            Thread.sleep(100);
            h.x.run(isCancel ? cancel(1) : discard(1));
            return null;
          }).get(10, TimeUnit.SECONDS);
          h.quiet(300);
          Path file = Executor.downloadsDir(state).resolve("d1");
          for (int i = 0; i < 100 && Files.exists(file); i++) {
            Thread.sleep(10);
          }
          assertFalse(Files.exists(file), "cancel: " + isCancel + ", a failed write: " + fails);
        }
      }
    }
  }

  @Test
  void aFailedWriteDeletesTheFileAndAnswersDownloadFailedOnce(@TempDir Path dir) throws Exception {
    SlowDisk disk = new SlowDisk();
    disk.failWrites = true;
    disk.permits.release(100);
    Path state = dir.resolve("state");
    try (Harness h = new Harness(new MemoryNet(), disk, state)) {
      h.run(data(1, ascii("doomed")));
      Thread.sleep(100);
      h.run(data(1, ascii("dropped")));
      h.run(done(1));
      CoreProto.Input in = h.next();
      assertTrue(in.getDownloadFailed().getDetail().contains("no space left"), in.toString());
      h.quiet(200);
      assertFalse(Files.exists(Executor.downloadsDir(state).resolve("d1")));
    }
  }

  @Test
  void shutdownWaitsForTheWriteInFlightDeletesFilesNotCompleteAndSaysNothing(@TempDir Path dir) throws Exception {
    SlowDisk disk = new SlowDisk();
    Path state = dir.resolve("state");
    try (Harness h = new Harness(new MemoryNet(), disk, state)) {
      h.run(data(1, ascii("half")));
      for (int i = 0; i < 100 && !disk.ops.contains("writing d1"); i++) {
        Thread.sleep(10);
      }
      CompletableFuture<Void> over = h.shutdown();
      Thread.sleep(100);
      assertFalse(over.isDone(), "the write in flight is waited for");
      disk.permits.release(10);
      over.get(5, TimeUnit.SECONDS);
      assertFalse(Files.exists(Executor.downloadsDir(state).resolve("d1")));
      h.quiet(200);
    }
  }

  // ---------------------------------------------------------------------------------------------
  // TLS, over real sockets.

  /**
   * The client certificate goes only to a handshake the core says is with the server's own origin
   * (stream_start_tls's server_origin); any other presents none, and the CA still applies.
   */
  // covers: RULE-TLS-02
  @Test
  void theClientCertificateOnlyAtTheServersOrigin(@TempDir Path dir) throws Exception {
    SSLContext server = Tls.builder().trustedCa(TransportTest.CA)
        .clientCertificate(TransportTest.pem("server.pem"), TransportTest.pem("server-key.pem")).build().context();
    try (ServerSocket listener = new ServerSocket(0, 50, InetAddress.getByName("127.0.0.1"));
        Harness h = new Harness(new Streams.Platform() {
          @Override
          public Streams.Conn connect(String host, int port, Streams.Attempt attempt) throws IOException {
            return Streams.SOCKETS.connect("127.0.0.1", listener.getLocalPort(), attempt);
          }

          @Override
          public List<String> resolve(String host) {
            return List.of();
          }
        }, Disk.REAL, dir.resolve("state"), TransportTest.withClientCert())) {
      List<String> peers = new CopyOnWriteArrayList<>();
      Thread t = new Thread(() -> {
        try {
          for (int i = 0; i < 2; i++) {
            Socket raw = listener.accept();
            SSLSocket s = (SSLSocket) server.getSocketFactory().createSocket(raw, null, raw.getPort(), true);
            s.setUseClientMode(false);
            s.setWantClientAuth(true);
            try (s) {
              s.startHandshake();
              String peer;
              try {
                peer = ((java.security.cert.X509Certificate) s.getSession().getPeerCertificates()[0])
                    .getSubjectX500Principal().getName();
              } catch (javax.net.ssl.SSLPeerUnverifiedException none) {
                peer = "none";
              }
              peers.add(peer);
              s.getInputStream().read(); // until the client goes
            }
          }
        } catch (Exception e) {
          peers.add("failed: " + e);
        }
      });
      t.start();
      for (long id : new long[] {1, 2}) {
        boolean origin = id == 1;
        h.run(open(id));
        CoreProto.Input opened = h.next();
        assertTrue(opened.hasStreamOpened(), opened.toString());
        h.run(startTls(id, "127.0.0.1", origin));
        CoreProto.Input ready = h.next();
        assertEquals(id, ready.getStreamTlsReady().getId(), ready.toString());
        for (int i = 0; i < 500 && peers.size() < id; i++) {
          Thread.sleep(10);
        }
        String peer = peers.get((int) id - 1);
        assertEquals(origin, peer.contains("test-agent"), "server_origin " + origin + ": " + peer);
        assertTrue(origin || peer.equals("none"), peer);
        h.run(close(id, false));
      }
      t.join(10_000);
    }
  }

  /** TLS up, bytes both ways under it; a handshake still going when its stream is closed says nothing. */
  @Test
  void tlsOverAStreamAndAHandshakeCompletingAfterItsCloseIsAborted(@TempDir Path dir) throws Exception {
    SSLContext server = Tls.builder().trustedCa(TransportTest.CA)
        .clientCertificate(TransportTest.pem("server.pem"), TransportTest.pem("server-key.pem")).build().context();
    try (ServerSocket listener = new ServerSocket(0, 50, InetAddress.getByName("127.0.0.1"));
        Harness h = new Harness(new Streams.Platform() {
          @Override
          public Streams.Conn connect(String host, int port, Streams.Attempt attempt) throws IOException {
            return Streams.SOCKETS.connect("127.0.0.1", listener.getLocalPort(), attempt);
          }

          @Override
          public List<String> resolve(String host) {
            return List.of();
          }
        }, Disk.REAL, dir.resolve("state"))) {
      // Echo over TLS.
      CountDownLatch go = new CountDownLatch(1);
      List<Throwable> serverSaw = new CopyOnWriteArrayList<>();
      Thread t = new Thread(() -> {
        try {
          for (int i = 0; i < 4; i++) {
            Socket raw = listener.accept();
            if (i == 1) {
              go.await();
            }
            SSLSocket s = (SSLSocket) server.getSocketFactory().createSocket(raw, null, raw.getPort(), true);
            s.setUseClientMode(false);
            try (s) {
              InputStream in = s.getInputStream();
              byte[] b = new byte[5];
              int n = in.readNBytes(b, 0, 5);
              s.getOutputStream().write(b, 0, n);
              s.getOutputStream().flush();
            } catch (IOException e) {
              serverSaw.add(e);
            }
          }
        } catch (Exception e) {
          serverSaw.add(e);
        }
      });
      t.start();
      h.run(open(1));
      assertTrue(h.next().hasStreamOpened());
      h.run(startTls(1, "127.0.0.1"));
      assertEquals(1, h.next().getStreamTlsReady().getId());
      h.run(write(1, ascii("hello")));
      assertEquals("hello", h.next().getStreamData().getData().toStringUtf8());
      h.run(close(1, false));
      // The second: closed during its handshake, which the server then completes (or tries to).
      h.run(open(2));
      CoreProto.Input in = h.next();
      if (in.hasStreamEof()) {
        in = h.next(); // the first's: the server ended it at once, maybe before its close
      }
      assertTrue(in.hasStreamOpened(), in.toString());
      h.run(startTls(2, "localhost"));
      Thread.sleep(100);
      h.run(close(2, false));
      go.countDown();
      // A certificate verified for the name or the address the core gives: one it doesn't name fails.
      for (long id : new long[] {3, 4}) {
        h.run(open(id));
        CoreProto.Input opened = h.next();
        assertTrue(opened.hasStreamOpened(), opened.toString());
        h.run(startTls(id, id == 3 ? "not-in-the-certificate.example" : "192.0.2.7"));
        CoreProto.Input failed = h.next();
        assertEquals(id, failed.getStreamFailed().getId(), failed.toString());
      }
      t.join(10_000);
      h.quiet(300);
      for (int i = 0; i < 100 && !h.empty(); i++) {
        Thread.sleep(10);
      }
      assertEquals("", h.left());
    }
  }
}
