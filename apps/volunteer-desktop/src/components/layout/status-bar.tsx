import { useEffect, useState } from "react";
import { getDaemonProcessState, type DaemonProcessState } from "@/api/client";
import { useDaemonStatus } from "@/hooks/use-daemon-status";
import { useSystemMetrics } from "@/hooks/use-metrics";
import { cn, formatBytes, pausedLabel } from "@/lib/utils";

function StatusDot({ state }: { state: string }) {
  const color =
    state === "active"
      ? "bg-green-500"
      : state === "paused"
        ? "bg-yellow-500"
        : "bg-gray-400";

  return <span className={cn("inline-block h-2.5 w-2.5 rounded-full", color)} />;
}

function statusLabel(state: string, taskCount: number, pausedReason: string | null): string {
  if (state === "active") {
    return taskCount > 0
      ? `Active — ${taskCount} task${taskCount === 1 ? "" : "s"}`
      : "Active — waiting for tasks";
  }
  if (state === "paused") {
    return pausedLabel(pausedReason);
  }
  return "Stopped";
}

/**
 * What to say while the daemon is not answering, from the Rust host's view of
 * the daemon process: "Starting…" while the daemon this app started is alive
 * but not yet listening (registering, or waiting on the container engine),
 * and the daemon's own reason after it exited without ever answering — the
 * refusal the log shows, which a plain "Stopped" hid (TB-52).
 */
export function unreachableLabel(process: DaemonProcessState | null): string {
  if (process?.state === "starting") return "Starting…";
  if (process?.state === "exited" && process.reason) return `Stopped — ${process.reason}`;
  return "Stopped";
}

/**
 * The host's view of the daemon process, polled only while the daemon is
 * not answering the management API (so a running daemon costs nothing here).
 */
function useDaemonProcessState(enabled: boolean, intervalMs: number): DaemonProcessState | null {
  const [process, setProcess] = useState<DaemonProcessState | null>(null);

  useEffect(() => {
    if (!enabled) {
      setProcess(null);
      return;
    }
    let cancelled = false;
    const poll = () => {
      getDaemonProcessState()
        .then((state) => {
          if (!cancelled) setProcess(state);
        })
        .catch(() => {
          if (!cancelled) setProcess(null);
        });
    };
    poll();
    const id = setInterval(poll, intervalMs);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [enabled, intervalMs]);

  return process;
}

export function StatusBar() {
  const { status } = useDaemonStatus(3000);
  const process = useDaemonProcessState(status === null, 3000);
  // Host CPU and memory come from the app itself; the daemon's /metrics
  // reports zeros for both.
  const { system } = useSystemMetrics(5000);

  const state = status?.state ?? "stopped";
  const taskCount = status?.active_tasks?.length ?? 0;
  const pausedReason = status?.paused_reason ?? null;
  const label = status ? statusLabel(state, taskCount, pausedReason) : unreachableLabel(process);

  return (
    <div className="flex items-center justify-between border-t bg-muted/50 px-4 py-2 text-xs text-muted-foreground">
      <div className="flex items-center gap-2">
        <StatusDot state={state} />
        <span>{label}</span>
      </div>
      {system && (
        <div className="flex items-center gap-3">
          <span>CPU {Math.round(system.cpu_usage_pct)}%</span>
          <span>·</span>
          <span>MEM {formatBytes(system.memory_used_mb)}</span>
        </div>
      )}
    </div>
  );
}
