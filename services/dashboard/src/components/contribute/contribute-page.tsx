"use client";

import { useCallback, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Switch } from "@/components/ui/switch";
import { Badge } from "@/components/ui/badge";
import { HardwarePanel, type HardwareInfo } from "./hardware-panel";
import { LeafSelector } from "./leaf-selector";
import { StatusPanel } from "./status-panel";
import { ActivityLog, type LogEntry } from "./activity-log";
import { getOrCreateIdentity, createNewIdentity, deleteIdentity } from "@/lib/volunteer/identity";
import type { VolunteerIdentity } from "@/lib/volunteer/identity";
import { createVolunteerClient } from "@/lib/volunteer/client";
import { PoolManager, type PoolStats } from "@/lib/volunteer/pool-manager";

type ContributeStatus =
  | "idle"
  | "initializing"
  | "registering"
  | "running"
  | "stopping";

const MAX_LOG_ENTRIES = 50;

const serverUrl =
  typeof window !== "undefined"
    ? (process.env.NEXT_PUBLIC_INFRASTRUCTURE_URL || window.location.origin)
    : "";

export function ContributePage() {
  const [status, setStatus] = useState<ContributeStatus>("idle");
  const [identity, setIdentity] = useState<VolunteerIdentity | null>(null);
  const [hardware, setHardware] = useState<HardwareInfo | null>(null);
  const [selectedLeafIds, setSelectedLeafIds] = useState<Set<string>>(
    new Set()
  );
  const [workerCount, setWorkerCount] = useState(0);
  const [gpuEnabled, setGpuEnabled] = useState(true);
  const [poolStats, setPoolStats] = useState<PoolStats>({
    activeWorkers: 0,
    completedWorkUnits: 0,
    failedWorkUnits: 0,
    totalRuntimeSeconds: 0,
  });
  const [logEntries, setLogEntries] = useState<LogEntry[]>([]);
  const [identityError, setIdentityError] = useState<string | null>(null);
  const [confirmRegenerate, setConfirmRegenerate] = useState(false);

  const poolRef = useRef<PoolManager | null>(null);
  const logIdRef = useRef(0);

  const addLog = useCallback(
    (message: string, level: LogEntry["level"] = "info") => {
      setLogEntries((prev) => {
        const entry: LogEntry = {
          id: ++logIdRef.current,
          timestamp: new Date(),
          message,
          level,
        };
        return [entry, ...prev].slice(0, MAX_LOG_ENTRIES);
      });
    },
    []
  );

  const handleHardwareDetected = useCallback(
    (info: HardwareInfo) => {
      setHardware(info);
      const defaultWorkers =
        info.cpuCores < 4
          ? 1
          : Math.ceil(info.cpuCores / 2);
      setWorkerCount(defaultWorkers);

      // Load identity on hardware detection.
      getOrCreateIdentity()
        .then((id) => setIdentity(id))
        .catch(() => setIdentityError("Could not access identity storage (IndexedDB unavailable)."));
    },
    []
  );

  async function handleRegenerateIdentity() {
    if (!confirmRegenerate) {
      setConfirmRegenerate(true);
      return;
    }
    setConfirmRegenerate(false);
    try {
      await deleteIdentity();
      const newId = await createNewIdentity();
      setIdentity(newId);
      addLog("Identity regenerated", "info");
    } catch {
      setIdentityError("Failed to regenerate identity.");
    }
  }

  async function handleStart() {
    if (selectedLeafIds.size === 0 || !hardware) return;

    try {
      setStatus("initializing");
      addLog("Initializing...");

      let id = identity;
      if (!id) {
        id = await getOrCreateIdentity();
        setIdentity(id);
      }

      setStatus("registering");
      addLog("Registering with server...");

      const client = createVolunteerClient(serverUrl, id);
      await client.register({
        cpu_cores: hardware.cpuCores,
        memory_mb: (hardware.memoryGB ?? 4) * 1024,
        has_gpu: hardware.webgpuAvailable && gpuEnabled,
        gpu_vendors:
          hardware.webgpuAvailable && gpuEnabled ? ["WEBGPU"] : [],
        available_runtimes: ["WASM"],
      });

      addLog("Registered successfully", "success");
      setStatus("running");
      addLog(`Starting ${workerCount} worker(s)...`);

      const pool = new PoolManager({
        serverUrl,
        identity: id,
        workerCount,
        leafIds: Array.from(selectedLeafIds),
        gpuEnabled: hardware.webgpuAvailable && gpuEnabled,
        onProgress: (stats) => {
          setPoolStats(stats);
        },
      });

      poolRef.current = pool;
      await pool.start();
      addLog(`${workerCount} worker(s) started`, "success");
    } catch (err) {
      const msg =
        err instanceof Error ? err.message : "Failed to start";
      addLog(msg, "error");
      setStatus("idle");
    }
  }

  async function handleStop() {
    setStatus("stopping");
    addLog("Stopping workers...");

    if (poolRef.current) {
      await poolRef.current.stop();
      poolRef.current = null;
    }

    setPoolStats((prev) => ({
      ...prev,
      activeWorkers: 0,
    }));
    setStatus("idle");
    addLog("Stopped", "info");
  }

  function handleWorkerCountChange(e: React.ChangeEvent<HTMLInputElement>) {
    const count = parseInt(e.target.value, 10);
    setWorkerCount(count);
    if (poolRef.current && status === "running") {
      poolRef.current.setWorkerCount(count);
      addLog(`Worker count adjusted to ${count}`);
    }
  }

  const canStart =
    status === "idle" &&
    selectedLeafIds.size > 0 &&
    !identityError;
  const isRunning = status === "running";
  const isBusy =
    status === "initializing" ||
    status === "registering" ||
    status === "stopping";

  const showGpuToggle =
    hardware?.webgpuAvailable &&
    selectedLeafIds.size > 0;

  const maxWorkers = hardware?.cpuCores ?? 4;

  return (
    <div className="mx-auto max-w-4xl px-4 py-8 sm:px-6 lg:px-8">
      {/* Header */}
      <div className="mb-8">
        <h1 className="text-2xl font-bold tracking-tight">
          Contribute Compute
        </h1>
        <p className="mt-2 text-sm text-muted-foreground">
          Donate spare compute from your browser — no installation required.
          Your browser runs WASM computations for active research leafs on this
          server.
        </p>
      </div>

      <div className="grid gap-6 md:grid-cols-2">
        {/* Left column */}
        <div className="space-y-6">
          <HardwarePanel onDetected={handleHardwareDetected} />

          {/* Identity */}
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Identity</CardTitle>
            </CardHeader>
            <CardContent>
              {identityError ? (
                <p className="text-sm text-destructive">{identityError}</p>
              ) : identity ? (
                <div className="space-y-2">
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-muted-foreground">
                      Fingerprint
                    </span>
                    <Badge variant="secondary" className="font-mono text-xs">
                      {identity.fingerprint}
                    </Badge>
                  </div>
                  <div>
                    {confirmRegenerate ? (
                      <div className="space-y-2">
                        <p className="text-xs text-destructive">
                          Regenerating your identity will discard your credit
                          history. Continue?
                        </p>
                        <div className="flex gap-2">
                          <Button
                            variant="destructive"
                            size="sm"
                            onClick={handleRegenerateIdentity}
                            disabled={isBusy || isRunning}
                          >
                            Confirm
                          </Button>
                          <Button
                            variant="outline"
                            size="sm"
                            onClick={() => setConfirmRegenerate(false)}
                          >
                            Cancel
                          </Button>
                        </div>
                      </div>
                    ) : (
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={handleRegenerateIdentity}
                        disabled={isBusy || isRunning}
                      >
                        New Identity
                      </Button>
                    )}
                  </div>
                </div>
              ) : (
                <p className="text-sm text-muted-foreground">Loading...</p>
              )}
            </CardContent>
          </Card>

          <LeafSelector
            serverUrl={serverUrl}
            selectedIds={selectedLeafIds}
            onSelectionChange={setSelectedLeafIds}
            disabled={isRunning || isBusy}
          />
        </div>

        {/* Right column */}
        <div className="space-y-6">
          {/* Controls */}
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Controls</CardTitle>
            </CardHeader>
            <CardContent className="space-y-4">
              {/* Worker count slider */}
              <div>
                <label
                  htmlFor="worker-count"
                  className="mb-1 block text-sm text-muted-foreground"
                >
                  Workers: {workerCount}
                </label>
                <input
                  id="worker-count"
                  type="range"
                  min={1}
                  max={maxWorkers}
                  value={workerCount}
                  onChange={handleWorkerCountChange}
                  className="w-full accent-primary"
                />
                <div className="flex justify-between text-xs text-muted-foreground">
                  <span>1</span>
                  <span>{maxWorkers}</span>
                </div>
              </div>

              {/* GPU toggle */}
              {showGpuToggle && (
                <div className="flex items-center justify-between">
                  <span className="text-sm text-muted-foreground">
                    WebGPU Compute
                  </span>
                  <Switch
                    checked={gpuEnabled}
                    onCheckedChange={setGpuEnabled}
                    disabled={isBusy}
                    aria-label="Toggle WebGPU compute"
                  />
                </div>
              )}

              {/* Start/Stop button */}
              {isRunning || status === "stopping" ? (
                <Button
                  variant="destructive"
                  size="lg"
                  className="w-full"
                  onClick={handleStop}
                  disabled={status === "stopping"}
                >
                  {status === "stopping"
                    ? "Stopping..."
                    : "Stop Contributing"}
                </Button>
              ) : (
                <Button
                  size="lg"
                  className="w-full"
                  onClick={handleStart}
                  disabled={!canStart || isBusy}
                >
                  {isBusy
                    ? status === "initializing"
                      ? "Initializing..."
                      : "Registering..."
                    : "Start Contributing"}
                </Button>
              )}
            </CardContent>
          </Card>

          <StatusPanel
            stats={poolStats}
            workerCount={workerCount}
            running={isRunning}
          />

          <ActivityLog entries={logEntries} />
        </div>
      </div>
    </div>
  );
}
