import { useMemo } from "react";
import { cn } from "@/lib/utils";
import type { ActiveTaskInfo, QueuedTaskInfo } from "@/api/client";

export interface TaskFilterState {
  leafName: string | null;
  status: "all" | "running" | "suspended" | "error";
  showQueued: boolean;
}

export function applyTaskFilters(
  tasks: ActiveTaskInfo[],
  queuedTasks: QueuedTaskInfo[],
  filters: TaskFilterState,
): ActiveTaskInfo[] {
  let filtered = tasks;

  if (filters.leafName) {
    filtered = filtered.filter((t) => t.leaf_name === filters.leafName);
  }

  if (filters.status !== "all") {
    filtered = filtered.filter((t) => {
      switch (filters.status) {
        case "running":
          return t.task_status === "running";
        case "suspended":
          return t.task_status.startsWith("suspended");
        case "error":
          // The daemon never reports an "error" status today; kept for the filter option.
          return (t.task_status as string) === "error";
        default:
          return true;
      }
    });
  }

  if (filters.showQueued) {
    const leafFilter = filters.leafName;
    const matchingQueued = queuedTasks
      .filter((qt) => !leafFilter || qt.leaf_name === leafFilter)
      .map(
        (qt): ActiveTaskInfo => ({
          work_unit_id: qt.work_unit_id,
          leaf_name: qt.leaf_name,
          progress_pct: 0,
          elapsed_seconds: 0,
          estimated_remaining_seconds: undefined,
          work_dir: "",
          viz_bundle_path: null,
          checkpoint_sequence: 0,
          last_checkpoint_at: undefined,
          resumed_from_checkpoint: false,
          cpu_seconds: 0,
          task_status: "queued" as unknown as ActiveTaskInfo["task_status"],
          status_reason: null,
          deadline_seconds: qt.deadline_seconds,
          head_name: qt.server_name,
          runtime_type: "native",
          process_id: null,
        } as ActiveTaskInfo),
      );
    filtered = [...filtered, ...matchingQueued];
  }

  return filtered;
}

/** Check if a task object represents a queued task injected by applyTaskFilters. */
export function isQueuedTask(task: ActiveTaskInfo): boolean {
  return (task.task_status as string) === "queued";
}

export function TaskFilters({
  tasks,
  filters,
  onFiltersChange,
}: {
  tasks: ActiveTaskInfo[];
  filters: TaskFilterState;
  onFiltersChange: (filters: TaskFilterState) => void;
}) {
  const leafNames = useMemo(() => {
    const names = [...new Set(tasks.map((t) => t.leaf_name))];
    names.sort((a, b) => a.localeCompare(b));
    return names;
  }, [tasks]);

  if (tasks.length < 4) return null;

  return (
    <div className="flex items-center gap-2 flex-wrap">
      <select
        value={filters.leafName ?? ""}
        onChange={(e) =>
          onFiltersChange({ ...filters, leafName: e.target.value || null })
        }
        className="h-8 rounded-md border bg-background px-2 text-xs"
      >
        <option value="">All Leafs</option>
        {leafNames.map((name) => (
          <option key={name} value={name}>
            {name}
          </option>
        ))}
      </select>

      <select
        value={filters.status}
        onChange={(e) =>
          onFiltersChange({
            ...filters,
            status: e.target.value as TaskFilterState["status"],
          })
        }
        className="h-8 rounded-md border bg-background px-2 text-xs"
      >
        <option value="all">All</option>
        <option value="running">Running</option>
        <option value="suspended">Suspended</option>
        <option value="error">Error</option>
      </select>

      <div className="flex items-center gap-0.5 ml-1">
        <button
          onClick={() => onFiltersChange({ ...filters, showQueued: false })}
          className={cn(
            "px-2 py-1 rounded text-xs font-medium transition-colors",
            !filters.showQueued ? "bg-muted text-foreground" : "text-muted-foreground hover:text-foreground",
          )}
        >
          Active
        </button>
        <button
          onClick={() => onFiltersChange({ ...filters, showQueued: true })}
          className={cn(
            "px-2 py-1 rounded text-xs font-medium transition-colors",
            filters.showQueued ? "bg-muted text-foreground" : "text-muted-foreground hover:text-foreground",
          )}
        >
          All
        </button>
      </div>
    </div>
  );
}
