import { useState } from "react";
import { RefreshCw, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { restartDaemon } from "@/api/client";

interface RestartRequiredBannerProps {
  /** What was changed and why it is waiting on a restart, in plain language. */
  message: string;
  /** Called once a restart has completed and a fresh daemon is answering. */
  onRestarted?: () => void;
  /** When given, the banner can be dismissed without restarting. */
  onDismiss?: () => void;
}

/**
 * A notice that a saved change takes effect only when the volunteer daemon
 * ("Lettuce") next starts, with a button that restarts it now. Shown by any
 * page whose write the daemon answered with `restart_required`, or whose
 * change is known to be read only at daemon start (runtime trust, a newly
 * attached head).
 */
export function RestartRequiredBanner({
  message,
  onRestarted,
  onDismiss,
}: RestartRequiredBannerProps) {
  const [restarting, setRestarting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleRestart = async () => {
    setRestarting(true);
    setError(null);
    try {
      await restartDaemon();
      onRestarted?.();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setRestarting(false);
    }
  };

  return (
    <div
      role="status"
      className="rounded-md border border-amber-300 bg-amber-50 px-4 py-3 dark:border-amber-800 dark:bg-amber-950"
    >
      <div className="flex items-start justify-between gap-4">
        <div className="flex items-start gap-2 min-w-0">
          <RefreshCw className="mt-0.5 h-4 w-4 shrink-0 text-amber-700 dark:text-amber-400" />
          <div className="space-y-1 min-w-0">
            <p className="text-sm text-amber-900 dark:text-amber-100">{message}</p>
            <p className="text-xs text-amber-800 dark:text-amber-300">
              {restarting
                ? "Restarting Lettuce. This can take up to a minute."
                : "Restarting takes up to a minute."}
            </p>
            {error && (
              <p className="text-xs text-red-700 dark:text-red-400">
                Restart failed: {error}
              </p>
            )}
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <Button
            size="sm"
            className="h-7 text-xs bg-amber-600 hover:bg-amber-700 text-white"
            onClick={handleRestart}
            disabled={restarting}
          >
            {restarting ? "Restarting..." : "Restart Lettuce now"}
          </Button>
          {onDismiss && !restarting && (
            <button
              onClick={onDismiss}
              className="text-amber-700 hover:text-amber-900 dark:text-amber-400 dark:hover:text-amber-200"
              aria-label="Dismiss restart notice"
            >
              <X className="h-4 w-4" />
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
