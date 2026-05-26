import type { LeafStats } from "@/types/infrastructure";

const STATUS_SEGMENTS = [
  { key: "work_units_queued", label: "Queued", color: "bg-gray-400" },
  { key: "work_units_assigned", label: "Assigned", color: "bg-blue-500" },
  { key: "work_units_running", label: "Running", color: "bg-indigo-500" },
  { key: "work_units_completed", label: "Completed", color: "bg-yellow-500" },
  { key: "work_units_validated", label: "Validated", color: "bg-green-500" },
  { key: "work_units_failed", label: "Failed", color: "bg-red-500" },
] as const;

interface WorkUnitStatusProps {
  stats: LeafStats;
}

export function WorkUnitStatus({ stats }: WorkUnitStatusProps) {
  const total = stats.total_work_units;
  if (total === 0) return null;

  return (
    <div data-testid="work-unit-status" className="space-y-2">
      {/* Stacked bar */}
      <div className="flex h-3 w-full overflow-hidden rounded-full bg-muted">
        {STATUS_SEGMENTS.map(({ key, color }) => {
          const count = stats[key as keyof LeafStats] as number;
          if (count <= 0) return null;
          const pct = (count / total) * 100;
          return (
            <div
              key={key}
              className={`${color} transition-all duration-300`}
              style={{ width: `${pct}%` }}
              title={`${key.replace("work_units_", "")}: ${count}`}
            />
          );
        })}
      </div>

      {/* Legend */}
      <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
        {STATUS_SEGMENTS.map(({ key, label, color }) => {
          const count = stats[key as keyof LeafStats] as number;
          if (count <= 0) return null;
          return (
            <span key={key} className="flex items-center gap-1.5">
              <span className={`inline-block size-2.5 rounded-full ${color}`} />
              {label}: {count.toLocaleString()}
            </span>
          );
        })}
      </div>
    </div>
  );
}
