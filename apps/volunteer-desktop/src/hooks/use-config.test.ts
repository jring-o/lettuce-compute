import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import { useConfig } from "./use-config";
import type { ConfigResponse, ManagementClient } from "@/api/client";

const mockConfigFn = vi.fn<() => Promise<ConfigResponse>>();
const mockUpdateConfigFn = vi.fn<(partial: any) => Promise<ConfigResponse>>();
const mockClient = {
  config: mockConfigFn,
  updateConfig: mockUpdateConfigFn,
} as unknown as ManagementClient;

vi.mock("./use-api", () => ({
  useClient: () => ({ client: mockClient, error: null }),
  useApiQuery: (fetcher: (c: ManagementClient) => Promise<any>) => {
    // Simulate useApiQuery: call fetcher once, track state
    const { useState, useEffect, useCallback } = require("react");
    const [data, setData] = useState(null);
    const [isLoading, setIsLoading] = useState(true);
    const [error, setError] = useState(null);

    const refetch = useCallback(() => {
      setIsLoading(true);
      fetcher(mockClient)
        .then((result: any) => {
          setData(result);
          setError(null);
        })
        .catch((err: any) => {
          setError(err instanceof Error ? err : new Error(String(err)));
        })
        .finally(() => {
          setIsLoading(false);
        });
    }, []);

    useEffect(() => {
      refetch();
    }, [refetch]);

    return { data, isLoading, error, refetch };
  },
}));

function makeConfig(): ConfigResponse {
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
    servers: [],
    log_level: "info",
    max_concurrent_tasks: 1,
    available_runtimes: ["NATIVE"],
  };
}

describe("useConfig", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("loads config on mount", async () => {
    const cfg = makeConfig();
    mockConfigFn.mockResolvedValueOnce(cfg);

    const { result } = renderHook(() => useConfig());

    // Wait for initial load
    await vi.waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.config).toEqual(cfg);
    expect(result.current.error).toBeNull();
  });

  it("calls updateConfig on client and shows toast", async () => {
    const cfg = makeConfig();
    mockConfigFn.mockResolvedValue(cfg);

    const updatedCfg = { ...cfg, log_level: "debug" };
    mockUpdateConfigFn.mockResolvedValueOnce(updatedCfg);

    const { result } = renderHook(() => useConfig());

    await vi.waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    await act(async () => {
      await result.current.updateConfig({ log_level: "debug" });
    });

    expect(mockUpdateConfigFn).toHaveBeenCalledWith({ log_level: "debug" });
    expect(result.current.toast).toBe("Saved");
  });

  it("shows error toast when update fails", async () => {
    const cfg = makeConfig();
    mockConfigFn.mockResolvedValue(cfg);
    mockUpdateConfigFn.mockRejectedValueOnce(new Error("Validation failed"));

    const { result } = renderHook(() => useConfig());

    await vi.waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    await act(async () => {
      await result.current.updateConfig({ log_level: "invalid" });
    });

    expect(result.current.toast).toBe("Error: Validation failed");
  });

  it("sets saving=true during update", async () => {
    const cfg = makeConfig();
    mockConfigFn.mockResolvedValue(cfg);

    let resolveFn: (value: any) => void;
    mockUpdateConfigFn.mockReturnValueOnce(
      new Promise((resolve) => {
        resolveFn = resolve;
      })
    );

    const { result } = renderHook(() => useConfig());

    await vi.waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    expect(result.current.saving).toBe(false);

    // Start the update but don't resolve yet
    let updatePromise: Promise<void>;
    act(() => {
      updatePromise = result.current.updateConfig({ log_level: "debug" });
    });

    // saving should be true during the update
    expect(result.current.saving).toBe(true);

    // Now resolve
    await act(async () => {
      resolveFn!(cfg);
      await updatePromise!;
    });

    expect(result.current.saving).toBe(false);
  });
});
