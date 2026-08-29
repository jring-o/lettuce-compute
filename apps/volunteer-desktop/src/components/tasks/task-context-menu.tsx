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
  onShowDetails: (task: ActiveTaskInfo) => void;
  onCopyId: (workUnitId: string) => void;
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
    </>
  );
}
