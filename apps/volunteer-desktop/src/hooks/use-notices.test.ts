import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { useNotices, mergeNotices, resetNoticesCacheForTest } from "./use-notices";
import { ApiError, type Notice, type ManagementClient } from "@/api/client";

const mockNotices = vi.fn();
const mockClient = { notices: mockNotices } as unknown as ManagementClient;

vi.mock("./use-api", () => ({
  useClient: () => ({ client: mockClient, error: null }),
}));

function notice(overrides: Partial<Notice> & { id: number }): Notice {
  return {
    level: "warn",
    code: "TEST",
    message: `notice ${overrides.id}`,
    count: 1,
    first_at: "2026-01-01T00:00:00Z",
    at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

describe("mergeNotices", () => {
  it("drops info notices, replaces by id and orders newest first", () => {
    const merged = mergeNotices(
      [notice({ id: 2 }), notice({ id: 1 })],
      [notice({ id: 3, level: "error" }), notice({ id: 2, count: 4 }), notice({ id: 4, level: "info" })]
    );
    expect(merged.map((n) => n.id)).toEqual([3, 2, 1]);
    expect(merged[1].count).toBe(4);
  });
});

describe("useNotices", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    resetNoticesCacheForTest();
    mockNotices.mockReset();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("polls from the last seen id and exposes warn/error notices newest first", async () => {
    mockNotices
      .mockResolvedValueOnce({
        notices: [notice({ id: 1 }), notice({ id: 2, level: "info" })],
        latest_id: 2,
      })
      .mockResolvedValueOnce({
        notices: [notice({ id: 3, level: "error" })],
        latest_id: 3,
      });

    const { result } = renderHook(() => useNotices(10000));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(mockNotices).toHaveBeenNthCalledWith(1, undefined);
    expect(result.current.notices.map((n) => n.id)).toEqual([1]);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });
    expect(mockNotices).toHaveBeenNthCalledWith(2, 2);
    expect(result.current.notices.map((n) => n.id)).toEqual([3, 1]);
    expect(result.current.supported).toBe(true);
  });

  it("treats a 404 as an unsupported CLI build and stops polling", async () => {
    mockNotices.mockRejectedValue(new ApiError("NOT_FOUND", "no such route", 404));

    const { result } = renderHook(() => useNotices(10000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.supported).toBe(false);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(30000);
    });
    // A remount does not start polling again either.
    const again = renderHook(() => useNotices(10000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });
    expect(again.result.current.supported).toBe(false);
    expect(mockNotices).toHaveBeenCalledTimes(1);
  });

  it("keeps polling through other errors", async () => {
    mockNotices
      .mockRejectedValueOnce(new ApiError("DAEMON_UNREACHABLE", "restarting", 0))
      .mockResolvedValueOnce({ notices: [notice({ id: 7 })], latest_id: 7 });

    const { result } = renderHook(() => useNotices(10000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.supported).toBe(true);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });
    expect(result.current.notices.map((n) => n.id)).toEqual([7]);
  });

  it("hides a dismissed notice and keeps it hidden across remounts", async () => {
    mockNotices.mockResolvedValue({ notices: [notice({ id: 1 }), notice({ id: 2 })], latest_id: 2 });

    const first = renderHook(() => useNotices(10000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(first.result.current.notices).toHaveLength(2);

    act(() => {
      first.result.current.dismiss(notice({ id: 2 }));
    });
    expect(first.result.current.notices.map((n) => n.id)).toEqual([1]);
    first.unmount();

    const second = renderHook(() => useNotices(10000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(second.result.current.notices.map((n) => n.id)).toEqual([1]);
  });

  it("starts over when the daemon's ids reset after a restart", async () => {
    mockNotices
      .mockResolvedValueOnce({ notices: [notice({ id: 40 })], latest_id: 40 })
      // since=40 on the restarted daemon: nothing newer, latest_id fell back to 1
      .mockResolvedValueOnce({ notices: [], latest_id: 1 })
      // the full refetch
      .mockResolvedValueOnce({ notices: [notice({ id: 1, message: "fresh" })], latest_id: 1 });

    const { result } = renderHook(() => useNotices(10000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.notices.map((n) => n.id)).toEqual([40]);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });
    expect(mockNotices).toHaveBeenNthCalledWith(3, undefined);
    expect(result.current.notices.map((n) => n.message)).toEqual(["fresh"]);
  });
});
