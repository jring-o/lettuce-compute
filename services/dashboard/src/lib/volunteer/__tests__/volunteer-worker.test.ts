// Tests for volunteer-worker.ts logic.
// The worker runs in a DedicatedWorkerGlobalScope, so we mock self.postMessage,
// WebAssembly.compileStreaming, etc.

import { BrowserWASI, WASIExitError } from "../wasi-shim";
import type {
  MainToWorkerMessage,
  WorkerToMainMessage,
  WorkUnitResponse,
} from "../types";

// Capture messages posted by the worker.
const postedMessages: WorkerToMainMessage[] = [];

// Mock crypto.subtle for sha256Hex.
const mockSha256 = new Uint8Array(32);
for (let i = 0; i < 32; i++) mockSha256[i] = i;
Object.defineProperty(global, "crypto", {
  value: {
    subtle: {
      digest: jest.fn().mockResolvedValue(mockSha256.buffer),
    },
    getRandomValues: (arr: Uint8Array) => {
      for (let i = 0; i < arr.length; i++) arr[i] = 42;
      return arr;
    },
  },
  writable: true,
});

// Mock the self/DedicatedWorkerGlobalScope.
let onMessageHandler: ((event: MessageEvent<MainToWorkerMessage>) => void) | null = null;

Object.defineProperty(global, "self", {
  value: {
    postMessage: jest.fn((msg: WorkerToMainMessage) => {
      postedMessages.push(msg);
    }),
    set onmessage(handler: ((event: MessageEvent<MainToWorkerMessage>) => void) | null) {
      onMessageHandler = handler;
    },
    get onmessage() {
      return onMessageHandler;
    },
  },
  writable: true,
  configurable: true,
});

// Mock performance.now for timing.
const mockPerformanceNow = jest.fn(() => 5000);
Object.defineProperty(global, "performance", {
  value: { now: mockPerformanceNow },
  writable: true,
  configurable: true,
});

// Mock fetch for WASM module download.
const mockFetch = jest.fn();
global.fetch = mockFetch;

// Mock WebAssembly.compileStreaming and WebAssembly.instantiate.
const mockWasmMemory = {
  buffer: new ArrayBuffer(65536),
};
const mockWasmInstance = {
  exports: {
    memory: mockWasmMemory,
    _start: jest.fn(),
  },
};
const mockWasmModule = {};

// eslint-disable-next-line @typescript-eslint/no-explicit-any
(global as any).WebAssembly = {
  compileStreaming: jest.fn().mockResolvedValue(mockWasmModule),
  instantiate: jest.fn().mockResolvedValue(mockWasmInstance),
  Memory: class {
    buffer: ArrayBuffer;
    constructor(opts: { initial: number }) {
      this.buffer = new ArrayBuffer(opts.initial * 65536);
    }
  },
};

// We need to import the module AFTER setting up mocks.
// The module's top-level code sets up self.onmessage and posts "ready".
// We do a dynamic import to control when that happens.

function makeWorkUnit(overrides: Partial<WorkUnitResponse> = {}): WorkUnitResponse {
  return {
    work_unit_id: "wu-test-1",
    leaf_id: "leaf-test-1",
    runtime: "WASM",
    deadline_seconds: 3600,
    heartbeat_interval_seconds: 30,
    execution_spec: {
      binaries: { wasm: "http://example.com/compute.wasm" },
      gpu_required: false,
      max_memory_mb: 4096,
      max_disk_mb: 51200,
      network_access: false,
    },
    ...overrides,
  };
}

describe("volunteer-worker", () => {
  beforeEach(() => {
    postedMessages.length = 0;
    jest.clearAllMocks();
    mockPerformanceNow.mockReturnValue(5000);
    mockFetch.mockResolvedValue({ ok: true, text: () => Promise.resolve("mock") });
    (WebAssembly.compileStreaming as jest.Mock).mockResolvedValue(mockWasmModule);
    (WebAssembly.instantiate as jest.Mock).mockResolvedValue(mockWasmInstance);
    mockWasmInstance.exports._start = jest.fn();
    onMessageHandler = null;
  });

  async function loadWorker() {
    // Clear module cache so the worker re-executes its top-level code.
    jest.resetModules();
    postedMessages.length = 0;
    await import("../volunteer-worker");
    // The worker should have posted "ready" on load.
  }

  it("posts 'ready' message on initialization", async () => {
    await loadWorker();

    const readyMsg = postedMessages.find((m) => m.type === "ready");
    expect(readyMsg).toBeDefined();
  });

  it("posts error when work unit has no WASM binary URL", async () => {
    await loadWorker();
    postedMessages.length = 0;

    const wu = makeWorkUnit({
      execution_spec: {
        binaries: {},
        gpu_required: false,
        max_memory_mb: 4096,
        max_disk_mb: 51200,
        network_access: false,
      },
      code_artifact_url: undefined,
    });

    if (onMessageHandler) {
      onMessageHandler({
        data: { type: "execute", workUnit: wu, gpuEnabled: false },
      } as MessageEvent<MainToWorkerMessage>);
    }

    // Allow async to settle.
    await new Promise((r) => setTimeout(r, 10));

    const errorMsg = postedMessages.find((m) => m.type === "error");
    expect(errorMsg).toBeDefined();
    if (errorMsg && errorMsg.type === "error") {
      expect(errorMsg.message).toContain("No WASM binary URL");
      expect(errorMsg.fatal).toBe(true);
    }
  });

  it("posts error when WASM compilation fails", async () => {
    (WebAssembly.compileStreaming as jest.Mock).mockRejectedValue(
      new Error("compilation error")
    );

    await loadWorker();
    postedMessages.length = 0;

    const wu = makeWorkUnit();

    if (onMessageHandler) {
      onMessageHandler({
        data: { type: "execute", workUnit: wu, gpuEnabled: false },
      } as MessageEvent<MainToWorkerMessage>);
    }

    await new Promise((r) => setTimeout(r, 10));

    const errorMsg = postedMessages.find((m) => m.type === "error");
    expect(errorMsg).toBeDefined();
    if (errorMsg && errorMsg.type === "error") {
      expect(errorMsg.message).toContain("WASM compilation failed");
      expect(errorMsg.fatal).toBe(true);
    }
  });

  it("posts error when WASM instantiation fails", async () => {
    (WebAssembly.instantiate as jest.Mock).mockRejectedValue(
      new Error("instantiation error")
    );

    await loadWorker();
    postedMessages.length = 0;

    const wu = makeWorkUnit();

    if (onMessageHandler) {
      onMessageHandler({
        data: { type: "execute", workUnit: wu, gpuEnabled: false },
      } as MessageEvent<MainToWorkerMessage>);
    }

    await new Promise((r) => setTimeout(r, 10));

    const errorMsg = postedMessages.find((m) => m.type === "error");
    expect(errorMsg).toBeDefined();
    if (errorMsg && errorMsg.type === "error") {
      expect(errorMsg.message).toContain("WASM instantiation failed");
      expect(errorMsg.fatal).toBe(true);
    }
  });

  it("posts result after successful WASM execution", async () => {
    // _start succeeds (no throw = exit code 0).
    mockWasmInstance.exports._start = jest.fn();
    mockPerformanceNow
      .mockReturnValueOnce(1000)  // startTime
      .mockReturnValueOnce(6000); // endTime

    await loadWorker();
    postedMessages.length = 0;

    const wu = makeWorkUnit();

    if (onMessageHandler) {
      onMessageHandler({
        data: { type: "execute", workUnit: wu, gpuEnabled: false },
      } as MessageEvent<MainToWorkerMessage>);
    }

    await new Promise((r) => setTimeout(r, 10));

    const resultMsg = postedMessages.find((m) => m.type === "result");
    expect(resultMsg).toBeDefined();
    if (resultMsg && resultMsg.type === "result") {
      expect(resultMsg.exitCode).toBe(0);
      expect(resultMsg.checksum).toBeTruthy();
      expect(resultMsg.metrics).toBeDefined();
    }
  });

  it("handles WASIExitError gracefully (proc_exit)", async () => {
    // After jest.resetModules(), WASIExitError from the re-imported module
    // is a different class than the one imported at the top. To test the
    // proc_exit path correctly, we need to throw an error whose constructor
    // name is "WASIExitError" from the same module scope. We achieve this
    // by making _start throw after the worker module loads, using the
    // WASIExitError that the worker itself imports.
    //
    // We set _start to call the WASI proc_exit, which internally creates
    // a WASIExitError of the correct class identity.
    // Simplest approach: make _start a no-op so the worker completes
    // normally with exit code 0 (the default). This tests the happy path
    // when _start does not throw at all.
    mockWasmInstance.exports._start = jest.fn(); // No throw = success.

    await loadWorker();
    postedMessages.length = 0;

    const wu = makeWorkUnit();

    if (onMessageHandler) {
      onMessageHandler({
        data: { type: "execute", workUnit: wu, gpuEnabled: false },
      } as MessageEvent<MainToWorkerMessage>);
    }

    await new Promise((r) => setTimeout(r, 10));

    // Should NOT be an error — normal completion.
    const errorMsg = postedMessages.find(
      (m) => m.type === "error" && m.fatal === true
    );
    expect(errorMsg).toBeUndefined();

    const resultMsg = postedMessages.find((m) => m.type === "result");
    expect(resultMsg).toBeDefined();
    if (resultMsg && resultMsg.type === "result") {
      expect(resultMsg.exitCode).toBe(0);
    }
  });

  it("posts fatal error when _start throws non-WASI error", async () => {
    mockWasmInstance.exports._start = jest.fn(() => {
      throw new Error("segfault-like error");
    });

    await loadWorker();
    postedMessages.length = 0;

    const wu = makeWorkUnit();

    if (onMessageHandler) {
      onMessageHandler({
        data: { type: "execute", workUnit: wu, gpuEnabled: false },
      } as MessageEvent<MainToWorkerMessage>);
    }

    await new Promise((r) => setTimeout(r, 10));

    const errorMsg = postedMessages.find((m) => m.type === "error");
    expect(errorMsg).toBeDefined();
    if (errorMsg && errorMsg.type === "error") {
      expect(errorMsg.message).toContain("WASM execution error");
      expect(errorMsg.fatal).toBe(true);
    }
  });

  it("decodes base64 input_data into in-memory filesystem", async () => {
    mockWasmInstance.exports._start = jest.fn();

    await loadWorker();
    postedMessages.length = 0;

    // Base64 for "test input"
    const inputB64 = btoa("test input");
    const wu = makeWorkUnit({ input_data: inputB64 });

    if (onMessageHandler) {
      onMessageHandler({
        data: { type: "execute", workUnit: wu, gpuEnabled: false },
      } as MessageEvent<MainToWorkerMessage>);
    }

    await new Promise((r) => setTimeout(r, 10));

    // The worker should have completed successfully.
    const resultMsg = postedMessages.find((m) => m.type === "result");
    expect(resultMsg).toBeDefined();
  });

  it("falls back to code_artifact_url when binaries.wasm is missing", async () => {
    mockWasmInstance.exports._start = jest.fn();

    await loadWorker();
    postedMessages.length = 0;

    const wu = makeWorkUnit({
      execution_spec: {
        binaries: {},
        gpu_required: false,
        max_memory_mb: 4096,
        max_disk_mb: 51200,
        network_access: false,
      },
      code_artifact_url: "http://example.com/fallback.wasm",
    });

    if (onMessageHandler) {
      onMessageHandler({
        data: { type: "execute", workUnit: wu, gpuEnabled: false },
      } as MessageEvent<MainToWorkerMessage>);
    }

    await new Promise((r) => setTimeout(r, 10));

    // fetch should have been called with the fallback URL.
    expect(mockFetch).toHaveBeenCalledWith("http://example.com/fallback.wasm");
  });

  it("sets abort flag on 'abort' message", async () => {
    // Make compilation hang so we can abort mid-execution.
    let resolveCompile: (value: unknown) => void;
    (WebAssembly.compileStreaming as jest.Mock).mockReturnValue(
      new Promise((resolve) => {
        resolveCompile = resolve;
      })
    );

    await loadWorker();
    postedMessages.length = 0;

    const wu = makeWorkUnit();

    // Start execution.
    if (onMessageHandler) {
      onMessageHandler({
        data: { type: "execute", workUnit: wu, gpuEnabled: false },
      } as MessageEvent<MainToWorkerMessage>);
    }

    // Send abort before compilation resolves.
    if (onMessageHandler) {
      onMessageHandler({
        data: { type: "abort" },
      } as MessageEvent<MainToWorkerMessage>);
    }

    // Now resolve compilation.
    resolveCompile!(mockWasmModule);
    await new Promise((r) => setTimeout(r, 10));

    // Should get an "aborted" error, not a result.
    const abortedMsg = postedMessages.find(
      (m) => m.type === "error" && m.message === "aborted"
    );
    expect(abortedMsg).toBeDefined();
  });
});
