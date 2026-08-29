import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import {
  useHeads,
  useDebouncedHeadWeight,
  useWriteLeafPreferences,
  useWriteHeadTrust,
  useRaiseDiskAllowance,
  trustByHeadFromConfig,
} from "./use-heads";
import {
  resetRestartRequiredForTest,
  restartLettuce,
  useRestartRequired,
} from "./use-restart-required";
import type {
  ConfigResponse,
  ConfigUpdate,
  ConfigUpdateResponse,
  HeadInfo,
  HeadsResponse,
  LeafPreferences,
  MachineCapabilities,
  ServerConfig,
} from "@/api/client";

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
        research_area: ["math"],
        task_pattern: "PARAMETER_SWEEP",
        state: "ACTIVE",
        queued_work_units: 100,
        active_volunteers: 5,
        active_hosts: 6,
        enabled: true,
        effective_weight: 50,
      },
    ],
  },
];

const mockMachine: MachineCapabilities = {
  runtimes: ["container", "wasm"],
  has_gpu: false,
  max_memory_mb: 2048,
  max_disk_mb: 10240,
  max_cpu_cores: 4,
  max_gpu_vram_mb: 0,
  gpu_card_vram_mb: 0,
  gpu_vram_pct: 50,
  gpu_vendors: [],
  gpu_compute_capabilities: [],
};

const mockConfigFn = vi.fn<() => Promise<ConfigResponse>>();
const mockUpdateConfigFn = vi.fn<(partial: ConfigUpdate) => Promise<ConfigUpdateResponse>>();
const mockHeadsAndMachineFn = vi.fn<() => Promise<HeadsResponse>>();

const mockClient = {
  headsAndMachine: mockHeadsAndMachineFn,
  config: mockConfigFn,
  updateConfig: mockUpdateConfigFn,
};

const mockUseClient = vi.fn();

vi.mock("./use-api", () => ({
  useClient: () => mockUseClient(),
}));

function makeServer(overrides: Partial<ServerConfig> = {}): ServerConfig {
  return {
    grpc_address: "lettuce.science:443",
    http_address: "https://lettuce.science",
    name: "lettuce.science",
    insecure: false,
    weight: 70,
    leaf_preferences: { mode: "ALL" },
    trusted_runtimes: ["CONTAINER"],
    ...overrides,
  };
}

function makeConfig(servers: ConfigResponse["servers"] = []): ConfigResponse {
  return {
    data_dir: "/home/test/.lettuce",
    resource_limits: {
      max_cpu_cores: 4,
      max_memory_mb: 2048,
      max_disk_gb: 10,
      max_bandwidth_mbps: 0,
      max_gpu_vram_pct: 50,
      max_pids: 0,
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
      max_throttle_minutes: 0,
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
  };
}

// --- Tests ---

describe("trustByHeadFromConfig", () => {
  it("matches a head to its config entry by gRPC address, then by name", () => {
    const heads: HeadInfo[] = [
      { ...mockHeadsData[0], name: "Renamed", grpc_address: "lettuce.science:443" },
      { ...mockHeadsData[0], name: "einstein", grpc_address: "" },
    ];
    const servers = [
      makeServer({ trusted_runtimes: ["container"] }),
      makeServer({ name: "einstein", grpc_address: "einstein:443", trusted_runtimes: ["NATIVE"] }),
    ];
    expect(trustByHeadFromConfig(heads, servers)).toEqual({
      Renamed: ["CONTAINER"],
      einstein: ["NATIVE"],
    });
  });

  it("leaves a head out when no config entry matches", () => {
    expect(trustByHeadFromConfig(mockHeadsData, [])).toEqual({});
  });
});

describe("useHeads", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockConfigFn.mockResolvedValue(makeConfig([makeServer()]));
  });

  it("returns empty array and loading state initially when client is null", () => {
    mockUseClient.mockReturnValue({ client: null, error: null });

    const { result } = renderHook(() => useHeads());

    expect(result.current.heads).toEqual([]);
    expect(result.current.machine).toBeNull();
    expect(result.current.trustByHead).toEqual({});
    expect(result.current.isLoading).toBe(true);
    expect(result.current.error).toBeNull();
  });

  it("returns heads, machine and per-head trust when the client fetches successfully", async () => {
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockHeadsAndMachineFn.mockResolvedValue({ heads: mockHeadsData, machine: mockMachine });

    const { result } = renderHook(() => useHeads());

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.heads).toEqual(mockHeadsData);
    expect(result.current.machine).toEqual(mockMachine);
    await waitFor(() => {
      expect(result.current.trustByHead).toEqual({ "lettuce.science": ["CONTAINER"] });
    });
    expect(result.current.error).toBeNull();
  });

  it("keeps the heads and leaves trust unknown when config cannot be read", async () => {
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockHeadsAndMachineFn.mockResolvedValue({ heads: mockHeadsData, machine: mockMachine });
    mockConfigFn.mockRejectedValue(new Error("config unreadable"));

    const { result } = renderHook(() => useHeads());

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.heads).toEqual(mockHeadsData);
    expect(result.current.trustByHead).toEqual({});
    expect(result.current.error).toBeNull();
  });

  it("returns error when fetch fails", async () => {
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockHeadsAndMachineFn.mockRejectedValue(new Error("Network error"));

    const { result } = renderHook(() => useHeads());

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.heads).toEqual([]);
    expect(result.current.error).toEqual(new Error("Network error"));
  });

  it("calls client.headsAndMachine() on mount", async () => {
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockHeadsAndMachineFn.mockResolvedValue({ heads: [], machine: mockMachine });

    renderHook(() => useHeads());

    await waitFor(() => {
      expect(mockHeadsAndMachineFn).toHaveBeenCalledOnce();
    });
  });

  it("exposes refetch that calls client.headsAndMachine() again", async () => {
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockHeadsAndMachineFn.mockResolvedValue({ heads: mockHeadsData, machine: mockMachine });

    const { result } = renderHook(() => useHeads());

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    mockHeadsAndMachineFn.mockResolvedValue({ heads: [], machine: mockMachine });
    await act(async () => {
      result.current.refetch();
    });

    expect(mockHeadsAndMachineFn).toHaveBeenCalledTimes(2);
  });

  it("re-reads the heads after an in-app restart", async () => {
    resetRestartRequiredForTest();
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockHeadsAndMachineFn.mockResolvedValue({ heads: mockHeadsData, machine: mockMachine });

    const { result } = renderHook(() => useHeads());
    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });
    expect(mockHeadsAndMachineFn).toHaveBeenCalledOnce();

    await act(async () => {
      await restartLettuce();
    });
    await waitFor(() => {
      expect(mockHeadsAndMachineFn).toHaveBeenCalledTimes(2);
    });
  });

  it("exposes setHeads and setTrustByHead for local state updates", async () => {
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockHeadsAndMachineFn.mockResolvedValue({ heads: [], machine: mockMachine });

    const { result } = renderHook(() => useHeads());

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    act(() => {
      result.current.setHeads(mockHeadsData);
      result.current.setTrustByHead({ "lettuce.science": ["NATIVE"] });
    });

    expect(result.current.heads).toEqual(mockHeadsData);
    expect(result.current.trustByHead).toEqual({ "lettuce.science": ["NATIVE"] });
  });
});

describe("useWriteHeadTrust", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetRestartRequiredForTest();
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
  });

  it("records a pending restart when the daemon asks for one", async () => {
    mockUpdateConfigFn.mockResolvedValue({ status: "ok", restart_required: true });

    const { result } = renderHook(() => ({
      trust: useWriteHeadTrust(),
      restart: useRestartRequired(),
    }));
    await act(async () => {
      await result.current.trust.write("lettuce.science", ["NATIVE"]);
    });

    expect(result.current.restart.reasons).toEqual([
      "Trust settings for lettuce.science are saved. Lettuce applies them the next time it starts.",
    ]);
  });

  it("still records a restart when an older daemon reports no restart_required at all", async () => {
    // An older daemon echoes the whole config with no restart hint.
    mockUpdateConfigFn.mockResolvedValue({ data_dir: "/x" } as unknown as ConfigUpdateResponse);

    const { result } = renderHook(() => ({
      trust: useWriteHeadTrust(),
      restart: useRestartRequired(),
    }));
    await act(async () => {
      await result.current.trust.write("lettuce.science", []);
    });

    expect(result.current.restart.restartRequired).toBe(true);
  });

  it("records nothing when the daemon says trust applied live", async () => {
    mockUpdateConfigFn.mockResolvedValue({ status: "ok", restart_required: false });

    const { result } = renderHook(() => ({
      trust: useWriteHeadTrust(),
      restart: useRestartRequired(),
    }));
    await act(async () => {
      await result.current.trust.write("lettuce.science", ["CONTAINER"]);
    });

    expect(result.current.restart.restartRequired).toBe(false);
  });

  it("records nothing when the write fails", async () => {
    mockUpdateConfigFn.mockRejectedValue(new Error("unknown runtime"));

    const { result } = renderHook(() => ({
      trust: useWriteHeadTrust(),
      restart: useRestartRequired(),
    }));
    await act(async () => {
      await expect(result.current.trust.write("lettuce.science", ["NATIVE"])).rejects.toThrow(
        "unknown runtime"
      );
    });

    expect(result.current.restart.restartRequired).toBe(false);
  });

  it("PUTs exactly the head name and the new trusted runtimes, and returns the daemon's answer", async () => {
    mockUpdateConfigFn.mockResolvedValue({ status: "ok", restart_required: true });

    const { result } = renderHook(() => useWriteHeadTrust());

    let resp: ConfigUpdateResponse | null = null;
    await act(async () => {
      resp = await result.current.write("lettuce.science", ["CONTAINER", "NATIVE"]);
    });

    expect(mockConfigFn).not.toHaveBeenCalled();
    expect(mockUpdateConfigFn).toHaveBeenCalledOnce();
    expect(mockUpdateConfigFn).toHaveBeenCalledWith({
      servers: [{ name: "lettuce.science", trusted_runtimes: ["CONTAINER", "NATIVE"] }],
    });
    expect(resp).toEqual({ status: "ok", restart_required: true });
  });

  it("sends an empty list to revoke all opt-in trust", async () => {
    mockUpdateConfigFn.mockResolvedValue({ status: "ok" });

    const { result } = renderHook(() => useWriteHeadTrust());
    await act(async () => {
      await result.current.write("lettuce.science", []);
    });

    expect(mockUpdateConfigFn).toHaveBeenCalledWith({
      servers: [{ name: "lettuce.science", trusted_runtimes: [] }],
    });
  });

  it("resolves to null without writing when there is no client", async () => {
    mockUseClient.mockReturnValue({ client: null, error: null });

    const { result } = renderHook(() => useWriteHeadTrust());
    let resp: ConfigUpdateResponse | null = { status: "x" };
    await act(async () => {
      resp = await result.current.write("lettuce.science", ["NATIVE"]);
    });

    expect(resp).toBeNull();
    expect(mockUpdateConfigFn).not.toHaveBeenCalled();
  });
});

describe("useRaiseDiskAllowance", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetRestartRequiredForTest();
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
  });

  it("records a pending restart only when the daemon asks for one", async () => {
    mockConfigFn.mockResolvedValue(makeConfig());
    mockUpdateConfigFn.mockResolvedValue({ status: "ok", restart_required: false });

    const { result } = renderHook(() => ({
      disk: useRaiseDiskAllowance(),
      restart: useRestartRequired(),
    }));
    await act(async () => {
      await result.current.disk.raise(21);
    });
    expect(result.current.restart.restartRequired).toBe(false);

    mockUpdateConfigFn.mockResolvedValue({ status: "ok", restart_required: true });
    await act(async () => {
      await result.current.disk.raise(25);
    });
    expect(result.current.restart.reasons).toEqual([
      "Your disk allowance is now 25 GB. Lettuce applies it the next time it starts.",
    ]);
  });

  it("reads the current limits and PUTs the whole object with the new max_disk_gb", async () => {
    mockConfigFn.mockResolvedValue(makeConfig());
    mockUpdateConfigFn.mockResolvedValue({ status: "ok", restart_required: false });

    const { result } = renderHook(() => useRaiseDiskAllowance());

    let resp: ConfigUpdateResponse | null = null;
    await act(async () => {
      resp = await result.current.raise(25);
    });

    expect(mockConfigFn).toHaveBeenCalledOnce();
    expect(mockUpdateConfigFn).toHaveBeenCalledWith({
      resource_limits: {
        max_cpu_cores: 4,
        max_memory_mb: 2048,
        max_disk_gb: 25,
        max_bandwidth_mbps: 0,
        max_gpu_vram_pct: 50,
        max_pids: 0,
      },
    });
    expect(resp).toEqual({ status: "ok", restart_required: false });
  });

  it("resolves to null without writing when there is no client", async () => {
    mockUseClient.mockReturnValue({ client: null, error: null });

    const { result } = renderHook(() => useRaiseDiskAllowance());
    let resp: ConfigUpdateResponse | null = { status: "x" };
    await act(async () => {
      resp = await result.current.raise(25);
    });

    expect(resp).toBeNull();
    expect(mockConfigFn).not.toHaveBeenCalled();
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
    const cfg = makeConfig([makeServer()]);
    mockConfigFn.mockResolvedValue(cfg);
    mockUpdateConfigFn.mockResolvedValue({});

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
    const cfg = makeConfig([makeServer()]);
    mockConfigFn.mockResolvedValue(cfg);
    mockUpdateConfigFn.mockResolvedValue({});

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
    const cfg = makeConfig([
      makeServer(),
      makeServer({
        grpc_address: "einstein:443",
        http_address: "https://einstein.example",
        name: "einstein",
        weight: 30,
      }),
    ]);
    mockConfigFn.mockResolvedValue(cfg);
    mockUpdateConfigFn.mockResolvedValue({});

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
    const cfg = makeConfig([makeServer()]);
    mockConfigFn.mockResolvedValue(cfg);
    mockUpdateConfigFn.mockResolvedValue({});

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

  it("preserves other server fields, including trust, when updating leaf preferences", async () => {
    const cfg = makeConfig([makeServer({ pinned_leaf_ids: ["proj-1"] })]);
    mockConfigFn.mockResolvedValue(cfg);
    mockUpdateConfigFn.mockResolvedValue({});

    const { result } = renderHook(() => useWriteLeafPreferences());

    await act(async () => {
      await result.current.write("lettuce.science", { mode: "SPECIFIC", enabled: ["prime"] });
    });

    const updatedServers = mockUpdateConfigFn.mock.calls[0][0].servers as ServerConfig[];
    expect(updatedServers[0].grpc_address).toBe("lettuce.science:443");
    expect(updatedServers[0].weight).toBe(70);
    expect(updatedServers[0].pinned_leaf_ids).toEqual(["proj-1"]);
    expect(updatedServers[0].trusted_runtimes).toEqual(["CONTAINER"]);
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
