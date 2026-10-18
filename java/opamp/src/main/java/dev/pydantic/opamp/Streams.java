// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.nio.ByteBuffer;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashMap;
import java.util.HashSet;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.RejectedExecutionException;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.ScheduledFuture;
import java.util.concurrent.TimeUnit;
import java.util.function.Consumer;
import java.util.function.Function;
import java.util.function.LongFunction;
import javax.net.ssl.SSLEngine;
import javax.net.ssl.SSLEngineResult;
import javax.net.ssl.SSLException;

/**
 * Core mode: the core speaks HTTP/1.1, WebSocket, CONNECT and SOCKS5 itself,
 * and this carries out only what needs the platform: TCP connections (blocking sockets, a reader
 * and a writer thread each), TLS on them (the JDK's {@link SSLEngine}, with the operator's CAs,
 * certificate and versions: layered, so TLS to an {@code https://} proxy carries TLS to the
 * server), name resolution (only for a SOCKS5 proxy with resolve-locally: otherwise the proxy
 * resolves), the proxy for a URL ({@link CoreProxy}), and the files of downloads. It decides
 * nothing: every byte it writes, the core wrote, and the core keeps every time (it closes a
 * stream it gives up on, and cancels a download's file). As node/opamp/src/streams.ts and
 * python/src/pydantic_opamp_client/_streams.py do.
 *
 * <p>Everything about a stream is decided on the client's thread (the loop): effects are carried
 * out there, and what the threads doing the I/O see (bytes read, a connection made, a handshake
 * done, a failure) is posted there and reported only if its stream is still the core's. One
 * registry ({@link InFlight}, the shape of {@link dev.pydantic.opamp.InFlight} and of
 * Python's {@code _InFlight}) owns everything under way: each stream (its connection, and the
 * attempt connecting it), each name being resolved, each file's writes, each linger timer. A
 * close, from the core or at shutdown, ends what it names at once; a connection or a TLS
 * handshake that completes after its stream was closed is aborted as it arrives, and reported to
 * no one; nothing is reported after a stream's close, or after shutdown.
 *
 * <p>Download files are written by a thread of their own, one write at a time per file, in the
 * order the bytes came, made new (never one left behind), hashed as written, through the
 * client's {@link Disk} (journaled into the core tap). Each download ends here once, answering
 * the shell's download_done (synced, then downloaded; or download_failed if a write failed: what
 * came after is dropped, and the file deleted), or not at all when the core discards or cancels
 * it first: then the file goes once the write in flight is over, and nothing more is said.
 *
 * <p>Backpressure, both ways. Socket to file: once more than {@link #HIGH_WATER} bytes wait for a
 * file's writes, the stream that brought the last of them stops reading (its spliced bytes, or
 * the data the core turned into download_data just then) until they are down to {@link
 * #LOW_WATER}; nothing is dropped or reordered: the kernel's window stops the server. Core to
 * socket: while more than {@link #WRITE_HIGH} bytes written to a stream wait for the peer, the
 * stream reads nothing more (a peer that doesn't read hears nothing more to answer), until they
 * are down to {@link #WRITE_LOW}; and a write that finds more than {@link #MAX_UNSENT} still
 * waiting fails the stream (stream_failed: the core reconnects) rather than buffering without
 * end. And the loop itself: a stream's reader hands the loop one chunk at a time, and reads the
 * next once the loop has taken it.
 */
final class Streams {
  /** How long a closed stream has to flush what was written before it is aborted. */
  static final long LINGER_MS = 2_000;
  /** Bytes waiting for a file's writes beyond which the stream they come from stops reading... */
  static final int HIGH_WATER = 4 << 20;
  /** ...until they are down to this. */
  static final int LOW_WATER = 1 << 20;
  /** Bytes written to a stream and not yet taken by the peer beyond which it stops reading... */
  static final int WRITE_HIGH = 64 << 10;
  /** ...until they are down to this. */
  static final int WRITE_LOW = 16 << 10;
  /** Bytes written to a stream and not yet taken by the peer beyond which the next write fails it. */
  static final long MAX_UNSENT = 16 << 20;
  /** The most bytes one read takes, and one file write. */
  static final int CHUNK = 64 << 10;

  /** A connection: a TCP socket's streams (the platform's, or a test's in memory). */
  interface Conn {
    InputStream input() throws IOException;

    OutputStream output() throws IOException;

    /** Close it once what was written has gone (a FIN). */
    void close() throws IOException;

    /** Close it at once (a reset): what was written no longer matters. Never throws. */
    void abort();
  }

  /**
   * What core mode does its network work with: the JDK's sockets and name resolution ({@link
   * #SOCKETS}). A test replaces it to run over streams in memory and choose when each step
   * completes (Fray: StreamsFrayTest, ShellFrayTest in core mode).
   */
  interface Platform {
    /**
     * A TCP connection to {@code host} (a name, or an address) and {@code port}, with TCP_NODELAY.
     * Blocking; {@code attempt}'s abort, from another thread, ends it at once.
     */
    Conn connect(String host, int port, Attempt attempt) throws IOException;

    /** The addresses {@code host} resolves to, the preferred first. Blocking. */
    List<String> resolve(String host) throws IOException;

    /** Run {@code r} on a thread of its own. */
    default void start(String name, Runnable r) {
      Thread t = new Thread(r, name);
      t.setDaemon(true);
      t.start();
    }
  }

  /** The JDK's: blocking sockets, InetAddress for names. */
  static final Platform SOCKETS = new Platform() {
    @Override
    public Conn connect(String host, int port, Attempt attempt) throws IOException {
      IOException last = null;
      for (InetAddress a : InetAddress.getAllByName(host)) {
        Socket s = new Socket();
        if (!attempt.onAbort(() -> abortQuietly(s))) {
          throw new IOException("closed while connecting");
        }
        try {
          s.setTcpNoDelay(true);
          s.connect(new InetSocketAddress(a, port));
          return new SocketConn(s);
        } catch (IOException e) {
          abortQuietly(s);
          last = e;
        }
      }
      throw last != null ? last : new IOException("no address for " + host);
    }

    @Override
    public List<String> resolve(String host) throws IOException {
      List<String> out = new ArrayList<>();
      for (InetAddress a : InetAddress.getAllByName(host)) {
        String s = a.getHostAddress();
        int zone = s.indexOf('%');
        s = zone < 0 ? s : s.substring(0, zone);
        if (!out.contains(s)) {
          out.add(s);
        }
      }
      return out;
    }
  };

  /**
   * TLS for core mode's streams, from the operator's settings ({@code tls}; null: the system's
   * CAs, TLS 1.2 and 1.3), as platform mode's {@link Transport} takes them: a client engine for
   * a host, which verifies the certificate for it (its name, or its address when it is an IP
   * literal: no SNI then) with the JDK's HTTPS rules. The context is built here, once: settings
   * that can't work fail here.
   */
  static Function<String, SSLEngine> engines(Tls tls) {
    javax.net.ssl.SSLContext ctx;
    String[] protocols;
    if (tls != null) {
      ctx = tls.context();
      protocols = tls.protocols();
    } else {
      try {
        ctx = javax.net.ssl.SSLContext.getDefault();
      } catch (NoSuchAlgorithmException e) {
        throw new IllegalStateException(e);
      }
      protocols = Tls.builder().build().protocols();
    }
    return host -> {
      String h = host.startsWith("[") && host.endsWith("]") ? host.substring(1, host.length() - 1) : host;
      SSLEngine e = ctx.createSSLEngine(h, -1);
      e.setUseClientMode(true);
      javax.net.ssl.SSLParameters p = e.getSSLParameters();
      p.setProtocols(protocols);
      p.setEndpointIdentificationAlgorithm("HTTPS");
      List<javax.net.ssl.SNIServerName> sni = List.of();
      if (!CoreProxy.Ip.literal(h)) {
        try {
          sni = List.of(new javax.net.ssl.SNIHostName(h));
        } catch (IllegalArgumentException notAHostName) {
          // a name SNI can't carry: verified all the same
        }
      }
      p.setServerNames(sni);
      e.setSSLParameters(p);
      return e;
    };
  }

  /** A socket as a {@link Conn}. */
  static final class SocketConn implements Conn {
    private final Socket s;

    SocketConn(Socket s) {
      this.s = s;
    }

    @Override
    public InputStream input() throws IOException {
      return s.getInputStream();
    }

    @Override
    public OutputStream output() throws IOException {
      return s.getOutputStream();
    }

    @Override
    public void close() throws IOException {
      s.close();
    }

    @Override
    public void abort() {
      abortQuietly(s);
    }
  }

  private static void abortQuietly(Socket s) {
    try {
      s.setSoLinger(true, 0);
    } catch (IOException e) {
      // closed already
    }
    try {
      s.close();
    } catch (IOException e) {
      // it is going anyway
    }
  }

  /**
   * An attempt to connect: its abort, once the platform has said how (the socket it is
   * connecting), runs at once if the attempt was aborted already. Thread-safe.
   */
  static final class Attempt {
    private Runnable abort;
    private boolean aborted;

    /** How to abort the attempt; false (having run it) if it was aborted already. */
    boolean onAbort(Runnable r) {
      synchronized (this) {
        if (!aborted) {
          abort = r;
          return true;
        }
      }
      r.run();
      return false;
    }

    void abort() {
      Runnable r;
      synchronized (this) {
        aborted = true;
        r = abort;
        abort = null;
      }
      if (r != null) {
        r.run();
      }
    }

    synchronized boolean aborted() {
      return aborted;
    }
  }

  private final ScheduledExecutorService loop;
  private final Consumer<CoreProto.Input> deliver;
  private final Platform platform;
  private final Function<String, SSLEngine> tls;
  private final Function<String, CoreProto.ProxyAnswer.Builder> proxies;
  private final Disk disk;
  private final Path stateFile;
  private final LongFunction<Path> downloadPath;
  final InFlight in = new InFlight();
  /** Downloads' files not yet answered, by download id (the loop and their threads). */
  private final Map<Long, DownloadFile> files = new ConcurrentHashMap<>();
  /** The downloads whose file was made, answered and not yet deleted (a discard deletes it). */
  private final Set<Long> answered = ConcurrentHashMap.newKeySet();
  /**
   * The downloads whose answer (downloaded, download_failed) is made and on its way to the loop:
   * a cancel or a discard the loop carries out first takes it from here, and the answer is
   * dropped (nothing about a file follows its cancel).
   */
  private final Set<Long> telling = ConcurrentHashMap.newKeySet();
  /** The stream whose bytes the core is being given: the download_data it makes of them came from it. Loop only. */
  private Stream reading;

  /**
   * Core mode's executor. {@code loop}: the client's thread, where effects are carried out and
   * where {@code deliver} feeds the core; {@code tls}: a client engine for a host, with the
   * operator's settings; {@code proxies}: the answer to proxy_query for a URL.
   */
  Streams(ScheduledExecutorService loop, Consumer<CoreProto.Input> deliver, Platform platform,
      Function<String, SSLEngine> tls, Function<String, CoreProto.ProxyAnswer.Builder> proxies, Disk disk,
      Path stateFile, LongFunction<Path> downloadPath) {
    this.loop = loop;
    this.deliver = deliver;
    this.platform = platform;
    this.tls = tls;
    this.proxies = proxies;
    this.disk = disk;
    this.stateFile = stateFile;
    this.downloadPath = downloadPath;
    // The loop's scheduler runs the linger timers (cancelled at shutdown).
    in.timers = loop;
  }

  /**
   * Carry out {@code e} if it is one of core mode's (on the loop): false if it isn't, or if
   * another must carry it out too (a cancel names a save as well as a download's file; a discard
   * of a download this didn't write is the platform executor's).
   */
  boolean run(CoreProto.Effect e) {
    switch (e.getKindCase()) {
      case PROXY_QUERY -> {
        CoreProto.Url q = e.getProxyQuery();
        CoreProto.ProxyAnswer.Builder a;
        try {
          a = proxies.apply(q.getUrl());
        } catch (RuntimeException ex) {
          // An answer the core can't use fails the connection; it never goes direct.
          a = CoreProto.ProxyAnswer.newBuilder().setProxy("invalid://" + ex.getMessage());
        }
        report(CoreProto.Input.newBuilder().setProxyAnswer(a.setId(q.getId())).build());
      }
      case STREAM_RESOLVE -> resolve(e.getStreamResolve().getId(), e.getStreamResolve().getHost());
      case STREAM_OPEN -> open(e.getStreamOpen().getId(), e.getStreamOpen().getHost(), e.getStreamOpen().getPort());
      case STREAM_START_TLS -> startTls(e.getStreamStartTls().getId(), e.getStreamStartTls().getHost());
      case STREAM_WRITE -> write(e.getStreamWrite().getId(), e.getStreamWrite().getData().toByteArray());
      case STREAM_CLOSE -> {
        long id = e.getStreamClose().getId();
        Resolution r = in.resolving.remove(id);
        if (r != null) {
          r.cancelled = true;
        }
        // A failure of it still on its way to the core goes nowhere.
        in.failing.remove(id);
        Stream s = in.streams.get(id);
        if (s != null) {
          in.close(s, e.getStreamClose().getAbort());
        }
      }
      case STREAM_SPLICE -> {
        Stream s = in.streams.get(e.getStreamSplice().getId());
        if (s != null) {
          s.splice = new long[] {e.getStreamSplice().getDownload(), e.getStreamSplice().getBytes()};
        }
      }
      case DOWNLOAD_DATA -> append(e.getDownloadData().getId(), e.getDownloadData().getData().toByteArray(), reading);
      case DOWNLOAD_DONE -> {
        DownloadFile f = file(e.getDownloadDone().getId());
        synchronized (f) {
          f.done = true;
        }
        pump(f);
      }
      // A download's file the core is done with, or gave up on (cancel: its stream is closed
      // already). A save's cancel is the platform executor's too.
      case DISCARD -> {
        return forget(e.getDiscard().getId());
      }
      case CANCEL -> {
        forget(e.getCancel().getId());
        return false;
      }
      default -> {
        return false;
      }
    }
    return true;
  }

  /** Download {@code id}'s file goes (once the write in flight is over): whether it was this one's. */
  private boolean forget(long id) {
    DownloadFile f = files.get(id);
    if (f != null) {
      synchronized (f) {
        f.gone = true;
      }
    }
    // From here its thread answers nothing; an answer it made before, still on its way to the
    // loop, is dropped.
    boolean told = telling.remove(id);
    if (f != null) {
      // (If it was answered as this looked, the file it made is deleted with the rest.)
      answered.remove(id);
      pump(f);
      return true;
    }
    if (answered.remove(id)) {
      // Answered already: only its file is left.
      if (stateFile != null && !in.closed) {
        Path path = downloadPath.apply(id);
        in.task(platform, "opamp-download-" + id + "-remove", () -> {
          try {
            disk.remove(path);
          } catch (IOException ex) {
            // it goes at the next start
          }
        });
      }
      return true;
    }
    return told;
  }

  /**
   * Drop every stream and resolution, and every file not complete (once its write in flight is
   * over), on the loop. The future completes when the file work is over (the caller bounds the
   * wait); nothing is reported from now on.
   */
  CompletableFuture<Void> shutdown() {
    in.stopAll();
    for (DownloadFile f : files.values()) {
      synchronized (f) {
        f.gone = true;
      }
      pump(f);
    }
    return in.tasks();
  }

  /** Bytes waiting for files' writes, all of them (for tests). */
  long unwritten() {
    long n = 0;
    for (DownloadFile f : files.values()) {
      synchronized (f) {
        n += f.unwritten;
      }
    }
    return n;
  }

  // ---------------------------------------------------------------------------------------------
  // The loop's side.

  /** Run {@code r} on the loop; false if the loop has ended. */
  private boolean post(Runnable r) {
    try {
      loop.execute(r);
      return true;
    } catch (RejectedExecutionException e) {
      return false;
    }
  }

  /** Feed {@code i} to the core from the loop's next turn (never from within an effect). */
  private void report(CoreProto.Input i) {
    post(() -> {
      if (!in.closed) {
        deliver.accept(i);
      }
    });
  }

  private boolean mine(Stream s) {
    return in.streams.get(s.id) == s;
  }

  /**
   * Give {@code s} up from within an effect: its connection goes at once, and the core hears of
   * it from the loop's next turn (never from within an effect), unless it has closed the stream
   * by then: nothing about a stream follows its close.
   */
  private void fail(Stream s, String detail) {
    in.close(s, true);
    in.failing.put(s.id, s);
    post(() -> {
      if (in.failing.remove(s.id, s) && !in.closed) {
        deliver.accept(failed(s.id, detail));
      }
    });
  }

  private static CoreProto.Input failed(long id, String detail) {
    return CoreProto.Input.newBuilder().setStreamFailed(CoreProto.Failed.newBuilder().setId(id).setDetail(detail)).build();
  }

  private static CoreProto.Input id(long id, java.util.function.BiFunction<CoreProto.Input.Builder, CoreProto.Id, CoreProto.Input.Builder> set) {
    return set.apply(CoreProto.Input.newBuilder(), CoreProto.Id.newBuilder().setId(id).build()).build();
  }

  static String detail(Throwable e, String otherwise) {
    if (e == null) {
      return otherwise;
    }
    return e.getMessage() == null || e.getMessage().isEmpty() ? e.getClass().getSimpleName() : e.getMessage();
  }

  private void resolve(long id, String host) {
    if (in.closed) {
      return;
    }
    Resolution r = new Resolution();
    in.resolving.put(id, r);
    platform.start("opamp-stream-" + id + "-resolve", () -> {
      List<String> addresses = null;
      String error = null;
      try {
        addresses = platform.resolve(host);
        if (addresses.isEmpty()) {
          error = "no address for " + host;
        }
      } catch (IOException | RuntimeException e) {
        error = detail(e, "no address");
      }
      List<String> got = addresses;
      String err = error;
      post(() -> {
        // Closed (or replaced) meanwhile: nothing.
        if (in.resolving.get(id) != r || r.cancelled) {
          return;
        }
        in.resolving.remove(id);
        deliver.accept(err != null ? failed(id, err)
            : CoreProto.Input.newBuilder().setStreamResolved(
                CoreProto.StreamResolved.newBuilder().setId(id).addAllAddresses(got)).build());
      });
    });
  }

  private void open(long id, String host, int port) {
    if (in.closed) {
      return;
    }
    Stream old = in.streams.get(id);
    if (old != null) {
      in.close(old, true);
    }
    in.failing.remove(id);
    Stream s = new Stream(id);
    in.streams.put(id, s);
    platform.start("opamp-stream-" + id + "-connect", () -> {
      Conn c;
      try {
        c = platform.connect(host, port, s.attempt);
      } catch (IOException | RuntimeException e) {
        post(() -> {
          if (mine(s)) {
            in.close(s, true);
            deliver.accept(failed(id, detail(e, "connecting failed")));
          }
        });
        return;
      }
      if (!post(() -> opened(s, c))) {
        c.abort();
      }
    });
  }

  /** Stream {@code s} connected: tell the core, unless it was closed meanwhile (then it goes at once). */
  private void opened(Stream s, Conn c) {
    if (!mine(s) || s.attempt.aborted()) {
      c.abort();
      return;
    }
    s.conn = c;
    s.pending = false;
    platform.start("opamp-stream-" + s.id + "-read", () -> read(s, c));
    platform.start("opamp-stream-" + s.id + "-write", () -> writeOut(s, c));
    List<byte[]> unsent = new ArrayList<>(s.unsent);
    s.unsent.clear();
    for (byte[] d : unsent) {
      write(s.id, d);
    }
    if (mine(s)) {
      deliver.accept(id(s.id, CoreProto.Input.Builder::setStreamOpened));
    }
  }

  private void startTls(long id, String host) {
    Stream s = in.streams.get(id);
    if (s == null || s.conn == null || s.pending) {
      return;
    }
    // From now on the bytes are TLS's until the handshake is done: the core hears of it first.
    s.pending = true;
    try {
      SSLEngine engine = tls.apply(host);
      synchronized (s) {
        s.startTls(engine);
      }
    } catch (SSLException | RuntimeException e) {
      fail(s, detail(e, "the TLS handshake failed"));
    }
  }

  private void write(long id, byte[] data) {
    Stream s = in.streams.get(id);
    if (s == null) {
      return;
    }
    if (s.conn == null || s.pending) {
      s.unsent.add(data); // the core waits for the stream to be ready: kept, in order
      return;
    }
    long unsent;
    synchronized (s) {
      unsent = s.queued;
    }
    if (unsent > MAX_UNSENT) {
      // The peer has stopped reading: give up on it rather than buffer without end.
      fail(s, "the peer has not taken the last " + unsent + " bytes written to it");
      return;
    }
    try {
      synchronized (s) {
        s.write(data);
      }
    } catch (SSLException e) {
      fail(s, detail(e, "TLS failed"));
    }
  }

  /** What the reader of {@code s} hands the loop: one chunk, in order. */
  private record Chunk(boolean ready, byte[] data, boolean eof) {}

  /** A chunk arrived on {@code s} (its reader waits until this has taken it). */
  private void arrived(Stream s, Chunk ch) {
    try {
      if (!mine(s)) {
        return;
      }
      if (ch.ready()) {
        // TLS is up: what was written meanwhile goes now, then the core hears of it.
        s.pending = false;
        List<byte[]> unsent = new ArrayList<>(s.unsent);
        s.unsent.clear();
        for (byte[] d : unsent) {
          write(s.id, d);
        }
        if (!mine(s)) {
          return;
        }
        deliver.accept(id(s.id, CoreProto.Input.Builder::setStreamTlsReady));
      }
      if (ch.data().length > 0 && mine(s)) {
        data(s, ch.data());
      }
      if (ch.eof() && mine(s)) {
        deliver.accept(id(s.id, CoreProto.Input.Builder::setStreamEof));
      }
    } finally {
      synchronized (s) {
        s.inbox = false;
        s.notifyAll();
      }
    }
  }

  /** Bytes read from {@code s}: its splice's to a file, the rest to the core. */
  private void data(Stream s, byte[] data) {
    int off = 0;
    // Body bytes: straight to the file, past the core. Hearing a splice done, the core may ask for
    // the next in that step: what it asks in answer to a stream's input applies to the very next
    // bytes, so what is left of this chunk is then that splice's, not the core's.
    while (s.splice != null) {
      long[] sp = s.splice;
      int n = (int) Math.min(sp[1] < 0 ? Long.MAX_VALUE : sp[1], data.length - off);
      append(sp[0], Arrays.copyOfRange(data, off, off + n), s);
      sp[1] -= n;
      off += n;
      if (sp[1] == 0) {
        s.splice = null;
        from(s, id(s.id, CoreProto.Input.Builder::setStreamSpliced));
      }
      // (Hearing the splice done, the shell may have closed the stream: nothing more.)
      if (off == data.length || !mine(s)) {
        return;
      }
      if (n == 0) {
        // A splice of no bytes (the real core asks for none) took none: the rest is the core's,
        // whatever it asked on hearing that one done (a core that answers each with another
        // doesn't hold the loop).
        break;
      }
    }
    from(s, CoreProto.Input.newBuilder().setStreamData(CoreProto.WsMessage.newBuilder()
        .setId(s.id).setData(ByteString.copyFrom(data, off, data.length - off))).build());
  }

  /** Deliver {@code i}, what {@code s} read: the download_data the core makes of it came from {@code s}. */
  private void from(Stream s, CoreProto.Input i) {
    reading = s;
    try {
      deliver.accept(i);
    } finally {
      reading = null;
    }
  }

  /**
   * Reading or writing {@code s} failed: its connection goes now (one the core closed, lingering
   * while what was written goes, has nowhere to send it: its linger is over only once the
   * connection is, or shutdown would no longer know of it), and the core hears of it, if it is
   * still the core's.
   */
  private void lost(Stream s, String detail) {
    in.lingered(s);
    s.abort();
    if (mine(s)) {
      in.close(s, true);
      deliver.accept(failed(s.id, detail));
    }
  }

  // ---------------------------------------------------------------------------------------------
  // The threads' side.

  /** {@code s}'s reader: a chunk at a time, while nothing holds it, handed to the loop. */
  private void read(Stream s, Conn c) {
    byte[] buf = new byte[CHUNK];
    try {
      InputStream input = c.input();
      while (true) {
        synchronized (s) {
          while (s.reading && (s.holds > 0 || s.inbox)) {
            s.wait();
          }
          if (!s.reading) {
            return;
          }
        }
        int n = input.read(buf);
        Chunk ch;
        synchronized (s) {
          if (!s.reading) {
            return;
          }
          ch = n < 0 ? new Chunk(false, new byte[0], true) : s.inbound(Arrays.copyOf(buf, n));
          if (ch == null) {
            continue; // nothing for the core yet (a TLS record not all here)
          }
          s.inbox = true;
          if (ch.eof()) {
            s.reading = false;
          }
        }
        if (!post(() -> arrived(s, ch)) || ch.eof()) {
          return;
        }
      }
    } catch (IOException e) {
      post(() -> lost(s, detail(e, "reading failed")));
    } catch (InterruptedException e) {
      Thread.currentThread().interrupt();
    }
  }

  /** {@code s}'s writer: what the core wrote, in order; once closed, a FIN after the last of it. */
  private void writeOut(Stream s, Conn c) {
    try {
      OutputStream output = c.output();
      while (true) {
        byte[] chunk;
        synchronized (s) {
          while (s.writing && s.out.isEmpty() && !s.finishing) {
            s.wait();
          }
          if (!s.writing) {
            return;
          }
          chunk = s.nextOut();
          if (chunk == null) {
            break; // closed, and all of it written
          }
        }
        output.write(chunk);
        output.flush();
        synchronized (s) {
          s.wentOut(chunk);
        }
      }
      c.close();
      synchronized (s) {
        s.writing = false;
        s.reading = false;
        s.notifyAll();
      }
      post(() -> in.lingered(s));
    } catch (IOException e) {
      c.abort();
      synchronized (s) {
        s.writing = false;
        s.reading = false;
        s.notifyAll();
      }
      post(() -> lost(s, detail(e, "writing failed")));
    } catch (InterruptedException e) {
      Thread.currentThread().interrupt();
    }
  }

  // ---------------------------------------------------------------------------------------------
  // Files.

  private DownloadFile file(long id) {
    return files.computeIfAbsent(id, k -> new DownloadFile(k, stateFile == null ? null : downloadPath.apply(k)));
  }

  /**
   * Queue {@code data} for the end of download {@code id}'s file (made at the first); {@code src}:
   * the stream it came from, held while too much waits.
   */
  private void append(long id, byte[] data, Stream src) {
    DownloadFile f = file(id);
    synchronized (f) {
      // After a failure (or a discard), the bytes go nowhere.
      if (f.error != null || f.gone || data.length == 0) {
        return;
      }
      f.queue.add(data);
      f.unwritten += data.length;
      if (src != null && f.unwritten > HIGH_WATER && f.paused.add(src)) {
        synchronized (src) {
          src.hold();
        }
      }
    }
    pump(f);
  }

  /** Carry {@code f} forward on a thread of its own, unless that is under way. */
  private void pump(DownloadFile f) {
    synchronized (f) {
      if (f.busy) {
        return;
      }
      f.busy = true;
    }
    in.task(platform, "opamp-download-" + f.id, () -> run(f));
  }

  private enum Act { CREATE, WRITE, FINISH, REMOVE }

  /**
   * Write what is queued; then, if it is done, sync and answer; if it is gone or failed, delete
   * it. Each decision under the file's lock, the I/O outside it; it stops (not busy) only when a
   * decision finds nothing to do, so whatever comes meanwhile is seen.
   */
  private void run(DownloadFile f) {
    while (true) {
      Act act;
      List<byte[]> batch = null;
      synchronized (f) {
        if (f.gone || f.error != null) {
          act = Act.REMOVE;
        } else if (!f.queue.isEmpty() || f.done) {
          if (f.handle == null && !f.made) {
            act = Act.CREATE;
          } else if (!f.queue.isEmpty()) {
            act = Act.WRITE;
            batch = new ArrayList<>();
            long n = 0;
            while (!f.queue.isEmpty() && (batch.isEmpty() || n + f.queue.peek().length <= CHUNK * 4L)) {
              byte[] b = f.queue.poll();
              batch.add(b);
              n += b.length;
            }
          } else {
            act = Act.FINISH;
          }
        } else {
          f.busy = false;
          return;
        }
      }
      try {
        switch (act) {
          case CREATE -> {
            if (f.path == null) {
              throw new IOException("no state file");
            }
            disk.mkdir(f.path.getParent());
            Disk.Handle h = disk.create(f.path, true);
            synchronized (f) {
              f.handle = h;
              f.made = true;
            }
          }
          case WRITE -> {
            long n = 0;
            try {
              for (byte[] b : batch) {
                f.handle.write(b, 0, b.length);
                f.hash.update(b);
                n += b.length;
              }
            } finally {
              List<Stream> unpause = List.of();
              synchronized (f) {
                f.unwritten -= sum(batch);
                if (f.unwritten <= LOW_WATER) {
                  unpause = new ArrayList<>(f.paused);
                  f.paused.clear();
                }
              }
              release(unpause);
            }
          }
          case FINISH -> {
            // Complete: durable before the core hears of it.
            Disk.Handle h = f.handle;
            try {
              h.sync();
            } finally {
              synchronized (f) {
                f.handle = null;
              }
              h.close();
            }
            byte[] sha;
            synchronized (f) {
              if (f.gone) {
                continue; // deleted next, and nothing said
              }
              answered.add(f.id);
              telling.add(f.id);
              files.remove(f.id, f);
              f.busy = false;
              sha = f.hash.digest();
            }
            tell(f.id, CoreProto.Input.newBuilder().setDownloaded(
                CoreProto.Downloaded.newBuilder().setId(f.id).setSha256(ByteString.copyFrom(sha))).build());
            return;
          }
          case REMOVE -> {
            if (remove(f)) {
              return;
            }
          }
        }
      } catch (IOException | RuntimeException e) {
        synchronized (f) {
          if (f.error == null) {
            f.error = "writing " + f.path + ": " + detail(e, "failed");
          }
        }
      }
    }
  }

  private static long sum(List<byte[]> bs) {
    long n = 0;
    for (byte[] b : bs) {
      n += b.length;
    }
    return n;
  }

  private static void release(List<Stream> streams) {
    for (Stream s : streams) {
      synchronized (s) {
        s.release();
      }
    }
  }

  /**
   * Close and delete a file that failed or is gone, then answer download_done if it came: whether
   * the file is over (false: it failed and download_done is still to come).
   */
  private boolean remove(DownloadFile f) {
    List<Stream> unpause;
    Disk.Handle h;
    boolean made;
    synchronized (f) {
      // Nothing more is written: the streams it held read on (into nothing, until they end).
      f.queue.clear();
      f.unwritten = 0;
      unpause = new ArrayList<>(f.paused);
      f.paused.clear();
      h = f.handle;
      f.handle = null;
      made = f.made;
    }
    release(unpause);
    if (h != null) {
      try {
        h.close();
      } catch (IOException e) {
        // deleted next
      }
    }
    if (made) {
      try {
        disk.remove(f.path);
      } catch (IOException e) {
        // it goes at the next start
      }
    }
    String error;
    synchronized (f) {
      if (f.gone) {
        files.remove(f.id, f);
        f.busy = false;
        return true;
      }
      if (!f.done) {
        f.busy = false;
        return true;
      }
      telling.add(f.id);
      files.remove(f.id, f);
      f.busy = false;
      error = f.error;
    }
    tell(f.id, CoreProto.Input.newBuilder().setDownloadFailed(
        CoreProto.Failed.newBuilder().setId(f.id).setDetail(error == null ? "" : error)).build());
    return true;
  }

  /**
   * Report {@code i}, the answer about download {@code id}'s file, from the loop: unless the core
   * cancelled or discarded the file before the loop got to it, or this has shut down.
   */
  private void tell(long id, CoreProto.Input i) {
    post(() -> {
      if (telling.remove(id) && !in.closed) {
        deliver.accept(i);
      }
    });
  }

  // ---------------------------------------------------------------------------------------------
  // State.

  /** A name being resolved. */
  static final class Resolution {
    /** Closed: its answer goes nowhere. Loop only. */
    boolean cancelled;
  }

  /**
   * Everything under way, by owner: streams by id, names being resolved by stream id, streams
   * given up whose failure the core has yet to hear, closed streams still flushing (each with its
   * linger timer), and file work. The loop's alone, but
   * for {@link #tasks}: {@code close} and {@link #stopAll} end what is here; nothing under way
   * is anywhere else.
   */
  static final class InFlight {
    final Map<Long, Stream> streams = new HashMap<>();
    final Map<Long, Resolution> resolving = new HashMap<>();
    /** Streams given up from within an effect, by id: their failure is on its way to the core. */
    final Map<Long, Stream> failing = new HashMap<>();
    /** Streams closed with what was written still going ({@link #LINGER_MS} at most), and their timers. */
    final Map<Stream, ScheduledFuture<?>> lingering = new HashMap<>();
    /** File work under way (and deletions): shutdown waits for it. */
    private final Map<CompletableFuture<Void>, String> work = new ConcurrentHashMap<>();
    volatile boolean closed;
    /** Where linger timers run: the loop. */
    ScheduledExecutorService timers;

    /** Run {@code r} on a thread of its own, as work shutdown waits for. */
    void task(Platform platform, String name, Runnable r) {
      CompletableFuture<Void> done = new CompletableFuture<>();
      work.put(done, name);
      try {
        platform.start(name, () -> {
          try {
            r.run();
          } finally {
            // Gone from the registry before anyone waiting for it hears it is over.
            work.remove(done);
            done.complete(null);
          }
        });
      } catch (RuntimeException e) {
        work.remove(done);
        done.complete(null);
        throw e;
      }
    }

    /** The file work under way now, over. */
    CompletableFuture<Void> tasks() {
      return CompletableFuture.allOf(work.keySet().toArray(new CompletableFuture[0]));
    }

    /** What is under way, if anything: empty if nothing (tests). */
    String left() {
      StringBuilder b = new StringBuilder();
      if (!streams.isEmpty()) {
        b.append("streams ").append(streams.keySet()).append(' ');
      }
      if (!resolving.isEmpty()) {
        b.append("resolving ").append(resolving.keySet()).append(' ');
      }
      if (!failing.isEmpty()) {
        b.append("failing ").append(failing.keySet()).append(' ');
      }
      if (!lingering.isEmpty()) {
        b.append("lingering ").append(lingering.keySet().stream().map(s -> s.id).toList()).append(' ');
      }
      // (The map's contents, not its size: a size can lag a removal that is over.)
      List<String> files = new ArrayList<>(work.values());
      if (!files.isEmpty()) {
        b.append("file work ").append(files);
      }
      return b.toString().strip();
    }

    /**
     * Stream {@code s} is no longer the core's: stop its attempt, and close its connection (abort:
     * at once; otherwise once what was written has gone, or {@link #LINGER_MS}).
     */
    void close(Stream s, boolean abort) {
      if (streams.get(s.id) == s) {
        streams.remove(s.id);
      }
      s.splice = null;
      s.unsent.clear();
      if (s.conn == null || abort || s.pending) {
        // Given up on (or never ready: nothing written has anywhere to go).
        s.abort();
        return;
      }
      synchronized (s) {
        s.reading = false;
        if (s.writing) {
          try {
            s.closeTls();
          } catch (SSLException e) {
            s.writing = false;
          }
        }
        s.finishing = true;
        s.notifyAll();
        if (!s.writing) {
          s.abort();
          return;
        }
      }
      ScheduledFuture<?> timer = null;
      if (timers != null) {
        try {
          timer = timers.schedule(() -> {
            if (lingering.remove(s) != null) {
              s.abort();
            }
          }, LINGER_MS, TimeUnit.MILLISECONDS);
        } catch (RejectedExecutionException e) {
          s.abort();
          return;
        }
      }
      lingering.put(s, timer);
    }

    /** {@code s} has flushed what was written and closed (or failed): its linger is over. */
    void lingered(Stream s) {
      ScheduledFuture<?> t = lingering.remove(s);
      if (t != null) {
        t.cancel(false);
      }
    }

    /** Close every stream at once, stop every resolution and connect, fire no timer. */
    void stopAll() {
      closed = true;
      for (Stream s : new ArrayList<>(streams.values())) {
        close(s, true);
      }
      for (Resolution r : resolving.values()) {
        r.cancelled = true;
      }
      resolving.clear();
      failing.clear();
      for (Map.Entry<Stream, ScheduledFuture<?>> e : lingering.entrySet()) {
        if (e.getValue() != null) {
          e.getValue().cancel(false);
        }
        e.getKey().abort();
      }
      lingering.clear();
    }
  }

  /** A download's file. */
  static final class DownloadFile {
    final long id;
    final Path path;
    /** Bytes waiting for the next write, in order. */
    final ArrayDeque<byte[]> queue = new ArrayDeque<>();
    /** How many bytes are queued or being written. */
    long unwritten;
    /** Streams not reading until the bytes waiting are down to LOW_WATER. */
    final Set<Stream> paused = new LinkedHashSet<>();
    /** Of the bytes written (touched only by the write in flight). */
    final MessageDigest hash;
    /** A write is running: only it touches the handle and the hash. */
    boolean busy;
    /** The file was made (by this: only then is it this one's to delete). */
    boolean made;
    /** download_done came: sync, close and answer once everything queued is written. */
    boolean done;
    /** Discarded or cancelled (or shut down): delete it, say nothing. */
    boolean gone;
    /** A write failed: the file is deleted, and download_done is answered with this. */
    String error;
    Disk.Handle handle;

    DownloadFile(long id, Path path) {
      this.id = id;
      this.path = path;
      try {
        this.hash = MessageDigest.getInstance("SHA-256");
      } catch (NoSuchAlgorithmException e) {
        throw new IllegalStateException(e);
      }
    }
  }

  /** A TLS layer of a stream: an engine, and the bytes from below it not yet unwrapped. */
  static final class TlsLayer {
    final SSLEngine engine;
    ByteBuffer netIn = ByteBuffer.allocate(0);
    /** Its handshake is done... */
    boolean finished;
    /** ...and the core was told. */
    boolean told;
    /** The peer's close_notify came. */
    boolean closed;

    TlsLayer(SSLEngine engine) {
      this.engine = engine;
    }
  }

  private static final ByteBuffer EMPTY = ByteBuffer.allocate(0);

  /**
   * One stream: its connection, and what the loop and its two threads share (under its lock).
   * Until the core has heard it is ready (opened, or TLS up) what the core writes waits in {@code
   * unsent}.
   */
  static final class Stream {
    final long id;
    final Attempt attempt = new Attempt();
    // The loop's.
    Conn conn;
    /** Connecting, or in a TLS handshake. */
    boolean pending = true;
    /** Written while pending: sent once it is ready. */
    final List<byte[]> unsent = new ArrayList<>();
    /** Its next bytes go to a download's file: which download, and how many are left. */
    long[] splice;
    // Under its lock.
    /** Bytes for the socket, in order (the first being written). */
    final ArrayDeque<byte[]> out = new ArrayDeque<>();
    /** How many bytes are in {@link #out}. */
    long queued;
    /** Read on (false once closed, or ended). */
    boolean reading = true;
    /** Write on (false once aborted, or failed). */
    boolean writing = true;
    /** Closed: the FIN goes once {@link #out} is written. */
    boolean finishing;
    /** Why it is not reading: how many files it waits for, and a full write buffer. */
    int holds;
    boolean blocked;
    /** A chunk read is with the loop: the next read waits for it. */
    boolean inbox;
    /** Its TLS layers, the socket's first. */
    final List<TlsLayer> layers = new ArrayList<>();

    Stream(long id) {
      this.id = id;
    }

    /** Stop reading (for one more reason). Under the lock. */
    void hold() {
      holds++;
    }

    /** One reason fewer not to read: read again if it was the last. Under the lock. */
    void release() {
      if (holds == 0) {
        return;
      }
      holds--;
      if (holds == 0) {
        notifyAll();
      }
    }

    /** Close it at once: its attempt, its connection, its threads. */
    void abort() {
      attempt.abort();
      synchronized (this) {
        reading = false;
        writing = false;
        notifyAll();
      }
      Conn c = conn;
      if (c != null) {
        c.abort();
      }
    }

    /**
     * The core's bytes, through the TLS layers to the socket: past {@link #WRITE_HIGH} waiting for
     * the peer, it reads nothing more until they are down to {@link #WRITE_LOW}. Under the lock.
     */
    void write(byte[] data) throws SSLException {
      send(layers.size() - 1, data);
      if (!blocked && queued > WRITE_HIGH) {
        blocked = true;
        hold();
      }
    }

    /** The next bytes for the socket (they stay first until {@link #wentOut}), or null. Under the lock. */
    byte[] nextOut() {
      return out.peek();
    }

    /** {@code chunk}, the first, has gone to the socket. Under the lock. */
    void wentOut(byte[] chunk) {
      out.poll();
      queued -= chunk.length;
      if (blocked && queued <= WRITE_LOW) {
        blocked = false;
        release();
      }
    }

    /** Bytes for the socket. Under the lock. */
    private void enqueue(byte[] raw) {
      if (raw.length == 0) {
        return;
      }
      out.add(raw);
      queued += raw.length;
      notifyAll();
    }

    /** {@code plain}, through layer {@code from} and those below it, to the socket. Under the lock. */
    void send(int from, byte[] plain) throws SSLException {
      byte[] b = plain;
      for (int k = from; k >= 0; k--) {
        b = wrap(k, b);
      }
      enqueue(b);
    }

    /** Start TLS over what is there (TCP, or TLS to a proxy). Under the lock. */
    void startTls(SSLEngine engine) throws SSLException {
      layers.add(new TlsLayer(engine));
      engine.beginHandshake();
      handshake(layers.size() - 1, engine.getHandshakeStatus());
    }

    /** Each layer's close_notify, the innermost first. Under the lock. */
    void closeTls() throws SSLException {
      for (int k = layers.size() - 1; k >= 0; k--) {
        SSLEngine e = layers.get(k).engine;
        e.closeOutbound();
        handshake(k, e.getHandshakeStatus());
      }
    }

    /**
     * Bytes read from the socket, up through the layers: what the core is to hear of them, or
     * null for nothing yet. Under the lock.
     */
    Chunk inbound(byte[] raw) throws SSLException {
      byte[] data = raw;
      boolean eof = false;
      for (int i = 0; i < layers.size() && !eof; i++) {
        data = unwrap(i, data);
        eof = layers.get(i).closed;
      }
      boolean ready = false;
      if (!layers.isEmpty()) {
        TlsLayer top = layers.get(layers.size() - 1);
        if (top.finished && !top.told) {
          top.told = true;
          ready = true;
        }
      }
      if (data.length == 0 && !ready && !eof) {
        return null;
      }
      return new Chunk(ready, data, eof);
    }

    private byte[] unwrap(int i, byte[] data) throws SSLException {
      TlsLayer l = layers.get(i);
      if (l.closed) {
        return new byte[0];
      }
      ByteBuffer in = ByteBuffer.allocate(l.netIn.remaining() + data.length);
      in.put(l.netIn).put(data).flip();
      l.netIn = in;
      ByteArrayOutputStream app = new ByteArrayOutputStream();
      ByteBuffer dst = ByteBuffer.allocate(l.engine.getSession().getApplicationBufferSize());
      while (true) {
        SSLEngineResult r = l.engine.unwrap(l.netIn, dst);
        if (r.getHandshakeStatus() == SSLEngineResult.HandshakeStatus.FINISHED) {
          l.finished = true;
        }
        if (r.getStatus() == SSLEngineResult.Status.BUFFER_OVERFLOW) {
          ByteBuffer bigger = ByteBuffer.allocate(Math.max(dst.capacity() * 2, l.engine.getSession().getApplicationBufferSize()));
          dst.flip();
          bigger.put(dst);
          dst = bigger;
          continue;
        }
        dst.flip();
        app.write(dst.array(), dst.arrayOffset() + dst.position(), dst.remaining());
        dst.clear();
        if (r.getStatus() == SSLEngineResult.Status.CLOSED) {
          l.closed = true;
          handshake(i, l.engine.getHandshakeStatus()); // our close_notify in answer
          break;
        }
        handshake(i, r.getHandshakeStatus());
        if (r.getStatus() == SSLEngineResult.Status.BUFFER_UNDERFLOW) {
          break; // a record not all here yet
        }
        if (!l.netIn.hasRemaining() || (r.bytesConsumed() == 0 && r.bytesProduced() == 0
            && l.engine.getHandshakeStatus() != SSLEngineResult.HandshakeStatus.NEED_UNWRAP)) {
          break;
        }
      }
      if (!l.finished && l.engine.getHandshakeStatus() == SSLEngineResult.HandshakeStatus.NOT_HANDSHAKING
          && l.engine.getSession().isValid() && !l.engine.isOutboundDone()) {
        l.finished = true;
      }
      return app.toByteArray();
    }

    /** Carry layer {@code i}'s handshake forward as far as it goes without the peer. */
    private void handshake(int i, SSLEngineResult.HandshakeStatus hs) throws SSLException {
      TlsLayer l = layers.get(i);
      while (true) {
        switch (hs) {
          case NEED_TASK -> {
            for (Runnable t = l.engine.getDelegatedTask(); t != null; t = l.engine.getDelegatedTask()) {
              t.run();
            }
            hs = l.engine.getHandshakeStatus();
          }
          case NEED_WRAP -> {
            ByteBuffer dst = ByteBuffer.allocate(l.engine.getSession().getPacketBufferSize());
            SSLEngineResult r = l.engine.wrap(EMPTY, dst);
            if (r.getStatus() == SSLEngineResult.Status.BUFFER_OVERFLOW) {
              throw new SSLException("a TLS handshake record larger than the engine's packet size");
            }
            if (r.getHandshakeStatus() == SSLEngineResult.HandshakeStatus.FINISHED) {
              l.finished = true;
            }
            dst.flip();
            byte[] b = new byte[dst.remaining()];
            dst.get(b);
            if (b.length > 0) {
              send(i - 1, b);
            }
            hs = r.getHandshakeStatus();
            if (r.getStatus() == SSLEngineResult.Status.CLOSED && b.length == 0) {
              return;
            }
          }
          default -> {
            return;
          }
        }
      }
    }

    private byte[] wrap(int k, byte[] plain) throws SSLException {
      TlsLayer l = layers.get(k);
      ByteBuffer src = ByteBuffer.wrap(plain);
      ByteArrayOutputStream net = new ByteArrayOutputStream();
      ByteBuffer dst = ByteBuffer.allocate(l.engine.getSession().getPacketBufferSize());
      do {
        SSLEngineResult r = l.engine.wrap(src, dst);
        if (r.getStatus() == SSLEngineResult.Status.BUFFER_OVERFLOW) {
          dst = ByteBuffer.allocate(dst.capacity() * 2);
          continue;
        }
        if (r.getStatus() == SSLEngineResult.Status.CLOSED && src.hasRemaining()) {
          throw new SSLException("the TLS connection is closed");
        }
        if (r.getHandshakeStatus() == SSLEngineResult.HandshakeStatus.NEED_TASK) {
          for (Runnable t = l.engine.getDelegatedTask(); t != null; t = l.engine.getDelegatedTask()) {
            t.run();
          }
        }
        dst.flip();
        net.write(dst.array(), dst.arrayOffset() + dst.position(), dst.remaining());
        dst.clear();
        if (r.bytesConsumed() == 0 && r.bytesProduced() == 0 && src.hasRemaining()) {
          throw new SSLException("TLS took none of the bytes written (" + r + ")");
        }
      } while (src.hasRemaining());
      return net.toByteArray();
    }
  }
}
