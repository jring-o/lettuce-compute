import { useCallback, useEffect, useState } from "react";
import { ApiError, type Notice } from "../api/client";
import { useClient } from "./use-api";

/** Notices kept in memory; older ones fall off the end. */
const MAX_NOTICES = 100;

/**
 * Notice state shared across mounts of the hook, so leaving the Overview tab
 * and coming back neither re-shows dismissed notices nor loses the polling
 * position. It lives for the app process, like the management client itself.
 */
interface NoticesCache {
  /** Warn and error notices, newest (highest id) first. */
  notices: Notice[];
  /** Highest notice id seen; the `since` of the next poll. 0 = nothing seen. */
  latestId: number;
  /** False once the daemon answered 404: this CLI build has no notices route. */
  supported: boolean;
  /** Keys (`id@at`) of notices the user dismissed. A repeat with a newer `at` returns. */
  dismissed: Set<string>;
}

const cache: NoticesCache = {
  notices: [],
  latestId: 0,
  supported: true,
  dismissed: new Set(),
};

/** Visible for tests: forget everything, as if the app had just started. */
export function resetNoticesCacheForTest(): void {
  cache.notices = [];
  cache.latestId = 0;
  cache.supported = true;
  cache.dismissed = new Set();
}

function noticeKey(n: Notice): string {
  return `${n.id}@${n.at}`;
}

/** Merge a poll's notices into `current`: replace by id, drop info, newest first. */
export function mergeNotices(current: Notice[], incoming: Notice[]): Notice[] {
  const byId = new Map<number, Notice>();
  for (const n of current) byId.set(n.id, n);
  for (const n of incoming) {
    if (n.level === "info") continue;
    byId.set(n.id, n);
  }
  return Array.from(byId.values())
    .sort((a, b) => b.id - a.id)
    .slice(0, MAX_NOTICES);
}

export interface NoticesView {
  /** Warn/error notices the user has not dismissed, newest first. */
  notices: Notice[];
  /** False when the running CLI build has no `GET /api/v1/notices`. */
  supported: boolean;
  dismiss: (notice: Notice) => void;
}

/**
 * Poll `GET /api/v1/notices` every `intervalMs` for warnings and errors the
 * daemon wants the volunteer to see. A 404 means the CLI build predates the
 * route; polling stops and `supported` turns false so the panel can hide.
 */
export function useNotices(intervalMs: number = 10000): NoticesView {
  const { client } = useClient();
  const [notices, setNotices] = useState<Notice[]>(cache.notices);
  const [supported, setSupported] = useState(cache.supported);
  const [dismissedVersion, setDismissedVersion] = useState(0);

  useEffect(() => {
    if (!client || !cache.supported) return;
    let cancelled = false;
    let timer: ReturnType<typeof setInterval> | undefined;

    const stop = () => {
      if (timer !== undefined) clearInterval(timer);
      timer = undefined;
    };

    const poll = async () => {
      if (!cache.supported) {
        stop();
        return;
      }
      try {
        let resp = await client.notices(cache.latestId > 0 ? cache.latestId : undefined);
        if (cancelled) return;
        if (resp.latest_id < cache.latestId) {
          // The daemon restarted and its ids began again: forget what was
          // seen and take everything it retains.
          cache.notices = [];
          cache.latestId = 0;
          resp = await client.notices(undefined);
          if (cancelled) return;
        }
        cache.notices = mergeNotices(cache.notices, resp.notices);
        cache.latestId = Math.max(cache.latestId, resp.latest_id);
        setNotices(cache.notices);
      } catch (err) {
        if (cancelled) return;
        if (err instanceof ApiError && err.status === 404) {
          cache.supported = false;
          setSupported(false);
          stop();
        }
        // Any other failure (daemon restarting, unreachable) is retried next tick.
      }
    };

    poll();
    if (intervalMs > 0) timer = setInterval(poll, intervalMs);
    return () => {
      cancelled = true;
      stop();
    };
  }, [client, intervalMs]);

  const dismiss = useCallback((notice: Notice) => {
    cache.dismissed.add(noticeKey(notice));
    setDismissedVersion((v) => v + 1);
  }, []);

  // dismissedVersion is read so the filtered list recomputes after a dismiss.
  void dismissedVersion;
  const visible = notices.filter((n) => !cache.dismissed.has(noticeKey(n)));

  return { notices: visible, supported, dismiss };
}
