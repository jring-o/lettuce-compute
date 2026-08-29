import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { invoke } from "@tauri-apps/api/core";
import {
  ManagementClient,
  ApiError,
  getContainerRuntimeStatus,
  setupContainerRuntime,
  startContainerRuntime,
  stopContainerRuntime,
  getSystemMetrics,
  getClientVersion,
  restartDaemon,
} from "./client";

// `invoke` is the mock in src/__mocks__/@tauri-apps/api/core.ts (aliased in
// vitest.config.ts). Every management-API call must arrive as
// invoke("mgmt_request", { method, path, body }); no browser fetch is used.

interface MgmtArgs {
  method: string;
  path: string;
  body: unknown;
}

/** Arguments of the n-th `mgmt_request` invoke (default: the last one). */
function mgmtCall(index?: number): MgmtArgs {
  const calls = vi
    .mocked(invoke)
    .mock.calls.filter(([cmd]) => cmd === "mgmt_request");
  const call = calls[index ?? calls.length - 1];
  if (!call) throw new Error("no mgmt_request invoke recorded");
  return call[1] as MgmtArgs;
}

/** Make the next `mgmt_request` invoke resolve with `value`. */
function respond(value: unknown) {
  vi.mocked(invoke).mockResolvedValueOnce(value);
}

/** Make the next `mgmt_request` invoke reject the way the Rust command does. */
function fail(err: unknown) {
  vi.mocked(invoke).mockRejectedValueOnce(err);
}

describe("ManagementClient", () => {
  let client: ManagementClient;

  beforeEach(async () => {
    vi.mocked(invoke).mockReset();
    vi.mocked(invoke).mockResolvedValue(undefined);
    client = await ManagementClient.create();
    vi.mocked(invoke).mockClear();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  describe("create", () => {
    it("probes GET /api/v1/status through mgmt_request", async () => {
      vi.mocked(invoke).mockReset();
      vi.mocked(invoke).mockResolvedValue({ state: "active" });
      await ManagementClient.create();
      expect(invoke).toHaveBeenCalledTimes(1);
      expect(invoke).toHaveBeenCalledWith("mgmt_request", {
        method: "GET",
        path: "/api/v1/status",
        body: null,
      });
    });

    it("rejects with DAEMON_UNREACHABLE while the daemon is not up", async () => {
      vi.mocked(invoke).mockReset();
      vi.mocked(invoke).mockRejectedValue({
        code: "DAEMON_UNREACHABLE",
        message: "Failed to read daemon.json",
        status: 0,
      });
      await expect(ManagementClient.create()).rejects.toMatchObject({
        name: "ApiError",
        code: "DAEMON_UNREACHABLE",
        status: 0,
      });
    });

    it("never calls browser fetch", async () => {
      const fetchSpy = vi.fn();
      vi.stubGlobal("fetch", fetchSpy);
      respond({ state: "active" });
      await client.status();
      expect(fetchSpy).not.toHaveBeenCalled();
      vi.unstubAllGlobals();
    });
  });

  describe("status", () => {
    it("sends GET /api/v1/status and normalises list fields", async () => {
      respond({
        state: "paused",
        uptime_seconds: 3600,
        connected_servers: 2,
        active_tasks: null,
        queued_tasks: undefined,
        paused_reason: "scheduled",
        failing_leafs: [{ leaf_id: "l1", leaf_name: "L", consecutive_failures: 1, total_failures: 1, paused: false }],
      });
      const result = await client.status();
      expect(mgmtCall()).toEqual({ method: "GET", path: "/api/v1/status", body: null });
      expect(result.state).toBe("paused");
      expect(result.paused_reason).toBe("scheduled");
      expect(result.active_tasks).toEqual([]);
      expect(result.queued_tasks).toEqual([]);
      expect(result.failing_leafs).toHaveLength(1);
    });
  });

  describe("pause / resume", () => {
    it("sends POST /api/v1/daemon/pause with no body", async () => {
      respond({ state: "paused" });
      const result = await client.pause();
      expect(result).toBeUndefined();
      expect(mgmtCall()).toEqual({ method: "POST", path: "/api/v1/daemon/pause", body: null });
    });

    it("sends POST /api/v1/daemon/resume", async () => {
      respond({ state: "active" });
      await client.resume();
      expect(mgmtCall()).toEqual({ method: "POST", path: "/api/v1/daemon/resume", body: null });
    });
  });

  describe("metrics", () => {
    it("sends GET /api/v1/metrics and returns the current shape", async () => {
      const body = {
        cpu_usage_pct: 0,
        gpu_usage_pct: 0,
        memory_used_mb: 0,
        memory_total_mb: 0,
        cpu_temp_c: 0,
        gpu_temp_c: 0,
        disk_used_mb: 2048,
        disk_allowance_mb: 10240,
        disk_usage_known: true,
      };
      respond(body);
      const result = await client.metrics();
      expect(result).toEqual(body);
      expect(mgmtCall()).toEqual({ method: "GET", path: "/api/v1/metrics", body: null });
    });
  });

  describe("notices", () => {
    it("sends GET /api/v1/notices without since", async () => {
      respond({ notices: [], latest_id: 0 });
      const result = await client.notices();
      expect(mgmtCall().path).toBe("/api/v1/notices");
      expect(result).toEqual({ notices: [], latest_id: 0 });
    });

    it("passes since as a query parameter and normalises a null list", async () => {
      respond({ notices: null, latest_id: 7 });
      const result = await client.notices(7);
      expect(mgmtCall().path).toBe("/api/v1/notices?since=7");
      expect(result).toEqual({ notices: [], latest_id: 7 });
    });
  });

  describe("attachHead / detachHead", () => {
    it("sends POST /api/v1/leafs/attach with the request body", async () => {
      respond({ status: "attached" });
      const req = {
        server_address: "grpc://example.com:50051",
        leaf_id: "leaf-1",
        trusted_runtimes: ["CONTAINER"],
      };
      await client.attachHead(req);
      expect(mgmtCall()).toEqual({ method: "POST", path: "/api/v1/leafs/attach", body: req });
    });

    it("sends POST /api/v1/leafs/detach with the request body", async () => {
      respond({ status: "detached" });
      const req = { server_name: "my-server" };
      await client.detachHead(req);
      expect(mgmtCall()).toEqual({ method: "POST", path: "/api/v1/leafs/detach", body: req });
    });
  });

  describe("leaf listings", () => {
    it("attachedLeafs sends GET /api/v1/leafs and unwraps", async () => {
      const rows = [
        {
          server_name: "lettuce.science",
          server_address: "lettuce.science:443",
          leaf_id: "leaf-1",
          status: "connected",
          credit_earned: 3,
          work_units_completed: 3,
        },
      ];
      respond({ leafs: rows });
      expect(await client.attachedLeafs()).toEqual(rows);
      expect(mgmtCall()).toEqual({ method: "GET", path: "/api/v1/leafs", body: null });
    });

    it("availableLeafs sends GET /api/v1/leafs/browse without params", async () => {
      respond({ leafs: null });
      expect(await client.availableLeafs()).toEqual([]);
      expect(mgmtCall().path).toBe("/api/v1/leafs/browse");
    });

    it("availableLeafs encodes search and research_area", async () => {
      respond({ leafs: [] });
      await client.availableLeafs({ query: "climate", research_area: "earth" });
      const { path } = mgmtCall();
      expect(path).toContain("/api/v1/leafs/browse?");
      expect(path).toContain("search=climate");
      expect(path).toContain("research_area=earth");
    });

    it("catalogLeafs sends GET /api/v1/leafs/available and normalises research_area", async () => {
      respond({ leafs: [{ id: "l1", slug: "prime", name: "Prime" }] });
      const result = await client.catalogLeafs();
      expect(mgmtCall().path).toBe("/api/v1/leafs/available");
      expect(result[0].research_area).toEqual([]);
    });
  });

  describe("history", () => {
    it("sends GET /api/v1/history without params", async () => {
      const body = { entries: [], pagination: { next_cursor: "", has_more: false } };
      respond(body);
      expect(await client.history()).toEqual(body);
      expect(mgmtCall()).toEqual({ method: "GET", path: "/api/v1/history", body: null });
    });

    it("appends cursor and limit params", async () => {
      respond({ entries: [], pagination: { next_cursor: "", has_more: false } });
      await client.history({ cursor: "abc123", limit: 25 });
      const { path } = mgmtCall();
      expect(path).toContain("/api/v1/history?");
      expect(path).toContain("cursor=abc123");
      expect(path).toContain("limit=25");
    });
  });

  describe("config", () => {
    it("sends GET /api/v1/config and normalises trusted_runtimes", async () => {
      respond({
        data_dir: "/tmp",
        work_buffer_hours: 2,
        servers: [
          { grpc_address: "a:443", name: "a", leaf_preferences: { mode: "ALL" }, trusted_runtimes: null },
          { grpc_address: "b:443", name: "b", leaf_preferences: { mode: "ALL" }, trusted_runtimes: ["CONTAINER"] },
        ],
      });
      const result = await client.config();
      expect(mgmtCall()).toEqual({ method: "GET", path: "/api/v1/config", body: null });
      expect(result.work_buffer_hours).toBe(2);
      expect(result.servers[0].trusted_runtimes).toEqual([]);
      expect(result.servers[1].trusted_runtimes).toEqual(["CONTAINER"]);
    });

    it("sends PUT /api/v1/config with the partial body", async () => {
      const partial = { log_level: "debug", work_buffer_hours: 3 };
      respond({ status: "ok", restart_required: false });
      const result = await client.updateConfig(partial);
      expect(mgmtCall()).toEqual({ method: "PUT", path: "/api/v1/config", body: partial });
      expect(result).toEqual({ status: "ok", restart_required: false });
    });
  });

  describe("credit", () => {
    it("sends GET /api/v1/credit and keeps decimal figures", async () => {
      const body = {
        total_credit: 1234.5,
        today: 12.25,
        this_week: 100,
        this_month: 800.75,
        by_leaf: [{ leaf_id: "p1", leaf_name: "Primes", credit: 1234.5 }],
        by_head: [{ head_name: "lettuce.science", volunteer_id: "v1", total_credit: 1234.5, available: true }],
        source: "head",
      };
      respond(body);
      expect(await client.credit()).toEqual(body);
      expect(mgmtCall()).toEqual({ method: "GET", path: "/api/v1/credit", body: null });
    });

    it("normalises null by_leaf and by_head", async () => {
      respond({ total_credit: 0, today: 0, this_week: 0, this_month: 0, by_leaf: null, by_head: null, source: "local" });
      const result = await client.credit();
      expect(result.by_leaf).toEqual([]);
      expect(result.by_head).toEqual([]);
    });
  });

  describe("heads", () => {
    const leaf = {
      id: "leaf-1",
      slug: "prime",
      name: "Prime Study",
      description: "Primes",
      task_pattern: "PARAMETER_SWEEP",
      state: "ACTIVE",
      queued_work_units: 100,
      active_volunteers: 5,
      active_hosts: 7,
      enabled: true,
      effective_weight: 50,
      resource_requirements: { min_disk_mb: 2048, gpu_required: false },
      disk_gate: { blocked: false },
    };
    const head = {
      name: "lettuce.science",
      description: "Open science",
      url: "https://lettuce.science",
      grpc_address: "lettuce.science:443",
      status: "connected",
      weight: 70,
      leafs_refreshed_at: "2026-08-29T10:00:00Z",
      leafs: [{ ...leaf, research_area: ["math"] }, { ...leaf, id: "leaf-2", slug: "two" }],
    };
    const machine = {
      runtimes: ["container", "wasm"],
      has_gpu: false,
      max_memory_mb: 4096,
      max_disk_mb: 10240,
      max_cpu_cores: 4,
      max_gpu_vram_mb: 0,
      gpu_card_vram_mb: 0,
      gpu_vram_pct: 50,
      gpu_vendors: null,
      gpu_compute_capabilities: null,
    };

    it("headsAndMachine sends GET /api/v1/heads and normalises arrays", async () => {
      respond({ heads: [head], machine });
      const result = await client.headsAndMachine();
      expect(mgmtCall()).toEqual({ method: "GET", path: "/api/v1/heads", body: null });
      expect(result.heads[0].leafs[0].research_area).toEqual(["math"]);
      expect(result.heads[0].leafs[1].research_area).toEqual([]);
      expect(result.heads[0].leafs_refreshed_at).toBe("2026-08-29T10:00:00Z");
      expect(result.machine.runtimes).toEqual(["container", "wasm"]);
      expect(result.machine.gpu_vendors).toEqual([]);
      expect(result.machine.gpu_compute_capabilities).toEqual([]);
      expect(result.machine.max_disk_mb).toBe(10240);
    });

    it("heads returns only the head list", async () => {
      respond({ heads: [head], machine });
      const result = await client.heads();
      expect(result).toHaveLength(1);
      expect(result[0].name).toBe("lettuce.science");
    });

    it("tolerates a missing machine block", async () => {
      respond({ heads: null });
      const result = await client.headsAndMachine();
      expect(result.heads).toEqual([]);
      expect(result.machine.runtimes).toEqual([]);
      expect(result.machine.has_gpu).toBe(false);
    });
  });

  describe("signChallenge", () => {
    it("sends POST /api/v1/identity/sign with challenge hex", async () => {
      const body = { public_key: "ed25519-pub-key", signature: "sig" };
      respond(body);
      const result = await client.signChallenge("deadbeef");
      expect(result).toEqual(body);
      expect(mgmtCall()).toEqual({
        method: "POST",
        path: "/api/v1/identity/sign",
        body: { challenge_hex: "deadbeef" },
      });
    });
  });

  describe("error handling", () => {
    it("maps the Rust error object to ApiError with code, message and status", async () => {
      fail({ code: "NOT_FOUND", message: "Resource not found", status: 404 });
      const err = await client.status().catch((e) => e);
      expect(err).toBeInstanceOf(ApiError);
      expect(err.code).toBe("NOT_FOUND");
      expect(err.message).toBe("Resource not found");
      expect(err.status).toBe(404);
    });

    it("maps a transport failure to DAEMON_UNREACHABLE", async () => {
      fail({ code: "DAEMON_UNREACHABLE", message: "connection refused", status: 0 });
      await expect(client.metrics()).rejects.toMatchObject({
        code: "DAEMON_UNREACHABLE",
        status: 0,
      });
    });

    it("wraps a plain string rejection as UNKNOWN", async () => {
      fail("something odd");
      const err = await client.status().catch((e) => e);
      expect(err).toBeInstanceOf(ApiError);
      expect(err.code).toBe("UNKNOWN");
      expect(err.message).toBe("something odd");
      expect(err.status).toBe(0);
    });

    it("passes an ApiError through unchanged", async () => {
      fail(new ApiError("CONFLICT", "already paused", 409));
      const err = await client.pause().catch((e) => e);
      expect(err.code).toBe("CONFLICT");
      expect(err.status).toBe(409);
    });
  });

  describe("empty responses", () => {
    it("returns undefined when the Rust side yields null (204)", async () => {
      respond(null);
      const result = await client.pause();
      expect(result).toBeUndefined();
    });
  });

  describe("task management", () => {
    it("suspendTask sends POST /api/v1/tasks/:id/suspend", async () => {
      respond({ status: "suspended" });
      const result = await client.suspendTask("wu-abc-123");
      expect(result).toBeUndefined();
      expect(mgmtCall()).toEqual({ method: "POST", path: "/api/v1/tasks/wu-abc-123/suspend", body: null });
    });

    it("resumeTask sends POST /api/v1/tasks/:id/resume", async () => {
      respond({ status: "resumed" });
      await client.resumeTask("wu-def-456");
      expect(mgmtCall()).toEqual({ method: "POST", path: "/api/v1/tasks/wu-def-456/resume", body: null });
    });

    it("abortTask sends POST /api/v1/tasks/:id/abort", async () => {
      respond({ status: "aborted" });
      await client.abortTask("wu-ghi-789");
      expect(mgmtCall()).toEqual({ method: "POST", path: "/api/v1/tasks/wu-ghi-789/abort", body: null });
    });

    it("abortTask throws ApiError on 404", async () => {
      fail({ code: "NOT_FOUND", message: "Task not found", status: 404 });
      await expect(client.abortTask("wu-missing")).rejects.toThrow(ApiError);
    });

    it("taskDetails sends GET /api/v1/tasks/:id/details", async () => {
      const detail = {
        work_unit_id: "wu-detail-001",
        leaf_name: "Climate Model",
        progress_pct: 55,
        elapsed_seconds: 2000,
        cpu_seconds: 1800,
        task_status: "running",
        status_reason: null,
        deadline_seconds: 86400,
        head_name: "lettuce.science",
        runtime_type: "container",
        process_id: null,
        work_dir: "/tmp/wu-detail-001",
        viz_bundle_path: null,
        memory_rss_mb: 512,
        virtual_memory_mb: 1024,
        cpu_usage_pct: 98.5,
        disk_read_mb: 10,
        disk_written_mb: 5,
        time_since_checkpoint_seconds: 120,
        estimated_completion_at: "2026-03-29T12:00:00Z",
        progress_rate_pct_per_hour: 2.5,
        fraction_done: 55,
        container_image: "ghcr.io/research/climate:latest",
      };
      respond(detail);
      const result = await client.taskDetails("wu-detail-001");
      expect(result).toEqual(detail);
      expect(mgmtCall()).toEqual({ method: "GET", path: "/api/v1/tasks/wu-detail-001/details", body: null });
    });
  });

  describe("results", () => {
    it("results sends GET /api/v1/results and normalises a null list", async () => {
      respond({ results: null });
      expect(await client.results()).toEqual({ results: [] });
      expect(mgmtCall().path).toBe("/api/v1/results");
    });

    it("resultData sends GET /api/v1/results/:id", async () => {
      respond({ answer: 42 });
      expect(await client.resultData("wu-1")).toEqual({ answer: 42 });
      expect(mgmtCall().path).toBe("/api/v1/results/wu-1");
    });
  });
});

describe("Host-side command wrappers", () => {
  beforeEach(() => {
    vi.mocked(invoke).mockReset();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("getSystemMetrics invokes system_metrics", async () => {
    const metrics = { cpu_usage_pct: 12.5, memory_used_mb: 4096, memory_total_mb: 16384 };
    vi.mocked(invoke).mockResolvedValue(metrics);
    expect(await getSystemMetrics()).toEqual(metrics);
    expect(invoke).toHaveBeenCalledWith("system_metrics");
  });

  it("getClientVersion invokes get_client_version", async () => {
    vi.mocked(invoke).mockResolvedValue("1.4.0");
    expect(await getClientVersion()).toBe("1.4.0");
    expect(invoke).toHaveBeenCalledWith("get_client_version");
  });

  it("restartDaemon invokes restart_daemon", async () => {
    vi.mocked(invoke).mockResolvedValue(undefined);
    await restartDaemon();
    expect(invoke).toHaveBeenCalledWith("restart_daemon");
  });
});

describe("Container Runtime API functions", () => {
  beforeEach(() => {
    vi.mocked(invoke).mockReset();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  describe("getContainerRuntimeStatus", () => {
    it("invokes get_container_runtime_status command", async () => {
      const mockStatus = {
        backend: "podman" as const,
        status: "running" as const,
        version: "5.3.1",
        socket_path: "/run/podman/podman.sock",
        machine_required: false,
        machine_name: "",
        machine_cpus: 0,
        machine_memory_mb: 0,
        machine_disk_gb: 0,
        error: null,
      };
      vi.mocked(invoke).mockResolvedValue(mockStatus);

      const result = await getContainerRuntimeStatus();

      expect(invoke).toHaveBeenCalledWith("get_container_runtime_status");
      expect(result).toEqual(mockStatus);
    });

    it("returns status with error field populated", async () => {
      const mockStatus = {
        backend: "none" as const,
        status: "not_installed" as const,
        version: "",
        socket_path: "",
        machine_required: false,
        machine_name: "",
        machine_cpus: 0,
        machine_memory_mb: 0,
        machine_disk_gb: 0,
        error: "podman not found in PATH",
      };
      vi.mocked(invoke).mockResolvedValue(mockStatus);

      const result = await getContainerRuntimeStatus();

      expect(result.backend).toBe("none");
      expect(result.status).toBe("not_installed");
      expect(result.error).toBe("podman not found in PATH");
    });
  });

  describe("setupContainerRuntime", () => {
    it("invokes setup_container_runtime with resource parameters", async () => {
      const mockResponse = { status: "ok", message: "Machine created" };
      vi.mocked(invoke).mockResolvedValue(mockResponse);

      const result = await setupContainerRuntime(4, 4096, 20);

      expect(invoke).toHaveBeenCalledWith("setup_container_runtime", {
        cpus: 4,
        memory_mb: 4096,
        disk_gb: 20,
      });
      expect(result).toEqual(mockResponse);
    });

    it("invokes setup_container_runtime with undefined params when not provided", async () => {
      const mockResponse = { status: "ok", message: "Machine created with defaults" };
      vi.mocked(invoke).mockResolvedValue(mockResponse);

      const result = await setupContainerRuntime();

      expect(invoke).toHaveBeenCalledWith("setup_container_runtime", {
        cpus: undefined,
        memory_mb: undefined,
        disk_gb: undefined,
      });
      expect(result).toEqual(mockResponse);
    });

    it("invokes setup_container_runtime with partial params", async () => {
      const mockResponse = { status: "ok", message: "Done" };
      vi.mocked(invoke).mockResolvedValue(mockResponse);

      await setupContainerRuntime(2, undefined, 10);

      expect(invoke).toHaveBeenCalledWith("setup_container_runtime", {
        cpus: 2,
        memory_mb: undefined,
        disk_gb: 10,
      });
    });
  });

  describe("startContainerRuntime", () => {
    it("invokes start_container_runtime command", async () => {
      const mockResponse = { status: "ok", message: "Runtime started" };
      vi.mocked(invoke).mockResolvedValue(mockResponse);

      const result = await startContainerRuntime();

      expect(invoke).toHaveBeenCalledWith("start_container_runtime");
      expect(result).toEqual(mockResponse);
    });
  });

  describe("stopContainerRuntime", () => {
    it("invokes stop_container_runtime command", async () => {
      const mockResponse = { status: "ok", message: "Runtime stopped" };
      vi.mocked(invoke).mockResolvedValue(mockResponse);

      const result = await stopContainerRuntime();

      expect(invoke).toHaveBeenCalledWith("stop_container_runtime");
      expect(result).toEqual(mockResponse);
    });
  });
});
