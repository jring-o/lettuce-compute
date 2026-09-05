import { useCallback, useEffect, useRef, useState } from "react";
import { useClient } from "./use-api";
import type { HistoryEntry, HistoryParams } from "../api/client";

/**
 * Filter on `validation_status`, which records whether the head ACCEPTED the
 * submission when it arrived. Validation and credit are decided later on the
 * head and are not reflected here.
 */
export type HeadAcceptedFilter = "all" | "accepted" | "rejected";

export interface HistoryFilters {
  /**
   * Leaf display name to show. Applied client-side: the daemon's `leaf_id`
   * query parameter matches the leaf's ID (`daemon_bridge.GetHistory`), and
   * the name is all a history entry carries.
   */
  leafName?: string;
  dateRange: "7d" | "30d" | "all" | "custom";
  /** RFC 3339, used when `dateRange` is "custom". */
  customFrom?: string;
  /** RFC 3339, used when `dateRange` is "custom". */
  customTo?: string;
  /** Applied client-side; the daemon does not filter on it. */
  headAccepted: HeadAcceptedFilter;
}

/** Page size requested from the daemon (its default; maximum is 200). */
export const HISTORY_PAGE_SIZE = 50;

/**
 * Most pages one `loadMore` call walks through looking for a matching entry
 * when a client-side filter is active. Bounds the work of a filter that
 * matches nothing in a long history; the infinite scroll keeps going after.
 */
const MAX_PAGES_PER_LOAD = 5;

/** The daemon-side query for a filter set: only the date window is server-side. */
export function filtersToParams(filters: HistoryFilters): Omit<HistoryParams, "cursor"> {
  const params: Omit<HistoryParams, "cursor"> = { limit: HISTORY_PAGE_SIZE };

  const now = new Date();
  if (filters.dateRange === "7d") {
    const from = new Date(now);
    from.setDate(from.getDate() - 7);
    params.from = from.toISOString();
  } else if (filters.dateRange === "30d") {
    const from = new Date(now);
    from.setDate(from.getDate() - 30);
    params.from = from.toISOString();
  } else if (filters.dateRange === "custom") {
    if (filters.customFrom) params.from = filters.customFrom;
    if (filters.customTo) params.to = filters.customTo;
  }

  return params;
}

/** The client-side half of the filter set: leaf name and head acceptance. */
export function applyClientFilters(
  entries: HistoryEntry[],
  filters: Pick<HistoryFilters, "leafName" | "headAccepted">
): HistoryEntry[] {
  return entries.filter((e) => {
    if (filters.leafName && e.leaf_name !== filters.leafName) return false;
    if (filters.headAccepted !== "all" && e.validation_status !== filters.headAccepted) {
      return false;
    }
    return true;
  });
}

/** True when a filter narrows the daemon's rows client-side. */
export function hasClientFilter(
  filters: Pick<HistoryFilters, "leafName" | "headAccepted">
): boolean {
  return Boolean(filters.leafName) || filters.headAccepted !== "all";
}

export interface HistoryState {
  /** Entries matching every filter, newest first. */
  entries: HistoryEntry[];
  /** Entries received from the daemon for the current date window, before client-side filters. */
  loadedCount: number;
  /** Every leaf name seen in any page this hook has loaded, sorted. */
  leafNames: string[];
  hasMore: boolean;
  isLoading: boolean;
  loadMore: () => void;
  /**
   * Re-read the newest page and put what it holds that is not loaded yet
   * above the list, keeping every page loaded so far and the paging cursor.
   * If nothing loaded is on that page any more (more than a page of new
   * completions, or nothing was loaded), the list starts over from it.
   * Silent: no loading state, so a page that stays mounted between visits
   * can pick up new completions without dropping what the reader scrolled to.
   */
  refresh: () => void;
  error: Error | null;
}

export function useHistory(filters: HistoryFilters): HistoryState {
  const { client } = useClient();
  const [entries, setEntries] = useState<HistoryEntry[]>([]);
  const [loadedCount, setLoadedCount] = useState(0);
  const [leafNames, setLeafNames] = useState<string[]>([]);
  const [hasMore, setHasMore] = useState(false);
  const [isLoading, setIsLoading] = useState(true);
  const [error, setError] = useState<Error | null>(null);
  const cursorRef = useRef<string | undefined>(undefined);
  const filtersRef = useRef(filters);
  // Bumped whenever the filters change so a response to an older query is dropped.
  const generationRef = useRef(0);
  // Leaf names are a property of the history, not of the filter, so they are
  // never reset: selecting a leaf must not shrink the list to that one leaf.
  const seenLeafNamesRef = useRef<Set<string>>(new Set());
  // Every work unit the daemon has returned for the current filter set,
  // before client-side filters: how `refresh` tells new completions from
  // rows it already holds.
  const loadedIdsRef = useRef<Set<string>>(new Set());

  // Reset when filters change
  useEffect(() => {
    filtersRef.current = filters;
    cursorRef.current = undefined;
    generationRef.current += 1;
    loadedIdsRef.current = new Set();
    setEntries([]);
    setLoadedCount(0);
    setHasMore(false);
    setIsLoading(true);
    setError(null);
  }, [
    filters.leafName,
    filters.dateRange,
    filters.customFrom,
    filters.customTo,
    filters.headAccepted,
  ]);

  const fetchPage = useCallback(
    async (append: boolean) => {
      if (!client) return;
      const generation = generationRef.current;
      setIsLoading(true);
      try {
        const params = filtersToParams(filtersRef.current);
        let cursor = append ? cursorRef.current : undefined;
        const matched: HistoryEntry[] = [];
        let fetched = 0;
        let more = false;

        // Walk pages until one yields a visible entry (or history ends), so
        // a narrow client-side filter does not leave the list empty while
        // "load more" still has pages to show.
        for (let page = 0; page < MAX_PAGES_PER_LOAD; page++) {
          const resp = await client.history({ ...params, cursor });
          fetched += resp.entries.length;
          for (const e of resp.entries) {
            seenLeafNamesRef.current.add(e.leaf_name);
            loadedIdsRef.current.add(e.work_unit_id);
          }
          matched.push(...applyClientFilters(resp.entries, filtersRef.current));
          more = resp.pagination.has_more;
          cursor = resp.pagination.next_cursor;
          if (matched.length > 0 || !more) break;
        }

        if (generation !== generationRef.current) return; // filters changed meanwhile

        setEntries((prev) => (append ? [...prev, ...matched] : matched));
        setLoadedCount((prev) => (append ? prev + fetched : fetched));
        setLeafNames(Array.from(seenLeafNamesRef.current).sort());
        cursorRef.current = cursor;
        setHasMore(more);
        setError(null);
      } catch (err) {
        if (generation !== generationRef.current) return;
        setError(err instanceof Error ? err : new Error(String(err)));
      } finally {
        if (generation === generationRef.current) setIsLoading(false);
      }
    },
    [client]
  );

  // Initial fetch when client or filters change
  useEffect(() => {
    fetchPage(false);
  }, [
    fetchPage,
    filters.leafName,
    filters.dateRange,
    filters.customFrom,
    filters.customTo,
    filters.headAccepted,
  ]);

  const loadMore = useCallback(() => {
    if (hasMore && !isLoading) {
      fetchPage(true);
    }
  }, [hasMore, isLoading, fetchPage]);

  const refresh = useCallback(async () => {
    if (!client) return;
    const generation = generationRef.current;
    try {
      const resp = await client.history(filtersToParams(filtersRef.current));
      if (generation !== generationRef.current) return; // filters changed meanwhile
      for (const e of resp.entries) seenLeafNamesRef.current.add(e.leaf_name);
      const fresh = resp.entries.filter((e) => !loadedIdsRef.current.has(e.work_unit_id));

      if (fresh.length === resp.entries.length) {
        // Nothing loaded is on the newest page any more: start over from it.
        loadedIdsRef.current = new Set(resp.entries.map((e) => e.work_unit_id));
        setEntries(applyClientFilters(resp.entries, filtersRef.current));
        setLoadedCount(resp.entries.length);
        cursorRef.current = resp.pagination.next_cursor;
        setHasMore(resp.pagination.has_more);
      } else if (fresh.length > 0) {
        // The newest page overlaps what is loaded: the entries it holds that
        // are not loaded yet are newer than everything here — they go on top.
        for (const e of fresh) loadedIdsRef.current.add(e.work_unit_id);
        const matched = applyClientFilters(fresh, filtersRef.current);
        if (matched.length > 0) setEntries((prev) => [...matched, ...prev]);
        setLoadedCount((prev) => prev + fresh.length);
      }
      setLeafNames(Array.from(seenLeafNamesRef.current).sort());
      setError(null);
    } catch (err) {
      if (generation !== generationRef.current) return;
      setError(err instanceof Error ? err : new Error(String(err)));
    }
  }, [client]);

  return { entries, loadedCount, leafNames, hasMore, isLoading, loadMore, refresh, error };
}
