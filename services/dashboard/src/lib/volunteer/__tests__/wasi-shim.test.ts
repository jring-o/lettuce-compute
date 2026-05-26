import { BrowserWASI, WASIExitError } from "../wasi-shim";

// Mock crypto.getRandomValues for random_get.
Object.defineProperty(global, "crypto", {
  value: {
    getRandomValues: (arr: Uint8Array) => {
      for (let i = 0; i < arr.length; i++) arr[i] = 42;
      return arr;
    },
  },
  writable: true,
});

// Helper: create a minimal WASM memory and run a WASI function.
function createTestMemory(pages = 1): WebAssembly.Memory {
  return new WebAssembly.Memory({ initial: pages });
}

function getImportFn(
  wasi: BrowserWASI,
  fnName: string
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
): (...args: any[]) => any {
  const imports = wasi.getImports();
  const wasiImports = imports.wasi_snapshot_preview1 as Record<
    string,
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (...args: any[]) => any
  >;
  return wasiImports[fnName];
}

describe("BrowserWASI", () => {
  describe("fd_write to stdout", () => {
    it("captures stdout output", () => {
      const wasi = new BrowserWASI();
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);
      const mem = new Uint8Array(memory.buffer);

      // Write "hello" at offset 256.
      const hello = new TextEncoder().encode("hello");
      mem.set(hello, 256);

      // Set up iov: ptr=256, len=5.
      view.setUint32(0, 256, true);
      view.setUint32(4, 5, true);

      const fdWrite = getImportFn(wasi, "fd_write");
      // fd=1 (stdout), iovs=0, iovsLen=1, nwritten=64.
      const errno = fdWrite(1, 0, 1, 64);
      expect(errno).toBe(0);
      expect(view.getUint32(64, true)).toBe(5);

      const stdout = wasi.getStdout();
      expect(new TextDecoder().decode(stdout)).toBe("hello");
    });

    it("captures stderr separately", () => {
      const wasi = new BrowserWASI();
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);
      const mem = new Uint8Array(memory.buffer);

      const msg = new TextEncoder().encode("error msg");
      mem.set(msg, 256);
      view.setUint32(0, 256, true);
      view.setUint32(4, msg.length, true);

      const fdWrite = getImportFn(wasi, "fd_write");
      fdWrite(2, 0, 1, 64);

      const stderr = wasi.getStderr();
      expect(new TextDecoder().decode(stderr)).toBe("error msg");
      expect(wasi.getStdout().length).toBe(0);
    });
  });

  describe("fd_read from stdin", () => {
    it("reads provided stdin data", () => {
      const stdinContent = new TextEncoder().encode("stdin input data");
      const wasi = new BrowserWASI({ stdin: stdinContent });
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);
      const mem = new Uint8Array(memory.buffer);

      // Set up iov: ptr=256 (where data will be read into), len=100 (max read).
      view.setUint32(0, 256, true);
      view.setUint32(4, 100, true);

      const fdRead = getImportFn(wasi, "fd_read");
      // fd=0 (stdin), iovs=0, iovsLen=1, nread=64.
      const errno = fdRead(0, 0, 1, 64);
      expect(errno).toBe(0);

      const bytesRead = view.getUint32(64, true);
      expect(bytesRead).toBe(stdinContent.length);

      const readBack = new TextDecoder().decode(
        mem.subarray(256, 256 + bytesRead)
      );
      expect(readBack).toBe("stdin input data");
    });

    it("returns 0 bytes when stdin is empty", () => {
      const wasi = new BrowserWASI(); // No stdin provided.
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);

      view.setUint32(0, 256, true);
      view.setUint32(4, 100, true);

      const fdRead = getImportFn(wasi, "fd_read");
      const errno = fdRead(0, 0, 1, 64);
      expect(errno).toBe(0);
      expect(view.getUint32(64, true)).toBe(0);
    });

    it("reads stdin incrementally across multiple calls", () => {
      const stdinContent = new TextEncoder().encode("abcdefghij");
      const wasi = new BrowserWASI({ stdin: stdinContent });
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);
      const mem = new Uint8Array(memory.buffer);
      const fdRead = getImportFn(wasi, "fd_read");

      // First read: request 5 bytes.
      view.setUint32(0, 256, true);
      view.setUint32(4, 5, true);
      fdRead(0, 0, 1, 64);
      expect(view.getUint32(64, true)).toBe(5);
      expect(new TextDecoder().decode(mem.subarray(256, 261))).toBe("abcde");

      // Second read: request 5 more bytes.
      view.setUint32(0, 300, true);
      view.setUint32(4, 5, true);
      fdRead(0, 0, 1, 64);
      expect(view.getUint32(64, true)).toBe(5);
      expect(new TextDecoder().decode(mem.subarray(300, 305))).toBe("fghij");

      // Third read: no data remaining.
      view.setUint32(0, 400, true);
      view.setUint32(4, 100, true);
      fdRead(0, 0, 1, 64);
      expect(view.getUint32(64, true)).toBe(0);
    });
  });

  describe("fd_read from opened file", () => {
    it("reads preopened file data via fd_read", () => {
      const fileData = new TextEncoder().encode("file contents here");
      const wasi = new BrowserWASI({
        preopens: { "/work": { "input.dat": fileData } },
      });
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);
      const mem = new Uint8Array(memory.buffer);

      // Open /work/input.dat via path_open.
      const pathOpen = getImportFn(wasi, "path_open");
      const pathStr = new TextEncoder().encode("input.dat");
      mem.set(pathStr, 100);
      pathOpen(3, 0, 100, pathStr.length, 0, 0n, 0n, 0, 200);
      const fd = view.getUint32(200, true);

      // Read from the opened file.
      view.setUint32(0, 300, true); // iov ptr
      view.setUint32(4, 100, true); // iov len (larger than file)

      const fdRead = getImportFn(wasi, "fd_read");
      const errno = fdRead(fd, 0, 1, 64);
      expect(errno).toBe(0);

      const bytesRead = view.getUint32(64, true);
      expect(bytesRead).toBe(fileData.length);
      expect(
        new TextDecoder().decode(mem.subarray(300, 300 + bytesRead))
      ).toBe("file contents here");
    });

    it("returns EBADF for invalid fd", () => {
      const wasi = new BrowserWASI();
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);
      view.setUint32(0, 256, true);
      view.setUint32(4, 10, true);

      const fdRead = getImportFn(wasi, "fd_read");
      const errno = fdRead(99, 0, 1, 64); // fd 99 does not exist.
      expect(errno).toBe(8); // ERRNO_BADF
    });
  });

  describe("fd_write error cases", () => {
    it("returns EBADF for invalid fd on write", () => {
      const wasi = new BrowserWASI();
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);
      const mem = new Uint8Array(memory.buffer);

      const data = new TextEncoder().encode("data");
      mem.set(data, 256);
      view.setUint32(0, 256, true);
      view.setUint32(4, data.length, true);

      const fdWrite = getImportFn(wasi, "fd_write");
      const errno = fdWrite(99, 0, 1, 64); // fd 99 does not exist.
      expect(errno).toBe(8); // ERRNO_BADF
    });
  });

  describe("fd_prestat_get", () => {
    it("returns preopen info for valid directory fd", () => {
      const wasi = new BrowserWASI({
        preopens: { "/work": {} },
      });
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);
      const fdPrestatGet = getImportFn(wasi, "fd_prestat_get");

      // fd 3 = first preopened dir.
      const errno = fdPrestatGet(3, 0);
      expect(errno).toBe(0);
      expect(view.getUint8(0)).toBe(0); // Type: directory
      const pathLen = view.getUint32(4, true);
      expect(pathLen).toBe(new TextEncoder().encode("/work").length);
    });

    it("returns EBADF for non-preopened fd", () => {
      const wasi = new BrowserWASI({
        preopens: { "/work": {} },
      });
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const fdPrestatGet = getImportFn(wasi, "fd_prestat_get");
      // fd 4 = beyond preopened dirs.
      expect(fdPrestatGet(4, 0)).toBe(8); // ERRNO_BADF
    });
  });

  describe("fd_fdstat_get", () => {
    it("returns character device type for stdin/stdout/stderr", () => {
      const wasi = new BrowserWASI();
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);
      const fdFdstatGet = getImportFn(wasi, "fd_fdstat_get");

      for (const fd of [0, 1, 2]) {
        const errno = fdFdstatGet(fd, 0);
        expect(errno).toBe(0);
        expect(view.getUint8(0)).toBe(2); // Character device
      }
    });

    it("returns directory type for preopened dir fd", () => {
      const wasi = new BrowserWASI({
        preopens: { "/work": {} },
      });
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);
      const fdFdstatGet = getImportFn(wasi, "fd_fdstat_get");

      const errno = fdFdstatGet(3, 0);
      expect(errno).toBe(0);
      expect(view.getUint8(0)).toBe(3); // Directory
    });
  });

  describe("clock_time_get error case", () => {
    it("returns ENOSYS for unsupported clock id", () => {
      const wasi = new BrowserWASI();
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const clockTimeGet = getImportFn(wasi, "clock_time_get");
      const errno = clockTimeGet(99, 0n, 0); // Invalid clock id.
      expect(errno).toBe(52); // ERRNO_NOSYS
    });
  });

  describe("environ_get", () => {
    it("returns configured env vars", () => {
      const wasi = new BrowserWASI({
        env: { FOO: "bar", LETTUCE_ID: "wu-1" },
      });
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);

      // Get sizes first.
      const envSizesGet = getImportFn(wasi, "environ_sizes_get");
      envSizesGet(0, 4);
      const count = view.getUint32(0, true);
      const bufSize = view.getUint32(4, true);

      expect(count).toBe(2);
      expect(bufSize).toBeGreaterThan(0);

      // Get actual env vars.
      const environGet = getImportFn(wasi, "environ_get");
      const environPtrs = 512;
      const environBuf = 1024;
      const errno = environGet(environPtrs, environBuf);
      expect(errno).toBe(0);

      // Read first env var.
      const firstPtr = view.getUint32(environPtrs, true);
      expect(firstPtr).toBe(environBuf);

      const mem = new Uint8Array(memory.buffer);
      // Find the null terminator.
      let end = firstPtr;
      while (mem[end] !== 0 && end < firstPtr + 100) end++;
      const firstVar = new TextDecoder().decode(mem.subarray(firstPtr, end));
      expect(firstVar).toBe("FOO=bar");
    });
  });

  describe("args_get", () => {
    it("returns configured args", () => {
      const wasi = new BrowserWASI({ args: ["program", "--flag", "value"] });
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);

      const argsSizesGet = getImportFn(wasi, "args_sizes_get");
      argsSizesGet(0, 4);
      expect(view.getUint32(0, true)).toBe(3);
    });
  });

  describe("clock_time_get", () => {
    it("returns non-zero realtime value", () => {
      const wasi = new BrowserWASI();
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);
      const clockTimeGet = getImportFn(wasi, "clock_time_get");

      // id=0 (REALTIME), precision=0n, time offset=0.
      const errno = clockTimeGet(0, 0n, 0);
      expect(errno).toBe(0);

      const nanos = view.getBigUint64(0, true);
      expect(nanos).toBeGreaterThan(0n);
    });

    it("returns non-zero monotonic value", () => {
      const wasi = new BrowserWASI();
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);
      const clockTimeGet = getImportFn(wasi, "clock_time_get");

      const errno = clockTimeGet(1, 0n, 0);
      expect(errno).toBe(0);

      const nanos = view.getBigUint64(0, true);
      expect(nanos).toBeGreaterThanOrEqual(0n);
    });
  });

  describe("random_get", () => {
    it("fills buffer with random bytes", () => {
      const wasi = new BrowserWASI();
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const randomGet = getImportFn(wasi, "random_get");
      const errno = randomGet(0, 16);
      expect(errno).toBe(0);

      const mem = new Uint8Array(memory.buffer);
      // Our mock fills with 42.
      expect(mem[0]).toBe(42);
      expect(mem[15]).toBe(42);
    });
  });

  describe("proc_exit", () => {
    it("sets exit code and throws WASIExitError", () => {
      const wasi = new BrowserWASI();
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const procExit = getImportFn(wasi, "proc_exit");

      expect(() => procExit(42)).toThrow(WASIExitError);
      expect(wasi.getExitCode()).toBe(42);
      expect(wasi.hasExited()).toBe(true);
    });

    it("defaults to exit code 0", () => {
      const wasi = new BrowserWASI();
      expect(wasi.getExitCode()).toBe(0);
      expect(wasi.hasExited()).toBe(false);
    });
  });

  describe("in-memory filesystem", () => {
    it("reads preopened files", () => {
      const inputData = new TextEncoder().encode("input content");
      const wasi = new BrowserWASI({
        preopens: { "/work": { "input.dat": inputData } },
      });

      const contents = wasi.getFileContents("/work/input.dat");
      expect(contents).not.toBeNull();
      expect(new TextDecoder().decode(contents!)).toBe("input content");
    });

    it("writes and reads back file content via fd operations", () => {
      const wasi = new BrowserWASI({
        preopens: { "/work": {} },
      });
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const view = new DataView(memory.buffer);
      const mem = new Uint8Array(memory.buffer);

      // Open /work/output.dat via path_open.
      const pathOpen = getImportFn(wasi, "path_open");
      const pathStr = new TextEncoder().encode("output.dat");
      mem.set(pathStr, 100);
      // dirFd=3 (first preopened), path at 100, len=pathStr.length, fdOut at 200.
      const openErrno = pathOpen(3, 0, 100, pathStr.length, 0, 0n, 0n, 0, 200);
      expect(openErrno).toBe(0);

      const fd = view.getUint32(200, true);
      expect(fd).toBeGreaterThanOrEqual(4);

      // Write data to the opened file.
      const writeData = new TextEncoder().encode("result data");
      mem.set(writeData, 300);
      view.setUint32(400, 300, true); // iov ptr
      view.setUint32(404, writeData.length, true); // iov len

      const fdWrite = getImportFn(wasi, "fd_write");
      const writeErrno = fdWrite(fd, 400, 1, 500);
      expect(writeErrno).toBe(0);
      expect(view.getUint32(500, true)).toBe(writeData.length);

      // Close the file.
      const fdClose = getImportFn(wasi, "fd_close");
      fdClose(fd);

      // Read back from filesystem.
      const contents = wasi.getFileContents("/work/output.dat");
      expect(contents).not.toBeNull();
      expect(new TextDecoder().decode(contents!)).toBe("result data");
    });

    it("returns null for non-existent files", () => {
      const wasi = new BrowserWASI({ preopens: { "/work": {} } });
      expect(wasi.getFileContents("/work/missing.dat")).toBeNull();
    });
  });

  describe("stub functions", () => {
    it("returns ENOSYS (52) for unsupported operations", () => {
      const wasi = new BrowserWASI();
      const memory = createTestMemory();
      wasi.setMemory(memory);

      const stubs = [
        "path_create_directory",
        "path_remove_directory",
        "path_rename",
        "path_filestat_get",
        "fd_filestat_get",
        "path_unlink_file",
      ];

      for (const name of stubs) {
        const fn = getImportFn(wasi, name);
        expect(fn()).toBe(52);
      }
    });
  });
});
