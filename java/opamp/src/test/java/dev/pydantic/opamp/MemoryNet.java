// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.LinkedBlockingQueue;

/**
 * Core mode's network in memory ({@link Streams.Platform}): each connection the client asks for
 * waits as a {@link Dial} until the test accepts it (a pair of bounded pipes, the far end the
 * test's {@link End}) or refuses it; names resolve as the test says. Only monitors (wait and
 * notify) and threads: Fray explores it as it explores the rest (StreamsFrayTest, ShellFrayTest
 * in core mode), and a plain test uses it to choose when each step completes.
 */
final class MemoryNet implements Streams.Platform {
  /** Bytes a pipe holds before its writer waits: the kernel's buffers. */
  final int window;
  /** Attempts to connect, in order, for the test to accept or refuse. */
  final LinkedBlockingQueue<Dial> dials = new LinkedBlockingQueue<>();
  /** Every attempt, and so every connection, there has been. */
  final List<Dial> all = new CopyOnWriteArrayList<>();
  /** What names resolve to (a name not here fails). */
  final Map<String, List<String>> names = new ConcurrentHashMap<>();
  /** Each name asked for, in order. */
  final List<String> resolved = new CopyOnWriteArrayList<>();

  MemoryNet(int window) {
    this.window = window;
  }

  MemoryNet() {
    this(64 << 10);
  }

  @Override
  public Streams.Conn connect(String host, int port, Streams.Attempt attempt) throws IOException {
    Dial d = new Dial(host, port, window);
    if (!attempt.onAbort(d::abort)) {
      throw new IOException("closed while connecting");
    }
    all.add(d);
    dials.add(d);
    return d.await();
  }

  /** While closed, resolutions wait (a test's to open). */
  private boolean resolveHeld;

  synchronized void holdResolutions(boolean hold) {
    resolveHeld = hold;
    notifyAll();
  }

  @Override
  public List<String> resolve(String host) throws IOException {
    synchronized (this) {
      while (resolveHeld) {
        try {
          wait();
        } catch (InterruptedException e) {
          Thread.currentThread().interrupt();
          throw new java.io.InterruptedIOException();
        }
      }
    }
    resolved.add(host);
    List<String> a = names.get(host);
    if (a == null) {
      throw new IOException("no such name: " + host);
    }
    return a;
  }

  /** Whether every connection there has been is over at the client's end (closed or reset), and no attempt waits. */
  String leftOpen() {
    StringBuilder b = new StringBuilder();
    for (Dial d : all) {
      String s = d.state();
      if (!s.isEmpty()) {
        b.append(b.length() == 0 ? "" : "; ").append(d.host).append(':').append(d.port).append(' ').append(s);
      }
    }
    return b.toString();
  }

  /** An attempt to connect, until the test accepts or refuses it (or the client gives up on it). */
  static final class Dial {
    final String host;
    final int port;
    private final int window;
    private End client;
    private End server;
    private String refused;
    private boolean aborted;

    Dial(String host, int port, int window) {
      this.host = host;
      this.port = port;
      this.window = window;
    }

    /** Accept it: the far end (one already reset if the client gave up on the attempt). */
    synchronized End accept() {
      if (client == null && refused == null) {
        Pipe up = new Pipe(window);
        Pipe down = new Pipe(window);
        client = new End(down, up);
        server = new End(up, down);
        if (aborted) {
          client.abort();
        }
        notifyAll();
      }
      return server;
    }

    synchronized void refuse(String why) {
      if (client == null && refused == null) {
        refused = why;
        notifyAll();
      }
    }

    /** The client gave up on it: it fails, and a connection it made is reset. */
    void abort() {
      End c;
      synchronized (this) {
        aborted = true;
        notifyAll();
        c = client;
      }
      if (c != null) {
        c.abort();
      }
    }

    /** Wait until the client gives up on the attempt (its close, or the client's end). */
    synchronized void awaitAborted() throws InterruptedException {
      while (!aborted) {
        wait();
      }
    }

    synchronized boolean answered() {
      return client != null || refused != null;
    }

    synchronized boolean aborted() {
      return aborted;
    }

    synchronized Streams.Conn await() throws IOException {
      while (client == null && refused == null && !aborted) {
        try {
          wait();
        } catch (InterruptedException e) {
          Thread.currentThread().interrupt();
          throw new IOException("interrupted");
        }
      }
      if (refused != null) {
        throw new IOException(refused);
      }
      if (aborted && client == null) {
        throw new IOException("closed while connecting");
      }
      return client;
    }

    /** What is left of it at the client's end: empty if nothing (refused, given up, closed or reset). */
    synchronized String state() {
      if (client == null) {
        return refused != null || aborted ? "" : "still connecting";
      }
      return client.over() ? "" : "still open";
    }

    /** The client's end, once accepted. */
    synchronized End client() {
      return client;
    }
  }

  /** One end of a connection: what it reads, and what it writes. */
  static final class End implements Streams.Conn {
    final Pipe in;
    final Pipe out;
    private boolean closed;
    private boolean aborted;

    End(Pipe in, Pipe out) {
      this.in = in;
      this.out = out;
    }

    @Override
    public InputStream input() {
      return new InputStream() {
        @Override
        public int read() throws IOException {
          byte[] b = new byte[1];
          return read(b, 0, 1) < 0 ? -1 : b[0] & 0xff;
        }

        @Override
        public int read(byte[] b, int off, int len) throws IOException {
          return in.read(b, off, len);
        }
      };
    }

    @Override
    public OutputStream output() {
      return new OutputStream() {
        @Override
        public void write(int b) throws IOException {
          write(new byte[] {(byte) b}, 0, 1);
        }

        @Override
        public void write(byte[] b, int off, int len) throws IOException {
          out.write(b, off, len);
        }
      };
    }

    /** A FIN after what was written; this end reads nothing more. */
    @Override
    public void close() {
      synchronized (this) {
        closed = true;
      }
      out.close();
      in.stopReading();
    }

    /** A reset, both ways; nothing once closed (a socket's, closed already). */
    @Override
    public void abort() {
      synchronized (this) {
        if (closed) {
          return;
        }
        aborted = true;
      }
      out.reset();
      in.reset();
    }

    /** A FIN only (shutdownOutput): this end still reads. */
    void finish() {
      out.close();
    }

    synchronized boolean over() {
      return closed || aborted;
    }

    synchronized boolean aborted() {
      return aborted;
    }

    synchronized boolean closed() {
      return closed;
    }

    /** Read exactly {@code n} bytes, or fewer at the end. */
    byte[] read(int n) throws IOException {
      byte[] b = new byte[n];
      int got = 0;
      while (got < n) {
        int k = in.read(b, got, n - got);
        if (k < 0) {
          return java.util.Arrays.copyOf(b, got);
        }
        got += k;
      }
      return b;
    }

    /** A line, without its CRLF; null at the end. */
    String line() throws IOException {
      StringBuilder s = new StringBuilder();
      byte[] one = new byte[1];
      while (true) {
        int k = in.read(one, 0, 1);
        if (k < 0) {
          return s.length() == 0 ? null : s.toString();
        }
        if (one[0] == '\n') {
          int len = s.length();
          return len > 0 && s.charAt(len - 1) == '\r' ? s.substring(0, len - 1) : s.toString();
        }
        s.append((char) (one[0] & 0xff));
      }
    }

    void write(byte[] b) throws IOException {
      out.write(b, 0, b.length);
    }
  }

  /** Bytes one way, at most {@code capacity} waiting: the writer waits for the reader. */
  static final class Pipe {
    private final byte[] buf;
    private int head;
    private int size;
    /** The writer's FIN: the reader reads what is left, then the end. */
    private boolean closed;
    /** A reset: both ends fail at once. */
    private boolean reset;
    /** The reader closed its end: a write fails, a read fails. */
    private boolean gone;
    /** Reads waiting for a byte. */
    private int reading;

    Pipe(int capacity) {
      buf = new byte[capacity];
    }

    synchronized void write(byte[] b, int off, int len) throws IOException {
      while (len > 0) {
        while (!reset && !closed && !gone && size == buf.length) {
          waitQuietly();
        }
        if (reset) {
          throw new IOException("connection reset");
        }
        if (closed) {
          throw new IOException("closed");
        }
        if (gone) {
          throw new IOException("broken pipe");
        }
        int n = Math.min(len, buf.length - size);
        for (int i = 0; i < n; i++) {
          buf[(head + size + i) % buf.length] = b[off + i];
        }
        size += n;
        off += n;
        len -= n;
        notifyAll();
      }
    }

    synchronized int read(byte[] b, int off, int len) throws IOException {
      if (len == 0) {
        return 0;
      }
      reading++;
      try {
        while (size == 0 && !closed && !reset && !gone) {
          waitQuietly();
        }
      } finally {
        reading--;
      }
      if (reset) {
        throw new IOException("connection reset");
      }
      if (gone) {
        throw new IOException("socket closed");
      }
      if (size == 0) {
        return -1;
      }
      int n = Math.min(len, size);
      for (int i = 0; i < n; i++) {
        b[off + i] = buf[(head + i) % buf.length];
      }
      head = (head + n) % buf.length;
      size -= n;
      notifyAll();
      return n;
    }

    synchronized void close() {
      closed = true;
      notifyAll();
    }

    synchronized void reset() {
      reset = true;
      notifyAll();
    }

    synchronized void stopReading() {
      gone = true;
      notifyAll();
    }

    /** Bytes waiting to be read. */
    synchronized int waiting() {
      return size;
    }

    /** Whether a read is waiting for a byte: the reader is in its blocking read. */
    synchronized boolean readerWaiting() {
      return reading > 0 && size == 0;
    }

    private void waitQuietly() throws IOException {
      try {
        wait();
      } catch (InterruptedException e) {
        Thread.currentThread().interrupt();
        throw new java.io.InterruptedIOException();
      }
    }
  }
}
