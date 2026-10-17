// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Download throughput: a local OpAMP server (plain HTTP) offers one package whose file is
// BENCH_MB megabytes, served with Content-Length or chunked; the client downloads it in the mode
// asked for. The time is from the server's first byte of the file to the installPackage
// notification (the file written, hashed and checked).
//
//   node bench/download.mjs rust cl [record.bin]      # Rust mode, Content-Length
//   node bench/download.mjs node chunked              # Node mode (undici), chunked
//   BENCH_SPLICE=0 node bench/download.mjs rust cl     # Rust mode, every byte through the core
//
// With a third argument, every init and step the module gets is recorded there (length-prefixed),
// to replay under another runtime (java/bench).

import { createHash, randomBytes } from "node:crypto";
import { closeSync, mkdtempSync, openSync, rmSync, writeSync } from "node:fs";
import http from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";

const [mode = "rust", framing = "cl", record] = process.argv.slice(2);
const MB = Number(process.env.BENCH_MB ?? 100);

if (record) {
  // Record what goes into the module: wrap init and step to copy their input out of its memory.
  const fd = openSync(record, "w");
  const Real = WebAssembly.Instance;
  WebAssembly.Instance = function (module, imports) {
    const real = new Real(module, imports);
    const x = real.exports;
    const wrap = (tag, fn) => (ptr, len) => {
      const head = Buffer.alloc(5);
      head.writeUInt8(tag, 0);
      head.writeUInt32LE(len, 1);
      writeSync(fd, head);
      writeSync(fd, new Uint8Array(x.memory.buffer, ptr, len));
      return fn(ptr, len);
    };
    return { exports: { ...x, init: wrap(0, x.init), step: wrap(1, x.step) } };
  };
  process.on("exit", () => closeSync(fd));
}

const { Client, create, toBinary, ServerToAgentSchema, PackageType } = await import("../dist/src/index.js");

// The file: random bytes (so nothing can compress it), repeated from one 1 MiB block.
const block = randomBytes(1 << 20);
const hash = createHash("sha256");
for (let i = 0; i < MB; i++) hash.update(block);
const digest = new Uint8Array(hash.digest());

let started = 0;
const server = http.createServer((req, res) => {
  if (req.method === "GET") {
    started = performance.now();
    res.writeHead(200, framing === "cl" ? { "Content-Length": String(MB << 20) } : {});
    let i = 0;
    const more = () => {
      while (i < MB) {
        i++;
        if (!res.write(block)) return void res.once("drain", more);
      }
      res.end();
    };
    more();
    return;
  }
  req.resume();
  req.on("end", () => {
    const { port } = server.address();
    const msg = create(ServerToAgentSchema, {
      packagesAvailable: {
        allPackagesHash: new Uint8Array([1]),
        packages: {
          big: {
            type: PackageType.PackageType_TopLevel,
            version: "1",
            hash: new Uint8Array([2]),
            file: { downloadUrl: `http://127.0.0.1:${port}/file`, contentHash: digest },
          },
        },
      },
    });
    res.writeHead(200, { "Content-Type": "application/x-protobuf" });
    res.end(Buffer.from(toBinary(ServerToAgentSchema, msg)));
  });
});
await new Promise((r) => server.listen(0, "127.0.0.1", r));

const dir = mkdtempSync(join(tmpdir(), "opamp-bench-"));
const done = new Promise((resolve, reject) => {
  const client = Client.start(
    {
      endpoint: `http://127.0.0.1:${server.address().port}/v1/opamp`,
      stateFile: join(dir, "state"),
      acceptPackages: true,
      heartbeatIntervalMs: 60_000,
      mode,
      rustSplice: process.env.BENCH_SPLICE !== "0",
      proxy: false,
    },
    {},
    (n) => {
      if (n.kind === "installPackage") {
        const ms = performance.now() - started;
        resolve(ms);
        void client.stop();
      } else if (n.kind === "stopped" && n.error) {
        reject(n.error);
      }
    },
  );
});
const ms = await done;
console.log(JSON.stringify({ mode, splice: mode === "rust" && process.env.BENCH_SPLICE !== "0", framing, mb: MB, ms: Math.round(ms), mbPerS: Math.round((MB / ms) * 1000 * 10) / 10 }));
server.close();
rmSync(dir, { recursive: true, force: true });
process.exit(0);
