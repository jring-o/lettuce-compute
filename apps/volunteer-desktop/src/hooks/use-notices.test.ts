import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { useNotices, liveNotices, noticeKey, resetNoticesCacheForTest } from "./use-notices";
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

const STORAGE_KEY = "lettuce.notices.dismissed";

function storedDismissals(): string[] {
  return JSON.parse(localStorage.getItem(STORAGE_KEY) ?? "[]");
}

describe("liveNotices", () => {
  it("drops info and resolved notices and orders newest first", () => {
    const live = liveNotices([
      notice({ id: 2, count: 4 }),
      notice({ id: 4, level: "info" }),
      notice({ id: 3, level: "error" }),
      notice({ id: 1, resolved_at: "2026-01-01T03:00:00Z" }),
    ]);
    expect(live.map((n) => n.id)).toEqual([3, 2]);
    expect(live[1].count).toBe(4);
  });
});

describe("noticeKey", () => {
  it("names the episode by condition and start, not by id or latest time", () => {
    const first = notice({ id: 1, code: "no_work", head: "h", leaf: "l", first_at: "A", at: "A" });
    const refreshed = notice({ id: 1, code: "no_work", head: "h", leaf: "l", first_at: "A", at: "B", count: 3 });
    const newEpisode = notice({ id: 9, code: "no_work", head: "h", leaf: "l", first_at: "C", at: "C" });
    expect(noticeKey(refreshed)).toBe(noticeKey(first));
    expect(noticeKey(newEpisode)).not.toBe(noticeKey(first));
  });
});

describe("useNotices", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    localStorage.clear();
    resetNoticesCacheForTest();
    mockNotices.mockReset();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("polls the whole ring every tick and shows the refreshed count", async () => {
    mockNotices
      .mockResolvedValueOnce({
        notices: [notice({ id: 1 }), notice({ id: 2, level: "info" })],
        latest_id: 2,
      })
      .mockResolvedValueOnce({
        notices: [notice({ id: 3, level: "error" }), notice({ id: 1, count: 6, at: "2026-01-01T01:00:00Z" })],
        latest_id: 3,
      });

    const { result } = renderHook(() => useNotices(10000));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.notices.map((n) => n.id)).toEqual([1]);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });
    expect(result.current.notices.map((n) => n.id)).toEqual([3, 1]);
    expect(result.current.notices[1].count).toBe(6);
    expect(result.current.supported).toBe(true);

    // Never a `since` cursor: a refreshed or resolved notice keeps its id, so
    // only the whole ring tells the truth about it.
    expect(mockNotices).toHaveBeenCalledTimes(2);
    for (const args of mockNotices.mock.calls) {
      expect(args[0]).toBeUndefined();
    }
  });

  // The tester's report: "Connected but getting no work" under Needs attention
  // for 16 hours while units arrived. The daemon now marks it resolved; the
  // panel must drop it on the next poll.
  it("drops a notice once the daemon reports it resolved", async () => {
    mockNotices
      .mockResolvedValueOnce({ notices: [notice({ id: 1, code: "no_work" })], latest_id: 1 })
      .mockResolvedValueOnce({
        notices: [notice({ id: 1, code: "no_work", resolved_at: "2026-01-01T04:00:00Z" })],
        latest_id: 1,
      });

    const { result } = renderHook(() => useNotices(10000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.notices.map((n) => n.id)).toEqual([1]);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });
    expect(result.current.notices).toEqual([]);
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
    mockNotices.mockResolvedValue({
      notices: [notice({ id: 1, code: "no_work" }), notice({ id: 2, code: "disk_gate_blocked" })],
      latest_id: 2,
    });

    const first = renderHook(() => useNotices(10000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(first.result.current.notices).toHaveLength(2);

    act(() => {
      first.result.current.dismiss(notice({ id: 2, code: "disk_gate_blocked" }));
    });
    expect(first.result.current.notices.map((n) => n.id)).toEqual([1]);
    first.unmount();

    const second = renderHook(() => useNotices(10000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(second.result.current.notices.map((n) => n.id)).toEqual([1]);
  });

  // The tester's reload (right-click → Reload) brought back a 12-hour-old
  // warning he had dismissed: the dismissed set lived in web-view memory only.
  it("keeps a dismissal across a web-view reload", async () => {
    mockNotices.mockResolvedValue({
      notices: [notice({ id: 1, code: "no_work" }), notice({ id: 2, code: "leaf_failing", leaf: "l" })],
      latest_id: 2,
    });

    const before = renderHook(() => useNotices(10000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    act(() => {
      before.result.current.dismiss(notice({ id: 1, code: "no_work" }));
    });
    expect(before.result.current.notices.map((n) => n.id)).toEqual([2]);
    before.unmount();

    // A reload: module memory is gone, localStorage is not.
    resetNoticesCacheForTest();

    const after = renderHook(() => useNotices(10000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(after.result.current.notices.map((n) => n.id)).toEqual([2]);
  });

  // The daemon refreshes a repeating condition in place: same id and
  // first_at, newer at and a higher count. That is the same episode, and it
  // stays dismissed. A condition that returns after ending is a new episode
  // (new first_at) and must show again.
  it("keeps a refreshed episode dismissed and shows a new episode", async () => {
    mockNotices
      .mockResolvedValueOnce({
        notices: [notice({ id: 1, code: "no_work", first_at: "2026-01-01T00:00:00Z", at: "2026-01-01T00:00:00Z" })],
        latest_id: 1,
      })
      .mockResolvedValueOnce({
        notices: [notice({ id: 1, code: "no_work", first_at: "2026-01-01T00:00:00Z", at: "2026-01-01T00:05:00Z", count: 2 })],
        latest_id: 1,
      })
      .mockResolvedValueOnce({
        notices: [
          notice({ id: 1, code: "no_work", first_at: "2026-01-01T00:00:00Z", at: "2026-01-01T00:05:00Z", count: 2, resolved_at: "2026-01-01T00:06:00Z" }),
          notice({ id: 5, code: "no_work", first_at: "2026-01-01T03:00:00Z", at: "2026-01-01T03:00:00Z" }),
        ],
        latest_id: 5,
      });

    const { result } = renderHook(() => useNotices(10000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    act(() => {
      result.current.dismiss(result.current.notices[0]);
    });
    expect(result.current.notices).toEqual([]);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });
    expect(result.current.notices).toEqual([]);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });
    expect(result.current.notices.map((n) => n.id)).toEqual([5]);
  });

  it("forgets dismissals of episodes the daemon no longer retains", async () => {
    mockNotices
      .mockResolvedValueOnce({ notices: [notice({ id: 1, code: "no_work" })], latest_id: 1 })
      // The daemon restarted: its ring is empty.
      .mockResolvedValueOnce({ notices: [], latest_id: 0 });

    const { result } = renderHook(() => useNotices(10000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    act(() => {
      result.current.dismiss(result.current.notices[0]);
    });
    expect(storedDismissals()).toHaveLength(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });
    expect(result.current.notices).toEqual([]);
    expect(storedDismissals()).toEqual([]);
  });
});
