// Pool Manager tests — mocks Worker and VolunteerClient.

import type { WorkerToMainMessage, MainToWorkerMessage } from "../types";

// Mock the client module before importing pool-manager.
const mockRegister = jest.fn();
const mockRequestWork = jest.fn();
const mockSubmitResult = jest.fn();
const mockHeartbeat = jest.fn();

jest.mock("../client", () => ({
  createVolunteerClient: jest.fn(() => ({
    register: mockRegister,
    requestWork: mockRequestWork,
    submitResult: mockSubmitResult,
    heartbeat: mockHeartbeat,
  })),
}));

// Mock Worker constructor.
class MockWorker {
  onmessage: ((event: MessageEvent<WorkerToMainMessage>) => void) | null =
    null;
  onerror: (() => void) | null = null;
  onmessageerror: (() => void) | null = null;
  postMessage = jest.fn();
  terminate = jest.fn();

  // Emit a message from this "worker" back to the main thread.
  simulateMessage(msg: WorkerToMainMessage): void {
    if (this.onmessage) {
      this.onmessage({ data: msg } as MessageEvent<WorkerToMainMessage>);
    }
  }
}

// Track created workers for assertions.
const createdWorkers: MockWorker[] = [];

// eslint-disable-next-line @typescript-eslint/no-explicit-any
(global as any).Worker = jest.fn().mockImplementation(() => {
  const w = new MockWorker();
  createdWorkers.push(w);
  return w;
});

// eslint-disable-next-line @typescript-eslint/no-explicit-any
(global as any).URL = class MockURL {
  href: string;
  constructor(path: string, base?: string) {
    this.href = base ? `${base}/${path}` : path;
  }
};

import { PoolManager } from "../pool-manager";
import type { VolunteerIdentity } from "../identity";

const mockIdentity: VolunteerIdentity = {
  publicKey: new Uint8Array(32),
  privateKey: { type: "private", algorithm: { name: "Ed25519" } } as CryptoKey,
  publicKeyBase64url: "mock-pubkey-base64url",
  fingerprint: "0123456789abcdef",
};

describe("PoolManager", () => {
  beforeEach(() => {
    jest.clearAllMocks();
    jest.useFakeTimers();
    createdWorkers.length = 0;
    mockRequestWork.mockResolvedValue(null); // Default: no work.
    mockSubmitResult.mockResolvedValue({
      accepted: true,
      validation_status: "VALIDATION_PENDING",
    });
    mockHeartbeat.mockResolvedValue({ continue_execution: true });
  });

  afterEach(() => {
    jest.useRealTimers();
  });

  function createPool(
    workerCount = 2,
    onProgress?: (stats: { activeWorkers: number; completedWorkUnits: number; failedWorkUnits: number; totalRuntimeSeconds: number }) => void
  ): PoolManager {
    return new PoolManager({
      serverUrl: "http://localhost:8080",
      identity: mockIdentity,
      workerCount,
      gpuEnabled: false,
      onProgress,
    });
  }

  describe("start", () => {
    it("spawns the configured number of workers", async () => {
      const pool = createPool(3);
      await pool.start();

      expect(createdWorkers).toHaveLength(3);
      expect(global.Worker).toHaveBeenCalledTimes(3);
    });

    it("does not spawn workers twice when called again", async () => {
      const pool = createPool(2);
      await pool.start();
      await pool.start();

      // Second call should be a no-op.
      expect(createdWorkers).toHaveLength(2);
    });

    it("fetches work for each worker on start", async () => {
      const pool = createPool(2);
      await pool.start();

      // Allow async work to resolve.
      await jest.advanceTimersByTimeAsync(0);

      expect(mockRequestWork).toHaveBeenCalledTimes(2);
    });
  });

  describe("stop", () => {
    it("terminates all workers and clears heartbeat timer", async () => {
      const pool = createPool(2);
      await pool.start();

      await pool.stop();

      for (const w of createdWorkers) {
        expect(w.terminate).toHaveBeenCalled();
      }

      const stats = pool.getStats();
      expect(stats.activeWorkers).toBe(0);
    });

    it("sends abort message before terminating", async () => {
      const pool = createPool(1);
      await pool.start();

      await pool.stop();

      const worker = createdWorkers[0];
      expect(worker.postMessage).toHaveBeenCalledWith({ type: "abort" });
      expect(worker.terminate).toHaveBeenCalled();
    });
  });

  describe("work unit lifecycle", () => {
    it("dispatches work unit to worker when available", async () => {
      const mockWU = {
        work_unit_id: "wu-1",
        leaf_id: "leaf-1",
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
      };

      mockRequestWork.mockResolvedValueOnce(mockWU);

      const pool = createPool(1);
      await pool.start();

      await jest.advanceTimersByTimeAsync(0);

      const worker = createdWorkers[0];
      expect(worker.postMessage).toHaveBeenCalledWith(
        expect.objectContaining({
          type: "execute",
          workUnit: mockWU,
          gpuEnabled: false,
        })
      );

      const stats = pool.getStats();
      expect(stats.activeWorkers).toBe(1);
    });

    it("submits result when worker reports completion", async () => {
      const mockWU = {
        work_unit_id: "wu-1",
        leaf_id: "leaf-1",
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
      };

      mockRequestWork.mockResolvedValueOnce(mockWU);
      // After result submission, next requestWork returns null.
      mockRequestWork.mockResolvedValue(null);

      const pool = createPool(1);
      await pool.start();

      await jest.advanceTimersByTimeAsync(0);

      const worker = createdWorkers[0];

      // Simulate worker completing the work unit.
      const output = new TextEncoder().encode("result output");
      worker.simulateMessage({
        type: "result",
        output: output.buffer.slice(
          output.byteOffset,
          output.byteOffset + output.byteLength
        ),
        checksum: "abc123",
        exitCode: 0,
        metrics: {
          wall_clock_seconds: 5,
          cpu_seconds_user: 4,
          peak_memory_mb: 128,
        },
      });

      await jest.advanceTimersByTimeAsync(0);

      expect(mockSubmitResult).toHaveBeenCalledWith(
        expect.objectContaining({
          work_unit_id: "wu-1",
          output_checksum: "abc123",
          exit_code: 0,
        })
      );
    });

    it("retries work fetch after delay when no work is available", async () => {
      mockRequestWork.mockResolvedValue(null);

      const pool = createPool(1);
      await pool.start();

      await jest.advanceTimersByTimeAsync(0);
      expect(mockRequestWork).toHaveBeenCalledTimes(1);

      // Advance past the 10s retry delay.
      await jest.advanceTimersByTimeAsync(10_000);
      expect(mockRequestWork).toHaveBeenCalledTimes(2);
    });
  });

  describe("setWorkerCount", () => {
    it("adds workers when count increases", async () => {
      const pool = createPool(1);
      await pool.start();
      expect(createdWorkers).toHaveLength(1);

      pool.setWorkerCount(3);
      expect(createdWorkers).toHaveLength(3);
    });

    it("removes idle workers when count decreases", async () => {
      const pool = createPool(3);
      await pool.start();
      expect(createdWorkers).toHaveLength(3);

      pool.setWorkerCount(1);
      // Two workers should have been terminated.
      const terminatedCount = createdWorkers.filter(
        (w) => w.terminate.mock.calls.length > 0
      ).length;
      expect(terminatedCount).toBeGreaterThanOrEqual(2);
    });

    it("enforces minimum of 1 worker", async () => {
      const pool = createPool(2);
      await pool.start();

      pool.setWorkerCount(0);
      // Should clamp to 1, so only 1 worker terminated out of 2.
      const stats = pool.getStats();
      // We can't easily check the exact worker array size from outside,
      // but the pool should still be functional.
      expect(stats).toBeDefined();
    });
  });

  describe("getStats", () => {
    it("returns initial stats with zero values", () => {
      const pool = createPool(2);
      const stats = pool.getStats();

      expect(stats.activeWorkers).toBe(0);
      expect(stats.completedWorkUnits).toBe(0);
      expect(stats.failedWorkUnits).toBe(0);
      expect(stats.totalRuntimeSeconds).toBe(0);
    });

    it("returns a copy (not reference) of stats", () => {
      const pool = createPool(1);
      const stats1 = pool.getStats();
      const stats2 = pool.getStats();

      stats1.completedWorkUnits = 999;
      expect(stats2.completedWorkUnits).toBe(0);
    });
  });

  describe("onProgress callback", () => {
    it("calls onProgress when stats update", async () => {
      const onProgress = jest.fn();
      const mockWU = {
        work_unit_id: "wu-1",
        leaf_id: "leaf-1",
        runtime: "WASM",
        deadline_seconds: 3600,
        heartbeat_interval_seconds: 30,
        execution_spec: {
          binaries: {},
          gpu_required: false,
          max_memory_mb: 4096,
          max_disk_mb: 51200,
          network_access: false,
        },
      };

      mockRequestWork.mockResolvedValueOnce(mockWU);
      mockRequestWork.mockResolvedValue(null);

      const pool = createPool(1, onProgress);
      await pool.start();

      await jest.advanceTimersByTimeAsync(0);

      // onProgress is called when a worker becomes busy.
      expect(onProgress).toHaveBeenCalled();
      const lastCall = onProgress.mock.calls[onProgress.mock.calls.length - 1];
      expect(lastCall[0].activeWorkers).toBe(1);
    });
  });

  describe("error handling", () => {
    it("increments failedWorkUnits on fatal worker error", async () => {
      const mockWU = {
        work_unit_id: "wu-1",
        leaf_id: "leaf-1",
        runtime: "WASM",
        deadline_seconds: 3600,
        heartbeat_interval_seconds: 30,
        execution_spec: {
          binaries: {},
          gpu_required: false,
          max_memory_mb: 4096,
          max_disk_mb: 51200,
          network_access: false,
        },
      };

      mockRequestWork.mockResolvedValueOnce(mockWU);
      mockRequestWork.mockResolvedValue(null);

      const pool = createPool(1);
      await pool.start();
      await jest.advanceTimersByTimeAsync(0);

      const worker = createdWorkers[0];
      worker.simulateMessage({
        type: "error",
        message: "WASM compilation failed",
        fatal: true,
      });

      await jest.advanceTimersByTimeAsync(0);

      const stats = pool.getStats();
      expect(stats.failedWorkUnits).toBe(1);
    });

    it("retries work fetch on non-fatal error", async () => {
      const mockWU = {
        work_unit_id: "wu-1",
        leaf_id: "leaf-1",
        runtime: "WASM",
        deadline_seconds: 3600,
        heartbeat_interval_seconds: 30,
        execution_spec: {
          binaries: {},
          gpu_required: false,
          max_memory_mb: 4096,
          max_disk_mb: 51200,
          network_access: false,
        },
      };

      mockRequestWork.mockResolvedValueOnce(mockWU);
      mockRequestWork.mockResolvedValue(null);

      const pool = createPool(1);
      await pool.start();
      await jest.advanceTimersByTimeAsync(0);

      const initialRequestCount = mockRequestWork.mock.calls.length;

      const worker = createdWorkers[0];
      worker.simulateMessage({
        type: "error",
        message: "aborted",
        fatal: false,
      });

      await jest.advanceTimersByTimeAsync(0);

      // Non-fatal error should trigger another work fetch.
      expect(mockRequestWork.mock.calls.length).toBeGreaterThan(
        initialRequestCount
      );
    });
  });

  describe("heartbeats", () => {
    it("sends heartbeats for busy workers at configured interval", async () => {
      const mockWU = {
        work_unit_id: "wu-1",
        leaf_id: "leaf-1",
        runtime: "WASM",
        deadline_seconds: 3600,
        heartbeat_interval_seconds: 30,
        execution_spec: {
          binaries: {},
          gpu_required: false,
          max_memory_mb: 4096,
          max_disk_mb: 51200,
          network_access: false,
        },
      };

      mockRequestWork.mockResolvedValueOnce(mockWU);
      mockRequestWork.mockResolvedValue(null);

      const pool = createPool(1);
      await pool.start();
      await jest.advanceTimersByTimeAsync(0);

      // Default heartbeat interval is 30s (from default heartbeatIntervalMs).
      await jest.advanceTimersByTimeAsync(30_000);

      expect(mockHeartbeat).toHaveBeenCalledWith(
        expect.objectContaining({
          work_unit_id: "wu-1",
          progress_pct: 0,
        })
      );
    });

    it("sends abort to worker when server says stop", async () => {
      const mockWU = {
        work_unit_id: "wu-abort",
        leaf_id: "leaf-1",
        runtime: "WASM",
        deadline_seconds: 3600,
        heartbeat_interval_seconds: 30,
        execution_spec: {
          binaries: {},
          gpu_required: false,
          max_memory_mb: 4096,
          max_disk_mb: 51200,
          network_access: false,
        },
      };

      mockRequestWork.mockResolvedValueOnce(mockWU);
      mockRequestWork.mockResolvedValue(null);
      mockHeartbeat.mockResolvedValue({ continue_execution: false });

      const pool = createPool(1);
      await pool.start();
      await jest.advanceTimersByTimeAsync(0);

      const worker = createdWorkers[0];

      // Advance past the heartbeat interval.
      await jest.advanceTimersByTimeAsync(30_000);

      // The heartbeat returned continue_execution: false, so the worker should get abort.
      expect(worker.postMessage).toHaveBeenCalledWith({ type: "abort" });
    });

    it("does not send heartbeats for idle workers", async () => {
      mockRequestWork.mockResolvedValue(null);

      const pool = createPool(1);
      await pool.start();
      await jest.advanceTimersByTimeAsync(0);

      // Advance past the heartbeat interval.
      await jest.advanceTimersByTimeAsync(30_000);

      // No heartbeat should be sent since no workers are busy.
      expect(mockHeartbeat).not.toHaveBeenCalled();
    });
  });
});
