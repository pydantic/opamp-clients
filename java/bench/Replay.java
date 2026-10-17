// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Replays a recording of a module's inputs (node/opamp/bench/download.mjs) in Chicory, the runtime
// java/opamp uses: the same init and steps, so the same work, and reports the time spent in the
// module. `compiled` replays with the classes java/opamp ships (the core compiled to JVM classes
// when its jar is built); a module's path replays that module under Chicory's interpreter.
//
//   java -cp opamp-client.jar:runtime.jar:wasm.jar Replay.java compiled record.bin
//   java -cp opamp-client.jar:runtime.jar:wasm.jar Replay.java opamp_core.wasm record.bin

import com.dylibso.chicory.runtime.ExportFunction;
import com.dylibso.chicory.runtime.Instance;
import com.dylibso.chicory.runtime.Memory;
import com.dylibso.chicory.wasm.Parser;
import dev.pydantic.opamp.core.CoreModule;
import java.io.BufferedInputStream;
import java.io.DataInputStream;
import java.io.EOFException;
import java.io.File;
import java.io.FileInputStream;

public class Replay {
  public static void main(String[] args) throws Exception {
    Instance instance = args[0].equals("compiled")
        ? Instance.builder(CoreModule.load()).withMachineFactory(CoreModule::create).build()
        : Instance.builder(Parser.parse(new File(args[0]))).build();
    Memory memory = instance.memory();
    ExportFunction alloc = instance.export("alloc");
    ExportFunction free = instance.export("free");
    ExportFunction init = instance.export("init");
    ExportFunction step = instance.export("step");
    long in = 0, out = 0, steps = 0, nanos = 0;
    try (var r = new DataInputStream(new BufferedInputStream(new FileInputStream(args[1]), 1 << 20))) {
      while (true) {
        int tag = r.read();
        if (tag < 0) break;
        int len = Integer.reverseBytes(r.readInt());
        byte[] buf = new byte[len];
        r.readFully(buf);
        long t0 = System.nanoTime();
        int ptr = (int) alloc.apply(len)[0];
        memory.write(ptr, buf);
        long packed = (tag == 0 ? init : step).apply(ptr, len)[0];
        int outPtr = (int) (packed >>> 32);
        int outLen = (int) (packed & 0xffffffffL);
        byte[] reply = memory.readBytes(outPtr, outLen);
        free.apply(outPtr, outLen);
        nanos += System.nanoTime() - t0;
        in += len;
        out += reply.length;
        steps++;
      }
    } catch (EOFException e) {
      // a truncated recording: what was read counts
    }
    double s = nanos / 1e9;
    System.out.printf("{\"steps\": %d, \"in_mb\": %.1f, \"out_mb\": %.1f, \"seconds\": %.2f, \"mb_per_s\": %.2f}%n",
        steps, in / 1048576.0, out / 1048576.0, s, in / 1048576.0 / s);
  }
}
