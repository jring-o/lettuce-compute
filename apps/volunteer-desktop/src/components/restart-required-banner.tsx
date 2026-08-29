import { useState } from "react";
import { RefreshCw, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { restartLettuce, useRestartRequired } from "@/hooks/use-restart-required";

/**
 * The app-wide notice that a saved change takes effect only when the
 * volunteer daemon ("Lettuce") next starts, with a button that restarts it
 * now. It renders from the shared restart store (`use-restart-required.ts`),
 * so it appears on whichever page is open once any page records a pending
 * reason, and disappears when the in-app restart succeeds or the volunteer
 * dismisses it. Mounted once, in the tab layout under the update banner.
 */
export function RestartRequiredBanner() {
  const { restartRequired, reasons, clearRestartRequired } = useRestartRequired();
  const [restarting, setRestarting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (!restartRequired) return null;

  const handleRestart = async () => {
    setRestarting(true);
    setError(null);
    try {
      await restartLettuce();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setRestarting(false);
    }
  };

  return (
    <div
      role="status"
      className="border-b border-amber-300 bg-amber-50 px-4 py-3 dark:border-amber-800 dark:bg-amber-950"
    >
      <div className="flex items-start justify-between gap-4">
        <div className="flex items-start gap-2 min-w-0">
          <RefreshCw
            className={
              "mt-0.5 h-4 w-4 shrink-0 text-amber-700 dark:text-amber-400" +
              (restarting ? " animate-spin" : "")
            }
          />
          <div className="space-y-1 min-w-0">
            <p className="text-sm font-medium text-amber-900 dark:text-amber-100">
              Restart required
            </p>
            {reasons.map((reason) => (
              <p key={reason} className="text-sm text-amber-900 dark:text-amber-100">
                {reason}
              </p>
            ))}
            <p className="text-xs text-amber-800 dark:text-amber-300">
              {restarting
                ? "Restarting Lettuce. This can take up to a minute; running work is checkpointed and picked up again."
                : "Restarting takes up to a minute; running work is checkpointed and picked up again."}
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
          {!restarting && (
            <button
              onClick={clearRestartRequired}
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
