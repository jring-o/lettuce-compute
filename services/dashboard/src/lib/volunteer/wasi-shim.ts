// WASI preview 1 shim for browser execution of WASM modules.
// Provides in-memory filesystem, stdin/stdout capture, env vars, args, clock, and random.

const ERRNO_SUCCESS = 0;
const ERRNO_BADF = 8;
const ERRNO_NOSYS = 52;

const CLOCKID_REALTIME = 0;
const CLOCKID_MONOTONIC = 1;

interface InMemoryFile {
  data: Uint8Array;  // backing buffer (may be larger than logical content)
  length: number;    // logical content size (may be < data.byteLength due to growth strategy)
  offset: number;    // current read/write cursor
}

export interface InMemoryDirectory {
  [path: string]: Uint8Array;
}

export interface BrowserWASIOptions {
  args?: string[];
  env?: Record<string, string>;
  stdin?: Uint8Array;
  preopens?: Record<string, InMemoryDirectory>;
}

export class BrowserWASI {
  private memory: WebAssembly.Memory | null = null;
  private stdout: Uint8Array[] = [];
  private stderr: Uint8Array[] = [];
  private stdinData: Uint8Array;
  private stdinOffset = 0;
  private args: string[];
  private envVars: Record<string, string>;
  private exitCode = 0;
  private exited = false;

  // In-memory filesystem: fd -> file.
  // fd 0 = stdin, 1 = stdout, 2 = stderr, 3 = preopened dir.
  // Files opened via path_open start at fd 4.
  private nextFd = 4;
  private openFiles = new Map<number, InMemoryFile>();
  private fileSystem = new Map<string, Uint8Array>();
  private preopenPaths: string[] = [];

  constructor(options: BrowserWASIOptions = {}) {
    this.args = options.args ?? [];
    this.envVars = options.env ?? {};
    this.stdinData = options.stdin ?? new Uint8Array(0);

    if (options.preopens) {
      for (const [dirPath, files] of Object.entries(options.preopens)) {
        this.preopenPaths.push(dirPath);
        for (const [filePath, data] of Object.entries(files)) {
          // Store with full path: /work/input.dat
          const fullPath = `${dirPath}/${filePath}`.replace(/\/+/g, "/");
          this.fileSystem.set(fullPath, data);
        }
      }
    }
  }

  setMemory(memory: WebAssembly.Memory): void {
    this.memory = memory;
  }

  getStdout(): Uint8Array {
    return concatBuffers(this.stdout);
  }

  getStderr(): Uint8Array {
    return concatBuffers(this.stderr);
  }

  getFileContents(path: string): Uint8Array | null {
    // Check open file descriptors first — WASM modules that exit via proc_exit
    // may not have explicitly closed their files.
    for (const [fd, file] of this.openFiles) {
      const fdPath = this.fdPaths.get(fd);
      if (fdPath === path) {
        return file.data.subarray(0, file.length);
      }
    }
    return this.fileSystem.get(path) ?? null;
  }

  getExitCode(): number {
    return this.exitCode;
  }

  hasExited(): boolean {
    return this.exited;
  }

  getImports(): WebAssembly.Imports {
    return {
      wasi_snapshot_preview1: {
        fd_write: (
          fd: number,
          iovs: number,
          iovsLen: number,
          nwritten: number
        ): number => {
          const view = this.dataView();
          const mem = this.memBytes();
          let totalWritten = 0;

          for (let i = 0; i < iovsLen; i++) {
            const ptr = view.getUint32(iovs + i * 8, true);
            const len = view.getUint32(iovs + i * 8 + 4, true);
            const chunk = mem.slice(ptr, ptr + len);

            if (fd === 1) {
              this.stdout.push(chunk);
            } else if (fd === 2) {
              this.stderr.push(chunk);
            } else if (this.openFiles.has(fd)) {
              const file = this.openFiles.get(fd)!;
              const needed = file.offset + len;
              if (needed > file.data.byteLength) {
                // Amortized doubling avoids O(N^2) copies for incremental writes.
                const newCap = Math.max(needed, file.data.byteLength * 2, 1024);
                const newData = new Uint8Array(newCap);
                newData.set(file.data.subarray(0, file.length));
                file.data = newData;
              }
              file.data.set(chunk, file.offset);
              file.offset += len;
              if (file.offset > file.length) {
                file.length = file.offset;
              }
            } else {
              return ERRNO_BADF;
            }
            totalWritten += len;
          }

          view.setUint32(nwritten, totalWritten, true);
          return ERRNO_SUCCESS;
        },

        fd_read: (
          fd: number,
          iovs: number,
          iovsLen: number,
          nread: number
        ): number => {
          const view = this.dataView();
          const mem = this.memBytes();
          let totalRead = 0;

          for (let i = 0; i < iovsLen; i++) {
            const ptr = view.getUint32(iovs + i * 8, true);
            const len = view.getUint32(iovs + i * 8 + 4, true);

            if (fd === 0) {
              const remaining = this.stdinData.length - this.stdinOffset;
              const toRead = Math.min(len, remaining);
              mem.set(
                this.stdinData.subarray(
                  this.stdinOffset,
                  this.stdinOffset + toRead
                ),
                ptr
              );
              this.stdinOffset += toRead;
              totalRead += toRead;
              if (toRead < len) break;
            } else if (this.openFiles.has(fd)) {
              const file = this.openFiles.get(fd)!;
              const remaining = file.length - file.offset;
              const toRead = Math.min(len, remaining);
              mem.set(
                file.data.subarray(file.offset, file.offset + toRead),
                ptr
              );
              file.offset += toRead;
              totalRead += toRead;
              if (toRead < len) break;
            } else {
              return ERRNO_BADF;
            }
          }

          view.setUint32(nread, totalRead, true);
          return ERRNO_SUCCESS;
        },

        environ_get: (environ: number, environBuf: number): number => {
          const view = this.dataView();
          const mem = this.memBytes();
          const entries = Object.entries(this.envVars);
          let bufOffset = environBuf;

          for (let i = 0; i < entries.length; i++) {
            view.setUint32(environ + i * 4, bufOffset, true);
            const str = `${entries[i][0]}=${entries[i][1]}\0`;
            const encoded = new TextEncoder().encode(str);
            mem.set(encoded, bufOffset);
            bufOffset += encoded.length;
          }

          return ERRNO_SUCCESS;
        },

        environ_sizes_get: (count: number, bufSize: number): number => {
          const view = this.dataView();
          const entries = Object.entries(this.envVars);
          let totalSize = 0;
          for (const [key, val] of entries) {
            totalSize += new TextEncoder().encode(`${key}=${val}\0`).length;
          }
          view.setUint32(count, entries.length, true);
          view.setUint32(bufSize, totalSize, true);
          return ERRNO_SUCCESS;
        },

        args_get: (argv: number, argvBuf: number): number => {
          const view = this.dataView();
          const mem = this.memBytes();
          let bufOffset = argvBuf;

          for (let i = 0; i < this.args.length; i++) {
            view.setUint32(argv + i * 4, bufOffset, true);
            const str = `${this.args[i]}\0`;
            const encoded = new TextEncoder().encode(str);
            mem.set(encoded, bufOffset);
            bufOffset += encoded.length;
          }

          return ERRNO_SUCCESS;
        },

        args_sizes_get: (argc: number, argvBufSize: number): number => {
          const view = this.dataView();
          let totalSize = 0;
          for (const arg of this.args) {
            totalSize += new TextEncoder().encode(`${arg}\0`).length;
          }
          view.setUint32(argc, this.args.length, true);
          view.setUint32(argvBufSize, totalSize, true);
          return ERRNO_SUCCESS;
        },

        clock_time_get: (
          id: number,
          _precision: bigint,
          time: number
        ): number => {
          const view = this.dataView();
          let nanos: bigint;
          if (id === CLOCKID_REALTIME) {
            nanos = BigInt(Math.floor(Date.now() * 1_000_000));
          } else if (id === CLOCKID_MONOTONIC) {
            nanos = BigInt(
              Math.floor(performance.now() * 1_000_000)
            );
          } else {
            return ERRNO_NOSYS;
          }
          view.setBigUint64(time, nanos, true);
          return ERRNO_SUCCESS;
        },

        proc_exit: (code: number): void => {
          this.exitCode = code;
          this.exited = true;
          throw new WASIExitError(code);
        },

        random_get: (buf: number, bufLen: number): number => {
          const mem = this.memBytes();
          const random = new Uint8Array(bufLen);
          crypto.getRandomValues(random);
          mem.set(random, buf);
          return ERRNO_SUCCESS;
        },

        // In-memory filesystem support for preopened directories.
        fd_prestat_get: (fd: number, buf: number): number => {
          const preopenIndex = fd - 3;
          if (preopenIndex < 0 || preopenIndex >= this.preopenPaths.length) {
            return ERRNO_BADF;
          }
          const view = this.dataView();
          // Type 0 = directory.
          view.setUint8(buf, 0);
          const pathLen = new TextEncoder().encode(
            this.preopenPaths[preopenIndex]
          ).length;
          view.setUint32(buf + 4, pathLen, true);
          return ERRNO_SUCCESS;
        },

        fd_prestat_dir_name: (
          fd: number,
          path: number,
          pathLen: number
        ): number => {
          const preopenIndex = fd - 3;
          if (preopenIndex < 0 || preopenIndex >= this.preopenPaths.length) {
            return ERRNO_BADF;
          }
          const mem = this.memBytes();
          const encoded = new TextEncoder().encode(
            this.preopenPaths[preopenIndex]
          );
          mem.set(encoded.subarray(0, pathLen), path);
          return ERRNO_SUCCESS;
        },

        path_open: (
          dirFd: number,
          _dirflags: number,
          pathPtr: number,
          pathLen: number,
          _oflags: number,
          _fsRightsBase: bigint,
          _fsRightsInheriting: bigint,
          _fdflags: number,
          fdOut: number
        ): number => {
          const preopenIndex = dirFd - 3;
          if (preopenIndex < 0 || preopenIndex >= this.preopenPaths.length) {
            return ERRNO_BADF;
          }

          const mem = this.memBytes();
          const view = this.dataView();
          const relativePath = new TextDecoder().decode(
            mem.subarray(pathPtr, pathPtr + pathLen)
          );

          // Prevent directory traversal: reject paths containing ".." components.
          // A malicious WASM module could use "../" to escape the preopened directory.
          const segments = relativePath.split("/");
          if (segments.some((s) => s === "..")) {
            return 76; // ERRNO_NOTCAPABLE — path escapes sandbox
          }

          const dirPath = this.preopenPaths[preopenIndex];
          const fullPath = `${dirPath}/${relativePath}`.replace(/\/+/g, "/");

          // Open existing file or create new one.
          const fd = this.nextFd++;
          const existingData = this.fileSystem.get(fullPath);
          const initData = existingData ? new Uint8Array(existingData) : new Uint8Array(0);
          const file: InMemoryFile = {
            data: initData,
            length: initData.byteLength,
            offset: 0,
          };
          this.openFiles.set(fd, file);
          // Track the path for this fd.
          this.fdPaths.set(fd, fullPath);

          // If creating new, register in filesystem.
          if (!existingData) {
            this.fileSystem.set(fullPath, file.data);
          }

          view.setUint32(fdOut, fd, true);
          return ERRNO_SUCCESS;
        },

        fd_close: (fd: number): number => {
          if (fd <= 2) return ERRNO_SUCCESS;
          // Sync file data back to filesystem before closing, trimming to logical length.
          const path = this.fdPaths.get(fd);
          const file = this.openFiles.get(fd);
          if (path && file) {
            this.fileSystem.set(path, file.data.subarray(0, file.length));
          }
          this.openFiles.delete(fd);
          this.fdPaths.delete(fd);
          return ERRNO_SUCCESS;
        },

        fd_seek: (_fd: number, _offset: bigint, _whence: number, _newOffset: number): number => {
          return ERRNO_NOSYS;
        },

        fd_fdstat_get: (fd: number, buf: number): number => {
          const view = this.dataView();
          if (fd <= 2) {
            // Character device.
            view.setUint8(buf, 2);
            view.setUint16(buf + 2, 0, true);
            view.setBigUint64(buf + 8, 0n, true);
            view.setBigUint64(buf + 16, 0n, true);
            return ERRNO_SUCCESS;
          }
          if (fd >= 3 && fd - 3 < this.preopenPaths.length) {
            // Directory.
            view.setUint8(buf, 3);
            view.setUint16(buf + 2, 0, true);
            view.setBigUint64(buf + 8, BigInt((1 << 28) - 1), true);
            view.setBigUint64(buf + 16, BigInt((1 << 28) - 1), true);
            return ERRNO_SUCCESS;
          }
          if (this.openFiles.has(fd)) {
            // Regular file.
            view.setUint8(buf, 4);
            view.setUint16(buf + 2, 0, true);
            view.setBigUint64(buf + 8, BigInt((1 << 28) - 1), true);
            view.setBigUint64(buf + 16, 0n, true);
            return ERRNO_SUCCESS;
          }
          return ERRNO_BADF;
        },

        // Stubs.
        path_create_directory: (): number => ERRNO_NOSYS,
        path_remove_directory: (): number => ERRNO_NOSYS,
        path_rename: (): number => ERRNO_NOSYS,
        path_filestat_get: (): number => ERRNO_NOSYS,
        fd_filestat_get: (): number => ERRNO_NOSYS,
        path_unlink_file: (): number => ERRNO_NOSYS,
        path_readlink: (): number => ERRNO_NOSYS,
        path_symlink: (): number => ERRNO_NOSYS,
        fd_readdir: (): number => ERRNO_NOSYS,
        fd_fdstat_set_flags: (): number => ERRNO_NOSYS,
        fd_advise: (): number => ERRNO_NOSYS,
        fd_allocate: (): number => ERRNO_NOSYS,
        fd_datasync: (): number => ERRNO_NOSYS,
        fd_sync: (): number => ERRNO_NOSYS,
        fd_tell: (): number => ERRNO_NOSYS,
        fd_filestat_set_size: (): number => ERRNO_NOSYS,
        fd_filestat_set_times: (): number => ERRNO_NOSYS,
        fd_pread: (): number => ERRNO_NOSYS,
        fd_pwrite: (): number => ERRNO_NOSYS,
        fd_renumber: (): number => ERRNO_NOSYS,
        path_filestat_set_times: (): number => ERRNO_NOSYS,
        poll_oneoff: (): number => ERRNO_NOSYS,
        sched_yield: (): number => ERRNO_SUCCESS,
        sock_accept: (): number => ERRNO_NOSYS,
        sock_recv: (): number => ERRNO_NOSYS,
        sock_send: (): number => ERRNO_NOSYS,
        sock_shutdown: (): number => ERRNO_NOSYS,
      },
    };
  }

  // Internal helpers.
  private fdPaths = new Map<number, string>();

  private dataView(): DataView {
    return new DataView(this.memory!.buffer);
  }

  private memBytes(): Uint8Array {
    return new Uint8Array(this.memory!.buffer);
  }
}

export class WASIExitError extends Error {
  exitCode: number;
  constructor(code: number) {
    super(`WASI exit with code ${code}`);
    this.name = "WASIExitError";
    this.exitCode = code;
  }
}

function concatBuffers(buffers: Uint8Array[]): Uint8Array {
  const totalLength = buffers.reduce((sum, buf) => sum + buf.length, 0);
  const result = new Uint8Array(totalLength);
  let offset = 0;
  for (const buf of buffers) {
    result.set(buf, offset);
    offset += buf.length;
  }
  return result;
}
