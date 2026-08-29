import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook } from "@testing-library/react";
import { useCredit } from "./use-credit";

// Mock the useApiQuery hook
const mockUseApiQuery = vi.fn();
vi.mock("./use-api", () => ({
  useApiQuery: (...args: unknown[]) => mockUseApiQuery(...args),
}));

describe("useCredit", () => {
  beforeEach(() => {
    mockUseApiQuery.mockReset();
  });

  it("returns loading state initially", () => {
    mockUseApiQuery.mockReturnValue({
      data: null,
      isLoading: true,
      error: null,
    });

    const { result } = renderHook(() => useCredit());

    expect(result.current.credit).toBeNull();
    expect(result.current.isLoading).toBe(true);
    expect(result.current.error).toBeNull();
  });

  it("returns credit data when loaded", () => {
    const creditData = {
      total_credit: 5000,
      today: 50,
      this_week: 200,
      this_month: 800,
      by_leaf: [
        { leaf_id: "p1", leaf_name: "Climate", credit: 3000 },
        { leaf_id: "p2", leaf_name: "Biology", credit: 2000 },
      ],
      by_head: [
        { head_name: "lettuce.science", volunteer_id: "vol-1", total_credit: 5000, available: true },
      ],
      source: "head",
    };

    mockUseApiQuery.mockReturnValue({
      data: creditData,
      isLoading: false,
      error: null,
    });

    const { result } = renderHook(() => useCredit());

    expect(result.current.credit).toEqual(creditData);
    expect(result.current.isLoading).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("returns error when fetch fails", () => {
    const error = new Error("Network error");
    mockUseApiQuery.mockReturnValue({
      data: null,
      isLoading: false,
      error,
    });

    const { result } = renderHook(() => useCredit());

    expect(result.current.credit).toBeNull();
    expect(result.current.error).toBe(error);
  });

  it("passes default interval of 10000ms to useApiQuery", () => {
    mockUseApiQuery.mockReturnValue({
      data: null,
      isLoading: true,
      error: null,
    });

    renderHook(() => useCredit());

    expect(mockUseApiQuery).toHaveBeenCalledWith(expect.any(Function), 10000);
  });

  it("passes custom interval to useApiQuery", () => {
    mockUseApiQuery.mockReturnValue({
      data: null,
      isLoading: true,
      error: null,
    });

    renderHook(() => useCredit(30000));

    expect(mockUseApiQuery).toHaveBeenCalledWith(expect.any(Function), 30000);
  });

  it("the fetcher calls client.credit()", () => {
    const mockCreditFn = vi.fn().mockResolvedValue({ total_credit: 0 });
    const mockClient = { credit: mockCreditFn };

    mockUseApiQuery.mockImplementation((fetcher: Function) => {
      fetcher(mockClient);
      return { data: null, isLoading: true, error: null };
    });

    renderHook(() => useCredit());

    expect(mockCreditFn).toHaveBeenCalled();
  });
});
