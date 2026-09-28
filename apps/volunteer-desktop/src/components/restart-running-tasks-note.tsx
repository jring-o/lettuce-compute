import { useState } from "react";
import { useClient } from "@/hooks/use-api";
import { useDaemonStatus } from "@/hooks/use-daemon-status";
import { ConfirmDialog } from "@/components/ui/confirm-dialog";
import { RESTART_TASK_DESCRIPTION } from "@/components/tasks/task-context-menu";

/**
 * Shown once a CPU setting has been saved (`show`): the change applies to
 * tasks started from now on, and the tasks already running keep the cores
 * they started with — with an offer to restart them under the new settings.
 * Nothing is shown while no task runs.
 */
export function RestartRunningTasksNote({ show }: { show: boolean }) {
  const { status } = useDaemonStatus(5000);
  const { client } = useClient();
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const running = status?.active_tasks ?? [];

  if (!show || running.length === 0) return null;

  const restartAll = async () => {
    if (!client) return;
    let failed = 0;
    for (const task of running) {
      try {
        await client.restartTask(task.work_unit_id);
      } catch {
        failed++;
      }
    }
    setMessage(
      failed === 0
        ? `Restarting ${running.length === 1 ? "the running task" : `${running.length} running tasks`} with the new settings.`
        : `${failed} of ${running.length} tasks could not be restarted (a task still starting cannot be); try again in a moment.`
    );
  };

  return (
    <div className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-900 dark:border-amber-800 dark:bg-amber-950 dark:text-amber-200 space-y-1" data-testid="restart-running-note">
      <p>
        This applies to tasks started from now on.{" "}
        {running.length === 1
          ? "The running task keeps the cores and settings it started with until it finishes."
          : `The ${running.length} running tasks keep the cores and settings they started with until they finish.`}
      </p>
      {message ? (
        <p>{message}</p>
      ) : (
        <button onClick={() => setConfirmOpen(true)} className="text-blue-600 hover:underline">
          {running.length === 1 ? "Restart it with the new settings…" : "Restart them with the new settings…"}
        </button>
      )}
      <ConfirmDialog
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
        title={running.length === 1 ? "Restart the running task?" : `Restart the ${running.length} running tasks?`}
        description={RESTART_TASK_DESCRIPTION}
        confirmLabel="Restart"
        onConfirm={() => {
          setConfirmOpen(false);
          void restartAll();
        }}
      />
    </div>
  );
}
