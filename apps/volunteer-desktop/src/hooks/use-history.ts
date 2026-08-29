import { useCallback, useEffect, useRef, useState } from "react";
import { useClient } from "./use-api";
import type { HistoryEntry, HistoryParams } from "../api/client";

export interface HistoryFilters {
  leafId?: string;
  dateRange: "7d" | "30d" | "all" | "custom";
  customFrom?: string;
  customTo?: string;
  validationStatus: "all" | "accepted" | "pending" | "rejected";
}

export function filtersToParams(filters: HistoryFilters): Omit<HistoryParams, "cursor"> {
  const params: Omit<HistoryParams, "cursor"> = { limit: 50 };
  if (filters.leafId) {
    params.leaf_id = filters.leafId;
  }

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

export function useHistory(filters: HistoryFilters) {
  const { client } = useClient();
  const [entries, setEntries] = useState<HistoryEntry[]>([]);
  const [hasMore, setHasMore] = useState(false);
  const [isLoading, setIsLoading] = useState(true);
  const [error, setError] = useState<Error | null>(null);
  const cursorRef = useRef<string | undefined>(undefined);
  const filtersRef = useRef(filters);

  // Reset when filters change
  useEffect(() => {
    filtersRef.current = filters;
    cursorRef.current = undefined;
    setEntries([]);
    setHasMore(false);
    setIsLoading(true);
    setError(null);
  }, [filters.leafId, filters.dateRange, filters.customFrom, filters.customTo, filters.validationStatus]);

  // Fetch entries
  const fetchPage = useCallback(
    async (append: boolean) => {
      if (!client) return;
      setIsLoading(true);
      try {
        const params = filtersToParams(filtersRef.current);
        const resp = await client.history({
          ...params,
          cursor: append ? cursorRef.current : undefined,
        });

        let filtered = resp.entries;
        // Client-side filter for validation status (API doesn't filter this)
        if (filtersRef.current.validationStatus !== "all") {
          filtered = filtered.filter(
            (e) => e.validation_status === filtersRef.current.validationStatus
          );
        }

        if (append) {
          setEntries((prev) => [...prev, ...filtered]);
        } else {
          setEntries(filtered);
        }
        cursorRef.current = resp.pagination.next_cursor;
        setHasMore(resp.pagination.has_more);
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err : new Error(String(err)));
      } finally {
        setIsLoading(false);
      }
    },
    [client]
  );

  // Initial fetch when client or filters change
  useEffect(() => {
    fetchPage(false);
  }, [fetchPage, filters.leafId, filters.dateRange, filters.customFrom, filters.customTo, filters.validationStatus]);

  const loadMore = useCallback(() => {
    if (hasMore && !isLoading) {
      fetchPage(true);
    }
  }, [hasMore, isLoading, fetchPage]);

  return { entries, hasMore, isLoading, loadMore, error };
}
