import { useEffect } from "react";
import { useApiQuery } from "@/hooks/use-api";
import type { RunPreview } from "@/api/client";

function cores(n: number): string {
  return `${n} core${n === 1 ? "" : "s"}`;
}

/** "2 cores each", or "4, 2 and 2 cores" when the tasks differ. */
function coreFigures(tasks: number[]): string {
  if (tasks.every((c) => c === tasks[0])) return `${cores(tasks[0])} each`;
  return `${tasks.slice(0, -1).join(", ")} and ${tasks[tasks.length - 1]} cores`;
}

/**
 * What would run together on this machine under the current settings, as
 * the daemon works it out with the scheduler's own arithmetic
 * (`GET /api/v1/run-preview`): the tasks that start when the buffer holds
 * work of every enabled leaf in turn, and each leaf on its own. Refetched
 * when `refreshKey` changes (after a setting is saved). Shows nothing for a
 * daemon that does not offer the preview.
 */
export function RunPreviewCard({ refreshKey }: { refreshKey?: unknown }) {
  const { data, refetch } = useApiQuery<RunPreview>((c) => c.runPreview(), 30000);

  useEffect(() => {
    if (refreshKey !== undefined) refetch();
    // Only a change of the key asks again.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refreshKey]);

  if (!data || data.alone.length === 0) return null;

  return (
    <div className="rounded-md border p-3 space-y-2 text-sm" data-testid="run-preview">
      <p className="font-medium">What runs together here</p>
      {data.together.length === 0 ? (
        <p className="text-xs text-muted-foreground">No enabled leaf can start on this machine.</p>
      ) : (
        <div className="space-y-1">
          <div className="flex flex-wrap gap-1.5">
            {data.together.map((task, i) => (
              <span
                key={i}
                className="inline-flex items-center rounded-full border px-2 py-0.5 text-xs"
                data-testid="run-preview-task"
              >
                {task.leaf_name} · {cores(task.cores)}
              </span>
            ))}
          </div>
          <p className="text-xs text-muted-foreground">
            {data.cpu_limit > 0 && `${data.together_cores} of ${data.cpu_limit} cores. `}
            {data.waiting_for_cores &&
              `The next ${data.waiting_for_cores.leaf_name} task needs ${cores(data.waiting_for_cores.cores)} and waits for them: Lettuce keeps the free cores for it rather than start narrower tasks in its place. `}
            When the buffer holds work of every enabled leaf in turn.
          </p>
        </div>
      )}
      <ul className="text-xs text-muted-foreground space-y-0.5">
        {data.alone.map((leaf) => (
          <li key={`${leaf.head}/${leaf.leaf_id}`}>
            {leaf.leaf_name} on its own:{" "}
            {leaf.cannot_start
              ? "cannot start here"
              : leaf.tasks.length === 0
                ? "none"
                : `${leaf.tasks.length} at once, ${coreFigures(leaf.tasks)}`}
          </li>
        ))}
      </ul>
    </div>
  );
}
