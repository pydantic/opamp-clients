// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.io.ByteArrayOutputStream;
import java.io.EOFException;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.net.SocketTimeoutException;
import java.nio.ByteBuffer;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.locks.ReentrantLock;
import javax.net.ssl.SSLContext;
import javax.net.ssl.SSLEngine;
import javax.net.ssl.SSLEngineResult;
import javax.net.ssl.SSLSocket;
import org.junit.jupiter.api.Test;

/**
 * The adversary's raw server: what the
 * streams of a core mode run of {@link AdversaryPropertiesTest} connect to. A stream is bytes, not
 * HTTP, so the adversary tells the server what to do in the first line it writes:
 *
 * <pre>opamp-raw tag=7 send=1000 chunk=100 gap=5 end=fin</pre>
 *
 * <p>The server answers {@code ok\n}, then does what the line says, and keeps for each connection
 * that said who it was (tag) how many bytes it read after the line, whether each was the pattern's
 * (byte i is i % 251), and how the connection ended; the adversary asks for that on another stream
 * (ask=TAG). One server per run, on a port of its own, so every connection is that run's, those
 * that never say who they are too. As go/opamp/adversary_raw_test.go, the reference.
 */
class AdversaryRawTest {
  /** The longest first line the server reads. */
  private static final int LINE = 256;
  /** The name the raw server's certificate is for, which the adversary starts TLS under (tls=NAME). */
  static final String TLS_NAME = "localhost";

  /**
   * What tls=1 starts TLS with: the tests' server certificate (src/test/resources/tls: for
   * 127.0.0.1 and localhost, under {@link TransportTest#CA}).
   */
  static SSLContext certificate() {
    return Tls.builder().trustedCa(TransportTest.CA)
        .clientCertificate(TransportTest.pem("server.pem"), TransportTest.pem("server-key.pem")).build().context();
  }

  /** What the server knows of one connection. Its fields are the server's lock's. */
  static final class Conn {
    final Socket socket;
    /** When (System.nanoTime) it was accepted. */
    final long accepted;
    /** Its first line came. */
    boolean said;
    /** Bytes read after the line. */
    long got;
    /** One of them wasn't the pattern's. */
    boolean bad;
    /** null: open; fin: the peer closed its side; reset: it failed; closed: the server ended it. */
    String end;

    Conn(Socket socket, long accepted) {
      this.socket = socket;
      this.accepted = accepted;
    }
  }

  /** One run's raw server. */
  static final class Server implements AutoCloseable {
    private final ServerSocket listener = new ServerSocket(0, 200, InetAddress.getByName("127.0.0.1"));
    /** What tls=1 starts TLS with (null: it can't). */
    private final SSLContext tls;
    /** Open once the run's stop has returned: read=0 connections read on. */
    private final CountDownLatch released = new CountDownLatch(1);
    /** Open at teardown. */
    private final CountDownLatch quit = new CountDownLatch(1);
    /** Guards what follows; notified each time a connection says who it is or ends. */
    private final Object lock = new Object();
    /** Every connection, but the harness's marker. */
    private final List<Conn> conns = new ArrayList<>();
    /** Those that said who they were. */
    private final Map<Long, Conn> tags = new HashMap<>();
    /** When the marker's connection was accepted (null: none came). */
    private Long marked;

    /** A raw server on a port of its own. */
    Server(SSLContext tls) throws IOException {
      this.tls = tls;
      start("adversary-raw-accept", () -> {
        while (true) {
          try {
            Socket s = listener.accept();
            Conn rc = new Conn(s, System.nanoTime());
            // Its run's from the moment it is accepted, whether or not it ever says who it is.
            note(() -> conns.add(rc));
            if (quit.getCount() == 0) {
              s.close(); // accepted as the server was torn down
            }
            start("adversary-raw", () -> serve(rc));
          } catch (IOException e) {
            return;
          }
        }
      });
    }

    private static void start(String name, Runnable r) {
      Thread t = new Thread(r, name);
      t.setDaemon(true);
      t.start();
    }

    int port() {
      return listener.getLocalPort();
    }

    /**
     * Let the connections told not to read (read=0) read on: the run's stop has returned, and only
     * a read tells the server a connection has gone.
     */
    void release() {
      released.countDown();
    }

    /** End the server and every connection. */
    @Override
    public void close() throws IOException {
      release();
      quit.countDown();
      listener.close();
      List<Conn> all;
      synchronized (lock) {
        all = new ArrayList<>(conns);
        lock.notifyAll();
      }
      for (Conn rc : all) {
        rc.socket.close();
      }
    }

    /** Change what the server knows, and wake whoever waits for a connection to say who it is or end. */
    private void note(Runnable change) {
      synchronized (lock) {
        change.run();
        lock.notifyAll();
      }
    }

    private void ended(Conn rc, String how) {
      note(() -> {
        if (rc.end == null) {
          rc.end = how;
        }
      });
    }

    /** The connections still open, and how many of them never said who they were. */
    int[] open() {
      int all = 0;
      int anonymous = 0;
      synchronized (lock) {
        for (Conn rc : conns) {
          if (rc.end == null) {
            all++;
            if (!rc.said) {
              anonymous++;
            }
          }
        }
      }
      return new int[] {all, anonymous};
    }

    /**
     * What breaks RULE-SHELL-07 in when the run's connections were accepted (null: nothing does):
     * one accepted more than {@code graceMs} after the marker's, which the harness opened the
     * moment stop returned. (The server accepts in the order the connections were made.)
     */
    String afterMark(long graceMs) {
      synchronized (lock) {
        if (marked == null) {
          return "the raw server never got the harness's marker";
        }
        for (Conn rc : conns) {
          long late = (rc.accepted - marked) / 1_000_000;
          if (late > graceMs) {
            return "RULE-SHELL-07: a stream's connection reached the raw server " + late
                + " ms after one the harness made as stop returned";
          }
        }
        return null;
      }
    }

    /** Wait {@code ms}: false if the server is torn down first. */
    private boolean sleep(long ms) {
      if (ms <= 0) {
        return true;
      }
      try {
        return !quit.await(ms, TimeUnit.MILLISECONDS);
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
        return false;
      }
    }

    /** The first line, without its newline ({@link #LINE} bytes at most); null if none came. */
    private static String firstLine(InputStream in) throws IOException {
      ByteArrayOutputStream line = new ByteArrayOutputStream();
      // A byte at a time: what follows the line is the connection's (TLS's, with tls=1).
      while (line.size() < LINE) {
        int b = in.read();
        if (b < 0) {
          return null;
        }
        if (b == '\n') {
          return line.toString(StandardCharsets.ISO_8859_1);
        }
        line.write(b);
      }
      return null;
    }

    private static long num(Map<String, String> set, String name, long dflt) {
      try {
        return Long.parseLong(set.getOrDefault(name, ""));
      } catch (NumberFormatException e) {
        return dflt;
      }
    }

    private void serve(Conn rc) {
      Socket c = rc.socket;
      try (c) {
        InputStream plain = c.getInputStream();
        String line = null;
        try {
          line = firstLine(plain);
        } catch (IOException e) {
          // it went before it said who it was
        }
        Map<String, String> set = new HashMap<>();
        if (line != null && line.startsWith("opamp-raw ")) {
          for (String kv : line.substring("opamp-raw ".length()).trim().split("\\s+")) {
            int eq = kv.indexOf('=');
            if (eq >= 0) {
              set.put(kv.substring(0, eq), kv.substring(eq + 1));
            }
          }
        } else {
          line = null; // not the adversary's
        }
        if (line != null && num(set, "mark", 0) == 1) {
          // The harness's own, not the run's: when what was opened as stop returned got here.
          note(() -> {
            conns.remove(rc);
            marked = rc.accepted;
          });
          c.getOutputStream().write("ok\n".getBytes(StandardCharsets.US_ASCII));
          c.getOutputStream().flush();
          return;
        }
        if (line == null) {
          // It went, or said something else, before it said who it was.
          ended(rc, "closed");
          return;
        }
        long tag = num(set, "tag", 0);
        note(() -> {
          rc.said = true;
          tags.put(tag, rc);
        });

        Wire wire = new Wire(plain, c.getOutputStream());
        long delay = num(set, "delay", 0);
        boolean held = delay < 0; // never answered: only read, until it goes
        if (!held) {
          try {
            if (!sleep(delay)) {
              ended(rc, "closed");
              return;
            }
            wire.write("ok\n".getBytes(StandardCharsets.US_ASCII));
            if (num(set, "tls", 0) == 1) {
              if (tls == null) {
                ended(rc, "closed");
                return;
              }
              c.setSoTimeout(30_000);
              wire.startTls(tls);
              c.setSoTimeout(0);
            }
          } catch (IOException | RuntimeException e) {
            ended(rc, "closed");
            return;
          }
        }

        // What the line asks the server to send, then how it ends the stream, beside the reading.
        CountDownLatch sent = new CountDownLatch(1);
        start("adversary-raw-send", () -> {
          try {
            if (!held) {
              send(rc, wire, set);
            }
          } catch (IOException | RuntimeException e) {
            // the connection went
          } finally {
            sent.countDown();
          }
        });

        if (num(set, "read", 1) == 0) {
          // Reads nothing until the run is over.
          released.await();
        }
        boolean echo = num(set, "echo", 0) == 1;
        byte[] buf = new byte[64 << 10];
        String how;
        while (true) {
          int n;
          try {
            n = wire.read(buf);
          } catch (IOException | RuntimeException e) {
            how = "reset";
            break;
          }
          if (n < 0) {
            how = "fin";
            break;
          }
          synchronized (lock) {
            for (int i = 0; i < n; i++) {
              if (buf[i] != (byte) ((rc.got + i) % 251)) {
                rc.bad = true;
              }
            }
            rc.got += n;
          }
          if (echo) {
            try {
              wire.write(buf, 0, n);
            } catch (IOException | RuntimeException e) {
              how = "reset";
              break;
            }
          }
        }
        ended(rc, how);
        c.close();
        sent.await();
      } catch (IOException e) {
        // the connection went
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
      } finally {
        ended(rc, "closed");
      }
    }

    /** What the line asks the server to send (an answer, the pattern), then how it ends the stream. */
    private void send(Conn rc, Wire wire, Map<String, String> set) throws IOException {
      Socket c = rc.socket;
      if (set.containsKey("ask")) {
        long about;
        try {
          about = Long.parseUnsignedLong(set.get("ask"));
        } catch (NumberFormatException e) {
          about = 0;
        }
        wire.write(answer(about, num(set, "wait", 0)).getBytes(StandardCharsets.US_ASCII));
      }
      byte[] body = AdversaryPropertiesTest.AdversaryServer.body((int) Math.max(num(set, "send", 0), 0));
      int chunk = (int) num(set, "chunk", 0);
      if (chunk <= 0) {
        chunk = Math.max(body.length, 1);
      }
      for (int off = 0; off < body.length; off += chunk) {
        if (off > 0 && !sleep(num(set, "gap", 0))) {
          return;
        }
        wire.write(body, off, Math.min(chunk, body.length - off));
      }
      String end = set.getOrDefault("end", "hold");
      if (end.isEmpty() || end.equals("hold")) {
        return;
      }
      if (!sleep(num(set, "after", 0))) {
        return;
      }
      switch (end) {
        // Its side only: it still reads.
        case "fin" -> c.shutdownOutput();
        case "close" -> {
          ended(rc, "closed");
          wire.close();
          c.close();
        }
        case "reset" -> {
          ended(rc, "closed");
          c.setSoLinger(true, 0);
          c.close();
        }
        default -> {}
      }
    }

    /**
     * Wait until connection {@code tag} has ended (tag 0: until no connection that never said who
     * it was is open), {@code waitMs} at most, and say what the server knows of it: {@code "STATE
     * GOT BAD\n"}.
     */
    private String answer(long tag, long waitMs) {
      long deadline = System.nanoTime() + waitMs * 1_000_000L;
      synchronized (lock) {
        while (true) {
          String state = "none";
          long got = 0;
          int bad = 0;
          if (tag == 0) {
            for (Conn rc : conns) {
              if (rc.end == null && !rc.said) {
                state = "open";
                got++;
              }
            }
          } else {
            Conn rc = tags.get(tag);
            if (rc != null) {
              state = rc.end == null ? "open" : rc.end;
              got = rc.got;
              bad = rc.bad ? 1 : 0;
            }
          }
          String answer = state + " " + got + " " + bad + "\n";
          long left = deadline - System.nanoTime();
          if (!state.equals("open") || left <= 0 || quit.getCount() == 0) {
            return answer;
          }
          try {
            TimeUnit.NANOSECONDS.timedWait(lock, left);
          } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            return answer;
          }
        }
      }
    }
  }

  /**
   * A connection's bytes after its first line: plain, or inside TLS once {@link #startTls} is
   * over. One thread reads; writes (the reader's echoes, the sender's bytes) go one at a time.
   *
   * <p>TLS is the JDK's engine, driven here: an {@code SSLSocket} reads a connection that ended
   * without TLS's close as one that closed, and the raw server tells the two apart (fin: TLS's
   * close; reset: the connection went without it), as Go's does.
   */
  static final class Wire {
    private static final ByteBuffer EMPTY = ByteBuffer.allocate(0);
    private final InputStream in;
    private final OutputStream out;
    private final ReentrantLock writing = new ReentrantLock();
    private SSLEngine engine;
    /** Read from the connection, not yet unwrapped (ready to be taken from). */
    private ByteBuffer net;
    /** Unwrapped, not yet handed over (ready to be added to). */
    private ByteBuffer app;
    /** TLS's close came. */
    private boolean closed;

    Wire(InputStream in, OutputStream out) {
      this.in = in;
      this.out = out;
    }

    /** Complete a TLS handshake as the server: everything after is inside it. */
    void startTls(SSLContext ctx) throws IOException {
      SSLEngine e = ctx.createSSLEngine();
      e.setUseClientMode(false);
      net = ByteBuffer.allocate(e.getSession().getPacketBufferSize()).flip();
      app = ByteBuffer.allocate(e.getSession().getApplicationBufferSize());
      engine = e;
      e.beginHandshake();
      while (true) {
        switch (e.getHandshakeStatus()) {
          case NEED_TASK -> tasks();
          case NEED_WRAP -> wrap(EMPTY);
          case NEED_UNWRAP -> {
            if (!unwrap()) {
              throw new EOFException("TLS closed in its handshake");
            }
          }
          default -> {
            return;
          }
        }
      }
    }

    private void tasks() {
      for (Runnable t = engine.getDelegatedTask(); t != null; t = engine.getDelegatedTask()) {
        t.run();
      }
    }

    /** Up to {@code b.length} bytes; -1 at the peer's end (inside TLS: its close). */
    int read(byte[] b) throws IOException {
      if (engine == null) {
        return in.read(b);
      }
      while (app.position() == 0) {
        if (closed || !unwrap()) {
          closed = true;
          return -1;
        }
        // What the record asks of this end: a task, or bytes of its own.
        for (boolean more = true; more; ) {
          switch (engine.getHandshakeStatus()) {
            case NEED_TASK -> tasks();
            case NEED_WRAP -> wrap(EMPTY);
            default -> more = false;
          }
        }
      }
      app.flip();
      int n = Math.min(b.length, app.remaining());
      app.get(b, 0, n);
      app.compact();
      return n;
    }

    /**
     * One record in, read from the connection if it isn't all here: false at TLS's close. A
     * connection that ends without it fails.
     */
    private boolean unwrap() throws IOException {
      while (true) {
        SSLEngineResult r = engine.unwrap(net, app);
        switch (r.getStatus()) {
          case OK -> {
            return true;
          }
          case CLOSED -> {
            return false;
          }
          case BUFFER_OVERFLOW -> {
            ByteBuffer bigger = ByteBuffer.allocate(app.capacity() * 2);
            app.flip();
            bigger.put(app);
            app = bigger;
          }
          case BUFFER_UNDERFLOW -> {
            net.compact();
            if (!net.hasRemaining()) {
              ByteBuffer bigger = ByteBuffer.allocate(net.capacity() * 2);
              net.flip();
              bigger.put(net);
              net = bigger;
            }
            int n = in.read(net.array(), net.arrayOffset() + net.position(), net.remaining());
            if (n < 0) {
              net.flip();
              throw new EOFException("the connection ended without TLS's close");
            }
            net.position(net.position() + n);
            net.flip();
          }
        }
      }
    }

    void write(byte[] b) throws IOException {
      write(b, 0, b.length);
    }

    void write(byte[] b, int off, int len) throws IOException {
      if (engine != null) {
        wrap(ByteBuffer.wrap(b, off, len));
        return;
      }
      writing.lock();
      try {
        out.write(b, off, len);
        out.flush();
      } finally {
        writing.unlock();
      }
    }

    /** {@code src} (or, with none, what the engine has to say itself) through TLS to the connection. */
    private void wrap(ByteBuffer src) throws IOException {
      writing.lock();
      try {
        ByteBuffer dst = ByteBuffer.allocate(engine.getSession().getPacketBufferSize());
        do {
          SSLEngineResult r = engine.wrap(src, dst);
          if (r.getStatus() == SSLEngineResult.Status.BUFFER_OVERFLOW) {
            dst = ByteBuffer.allocate(dst.capacity() * 2);
            continue;
          }
          dst.flip();
          out.write(dst.array(), dst.arrayOffset(), dst.limit());
          dst.clear();
          if (r.getStatus() == SSLEngineResult.Status.CLOSED) {
            if (src.hasRemaining()) {
              throw new IOException("the TLS connection is closed");
            }
            break;
          }
          if (r.bytesConsumed() == 0 && r.bytesProduced() == 0) {
            if (src.hasRemaining() || engine.getHandshakeStatus() == SSLEngineResult.HandshakeStatus.NEED_WRAP) {
              throw new IOException("TLS made nothing of what was to be written (" + r + ")");
            }
            break;
          }
        } while (src.hasRemaining());
        out.flush();
      } finally {
        writing.unlock();
      }
    }

    /**
     * TLS's close, if TLS is up and no write is under way (one blocked on a peer that doesn't read
     * would hold this too: the connection's close ends it). Never throws.
     */
    void close() {
      if (engine == null || !writing.tryLock()) {
        return;
      }
      try {
        engine.closeOutbound();
        wrap(EMPTY);
      } catch (IOException | RuntimeException e) {
        // the connection is going anyway
      } finally {
        writing.unlock();
      }
    }
  }

  /**
   * The harness's marker on a run's raw server: a connection of its own, made as this is (the
   * moment stop returns), that says {@code opamp-raw mark=1}.
   */
  static final class Marker {
    private Socket socket;
    private String error;

    Marker(int port, int boundMs) {
      try {
        socket = new Socket();
        socket.connect(new InetSocketAddress(InetAddress.getByName("127.0.0.1"), port), boundMs);
        socket.setSoTimeout(boundMs);
        socket.getOutputStream().write("opamp-raw mark=1\n".getBytes(StandardCharsets.US_ASCII));
        socket.getOutputStream().flush();
      } catch (IOException e) {
        error = e.toString();
      }
    }

    /** Wait for the answer: what went wrong, or null if the server said ok. */
    String answered() {
      try (Socket s = socket) {
        if (error != null) {
          return error;
        }
        String line = new String(s.getInputStream().readNBytes(3), StandardCharsets.US_ASCII);
        return line.equals("ok\n") ? null : "the raw server answered the marker \"" + line + "\"";
      } catch (IOException e) {
        return e.toString();
      }
    }
  }

  // -----------------------------------------------------------------------------------------------
  // The server by itself.

  private final List<Socket> dialled = new ArrayList<>();

  /** A connection that wrote {@code line} and read the server's ok. */
  private Socket dial(Server s, String line) throws IOException {
    Socket c = new Socket(InetAddress.getByName("127.0.0.1"), s.port());
    dialled.add(c);
    c.setSoTimeout(20_000);
    c.getOutputStream().write((line + "\n").getBytes(StandardCharsets.US_ASCII));
    c.getOutputStream().flush();
    assertEquals("ok\n", line(c), line);
    return c;
  }

  /** The next line, with its newline (what came, if the connection ended first). */
  private static String line(Socket c) throws IOException {
    ByteArrayOutputStream b = new ByteArrayOutputStream();
    InputStream in = c.getInputStream();
    for (int ch = in.read(); ch >= 0; ch = in.read()) {
      b.write(ch);
      if (ch == '\n') {
        break;
      }
    }
    return b.toString(StandardCharsets.US_ASCII);
  }

  /** What the server says of connection {@code tag}, having waited up to {@code waitMs} for its end. */
  private String ask(Server s, long tag, long waitMs) throws IOException {
    return line(dial(s, "opamp-raw tag=" + (1000 + tag) + " ask=" + tag + " wait=" + waitMs));
  }

  private static byte[] body(int size) {
    return AdversaryPropertiesTest.AdversaryServer.body(size);
  }

  /** TLS as a client over {@code c}, under the certificate's name. */
  private static SSLSocket tlsClient(Socket c) throws IOException {
    SSLSocket t = (SSLSocket) TransportTest.trusting(TransportTest.CA).context().getSocketFactory()
        .createSocket(c, TLS_NAME, c.getPort(), true);
    t.setUseClientMode(true);
    javax.net.ssl.SSLParameters p = t.getSSLParameters();
    p.setEndpointIdentificationAlgorithm("HTTPS");
    t.setSSLParameters(p);
    t.startHandshake();
    return t;
  }

  /** The raw server does what its first line says, and tells what it saw of a connection. */
  // covers: RULE-SHELL-07
  @Test
  void theRawServerDoesWhatItsFirstLineSays() throws Exception {
    try (Server s = new Server(certificate())) {
      // send, in pieces, then its side closed: the pattern, then the end.
      Socket c = dial(s, "opamp-raw tag=1 send=1000 chunk=300 gap=1 end=fin");
      assertArrayEquals(body(1000), c.getInputStream().readAllBytes(), "send=1000");

      // echo, and what it read: all of the pattern, then the peer's end.
      c = dial(s, "opamp-raw tag=2 echo=1");
      c.getOutputStream().write(body(5000));
      assertArrayEquals(body(5000), c.getInputStream().readNBytes(5000), "echo");
      c.shutdownOutput();
      assertEquals("fin 5000 0\n", ask(s, 2, 5000), "after the pattern and a close");

      // Bytes that aren't the pattern's, and a reset.
      c = dial(s, "opamp-raw tag=3");
      c.getOutputStream().write(new byte[] {0, 1, 9});
      Thread.sleep(100);
      c.setSoLinger(true, 0);
      c.close();
      assertEquals("reset 3 1\n", ask(s, 3, 5000), "after three bytes, one wrong, and a reset");

      // One still open is said to be, once the wait is over; one never seen is none.
      dial(s, "opamp-raw tag=4");
      assertEquals("open 0 0\n", ask(s, 4, 200), "a connection still open");
      assertEquals("none 0 0\n", ask(s, 99, 5000), "a connection never made");

      // A connection that never says who it is counts until it goes.
      Socket anon = new Socket(InetAddress.getByName("127.0.0.1"), s.port());
      assertEquals("open 1 0\n", ask(s, 0, 300), "with a connection that never said who it was");
      assertEquals(1, s.open()[1], "counted among those open");
      anon.close();
      assertEquals("none 0 0\n", ask(s, 0, 5000), "once it has gone");
      // One that says something else is closed.
      Socket other = new Socket(InetAddress.getByName("127.0.0.1"), s.port());
      dialled.add(other);
      other.setSoTimeout(20_000);
      other.getOutputStream().write("GET / HTTP/1.1\n".getBytes(StandardCharsets.US_ASCII));
      assertEquals("", line(other), "a first line that isn't the adversary's");

      // TLS after ok, under the certificate's name; then the pattern inside it, and a close.
      c = dial(s, "opamp-raw tag=6 tls=1 send=100 end=close");
      assertArrayEquals(body(100), tlsClient(c).getInputStream().readAllBytes(), "tls=1 send=100");
      // Inside TLS the peer's end is TLS's close: a connection that goes without it has failed.
      c = dial(s, "opamp-raw tag=61 tls=1 echo=1");
      SSLSocket t = tlsClient(c);
      t.getOutputStream().write(body(3000));
      assertArrayEquals(body(3000), t.getInputStream().readNBytes(3000), "echo inside TLS");
      t.close();
      assertEquals("fin 3000 0\n", ask(s, 61, 5000), "after the pattern and TLS's close");
      c = dial(s, "opamp-raw tag=62 tls=1 echo=1");
      t = tlsClient(c);
      t.getOutputStream().write(body(10));
      assertArrayEquals(body(10), t.getInputStream().readNBytes(10));
      c.shutdownOutput(); // the connection's end, without TLS's
      assertEquals("reset 10 0\n", ask(s, 62, 5000), "a connection that ended without TLS's close");

      // read=0 reads nothing until the run is released; delay=-1 never answers; reset is one.
      c = dial(s, "opamp-raw tag=7 read=0");
      c.getOutputStream().write(body(10));
      c.shutdownOutput();
      assertEquals("open 0 0\n", ask(s, 7, 200), "read=0 before the release");
      s.release();
      assertEquals("fin 10 0\n", ask(s, 7, 5000), "read=0 after the release");
      Socket reset = dial(s, "opamp-raw tag=8 end=reset after=50");
      assertThrows(IOException.class, () -> reset.getInputStream().readAllBytes(), "end=reset ended the stream cleanly");
      Socket silent = new Socket(InetAddress.getByName("127.0.0.1"), s.port());
      dialled.add(silent);
      silent.getOutputStream().write("opamp-raw tag=9 delay=-1\n".getBytes(StandardCharsets.US_ASCII));
      silent.setSoTimeout(200);
      assertThrows(SocketTimeoutException.class, () -> silent.getInputStream().read(), "delay=-1 answered");

      // The marker: a connection accepted after it is late, one before it isn't.
      long grace = 50;
      String said = s.afterMark(grace);
      assertTrue(said != null && said.contains("never got"), "before the marker: " + said);
      assertNull(new Marker(s.port(), 30_000).answered(), "the marker");
      assertNull(s.afterMark(grace), "nothing came after the marker");
      Thread.sleep(2 * grace);
      dial(s, "opamp-raw tag=10");
      said = s.afterMark(grace);
      assertNotNull(said, "a connection made after the marker passed");
      assertTrue(said.contains("RULE-SHELL-07"), said);
      // The marker is no connection of the run's: those open are the ones this left open.
      assertEquals(0, s.open()[1], "no connection that never said who it was is left");
    } finally {
      for (Socket c : dialled) {
        c.close();
      }
    }
  }
}
