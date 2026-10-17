// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.ByteString;
import com.sun.net.httpserver.HttpServer;
import dev.pydantic.opamp.core.CoreProto;
import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.http.HttpClient;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.security.MessageDigest;
import java.time.Duration;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.HashSet;
import java.util.HexFormat;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.SplittableRandom;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.regex.Matcher;
import java.util.regex.Pattern;
import opamp.proto.v1.Opamp;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.EnumSource;

/**
 * The client's file operations: each save and each download's file is journaled into the core
 * tap (kind 5, opamp-tap's file records) as it is done, and the recipe holds when the disk fails:
 * errors, a write cut short, a slow disk, a rename that lands but whose directory flush fails, a
 * downloads directory that can't be made, a discard that can't delete. Faults are injected through
 * the client's {@link Disk}, under the journal. OPAMP_PROP_RUNS sets the number of fault plans
 * (default 6); a failure prints its run's seed, and OPAMP_PROP_SEED replays that one run.
 */
class FilesTest {
  private static final byte[] UID = new byte[16];
  private static final byte[] PACKAGE = "the package, ".repeat(10_000).getBytes(java.nio.charset.StandardCharsets.US_ASCII);
  private static final String STATE = "state.bin";
  private static final String TMP = "state.bin.tmp";
  private static final String DOWNLOADS = "state.bin.downloads";

  static {
    java.util.Arrays.fill(UID, (byte) 0x17);
  }

  // ---------------------------------------------------------------------------------------------
  // The journal: a save's records and a download's, in the order the checker needs them.

  // In both modes: in Rust mode the download's file is Streams', through the same journaled Disk,
  // and the core hears the client's downloaded (the shell makes the response of it).
  @ParameterizedTest
  @EnumSource(OpampClient.Mode.class)
  void aSaveAndADownloadAreJournaled(OpampClient.Mode mode, @TempDir Path dir) throws Exception {
    Path state = dir.resolve(STATE);
    // Left by a run that crashed: a save's temporary file, and a download's file.
    Files.write(StateFile.tmp(state), new byte[] {9, 9});
    Files.createDirectories(Executor.downloadsDir(state));
    Files.write(Executor.downloadsDir(state).resolve("junk"), new byte[] {9});
    Path tap = dir.resolve("core.tap");
    try (Servers s = new Servers(1)) {
      List<Notification.InstallPackage> installs = new CopyOnWriteArrayList<>();
      List<byte[]> handed = new CopyOnWriteArrayList<>();
      CompletableFuture<OpampClient> handle = new CompletableFuture<>();
      OpampClient client = OpampClient.start(s.config(state, mode), OpampClient.Status.of(null), n -> {
        if (n instanceof Notification.InstallPackage ip) {
          try {
            handed.add(Files.readAllBytes(ip.path()));
          } catch (IOException e) {
            handed.add(new byte[0]);
          }
          installs.add(ip);
          handle.join().packageInstalled(ip.name(), ip.pkg().getHash().toByteArray(), null);
        }
      }, null, tap.toString(), d -> d);
      handle.complete(client);
      String tapPath = client.tapPath();
      LifecycleTest.waitFor(() -> {
        List<Event> ev = eventsQuietly(Path.of(tapPath));
        return !installs.isEmpty() && ev.stream().anyMatch(e -> e.in() != null && e.in().hasStored())
            && ev.stream().anyMatch(e -> e.file() != null && e.file().get("op").equals("remove")
                && e.file().get("path").startsWith(DOWNLOADS + "/"));
      }, 30_000);
      client.stop().get(20, TimeUnit.SECONDS);
      assertEquals(1, installs.size(), "the package is installed");
      assertArrayEquals(sha(PACKAGE), sha(handed.get(0)), "the file handed over is the one offered");

      List<Event> ev = events(Path.of(tapPath));
      keep(Path.of(tapPath), "journal-" + mode);
      // init in, init out, then the state file named, then the downloads of before cleared.
      assertEquals(List.of(1, 2, 5, 5), ev.subList(0, 4).stream().map(Event::kind).toList());
      assertEquals(Map.of("op", "state", "path", STATE), ev.get(2).file());
      assertEquals(Map.of("op", "clear", "path", DOWNLOADS), ev.get(3).file());

      // The first save: the leftover goes, then a new file, its data, flushed, renamed, the
      // directory flushed; only then stored.
      int storeAt = indexOf(ev, e -> e.out() != null && e.out().getEffectsList().stream().anyMatch(CoreProto.Effect::hasStore));
      CoreProto.Store store = ev.get(storeAt).out().getEffectsList().stream()
          .filter(CoreProto.Effect::hasStore).findFirst().orElseThrow().getStore();
      List<Integer> saveAt = new ArrayList<>();
      for (int i = storeAt + 1; i < ev.size() && saveAt.size() < 6; i++) {
        Map<String, String> f = ev.get(i).file();
        if (f != null && !f.getOrDefault("path", "").startsWith(DOWNLOADS)) {
          saveAt.add(i);
        }
      }
      String data = HexFormat.of().formatHex(store.getData().toByteArray());
      assertEquals(List.of(
          Map.of("op", "remove", "path", TMP),
          Map.of("op", "create", "path", TMP),
          Map.of("op", "write", "path", TMP, "data", data),
          Map.of("op", "sync", "path", TMP),
          Map.of("op", "rename", "from", TMP, "to", STATE),
          Map.of("op", "syncdir", "path", ".")), saveAt.stream().map(i -> ev.get(i).file()).toList());
      int stored = indexOf(ev, e -> e.in() != null && e.in().hasStored() && e.in().getStored().getId() == store.getId());
      assertTrue(stored > saveAt.get(5), "stored comes after the directory is flushed");

      // The download: its directory made, its file made new, written, flushed; then the response;
      // and once installed, discarded.
      String file = DOWNLOADS + "/" + installs.get(0).path().getFileName();
      List<Map<String, String>> download = ev.stream().map(Event::file)
          .filter(f -> f != null && f.getOrDefault("path", "").startsWith(DOWNLOADS) && !f.get("op").equals("clear")).toList();
      assertEquals(Map.of("op", "mkdir", "path", DOWNLOADS), download.get(0));
      assertEquals(Map.of("op", "create", "path", file), download.get(1));
      long written = download.stream().filter(f -> f.get("op").equals("write")).mapToLong(f -> Long.parseLong(f.get("len"))).sum();
      assertEquals(PACKAGE.length, written, "every byte written is journaled");
      assertEquals(List.of(Map.of("op", "sync", "path", file), Map.of("op", "remove", "path", file)),
          download.subList(download.size() - 2, download.size()));
      int synced = indexOf(ev, e -> Map.of("op", "sync", "path", file).equals(e.file()));
      int response = indexOf(ev, e -> e.in() != null && (e.in().hasDownloadResponse() || e.in().hasDownloaded()));
      assertTrue(response > synced, "the response comes after the file is flushed");
      assertEquals(List.of(), problems(ev));
      opampTap(Path.of(tapPath));
    }
  }

  // ---------------------------------------------------------------------------------------------
  // Faults.

  enum Fault {
    /** The operation fails (EIO), not done. */
    EIO,
    /** A write writes half, then fails (ENOSPC). */
    HALF_WRITE,
    /** The operation takes 0.5 to 2 s. */
    SLOW,
    /** The rename lands, then the directory's flush fails. */
    LANDS_BUT_FAILS,
    /** The downloads directory can't be made. */
    NO_DOWNLOADS_DIR,
    /** A download's file can't be deleted (a discard, say). */
    REMOVE_FAILS,
  }

  /** A save's operations, in order. */
  static final List<String> SAVE_OPS = List.of("remove", "create", "write", "sync", "rename", "syncdir");

  /**
   * A plan: {@code fault} on operation {@code op} of save {@code save} (the core may ask for fewer
   * saves: then none), {@code slowMs} for a slow one; the server offers {@code configs} configs,
   * each saved; {@code leftover}: a crashed run's temporary file is there at each start; {@code
   * stopEarly}: stop as soon as the fault is injected.
   */
  record Plan(Fault fault, int save, int op, long slowMs, int configs, boolean leftover, boolean stopEarly) {}

  static Plan plan(SplittableRandom r) {
    Fault f = Fault.values()[r.nextInt(Fault.values().length)];
    int configs = 1 + r.nextInt(4);
    int op = switch (f) {
      case HALF_WRITE -> 2;
      case LANDS_BUT_FAILS -> 5;
      default -> r.nextInt(SAVE_OPS.size());
    };
    return new Plan(f, r.nextInt(configs + 1), op, 500 + r.nextInt(1501), configs, r.nextBoolean(), r.nextInt(4) == 0);
  }

  @Test
  void savesAndDownloadsSurviveFaults(@TempDir Path root) throws Exception {
    String runsEnv = System.getenv("OPAMP_PROP_RUNS");
    String seedEnv = System.getenv("OPAMP_PROP_SEED");
    int runs = seedEnv != null && !seedEnv.isEmpty() ? 1
        : runsEnv == null || runsEnv.isEmpty() ? 6 : Integer.parseInt(runsEnv);
    long base = System.nanoTime();
    for (int i = 0; i < runs; i++) {
      long seed = seedEnv != null && !seedEnv.isEmpty() ? Long.parseLong(seedEnv) : base + i * 0x9E3779B97F4A7C15L;
      Plan plan = plan(new SplittableRandom(seed));
      try {
        check(plan, Files.createDirectory(root.resolve("run" + i)));
      } catch (Throwable e) {
        throw new AssertionError("run " + i + " of " + runs + " (replay: OPAMP_PROP_SEED=" + seed + "), " + plan
            + ": " + e.getMessage(), e);
      }
    }
  }

  // The rename landed, but the directory's flush failed: the rename may not be on the disk, so the
  // save isn't stored (store_failed), and nothing is said stored before it is durable.
  @Test
  void aRenameThatLandsButIsntFlushedIsNotStored(@TempDir Path dir) throws Exception {
    check(new Plan(Fault.LANDS_BUT_FAILS, 0, 5, 0, 2, false, false), dir);
  }

  // A slow rename (the disk takes 1.5 s) holds up neither heartbeats nor anything else.
  @Test
  void aSlowRenameHoldsUpNothing(@TempDir Path dir) throws Exception {
    check(new Plan(Fault.SLOW, 0, 4, 1500, 2, false, false), dir);
  }

  /** Run {@code plan} in {@code dir}, then a clean client on what it left. */
  static void check(Plan plan, Path dir) throws Exception {
    Path state = dir.resolve(STATE);
    if (plan.leftover()) {
      Files.write(StateFile.tmp(state), new byte[] {1, 2, 3});
    }
    try (Servers s = new Servers(plan.configs())) {
      List<Throwable> errors = new CopyOnWriteArrayList<>();
      List<Notification.InstallPackage> installs = new CopyOnWriteArrayList<>();
      FaultDisk[] fd = new FaultDisk[1];
      CompletableFuture<OpampClient> handle = new CompletableFuture<>();
      OpampClient a = OpampClient.start(s.config(state), OpampClient.Status.of(null), n -> {
        if (n instanceof Notification.Stopped st && st.error() != null) {
          errors.add(st.error());
        } else if (n instanceof Notification.InstallPackage ip) {
          installs.add(ip);
          handle.join().packageInstalled(ip.name(), ip.pkg().getHash().toByteArray(), null);
        }
      }, null, dir.resolve("a.tap").toString(), d -> fd[0] = new FaultDisk(d, plan, state));
      handle.complete(a);
      Path tapA = Path.of(a.tapPath());
      if (plan.stopEarly()) {
        fd[0].injected.await(15, TimeUnit.SECONDS);
      } else {
        LifecycleTest.waitFor(() -> s.served.get() >= plan.configs() + 4
            && (plan.fault() == Fault.NO_DOWNLOADS_DIR || !installs.isEmpty())
            && (plan.fault() != Fault.SLOW || !fd[0].fired() || fd[0].slowTo > 0)
            && settled(eventsQuietly(tapA)), 20_000);
      }
      long t0 = System.nanoTime();
      a.stop().get(30, TimeUnit.SECONDS);
      long stopMs = TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - t0);
      long bound = STOP_TIMEOUT_MS + (plan.fault() == Fault.SLOW ? plan.slowMs() : 0) + 4000;
      assertTrue(stopMs <= bound, "stop took " + stopMs + " ms, more than " + bound);
      assertEquals(List.of(), errors, "the client survives");

      List<Event> ev = events(tapA);
      keep(tapA, dir.getFileName() + "-a");
      System.out.println("FilesTest: " + plan + (fd[0].fired() ? ": injected" : ": not injected (fewer saves)")
          + ", stop took " + stopMs + " ms");
      assertEquals(List.of(), problems(ev), "the journal");
      answeredOnce(ev);
      if (fd[0].fired() && !plan.stopEarly()) {
        if (plan.fault() == Fault.SLOW) {
          long during = s.arrivals.stream().filter(t -> t > fd[0].slowFrom && t < fd[0].slowTo).count();
          assertTrue(during >= 2, "heartbeats go on while the disk is slow (" + (fd[0].slowTo - fd[0].slowFrom) / 1_000_000
              + " ms): " + during + " messages meanwhile");
        }
        long after = fd[0].firedAt;
        assertTrue(s.arrivals.stream().anyMatch(t -> t > after), "the client goes on after the fault");
      }
      opampTap(tapA);

      // What is on the disk now: what it held at start, or one of the saves, never older than the
      // last one said stored.
      StateFile.settled(state);
      byte[] now = StateFile.load(state);
      List<String> saves = stores(ev);
      int at = saves.lastIndexOf(HexFormat.of().formatHex(now));
      assertTrue(at >= 0 || now.length == 0, "the state file holds " + now.length + " bytes that are no save");
      assertTrue(at >= lastStored(ev, saves), "the state file is older than the last save said stored");

      // The next client starts from it, and saves, whatever was left behind.
      if (plan.leftover()) {
        Files.write(StateFile.tmp(state), new byte[] {4, 5, 6});
      }
      s.configs.set(plan.configs() + 1);
      OpampClient b = OpampClient.start(s.config(state), OpampClient.Status.of(null), n -> {
        if (n instanceof Notification.Stopped st && st.error() != null) {
          errors.add(st.error());
        }
      }, null, dir.resolve("b.tap").toString(), d -> d);
      Path tapB = Path.of(b.tapPath());
      LifecycleTest.waitFor(() -> eventsQuietly(tapB).stream().anyMatch(e -> e.in() != null && e.in().hasStored()), 20_000);
      b.stop().get(30, TimeUnit.SECONDS);
      List<Event> evB = events(tapB);
      keep(tapB, dir.getFileName() + "-b");
      CoreProto.Config started = CoreProto.Config.parseFrom(firstBytes(tapB));
      assertArrayEquals(now, started.getSavedState().toByteArray(), "the next client starts from the file");
      assertTrue(evB.stream().anyMatch(e -> e.in() != null && e.in().hasStored()), "the next client saves");
      assertFalse(evB.stream().anyMatch(e -> e.in() != null && e.in().hasStoreFailed()),
          "no save of the next client fails: " + evB.stream().filter(e -> e.in() != null && e.in().hasStoreFailed()).toList());
      assertEquals(List.of(), problems(evB), "the next client's journal");
      assertEquals(List.of(), errors, "the next client survives");
      opampTap(tapB);
      StateFile.settled(state);
      List<String> savesB = stores(evB);
      assertTrue(savesB.lastIndexOf(HexFormat.of().formatHex(StateFile.load(state))) >= lastStored(evB, savesB),
          "the state file holds the next client's last save stored, or a later one");
    }
  }

  static final long STOP_TIMEOUT_MS = 1000;

  /** A disk that injects a plan's fault. Saves run one at a time, on one thread. */
  static final class FaultDisk implements Disk {
    private final Disk inner;
    private final Plan plan;
    private final Path tmp;
    private final Path downloads;
    /** Saves begun (each begins with the removal of a leftover), and where the current one is. */
    private int saves = -1;
    private boolean inSave;
    private volatile boolean fired;
    volatile long firedAt;
    volatile long slowFrom;
    volatile long slowTo;
    final CountDownLatch injected = new CountDownLatch(1);

    FaultDisk(Disk inner, Plan plan, Path state) {
      this.inner = inner;
      this.plan = plan;
      this.tmp = StateFile.tmp(state).toAbsolutePath();
      this.downloads = Executor.downloadsDir(state).toAbsolutePath();
    }

    boolean fired() {
      return fired;
    }

    private boolean hit(int op) {
      boolean saveFault = plan.fault() != Fault.NO_DOWNLOADS_DIR && plan.fault() != Fault.REMOVE_FAILS;
      if (fired || !saveFault || saves != plan.save() || op != plan.op()) {
        return false;
      }
      fired = true;
      firedAt = System.nanoTime();
      return true;
    }

    /** The fault, if this is its operation (before it is done); true: slow, it is done now. */
    private void before(int op) throws IOException {
      if (!hit(op)) {
        return;
      }
      injected.countDown();
      switch (plan.fault()) {
        case SLOW -> {
          slowFrom = System.nanoTime();
          try {
            Thread.sleep(plan.slowMs());
          } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
          }
          slowTo = System.nanoTime();
        }
        case LANDS_BUT_FAILS -> throw new IOException("the directory's flush failed (injected)");
        case HALF_WRITE -> throw new IllegalStateException("a half write is the write's");
        default -> throw new IOException("EIO (injected)");
      }
    }

    private boolean under(Path p) {
      return p.toAbsolutePath().startsWith(downloads);
    }

    @Override
    public Handle create(Path path, boolean inherit) throws IOException {
      boolean save = path.toAbsolutePath().equals(tmp);
      if (save) {
        before(1);
      }
      Handle h = inner.create(path, inherit);
      return new Handle() {
        @Override
        public void write(byte[] b, int off, int len) throws IOException {
          if (save && plan.fault() == Fault.HALF_WRITE && hit(2)) {
            injected.countDown();
            h.write(b, off, len / 2);
            throw new IOException("ENOSPC (injected), after " + len / 2 + " of " + len + " bytes");
          }
          if (save) {
            before(2);
          }
          h.write(b, off, len);
        }

        @Override
        public void sync() throws IOException {
          if (save) {
            before(3);
          }
          h.sync();
        }

        @Override
        public void close() throws IOException {
          h.close();
        }
      };
    }

    @Override
    public boolean remove(Path path) throws IOException {
      if (path.toAbsolutePath().equals(tmp)) {
        if (!inSave) {
          // A save begins: its leftover goes. (Otherwise this is a failed save's cleanup.)
          saves++;
          inSave = true;
          before(0);
        } else {
          inSave = false;
        }
      } else if (under(path) && plan.fault() == Fault.REMOVE_FAILS) {
        fired = true;
        firedAt = System.nanoTime();
        injected.countDown();
        throw new IOException("EIO deleting " + path + " (injected)");
      }
      return inner.remove(path);
    }

    @Override
    public void rename(Path from, Path to) throws IOException {
      before(4);
      inner.rename(from, to);
    }

    @Override
    public boolean syncDir(Path dir) throws IOException {
      try {
        before(5);
      } finally {
        inSave = false;
      }
      return inner.syncDir(dir);
    }

    @Override
    public boolean mkdir(Path dir) throws IOException {
      if (plan.fault() == Fault.NO_DOWNLOADS_DIR) {
        fired = true;
        firedAt = System.nanoTime();
        injected.countDown();
        throw new IOException("EACCES making " + dir + " (injected)");
      }
      return inner.mkdir(dir);
    }

    @Override
    public boolean clear(Path dir) throws IOException {
      return inner.clear(dir);
    }
  }

  // ---------------------------------------------------------------------------------------------
  // Downloads' files are made new.

  // A file already at a download's path (left by a run that crashed) is neither written over, nor
  // handed over, nor deleted: the download fails.
  @Test
  void aFileAlreadyAtADownloadsPathIsLeftAlone(@TempDir Path dir) throws Exception {
    HttpServer files = files();
    try {
      Path state = dir.resolve(STATE);
      LinkedBlockingQueue<CoreProto.Input> got = new LinkedBlockingQueue<>();
      Executor x = new Executor(Transport.of(HttpClient.newHttpClient()), got::add, state);
      byte[] left = {7, 7, 7};
      Files.createDirectories(Executor.downloadsDir(state));
      Files.write(x.downloadPath(1), left);
      x.run(download(1, files));
      CoreProto.Input in = got.poll(20, TimeUnit.SECONDS);
      assertEquals(CoreProto.Input.KindCase.DOWNLOAD_FAILED, in.getKindCase(), String.valueOf(in));
      StateFile.settled(state);
      assertTrue(Files.exists(x.downloadPath(1)), "the file left behind is not deleted");
      assertArrayEquals(left, Files.readAllBytes(x.downloadPath(1)), "the file left behind is not written over");
    } finally {
      files.stop(0);
    }
  }

  // A download cancelled while its file is being written: the file goes, and only once nothing
  // more is written to it (a write after its removal is a write to a file not made new).
  @Test
  void aCancelledDownloadsFileIsRemovedAfterItsLastWrite(@TempDir Path dir) throws Exception {
    CountDownLatch release = new CountDownLatch(1);
    HttpServer files = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    files.setExecutor(java.util.concurrent.Executors.newCachedThreadPool());
    files.createContext("/p", ex -> {
      ex.sendResponseHeaders(200, 2L * PACKAGE.length);
      try (OutputStream out = ex.getResponseBody()) {
        out.write(PACKAGE);
        out.flush();
        Raw.await(release);
        out.write(PACKAGE);
      } catch (IOException e) {
        // the client went
      }
    });
    files.start();
    Path state = dir.resolve(STATE);
    Core.Tap tap = Core.Tap.open(dir.resolve("x.tap").toString());
    try {
      Disk.Journal.state(tap, state);
      CountDownLatch writing = new CountDownLatch(1);
      CountDownLatch cancelled = new CountDownLatch(1);
      Disk slowWrites = new Disk() {
        @Override
        public Handle create(Path path, boolean inherit) throws IOException {
          Handle h = REAL.create(path, inherit);
          return new Handle() {
            @Override
            public void write(byte[] b, int off, int len) throws IOException {
              writing.countDown();
              Raw.await(cancelled);
              h.write(b, off, len);
            }

            @Override
            public void sync() throws IOException {
              h.sync();
            }

            @Override
            public void close() throws IOException {
              h.close();
            }
          };
        }

        @Override
        public boolean remove(Path path) throws IOException {
          return REAL.remove(path);
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
      };
      LinkedBlockingQueue<CoreProto.Input> got = new LinkedBlockingQueue<>();
      Executor x = new Executor(Transport.of(HttpClient.newHttpClient()), got::add, state, new Disk.Journal(slowWrites, tap, state));
      x.run(download(1, files));
      assertTrue(writing.await(20, TimeUnit.SECONDS), "the file is being written");
      x.run(CoreProto.Effect.newBuilder().setCancel(CoreProto.Id.newBuilder().setId(1)).build());
      cancelled.countDown();
      release.countDown();
      StateFile.settled(state);
      assertFalse(Files.exists(x.downloadPath(1)), "nothing is left");
    } finally {
      release.countDown();
      tap.close();
      files.stop(0);
    }
    List<Event> ev = events(Path.of(tap.path));
    keep(Path.of(tap.path), "cancelled-download");
    assertEquals(List.of(), problems(ev), String.valueOf(ev.stream().map(Event::file).toList()));
    assertEquals("remove", ev.get(ev.size() - 1).file().get("op"), "the removal is the last word");
  }

  // ---------------------------------------------------------------------------------------------
  // Servers.

  /**
   * An OpAMP server whose answers offer a remote config (each message the next one, up to {@code
   * configs}: each is saved) and a package, served by a file server.
   */
  static final class Servers implements AutoCloseable {
    final AtomicInteger configs = new AtomicInteger();
    final AtomicInteger served = new AtomicInteger();
    final List<Long> arrivals = new CopyOnWriteArrayList<>();
    private final HttpServer files;
    private final HttpServer opamp;

    Servers(int configs) throws Exception {
      this.configs.set(configs);
      files = files();
      Opamp.PackagesAvailable offer = Opamp.PackagesAvailable.newBuilder()
          .putPackages("p", Opamp.PackageAvailable.newBuilder()
              .setType(Opamp.PackageType.PackageType_TopLevel).setVersion("1")
              .setHash(ByteString.copyFromUtf8("p1"))
              .setFile(Opamp.DownloadableFile.newBuilder()
                  .setDownloadUrl("http://127.0.0.1:" + files.getAddress().getPort() + "/p")
                  .setContentHash(ByteString.copyFrom(sha(PACKAGE))))
              .build())
          .setAllPackagesHash(ByteString.copyFromUtf8("all1"))
          .build();
      opamp = LifecycleTest.opamp(new CopyOnWriteArrayList<>(), m -> {
        arrivals.add(System.nanoTime());
        int n = Math.min(served.incrementAndGet(), this.configs.get());
        return Opamp.ServerToAgent.newBuilder().setPackagesAvailable(offer).setRemoteConfig(remote(n));
      });
    }

    OpampClient.Config config(Path state) {
      return config(state, OpampClient.Mode.JAVA);
    }

    OpampClient.Config config(Path state, OpampClient.Mode mode) {
      return OpampClient.Config.builder(LifecycleTest.endpoint(opamp), UID)
          .mode(mode)
          .environment(Map.of())
          .capabilities(Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE)
          .heartbeatInterval(Duration.ofMillis(50))
          .stopTimeout(Duration.ofMillis(STOP_TIMEOUT_MS))
          .stateFile(state)
          .acceptPackages(true)
          .build();
    }

    @Override
    public void close() {
      opamp.stop(0);
      files.stop(0);
    }
  }

  static Opamp.AgentRemoteConfig remote(int n) {
    return Opamp.AgentRemoteConfig.newBuilder()
        .setConfig(Opamp.AgentConfigMap.newBuilder().putConfigMap("", Opamp.AgentConfigObject.newBuilder()
            .setBody(ByteString.copyFromUtf8("{\"n\":" + n + "}")).setContentType("application/json").build()))
        .setConfigHash(ByteString.copyFromUtf8("h" + n))
        .build();
  }

  /** A file server: /p is the package. */
  static HttpServer files() throws IOException {
    HttpServer files = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    files.setExecutor(java.util.concurrent.Executors.newCachedThreadPool());
    files.createContext("/p", ex -> {
      ex.sendResponseHeaders(200, PACKAGE.length);
      try (OutputStream out = ex.getResponseBody()) {
        out.write(PACKAGE);
      }
    });
    files.start();
    return files;
  }

  static CoreProto.Effect download(long id, HttpServer files) {
    return CoreProto.Effect.newBuilder().setDownload(CoreProto.Download.newBuilder()
        .setId(id).setUrl("http://127.0.0.1:" + files.getAddress().getPort() + "/p")
        .setMaxBytes(1 << 24).setTimeoutMs(20_000)).build();
  }

  static byte[] sha(byte[] b) throws Exception {
    return MessageDigest.getInstance("SHA-256").digest(b);
  }

  // ---------------------------------------------------------------------------------------------
  // The tap, read back.

  /** A record: its kind, and decoded: a file operation's fields, a step's input, a reply's output. */
  record Event(int kind, Map<String, String> file, CoreProto.Input in, CoreProto.Output out) {}

  private static final Pattern FIELD = Pattern.compile("\"(\\w+)\":(?:\"((?:[^\"\\\\]|\\\\.)*)\"|(\\d+))");

  static Map<String, String> json(byte[] bytes) {
    Map<String, String> out = new HashMap<>();
    Matcher m = FIELD.matcher(new String(bytes, java.nio.charset.StandardCharsets.UTF_8));
    while (m.find()) {
      out.put(m.group(1), m.group(2) != null ? m.group(2) : m.group(3));
    }
    return out;
  }

  /** The records of {@code tap}: a last one being written is left out. */
  static List<Event> events(Path tap) throws Exception {
    ByteBuffer b = ByteBuffer.wrap(Files.readAllBytes(tap)).order(ByteOrder.LITTLE_ENDIAN);
    List<Event> out = new ArrayList<>();
    while (b.remaining() >= 13) {
      int kind = b.get();
      int len = b.getInt();
      b.getLong();
      if (b.remaining() < len) {
        break;
      }
      byte[] bytes = new byte[len];
      b.get(bytes);
      out.add(switch (kind) {
        case 3 -> new Event(kind, null, CoreProto.Step.parseFrom(bytes).getInput(), null);
        case 4 -> new Event(kind, null, null, CoreProto.Reply.parseFrom(bytes).getOutput());
        case 5 -> new Event(kind, json(bytes), null, null);
        default -> new Event(kind, null, null, null);
      });
    }
    return out;
  }

  static List<Event> eventsQuietly(Path tap) {
    try {
      return events(tap);
    } catch (Exception e) {
      return List.of();
    }
  }

  static byte[] firstBytes(Path tap) throws IOException {
    ByteBuffer b = ByteBuffer.wrap(Files.readAllBytes(tap)).order(ByteOrder.LITTLE_ENDIAN);
    b.get();
    byte[] bytes = new byte[b.getInt()];
    b.getLong();
    b.get(bytes);
    return bytes;
  }

  static int indexOf(List<Event> ev, java.util.function.Predicate<Event> p) {
    for (int i = 0; i < ev.size(); i++) {
      if (p.test(ev.get(i))) {
        return i;
      }
    }
    throw new AssertionError("no such record");
  }

  /** The data of the core's saves, in the order it asked for them (hex). */
  static List<String> stores(List<Event> ev) {
    List<String> out = new ArrayList<>();
    for (Event e : ev) {
      if (e.out() != null) {
        e.out().getEffectsList().stream().filter(CoreProto.Effect::hasStore)
            .forEach(x -> out.add(HexFormat.of().formatHex(x.getStore().getData().toByteArray())));
      }
    }
    return out;
  }

  /** The place in {@link #stores} of the last save fed back stored (-1: none). */
  static int lastStored(List<Event> ev, List<String> saves) {
    List<Long> ids = new ArrayList<>();
    int last = -1;
    for (Event e : ev) {
      if (e.out() != null) {
        e.out().getEffectsList().stream().filter(CoreProto.Effect::hasStore).forEach(x -> ids.add(x.getStore().getId()));
      }
      if (e.in() != null && e.in().hasStored()) {
        last = Math.max(last, ids.indexOf(e.in().getStored().getId()));
      }
    }
    return last;
  }

  /** Every save asked for has its answer, stored or store_failed: once, unless the core cancelled it. */
  static boolean settled(List<Event> ev) {
    Map<Long, Integer> answers = answers(ev);
    return !ev.isEmpty() && answers.values().stream().allMatch(n -> n > 0);
  }

  private static Map<Long, Integer> answers(List<Event> ev) {
    Map<Long, Integer> answers = new HashMap<>();
    for (Event e : ev) {
      if (e.out() != null) {
        for (CoreProto.Effect x : e.out().getEffectsList()) {
          if (x.hasStore()) {
            answers.put(x.getStore().getId(), 0);
          } else if (x.hasCancel()) {
            answers.computeIfPresent(x.getCancel().getId(), (k, n) -> n == 0 ? 1 : n);
          }
        }
      }
      if (e.in() != null && (e.in().hasStored() || e.in().hasStoreFailed())) {
        long id = e.in().hasStored() ? e.in().getStored().getId() : e.in().getStoreFailed().getId();
        answers.merge(id, 1, Integer::sum);
      }
    }
    return answers;
  }

  static void answeredOnce(List<Event> ev) {
    Map<Long, Integer> answered = new HashMap<>();
    Set<Long> asked = new HashSet<>();
    Set<Long> cancelled = new HashSet<>();
    for (Event e : ev) {
      if (e.out() != null) {
        for (CoreProto.Effect x : e.out().getEffectsList()) {
          if (x.hasStore()) {
            asked.add(x.getStore().getId());
          } else if (x.hasCancel()) {
            cancelled.add(x.getCancel().getId());
          }
        }
      }
      if (e.in() != null && (e.in().hasStored() || e.in().hasStoreFailed())) {
        answered.merge(e.in().hasStored() ? e.in().getStored().getId() : e.in().getStoreFailed().getId(), 1, Integer::sum);
      }
    }
    for (long id : asked) {
      int n = answered.getOrDefault(id, 0);
      assertTrue(n == 1 || (n == 0 && cancelled.contains(id)), "store " + id + " answered " + n + " times");
    }
  }

  /**
   * What opamp-tap's files checker would say, in short: a write only to a file made new, a
   * rename only of a file whose data is flushed, and stored only once its save's rename is
   * flushed (the directory) .
   */
  static List<String> problems(List<Event> ev) {
    List<String> out = new ArrayList<>();
    List<Long> ids = new ArrayList<>();
    List<String> data = new ArrayList<>();
    Set<String> made = new HashSet<>();
    Map<String, StringBuilder> written = new HashMap<>();
    Map<String, String> synced = new HashMap<>();
    String state = null;
    String landed = null;
    String durable = null;
    for (Event e : ev) {
      Map<String, String> f = e.file();
      if (f != null) {
        String p = f.get("path");
        switch (f.get("op")) {
          case "state" -> state = p;
          case "create" -> {
            made.add(p);
            written.put(p, new StringBuilder());
            synced.remove(p);
          }
          case "open" -> out.add("written in place: " + p);
          case "write" -> {
            if (!made.contains(p)) {
              out.add("a write to a file not made new: " + p);
            }
            written.computeIfAbsent(p, k -> new StringBuilder()).append(f.getOrDefault("data", ""));
          }
          case "sync" -> synced.put(p, String.valueOf(written.get(p)));
          case "rename" -> {
            String from = f.get("from");
            if (!made.contains(from)) {
              out.add("a rename of a file not made new: " + from);
            }
            String s = synced.get(from);
            if (s == null || !s.equals(String.valueOf(written.get(from)))) {
              out.add("renamed before its data was flushed: " + from);
            }
            if (f.get("to").equals(state)) {
              landed = s;
            }
            made.remove(from);
          }
          case "syncdir" -> {
            if (".".equals(p)) {
              durable = landed;
            }
          }
          case "remove" -> made.remove(p);
          case "clear" -> made.removeIf(m -> m.equals(p) || m.startsWith(p + "/"));
          default -> {}
        }
      } else if (e.out() != null) {
        for (CoreProto.Effect x : e.out().getEffectsList()) {
          if (x.hasStore()) {
            ids.add(x.getStore().getId());
            data.add(HexFormat.of().formatHex(x.getStore().getData().toByteArray()));
          }
        }
      } else if (e.in() != null && e.in().hasStored()) {
        int i = ids.indexOf(e.in().getStored().getId());
        int d = durable == null ? -1 : data.lastIndexOf(durable);
        if (i >= 0 && d < i) {
          out.add("stored " + e.in().getStored().getId() + " before its save was on the disk (no directory flush after its rename)");
        }
      }
    }
    return out;
  }

  /** The tap, kept under target/taps for a look with opamp-tap. */
  static void keep(Path tap, String name) {
    try {
      Path dir = Files.createDirectories(Path.of("target", "taps"));
      Files.copy(tap, dir.resolve(name + ".tap"), StandardCopyOption.REPLACE_EXISTING);
    } catch (IOException e) {
      // only for a look
    }
  }

  private static volatile boolean toldNoChecker;

  /** opamp-tap's verdict on {@code tap}, if it is there (the repository's target/release). */
  static void opampTap(Path tap) throws Exception {
    Path bin = Path.of("..", "..", "target", "release", "opamp-tap").toAbsolutePath().normalize();
    if (!Files.isExecutable(bin)) {
      if (!toldNoChecker) {
        toldNoChecker = true;
        System.out.println("FilesTest: " + bin + " isn't there (or this is a container without it): opamp-tap's"
            + " checks of the taps skipped; they are kept under target/taps");
      }
      return;
    }
    Process p = new ProcessBuilder(bin.toString(), tap.toString()).redirectErrorStream(true).start();
    String said = new String(p.getInputStream().readAllBytes(), java.nio.charset.StandardCharsets.UTF_8);
    assertTrue(p.waitFor(60, TimeUnit.SECONDS), "opamp-tap ends");
    assertEquals(0, p.exitValue(), "opamp-tap on " + tap + ":\n" + said);
  }
}
