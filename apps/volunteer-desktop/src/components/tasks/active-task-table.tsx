import { useState, useMemo } from "react";
import { TaskContextMenu, type TaskActions } from "./task-context-menu";
import { STATUS_DOT_COLOR, STATUS_TEXT_SHORT } from "./task-status";
import { cn, formatDuration } from "@/lib/utils";
import type { ActiveTaskInfo } from "@/api/client";

type SortColumn = "leaf_name" | "task_status" | "progress_pct" | "cpu_seconds" | "remaining" | "deadline_seconds" | "work_unit_id";
type SortDirection = "asc" | "desc";

interface ColumnDef {
  key: SortColumn;
  label: string;
  className?: string;
}

const COLUMNS: ColumnDef[] = [
  { key: "leaf_name", label: "Leaf" },
  { key: "task_status", label: "Status" },
  { key: "progress_pct", label: "Progress", className: "w-[120px]" },
  { key: "cpu_seconds", label: "CPU Time" },
  { key: "remaining", label: "Remaining" },
  { key: "deadline_seconds", label: "Deadline" },
  { key: "work_unit_id", label: "WU ID" },
];

function getSortValue(task: ActiveTaskInfo, column: SortColumn): string | number {
  switch (column) {
    case "leaf_name": return task.leaf_name.toLowerCase();
    case "task_status": return (STATUS_TEXT_SHORT[task.task_status] ?? task.task_status).toLowerCase();
    case "progress_pct": return task.progress_pct;
    case "cpu_seconds": return task.cpu_seconds;
    case "remaining": return task.estimated_remaining_seconds ?? Infinity;
    case "deadline_seconds": return task.deadline_seconds;
    case "work_unit_id": return task.work_unit_id.toLowerCase();
  }
}

function SortArrow({ direction }: { direction: SortDirection }) {
  return (
    <svg className="inline-block ml-1 h-3 w-3" viewBox="0 0 12 12" fill="currentColor">
      {direction === "asc" ? (
        <path d="M6 2L10 8H2L6 2Z" />
      ) : (
        <path d="M6 10L2 4H10L6 10Z" />
      )}
    </svg>
  );
}

export function ActiveTaskTable({
  tasks,
  actions,
  onRowClick,
}: {
  tasks: ActiveTaskInfo[];
  actions: TaskActions;
  onRowClick: (task: ActiveTaskInfo) => void;
}) {
  const [sort, setSort] = useState<{ column: SortColumn; direction: SortDirection }>({
    column: "leaf_name",
    direction: "asc",
  });
  const [contextMenu, setContextMenu] = useState<{ task: ActiveTaskInfo; x: number; y: number } | null>(null);

  const sorted = useMemo(() => {
    return Array.from(tasks).sort((a, b) => {
      const aVal = getSortValue(a, sort.column);
      const bVal = getSortValue(b, sort.column);
      const cmp = aVal < bVal ? -1 : aVal > bVal ? 1 : 0;
      return sort.direction === "asc" ? cmp : -cmp;
    });
  }, [tasks, sort]);

  const handleHeaderClick = (column: SortColumn) => {
    setSort((prev) =>
      prev.column === column
        ? { column, direction: prev.direction === "asc" ? "desc" : "asc" }
        : { column, direction: "asc" }
    );
  };

  const handleContextMenu = (e: React.MouseEvent, task: ActiveTaskInfo) => {
    e.preventDefault();
    setContextMenu({ task, x: e.clientX, y: e.clientY });
  };

  return (
    <div className="rounded-lg border overflow-hidden">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b bg-muted/30">
            {COLUMNS.map((col) => (
              <th
                key={col.key}
                className={cn(
                  "text-left px-3 py-2 text-xs font-medium text-muted-foreground cursor-pointer select-none hover:text-foreground transition-colors",
                  col.className
                )}
                onClick={() => handleHeaderClick(col.key)}
              >
                {col.label}
                {sort.column === col.key && <SortArrow direction={sort.direction} />}
              </th>
            ))}
            <th className="w-8" />
          </tr>
        </thead>
        <tbody>
          {sorted.map((task) => (
            <tr
              key={task.work_unit_id}
              className="border-b last:border-b-0 hover:bg-muted/50 transition-colors cursor-pointer"
              onClick={() => onRowClick(task)}
              onContextMenu={(e) => handleContextMenu(e, task)}
            >
              <td className="px-3 py-2 text-sm">{task.leaf_name}</td>
              <td className="px-3 py-2">
                <span className="flex items-center gap-1.5">
                  <span className={cn("h-2 w-2 rounded-full shrink-0", STATUS_DOT_COLOR[task.task_status] ?? "bg-gray-500")} />
                  <span className="text-xs">{STATUS_TEXT_SHORT[task.task_status] ?? task.task_status}</span>
                </span>
              </td>
              <td className="px-3 py-2">
                <div className="flex items-center gap-2">
                  <div className="h-2 flex-1 rounded-full bg-secondary overflow-hidden">
                    <div
                      className="h-full rounded-full bg-primary transition-all duration-300"
                      style={{ width: `${Math.min(100, task.progress_pct)}%` }}
                    />
                  </div>
                  <span className="text-xs text-muted-foreground w-8 text-right">
                    {Math.round(task.progress_pct)}%
                  </span>
                </div>
              </td>
              <td className="px-3 py-2 text-xs font-mono">{formatDuration(task.cpu_seconds)}</td>
              <td className="px-3 py-2 text-xs">
                {task.estimated_remaining_seconds != null && task.estimated_remaining_seconds > 0
                  ? formatDuration(task.estimated_remaining_seconds)
                  : "---"}
              </td>
              <td className={cn(
                "px-3 py-2 text-xs",
                task.deadline_seconds < 0
                  ? "text-red-500"
                  : task.deadline_seconds < 7200
                    ? "text-yellow-500"
                    : ""
              )}>
                {formatDuration(Math.abs(task.deadline_seconds))}
              </td>
              <td className="px-3 py-2 text-xs font-mono">{task.work_unit_id.slice(0, 8)}</td>
              <td className="px-1 py-2" onClick={(e) => e.stopPropagation()}>
                <TaskContextMenu
                  task={task}
                  actions={actions}
                  trigger={
                    <button className="p-1 rounded hover:bg-accent text-muted-foreground text-sm">
                      ···
                    </button>
                  }
                />
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {/* Right-click context menu rendered as a floating dropdown */}
      {contextMenu && (
        <TaskContextMenu
          task={contextMenu.task}
          actions={actions}
          open={true}
          onOpenChange={(open) => { if (!open) setContextMenu(null); }}
          trigger={
            <div
              style={{
                position: "fixed",
                left: contextMenu.x,
                top: contextMenu.y,
                width: 0,
                height: 0,
              }}
            />
          }
        />
      )}
    </div>
  );
}
