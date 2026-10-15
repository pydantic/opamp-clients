// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertNotNull;

import java.io.BufferedInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.net.SocketTimeoutException;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.Base64;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;

/** A TCP server that hands each connection it accepts to a handler of the test's. */
final class Raw implements AutoCloseable {
  interface Handler {
    void handle(Socket s) throws Exception;
  }

  /** Connections a handler holds, for the test to watch. */
  static final LinkedBlockingQueue<Socket> HELD = new LinkedBlockingQueue<>();

  private final ServerSocket listener = new ServerSocket(0, 50, InetAddress.getByName("127.0.0.1"));
  final LinkedBlockingQueue<Socket> accepted = new LinkedBlockingQueue<>();

  Raw(Handler handler) throws IOException {
    HELD.clear();
    start(() -> {
      while (!listener.isClosed()) {
        try {
          Socket s = listener.accept();
          accepted.add(s);
          start(() -> {
            try {
              handler.handle(s);
            } catch (Exception e) {
              // the connection went
            }
          });
        } catch (IOException e) {
          return;
        }
      }
    });
  }

  String url(String scheme) {
    return scheme + "://127.0.0.1:" + listener.getLocalPort();
  }

  /** The connection a handler holds next. */
  Socket held() throws InterruptedException {
    Socket s = HELD.poll(20, TimeUnit.SECONDS);
    assertNotNull(s, "no connection held");
    return s;
  }

  @Override
  public void close() throws IOException {
    listener.close();
    for (Socket s : accepted) {
      s.close();
    }
  }

  /** Read the request and hold the connection, answering nothing. */
  static void hold(Socket s) throws IOException {
    InputStream in = new BufferedInputStream(s.getInputStream());
    TransportTest.Head.read(in);
    hold(s, in);
  }

  /** Hold the connection: the test watches it. */
  static void hold(Socket s, InputStream in) {
    HELD.add(s);
  }

  /** Complete the upgrade and hold the connection. */
  static void websocket(Socket s) throws IOException {
    InputStream in = new BufferedInputStream(s.getInputStream());
    upgrade(s.getOutputStream(), TransportTest.Head.read(in));
    HELD.add(s);
  }

  /**
   * Complete the upgrade and read frames, their opcodes to {@code frames}; answer our Close with
   * one of the server's if {@code answer}.
   */
  static void websocket(Socket s, LinkedBlockingQueue<Integer> frames, boolean answer) throws IOException {
    InputStream in = new BufferedInputStream(s.getInputStream());
    OutputStream out = s.getOutputStream();
    upgrade(out, TransportTest.Head.read(in));
    HELD.add(s);
    while (true) {
      int op = frame(in);
      frames.add(op);
      if (op == 0x8 && answer) {
        out.write(new byte[] {(byte) 0x88, 2, 0x03, (byte) 0xe8}); // Close, 1000
        out.flush();
        return;
      }
    }
  }

  static void upgrade(OutputStream out, TransportTest.Head h) throws IOException {
    String accept;
    try {
      accept = Base64.getEncoder().encodeToString(MessageDigest.getInstance("SHA-1")
          .digest((h.headers().get("sec-websocket-key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
              .getBytes(StandardCharsets.US_ASCII)));
    } catch (java.security.NoSuchAlgorithmException e) {
      throw new IllegalStateException(e);
    }
    out.write(("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
        + "Sec-WebSocket-Accept: " + accept + "\r\n\r\n").getBytes(StandardCharsets.US_ASCII));
    out.flush();
  }

  /** Read one (masked) frame from the client: its opcode. */
  static int frame(InputStream in) throws IOException {
    int b0 = in.read();
    int b1 = in.read();
    if (b0 < 0 || b1 < 0) {
      throw new IOException("gone");
    }
    long len = b1 & 0x7f;
    if (len == 126) {
      len = (in.read() << 8) | in.read();
    } else if (len == 127) {
      len = 0;
      for (int i = 0; i < 8; i++) {
        len = (len << 8) | in.read();
      }
    }
    in.readNBytes(4 + (int) len);
    return b0 & 0x0f;
  }

  /** Whether the client closes {@code s} within {@code ms} (what it sends meanwhile is read). */
  static boolean closedWithin(Socket s, long ms) throws IOException {
    long end = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(ms);
    InputStream in = s.getInputStream();
    byte[] buf = new byte[4096];
    while (System.nanoTime() < end) {
      s.setSoTimeout((int) Math.max(1, TimeUnit.NANOSECONDS.toMillis(end - System.nanoTime())));
      try {
        if (in.read(buf) < 0) {
          return true;
        }
      } catch (SocketTimeoutException e) {
        return false;
      } catch (IOException e) {
        return true; // reset
      }
    }
    return false;
  }

  static void await(CountDownLatch l) {
    try {
      l.await();
    } catch (InterruptedException e) {
      Thread.currentThread().interrupt();
    }
  }

  private static void start(Runnable r) {
    Thread t = new Thread(r);
    t.setDaemon(true);
    t.start();
  }
}
