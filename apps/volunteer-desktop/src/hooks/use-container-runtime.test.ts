import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import { useContainerRuntime } from "./use-container-runtime";
import type { ContainerRuntimeStatus } from "@/api/client";

// Mock the api/client module
const mockGetContainerRuntimeStatus = vi.fn<() => Promise<ContainerRuntimeStatus>>();

vi.mock("@/api/client", () => ({
  getContainerRuntimeStatus: (...args: unknown[]) =>
    mockGetContainerRuntimeStatus(...(args as [])),
}));

function makeStatus(
  overrides: Partial<ContainerRuntimeStatus> = {}
): ContainerRuntimeStatus {
  return {
    backend: "podman",
    status: "running",
    version: "5.3.1",
    socket_path: "/run/podman/podman.sock",
    machine_required: false,
    machine_name: "",
    machine_cpus: 0,
    machine_memory_mb: 0,
    machine_disk_gb: 0,
    error: null,
    ...overrides,
  };
}

describe("useContainerRuntime", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("returns loading state initially", () => {
    mockGetContainerRuntimeStatus.mockReturnValue(new Promise(() => {})); // never resolves

    const { result } = renderHook(() => useContainerRuntime());

    expect(result.current.status).toBeNull();
    expect(result.current.loading).toBe(true);
    expect(result.current.error).toBeNull();
  });

  it("returns status after initial fetch", async () => {
    const status = makeStatus();
    mockGetContainerRuntimeStatus.mockResolvedValue(status);

    const { result } = renderHook(() => useContainerRuntime());

    await waitFor(() => {
      expect(result.current.loading).toBe(false);
    });

    expect(result.current.status).toEqual(status);
    expect(result.current.error).toBeNull();
  });

  it("returns error when fetch fails", async () => {
    mockGetContainerRuntimeStatus.mockRejectedValue(
      new Error("invoke error")
    );

    const { result } = renderHook(() => useContainerRuntime());

    await waitFor(() => {
      expect(result.current.loading).toBe(false);
    });

    expect(result.current.status).toBeNull();
    expect(result.current.error).toBe("invoke error");
  });

  it("stringifies non-Error rejections", async () => {
    mockGetContainerRuntimeStatus.mockRejectedValue("raw string error");

    const { result } = renderHook(() => useContainerRuntime());

    await waitFor(() => {
      expect(result.current.loading).toBe(false);
    });

    expect(result.current.error).toBe("raw string error");
  });

  it("refresh function fetches new status", async () => {
    const status1 = makeStatus({ version: "5.3.0" });
    const status2 = makeStatus({ version: "5.3.1" });
    mockGetContainerRuntimeStatus
      .mockResolvedValueOnce(status1)
      .mockResolvedValueOnce(status2);

    const { result } = renderHook(() => useContainerRuntime());

    await waitFor(() => {
      expect(result.current.status?.version).toBe("5.3.0");
    });

    await act(async () => {
      await result.current.refresh();
    });

    expect(result.current.status?.version).toBe("5.3.1");
  });

  it("clears error on successful refresh after failure", async () => {
    mockGetContainerRuntimeStatus
      .mockRejectedValueOnce(new Error("fail"))
      .mockResolvedValueOnce(makeStatus());

    const { result } = renderHook(() => useContainerRuntime());

    await waitFor(() => {
      expect(result.current.error).toBe("fail");
    });

    await act(async () => {
      await result.current.refresh();
    });

    expect(result.current.error).toBeNull();
    expect(result.current.status).not.toBeNull();
  });

  it("polls at the specified interval", async () => {
    vi.useFakeTimers();
    const status = makeStatus();
    mockGetContainerRuntimeStatus.mockResolvedValue(status);

    renderHook(() => useContainerRuntime(5000));

    // Flush the initial async call (useEffect calls refresh() immediately)
    await vi.runOnlyPendingTimersAsync();
    const callsAfterInit = mockGetContainerRuntimeStatus.mock.calls.length;

    // Advance past first interval
    await vi.advanceTimersByTimeAsync(5000);
    expect(mockGetContainerRuntimeStatus).toHaveBeenCalledTimes(callsAfterInit + 1);

    // Advance past second interval
    await vi.advanceTimersByTimeAsync(5000);
    expect(mockGetContainerRuntimeStatus).toHaveBeenCalledTimes(callsAfterInit + 2);

    vi.useRealTimers();
  });

  it("uses default 10000ms poll interval", async () => {
    vi.useFakeTimers();
    const status = makeStatus();
    mockGetContainerRuntimeStatus.mockResolvedValue(status);

    renderHook(() => useContainerRuntime());

    // Flush the initial async call
    await vi.runOnlyPendingTimersAsync();
    const callsAfterInit = mockGetContainerRuntimeStatus.mock.calls.length;

    // At 5s — should NOT have polled again
    await vi.advanceTimersByTimeAsync(5000);
    expect(mockGetContainerRuntimeStatus).toHaveBeenCalledTimes(callsAfterInit);

    // At 10s — should have polled again
    await vi.advanceTimersByTimeAsync(5000);
    expect(mockGetContainerRuntimeStatus).toHaveBeenCalledTimes(callsAfterInit + 1);

    vi.useRealTimers();
  });

  it("clears interval on unmount", async () => {
    vi.useFakeTimers();
    mockGetContainerRuntimeStatus.mockResolvedValue(makeStatus());

    const { unmount } = renderHook(() => useContainerRuntime(5000));

    // Flush the initial async call
    await vi.runOnlyPendingTimersAsync();
    const callsAfterInit = mockGetContainerRuntimeStatus.mock.calls.length;

    unmount();

    await vi.advanceTimersByTimeAsync(10000);

    // Should not have polled after unmount
    expect(mockGetContainerRuntimeStatus).toHaveBeenCalledTimes(callsAfterInit);

    vi.useRealTimers();
  });
});
