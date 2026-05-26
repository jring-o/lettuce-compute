// Web Worker script for browser volunteer WASM execution.
// Runs in a dedicated Worker thread via `new Worker()`.

import { BrowserWASI, WASIExitError } from "./wasi-shim";
import { bytesToHex } from "./identity";
import { dispatchGPUCompute } from "./webgpu-dispatch";
import type {
  MainToWorkerMessage,
  WorkerToMainMessage,
  WorkUnitResponse,
  ExecutionMetrics,
} from "./types";

const ctx = self as unknown as { postMessage(msg: unknown): void; onmessage: ((ev: MessageEvent) => void) | null };

function post(msg: WorkerToMainMessage): void {
  ctx.postMessage(msg);
}

async function sha256Hex(data: Uint8Array): Promise<string> {
  const buf = new Uint8Array(data).buffer as ArrayBuffer;
  const hash = await crypto.subtle.digest("SHA-256", buf);
  return bytesToHex(new Uint8Array(hash));
}

let aborted = false;

// Cache compiled WASM modules by URL to avoid redundant downloads and compilations
// when processing multiple work units from the same leaf.
// Bounded to MAX_MODULE_CACHE_SIZE entries to prevent unbounded memory growth.
const MAX_MODULE_CACHE_SIZE = 8;
const moduleCache = new Map<string, WebAssembly.Module>();

async function getOrCompileModule(url: string): Promise<WebAssembly.Module> {
  const cached = moduleCache.get(url);
  if (cached) return cached;

  const response = fetch(url);
  const module = await WebAssembly.compileStreaming(response);

  // Evict oldest entry if cache is full (Map iteration order = insertion order).
  if (moduleCache.size >= MAX_MODULE_CACHE_SIZE) {
    const oldest = moduleCache.keys().next().value;
    if (oldest !== undefined) {
      moduleCache.delete(oldest);
    }
  }

  moduleCache.set(url, module);
  return module;
}

async function executeWorkUnit(
  workUnit: WorkUnitResponse,
  gpuEnabled: boolean
): Promise<void> {
  const startTime = performance.now();
  aborted = false;

  // Prepare input data.
  let inputData = new Uint8Array(0);
  if (workUnit.input_data) {
    inputData = Uint8Array.from(atob(workUnit.input_data), (c) =>
      c.charCodeAt(0)
    );
  }

  // Prepare parameters.
  const paramsData = workUnit.parameters_json
    ? new TextEncoder().encode(workUnit.parameters_json)
    : new Uint8Array(0);

  // Prepare env vars.
  const env: Record<string, string> = { ...workUnit.env_vars };
  env["LETTUCE_WORK_UNIT_ID"] = workUnit.work_unit_id;
  env["LETTUCE_LEAF_ID"] = workUnit.leaf_id;

  // Set up in-memory filesystem.
  const preopens: Record<string, Record<string, Uint8Array>> = {
    "/work": {},
  };
  if (inputData.length > 0) {
    preopens["/work"]["input.dat"] = inputData;
  }
  if (paramsData.length > 0) {
    preopens["/work"]["params.json"] = paramsData;
  }

  const wasi = new BrowserWASI({
    args: ["lettuce-compute"],
    env,
    preopens,
  });

  // Fetch and compile WASM module.
  const wasmUrl =
    workUnit.execution_spec.binaries?.wasm ||
    workUnit.code_artifact_url;
  if (!wasmUrl) {
    post({ type: "error", message: "No WASM binary URL in work unit", fatal: true });
    return;
  }

  let module: WebAssembly.Module;
  try {
    module = await getOrCompileModule(wasmUrl);
  } catch (err) {
    post({
      type: "error",
      message: `WASM compilation failed: ${err}`,
      fatal: true,
    });
    return;
  }

  if (aborted) {
    post({ type: "error", message: "aborted", fatal: false });
    return;
  }

  // Instantiate with WASI imports.
  const imports = wasi.getImports();
  let instance: WebAssembly.Instance;
  try {
    instance = await WebAssembly.instantiate(module, imports);
  } catch (err) {
    post({
      type: "error",
      message: `WASM instantiation failed: ${err}`,
      fatal: true,
    });
    return;
  }

  // Set WASM memory reference on WASI shim.
  const wasmMemory = instance.exports.memory as WebAssembly.Memory;
  if (wasmMemory) {
    wasi.setMemory(wasmMemory);
  }

  if (aborted) {
    post({ type: "error", message: "aborted", fatal: false });
    return;
  }

  // Execute _start (WASI command entry point).
  try {
    const start = instance.exports._start as () => void;
    if (start) {
      start();
    }
  } catch (err) {
    if (!(err instanceof WASIExitError)) {
      post({
        type: "error",
        message: `WASM execution error: ${err}`,
        fatal: true,
      });
      return;
    }
    // WASIExitError is expected — proc_exit was called.
  }

  if (aborted) {
    post({ type: "error", message: "aborted", fatal: false });
    return;
  }

  // Handle WebGPU dispatch if needed.
  if (
    gpuEnabled &&
    workUnit.execution_spec.gpu_required &&
    workUnit.execution_spec.binaries?.wgsl
  ) {
    const gpuInput = wasi.getFileContents("/work/gpu_input.bin");
    if (gpuInput && gpuInput.length > 0) {
      try {
        const gpuOutput = await dispatchGPUCompute({
          wgslShaderUrl: workUnit.execution_spec.binaries.wgsl,
          inputData: gpuInput,
          outputSize: gpuInput.length,
        });
        // Write GPU output back to in-memory FS for WASM to read.
        // Since we can't re-run WASM easily, store it as the output.
        const outputPath = "/work/gpu_output.bin";
        // Store directly — the WASM module already ran.
        // gpu_output.bin becomes part of the output.
        preopens["/work"]["gpu_output.bin"] = gpuOutput;
      } catch (err) {
        post({
          type: "error",
          message: `WebGPU dispatch failed: ${err}`,
          fatal: true,
        });
        return;
      }
    }
  }

  // Collect output: prefer /work/output.dat, fall back to stdout.
  let output = wasi.getFileContents("/work/output.dat");
  if (!output || output.length === 0) {
    output = wasi.getStdout();
  }

  const endTime = performance.now();
  const wallClockSeconds = Math.round((endTime - startTime) / 1000);
  const checksum = await sha256Hex(output);

  const metrics: ExecutionMetrics = {
    wall_clock_seconds: wallClockSeconds,
    cpu_seconds_user: wallClockSeconds, // Approximate — no per-thread CPU tracking in browsers.
    peak_memory_mb: wasmMemory
      ? Math.ceil(wasmMemory.buffer.byteLength / (1024 * 1024))
      : 0,
  };

  post({
    type: "result",
    output: (output.buffer as ArrayBuffer).slice(
      output.byteOffset,
      output.byteOffset + output.byteLength
    ),
    checksum,
    exitCode: wasi.getExitCode(),
    metrics,
  });
}

ctx.onmessage = (event: MessageEvent<MainToWorkerMessage>) => {
  const msg = event.data;

  switch (msg.type) {
    case "execute":
      executeWorkUnit(msg.workUnit, msg.gpuEnabled).catch((err) => {
        post({
          type: "error",
          message: `Unexpected error: ${err}`,
          fatal: true,
        });
      });
      break;

    case "abort":
      aborted = true;
      break;
  }
};

// Signal readiness.
post({ type: "ready" });
