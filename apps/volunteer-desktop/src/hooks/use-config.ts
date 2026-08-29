import { useCallback, useState, useSyncExternalStore } from "react";
import { useApiQuery } from "./use-api";
import type { ConfigResponse, ConfigUpdate } from "../api/client";
import { useClient } from "./use-api";

/**
 * Settings the daemon reads only when it starts. `PUT /api/v1/config` saves
 * them and `Daemon.ApplyConfig` swaps the config pointer, but the parts of
 * the daemon that consume these were built from the old values:
 *
 * - `scheduling`: `resource.NewScheduler` copies mode, idle threshold and the
 *   schedule ranges at construction and is never rebuilt.
 * - `thermal`: copied into the thermal monitor's config at construction.
 * - `resource_limits`: the hardware profile advertised to heads (which decides
 *   what work they offer) is detected once at start, and the native process
 *   limiter holds the original limits struct.
 * - `max_concurrent_tasks`: the slot count is fixed at start (the daemon logs
 *   "restart daemon to apply").
 * - `log_level`: the logger's level is parsed once at start.
 *
 * `work_buffer_hours`, `notifications`, `leafs` and per-head weights and leaf
 * preferences are read live and need no restart.
 */
const RESTART_ONLY_KEYS: ReadonlyArray<keyof ConfigUpdate> = [
  "scheduling",
  "thermal",
  "resource_limits",
  "max_concurrent_tasks",
  "log_level",
];

/** True when `partial` changes at least one restart-only setting. */
export function needsRestart(partial: ConfigUpdate): boolean {
  return RESTART_ONLY_KEYS.some((key) => partial[key] !== undefined);
}

// A "restart required" flag shared by every mount of the hook: it must
// outlive the Settings page (which unmounts on a tab switch) and is cleared
// only when the daemon is restarted from the app.
let restartRequired = false;
const restartListeners = new Set<() => void>();

function setRestartRequired(value: boolean) {
  if (restartRequired === value) return;
  restartRequired = value;
  for (const l of restartListeners) l();
}

function subscribeRestart(listener: () => void) {
  restartListeners.add(listener);
  return () => {
    restartListeners.delete(listener);
  };
}

function readRestart() {
  return restartRequired;
}

/** Whether a saved setting is waiting for a daemon restart to take effect. */
export function useRestartRequired(): {
  restartRequired: boolean;
  markRestartRequired: () => void;
  clearRestartRequired: () => void;
} {
  const value = useSyncExternalStore(subscribeRestart, readRestart);
  return {
    restartRequired: value,
    markRestartRequired: () => setRestartRequired(true),
    clearRestartRequired: () => setRestartRequired(false),
  };
}

/** Visible for tests. */
export function resetRestartRequiredForTest(): void {
  setRestartRequired(false);
}

export function useConfig() {
  const { client } = useClient();
  const {
    data: config,
    isLoading,
    error,
    refetch,
  } = useApiQuery<ConfigResponse>((c) => c.config());
  const [saving, setSaving] = useState(false);
  const [toast, setToast] = useState<string | null>(null);

  const updateConfig = useCallback(
    async (partial: ConfigUpdate) => {
      if (!client) return;
      setSaving(true);
      try {
        const resp = await client.updateConfig(partial);
        if (needsRestart(partial) || resp?.restart_required === true) {
          setRestartRequired(true);
        }
        refetch();
        setToast("Saved");
        setTimeout(() => setToast(null), 2000);
      } catch (err) {
        setToast(
          err instanceof Error ? `Error: ${err.message}` : "Save failed"
        );
        setTimeout(() => setToast(null), 3000);
      } finally {
        setSaving(false);
      }
    },
    [client, refetch]
  );

  return { config, isLoading, error, updateConfig, saving, toast, refetch };
}
