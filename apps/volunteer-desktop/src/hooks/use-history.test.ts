import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import { useHistory, filtersToParams, type HistoryFilters } from "./use-history";
import type { HistoryResponse, ManagementClient } from "@/api/client";

// Mock use-api
const mockHistory = vi.fn<(params?: any) => Promise<HistoryResponse>>();
const mockClient = { history: mockHistory } as unknown as ManagementClient;

vi.mock("./use-api", () => ({
  useClient: () => ({ client: mockClient, error: null }),
}));

function defaultFilters(overrides: Partial<HistoryFilters> = {}): HistoryFilters {
  return {
    dateRange: "all",
    validationStatus: "all",
    ...overrides,
  };
}

function makeResponse(
  entries: HistoryResponse["entries"],
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
    mockHistory.mockResolvedValueOnce(
      makeResponse([
        {
          work_unit_id: "wu-1",
          leaf_name: "Test",
          completed_at: new Date().toISOString(),
          duration_seconds: 60,
          credit_earned: 10,
          validation_status: "accepted",
        },
      ])
    );

    const { result } = renderHook(() => useHistory(defaultFilters()));

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.entries).toHaveLength(1);
    expect(result.current.entries[0].work_unit_id).toBe("wu-1");
    expect(result.current.hasMore).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("sets hasMore when pagination indicates more pages", async () => {
    mockHistory.mockResolvedValueOnce(
      makeResponse(
        [
          {
            work_unit_id: "wu-1",
            leaf_name: "Test",
            completed_at: new Date().toISOString(),
            duration_seconds: 60,
            credit_earned: 10,
            validation_status: "accepted",
          },
        ],
        "50",
        true
      )
    );

    const { result } = renderHook(() => useHistory(defaultFilters()));

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.hasMore).toBe(true);
  });

  it("appends entries on loadMore", async () => {
    // First page
    mockHistory.mockResolvedValueOnce(
      makeResponse(
        [
          {
            work_unit_id: "wu-1",
            leaf_name: "Test",
            completed_at: new Date().toISOString(),
            duration_seconds: 60,
            credit_earned: 10,
            validation_status: "accepted",
          },
        ],
        "1",
        true
      )
    );

    const { result } = renderHook(() => useHistory(defaultFilters()));

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.entries).toHaveLength(1);

    // Second page
    mockHistory.mockResolvedValueOnce(
      makeResponse([
        {
          work_unit_id: "wu-2",
          leaf_name: "Test",
          completed_at: new Date().toISOString(),
          duration_seconds: 120,
          credit_earned: 20,
          validation_status: "accepted",
        },
      ])
    );

    act(() => {
      result.current.loadMore();
    });

    await waitFor(() => {
      expect(result.current.entries).toHaveLength(2);
    });

    expect(result.current.entries[0].work_unit_id).toBe("wu-1");
    expect(result.current.entries[1].work_unit_id).toBe("wu-2");
  });

  it("passes leaf_id filter to API", async () => {
    mockHistory.mockResolvedValueOnce(makeResponse([]));

    renderHook(() =>
      useHistory(defaultFilters({ leafId: "leaf-abc" }))
    );

    await waitFor(() => {
      expect(mockHistory).toHaveBeenCalled();
    });

    const params = mockHistory.mock.calls[0][0];
    expect(params.leaf_id).toBe("leaf-abc");
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

  it("filters entries by validation status client-side", async () => {
    mockHistory.mockResolvedValueOnce(
      makeResponse([
        {
          work_unit_id: "wu-1",
          leaf_name: "Test",
          completed_at: new Date().toISOString(),
          duration_seconds: 60,
          credit_earned: 10,
          validation_status: "accepted",
        },
        {
          work_unit_id: "wu-2",
          leaf_name: "Test",
          completed_at: new Date().toISOString(),
          duration_seconds: 60,
          credit_earned: 0,
          validation_status: "rejected",
        },
      ])
    );

    const { result } = renderHook(() =>
      useHistory(defaultFilters({ validationStatus: "accepted" }))
    );

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.entries).toHaveLength(1);
    expect(result.current.entries[0].validation_status).toBe("accepted");
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

describe("filtersToParams", () => {
  it("returns default limit of 50", () => {
    const params = filtersToParams({
      dateRange: "all",
      validationStatus: "all",
    });
    expect(params.limit).toBe(50);
  });

  it("sets leaf_id when leafId is provided", () => {
    const params = filtersToParams({
      dateRange: "all",
      validationStatus: "all",
      leafId: "proj-abc",
    });
    expect(params.leaf_id).toBe("proj-abc");
  });

  it("does not set leaf_id when leafId is undefined", () => {
    const params = filtersToParams({
      dateRange: "all",
      validationStatus: "all",
    });
    expect(params.leaf_id).toBeUndefined();
  });

  it("sets from to ~7 days ago for dateRange '7d'", () => {
    const params = filtersToParams({
      dateRange: "7d",
      validationStatus: "all",
    });
    expect(params.from).toBeDefined();
    const from = new Date(params.from!);
    const daysAgo = (Date.now() - from.getTime()) / (1000 * 60 * 60 * 24);
    expect(daysAgo).toBeGreaterThanOrEqual(6.9);
    expect(daysAgo).toBeLessThanOrEqual(7.1);
  });

  it("sets from to ~30 days ago for dateRange '30d'", () => {
    const params = filtersToParams({
      dateRange: "30d",
      validationStatus: "all",
    });
    expect(params.from).toBeDefined();
    const from = new Date(params.from!);
    const daysAgo = (Date.now() - from.getTime()) / (1000 * 60 * 60 * 24);
    expect(daysAgo).toBeGreaterThanOrEqual(29.9);
    expect(daysAgo).toBeLessThanOrEqual(30.1);
  });

  it("does not set from or to for dateRange 'all'", () => {
    const params = filtersToParams({
      dateRange: "all",
      validationStatus: "all",
    });
    expect(params.from).toBeUndefined();
    expect(params.to).toBeUndefined();
  });

  it("uses customFrom and customTo for dateRange 'custom'", () => {
    const params = filtersToParams({
      dateRange: "custom",
      validationStatus: "all",
      customFrom: "2026-01-01T00:00:00Z",
      customTo: "2026-01-31T23:59:59Z",
    });
    expect(params.from).toBe("2026-01-01T00:00:00Z");
    expect(params.to).toBe("2026-01-31T23:59:59Z");
  });

  it("handles custom range with only from set", () => {
    const params = filtersToParams({
      dateRange: "custom",
      validationStatus: "all",
      customFrom: "2026-01-01T00:00:00Z",
    });
    expect(params.from).toBe("2026-01-01T00:00:00Z");
    expect(params.to).toBeUndefined();
  });

  it("handles custom range with only to set", () => {
    const params = filtersToParams({
      dateRange: "custom",
      validationStatus: "all",
      customTo: "2026-01-31T23:59:59Z",
    });
    expect(params.from).toBeUndefined();
    expect(params.to).toBe("2026-01-31T23:59:59Z");
  });

  it("handles custom range with neither from nor to set", () => {
    const params = filtersToParams({
      dateRange: "custom",
      validationStatus: "all",
    });
    expect(params.from).toBeUndefined();
    expect(params.to).toBeUndefined();
  });
});
