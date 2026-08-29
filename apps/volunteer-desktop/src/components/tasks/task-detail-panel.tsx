import { useState, useEffect } from "react";
import { VisuallyHidden } from "@radix-ui/react-visually-hidden";
import { useClient } from "@/hooks/use-api";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
} from "@/components/ui/sheet";
import { ConfirmDialog } from "@/components/ui/confirm-dialog";
import { Button } from "@/components/ui/button";
import { STATUS_DOT_COLOR, STATUS_TEXT, RUNTIME_BADGE } from "./task-status";
import { cn, formatDuration, formatTimeAgo } from "@/lib/utils";
import type { TaskDetail } from "@/api/client";

interface TaskDetailPanelProps {
  workUnitId: string | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSuspend: (id: string) => void;
  onResume: (id: string) => void;
  onAbort: (id: string) => void;
}

function CollapsibleSection({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  const [open, setOpen] = useState(true);

  return (
    <div className="border-b last:border-b-0">
      <button
        onClick={() => setOpen(!open)}
        className="flex items-center gap-2 w-full py-2 text-xs font-medium text-muted-foreground hover:text-foreground transition-colors"
      >
        <span className={cn("transition-transform text-[10px]", open && "rotate-90")}>
          ▶
        </span>
        {title}
      </button>
      {open && <div className="pb-3">{children}</div>}
    </div>
  );
}

function KV({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="grid grid-cols-[140px_1fr] gap-1 py-0.5">
      <span className="text-xs text-muted-foreground">{label}</span>
      <span className="text-sm font-mono truncate">{value ?? "---"}</span>
    </div>
  );
}

function formatCompletionTime(iso: string | null): string {
  if (!iso) return "---";
  try {
    const d = new Date(iso);
    return d.toLocaleString();
  } catch {
    return "---";
  }
}

function buildCopyText(d: TaskDetail): string {
  const lines = [
    `Leaf: ${d.leaf_name}`,
    `Work Unit ID: ${d.work_unit_id}`,
    `Status: ${STATUS_TEXT[d.task_status] ?? d.task_status}`,
    "",
    "--- Timing ---",
    `CPU Time: ${formatDuration(d.cpu_seconds)}`,
    `Elapsed Time: ${formatDuration(d.elapsed_seconds)}`,
    `Time Paused: ${formatDuration(d.elapsed_seconds - d.cpu_seconds)}`,
    `Time Since Checkpoint: ${d.time_since_checkpoint_seconds != null ? formatDuration(d.time_since_checkpoint_seconds) : "---"}`,
    `Estimated Remaining: ${d.estimated_remaining_seconds != null ? formatDuration(d.estimated_remaining_seconds) : "---"}`,
    `Estimated Completion: ${formatCompletionTime(d.estimated_completion_at)}`,
    `Deadline: ${formatDuration(d.deadline_seconds)}`,
    "",
    "--- Progress ---",
    `Fraction Done: ${d.fraction_done.toFixed(3)}%`,
    `Progress Rate: ${d.progress_rate_pct_per_hour != null ? `${d.progress_rate_pct_per_hour.toFixed(1)}% / hour` : "---"}`,
    "",
    "--- Resources ---",
    `Memory (RSS): ${d.memory_rss_mb != null ? `${d.memory_rss_mb} MB` : "---"}`,
    `Virtual Memory: ${d.virtual_memory_mb != null ? `${d.virtual_memory_mb} MB` : "---"}`,
    `CPU Usage: ${d.cpu_usage_pct != null ? `${d.cpu_usage_pct}%` : "---"}`,
    `Disk Read: ${d.disk_read_mb != null ? `${d.disk_read_mb} MB` : "---"}`,
    `Disk Written: ${d.disk_written_mb != null ? `${d.disk_written_mb} MB` : "---"}`,
    "",
    "--- Task Info ---",
    `Head: ${d.head_name}`,
    `Leaf: ${d.leaf_name}`,
    `Runtime: ${d.runtime_type}`,
    `Container Image: ${d.container_image ?? "---"}`,
    `Work Directory: ${d.work_dir}`,
    `Process ID: ${d.process_id ?? "---"}`,
    `Checkpoint: ${d.checkpoint_sequence != null && d.checkpoint_sequence > 0 ? `seq ${d.checkpoint_sequence}${d.last_checkpoint_at ? `, saved ${formatTimeAgo(d.last_checkpoint_at)}` : ""}` : "None"}`,
    `Resumed from Checkpoint: ${d.resumed_from_checkpoint ? "Yes" : "No"}`,
  ];
  return lines.join("\n");
}

export function TaskDetailPanel({
  workUnitId,
  open,
  onOpenChange,
  onSuspend,
  onResume,
  onAbort,
}: TaskDetailPanelProps) {
  const { client } = useClient();
  const [detail, setDetail] = useState<TaskDetail | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [abortOpen, setAbortOpen] = useState(false);

  useEffect(() => {
    if (!workUnitId || !client || !open) {
      setDetail(null);
      setError(null);
      return;
    }
    setLoading(true);
    setError(null);
    client
      .taskDetails(workUnitId)
      .then(setDetail)
      .catch((e) => setError(e?.message ?? "Failed to fetch task details"))
      .finally(() => setLoading(false));
  }, [workUnitId, client, open]);

  const isSuspended = detail?.task_status.startsWith("suspended") ?? false;

  return (
    <>
      <Sheet open={open} onOpenChange={onOpenChange}>
        <SheetContent side="right" className="flex flex-col overflow-y-auto">
          {!detail && (
            <VisuallyHidden>
              <SheetTitle>Task Details</SheetTitle>
            </VisuallyHidden>
          )}
          {loading && (
            <div className="flex-1 flex items-center justify-center text-sm text-muted-foreground">
              Loading...
            </div>
          )}
          {error && (
            <div className="flex-1 flex items-center justify-center text-sm text-red-500">
              {error}
            </div>
          )}
          {detail && !loading && (
            <>
              <SheetHeader>
                <SheetTitle>{detail.leaf_name}</SheetTitle>
                <SheetDescription asChild>
                  <div className="space-y-1">
                    <div className="flex items-center gap-2">
                      <code className="text-xs font-mono text-muted-foreground">
                        {detail.work_unit_id}
                      </code>
                      <button
                        onClick={() => navigator.clipboard.writeText(detail.work_unit_id)}
                        className="p-0.5 rounded hover:bg-accent text-muted-foreground"
                        title="Copy Work Unit ID"
                      >
                        <svg className="h-3.5 w-3.5" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5">
                          <rect x="5" y="5" width="9" height="9" rx="1" />
                          <path d="M3 11V3a1 1 0 011-1h8" />
                        </svg>
                      </button>
                    </div>
                    <span className="inline-flex items-center gap-1.5">
                      <span className={cn("h-2 w-2 rounded-full", STATUS_DOT_COLOR[detail.task_status] ?? "bg-gray-500")} />
                      <span className="text-xs">
                        {STATUS_TEXT[detail.task_status] ?? detail.task_status}
                      </span>
                    </span>
                  </div>
                </SheetDescription>
              </SheetHeader>

              <div className="flex-1 mt-4 space-y-0 divide-y-0">
                <CollapsibleSection title="Timing">
                  <KV label="CPU Time" value={formatDuration(detail.cpu_seconds)} />
                  <KV label="Elapsed Time" value={formatDuration(detail.elapsed_seconds)} />
                  <KV label="Time Paused" value={formatDuration(detail.elapsed_seconds - detail.cpu_seconds)} />
                  <KV
                    label="Since Checkpoint"
                    value={detail.time_since_checkpoint_seconds != null ? formatDuration(detail.time_since_checkpoint_seconds) : "---"}
                  />
                  <KV
                    label="Est. Remaining"
                    value={detail.estimated_remaining_seconds != null ? formatDuration(detail.estimated_remaining_seconds) : "---"}
                  />
                  <KV label="Est. Completion" value={formatCompletionTime(detail.estimated_completion_at)} />
                  <KV label="Deadline" value={formatDuration(detail.deadline_seconds)} />
                </CollapsibleSection>

                <CollapsibleSection title="Progress">
                  <div className="space-y-2">
                    <div className="h-3 rounded-full bg-secondary overflow-hidden">
                      <div
                        className="h-full rounded-full bg-primary transition-all duration-300"
                        style={{ width: `${Math.min(100, detail.fraction_done)}%` }}
                      />
                    </div>
                    <div className="flex justify-between text-xs text-muted-foreground">
                      <span className="font-mono">{detail.fraction_done.toFixed(3)}%</span>
                      <span>
                        {detail.progress_rate_pct_per_hour != null
                          ? `${detail.progress_rate_pct_per_hour.toFixed(1)}% / hour`
                          : "---"}
                      </span>
                    </div>
                  </div>
                </CollapsibleSection>

                <CollapsibleSection title="Resources">
                  <KV label="Memory (RSS)" value={detail.memory_rss_mb != null ? `${detail.memory_rss_mb} MB` : "---"} />
                  <KV label="Virtual Memory" value={detail.virtual_memory_mb != null ? `${detail.virtual_memory_mb} MB` : "---"} />
                  <KV label="CPU Usage" value={detail.cpu_usage_pct != null ? `${detail.cpu_usage_pct}%` : "---"} />
                  <KV label="Disk Read" value={detail.disk_read_mb != null ? `${detail.disk_read_mb} MB` : "---"} />
                  <KV label="Disk Written" value={detail.disk_written_mb != null ? `${detail.disk_written_mb} MB` : "---"} />
                </CollapsibleSection>

                <CollapsibleSection title="Task Info">
                  <KV label="Head" value={detail.head_name} />
                  <KV label="Leaf" value={detail.leaf_name} />
                  <KV
                    label="Runtime"
                    value={
                      <span className="flex items-center gap-1.5">
                        {RUNTIME_BADGE[detail.runtime_type] ? (
                          <span className={cn("inline-flex items-center rounded-full border px-2 py-0.5 text-[10px] font-medium", RUNTIME_BADGE[detail.runtime_type].className)}>
                            {RUNTIME_BADGE[detail.runtime_type].label}
                          </span>
                        ) : (
                          detail.runtime_type
                        )}
                      </span>
                    }
                  />
                  <KV label="Container Image" value={detail.container_image ?? "---"} />
                  <KV label="Work Directory" value={detail.work_dir} />
                  <KV label="Process ID" value={detail.process_id ?? "---"} />
                  <KV
                    label="Checkpoint"
                    value={
                      detail.checkpoint_sequence != null && detail.checkpoint_sequence > 0
                        ? `seq ${detail.checkpoint_sequence}${detail.last_checkpoint_at ? `, saved ${formatTimeAgo(detail.last_checkpoint_at)}` : ""}`
                        : "None"
                    }
                  />
                  <KV label="Resumed" value={detail.resumed_from_checkpoint ? "Yes" : "No"} />
                </CollapsibleSection>
              </div>

              <div className="flex items-center gap-2 pt-4 border-t mt-4">
                {isSuspended ? (
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => {
                      onResume(detail.work_unit_id);
                      onOpenChange(false);
                    }}
                  >
                    Resume
                  </Button>
                ) : detail.task_status === "running" ? (
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => {
                      onSuspend(detail.work_unit_id);
                      onOpenChange(false);
                    }}
                  >
                    Suspend
                  </Button>
                ) : null}
                <Button
                  variant="destructive"
                  size="sm"
                  onClick={() => setAbortOpen(true)}
                >
                  Abort
                </Button>
                <div className="flex-1" />
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => navigator.clipboard.writeText(buildCopyText(detail))}
                >
                  Copy All
                </Button>
              </div>
            </>
          )}
        </SheetContent>
      </Sheet>

      <ConfirmDialog
        open={abortOpen}
        onOpenChange={setAbortOpen}
        title="Abort this task?"
        description="This will kill the process and the work unit will be reassigned."
        confirmLabel="Abort"
        variant="destructive"
        onConfirm={() => {
          if (workUnitId) {
            onAbort(workUnitId);
          }
          setAbortOpen(false);
          onOpenChange(false);
        }}
      />
    </>
  );
}
