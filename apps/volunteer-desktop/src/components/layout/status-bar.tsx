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

export function StatusBar() {
  const { status } = useDaemonStatus(3000);
  // Host CPU and memory come from the app itself; the daemon's /metrics
  // reports zeros for both.
  const { system } = useSystemMetrics(5000);

  const state = status?.state ?? "stopped";
  const taskCount = status?.active_tasks?.length ?? 0;
  const pausedReason = status?.paused_reason ?? null;

  return (
    <div className="flex items-center justify-between border-t bg-muted/50 px-4 py-2 text-xs text-muted-foreground">
      <div className="flex items-center gap-2">
        <StatusDot state={state} />
        <span>{statusLabel(state, taskCount, pausedReason)}</span>
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
