// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.logfire.fleet;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.channels.FileChannel;
import java.nio.file.FileSystem;
import java.nio.file.FileSystems;
import java.nio.file.Files;
import java.nio.file.NoSuchFileException;
import java.nio.file.OpenOption;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;
import java.nio.file.attribute.AclEntry;
import java.nio.file.attribute.AclEntryFlag;
import java.nio.file.attribute.AclEntryPermission;
import java.nio.file.attribute.AclEntryType;
import java.nio.file.attribute.AclFileAttributeView;
import java.nio.file.attribute.FileAttribute;
import java.nio.file.attribute.PosixFilePermissions;
import java.nio.file.attribute.UserPrincipal;
import java.nio.file.attribute.UserPrincipalLookupService;
import java.nio.file.attribute.UserPrincipalNotFoundException;
import java.util.ArrayList;
import java.util.EnumSet;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.function.Consumer;

/**
 * Where the client keeps the core's saved state: one file, replaced atomically, that only the
 * agent's user can open. It holds secrets: the credentials of connection settings the server
 * offered, and the private keys of telemetry settings' certificates.
 *
 * <p>On POSIX file systems the file is created with mode 0600. On Windows (ACL file systems) it is created with an access-control list that allows the
 * user alone, nothing inherited from its directory, and opened without sharing until that list is
 * set. No entry for
 * SYSTEM or Administrators is added: the client needs none, and administrators can take ownership
 * of a file whatever its list says. (Java can't mark a list protected from inheritance, so an
 * administrator pushing a directory's permissions down to its files could add entries; the next
 * save writes a new file with the list again.)
 */
final class StateFile {
  /** Windows' options that keep others from opening a file while it is open, if this JDK has them. */
  static final List<OpenOption> NO_SHARING = noSharing();

  private StateFile() {}

  /** What was saved at {@code path}: nothing if there is no file yet. */
  static byte[] load(Path path) {
    try {
      return Files.readAllBytes(path);
    } catch (NoSuchFileException e) {
      return new byte[0];
    } catch (IOException e) {
      throw new UncheckedIOException("reading the state file " + path, e);
    }
  }

  /** A test seam: called at the start of every save, on the thread saving (a slow disk). */
  static volatile Consumer<Path> beforeSave = p -> {};

  /**
   * Saves under way, by state file. One can outlive the client that started it (a save already
   * writing finishes whole), so a client started on the same state file waits for it first ({@link #settled}).
   */
  private static final Map<Path, Integer> WORK = new HashMap<>();

  private static Path key(Path stateFile) {
    return stateFile.toAbsolutePath().normalize();
  }

  /** File work on {@code stateFile} starts. */
  static void begin(Path stateFile) {
    synchronized (WORK) {
      WORK.merge(key(stateFile), 1, Integer::sum);
    }
  }

  /** File work on {@code stateFile} is over. */
  static void end(Path stateFile) {
    synchronized (WORK) {
      WORK.computeIfPresent(key(stateFile), (k, n) -> n == 1 ? null : n - 1);
      WORK.notifyAll();
    }
  }

  /** Wait until no file work on {@code stateFile} is under way: before a client starts on it. */
  static void settled(Path stateFile) {
    Path k = key(stateFile);
    synchronized (WORK) {
      while (WORK.containsKey(k)) {
        try {
          WORK.wait();
        } catch (InterruptedException e) {
          Thread.currentThread().interrupt();
          return;
        }
      }
    }
  }

  /** The rename that makes a save land. */
  @FunctionalInterface
  interface Rename {
    void run() throws IOException;
  }

  /** Lets a save land, or not (it was given up on): runs the rename, or says no. */
  @FunctionalInterface
  interface Gate {
    boolean land(Rename rename) throws IOException;
  }

  /** Replace what is saved at {@code path} with {@code data}, all or nothing, even across a crash. */
  static void save(Path path, byte[] data) throws IOException {
    save(Disk.REAL, path, data, r -> {
      r.run();
      return true;
    });
  }

  /** The temporary file a save of {@code path} writes before renaming it over {@code path}. */
  static Path tmp(Path path) {
    return path.resolveSibling(path.getFileName() + ".tmp");
  }

  /**
   * {@link #save(Path, byte[])} on {@code disk}, the rename through {@code gate}: if it says no,
   * nothing changes (the new file goes) and this is false. A new file (a leftover from a save that
   * failed or crashed goes first, never reused), written, flushed, renamed over the state file, and
   * the directory flushed so the rename is on the disk too: only then has it landed. Any step
   * failing is an error, the last one too (the rename may have landed then: it is not reported as
   * saved, as it might not be on the disk).
   */
  static boolean save(Disk disk, Path path, byte[] data, Gate gate) throws IOException {
    beforeSave.accept(path);
    Path tmp = tmp(path);
    try {
      disk.remove(tmp);
      try (Disk.Handle h = disk.create(tmp)) {
        h.write(data, 0, data.length);
        h.sync();
      }
      if (!gate.land(() -> disk.rename(tmp, path))) {
        disk.remove(tmp);
        return false;
      }
      // The rename itself must reach the disk (where the platform can flush a directory): if the
      // flush fails, the save may be lost in a crash, so it isn't stored.
      disk.syncDir(Disk.Journal.directory(path));
    } catch (IOException e) {
      try {
        disk.remove(tmp);
      } catch (IOException left) {
        e.addSuppressed(left); // the next save removes it
      }
      throw new IOException("saving " + path + ": " + e, e);
    }
    return true;
  }

  /** A new file at {@code path}, open for writing, that only the user can open. */
  static FileChannel createPrivate(Path path) throws IOException {
    FileSystem fs = path.getFileSystem();
    Set<OpenOption> opts = new HashSet<>(Set.of(StandardOpenOption.CREATE_NEW, StandardOpenOption.WRITE));
    if (!isPosix(fs) && isAcl(fs)) {
      List<AclEntry> acl = userOnly(currentUser(fs));
      if (fs == FileSystems.getDefault()) {
        // Windows: nobody else opens it before its list is set (a list given at creation may
        // still take entries inherited from the directory).
        opts.addAll(NO_SHARING);
      }
      // Its list is set before anything is written to it, whatever it was created with.
      FileChannel ch = FileChannel.open(path, opts, aclAtCreation(fs, acl));
      try {
        setAcl(path, acl);
      } catch (IOException e) {
        ch.close();
        throw e;
      }
      return ch;
    }
    return FileChannel.open(path, opts, posixAttributes(fs, "rw-------"));
  }

  /** An access-control list allowing {@code user} everything and nobody anything else. */
  static List<AclEntry> userOnly(UserPrincipal user) {
    return List.of(AclEntry.newBuilder()
        .setType(AclEntryType.ALLOW)
        .setPrincipal(user)
        .setPermissions(EnumSet.allOf(AclEntryPermission.class))
        .setFlags(EnumSet.noneOf(AclEntryFlag.class))
        .build());
  }

  /**
   * The user the process runs as. Qualified by its domain where there is one, so that a local
   * account of the same name isn't taken for it.
   */
  static UserPrincipal currentUser(FileSystem fs) throws IOException {
    UserPrincipalLookupService lookup = fs.getUserPrincipalLookupService();
    String name = System.getProperty("user.name");
    String domain = System.getenv("USERDOMAIN");
    if (domain != null && !domain.isEmpty()) {
      try {
        return lookup.lookupPrincipalByName(domain + "\\" + name);
      } catch (UserPrincipalNotFoundException e) {
        // a local account, or a service's
      }
    }
    return lookup.lookupPrincipalByName(name);
  }

  /**
   * {@code NOSHARE_READ} and {@code NOSHARE_WRITE} (com.sun.nio.file.ExtendedOpenOption, in
   * jdk.unsupported: looked up rather than compiled against, so a JDK without them still runs).
   */
  private static List<OpenOption> noSharing() {
    List<OpenOption> out = new ArrayList<>();
    try {
      for (Object o : Class.forName("com.sun.nio.file.ExtendedOpenOption").getEnumConstants()) {
        if (o instanceof Enum<?> e && (e.name().equals("NOSHARE_READ") || e.name().equals("NOSHARE_WRITE"))) {
          out.add((OpenOption) o);
        }
      }
    } catch (ClassNotFoundException e) {
      // not this JDK
    }
    return List.copyOf(out);
  }

  /** Set exactly {@code acl} on {@code path}: nothing inherited stays. */
  private static void setAcl(Path path, List<AclEntry> acl) throws IOException {
    Files.getFileAttributeView(path, AclFileAttributeView.class).setAcl(acl);
  }

  /**
   * {@code acl} as an attribute to create a file with, where the file system takes one: Windows'
   * does, and then the file is never open to others, even for a moment.
   */
  private static FileAttribute<?>[] aclAtCreation(FileSystem fs, List<AclEntry> acl) {
    if (fs != FileSystems.getDefault()) {
      return new FileAttribute<?>[0];
    }
    return new FileAttribute<?>[] {new FileAttribute<List<AclEntry>>() {
      @Override
      public String name() {
        return "acl:acl";
      }

      @Override
      public List<AclEntry> value() {
        return acl;
      }
    }};
  }

  private static FileAttribute<?>[] posixAttributes(FileSystem fs, String perms) {
    return isPosix(fs)
        ? new FileAttribute<?>[] {PosixFilePermissions.asFileAttribute(PosixFilePermissions.fromString(perms))}
        : new FileAttribute<?>[0];
  }

  private static boolean isPosix(FileSystem fs) {
    return fs.supportedFileAttributeViews().contains("posix");
  }

  private static boolean isAcl(FileSystem fs) {
    return fs.supportedFileAttributeViews().contains("acl");
  }
}
