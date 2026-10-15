// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// The file operations the client carries out for its core: a save's (a new temporary file,
// written, synced, renamed over the state file, its directory synced) and a download's file's
// (its directory made, the file made new, written, synced; deleted on failure or discard).
//
// They go through FileOps, whose default is the real file system: with the core tap on, it is
// wrapped by the journal, which records each operation in the tap once it has succeeded
// (the tap checker's file records: kind 5, a JSON object each); tests replace it to
// inject faults (TEST_FILE_OPS). Everything here is off the event loop (fs/promises), but for
// clearing the downloads at start.
//
// A state file's work (saves, downloads' files) is counted for the whole process: a client
// started on a state file waits for whatever a client before it left under way there (a save
// still writing, a cancelled download deleting its file) before it reads the file (known bugs S3, S4).

import { rmSync } from "node:fs";
import { type FileHandle, mkdir, open, rename, unlink } from "node:fs/promises";
import { dirname, relative, resolve, sep } from "node:path";

/** Tests only: a client Config carrying this key carries out its file operations with these
 * FileOps (beneath the journal), as a disk that fails or is slow. files.js is not a package
 * export, so an application can't reach it. */
export const TEST_FILE_OPS = Symbol("opamp test file ops");

/** A file being written, made new by FileOps.create. */
export interface OpenFile {
  readonly path: string;
  /** Append some of `bufs`, in order: how many bytes went (maybe fewer than all, never none). */
  write(bufs: Uint8Array[]): Promise<number>;
  /** Flush its data to the disk. */
  sync(): Promise<void>;
  close(): Promise<void>;
}

/** The file system, as saves and downloads use it. */
export interface FileOps {
  /** A new file, made exclusively (never one already there), the agent's user's only. */
  create(path: string): Promise<OpenFile>;
  /** Rename `from` over `to`. */
  rename(from: string, to: string): Promise<void>;
  /** Flush directory `dir` (its entries reach the disk): false where that can't be done
   * (Windows). */
  syncDir(dir: string): Promise<boolean>;
  /** Delete a file: whether there was one. */
  remove(path: string): Promise<boolean>;
  /** Make a directory and its parents, the agent's user's only: whether it made one. */
  mkdir(dir: string): Promise<boolean>;
  /** Delete a directory and everything under it (at start: synchronously). */
  clear(dir: string): void;
}

class RealFile implements OpenFile {
  readonly path: string;
  readonly #h: FileHandle;

  constructor(path: string, h: FileHandle) {
    this.path = path;
    this.#h = h;
  }

  async write(bufs: Uint8Array[]): Promise<number> {
    const { bytesWritten } = await this.#h.writev(bufs);
    if (bytesWritten === 0 && bufs.some((b) => b.length > 0)) {
      throw new Error("the file took none of the bytes written to it");
    }
    return bytesWritten;
  }

  sync(): Promise<void> {
    return this.#h.sync();
  }

  close(): Promise<void> {
    return this.#h.close();
  }
}

/** The real file system. */
export const REAL: FileOps = {
  async create(path) {
    return new RealFile(path, await open(path, "wx", 0o600));
  },
  rename,
  async syncDir(dir) {
    // Windows can't open a directory to flush it.
    if (process.platform === "win32") return false;
    const h = await open(dir, "r");
    try {
      await h.sync();
    } finally {
      await h.close();
    }
    return true;
  },
  async remove(path) {
    try {
      await unlink(path);
      return true;
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") return false;
      throw err;
    }
  },
  async mkdir(dir) {
    return (await mkdir(dir, { recursive: true, mode: 0o700 })) !== undefined;
  },
  clear(dir) {
    rmSync(dir, { recursive: true, force: true });
  },
};

/** Write all of `data` at the end of `file` (a short write is followed by the rest). */
export async function writeAll(file: OpenFile, data: Uint8Array): Promise<void> {
  for (let rest = data; rest.length > 0; ) {
    const n = await file.write([rest]);
    rest = rest.subarray(n);
  }
}

/** The state file's temporary file. */
export const tempOf = (stateFile: string): string => `${stateFile}.tmp`;

/** Replace what is saved at `path` with `data`, all or nothing, even across a crash: a new file,
 * written, synced, renamed over the old one, and the rename flushed (its directory synced). A
 * failure at any step fails the save, even once the rename is done: then it may have landed, but
 * isn't known to be on the disk. The temporary file is deleted when the save fails before its
 * rename. The recipe is docs/state-file.md's. */
export async function save(ops: FileOps, path: string, data: Uint8Array): Promise<void> {
  const tmp = tempOf(path);
  let file: OpenFile | undefined;
  let made = false;
  try {
    // A new file, never one left behind (by a crash: anyone's, with any mode).
    await ops.remove(tmp);
    file = await ops.create(tmp);
    made = true;
    await writeAll(file, data);
    await file.sync();
    const f = file;
    file = undefined;
    await f.close();
    await ops.rename(tmp, path);
    made = false;
    // The rename itself must reach the disk (the current directory for a bare name).
    await ops.syncDir(dirname(path));
  } catch (err) {
    await file?.close().catch(() => {});
    if (made) await ops.remove(tmp).catch(() => {});
    throw new Error(`saving ${path}: ${(err as Error).message}`);
  }
}

/** One file record (the tap checker's file records). */
export type FileRecord =
  | { op: "state" | "create" | "open" | "sync" | "syncdir" | "remove" | "mkdir" | "clear"; path: string }
  | { op: "write"; path: string; data: string }
  | { op: "write"; path: string; len: number }
  | { op: "rename"; from: string; to: string };

/** `ops`, each operation recorded once it succeeded by `record`, its paths relative to the state
 * file's directory (parts joined with `/`, the directory itself `.`). What is written to the
 * state file's temporary file is recorded whole (hex); to a download's file, only how much. */
export function journal(ops: FileOps, stateFile: string, record: (r: FileRecord) => void): FileOps {
  const base = dirname(resolve(stateFile));
  const tmp = resolve(tempOf(stateFile));
  const rel = (p: string): string => relative(base, resolve(p)).split(sep).join("/") || ".";
  return {
    async create(path) {
      const inner = await ops.create(path);
      const p = rel(path);
      record({ op: "create", path: p });
      const whole = resolve(path) === tmp;
      return {
        path,
        async write(bufs) {
          const n = await inner.write(bufs);
          if (whole) {
            const out: string[] = [];
            let left = n;
            for (const b of bufs) {
              if (left <= 0) break;
              out.push(Buffer.from(b.buffer, b.byteOffset, Math.min(left, b.length)).toString("hex"));
              left -= b.length;
            }
            record({ op: "write", path: p, data: out.join("") });
          } else {
            record({ op: "write", path: p, len: n });
          }
          return n;
        },
        async sync() {
          await inner.sync();
          record({ op: "sync", path: p });
        },
        close: () => inner.close(),
      };
    },
    async rename(from, to) {
      await ops.rename(from, to);
      record({ op: "rename", from: rel(from), to: rel(to) });
    },
    async syncDir(dir) {
      const flushed = await ops.syncDir(dir);
      if (flushed) record({ op: "syncdir", path: rel(dir) });
      return flushed;
    },
    async remove(path) {
      const was = await ops.remove(path);
      if (was) record({ op: "remove", path: rel(path) });
      return was;
    },
    async mkdir(dir) {
      const made = await ops.mkdir(dir);
      if (made) record({ op: "mkdir", path: rel(dir) });
      return made;
    },
    clear(dir) {
      ops.clear(dir);
      record({ op: "clear", path: rel(dir) });
    },
  };
}

/** The work under way on each state file's files, in this process. */
const busy = new Map<string, Set<Promise<unknown>>>();

/** Count `work` (which never rejects) as under way on `stateFile`'s files until it ends. */
export function track(stateFile: string, work: Promise<unknown>): void {
  const key = resolve(stateFile);
  let set = busy.get(key);
  if (!set) busy.set(key, (set = new Set()));
  const mine = set;
  mine.add(work);
  void work.then(
    () => untrack(key, mine, work),
    () => untrack(key, mine, work),
  );
}

function untrack(key: string, set: Set<Promise<unknown>>, work: Promise<unknown>): void {
  set.delete(work);
  if (set.size === 0 && busy.get(key) === set) busy.delete(key);
}

/** Whether work is under way on `stateFile`'s files. */
export function busyOn(stateFile: string): boolean {
  return busy.has(resolve(stateFile));
}

/** Once no work is under way on `stateFile`'s files. */
export async function settled(stateFile: string): Promise<void> {
  for (let set = busy.get(resolve(stateFile)); set && set.size > 0; set = busy.get(resolve(stateFile))) {
    await Promise.allSettled([...set]);
  }
}
