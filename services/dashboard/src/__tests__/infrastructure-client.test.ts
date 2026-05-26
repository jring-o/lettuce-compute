import {
  InfrastructureClient,
  InfrastructureApiError,
} from "@/lib/infrastructure-client";
import type {
  Leaf,
  LeafSummary,
  PaginatedResponse,
  Result,
  WorkUnit,
  WorkUnitSummary,
  LeafStats,
  GenerateWorkUnitsResponse,
  HealthResponse,
  AggregationResult,
} from "@/types/infrastructure";

const mockFetch = jest.fn();
global.fetch = mockFetch;

const client = new InfrastructureClient("http://localhost:8080");

function mockResponse(status: number, body: unknown) {
  mockFetch.mockResolvedValueOnce({
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  });
}

function mockNoContentResponse() {
  mockFetch.mockResolvedValueOnce({
    ok: true,
    status: 204,
    json: () => Promise.reject(new Error("No content")),
  });
}

const mockLeaf: Leaf = {
  id: "p1",
  name: "Test Leaf",
  slug: "test-leaf",
  description: "A test",
  state: "DRAFT",
  task_pattern: "PARAMETER_SWEEP",
  research_area: "physics",
  creator_id: "u1",
  execution_config: null,
  validation_config: null,
  fault_tolerance_config: null,
  data_config: null,
  credit_config: null,
  resource_requirements: null,
  is_ongoing: false,
  visibility: "PUBLIC",
  stats_cache_seconds: 60,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

const mockLeafSummary: LeafSummary = {
  id: "p1",
  name: "Test Leaf",
  slug: "test-leaf",
  description: "A test",
  research_area: "physics",
  state: "DRAFT",
  task_pattern: "PARAMETER_SWEEP",
  resource_requirements: null,
  runtime: "CONTAINER",
  is_ongoing: false,
  visibility: "PUBLIC",
  stats_cache_seconds: 60,
  active_volunteers: 0,
  progress_pct: null,
  created_at: "2026-01-01T00:00:00Z",
};

beforeEach(() => {
  mockFetch.mockReset();
});

describe("InfrastructureClient", () => {
  // --- Health ---

  describe("getHealth", () => {
    it("returns typed health response", async () => {
      const health: HealthResponse = {
        status: "ok",
        uptime_seconds: 120,
        database: "connected",
      };
      mockResponse(200, health);

      const result = await client.getHealth();
      expect(result).toEqual(health);
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/health/detailed",
        expect.objectContaining({ method: "GET" }),
      );
    });
  });

  // --- Projects ---

  describe("createLeaf", () => {
    it("sends POST and returns leaf", async () => {
      mockResponse(201, mockLeaf);

      const result = await client.createLeaf({
        name: "Test Leaf",
        description: "A test",
        task_pattern: "PARAMETER_SWEEP",
        creator_id: "u1",
      });

      expect(result).toEqual(mockLeaf);
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/leafs",
        expect.objectContaining({
          method: "POST",
          body: expect.any(String),
        }),
      );
    });
  });

  describe("getLeaf", () => {
    it("returns leaf by ID", async () => {
      mockResponse(200, mockLeaf);

      const result = await client.getLeaf("p1");
      expect(result).toEqual(mockLeaf);
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/leafs/p1",
        expect.objectContaining({ method: "GET" }),
      );
    });

    it("throws InfrastructureApiError on 404", async () => {
      mockResponse(404, {
        error: { code: "NOT_FOUND", message: "Leaf not found" },
      });

      await expect(client.getLeaf("missing")).rejects.toThrow(
        InfrastructureApiError,
      );

      try {
        mockResponse(404, {
          error: { code: "NOT_FOUND", message: "Leaf not found" },
        });
        await client.getLeaf("missing");
      } catch (err) {
        expect(err).toBeInstanceOf(InfrastructureApiError);
        const apiErr = err as InfrastructureApiError;
        expect(apiErr.code).toBe("NOT_FOUND");
        expect(apiErr.status).toBe(404);
        expect(apiErr.message).toBe("Leaf not found");
      }
    });
  });

  describe("listLeafs", () => {
    it("returns paginated response", async () => {
      const paginatedResponse: PaginatedResponse<LeafSummary> = {
        data: [mockLeafSummary],
        pagination: { next_cursor: null, has_more: false },
      };
      mockResponse(200, paginatedResponse);

      const result = await client.listLeafs();
      expect(result.data).toHaveLength(1);
      expect(result.pagination.has_more).toBe(false);
    });

    it("serializes query parameters correctly", async () => {
      const paginatedResponse: PaginatedResponse<LeafSummary> = {
        data: [],
        pagination: { next_cursor: null, has_more: false },
      };
      mockResponse(200, paginatedResponse);

      await client.listLeafs({
        creator_id: "u1",
        state: "ACTIVE",
        sort: "name",
        order: "asc",
        limit: 10,
      });

      const calledUrl = mockFetch.mock.calls[0][0] as string;
      expect(calledUrl).toContain("creator_id=u1");
      expect(calledUrl).toContain("state=ACTIVE");
      expect(calledUrl).toContain("sort=name");
      expect(calledUrl).toContain("order=asc");
      expect(calledUrl).toContain("limit=10");
    });

    it("forwards cursor for pagination", async () => {
      const paginatedResponse: PaginatedResponse<LeafSummary> = {
        data: [],
        pagination: { next_cursor: null, has_more: false },
      };
      mockResponse(200, paginatedResponse);

      await client.listLeafs({ cursor: "abc123" });

      const calledUrl = mockFetch.mock.calls[0][0] as string;
      expect(calledUrl).toContain("cursor=abc123");
    });

    it("omits undefined parameters", async () => {
      const paginatedResponse: PaginatedResponse<LeafSummary> = {
        data: [],
        pagination: { next_cursor: null, has_more: false },
      };
      mockResponse(200, paginatedResponse);

      await client.listLeafs({ creator_id: "u1" });

      const calledUrl = mockFetch.mock.calls[0][0] as string;
      expect(calledUrl).not.toContain("state=");
      expect(calledUrl).not.toContain("cursor=");
    });
  });

  describe("updateLeaf", () => {
    it("sends PUT with partial data", async () => {
      mockResponse(200, { ...mockLeaf, name: "Updated" });

      const result = await client.updateLeaf("p1", { name: "Updated" });
      expect(result.name).toBe("Updated");
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/leafs/p1",
        expect.objectContaining({ method: "PUT" }),
      );
    });
  });

  describe("deleteLeaf", () => {
    it("sends DELETE and returns void", async () => {
      mockNoContentResponse();

      await expect(client.deleteLeaf("p1")).resolves.toBeUndefined();
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/leafs/p1",
        expect.objectContaining({ method: "DELETE" }),
      );
    });
  });

  // --- State Transitions ---

  describe("state transitions", () => {
    it.each([
      ["activateLeaf", "activate", "ACTIVE"],
      ["pauseLeaf", "pause", "PAUSED"],
      ["resumeLeaf", "resume", "ACTIVE"],
      ["archiveLeaf", "archive", "ARCHIVED"],
      ["configureLeaf", "configure", "CONFIGURING"],
    ] as const)("%s sends POST to /%s", async (method, action, expectedState) => {
      mockResponse(200, { ...mockLeaf, state: expectedState });

      const result = await (client[method] as (id: string) => Promise<Leaf>)("p1");
      expect(result.state).toBe(expectedState);
      expect(mockFetch).toHaveBeenCalledWith(
        `http://localhost:8080/api/v1/leafs/p1/${action}`,
        expect.objectContaining({ method: "POST" }),
      );
    });
  });

  // --- Work Units ---

  describe("listWorkUnits", () => {
    it("returns paginated work unit summaries", async () => {
      const mockWuSummary: WorkUnitSummary = {
        id: "wu1",
        leaf_id: "p1",
        batch_id: null,
        state: "PENDING",
        priority: "NORMAL",
        assigned_to: null,
        attempts: 0,
        flagged_for_review: false,
        created_at: "2026-01-01T00:00:00Z",
        updated_at: "2026-01-01T00:00:00Z",
      };
      const response: PaginatedResponse<WorkUnitSummary> = {
        data: [mockWuSummary],
        pagination: { next_cursor: null, has_more: false },
      };
      mockResponse(200, response);

      const result = await client.listWorkUnits("p1");
      expect(result.data).toHaveLength(1);
      expect(result.data[0].state).toBe("PENDING");
    });

    it("serializes work unit query params", async () => {
      mockResponse(200, { data: [], pagination: { next_cursor: null, has_more: false } });

      await client.listWorkUnits("p1", {
        state: "COMPLETED",
        priority: "HIGH",
        limit: 25,
      });

      const calledUrl = mockFetch.mock.calls[0][0] as string;
      expect(calledUrl).toContain("state=COMPLETED");
      expect(calledUrl).toContain("priority=HIGH");
      expect(calledUrl).toContain("limit=25");
    });
  });

  describe("getWorkUnit", () => {
    it("returns full work unit", async () => {
      const mockWu: WorkUnit = {
        id: "wu1",
        leaf_id: "p1",
        batch_id: null,
        state: "COMPLETED",
        priority: "NORMAL",
        parameters: { x: 1, y: 2 },
        input_data_url: null,
        result_data_url: null,
        assigned_to: "v1",
        assigned_at: "2026-01-01T00:00:00Z",
        started_at: "2026-01-01T00:00:00Z",
        completed_at: "2026-01-01T00:01:00Z",
        deadline: null,
        attempts: 1,
        max_attempts: 3,
        error_message: null,
        flagged_for_review: false,
        credit_awarded: 10,
        created_at: "2026-01-01T00:00:00Z",
        updated_at: "2026-01-01T00:01:00Z",
      };
      mockResponse(200, mockWu);

      const result = await client.getWorkUnit("p1", "wu1");
      expect(result.id).toBe("wu1");
      expect(result.credit_awarded).toBe(10);
    });
  });

  describe("generateWorkUnits", () => {
    it("sends POST and returns generation result", async () => {
      const response: GenerateWorkUnitsResponse = {
        batch_id: "b1",
        work_units_created: 100,
        status: "complete",
      };
      mockResponse(202, response);

      const result = await client.generateWorkUnits("p1", { batch_size: 100 });
      expect(result.work_units_created).toBe(100);
      expect(result.status).toBe("complete");
    });
  });

  // --- Results ---

  describe("listResults", () => {
    it("returns paginated results", async () => {
      const mockResult: Result = {
        id: "r1",
        work_unit_id: "wu1",
        volunteer_id: "v1",
        output_data: { summary: { total_steps: 100 } },
        output_checksum: "abc123",
        execution_metadata: {
          wall_clock_seconds: 60,
          cpu_seconds_user: 50,
          cpu_seconds_system: 5,
          cpu_cores_used: 1,
          gpu_seconds: 0,
          gpu_vram_used_mb: 0,
          peak_memory_mb: 128,
          disk_read_mb: 0,
          disk_write_mb: 0,
          network_rx_mb: 0,
          network_tx_mb: 0,
        },
        validation_status: "AGREED",
        submitted_at: "2026-01-01T00:01:00Z",
        validated_at: "2026-01-01T00:02:00Z",
        created_at: "2026-01-01T00:01:00Z",
        updated_at: "2026-01-01T00:02:00Z",
      };
      const response: PaginatedResponse<Result> = {
        data: [mockResult],
        pagination: { next_cursor: null, has_more: false },
      };
      mockResponse(200, response);

      const result = await client.listResults("p1");
      expect(result.data).toHaveLength(1);
      expect(result.data[0].validation_status).toBe("AGREED");
    });

    it("serializes query parameters correctly", async () => {
      mockResponse(200, { data: [], pagination: { next_cursor: null, has_more: false } });

      await client.listResults("p1", {
        work_unit_id: "wu1",
        validation_status: "AGREED",
        limit: 1,
      });

      const calledUrl = mockFetch.mock.calls[0][0] as string;
      expect(calledUrl).toBe(
        "http://localhost:8080/api/v1/leafs/p1/results?work_unit_id=wu1&validation_status=AGREED&limit=1",
      );
    });

    it("omits undefined params", async () => {
      mockResponse(200, { data: [], pagination: { next_cursor: null, has_more: false } });

      await client.listResults("p1", {});

      const calledUrl = mockFetch.mock.calls[0][0] as string;
      expect(calledUrl).toBe("http://localhost:8080/api/v1/leafs/p1/results");
    });
  });

  // --- Statistics ---

  describe("getLeafStats", () => {
    it("returns stats snapshot", async () => {
      const stats: LeafStats = {
        id: "00000000-0000-0000-0000-000000000001",
        leaf_id: "p1",
        snapshot_at: "2026-01-01T00:00:00Z",
        total_work_units: 1000,
        work_units_queued: 500,
        work_units_assigned: 100,
        work_units_running: 200,
        work_units_completed: 150,
        work_units_validated: 150,
        work_units_failed: 30,
        active_volunteers: 50,
        total_credit_granted: 5000,
        avg_completion_seconds: 120,
        agreement_rate: 0.97,
        throughput_per_hour: 75,
        created_at: "2026-01-01T00:00:00Z",
      };
      mockResponse(200, stats);

      const result = await client.getLeafStats("p1");
      expect(result.total_work_units).toBe(1000);
      expect(result.active_volunteers).toBe(50);
    });
  });

  describe("getLeafStatsHistory", () => {
    it("sends from/to/interval params", async () => {
      mockResponse(200, { data: [] });

      await client.getLeafStatsHistory("p1", {
        from: "2026-01-01T00:00:00Z",
        to: "2026-01-02T00:00:00Z",
        interval: "daily",
      });

      const calledUrl = mockFetch.mock.calls[0][0] as string;
      expect(calledUrl).toContain("from=");
      expect(calledUrl).toContain("to=");
      expect(calledUrl).toContain("interval=daily");
    });
  });

  // --- Constructor ---

  describe("constructor", () => {
    it("strips trailing slashes from baseUrl", async () => {
      const clientWithSlash = new InfrastructureClient("http://localhost:8080/");
      const health: HealthResponse = {
        status: "ok",
        uptime_seconds: 120,
        database: "connected",
      };
      mockResponse(200, health);

      await clientWithSlash.getHealth();
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/health/detailed",
        expect.objectContaining({ method: "GET" }),
      );
    });

    it("strips multiple trailing slashes from baseUrl", async () => {
      const clientWithSlashes = new InfrastructureClient("http://localhost:8080///");
      const health: HealthResponse = {
        status: "ok",
        uptime_seconds: 120,
        database: "connected",
      };
      mockResponse(200, health);

      await clientWithSlashes.getHealth();
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/health/detailed",
        expect.objectContaining({ method: "GET" }),
      );
    });
  });

  // --- Request Headers ---

  describe("request headers", () => {
    it("sets Content-Type for POST requests with body", async () => {
      mockResponse(201, mockLeaf);

      await client.createLeaf({
        name: "Test",
        description: "A test",
        task_pattern: "PARAMETER_SWEEP",
      });

      const callArgs = mockFetch.mock.calls[0][1];
      expect(callArgs.headers["Content-Type"]).toBe("application/json");
    });

    it("does not set Content-Type for GET requests", async () => {
      mockResponse(200, mockLeaf);

      await client.getLeaf("p1");

      const callArgs = mockFetch.mock.calls[0][1];
      expect(callArgs.headers["Content-Type"]).toBeUndefined();
    });

    it("does not set Authorization header when no apiKey is provided", async () => {
      mockResponse(200, mockLeaf);

      await client.getLeaf("p1");

      const callArgs = mockFetch.mock.calls[0][1];
      expect(callArgs.headers["Authorization"]).toBeUndefined();
    });

    it("sets Bearer Authorization header when apiKey is provided", async () => {
      const authedClient = new InfrastructureClient(
        "http://localhost:8080",
        "lk_test_secret_key",
      );
      mockResponse(200, mockLeaf);

      await authedClient.getLeaf("p1");

      const callArgs = mockFetch.mock.calls[0][1];
      expect(callArgs.headers["Authorization"]).toBe(
        "Bearer lk_test_secret_key",
      );
    });

    it("sends both Authorization and Content-Type for POST with apiKey", async () => {
      const authedClient = new InfrastructureClient(
        "http://localhost:8080",
        "lk_my_api_key",
      );
      mockResponse(201, mockLeaf);

      await authedClient.createLeaf({
        name: "Test",
        description: "A test",
        task_pattern: "PARAMETER_SWEEP",
      });

      const callArgs = mockFetch.mock.calls[0][1];
      expect(callArgs.headers["Authorization"]).toBe("Bearer lk_my_api_key");
      expect(callArgs.headers["Content-Type"]).toBe("application/json");
    });

    it("does not set Authorization header when apiKey is undefined", async () => {
      const noKeyClient = new InfrastructureClient(
        "http://localhost:8080",
        undefined,
      );
      mockResponse(200, mockLeaf);

      await noKeyClient.getLeaf("p1");

      const callArgs = mockFetch.mock.calls[0][1];
      expect(callArgs.headers["Authorization"]).toBeUndefined();
    });
  });

  // --- Query String Edge Cases ---

  describe("query string edge cases", () => {
    it("omits null values from query string", async () => {
      const paginatedResponse = {
        data: [],
        pagination: { next_cursor: null, has_more: false },
      };
      mockResponse(200, paginatedResponse);

      // Pass an object with explicit null values
      await client.listLeafs({
        creator_id: "u1",
        state: undefined,
        search: undefined,
      });

      const calledUrl = mockFetch.mock.calls[0][0] as string;
      expect(calledUrl).toContain("creator_id=u1");
      expect(calledUrl).not.toContain("state=");
      expect(calledUrl).not.toContain("search=");
    });

    it("returns no query string when params object is empty", async () => {
      const paginatedResponse = {
        data: [],
        pagination: { next_cursor: null, has_more: false },
      };
      mockResponse(200, paginatedResponse);

      await client.listLeafs({});

      const calledUrl = mockFetch.mock.calls[0][0] as string;
      expect(calledUrl).toBe("http://localhost:8080/api/v1/leafs");
    });
  });

  // --- Error Handling ---

  describe("error handling", () => {
    it("parses 400 validation errors", async () => {
      mockResponse(400, {
        error: {
          code: "VALIDATION_ERROR",
          message: "Name is required",
          details: { field: "name" },
        },
      });

      try {
        await client.createLeaf({
          name: "",
          description: "test",
          task_pattern: "PARAMETER_SWEEP",
        });
        fail("Should have thrown");
      } catch (err) {
        expect(err).toBeInstanceOf(InfrastructureApiError);
        const apiErr = err as InfrastructureApiError;
        expect(apiErr.code).toBe("VALIDATION_ERROR");
        expect(apiErr.status).toBe(400);
        expect(apiErr.details).toEqual({ field: "name" });
      }
    });

    it("parses 500 server errors", async () => {
      mockResponse(500, {
        error: { code: "INTERNAL_ERROR", message: "Something went wrong" },
      });

      try {
        await client.getHealth();
        fail("Should have thrown");
      } catch (err) {
        expect(err).toBeInstanceOf(InfrastructureApiError);
        const apiErr = err as InfrastructureApiError;
        expect(apiErr.code).toBe("INTERNAL_ERROR");
        expect(apiErr.status).toBe(500);
      }
    });

    it("handles missing error body gracefully", async () => {
      mockResponse(502, {});

      try {
        await client.getHealth();
        fail("Should have thrown");
      } catch (err) {
        expect(err).toBeInstanceOf(InfrastructureApiError);
        const apiErr = err as InfrastructureApiError;
        expect(apiErr.code).toBe("UNKNOWN_ERROR");
        expect(apiErr.status).toBe(502);
      }
    });

    it("parses 409 conflict errors", async () => {
      mockResponse(409, {
        error: {
          code: "STATE_CONFLICT",
          message: "Cannot delete active leaf",
        },
      });

      try {
        await client.deleteLeaf("p1");
        fail("Should have thrown");
      } catch (err) {
        expect(err).toBeInstanceOf(InfrastructureApiError);
        const apiErr = err as InfrastructureApiError;
        expect(apiErr.code).toBe("STATE_CONFLICT");
        expect(apiErr.status).toBe(409);
      }
    });

    it("propagates network-level fetch errors (not InfrastructureApiError)", async () => {
      mockFetch.mockRejectedValueOnce(new TypeError("fetch failed"));

      try {
        await client.getHealth();
        fail("Should have thrown");
      } catch (err) {
        // Network errors are NOT InfrastructureApiError — they're raw errors
        expect(err).not.toBeInstanceOf(InfrastructureApiError);
        expect(err).toBeInstanceOf(TypeError);
        expect((err as TypeError).message).toBe("fetch failed");
      }
    });

    it("propagates DNS resolution errors", async () => {
      mockFetch.mockRejectedValueOnce(
        new Error("getaddrinfo ENOTFOUND bad-host"),
      );

      await expect(client.createLeaf({
        name: "Test",
        description: "desc",
        task_pattern: "PARAMETER_SWEEP",
      })).rejects.toThrow("getaddrinfo ENOTFOUND bad-host");
    });
  });

  // --- Aggregation ---

  describe("triggerAggregation", () => {
    it("sends POST to /aggregate with options", async () => {
      const mockAggResult: AggregationResult = {
        status: "complete",
        format: "json",
        result: { summary: { mean: 42 } },
        work_units_aggregated: 100,
        work_units_total: 100,
        aggregated_at: "2026-01-01T00:00:00Z",
      };
      mockResponse(200, { data: mockAggResult });

      const result = await client.triggerAggregation("p1", {
        batchId: "b1",
        format: "csv",
        force: true,
      });

      expect(result.data).toEqual(mockAggResult);
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/leafs/p1/aggregate",
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({ batchId: "b1", format: "csv", force: true }),
        }),
      );
    });
  });

  describe("getAggregation", () => {
    it("returns aggregation data on success", async () => {
      const mockAggResult: AggregationResult = {
        status: "complete",
        format: "json",
        result: { summary: { mean: 42 } },
        work_units_aggregated: 50,
        work_units_total: 100,
        aggregated_at: "2026-01-01T00:00:00Z",
      };
      mockResponse(200, { data: mockAggResult });

      const result = await client.getAggregation("p1");
      expect(result).not.toBeNull();
      expect(result!.data).toEqual(mockAggResult);
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/leafs/p1/aggregate",
        expect.objectContaining({ method: "GET" }),
      );
    });

    it("returns null on 404 instead of throwing", async () => {
      mockResponse(404, {
        error: { code: "NOT_FOUND", message: "No aggregation found" },
      });

      const result = await client.getAggregation("p1");
      expect(result).toBeNull();
    });
  });

  // --- Batch Stats ---

  describe("getLeafStatsBatch", () => {
    it("returns batch stats with comma-separated IDs", async () => {
      const statsP1: LeafStats = {
        id: "00000000-0000-0000-0000-000000000001",
        leaf_id: "p1",
        snapshot_at: "2026-01-01T00:00:00Z",
        total_work_units: 100,
        work_units_queued: 50,
        work_units_assigned: 10,
        work_units_running: 20,
        work_units_completed: 15,
        work_units_validated: 15,
        work_units_failed: 5,
        active_volunteers: 10,
        total_credit_granted: 500,
        avg_completion_seconds: 60,
        agreement_rate: 0.95,
        throughput_per_hour: 30,
        created_at: "2026-01-01T00:00:00Z",
      };
      const statsP2: LeafStats = {
        id: "00000000-0000-0000-0000-000000000002",
        leaf_id: "p2",
        snapshot_at: "2026-01-01T00:00:00Z",
        total_work_units: 200,
        work_units_queued: 100,
        work_units_assigned: 20,
        work_units_running: 40,
        work_units_completed: 30,
        work_units_validated: 30,
        work_units_failed: 10,
        active_volunteers: 20,
        total_credit_granted: 1000,
        avg_completion_seconds: 90,
        agreement_rate: 0.98,
        throughput_per_hour: 50,
        created_at: "2026-01-01T00:00:00Z",
      };
      mockResponse(200, { data: { p1: statsP1, p2: statsP2 } });

      const result = await client.getLeafStatsBatch(["p1", "p2"]);

      expect(result.p1.total_work_units).toBe(100);
      expect(result.p2.total_work_units).toBe(200);

      const calledUrl = mockFetch.mock.calls[0][0] as string;
      expect(calledUrl).toBe(
        "http://localhost:8080/api/v1/leafs/stats/batch?ids=p1,p2",
      );
      expect(mockFetch).toHaveBeenCalledWith(
        calledUrl,
        expect.objectContaining({ method: "GET" }),
      );
    });
  });
});
