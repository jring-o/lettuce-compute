import { useState, useEffect } from "react";
import { listen } from "@tauri-apps/api/event";
import { invoke } from "@tauri-apps/api/core";
import { Download, X } from "lucide-react";
import { Button } from "@/components/ui/button";

interface UpdateAvailableEvent {
  version: string;
  body?: string;
}

interface UpdateProgressEvent {
  progress_pct: number;
}

export function UpdateBanner() {
  const [update, setUpdate] = useState<UpdateAvailableEvent | null>(null);
  const [dismissed, setDismissed] = useState(false);
  const [installing, setInstalling] = useState(false);
  const [progress, setProgress] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const unlistenAvailable = listen<UpdateAvailableEvent>(
      "update:available",
      (event) => {
        setUpdate(event.payload);
        setDismissed(false);
        setError(null);
      }
    );

    const unlistenProgress = listen<UpdateProgressEvent>(
      "update:progress",
      (event) => {
        setProgress(event.payload.progress_pct);
      }
    );

    return () => {
      unlistenAvailable.then((fn) => fn());
      unlistenProgress.then((fn) => fn());
    };
  }, []);

  if (!update || dismissed) return null;

  const handleInstall = async () => {
    setInstalling(true);
    setProgress(0);
    setError(null);
    try {
      await invoke("install_update");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setInstalling(false);
      setProgress(null);
    }
  };

  return (
    <div className="bg-blue-50 dark:bg-blue-950 border-b border-blue-200 dark:border-blue-800 px-4 py-2">
      <div className="flex items-center justify-between gap-4">
        <div className="flex items-center gap-2 min-w-0">
          <Download className="h-4 w-4 text-blue-600 dark:text-blue-400 shrink-0" />
          {installing && progress !== null ? (
            <div className="flex items-center gap-3 flex-1 min-w-0">
              <span className="text-sm text-blue-800 dark:text-blue-200 shrink-0">
                Downloading update...
              </span>
              <div className="flex-1 h-2 bg-blue-200 dark:bg-blue-800 rounded-full overflow-hidden min-w-[100px]">
                <div
                  className="h-full bg-blue-600 dark:bg-blue-400 rounded-full transition-all duration-300"
                  style={{ width: `${progress}%` }}
                />
              </div>
              <span className="text-xs text-blue-600 dark:text-blue-400 shrink-0">
                {progress}%
              </span>
            </div>
          ) : (
            <span className="text-sm text-blue-800 dark:text-blue-200">
              Update available: v{update.version}
            </span>
          )}
          {error && (
            <span className="text-xs text-red-600 dark:text-red-400 ml-2">
              {error}
            </span>
          )}
        </div>
        <div className="flex items-center gap-2 shrink-0">
          {!installing && (
            <>
              <Button
                size="sm"
                variant="default"
                className="h-7 text-xs bg-blue-600 hover:bg-blue-700 text-white"
                onClick={handleInstall}
              >
                Install Now
              </Button>
              <button
                onClick={() => setDismissed(true)}
                className="text-blue-600 dark:text-blue-400 hover:text-blue-800 dark:hover:text-blue-200"
                aria-label="Dismiss update"
              >
                <X className="h-4 w-4" />
              </button>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
