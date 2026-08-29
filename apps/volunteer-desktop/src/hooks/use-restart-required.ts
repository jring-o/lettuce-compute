import { useEffect, useRef, useSyncExternalStore } from "react";
import { restartDaemon } from "@/api/client";

/**
 * The one record of "a saved change is waiting for Lettuce to restart".
 *
 * Some settings are read by the volunteer daemon only when it starts (see
 * `needsRestart` in `use-config.ts`), the daemon can say so itself with
 * `restart_required` in its answer to `PUT /api/v1/config`, and a head's
 * runtime trust or a newly attached head is likewise picked up on the next
 * start. Whatever the page that saved it, the fact belongs to the app as a
 * whole: it must outlive that page (pages unmount on a tab switch) and be
 * shown wherever the volunteer is. This module holds it, as a small store
 * outside React; `RestartRequiredBanner` renders it, and the in-app restart
 * clears it.
 *
 * Each entry is a plain-language reason, kept distinct so a volunteer who
 * changed trust and then a resource limit sees both.
 */
interface RestartState {
  /** Reasons a restart is pending, in the order they were recorded; empty when none. */
  reasons: readonly string[];
  /** How many in-app restarts have completed; lets data hooks refetch afterwards. */
  restarts: number;
}

/** The reason recorded when a restart-only setting is saved from Settings. */
export const RESTART_ONLY_SETTINGS_REASON =
  "A setting you saved is one Lettuce reads only when it starts (resource limits, schedule, thermal limits, concurrent tasks or log level). It is saved, but the running daemon is still using the old value.";

/** The reason recorded when the daemon itself flags `restart_required`. */
export const DAEMON_FLAGGED_REASON =
  "Lettuce reported that a setting you saved takes effect the next time it starts.";

let state: RestartState = { reasons: [], restarts: 0 };
const listeners = new Set<() => void>();

function setState(next: RestartState) {
  state = next;
  for (const l of listeners) l();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function read() {
  return state;
}

/** Record that `reason` is waiting for a restart. Repeats of the same reason are ignored. */
export function markRestartRequired(reason: string): void {
  if (state.reasons.includes(reason)) return;
  setState({ ...state, reasons: [...state.reasons, reason] });
}

/** Forget every pending reason, without restarting (the volunteer dismissed the notice). */
export function clearRestartRequired(): void {
  if (state.reasons.length === 0) return;
  setState({ ...state, reasons: [] });
}

/**
 * Restart the daemon from the app (`restartDaemon()`: graceful stop, forced
 * after 30 s, then a fresh start). On success the pending reasons are cleared
 * and every `useOnDaemonRestart` subscriber is told, so pages refetch what
 * the new daemon reports. Rejects with the host's message when the restart
 * fails; the pending reasons are then kept.
 */
export async function restartLettuce(): Promise<void> {
  await restartDaemon();
  setState({ reasons: [], restarts: state.restarts + 1 });
}

/** Whether a saved change is waiting for a daemon restart, and why. */
export function useRestartRequired(): {
  restartRequired: boolean;
  reasons: readonly string[];
  clearRestartRequired: () => void;
} {
  const current = useSyncExternalStore(subscribe, read);
  return {
    restartRequired: current.reasons.length > 0,
    reasons: current.reasons,
    clearRestartRequired,
  };
}

/**
 * Run `callback` after each in-app restart completes (not on mount). Data
 * hooks use it to refetch, since the restarted daemon re-reads its config
 * and reconnects to its heads.
 */
export function useOnDaemonRestart(callback: () => void): void {
  const restarts = useSyncExternalStore(subscribe, () => state.restarts);
  const callbackRef = useRef(callback);
  callbackRef.current = callback;
  const seen = useRef(restarts);
  useEffect(() => {
    if (restarts === seen.current) return;
    seen.current = restarts;
    callbackRef.current();
  }, [restarts]);
}

/** Visible for tests: forget every reason and the restart count. */
export function resetRestartRequiredForTest(): void {
  setState({ reasons: [], restarts: 0 });
}
