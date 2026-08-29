import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor, act } from "@testing-library/react";
import { useAvailableLeafs } from "./use-leafs";

// Mock useClient (for useAvailableLeafs)
const mockClient = {
  availableLeafs: vi.fn(),
};

const mockUseClient = vi.fn();
vi.mock("./use-api", () => ({
  useApiQuery: (...args: unknown[]) => ({ data: null, isLoading: true, error: null, refetch: vi.fn() }),
  useClient: () => mockUseClient(),
}));

describe("useAvailableLeafs", () => {
  beforeEach(() => {
    mockClient.availableLeafs.mockReset();
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
  });

  it("fetches available leafs on mount", async () => {
    const available = [
      {
        server_name: "Public Server",
        leaf_id: "p1",
        leaf_name: "Climate Model",
        description: "Climate simulations",
        research_area: "Climate Science",
      },
    ];
    mockClient.availableLeafs.mockResolvedValue(available);

    const { result } = renderHook(() =>
      useAvailableLeafs({})
    );

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.leafs).toEqual(available);
    expect(result.current.error).toBeNull();
  });

  it("passes search params to client", async () => {
    mockClient.availableLeafs.mockResolvedValue([]);

    renderHook(() =>
      useAvailableLeafs({
        query: "climate",
        server_address: "grpc://lab:50051",
      })
    );

    await waitFor(() => {
      expect(mockClient.availableLeafs).toHaveBeenCalled();
    });
  });

  it("sets error state on fetch failure", async () => {
    mockClient.availableLeafs.mockRejectedValue(
      new Error("Server unreachable")
    );

    const { result } = renderHook(() =>
      useAvailableLeafs({})
    );

    await waitFor(() => {
      expect(result.current.error).toBeInstanceOf(Error);
      expect(result.current.error?.message).toBe("Server unreachable");
    });

    expect(result.current.isLoading).toBe(false);
  });

  it("reports error when client has error", async () => {
    mockUseClient.mockReturnValue({
      client: null,
      error: new Error("Daemon not running"),
    });

    const { result } = renderHook(() =>
      useAvailableLeafs({})
    );

    await waitFor(() => {
      expect(result.current.error?.message).toBe("Daemon not running");
      expect(result.current.isLoading).toBe(false);
    });
  });

  it("refetches when query param changes", async () => {
    mockClient.availableLeafs.mockResolvedValue([]);

    const { rerender } = renderHook(
      (props: { query?: string }) =>
        useAvailableLeafs(props),
      { initialProps: { query: "climate" } }
    );

    await waitFor(() => {
      expect(mockClient.availableLeafs).toHaveBeenCalledTimes(1);
    });

    rerender({ query: "biology" });

    await waitFor(() => {
      expect(mockClient.availableLeafs).toHaveBeenCalledTimes(2);
    });
  });
});
