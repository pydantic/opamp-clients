// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.logfire.fleet;

import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.channels.FileChannel;
import java.nio.file.FileSystems;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.nio.file.StandardOpenOption;
import java.util.HexFormat;
import java.util.Locale;

/**
 * The file operations of the client's saves, and nothing else: the real file system ({@link
 * #REAL}), journaled into the core tap when there is one ({@link Journal}), and replaced by tests
 * to inject faults. Every save goes through one of these, so what the tap says is what was done.
 */
interface Disk {
  /** An open file, made new by {@link #create}. */
  interface Handle extends AutoCloseable {
    /** Append {@code len} bytes of {@code b} from {@code off}: all of them, or an error. */
    void write(byte[] b, int off, int len) throws IOException;

    /** Flush its data to the disk (fsync). */
    void sync() throws IOException;

    @Override
    void close() throws IOException;
  }

  /**
   * A new file at {@code path}, created exclusively (never one already there), that only the user
   * can open.
   */
  Handle create(Path path) throws IOException;

  /** Delete {@code path}: whether it was there. */
  boolean remove(Path path) throws IOException;

  /** Rename {@code from} over {@code to}, atomically. */
  void rename(Path from, Path to) throws IOException;

  /**
   * Flush directory {@code dir}: its entries reach the disk. False if this platform can't flush a
   * directory (Windows can't open one to); an error if it can and the flush failed.
   */
  boolean syncDir(Path dir) throws IOException;

  /** The real file system. */
  Disk REAL = new Disk() {
    @Override
    public Handle create(Path path) throws IOException {
      FileChannel ch = StateFile.createPrivate(path);
      return new Handle() {
        @Override
        public void write(byte[] b, int off, int len) throws IOException {
          ByteBuffer buf = ByteBuffer.wrap(b, off, len);
          while (buf.hasRemaining()) {
            ch.write(buf);
          }
        }

        @Override
        public void sync() throws IOException {
          ch.force(true);
        }

        @Override
        public void close() throws IOException {
          ch.close();
        }
      };
    }

    @Override
    public boolean remove(Path path) throws IOException {
      return Files.deleteIfExists(path);
    }

    @Override
    public void rename(Path from, Path to) throws IOException {
      Files.move(from, to, StandardCopyOption.ATOMIC_MOVE, StandardCopyOption.REPLACE_EXISTING);
    }

    @Override
    public boolean syncDir(Path dir) throws IOException {
      if (!canSyncDirectories(dir)) {
        return false;
      }
      try (FileChannel ch = FileChannel.open(dir, StandardOpenOption.READ)) {
        ch.force(true);
      }
      return true;
    }
  };

  /**
   * Whether a directory on {@code dir}'s file system can be opened and flushed: the platform's own
   * file system, but not on Windows.
   */
  static boolean canSyncDirectories(Path dir) {
    return dir.getFileSystem() == FileSystems.getDefault()
        && !System.getProperty("os.name", "").toLowerCase(Locale.ROOT).startsWith("windows");
  }

  /**
   * {@code inner}, each operation that succeeded recorded in {@code tap} (opamp-tap's file
   * records, kind 5), its paths relative to the state file's directory: the checker holds the
   * client's saves to what {@code stored} means. Recorded after the operation, and before anything
   * that depends on it reaches the core (the caller feeds the core after this returns).
   */
  final class Journal implements Disk {
    private final Disk inner;
    private final Core.Tap tap;
    /** The state file's directory, absolute. */
    private final Path dir;

    Journal(Disk inner, Core.Tap tap, Path stateFile) {
      this.inner = inner;
      this.tap = tap;
      this.dir = directory(stateFile);
    }

    /** The directory {@code stateFile} is in, absolute: the current one for a bare name. */
    static Path directory(Path stateFile) {
      Path abs = stateFile.toAbsolutePath().normalize();
      return abs.getParent() == null ? abs : abs.getParent();
    }

    /** The first file record: the state file's name. */
    static void state(Core.Tap tap, Path stateFile) {
      tap.file("{\"op\":\"state\",\"path\":" + quote(stateFile.toAbsolutePath().normalize().getFileName().toString()) + "}");
    }

    /** {@code p} as the tap names it: relative to the state file's directory, parts joined by /. */
    String name(Path p) {
      Path abs = p.toAbsolutePath().normalize();
      if (abs.equals(dir)) {
        return ".";
      }
      Path rel = dir.relativize(abs);
      StringBuilder b = new StringBuilder();
      for (Path part : rel) {
        if (b.length() > 0) {
          b.append('/');
        }
        b.append(part);
      }
      return b.toString();
    }

    private void record(String op, Path path) {
      tap.file("{\"op\":\"" + op + "\",\"path\":" + quote(name(path)) + "}");
    }

    @Override
    public Handle create(Path path) throws IOException {
      Handle h = inner.create(path);
      record("create", path);
      String name = quote(name(path));
      return new Handle() {
        @Override
        public void write(byte[] b, int off, int len) throws IOException {
          h.write(b, off, len);
          // The bytes themselves: the checker compares them with the core's saves.
          tap.file("{\"op\":\"write\",\"path\":" + name + ",\"data\":\"" + HexFormat.of().formatHex(b, off, off + len) + "\"}");
        }

        @Override
        public void sync() throws IOException {
          h.sync();
          tap.file("{\"op\":\"sync\",\"path\":" + name + "}");
        }

        @Override
        public void close() throws IOException {
          h.close();
        }
      };
    }

    @Override
    public boolean remove(Path path) throws IOException {
      boolean was = inner.remove(path);
      if (was) {
        record("remove", path);
      }
      return was;
    }

    @Override
    public void rename(Path from, Path to) throws IOException {
      inner.rename(from, to);
      tap.file("{\"op\":\"rename\",\"from\":" + quote(name(from)) + ",\"to\":" + quote(name(to)) + "}");
    }

    @Override
    public boolean syncDir(Path d) throws IOException {
      boolean synced = inner.syncDir(d);
      if (synced) {
        record("syncdir", d);
      }
      return synced;
    }

    /** {@code s} as a JSON string. */
    static String quote(String s) {
      StringBuilder b = new StringBuilder("\"");
      for (int i = 0; i < s.length(); i++) {
        char c = s.charAt(i);
        switch (c) {
          case '"' -> b.append("\\\"");
          case '\\' -> b.append("\\\\");
          default -> {
            if (c < 0x20) {
              b.append(String.format("\\u%04x", (int) c));
            } else {
              b.append(c);
            }
          }
        }
      }
      return b.append('"').toString();
    }
  }
}
