import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { useMetrics } from "./use-metrics";

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
      cpu_usage_pct: 45.2,
      gpu_usage_pct: 80,
      memory_used_mb: 8192,
      memory_total_mb: 16384,
      disk_used_gb: 100,
      disk_total_gb: 500,
      cpu_temp_c: 65,
      gpu_temp_c: 72,
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
