import { useCallback, useState } from "react";
import { useApiQuery } from "./use-api";
import type { ConfigResponse, ConfigUpdate } from "../api/client";
import { useClient } from "./use-api";
import {
  DAEMON_FLAGGED_REASON,
  RESTART_ONLY_SETTINGS_REASON,
  markRestartRequired,
  useOnDaemonRestart,
} from "./use-restart-required";

/**
 * Settings the daemon reads only when it starts. `PUT /api/v1/config` saves
 * them and `Daemon.ApplyConfig` swaps the config pointer, but the parts of
 * the daemon that consume these were built from the old values:
 *
 * - `scheduling`: `resource.NewScheduler` copies mode, idle threshold and the
 *   schedule ranges at construction and is never rebuilt.
 * - `thermal`: copied into the thermal monitor's config at construction.
 * - `max_concurrent_tasks`: the slot count is fixed at start (the daemon logs
 *   "restart daemon to apply").
 * - `log_level`: the logger's level is parsed once at start.
 *
 * `resource_limits` is live since the client's TB-79 fix: `ApplyConfig`
 * rebuilds the hardware profile heads are told (carried on the next poll),
 * admission books against the new figures at once, and the runtimes read the
 * new memory ceiling for the next task. Running tasks keep the ceilings they
 * started with. `work_buffer_hours`, `notifications`, `leafs` and per-head
 * weights and leaf preferences are read live as well. `yield` is live since
 * TB-90: `ApplyConfig` hands the block to the running monitor, which judges
 * its next sample against the new thresholds, starts sampling when the
 * setting is turned on and releases a pause it holds when it is turned off.
 */
const RESTART_ONLY_KEYS: ReadonlyArray<keyof ConfigUpdate> = [
  "scheduling",
  "thermal",
  "max_concurrent_tasks",
  "log_level",
];

/** True when `partial` changes at least one restart-only setting. */
export function needsRestart(partial: ConfigUpdate): boolean {
  return RESTART_ONLY_KEYS.some((key) => partial[key] !== undefined);
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

  // A restarted daemon re-reads config.yaml; show what it now holds.
  useOnDaemonRestart(refetch);

  const updateConfig = useCallback(
    async (partial: ConfigUpdate) => {
      if (!client) return;
      setSaving(true);
      try {
        const resp = await client.updateConfig(partial);
        if (needsRestart(partial)) {
          markRestartRequired(RESTART_ONLY_SETTINGS_REASON);
        } else if (resp?.restart_required === true) {
          markRestartRequired(DAEMON_FLAGGED_REASON);
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
