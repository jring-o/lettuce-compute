import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import { useHeads, useDebouncedHeadWeight, useWriteLeafPreferences } from "./use-heads";
import type { ConfigResponse, HeadInfo, LeafPreferences } from "@/api/client";

// --- Mocks ---

const mockHeadsData: HeadInfo[] = [
  {
    name: "lettuce.science",
    description: "Open science",
    url: "https://lettuce.science",
    grpc_address: "lettuce.science:443",
    status: "connected",
    weight: 70,
    leafs: [
      {
        id: "leaf-1",
        slug: "prime",
        name: "Prime Study",
        description: "Primes",
        research_area: "math",
        task_pattern: "PARAMETER_SWEEP",
        state: "ACTIVE",
        queued_work_units: 100,
        active_volunteers: 5,
        enabled: true,
        effective_weight: 50,
      },
    ],
  },
];

const mockConfigFn = vi.fn<() => Promise<ConfigResponse>>();
const mockUpdateConfigFn = vi.fn<(partial: any) => Promise<ConfigResponse>>();
const mockHeadsFn = vi.fn<() => Promise<HeadInfo[]>>();

const mockClient = {
  heads: mockHeadsFn,
  config: mockConfigFn,
  updateConfig: mockUpdateConfigFn,
} as unknown as { heads: typeof mockHeadsFn; config: typeof mockConfigFn; updateConfig: typeof mockUpdateConfigFn };

const mockUseClient = vi.fn();

vi.mock("./use-api", () => ({
  useClient: () => mockUseClient(),
}));

function makeConfig(servers: ConfigResponse["servers"] = []): ConfigResponse {
  return {
    data_dir: "/home/test/.lettuce",
    resource_limits: {
      max_cpu_cores: 4,
      max_memory_mb: 2048,
      max_disk_gb: 10,
      max_bandwidth_mbps: 0,
      max_gpu_vram_pct: 50,
    },
    scheduling: {
      mode: "ALWAYS",
      idle_threshold_mins: 5,
      cron_expression: "",
    },
    leafs: {
      mode: "ALL",
      leaf_ids: [],
      blocked_ids: [],
    },
    thermal: {
      enabled: true,
      cpu_pause_threshold: 85,
      cpu_resume_threshold: 75,
      gpu_pause_threshold: 80,
      gpu_resume_threshold: 70,
      poll_interval_seconds: 10,
    },
    notifications: {
      credit_milestones: true,
      credit_milestone_threshold: 100,
      work_unit_completed: false,
      errors: true,
      updates: true,
    },
    servers,
    log_level: "info",
    max_concurrent_tasks: 1,
    available_runtimes: ["NATIVE"],
  };
}

// --- Tests ---

describe("useHeads", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("returns empty array and loading state initially when client is null", () => {
    mockUseClient.mockReturnValue({ client: null, error: null });

    const { result } = renderHook(() => useHeads());

    expect(result.current.heads).toEqual([]);
    expect(result.current.isLoading).toBe(true);
    expect(result.current.error).toBeNull();
  });

  it("returns heads data when client fetches successfully", async () => {
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockHeadsFn.mockResolvedValue(mockHeadsData);

    const { result } = renderHook(() => useHeads());

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.heads).toEqual(mockHeadsData);
    expect(result.current.error).toBeNull();
  });

  it("returns error when fetch fails", async () => {
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockHeadsFn.mockRejectedValue(new Error("Network error"));

    const { result } = renderHook(() => useHeads());

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.heads).toEqual([]);
    expect(result.current.error).toEqual(new Error("Network error"));
  });

  it("calls client.heads() on mount", async () => {
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockHeadsFn.mockResolvedValue([]);

    renderHook(() => useHeads());

    await waitFor(() => {
      expect(mockHeadsFn).toHaveBeenCalledOnce();
    });
  });

  it("exposes refetch that calls client.heads() again", async () => {
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockHeadsFn.mockResolvedValue(mockHeadsData);

    const { result } = renderHook(() => useHeads());

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    mockHeadsFn.mockResolvedValue([]);
    await act(async () => {
      result.current.refetch();
    });

    expect(mockHeadsFn).toHaveBeenCalledTimes(2);
  });

  it("exposes setHeads for local state updates", async () => {
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockHeadsFn.mockResolvedValue([]);

    const { result } = renderHook(() => useHeads());

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    act(() => {
      result.current.setHeads(mockHeadsData);
    });

    expect(result.current.heads).toEqual(mockHeadsData);
  });
});

describe("useDebouncedHeadWeight", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("calls config then updateConfig with updated weight after debounce", async () => {
    const servers = [
      {
        grpc_address: "lettuce.science:443",
        http_address: "https://lettuce.science",
        leaf_id: "",
        name: "lettuce.science",
        insecure: false,
        weight: 70,
        leaf_preferences: { mode: "ALL" as const },
      },
    ];
    const cfg = makeConfig(servers);
    mockConfigFn.mockResolvedValue(cfg);
    mockUpdateConfigFn.mockResolvedValue(cfg);

    const { result } = renderHook(() => useDebouncedHeadWeight());

    act(() => {
      result.current.write("lettuce.science", 50);
    });

    // config should not have been called yet
    expect(mockConfigFn).not.toHaveBeenCalled();

    // Advance past the 300ms debounce
    await act(async () => {
      vi.advanceTimersByTime(300);
    });

    expect(mockConfigFn).toHaveBeenCalledOnce();
    expect(mockUpdateConfigFn).toHaveBeenCalledWith({
      servers: [
        expect.objectContaining({
          name: "lettuce.science",
          weight: 50,
        }),
      ],
    });
  });

  it("debounces rapid updates - only fires once", async () => {
    const servers = [
      {
        grpc_address: "lettuce.science:443",
        http_address: "https://lettuce.science",
        leaf_id: "",
        name: "lettuce.science",
        insecure: false,
        weight: 70,
        leaf_preferences: { mode: "ALL" as const },
      },
    ];
    const cfg = makeConfig(servers);
    mockConfigFn.mockResolvedValue(cfg);
    mockUpdateConfigFn.mockResolvedValue(cfg);

    const { result } = renderHook(() => useDebouncedHeadWeight());

    // Rapid-fire updates
    act(() => {
      result.current.write("lettuce.science", 50);
    });
    act(() => {
      result.current.write("lettuce.science", 60);
    });
    act(() => {
      result.current.write("lettuce.science", 80);
    });

    // Only after debounce should it fire
    await act(async () => {
      vi.advanceTimersByTime(300);
    });

    // Only one config+update cycle
    expect(mockConfigFn).toHaveBeenCalledOnce();
    expect(mockUpdateConfigFn).toHaveBeenCalledOnce();
  });

  it("does nothing when client is null", async () => {
    mockUseClient.mockReturnValue({ client: null, error: null });

    const { result } = renderHook(() => useDebouncedHeadWeight());

    act(() => {
      result.current.write("lettuce.science", 50);
    });

    await act(async () => {
      vi.advanceTimersByTime(300);
    });

    expect(mockConfigFn).not.toHaveBeenCalled();
  });

  it("only updates the matching server by name", async () => {
    const servers = [
      {
        grpc_address: "lettuce.science:443",
        http_address: "https://lettuce.science",
        leaf_id: "",
        name: "lettuce.science",
        insecure: false,
        weight: 70,
        leaf_preferences: { mode: "ALL" as const },
      },
      {
        grpc_address: "einstein:443",
        http_address: "https://einstein.example",
        leaf_id: "",
        name: "einstein",
        insecure: false,
        weight: 30,
        leaf_preferences: { mode: "ALL" as const },
      },
    ];
    const cfg = makeConfig(servers);
    mockConfigFn.mockResolvedValue(cfg);
    mockUpdateConfigFn.mockResolvedValue(cfg);

    const { result } = renderHook(() => useDebouncedHeadWeight());

    act(() => {
      result.current.write("lettuce.science", 90);
    });

    await act(async () => {
      vi.advanceTimersByTime(300);
    });

    expect(mockUpdateConfigFn).toHaveBeenCalledWith({
      servers: [
        expect.objectContaining({ name: "lettuce.science", weight: 90 }),
        expect.objectContaining({ name: "einstein", weight: 30 }),
      ],
    });
  });
});

describe("useWriteLeafPreferences", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
  });

  it("calls config then updateConfig with updated leaf_preferences immediately", async () => {
    const servers = [
      {
        grpc_address: "lettuce.science:443",
        http_address: "https://lettuce.science",
        leaf_id: "",
        name: "lettuce.science",
        insecure: false,
        weight: 70,
        leaf_preferences: { mode: "ALL" as const },
      },
    ];
    const cfg = makeConfig(servers);
    mockConfigFn.mockResolvedValue(cfg);
    mockUpdateConfigFn.mockResolvedValue(cfg);

    const { result } = renderHook(() => useWriteLeafPreferences());

    const newPrefs: LeafPreferences = {
      mode: "SPECIFIC",
      enabled: ["prime", "mandel"],
    };

    await act(async () => {
      await result.current.write("lettuce.science", newPrefs);
    });

    expect(mockConfigFn).toHaveBeenCalledOnce();
    expect(mockUpdateConfigFn).toHaveBeenCalledWith({
      servers: [
        expect.objectContaining({
          name: "lettuce.science",
          leaf_preferences: newPrefs,
        }),
      ],
    });
  });

  it("preserves other server fields when updating leaf preferences", async () => {
    const servers = [
      {
        grpc_address: "lettuce.science:443",
        http_address: "https://lettuce.science",
        leaf_id: "proj-1",
        name: "lettuce.science",
        insecure: false,
        weight: 70,
        leaf_preferences: { mode: "ALL" as const },
      },
    ];
    const cfg = makeConfig(servers);
    mockConfigFn.mockResolvedValue(cfg);
    mockUpdateConfigFn.mockResolvedValue(cfg);

    const { result } = renderHook(() => useWriteLeafPreferences());

    await act(async () => {
      await result.current.write("lettuce.science", { mode: "SPECIFIC", enabled: ["prime"] });
    });

    const updatedServers = mockUpdateConfigFn.mock.calls[0][0].servers;
    expect(updatedServers[0].grpc_address).toBe("lettuce.science:443");
    expect(updatedServers[0].weight).toBe(70);
    expect(updatedServers[0].leaf_id).toBe("proj-1");
  });

  it("does nothing when client is null", async () => {
    mockUseClient.mockReturnValue({ client: null, error: null });

    const { result } = renderHook(() => useWriteLeafPreferences());

    await act(async () => {
      await result.current.write("lettuce.science", { mode: "ALL" });
    });

    expect(mockConfigFn).not.toHaveBeenCalled();
  });
});
