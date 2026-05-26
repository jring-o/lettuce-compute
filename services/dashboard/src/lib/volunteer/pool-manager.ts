// Worker pool manager for coordinating multiple browser volunteer Web Workers.

import type { VolunteerIdentity } from "./identity";
import { bytesToBase64 } from "./identity";
import { createVolunteerClient } from "./client";
import type { VolunteerClient } from "./client";
import type {
  WorkUnitResponse,
  WorkerToMainMessage,
  MainToWorkerMessage,
} from "./types";

export interface PoolManagerOptions {
  serverUrl: string;
  identity: VolunteerIdentity;
  workerCount: number;
  leafIds?: string[];
  gpuEnabled: boolean;
  onProgress?: (stats: PoolStats) => void;
}

export interface PoolStats {
  activeWorkers: number;
  completedWorkUnits: number;
  failedWorkUnits: number;
  totalRuntimeSeconds: number;
}

interface WorkerState {
  worker: Worker;
  busy: boolean;
  currentWorkUnit: WorkUnitResponse | null;
  startedAt: number | null;
}

export class PoolManager {
  private client: VolunteerClient;
  private workers: WorkerState[] = [];
  private running = false;
  private heartbeatTimer: ReturnType<typeof setInterval> | null = null;
  private heartbeatIntervalMs = 30_000;
  private options: PoolManagerOptions;
  private stats: PoolStats = {
    activeWorkers: 0,
    completedWorkUnits: 0,
    failedWorkUnits: 0,
    totalRuntimeSeconds: 0,
  };

  constructor(options: PoolManagerOptions) {
    this.options = options;
    this.client = createVolunteerClient(options.serverUrl, options.identity);
  }

  async start(): Promise<void> {
    if (this.running) return;
    this.running = true;

    // Spawn workers.
    for (let i = 0; i < this.options.workerCount; i++) {
      this.spawnWorker();
    }

    // Start heartbeat timer.
    this.heartbeatTimer = setInterval(() => {
      this.sendHeartbeats();
    }, this.heartbeatIntervalMs);

    // Kick off initial work fetch for all workers.
    for (const ws of this.workers) {
      this.fetchAndDispatch(ws);
    }
  }

  async stop(): Promise<void> {
    this.running = false;

    if (this.heartbeatTimer) {
      clearInterval(this.heartbeatTimer);
      this.heartbeatTimer = null;
    }

    // Abort and terminate all workers.
    for (const ws of this.workers) {
      const abortMsg: MainToWorkerMessage = { type: "abort" };
      ws.worker.postMessage(abortMsg);
      ws.worker.terminate();
    }
    this.workers = [];
    this.updateStats();
  }

  setWorkerCount(count: number): void {
    const target = Math.max(1, count);

    // Add workers if needed.
    while (this.workers.length < target) {
      const ws = this.spawnWorker();
      if (this.running) {
        this.fetchAndDispatch(ws);
      }
    }

    // Remove idle workers if needed.
    while (this.workers.length > target) {
      const idleIdx = this.workers.findIndex((ws) => !ws.busy);
      const idx = idleIdx >= 0 ? idleIdx : this.workers.length - 1;
      const ws = this.workers[idx];
      if (ws.busy) {
        const abortMsg: MainToWorkerMessage = { type: "abort" };
        ws.worker.postMessage(abortMsg);
      }
      ws.worker.terminate();
      this.workers.splice(idx, 1);
    }

    this.updateStats();
  }

  getStats(): PoolStats {
    return { ...this.stats };
  }

  private spawnWorker(): WorkerState {
    const worker = new Worker(
      new URL("./volunteer-worker.ts", import.meta.url),
      { type: "module" }
    );

    const ws: WorkerState = {
      worker,
      busy: false,
      currentWorkUnit: null,
      startedAt: null,
    };

    worker.onmessage = (event: MessageEvent<WorkerToMainMessage>) => {
      this.handleWorkerMessage(ws, event.data);
    };

    worker.onerror = (e) => {
      console.error("[Lettuce Worker Crash]", e.message || e);
      this.replaceWorker(ws);
    };

    worker.onmessageerror = (e) => {
      console.error("[Lettuce Worker MessageError]", e);
      this.replaceWorker(ws);
    };

    this.workers.push(ws);
    return ws;
  }

  private replaceWorker(ws: WorkerState): void {
    const idx = this.workers.indexOf(ws);
    if (idx === -1) return;

    ws.worker.terminate();
    this.workers.splice(idx, 1);

    if (ws.busy) {
      this.stats.failedWorkUnits++;
    }

    if (this.running) {
      const newWs = this.spawnWorker();
      this.fetchAndDispatch(newWs);
    }
    this.updateStats();
  }

  private handleWorkerMessage(
    ws: WorkerState,
    msg: WorkerToMainMessage
  ): void {
    switch (msg.type) {
      case "ready":
        // Worker initialized, nothing to do — fetchAndDispatch handles it.
        break;

      case "result":
        this.handleResult(ws, msg);
        break;

      case "progress":
        // Progress updates are informational; heartbeats handle server communication.
        break;

      case "error":
        console.error("[Lettuce Worker Error]", msg.message, msg.fatal ? "(fatal)" : "(non-fatal)");
        if (msg.fatal) {
          this.stats.failedWorkUnits++;
        }
        ws.busy = false;
        ws.currentWorkUnit = null;
        ws.startedAt = null;
        this.updateStats();
        if (this.running && !msg.fatal) {
          this.fetchAndDispatch(ws);
        } else if (msg.fatal) {
          this.replaceWorker(ws);
        }
        break;
    }
  }

  private async handleResult(
    ws: WorkerState,
    msg: Extract<WorkerToMainMessage, { type: "result" }>
  ): Promise<void> {
    const wu = ws.currentWorkUnit;
    if (wu) {
      // Submit result to server.
      try {
        const outputBase64 = bytesToBase64(new Uint8Array(msg.output));
        await this.client.submitResult({
          work_unit_id: wu.work_unit_id,
          output_data: outputBase64,
          output_checksum: msg.checksum,
          exit_code: msg.exitCode,
          metrics: msg.metrics,
        });
        this.stats.completedWorkUnits++;
      } catch {
        this.stats.failedWorkUnits++;
      }

      if (ws.startedAt) {
        this.stats.totalRuntimeSeconds += Math.round(
          (performance.now() - ws.startedAt) / 1000
        );
      }
    }

    ws.busy = false;
    ws.currentWorkUnit = null;
    ws.startedAt = null;
    this.updateStats();

    if (this.running) {
      this.fetchAndDispatch(ws);
    }
  }

  private async fetchAndDispatch(ws: WorkerState): Promise<void> {
    if (!this.running || ws.busy) return;

    try {
      const wu = await this.client.requestWork({
        leaf_ids: this.options.leafIds,
        has_gpu: this.options.gpuEnabled,
        gpu_vendors: this.options.gpuEnabled ? ["WEBGPU"] : [],
      });

      if (!wu) {
        // No work available — retry after a delay.
        setTimeout(() => this.fetchAndDispatch(ws), 10_000);
        return;
      }

      // Update heartbeat interval from server response (only reset timer if changed).
      if (wu.heartbeat_interval_seconds > 0) {
        const newInterval = wu.heartbeat_interval_seconds * 1000;
        if (newInterval !== this.heartbeatIntervalMs) {
          this.heartbeatIntervalMs = newInterval;
          this.resetHeartbeatTimer();
        }
      }

      ws.busy = true;
      ws.currentWorkUnit = wu;
      ws.startedAt = performance.now();
      this.updateStats();

      const execMsg: MainToWorkerMessage = {
        type: "execute",
        workUnit: wu,
        gpuEnabled: this.options.gpuEnabled,
      };
      ws.worker.postMessage(execMsg);
    } catch {
      // Fetch failed — retry after a delay.
      setTimeout(() => this.fetchAndDispatch(ws), 10_000);
    }
  }

  private async sendHeartbeats(): Promise<void> {
    // Send all heartbeats concurrently to avoid serial latency accumulation.
    // With N workers and 200-500ms latency each, sequential sends would take
    // N * latency. Promise.allSettled ensures one failure doesn't block others.
    const promises = this.workers
      .filter((ws) => ws.busy && ws.currentWorkUnit)
      .map(async (ws) => {
        const elapsed = ws.startedAt
          ? Math.round((performance.now() - ws.startedAt) / 1000)
          : 0;

        try {
          const resp = await this.client.heartbeat({
            work_unit_id: ws.currentWorkUnit!.work_unit_id,
            progress_pct: 0,
            metrics: { wall_clock_seconds: elapsed },
          });

          if (!resp.continue_execution) {
            // Server says stop — abort this worker's task.
            const abortMsg: MainToWorkerMessage = { type: "abort" };
            ws.worker.postMessage(abortMsg);
          }
        } catch {
          // Heartbeat failed — continue; server will handle timeout.
        }
      });

    await Promise.allSettled(promises);
  }

  private resetHeartbeatTimer(): void {
    if (this.heartbeatTimer) {
      clearInterval(this.heartbeatTimer);
    }
    if (this.running) {
      this.heartbeatTimer = setInterval(() => {
        this.sendHeartbeats();
      }, this.heartbeatIntervalMs);
    }
  }

  private updateStats(): void {
    this.stats.activeWorkers = this.workers.filter((ws) => ws.busy).length;
    this.options.onProgress?.(this.getStats());
  }
}
