import { useDaemonStatus } from "@/hooks/use-daemon-status";
import { useApiQuery } from "@/hooks/use-api";
import { cn, formatBytes } from "@/lib/utils";
import type { ManagementClient } from "@/api/client";

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
    return pausedReason ? `Paused — ${pausedReason}` : "Paused";
  }
  return "Stopped";
}

export function StatusBar() {
  const { status } = useDaemonStatus(3000);
  const { data: metrics } = useApiQuery(
    (client: ManagementClient) => client.metrics(),
    5000
  );

  const state = status?.state ?? "stopped";
  const taskCount = status?.active_tasks?.length ?? 0;
  const pausedReason = status?.paused_reason ?? null;

  return (
    <div className="flex items-center justify-between border-t bg-muted/50 px-4 py-2 text-xs text-muted-foreground">
      <div className="flex items-center gap-2">
        <StatusDot state={state} />
        <span>{statusLabel(state, taskCount, pausedReason)}</span>
      </div>
      {metrics && (
        <div className="flex items-center gap-3">
          <span>CPU {Math.round(metrics.cpu_usage_pct)}%</span>
          <span>·</span>
          <span>MEM {formatBytes(metrics.memory_used_mb)}</span>
        </div>
      )}
    </div>
  );
}
