import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { useMetrics, useSystemMetrics } from "./use-metrics";
import { invoke } from "@tauri-apps/api/core";

// Mock the useApiQuery hook
const mockUseApiQuery = vi.fn();
vi.mock("./use-api", () => ({
  useApiQuery: (...args: unknown[]) => mockUseApiQuery(...args),
}));

describe("useMetrics", () => {
  beforeEach(() => {
    mockUseApiQuery.mockReset();
  });

  it("returns loading state initially", () => {
    mockUseApiQuery.mockReturnValue({
      data: null,
      isLoading: true,
      error: null,
    });

    const { result } = renderHook(() => useMetrics());

    expect(result.current.metrics).toBeNull();
    expect(result.current.isLoading).toBe(true);
    expect(result.current.error).toBeNull();
  });

  it("returns metrics data when loaded", () => {
    const metricsData = {
      cpu_usage_pct: 0,
      gpu_usage_pct: 0,
      memory_used_mb: 0,
      memory_total_mb: 0,
      cpu_temp_c: 0,
      gpu_temp_c: 0,
      disk_used_mb: 1024,
      disk_allowance_mb: 10240,
      disk_usage_known: true,
    };

    mockUseApiQuery.mockReturnValue({
      data: metricsData,
      isLoading: false,
      error: null,
    });

    const { result } = renderHook(() => useMetrics());

    expect(result.current.metrics).toEqual(metricsData);
    expect(result.current.isLoading).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("returns error when fetch fails", () => {
    const error = new Error("Connection failed");
    mockUseApiQuery.mockReturnValue({
      data: null,
      isLoading: false,
      error,
    });

    const { result } = renderHook(() => useMetrics());

    expect(result.current.metrics).toBeNull();
    expect(result.current.isLoading).toBe(false);
    expect(result.current.error).toBe(error);
  });

  it("passes default interval of 3000ms to useApiQuery", () => {
    mockUseApiQuery.mockReturnValue({
      data: null,
      isLoading: true,
      error: null,
    });

    renderHook(() => useMetrics());

    expect(mockUseApiQuery).toHaveBeenCalledWith(expect.any(Function), 3000);
  });

  it("passes custom interval to useApiQuery", () => {
    mockUseApiQuery.mockReturnValue({
      data: null,
      isLoading: true,
      error: null,
    });

    renderHook(() => useMetrics(5000));

    expect(mockUseApiQuery).toHaveBeenCalledWith(expect.any(Function), 5000);
  });

  it("the fetcher calls client.metrics()", async () => {
    const mockMetrics = vi.fn().mockResolvedValue({ cpu_usage_pct: 50 });
    const mockClient = { metrics: mockMetrics };

    mockUseApiQuery.mockImplementation((fetcher: Function) => {
      // Call the fetcher to verify it calls the right client method
      fetcher(mockClient);
      return { data: null, isLoading: true, error: null };
    });

    renderHook(() => useMetrics());

    expect(mockMetrics).toHaveBeenCalled();
  });
});

describe("useSystemMetrics", () => {
  const mockInvoke = invoke as unknown as ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.useFakeTimers();
    mockInvoke.mockReset();
  });

  afterEach(() => {
    vi.useRealTimers();
    mockInvoke.mockImplementation(() => Promise.resolve(undefined));
  });

  it("samples the Rust system_metrics command on mount and on every interval", async () => {
    mockInvoke.mockResolvedValue({ cpu_usage_pct: 12.5, memory_used_mb: 4096, memory_total_mb: 16384 });

    const { result } = renderHook(() => useSystemMetrics(3000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(mockInvoke).toHaveBeenCalledWith("system_metrics");
    expect(result.current.system).toEqual({ cpu_usage_pct: 12.5, memory_used_mb: 4096, memory_total_mb: 16384 });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(6000);
    });
    expect(mockInvoke).toHaveBeenCalledTimes(3);
  });

  it("reports an error and keeps the last good sample when the command fails", async () => {
    mockInvoke
      .mockResolvedValueOnce({ cpu_usage_pct: 1, memory_used_mb: 1, memory_total_mb: 2 })
      .mockRejectedValueOnce(new Error("no sysinfo"));

    const { result } = renderHook(() => useSystemMetrics(1000));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.error).toBeNull();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    expect(result.current.error?.message).toBe("no sysinfo");
    expect(result.current.system?.cpu_usage_pct).toBe(1);
  });
});
