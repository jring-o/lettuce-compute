import { useCallback, useEffect, useState } from "react";
import { ApiError, type Notice } from "../api/client";
import { useClient } from "./use-api";

/** Notices kept in memory; older ones fall off the end. */
const MAX_NOTICES = 100;

/** localStorage key under which dismissed episode keys are kept. */
const DISMISSED_STORAGE_KEY = "lettuce.notices.dismissed";

/**
 * Notice state shared across mounts of the hook, so leaving the Overview tab
 * and coming back neither re-shows dismissed notices nor repeats work. It
 * lives for the web view; dismissals also live in localStorage, so a reload
 * or an app restart does not resurrect a warning the person already closed.
 */
interface NoticesCache {
  /** Live warn and error notices from the last poll, newest (highest id) first. */
  notices: Notice[];
  /** False once the daemon answered 404: this CLI build has no notices route. */
  supported: boolean;
  /** Episode keys (see noticeKey) of notices the user dismissed. */
  dismissed: Set<string>;
}

const cache: NoticesCache = {
  notices: [],
  supported: true,
  dismissed: loadDismissed(),
};

/**
 * Visible for tests: forget everything held in memory, as if the web view had
 * just loaded — dismissals persisted in localStorage are read back, as they
 * would be.
 */
export function resetNoticesCacheForTest(): void {
  cache.notices = [];
  cache.supported = true;
  cache.dismissed = loadDismissed();
}

/**
 * A notice's episode: the condition (code, head, leaf) and when this
 * occurrence of it began. The daemon refreshes a repeating condition in
 * place (same id and first_at, newer at and count), so dismissing an episode
 * keeps it dismissed however often it repeats; a condition that returns
 * after ending is a new episode with a new first_at, and shows again.
 */
export function noticeKey(n: Notice): string {
  return `${n.code}|${n.head ?? ""}|${n.leaf ?? ""}|${n.first_at}`;
}

function loadDismissed(): Set<string> {
  try {
    const raw = localStorage.getItem(DISMISSED_STORAGE_KEY);
    if (!raw) return new Set();
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return new Set();
    return new Set(parsed.filter((k): k is string => typeof k === "string"));
  } catch {
    // Storage unavailable or unreadable: dismissals last for this web view only.
    return new Set();
  }
}

function saveDismissed(keys: Set<string>): void {
  try {
    localStorage.setItem(DISMISSED_STORAGE_KEY, JSON.stringify(Array.from(keys)));
  } catch {
    // Storage unavailable: dismissals last for this web view only.
  }
}

/**
 * The notices worth showing from one poll of the daemon's whole ring: warnings
 * and errors whose condition has not ended, newest first.
 */
export function liveNotices(incoming: Notice[]): Notice[] {
  return incoming
    .filter((n) => n.level !== "info" && !n.resolved_at)
    .sort((a, b) => b.id - a.id)
    .slice(0, MAX_NOTICES);
}

/**
 * Forget dismissals of episodes the daemon no longer retains — they fell off
 * its ring, or it restarted — so the stored set never grows past the ring.
 */
function pruneDismissed(retained: Notice[]): void {
  const present = new Set(retained.map(noticeKey));
  let changed = false;
  for (const key of cache.dismissed) {
    if (!present.has(key)) {
      cache.dismissed.delete(key);
      changed = true;
    }
  }
  if (changed) saveDismissed(cache.dismissed);
}

export interface NoticesView {
  /** Live warn/error notices the user has not dismissed, newest first. */
  notices: Notice[];
  /** False when the running CLI build has no `GET /api/v1/notices`. */
  supported: boolean;
  dismiss: (notice: Notice) => void;
}

/**
 * Poll `GET /api/v1/notices` every `intervalMs` for warnings and errors the
 * daemon wants the volunteer to see. Every poll reads the whole ring (at most
 * a hundred entries) rather than only ids newer than the last one, because a
 * notice changes after it is created: the daemon refreshes a repeating
 * condition in place and marks a condition that has ended `resolved_at`, and
 * a panel that only ever heard about new ids showed a "no work" warning for
 * twelve hours beside a machine that was working. A 404 means the CLI build
 * predates the route; polling stops and `supported` turns false so the panel
 * can hide.
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
        const resp = await client.notices();
        if (cancelled) return;
        pruneDismissed(resp.notices);
        cache.notices = liveNotices(resp.notices);
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
    saveDismissed(cache.dismissed);
    setDismissedVersion((v) => v + 1);
  }, []);

  // dismissedVersion is read so the filtered list recomputes after a dismiss.
  void dismissedVersion;
  const visible = notices.filter((n) => !cache.dismissed.has(noticeKey(n)));

  return { notices: visible, supported, dismiss };
}
