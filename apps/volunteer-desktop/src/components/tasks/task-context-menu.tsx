import { useState } from "react";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
} from "@/components/ui/dropdown-menu";
import { ConfirmDialog } from "@/components/ui/confirm-dialog";
import type { ActiveTaskInfo } from "@/api/client";

export interface TaskActions {
  onSuspend: (workUnitId: string) => void;
  onResume: (workUnitId: string) => void;
  onAbort: (workUnitId: string) => void;
  /** Restart the task with the current settings; the item is shown only when set. */
  onRestart?: (workUnitId: string) => void;
  onShowDetails: (task: ActiveTaskInfo) => void;
  onCopyId: (workUnitId: string) => void;
}

/**
 * What restarting a task does, said before it is done: the task starts over
 * with the settings in force now, so the work it has done is lost unless its
 * leaf checkpoints, and its deadline keeps counting from its first start.
 */
export const RESTART_TASK_DESCRIPTION =
  "The task stops now and starts again from the beginning with the cores and settings in force now — a running task otherwise keeps the ones it started with until it finishes. The work it has done so far is lost unless its leaf saves checkpoints, in which case it continues from the last one. Its deadline keeps counting from when it first started.";

/** The confirmation shown before a task is restarted. */
export function RestartTaskDialog({
  open,
  onOpenChange,
  onConfirm,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onConfirm: () => void;
}) {
  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title="Restart this task with the current settings?"
      description={RESTART_TASK_DESCRIPTION}
      confirmLabel="Restart"
      onConfirm={onConfirm}
    />
  );
}

/** Reusable dropdown menu items for task actions. Used by both card overflow and table row overflow. */
export function TaskContextMenu({
  task,
  actions,
  trigger,
  open,
  onOpenChange,
}: {
  task: ActiveTaskInfo;
  actions: TaskActions;
  trigger: React.ReactNode;
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
}) {
  const [abortOpen, setAbortOpen] = useState(false);
  const [restartOpen, setRestartOpen] = useState(false);
  const isSuspended = task.task_status.startsWith("suspended");

  return (
    <>
      <DropdownMenu open={open} onOpenChange={onOpenChange}>
        <DropdownMenuTrigger asChild>{trigger}</DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          {task.task_status === "running" && (
            <DropdownMenuItem onSelect={() => actions.onSuspend(task.work_unit_id)}>
              Suspend
            </DropdownMenuItem>
          )}
          {isSuspended && (
            <DropdownMenuItem onSelect={() => actions.onResume(task.work_unit_id)}>
              Resume
            </DropdownMenuItem>
          )}
          {actions.onRestart && (
            <DropdownMenuItem onSelect={() => setRestartOpen(true)}>
              Restart with current settings
            </DropdownMenuItem>
          )}
          <DropdownMenuSeparator />
          <DropdownMenuItem variant="destructive" onSelect={() => setAbortOpen(true)}>
            Abort
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem onSelect={() => actions.onShowDetails(task)}>
            Show Details
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => actions.onCopyId(task.work_unit_id)}>
            Copy Work Unit ID
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>

      <ConfirmDialog
        open={abortOpen}
        onOpenChange={setAbortOpen}
        title="Abort this task?"
        description="This will kill the process and the work unit will be reassigned."
        confirmLabel="Abort"
        variant="destructive"
        onConfirm={() => {
          actions.onAbort(task.work_unit_id);
          setAbortOpen(false);
        }}
      />

      <RestartTaskDialog
        open={restartOpen}
        onOpenChange={setRestartOpen}
        onConfirm={() => {
          actions.onRestart?.(task.work_unit_id);
          setRestartOpen(false);
        }}
      />
    </>
  );
}
