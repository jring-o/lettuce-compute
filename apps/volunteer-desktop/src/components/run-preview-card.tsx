import { useEffect, useState } from "react";
import { useApiQuery } from "@/hooks/use-api";
import type { PreviewTask, RunPreview } from "@/api/client";

function cores(n: number): string {
  return `${n} core${n === 1 ? "" : "s"}`;
}

/** Tasks of one leaf given the same cores: one chip however many there are. */
interface TaskGroup {
  key: string;
  leafName: string;
  cores: number;
  count: number;
}

/** The tasks that start together, one group per (head, leaf, cores), in the order they start. */
export function groupTasks(tasks: PreviewTask[]): TaskGroup[] {
  const groups = new Map<string, TaskGroup>();
  for (const t of tasks) {
    const key = `${t.head}/${t.leaf_id}/${t.cores}`;
    const g = groups.get(key);
    if (g) g.count++;
    else groups.set(key, { key, leafName: t.leaf_name, cores: t.cores, count: 1 });
  }
  return [...groups.values()];
}

/**
 * One leaf's tasks on its own: "2 cores each" when they are alike, else each
 * figure once with how many tasks get it ("62 × 4 cores, 4 × 2 cores").
 */
export function coreFigures(tasks: number[]): string {
  if (tasks.every((c) => c === tasks[0])) return `${cores(tasks[0])} each`;
  const counts = new Map<number, number>();
  for (const c of tasks) counts.set(c, (counts.get(c) ?? 0) + 1);
  return [...counts].map(([c, n]) => `${n} × ${cores(c)}`).join(", ");
}

/** Rows (task groups and leaf lines) past which the card starts folded. */
const FOLD_ROWS = 6;

function foldStorageKey(page: string): string {
  return `lettuce.run-preview.expanded.${page}`;
}

function loadExpanded(page: string): boolean | null {
  try {
    const raw = localStorage.getItem(foldStorageKey(page));
    return raw === null ? null : raw === "true";
  } catch {
    return null;
  }
}

function saveExpanded(page: string, expanded: boolean): void {
  try {
    localStorage.setItem(foldStorageKey(page), String(expanded));
  } catch {
    // Storage unavailable: the choice lasts while the page is open.
  }
}

/**
 * What would run together on this machine under the current settings, as
 * the daemon works it out with the scheduler's own arithmetic
 * (`GET /api/v1/run-preview`). It is a preview, not load: it says first what
 * would happen if the queue held work of every enabled leaf, then the tasks
 * that would start, grouped by leaf and cores, and each leaf on its own.
 * Past a few rows it starts folded to that first line; the choice is kept per
 * page (`page`). Refetched when `refreshKey` changes (after a setting is
 * saved). Shows nothing for a daemon that does not offer the preview.
 */
export function RunPreviewCard({ refreshKey, page = "default" }: { refreshKey?: unknown; page?: string }) {
  const { data, refetch } = useApiQuery<RunPreview>((c) => c.runPreview(), 30000);
  const [expandedChoice, setExpandedChoice] = useState<boolean | null>(() => loadExpanded(page));

  useEffect(() => {
    if (refreshKey !== undefined) refetch();
    // Only a change of the key asks again.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refreshKey]);

  if (!data || data.alone.length === 0) return null;

  const groups = groupTasks(data.together);
  const foldable = data.together.length > 0 && groups.length + data.alone.length > FOLD_ROWS;
  const expanded = !foldable || (expandedChoice ?? false);
  const toggle = () => {
    setExpandedChoice(!expanded);
    saveExpanded(page, !expanded);
  };
  const taskCount = data.together.length;

  return (
    <div className="rounded-md border p-3 space-y-2 text-sm" data-testid="run-preview">
      <div className="flex items-center justify-between gap-2">
        <p className="font-medium">What would run together here</p>
        {foldable && (
          <button
            type="button"
            onClick={toggle}
            aria-expanded={expanded}
            className="shrink-0 text-xs text-muted-foreground hover:text-foreground"
          >
            {expanded ? "Hide details" : "Show details"}
          </button>
        )}
      </div>
      {taskCount === 0 ? (
        <p className="text-xs text-muted-foreground">No enabled leaf can start on this machine.</p>
      ) : (
        <p className="text-xs text-muted-foreground" data-testid="run-preview-summary">
          If your queue held work of every enabled leaf: {taskCount} {taskCount === 1 ? "task" : "tasks"} would
          run at once, using{" "}
          {data.cpu_limit > 0
            ? `${data.together_cores} of your ${data.cpu_limit} allowed ${data.cpu_limit === 1 ? "core" : "cores"}.`
            : `${cores(data.together_cores)}.`}
          {data.waiting_for_cores &&
            ` The next ${data.waiting_for_cores.leaf_name} task needs ${cores(data.waiting_for_cores.cores)} and waits for them: Lettuce keeps the free cores for it rather than start narrower tasks in its place.`}
        </p>
      )}
      {expanded && (
        <>
          {groups.length > 0 && (
            <div className="flex flex-wrap gap-1.5">
              {groups.map((g) => (
                <span
                  key={g.key}
                  className="inline-flex items-center rounded-full border px-2 py-0.5 text-xs"
                  data-testid="run-preview-task"
                >
                  {g.leafName} × {g.count} · {cores(g.cores)}
                  {g.count > 1 ? " each" : ""}
                </span>
              ))}
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
                    : leaf.tasks.every((c) => c === leaf.tasks[0])
                      ? `${leaf.tasks.length} at once, ${coreFigures(leaf.tasks)}`
                      : `${leaf.tasks.length} at once: ${coreFigures(leaf.tasks)}`}
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}
