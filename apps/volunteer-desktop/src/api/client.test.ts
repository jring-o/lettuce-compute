import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { invoke } from "@tauri-apps/api/core";
import {
  ManagementClient,
  ApiError,
  getContainerRuntimeStatus,
  setupContainerRuntime,
  startContainerRuntime,
  stopContainerRuntime,
} from "./client";

// The invoke mock is set up via the alias in vitest.config.ts

// Mock global fetch
const mockFetch = vi.fn();
vi.stubGlobal("fetch", mockFetch);

function jsonResponse(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 200 ? "OK" : "Error",
    json: () => Promise.resolve(body),
    headers: new Headers(),
    redirected: false,
    type: "basic",
    url: "",
    clone: () => ({} as Response),
    body: null,
    bodyUsed: false,
    arrayBuffer: () => Promise.resolve(new ArrayBuffer(0)),
    blob: () => Promise.resolve(new Blob()),
    formData: () => Promise.resolve(new FormData()),
    text: () => Promise.resolve(""),
  } as Response;
}

function noContentResponse(): Response {
  return {
    ok: true,
    status: 204,
    statusText: "No Content",
    json: () => Promise.reject(new Error("No body")),
    headers: new Headers(),
    redirected: false,
    type: "basic",
    url: "",
    clone: () => ({} as Response),
    body: null,
    bodyUsed: false,
    arrayBuffer: () => Promise.resolve(new ArrayBuffer(0)),
    blob: () => Promise.resolve(new Blob()),
    formData: () => Promise.resolve(new FormData()),
    text: () => Promise.resolve(""),
  } as Response;
}

describe("ManagementClient", () => {
  let client: ManagementClient;

  beforeEach(async () => {
    vi.mocked(invoke).mockResolvedValue({ port: 9876, token: "test-token-abc" });
    mockFetch.mockReset();
    client = await ManagementClient.create();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  describe("create", () => {
    it("calls invoke with get_daemon_info", async () => {
      expect(invoke).toHaveBeenCalledWith("get_daemon_info");
    });

    it("configures base URL from port", async () => {
      // Verify by making a request and checking the URL
      mockFetch.mockResolvedValue(jsonResponse({ state: "active" }));
      await client.status();
      expect(mockFetch).toHaveBeenCalledWith(
        "http://127.0.0.1:9876/api/v1/status",
        expect.any(Object)
      );
    });
  });

  describe("auth headers", () => {
    it("sends Bearer token in Authorization header", async () => {
      mockFetch.mockResolvedValue(jsonResponse({ state: "active" }));
      await client.status();
      const [, opts] = mockFetch.mock.calls[0];
      expect(opts.headers.Authorization).toBe("Bearer test-token-abc");
      expect(opts.headers["Content-Type"]).toBe("application/json");
    });
  });

  describe("status", () => {
    it("sends GET to /api/v1/status", async () => {
      const body = {
        state: "active",
        uptime_seconds: 3600,
        connected_servers: 2,
        active_tasks: [],
        paused_reason: null,
      };
      mockFetch.mockResolvedValue(jsonResponse(body));
      const result = await client.status();
      expect(result).toEqual(body);
      expect(mockFetch).toHaveBeenCalledWith(
        "http://127.0.0.1:9876/api/v1/status",
        expect.objectContaining({ method: "GET" })
      );
    });
  });

  describe("pause", () => {
    it("sends POST to /api/v1/daemon/pause", async () => {
      mockFetch.mockResolvedValue(noContentResponse());
      await client.pause();
      expect(mockFetch).toHaveBeenCalledWith(
        "http://127.0.0.1:9876/api/v1/daemon/pause",
        expect.objectContaining({ method: "POST" })
      );
    });
  });

  describe("resume", () => {
    it("sends POST to /api/v1/daemon/resume", async () => {
      mockFetch.mockResolvedValue(noContentResponse());
      await client.resume();
      expect(mockFetch).toHaveBeenCalledWith(
        "http://127.0.0.1:9876/api/v1/daemon/resume",
        expect.objectContaining({ method: "POST" })
      );
    });
  });

  describe("metrics", () => {
    it("sends GET to /api/v1/metrics", async () => {
      const body = {
        cpu_usage_pct: 45.2,
        gpu_usage_pct: 0,
        memory_used_mb: 8192,
        memory_total_mb: 16384,
        disk_used_gb: 100,
        disk_total_gb: 500,
        cpu_temp_c: 65,
        gpu_temp_c: 0,
      };
      mockFetch.mockResolvedValue(jsonResponse(body));
      const result = await client.metrics();
      expect(result).toEqual(body);
    });
  });

  describe("attachHead", () => {
    it("sends POST with body to /api/v1/leafs/attach", async () => {
      mockFetch.mockResolvedValue(noContentResponse());
      const req = { server_address: "grpc://example.com:50051", leaf_id: "leaf-1" };
      await client.attachHead(req);
      const [url, opts] = mockFetch.mock.calls[0];
      expect(url).toBe("http://127.0.0.1:9876/api/v1/leafs/attach");
      expect(opts.method).toBe("POST");
      expect(JSON.parse(opts.body)).toEqual(req);
    });
  });

  describe("detachHead", () => {
    it("sends POST with body to /api/v1/leafs/detach", async () => {
      mockFetch.mockResolvedValue(noContentResponse());
      const req = { server_name: "my-server" };
      await client.detachHead(req);
      const [url, opts] = mockFetch.mock.calls[0];
      expect(url).toBe("http://127.0.0.1:9876/api/v1/leafs/detach");
      expect(opts.method).toBe("POST");
      expect(JSON.parse(opts.body)).toEqual(req);
    });
  });

  describe("availableLeafs", () => {
    it("sends GET to /api/v1/leafs/available without params", async () => {
      mockFetch.mockResolvedValue(jsonResponse({ leafs: [] }));
      await client.availableLeafs();
      expect(mockFetch).toHaveBeenCalledWith(
        "http://127.0.0.1:9876/api/v1/leafs/available",
        expect.any(Object)
      );
    });

    it("appends query params when provided", async () => {
      mockFetch.mockResolvedValue(jsonResponse({ leafs: [] }));
      await client.availableLeafs({ query: "climate", server_address: "grpc://s.com:50051" });
      const [url] = mockFetch.mock.calls[0];
      expect(url).toContain("/api/v1/leafs/available?");
      expect(url).toContain("search=climate");
      expect(url).toContain("server_address=");
    });
  });

  describe("history", () => {
    it("sends GET to /api/v1/history without params", async () => {
      const body = { entries: [], pagination: { next_cursor: "", has_more: false } };
      mockFetch.mockResolvedValue(jsonResponse(body));
      await client.history();
      expect(mockFetch).toHaveBeenCalledWith(
        "http://127.0.0.1:9876/api/v1/history",
        expect.any(Object)
      );
    });

    it("appends cursor and limit params", async () => {
      const body = { entries: [], pagination: { next_cursor: "", has_more: false } };
      mockFetch.mockResolvedValue(jsonResponse(body));
      await client.history({ cursor: "abc123", limit: 25 });
      const [url] = mockFetch.mock.calls[0];
      expect(url).toContain("cursor=abc123");
      expect(url).toContain("limit=25");
    });
  });

  describe("config", () => {
    it("sends GET to /api/v1/config", async () => {
      const body = { data_dir: "/tmp", servers: [] };
      mockFetch.mockResolvedValue(jsonResponse(body));
      await client.config();
      expect(mockFetch).toHaveBeenCalledWith(
        "http://127.0.0.1:9876/api/v1/config",
        expect.objectContaining({ method: "GET" })
      );
    });
  });

  describe("updateConfig", () => {
    it("sends PUT with body to /api/v1/config", async () => {
      const partial = { log_level: "debug" };
      const returned = { data_dir: "/tmp", log_level: "debug", servers: [] };
      mockFetch.mockResolvedValue(jsonResponse(returned));
      const result = await client.updateConfig(partial);
      const [url, opts] = mockFetch.mock.calls[0];
      expect(url).toBe("http://127.0.0.1:9876/api/v1/config");
      expect(opts.method).toBe("PUT");
      expect(JSON.parse(opts.body)).toEqual(partial);
      expect(result).toEqual(returned);
    });
  });

  describe("credit", () => {
    it("sends GET to /api/v1/credit", async () => {
      const body = { total_credit: 1000, today: 50, this_week: 200, this_month: 800, by_leaf: [] };
      mockFetch.mockResolvedValue(jsonResponse(body));
      const result = await client.credit();
      expect(result).toEqual(body);
    });
  });

  describe("heads", () => {
    it("sends GET to /api/v1/heads and unwraps response", async () => {
      const headsData = [
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
      mockFetch.mockResolvedValue(jsonResponse({ heads: headsData }));
      const result = await client.heads();
      expect(result).toEqual(headsData);
      expect(mockFetch).toHaveBeenCalledWith(
        "http://127.0.0.1:9876/api/v1/heads",
        expect.objectContaining({ method: "GET" })
      );
    });
  });

  describe("signChallenge", () => {
    it("sends POST to /api/v1/identity/sign with challenge hex", async () => {
      const body = { public_key: "ed25519-pub-key", signature: "sig-hex" };
      mockFetch.mockResolvedValue(jsonResponse(body));
      const result = await client.signChallenge("deadbeef");
      expect(result).toEqual(body);
      const [url, opts] = mockFetch.mock.calls[0];
      expect(url).toBe("http://127.0.0.1:9876/api/v1/identity/sign");
      expect(opts.method).toBe("POST");
      expect(JSON.parse(opts.body)).toEqual({ challenge_hex: "deadbeef" });
    });
  });

  describe("error handling", () => {
    it("throws ApiError with code and message on structured error response", async () => {
      mockFetch.mockResolvedValue(
        jsonResponse(
          { error: { code: "NOT_FOUND", message: "Resource not found" } },
          404
        )
      );
      await expect(client.status()).rejects.toThrow(ApiError);
      try {
        await client.status();
      } catch (err) {
        expect(err).toBeInstanceOf(ApiError);
        expect((err as ApiError).code).toBe("NOT_FOUND");
        expect((err as ApiError).message).toBe("Resource not found");
      }
    });

    it("throws ApiError with UNKNOWN code when response is not JSON", async () => {
      const resp = {
        ok: false,
        status: 500,
        statusText: "Internal Server Error",
        json: () => Promise.reject(new Error("not json")),
        headers: new Headers(),
        redirected: false,
        type: "basic" as ResponseType,
        url: "",
        clone: () => ({} as Response),
        body: null,
        bodyUsed: false,
        arrayBuffer: () => Promise.resolve(new ArrayBuffer(0)),
        blob: () => Promise.resolve(new Blob()),
        formData: () => Promise.resolve(new FormData()),
        text: () => Promise.resolve(""),
      } as Response;
      mockFetch.mockResolvedValue(resp);
      await expect(client.status()).rejects.toThrow(ApiError);
      try {
        await client.status();
      } catch (err) {
        expect((err as ApiError).code).toBe("UNKNOWN");
        expect((err as ApiError).message).toContain("500");
      }
    });
  });

  describe("no body on 204", () => {
    it("returns undefined for 204 responses", async () => {
      mockFetch.mockResolvedValue(noContentResponse());
      const result = await client.pause();
      expect(result).toBeUndefined();
    });
  });

  // --- S102: Task management methods ---

  describe("suspendTask", () => {
    it("sends POST to /api/v1/tasks/:id/suspend", async () => {
      mockFetch.mockResolvedValue(noContentResponse());
      await client.suspendTask("wu-abc-123");
      const [url, opts] = mockFetch.mock.calls[0];
      expect(url).toBe("http://127.0.0.1:9876/api/v1/tasks/wu-abc-123/suspend");
      expect(opts.method).toBe("POST");
    });

    it("returns undefined on success", async () => {
      mockFetch.mockResolvedValue(noContentResponse());
      const result = await client.suspendTask("wu-abc-123");
      expect(result).toBeUndefined();
    });
  });

  describe("resumeTask", () => {
    it("sends POST to /api/v1/tasks/:id/resume", async () => {
      mockFetch.mockResolvedValue(noContentResponse());
      await client.resumeTask("wu-def-456");
      const [url, opts] = mockFetch.mock.calls[0];
      expect(url).toBe("http://127.0.0.1:9876/api/v1/tasks/wu-def-456/resume");
      expect(opts.method).toBe("POST");
    });

    it("returns undefined on success", async () => {
      mockFetch.mockResolvedValue(noContentResponse());
      const result = await client.resumeTask("wu-def-456");
      expect(result).toBeUndefined();
    });
  });

  describe("abortTask", () => {
    it("sends POST to /api/v1/tasks/:id/abort", async () => {
      mockFetch.mockResolvedValue(noContentResponse());
      await client.abortTask("wu-ghi-789");
      const [url, opts] = mockFetch.mock.calls[0];
      expect(url).toBe("http://127.0.0.1:9876/api/v1/tasks/wu-ghi-789/abort");
      expect(opts.method).toBe("POST");
    });

    it("returns undefined on success", async () => {
      mockFetch.mockResolvedValue(noContentResponse());
      const result = await client.abortTask("wu-ghi-789");
      expect(result).toBeUndefined();
    });

    it("throws ApiError on 404", async () => {
      mockFetch.mockResolvedValue(
        jsonResponse(
          { error: { code: "NOT_FOUND", message: "Task not found" } },
          404
        )
      );
      await expect(client.abortTask("wu-missing")).rejects.toThrow(ApiError);
    });
  });

  describe("taskDetails", () => {
    it("sends GET to /api/v1/tasks/:id/details", async () => {
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
        runtime_type: "native",
        process_id: 12345,
        work_dir: "/tmp/wu-detail-001",
        memory_rss_mb: 512,
        virtual_memory_mb: 1024,
        cpu_usage_pct: 98.5,
        disk_read_mb: 10,
        disk_written_mb: 5,
        time_since_checkpoint_seconds: 120,
        estimated_completion_at: "2026-03-29T12:00:00Z",
        progress_rate_pct_per_hour: 2.5,
        fraction_done: 0.55,
        container_image: null,
      };
      mockFetch.mockResolvedValue(jsonResponse(detail));
      const result = await client.taskDetails("wu-detail-001");
      expect(result).toEqual(detail);
      const [url, opts] = mockFetch.mock.calls[0];
      expect(url).toBe("http://127.0.0.1:9876/api/v1/tasks/wu-detail-001/details");
      expect(opts.method).toBe("GET");
    });

    it("returns TaskDetail with container_image for container tasks", async () => {
      const detail = {
        work_unit_id: "wu-container-001",
        leaf_name: "Docker Task",
        progress_pct: 30,
        elapsed_seconds: 500,
        cpu_seconds: 480,
        task_status: "running",
        status_reason: null,
        deadline_seconds: 43200,
        head_name: "compute.example.com",
        runtime_type: "container",
        process_id: null,
        work_dir: "/tmp/wu-container-001",
        memory_rss_mb: 256,
        virtual_memory_mb: 512,
        cpu_usage_pct: 45.0,
        disk_read_mb: 2,
        disk_written_mb: 1,
        time_since_checkpoint_seconds: null,
        estimated_completion_at: null,
        progress_rate_pct_per_hour: null,
        fraction_done: 0.30,
        container_image: "ghcr.io/research/climate:latest",
      };
      mockFetch.mockResolvedValue(jsonResponse(detail));
      const result = await client.taskDetails("wu-container-001");
      expect(result.container_image).toBe("ghcr.io/research/climate:latest");
      expect(result.runtime_type).toBe("container");
      expect(result.process_id).toBeNull();
    });
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
