// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import com.dylibso.chicory.runtime.ExportFunction;
import com.dylibso.chicory.runtime.Instance;
import com.dylibso.chicory.runtime.Machine;
import com.dylibso.chicory.runtime.Memory;
import com.dylibso.chicory.wasm.Parser;
import com.dylibso.chicory.wasm.WasmModule;
import com.google.protobuf.InvalidProtocolBufferException;
import com.google.protobuf.MessageLite;
import dev.pydantic.opamp.core.CoreModule;
import dev.pydantic.opamp.core.CoreProto;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.OutputStream;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.nio.channels.Channels;
import java.nio.file.FileAlreadyExistsException;
import java.nio.file.Path;
import java.util.Collections;
import java.util.IdentityHashMap;
import java.util.Map;
import java.util.concurrent.atomic.AtomicLong;
import java.util.function.Function;

/**
 * The shared OpAMP client core (src/main/wasm/opamp_core.wasm), run with Chicory: a
 * WebAssembly runtime in plain Java, nothing native. The module is compiled to JVM classes when
 * this jar is built (pom.xml), so no instruction of it is interpreted. Each Core is one module
 * instance, holding one client's state. Not thread-safe: the client calls it from one thread.
 * {@link #close} it when done (it closes the core tap, if there is one).
 */
final class Core implements AutoCloseable {
  /** The embedded core: its classes compiled from the module when this jar was built. */
  private static WasmModule module() {
    return CoreModule.load();
  }

  /** Tests only: parse a module to hand to {@link #Core(WasmModule, CoreProto.Config)}. */
  static WasmModule parse(byte[] wasm) {
    return Parser.parse(new java.io.ByteArrayInputStream(wasm));
  }

  /** Tests only: modules handed in that run as classes compiled from them, not interpreted. */
  private static final Map<WasmModule, Function<Instance, Machine>> COMPILED =
      Collections.synchronizedMap(new IdentityHashMap<>());

  /** Tests only: run {@code module}, wherever it is handed in, as {@code machines} (its compiled classes). */
  static void compiled(WasmModule module, Function<Instance, Machine> machines) {
    COMPILED.put(module, machines);
  }

  /** The core refused an input (a bug in this client) or trapped (a bug in the core). */
  static final class CoreException extends RuntimeException {
    CoreException(String message, Throwable cause) {
      super(message, cause);
    }
  }

  private final Memory memory;
  private final ExportFunction alloc;
  private final ExportFunction free;
  private final ExportFunction init;
  private final ExportFunction step;
  /** The core tap's file, or null: no tap. */
  private final Tap tap;
  /** What init returned. */
  private final CoreProto.Output initOutput;

  /** A core with {@code config}, tapped if {@code OPAMP_CORE_TAP} names a file. */
  Core(CoreProto.Config config) {
    this(module(), config, System.getenv(Tap.VARIABLE));
  }

  /** A core with {@code config}, tapped to {@code tapPath} (null or empty: no tap). */
  Core(CoreProto.Config config, String tapPath) {
    this(module(), config, tapPath);
  }

  /**
   * Tests only: run {@code module}, a module with the core's exports and ABI (the adversary,
   * testdata/core/opamp_core_adversary.wasm), instead of the embedded core.
   */
  Core(WasmModule module, CoreProto.Config config) {
    this(module, config, System.getenv(Tap.VARIABLE));
  }

  /**
   * Tests only: a core running {@code module} (null: the embedded core) with {@code config}, tapped
   * to {@code tapPath} (null: as {@code OPAMP_CORE_TAP} says; empty: no tap).
   */
  static Core of(WasmModule module, CoreProto.Config config, String tapPath) {
    return new Core(module == null ? module() : module, config, tapPath == null ? System.getenv(Tap.VARIABLE) : tapPath);
  }

  private Core(WasmModule module, CoreProto.Config config, String tapPath) {
    Instance.Builder builder = Instance.builder(module);
    if (module == CoreModule.load()) {
      // The embedded core runs as its compiled classes; a module handed in (tests) is interpreted,
      // unless the test compiled it.
      builder.withMachineFactory(CoreModule::create);
    } else {
      Function<Instance, Machine> machines = COMPILED.get(module);
      if (machines != null) {
        builder.withMachineFactory(machines);
      }
    }
    Instance instance = builder.build();
    memory = instance.memory();
    alloc = instance.export("alloc");
    free = instance.export("free");
    init = instance.export("init");
    step = instance.export("step");
    tap = tapPath == null || tapPath.isEmpty() ? null : Tap.open(tapPath);
    try {
      initOutput = call(init, Tap.INIT_IN, config);
    } catch (RuntimeException e) {
      close();
      throw e;
    }
  }

  /** What init returned: the instance UID in use, above all. */
  CoreProto.Output initOutput() {
    return initOutput;
  }

  /** The core tap, or null: none. The client's file operations go to it too ({@link Disk.Journal}). */
  Tap tap() {
    return tap;
  }

  /** The core tap's file, or null: no tap. */
  String tapPath() {
    return tap == null ? null : tap.path;
  }

  /**
   * Feed the core one input at {@code nowMs} (milliseconds on a monotonic clock) and {@code unixMs}
   * (the wall clock, in ms since the Unix epoch: Rust mode reads Retry-After dates against it).
   */
  CoreProto.Output step(long nowMs, long unixMs, CoreProto.Input input) {
    return call(step, Tap.STEP_IN,
        CoreProto.Step.newBuilder().setNowMs(nowMs).setInput(input).setUnixMs(unixMs).build());
  }

  /**
   * Close the core tap's file, if there is one. The core is not called again; file operations
   * still finishing after this (a save outliving its client) are not recorded.
   */
  @Override
  public void close() {
    if (tap != null) {
      tap.close();
    }
  }

  private CoreProto.Output call(ExportFunction fn, int kind, MessageLite msg) {
    byte[] data = msg.toByteArray();
    if (tap != null) {
      tap.record(kind, data);
    }
    long packed;
    byte[] raw;
    try {
      int ptr = (int) alloc.apply(data.length)[0];
      memory.write(ptr, data);
      packed = fn.apply(ptr, data.length)[0];
      int outPtr = (int) (packed >>> 32);
      int outLen = (int) (packed & 0xffffffffL);
      raw = memory.readBytes(outPtr, outLen);
      if (tap != null) {
        tap.record(kind + 1, raw);
      }
      free.apply(outPtr, outLen);
    } catch (RuntimeException e) {
      throw new CoreException("trap: " + e, e);
    }
    CoreProto.Reply reply;
    try {
      reply = CoreProto.Reply.parseFrom(raw);
    } catch (InvalidProtocolBufferException e) {
      throw new CoreException("unreadable reply", e);
    }
    if (!reply.getError().isEmpty()) {
      throw new CoreException(reply.getError(), null);
    }
    return reply.getOutput();
  }

  // Core tap: with OPAMP_CORE_TAP=<path> set, every call into the core is appended to a file for the
  // effects checker (opamp-tap). Each record is a kind byte (1 init in, 2 init out, 3 step in,
  // 4 step out), a u32 LE length, a u64 LE monotonic time in nanoseconds, then the bytes exactly as
  // they crossed (Config, Reply, Step, Reply). A process's first core writes <path>, each later one
  // <path>.<pid>.<n>. Unset, it costs nothing. With a state file, the file operations of the
  // client's saves and downloads go to it too: kind 5, one JSON object each (opamp-tap's
  // file records), from the threads that carry them out, so records are written under a lock.
  // The file holds the Config's headers and the credentials saved, so a new one is the user's
  // only (0600, or an ACL of the user alone), as the state file is.
  static final class Tap {
    static final String VARIABLE = "OPAMP_CORE_TAP";
    static final int INIT_IN = 1;
    static final int STEP_IN = 3;
    static final int FILE = 5;
    /** Cores tapped so far in this process. */
    private static final AtomicLong TAPPED = new AtomicLong();
    /** The origin of the records' times: only order and differences matter. */
    private static final long ORIGIN = System.nanoTime();

    /** The file written, for tests. */
    final String path;
    private final OutputStream out;
    private boolean closed;

    private Tap(String path, OutputStream out) {
      this.path = path;
      this.out = out;
    }

    /** The tap of this process's next core, or null (said once on stderr) if its file won't open. */
    static Tap open(String base) {
      long n = TAPPED.getAndIncrement();
      String path = n == 0 ? base : base + "." + ProcessHandle.current().pid() + "." + n;
      try {
        return new Tap(path, append(Path.of(path)));
      } catch (IOException | RuntimeException e) {
        System.err.println("opamp: the core tap " + path + " can't be opened, running without it: " + e);
        return null;
      }
    }

    /** {@code path} to append to: made the user's only if new, as it was if not. */
    private static OutputStream append(Path path) throws IOException {
      try {
        return Channels.newOutputStream(StateFile.createPrivate(path));
      } catch (FileAlreadyExistsException e) {
        return new FileOutputStream(path.toFile(), true);
      }
    }

    /**
     * One record, in one unbuffered write, so a process killed leaves whole records; one at a time,
     * in the order of their times, whatever the thread. After {@link #close}, nothing.
     */
    synchronized void record(int kind, byte[] bytes) {
      if (closed) {
        return;
      }
      ByteBuffer b = ByteBuffer.allocate(13 + bytes.length).order(ByteOrder.LITTLE_ENDIAN);
      b.put((byte) kind).putInt(bytes.length).putLong(System.nanoTime() - ORIGIN).put(bytes);
      try {
        out.write(b.array());
      } catch (IOException e) {
        // A tap that fails must not fail the client: the checker sees the file end early.
      }
    }

    /** A file operation, done: {@code json} is its record (opamp-tap's file record format). */
    void file(String json) {
      record(FILE, json.getBytes(java.nio.charset.StandardCharsets.UTF_8));
    }

    synchronized void close() {
      if (closed) {
        return;
      }
      closed = true;
      try {
        out.close();
      } catch (IOException e) {
        // nothing more is written
      }
    }
  }
}
