import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import {
  useHistory,
  filtersToParams,
  applyClientFilters,
  hasClientFilter,
  HISTORY_PAGE_SIZE,
  type HistoryFilters,
} from "./use-history";
import type { HistoryEntry, HistoryResponse, ManagementClient } from "@/api/client";

// Mock use-api
const mockHistory = vi.fn<(params?: any) => Promise<HistoryResponse>>();
const mockClient = { history: mockHistory } as unknown as ManagementClient;

vi.mock("./use-api", () => ({
  useClient: () => ({ client: mockClient, error: null }),
}));

function defaultFilters(overrides: Partial<HistoryFilters> = {}): HistoryFilters {
  return {
    dateRange: "all",
    headAccepted: "all",
    ...overrides,
  };
}

function entry(overrides: Partial<HistoryEntry> = {}): HistoryEntry {
  return {
    work_unit_id: "wu-" + Math.random().toString(36).slice(2, 10),
    leaf_name: "Test",
    completed_at: new Date().toISOString(),
    duration_seconds: 60,
    cpu_seconds: 50,
    credit_earned: 0,
    validation_status: "accepted",
    head_name: "lettuce.science",
    ...overrides,
  };
}

function makeResponse(
  entries: HistoryEntry[],
  nextCursor = "",
  hasMore = false
): HistoryResponse {
  return {
    entries,
    pagination: { next_cursor: nextCursor, has_more: hasMore },
  };
}

describe("useHistory", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("fetches initial page on mount", async () => {
    mockHistory.mockResolvedValueOnce(makeResponse([entry({ work_unit_id: "wu-1" })]));

    const { result } = renderHook(() => useHistory(defaultFilters()));

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.entries).toHaveLength(1);
    expect(result.current.entries[0].work_unit_id).toBe("wu-1");
    expect(result.current.loadedCount).toBe(1);
    expect(result.current.hasMore).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("sets hasMore when pagination indicates more pages", async () => {
    mockHistory.mockResolvedValueOnce(
      makeResponse([entry({ work_unit_id: "wu-1" })], "50", true)
    );

    const { result } = renderHook(() => useHistory(defaultFilters()));

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.hasMore).toBe(true);
  });

  it("appends entries on loadMore and passes the cursor", async () => {
    mockHistory.mockResolvedValueOnce(
      makeResponse([entry({ work_unit_id: "wu-1" })], "1", true)
    );

    const { result } = renderHook(() => useHistory(defaultFilters()));

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.entries).toHaveLength(1);

    mockHistory.mockResolvedValueOnce(makeResponse([entry({ work_unit_id: "wu-2" })]));

    act(() => {
      result.current.loadMore();
    });

    await waitFor(() => {
      expect(result.current.entries).toHaveLength(2);
    });

    expect(result.current.entries[0].work_unit_id).toBe("wu-1");
    expect(result.current.entries[1].work_unit_id).toBe("wu-2");
    expect(result.current.loadedCount).toBe(2);
    expect(mockHistory.mock.calls[1][0].cursor).toBe("1");
    expect(result.current.hasMore).toBe(false);
  });

  it("never sends the leaf name as the daemon's leaf_id filter", async () => {
    mockHistory.mockResolvedValueOnce(makeResponse([]));

    renderHook(() => useHistory(defaultFilters({ leafName: "Beyblade Arena" })));

    await waitFor(() => {
      expect(mockHistory).toHaveBeenCalled();
    });

    const params = mockHistory.mock.calls[0][0];
    expect(params.leaf_id).toBeUndefined();
  });

  it("filters entries by leaf name client-side", async () => {
    mockHistory.mockResolvedValueOnce(
      makeResponse([
        entry({ work_unit_id: "wu-1", leaf_name: "Alpha" }),
        entry({ work_unit_id: "wu-2", leaf_name: "Beta" }),
        entry({ work_unit_id: "wu-3", leaf_name: "Alpha" }),
      ])
    );

    const { result } = renderHook(() => useHistory(defaultFilters({ leafName: "Alpha" })));

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.entries.map((e) => e.work_unit_id)).toEqual(["wu-1", "wu-3"]);
    // Counts and leaf names reflect everything the daemon returned
    expect(result.current.loadedCount).toBe(3);
    expect(result.current.leafNames).toEqual(["Alpha", "Beta"]);
  });

  it("passes from date for 7d filter", async () => {
    mockHistory.mockResolvedValueOnce(makeResponse([]));

    renderHook(() => useHistory(defaultFilters({ dateRange: "7d" })));

    await waitFor(() => {
      expect(mockHistory).toHaveBeenCalled();
    });

    const params = mockHistory.mock.calls[0][0];
    expect(params.from).toBeDefined();
    // from should be roughly 7 days ago
    const from = new Date(params.from);
    const daysAgo = (Date.now() - from.getTime()) / (1000 * 60 * 60 * 24);
    expect(daysAgo).toBeGreaterThanOrEqual(6.9);
    expect(daysAgo).toBeLessThanOrEqual(7.1);
  });

  it("filters entries by head acceptance client-side", async () => {
    mockHistory.mockResolvedValueOnce(
      makeResponse([
        entry({ work_unit_id: "wu-1", validation_status: "accepted" }),
        entry({ work_unit_id: "wu-2", validation_status: "rejected" }),
      ])
    );

    const { result } = renderHook(() =>
      useHistory(defaultFilters({ headAccepted: "rejected" }))
    );

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.entries).toHaveLength(1);
    expect(result.current.entries[0].work_unit_id).toBe("wu-2");
  });

  it("walks further pages when a client-side filter matches nothing on the first", async () => {
    mockHistory
      .mockResolvedValueOnce(makeResponse([entry({ leaf_name: "Beta" })], "1", true))
      .mockResolvedValueOnce(makeResponse([entry({ leaf_name: "Beta" })], "2", true))
      .mockResolvedValueOnce(
        makeResponse([entry({ work_unit_id: "wu-alpha", leaf_name: "Alpha" })], "3", true)
      );

    const { result } = renderHook(() => useHistory(defaultFilters({ leafName: "Alpha" })));

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(mockHistory).toHaveBeenCalledTimes(3);
    expect(mockHistory.mock.calls[1][0].cursor).toBe("1");
    expect(mockHistory.mock.calls[2][0].cursor).toBe("2");
    expect(result.current.entries.map((e) => e.work_unit_id)).toEqual(["wu-alpha"]);
    expect(result.current.loadedCount).toBe(3);
    expect(result.current.hasMore).toBe(true);
  });

  it("keeps every leaf name seen, even after narrowing to one leaf", async () => {
    mockHistory.mockResolvedValueOnce(
      makeResponse([entry({ leaf_name: "Alpha" }), entry({ leaf_name: "Beta" })])
    );

    const { result, rerender } = renderHook((filters: HistoryFilters) => useHistory(filters), {
      initialProps: defaultFilters(),
    });

    await waitFor(() => {
      expect(result.current.leafNames).toEqual(["Alpha", "Beta"]);
    });

    mockHistory.mockResolvedValueOnce(makeResponse([entry({ leaf_name: "Alpha" })]));
    rerender(defaultFilters({ leafName: "Alpha" }));

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
      expect(result.current.entries).toHaveLength(1);
    });

    expect(result.current.leafNames).toEqual(["Alpha", "Beta"]);
  });

  it("sets error on fetch failure", async () => {
    mockHistory.mockRejectedValueOnce(new Error("Network error"));

    const { result } = renderHook(() => useHistory(defaultFilters()));

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.error).toBeTruthy();
    expect(result.current.error?.message).toBe("Network error");
  });

  it("does not call loadMore when hasMore is false", async () => {
    mockHistory.mockResolvedValueOnce(makeResponse([], "", false));

    const { result } = renderHook(() => useHistory(defaultFilters()));

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    // loadMore should be a no-op when hasMore is false
    act(() => {
      result.current.loadMore();
    });

    // Only the initial call should have been made
    expect(mockHistory).toHaveBeenCalledTimes(1);
  });
});

describe("applyClientFilters", () => {
  const entries = [
    entry({ work_unit_id: "a1", leaf_name: "Alpha", validation_status: "accepted" }),
    entry({ work_unit_id: "a2", leaf_name: "Alpha", validation_status: "rejected" }),
    entry({ work_unit_id: "b1", leaf_name: "Beta", validation_status: "accepted" }),
  ];

  it("returns everything when no client filter is set", () => {
    expect(applyClientFilters(entries, { headAccepted: "all" })).toHaveLength(3);
    expect(hasClientFilter({ headAccepted: "all" })).toBe(false);
  });

  it("matches the leaf name exactly", () => {
    const out = applyClientFilters(entries, { leafName: "Alpha", headAccepted: "all" });
    expect(out.map((e) => e.work_unit_id)).toEqual(["a1", "a2"]);
    expect(hasClientFilter({ leafName: "Alpha", headAccepted: "all" })).toBe(true);
  });

  it("combines leaf name and head acceptance", () => {
    const out = applyClientFilters(entries, { leafName: "Alpha", headAccepted: "accepted" });
    expect(out.map((e) => e.work_unit_id)).toEqual(["a1"]);
  });
});

describe("filtersToParams", () => {
  it("requests the daemon's page size", () => {
    const params = filtersToParams(defaultFilters());
    expect(params.limit).toBe(HISTORY_PAGE_SIZE);
    expect(params.limit).toBe(50);
  });

  it("does not set leaf_id: the daemon filters by leaf ID and the app only has names", () => {
    const params = filtersToParams(defaultFilters({ leafName: "Alpha" }));
    expect(params.leaf_id).toBeUndefined();
  });

  it("sets from to ~7 days ago for dateRange '7d'", () => {
    const params = filtersToParams(defaultFilters({ dateRange: "7d" }));
    expect(params.from).toBeDefined();
    const from = new Date(params.from!);
    const daysAgo = (Date.now() - from.getTime()) / (1000 * 60 * 60 * 24);
    expect(daysAgo).toBeGreaterThanOrEqual(6.9);
    expect(daysAgo).toBeLessThanOrEqual(7.1);
  });

  it("sets from to ~30 days ago for dateRange '30d'", () => {
    const params = filtersToParams(defaultFilters({ dateRange: "30d" }));
    expect(params.from).toBeDefined();
    const from = new Date(params.from!);
    const daysAgo = (Date.now() - from.getTime()) / (1000 * 60 * 60 * 24);
    expect(daysAgo).toBeGreaterThanOrEqual(29.9);
    expect(daysAgo).toBeLessThanOrEqual(30.1);
  });

  it("does not set from or to for dateRange 'all'", () => {
    const params = filtersToParams(defaultFilters());
    expect(params.from).toBeUndefined();
    expect(params.to).toBeUndefined();
  });

  it("uses customFrom and customTo for dateRange 'custom'", () => {
    const params = filtersToParams(
      defaultFilters({
        dateRange: "custom",
        customFrom: "2026-01-01T00:00:00Z",
        customTo: "2026-01-31T23:59:59Z",
      })
    );
    expect(params.from).toBe("2026-01-01T00:00:00Z");
    expect(params.to).toBe("2026-01-31T23:59:59Z");
  });

  it("handles custom range with only from set", () => {
    const params = filtersToParams(
      defaultFilters({ dateRange: "custom", customFrom: "2026-01-01T00:00:00Z" })
    );
    expect(params.from).toBe("2026-01-01T00:00:00Z");
    expect(params.to).toBeUndefined();
  });

  it("handles custom range with only to set", () => {
    const params = filtersToParams(
      defaultFilters({ dateRange: "custom", customTo: "2026-01-31T23:59:59Z" })
    );
    expect(params.from).toBeUndefined();
    expect(params.to).toBe("2026-01-31T23:59:59Z");
  });

  it("handles custom range with neither from nor to set", () => {
    const params = filtersToParams(defaultFilters({ dateRange: "custom" }));
    expect(params.from).toBeUndefined();
    expect(params.to).toBeUndefined();
  });
});
