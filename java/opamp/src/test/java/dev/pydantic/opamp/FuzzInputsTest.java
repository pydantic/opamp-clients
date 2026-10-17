// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package dev.pydantic.opamp;

import java.io.IOException;
import java.io.InputStream;
import java.util.ArrayList;
import java.util.List;
import java.util.stream.Stream;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.MethodSource;

/**
 * The Jazzer targets' seeds, and every input a run of them found (src/test/resources/fuzz/TARGET/,
 * listed in its index file), run as plain tests with every {@code mvn test}: what the fuzzer found
 * once stays fixed. scripts/java_fuzz.sh fuzzes each target for a while.
 */
class FuzzInputsTest {
  interface Target {
    void run(byte[] data) throws Exception;
  }

  static Stream<Arguments> inputs() throws IOException {
    List<Arguments> out = new ArrayList<>();
    add(out, "ClientFuzz", ClientFuzz.SEEDS, ClientFuzz::fuzzerTestOneInput);
    add(out, "CoreFuzz", CoreFuzz.SEEDS, CoreFuzz::fuzzerTestOneInput);
    return out.stream();
  }

  private static void add(List<Arguments> out, String target, List<byte[]> seeds, Target t) throws IOException {
    for (int i = 0; i < seeds.size(); i++) {
      out.add(Arguments.of(target + " seed " + i, seeds.get(i), t));
    }
    // The inputs found are listed one per line in fuzz/TARGET/index (resources can't be listed).
    try (InputStream index = FuzzInputsTest.class.getResourceAsStream("/fuzz/" + target + "/index")) {
      if (index == null) {
        return;
      }
      for (String name : new String(index.readAllBytes(), java.nio.charset.StandardCharsets.UTF_8).split("\n")) {
        if (name.isBlank() || name.startsWith("#")) {
          continue;
        }
        try (InputStream in = FuzzInputsTest.class.getResourceAsStream("/fuzz/" + target + "/" + name.trim())) {
          if (in == null) {
            throw new IOException("fuzz/" + target + "/index names " + name + ", which isn't there");
          }
          out.add(Arguments.of(target + " " + name.trim(), in.readAllBytes(), t));
        }
      }
    }
  }

  /**
   * scripts/java_fuzz.sh: write target {@code args[0]}'s seeds and the inputs it found into the
   * corpus directory {@code args[1]}.
   */
  public static void main(String[] args) throws IOException {
    java.nio.file.Path dir = java.nio.file.Path.of(args[1]);
    int n = 0;
    for (Arguments a : inputs().filter(a -> ((String) a.get()[0]).startsWith(args[0] + " ")).toList()) {
      java.nio.file.Files.write(dir.resolve("input-" + n++), (byte[]) a.get()[1]);
    }
  }

  @ParameterizedTest(name = "{0}")
  @MethodSource("inputs")
  void stillPasses(String name, byte[] data, Target target) throws Exception {
    target.run(data);
  }
}
