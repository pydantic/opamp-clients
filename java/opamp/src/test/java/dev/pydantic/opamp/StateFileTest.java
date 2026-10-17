// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.common.jimfs.Configuration;
import com.google.common.jimfs.Jimfs;
import com.google.protobuf.ByteString;
import dev.pydantic.opamp.core.CoreProto;
import java.io.UncheckedIOException;
import java.net.http.HttpClient;
import java.nio.file.FileSystem;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.AclEntry;
import java.nio.file.attribute.AclEntryFlag;
import java.nio.file.attribute.AclEntryPermission;
import java.nio.file.attribute.AclEntryType;
import java.nio.file.attribute.AclFileAttributeView;
import java.nio.file.attribute.PosixFilePermissions;
import java.nio.file.attribute.UserPrincipal;
import java.util.ArrayList;
import java.util.EnumSet;
import java.util.List;
import java.util.Set;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

class StateFileTest {
  @TempDir Path dir;

  @Test
  void savesReplaceWhatWasSavedPrivately() throws Exception {
    Path path = dir.resolve("state.bin");
    assertArrayEquals(new byte[0], StateFile.load(path));
    StateFile.save(path, new byte[] {1});
    StateFile.save(path, new byte[] {2, 3});
    assertArrayEquals(new byte[] {2, 3}, StateFile.load(path));
    assertFalse(Files.exists(dir.resolve("state.bin.tmp")));
    if (path.getFileSystem().supportedFileAttributeViews().contains("posix")) {
      assertEquals("rw-------", PosixFilePermissions.toString(Files.getPosixFilePermissions(path)));
    }
  }

  @Test
  void aLeftoverTemporaryFileIsReplacedNotReused() throws Exception {
    Path path = dir.resolve("state.bin");
    Path tmp = dir.resolve("state.bin.tmp");
    Files.write(tmp, new byte[] {9, 9, 9});
    if (path.getFileSystem().supportedFileAttributeViews().contains("posix")) {
      Files.setPosixFilePermissions(tmp, PosixFilePermissions.fromString("rw-r--r--"));
    }
    StateFile.save(path, new byte[] {1});
    assertArrayEquals(new byte[] {1}, StateFile.load(path));
    if (path.getFileSystem().supportedFileAttributeViews().contains("posix")) {
      assertEquals("rw-------", PosixFilePermissions.toString(Files.getPosixFilePermissions(path)));
    }
  }

  @Test
  void theListAllowsTheUserAloneAndADirectorysIsInherited() {
    UserPrincipal alice = () -> "alice";
    AclEntry file = StateFile.userOnly(alice, false).get(0);
    assertEquals(1, StateFile.userOnly(alice, false).size());
    assertEquals(AclEntryType.ALLOW, file.type());
    assertEquals(alice, file.principal());
    assertEquals(EnumSet.allOf(AclEntryPermission.class), file.permissions());
    assertTrue(file.flags().isEmpty());
    AclEntry directory = StateFile.userOnly(alice, true).get(0);
    assertEquals(Set.of(AclEntryFlag.FILE_INHERIT, AclEntryFlag.DIRECTORY_INHERIT), directory.flags());
  }

  @Test
  void thisJdkCanOpenAFileUnshared() {
    assertEquals(List.of("NOSHARE_READ", "NOSHARE_WRITE"),
        StateFile.NO_SHARING.stream().map(o -> ((Enum<?>) o).name()).sorted().toList());
  }

  /** Windows' way (access-control lists, no POSIX modes), on an in-memory file system. */
  @Test
  void onAnAclFileSystemTheFilesAllowTheUserAlone() throws Exception {
    Configuration windows = Configuration.windows().toBuilder().setAttributeViews("basic", "owner", "acl").build();
    try (FileSystem fs = Jimfs.newFileSystem(windows)) {
      Path state = fs.getPath("C:\\agent\\state.bin");
      Files.createDirectories(state.getParent());
      // What a directory open to everyone would pass on.
      UserPrincipal everyone = fs.getUserPrincipalLookupService().lookupPrincipalByName("Everyone");
      Files.getFileAttributeView(state.getParent(), AclFileAttributeView.class).setAcl(List.of(AclEntry.newBuilder()
          .setType(AclEntryType.ALLOW).setPrincipal(everyone).setPermissions(AclEntryPermission.READ_DATA).build()));
      StateFile.save(state, new byte[] {1, 2});
      StateFile.save(state, new byte[] {3});
      assertArrayEquals(new byte[] {3}, StateFile.load(state));
      UserPrincipal user = StateFile.currentUser(fs);
      assertEquals(StateFile.userOnly(user, false), acl(state));

      Path download = Executor.downloadsDir(state).resolve("7");
      assertTrue(Disk.REAL.mkdir(download.getParent()));
      try (Disk.Handle out = Disk.REAL.create(download, true)) {
        out.write(new byte[] {4}, 0, 1);
      }
      assertArrayEquals(new byte[] {4}, Files.readAllBytes(download));
      assertEquals(StateFile.userOnly(user, true), acl(download.getParent()));
    }
  }

  private static List<AclEntry> acl(Path p) throws Exception {
    return Files.getFileAttributeView(p, AclFileAttributeView.class).getAcl();
  }

  @Test
  void aStoreIsAnswered() {
    List<CoreProto.Input> got = new java.util.concurrent.CopyOnWriteArrayList<>();
    HttpClient http = HttpClient.newHttpClient();
    for (Path p : new Path[] {dir.resolve("state.bin"), dir.resolve("missing").resolve("state.bin"), null}) {
      long id = got.size() + 7;
      CoreProto.Store store = CoreProto.Store.newBuilder().setId(id).setData(ByteString.copyFromUtf8("x")).build();
      new Executor(Transport.of(http), got::add, p).run(CoreProto.Effect.newBuilder().setStore(store).build());
      if (p != null) {
        StateFile.settled(p); // saved beside the client's thread
      }
    }
    assertEquals(7, got.get(0).getStored().getId());
    assertEquals(8, got.get(1).getStoreFailed().getId());
    assertTrue(got.get(1).getStoreFailed().getDetail().contains("state.bin"));
    assertEquals(9, got.get(2).getStoreFailed().getId());
  }

  @Test
  void anUnreadableStateFileStopsTheClientStarting() {
    for (OpampClient.Mode mode : OpampClient.Mode.values()) {
      OpampClient.Config config =
          OpampClient.Config.builder("http://127.0.0.1:1", new byte[16]).mode(mode).stateFile(dir).build();
      assertThrows(UncheckedIOException.class, () -> OpampClient.start(config, OpampClient.Status.of(null), null), mode.name());
    }
  }
}
