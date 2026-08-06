/**
 * Modest protection for the one anonymous-reachable data surface the per-leaf
 * public-visualization opt-in creates: GET /api/viz/results (design §4.7).
 * Every request on that route's denied-caller fallback drives admin-keyed
 * calls into the head (a leaf read, then a results read), so the public path
 * is bounded two ways:
 *
 *   - a per-IP fixed-window rate limit, so one client cannot turn the
 *     dashboard into an amplifier against the head; and
 *   - short-TTL in-memory caches for the leaf's public verdict and for the
 *     replay payload itself (a submitted result is immutable, and the
 *     visualize page is already ISR-cached for 60s, so 60s of staleness here
 *     changes nothing a viewer can observe).
 *
 * The caches mean an operator's results_visibility flip takes up to
 * LEAF_VERDICT_TTL_MS to reach anonymous callers — in both directions.
 *
 * All state is in-memory and per-process: the dashboard runs as a single
 * container, and the goal is bounding casual abuse and accidental load, not
 * distributed-attack resistance. Authenticated owner/admin traffic never
 * touches any of this.
 */

type CacheEntry<T> = { value: T; expires: number };

class TtlCache<T> {
  private entries = new Map<string, CacheEntry<T>>();

  constructor(
    private readonly ttlMs: number,
    private readonly maxEntries: number,
  ) {}

  get(key: string): T | undefined {
    const entry = this.entries.get(key);
    if (!entry) return undefined;
    if (Date.now() >= entry.expires) {
      this.entries.delete(key);
      return undefined;
    }
    return entry.value;
  }

  set(key: string, value: T): void {
    // Bound memory: evict the oldest insertion when full (Map preserves
    // insertion order). Precise LRU is not worth the bookkeeping here.
    if (!this.entries.has(key) && this.entries.size >= this.maxEntries) {
      const oldest = this.entries.keys().next().value;
      if (oldest !== undefined) this.entries.delete(oldest);
    }
    this.entries.delete(key);
    this.entries.set(key, { value, expires: Date.now() + this.ttlMs });
  }

  clear(): void {
    this.entries.clear();
  }
}

class FixedWindowRateLimiter {
  private windows = new Map<string, { windowStart: number; count: number }>();

  constructor(
    private readonly limit: number,
    private readonly windowMs: number,
    private readonly maxKeys: number,
  ) {}

  /** Records a request for `key` and returns whether it is within the limit. */
  allow(key: string): boolean {
    const now = Date.now();
    const window = this.windows.get(key);
    if (!window || now - window.windowStart >= this.windowMs) {
      if (!window && this.windows.size >= this.maxKeys) {
        const oldest = this.windows.keys().next().value;
        if (oldest !== undefined) this.windows.delete(oldest);
      }
      this.windows.delete(key);
      this.windows.set(key, { windowStart: now, count: 1 });
      return true;
    }
    window.count += 1;
    return window.count <= this.limit;
  }

  clear(): void {
    this.windows.clear();
  }
}

// 120 requests/min per IP: the visualize page steps through up to 50 work
// units, so a real anonymous viewer stays well under this even replaying
// briskly, while a scraper is capped at ~2 head calls/second.
const ANON_LIMIT_PER_WINDOW = 120;
const ANON_WINDOW_MS = 60_000;
const ANON_MAX_TRACKED_IPS = 10_000;

const LEAF_VERDICT_TTL_MS = 30_000;
const RESULT_TTL_MS = 60_000;
const MAX_CACHED_ENTRIES = 500;

export const anonVizRateLimiter = new FixedWindowRateLimiter(
  ANON_LIMIT_PER_WINDOW,
  ANON_WINDOW_MS,
  ANON_MAX_TRACKED_IPS,
);

/** leafId -> is this leaf publicly replayable right now (design §4.7)? */
export const publicLeafVerdictCache = new TtlCache<boolean>(
  LEAF_VERDICT_TTL_MS,
  MAX_CACHED_ENTRIES,
);

/** "leafId|workUnitId|volunteerId" -> the { result } payload served publicly. */
export const publicResultCache = new TtlCache<{ result: unknown }>(
  RESULT_TTL_MS,
  MAX_CACHED_ENTRIES,
);

/**
 * Best-effort client address for rate-limit bucketing: the first hop in
 * X-Forwarded-For (the reverse proxy in front of the dashboard sets it).
 * Absent the header (local dev), all callers share one bucket — fine, the
 * limiter only guards the anonymous public path.
 */
export function clientIpFrom(headers: { get(name: string): string | null }): string {
  const forwarded = headers.get("x-forwarded-for");
  const first = forwarded?.split(",")[0]?.trim();
  return first || "unknown";
}

/** Test hook: drop all guard state so cases cannot bleed into each other. */
export function resetPublicVizGuard(): void {
  anonVizRateLimiter.clear();
  publicLeafVerdictCache.clear();
  publicResultCache.clear();
}
