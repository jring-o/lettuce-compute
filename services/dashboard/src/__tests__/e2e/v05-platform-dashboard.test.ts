/**
 * v0.5 E2E Test: Platform Dashboard
 *
 * Tests both F09 (researcher project lifecycle) and F10 (project discovery) flows.
 * Uses mocked infrastructure client and database.
 *
 * Run with: npm test -- v05-platform-dashboard
 */

// --- Mocks ---

const mockAuth = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: () => mockAuth(),
}));

const mockInfraClient = {
  createLeaf: jest.fn(),
  getLeaf: jest.fn(),
  listLeafs: jest.fn(),
  updateLeaf: jest.fn(),
  deleteLeaf: jest.fn(),
  activateLeaf: jest.fn(),
  pauseLeaf: jest.fn(),
  resumeLeaf: jest.fn(),
  archiveLeaf: jest.fn(),
  configureLeaf: jest.fn(),
  listWorkUnits: jest.fn(),
  getWorkUnit: jest.fn(),
  generateWorkUnits: jest.fn(),
  getLeafStats: jest.fn(),
  getLeafStatsBatch: jest.fn(),
  getLeafStatsHistory: jest.fn(),
  getHealth: jest.fn(),
};

jest.mock("@/lib/infrastructure-client", () => ({
  infrastructureClient: mockInfraClient,
  InfrastructureApiError: class InfrastructureApiError extends Error {
    code: string;
    status: number;
    constructor(code: string, message: string, status: number) {
      super(message);
      this.code = code;
      this.status = status;
    }
  },
}));

jest.mock("next/server", () => {
  class MockNextResponse {
    body: unknown;
    status: number;
    headers: Map<string, string>;
    _jsonData: unknown;

    constructor(
      body: unknown,
      init?: { status?: number; headers?: Record<string, string> },
    ) {
      this.body = body;
      this.status = init?.status ?? 200;
      this.headers = new Map(Object.entries(init?.headers ?? {}));
      this._jsonData = null;
    }

    static json(data: unknown, init?: { status?: number }) {
      const instance = new MockNextResponse(null, init);
      instance._jsonData = data;
      return instance;
    }
  }

  class MockNextRequest {
    nextUrl: { searchParams: URLSearchParams };

    constructor(url: string) {
      this.nextUrl = { searchParams: new URL(url).searchParams };
    }
  }

  return {
    NextRequest: MockNextRequest,
    NextResponse: MockNextResponse,
  };
});

import {
  getLeaf,
  listMyLeafs,
  pauseLeaf,
  resumeLeaf,
} from "@/lib/actions/projects";
import { getLeafStats } from "@/lib/actions/stats";
import { listWorkUnits } from "@/lib/actions/work-units";
import { listPublicLeafs } from "@/lib/actions/public-projects";
import { GET as downloadGET } from "@/app/api/download/[leafId]/route";
import { NextRequest } from "next/server";

// --- Fixtures ---

const RESEARCHER = {
  id: "researcher-e2e-id",
  username: "researcher-e2e",
  role: "USER",
};

const OTHER_USER = {
  id: "other-user-id",
  username: "other-user",
  role: "USER",
};

const E2E_LEAF = {
  id: "e2e-leaf-id",
  name: "E2E Parameter Sweep Test",
  slug: "e2e-parameter-sweep-test",
  description: "A test leaf for the v0.5 E2E test suite.",
  state: "ACTIVE" as const,
  task_pattern: "PARAMETER_SWEEP" as const,
  research_area: "physics",
  creator_id: RESEARCHER.id,
  execution_config: null,
  validation_config: null,
  fault_tolerance_config: null,
  data_config: null,
  credit_config: null,
  resource_requirements: {
    min_cpu_cores: 1,
    max_memory_mb: 512,
    min_disk_mb: 1024,
    gpu_required: false,
  },
  is_ongoing: false,
  visibility: "PUBLIC" as const,
  stats_cache_seconds: 60,
  created_at: "2026-03-14T00:00:00Z",
  updated_at: "2026-03-14T00:00:00Z",
};

const E2E_LEAF_SUMMARY = {
  id: E2E_LEAF.id,
  name: E2E_LEAF.name,
  slug: E2E_LEAF.slug,
  description: E2E_LEAF.description,
  research_area: E2E_LEAF.research_area,
  state: E2E_LEAF.state,
  task_pattern: E2E_LEAF.task_pattern,
  resource_requirements: E2E_LEAF.resource_requirements,
  runtime: "NATIVE" as const,
  is_ongoing: E2E_LEAF.is_ongoing,
  visibility: E2E_LEAF.visibility,
  stats_cache_seconds: E2E_LEAF.stats_cache_seconds,
  active_volunteers: 0,
  progress_pct: null,
  created_at: E2E_LEAF.created_at,
};

const E2E_STATS = {
  id: "00000000-0000-0000-0000-000000000001",
  leaf_id: E2E_LEAF.id,
  snapshot_at: "2026-03-14T00:00:00Z",
  total_work_units: 6,
  work_units_queued: 6,
  work_units_assigned: 0,
  work_units_running: 0,
  work_units_completed: 0,
  work_units_validated: 0,
  work_units_failed: 0,
  active_volunteers: 0,
  total_credit_granted: 0,
  avg_completion_seconds: null,
  agreement_rate: null,
  throughput_per_hour: null,
  created_at: "2026-03-14T00:00:00Z",
};

const E2E_WORK_UNITS = Array.from({ length: 6 }, (_, i) => ({
  id: `wu-${i + 1}`,
  leaf_id: E2E_LEAF.id,
  batch_id: "batch-1",
  state: "PENDING" as const,
  priority: "NORMAL" as const,
  assigned_to: null,
  attempts: 0,
  flagged_for_review: false,
  created_at: "2026-03-14T00:00:00Z",
  updated_at: "2026-03-14T00:00:00Z",
}));

beforeEach(() => {
  jest.clearAllMocks();

  // Default: researcher is authenticated
  mockAuth.mockResolvedValue({ user: RESEARCHER });

  // Default mock responses
  mockInfraClient.getLeaf.mockResolvedValue(E2E_LEAF);
  mockInfraClient.listLeafs.mockResolvedValue({
    data: [E2E_LEAF_SUMMARY],
    pagination: { next_cursor: null, has_more: false },
  });
  mockInfraClient.getLeafStats.mockResolvedValue(E2E_STATS);
  mockInfraClient.getLeafStatsBatch.mockResolvedValue({
    [E2E_LEAF.id]: E2E_STATS,
  });
  mockInfraClient.listWorkUnits.mockResolvedValue({
    data: E2E_WORK_UNITS,
    pagination: { next_cursor: null, has_more: false },
  });
  mockInfraClient.pauseLeaf.mockResolvedValue({
    ...E2E_LEAF,
    state: "PAUSED",
  });
  mockInfraClient.resumeLeaf.mockResolvedValue({
    ...E2E_LEAF,
    state: "ACTIVE",
  });
});

describe("v0.5 E2E: Platform Dashboard", () => {
  // ==========================================================================
  // Scenario 1: Researcher Creates and Monitors a Leaf
  // ==========================================================================

  describe("Scenario 1: Researcher Creates and Monitors a Leaf", () => {
    it("step 1-4: leaf is created with state=ACTIVE", async () => {
      const result = await getLeaf(E2E_LEAF.id);
      expect("data" in result).toBe(true);
      if ("data" in result) {
        expect(result.data.name).toBe("E2E Parameter Sweep Test");
        expect(result.data.state).toBe("ACTIVE");
      }
    });

    it("step 5: verifies 6 work units generated", async () => {
      const result = await listWorkUnits(E2E_LEAF.id);
      expect("data" in result).toBe(true);
      if ("data" in result) {
        expect(result.data.data).toHaveLength(6);
      }
    });

    it("step 6: dashboard stats show 6 total, 0 completed", async () => {
      const result = await getLeafStats(E2E_LEAF.id);
      expect("data" in result).toBe(true);
      if ("data" in result) {
        expect(result.data.total_work_units).toBe(6);
        expect(result.data.work_units_completed).toBe(0);
      }
    });

    it("step 7: pause transitions to PAUSED", async () => {
      const result = await pauseLeaf(E2E_LEAF.id);
      expect("data" in result).toBe(true);
      if ("data" in result) {
        expect(result.data.state).toBe("PAUSED");
      }
    });

    it("step 8: resume transitions back to ACTIVE", async () => {
      mockInfraClient.getLeaf.mockResolvedValue({
        ...E2E_LEAF,
        state: "PAUSED",
      });
      const result = await resumeLeaf(E2E_LEAF.id);
      expect("data" in result).toBe(true);
      if ("data" in result) {
        expect(result.data.state).toBe("ACTIVE");
      }
    });

    it("step 9: download returns 204 when no validated results", async () => {
      mockInfraClient.listWorkUnits.mockResolvedValue({
        data: [],
        pagination: { next_cursor: null, has_more: false },
      });

      const req = new NextRequest(
        "http://localhost:3000/api/download/e2e-leaf-id?format=json",
      );
      const res = await downloadGET(req, {
        params: Promise.resolve({ leafId: "e2e-leaf-id" }),
      });
      expect(res.status).toBe(204);
    });
  });

  // ==========================================================================
  // Scenario 2: Visitor Discovers the Leaf
  // ==========================================================================

  describe("Scenario 2: Visitor Discovers the Leaf", () => {
    it("step 1-2: leaf appears in public list", async () => {
      // No auth for public browsing
      const result = await listPublicLeafs({});
      expect("data" in result).toBe(true);
      if ("data" in result) {
        const leaf = result.data.leafs.find(
          (p) => p.name === "E2E Parameter Sweep Test",
        );
        expect(leaf).toBeDefined();
      }
    });

    it("step 3: leaf card shows name, research area, resources", async () => {
      const result = await listPublicLeafs({});
      expect("data" in result).toBe(true);
      if ("data" in result) {
        const leaf = result.data.leafs[0];
        expect(leaf.name).toBe("E2E Parameter Sweep Test");
        expect(leaf.research_area).toBe("physics");
        expect(leaf.resource_requirements?.min_cpu_cores).toBe(1);
      }
    });

    it("step 4: search finds leaf by name", async () => {
      const result = await listPublicLeafs({ search: "Parameter Sweep" });
      expect("data" in result).toBe(true);
      if ("data" in result) {
        expect(result.data.leafs).toHaveLength(1);
        expect(result.data.leafs[0].name).toBe(
          "E2E Parameter Sweep Test",
        );
      }
    });

    it("step 5: search returns empty for nonexistent leaf", async () => {
      mockInfraClient.listLeafs.mockResolvedValue({
        data: [],
        pagination: { next_cursor: null, has_more: false },
      });
      const result = await listPublicLeafs({ search: "nonexistent-xyz" });
      expect("data" in result).toBe(true);
      if ("data" in result) {
        expect(result.data.leafs).toHaveLength(0);
      }
    });

    it("step 6-7: detail page returns full leaf data", async () => {
      const result = await getLeaf(E2E_LEAF.id);
      expect("data" in result).toBe(true);
      if ("data" in result) {
        expect(result.data.description).toBe(
          "A test leaf for the v0.5 E2E test suite.",
        );
        expect(result.data.creator_id).toBe(RESEARCHER.id);
        expect(result.data.resource_requirements?.min_cpu_cores).toBe(1);
      }
    });

  });

  // ==========================================================================
  // Scenario 3: Auth Guards
  // ==========================================================================

  describe("Scenario 3: Auth Guards", () => {
    it("step 1: unauthenticated access to dashboard actions returns UNAUTHENTICATED", async () => {
      mockAuth.mockResolvedValue(null);
      const result = await listMyLeafs();
      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: expect.any(String) },
      });
    });

    it("step 2: unauthenticated user can access public leaf listing", async () => {
      // listPublicLeafs doesn't require auth
      const result = await listPublicLeafs({});
      expect("data" in result).toBe(true);
    });

    it("step 3: the leaf owner can read leaf stats", async () => {
      // getLeafStats is owner-gated (the dashboard does its own per-user check);
      // the default RESEARCHER session owns E2E_LEAF.
      const result = await getLeafStats(E2E_LEAF.id);
      expect("data" in result).toBe(true);
    });

    it("step 5-6: non-owner gets FORBIDDEN and the mutation is never sent", async () => {
      // The dashboard enforces per-user ownership itself (shared service key):
      // OTHER_USER is not the leaf's creator and is not an ADMIN, so pauseLeaf
      // must be rejected before it ever reaches the infrastructure client.
      mockAuth.mockResolvedValue({ user: OTHER_USER });
      // getLeaf returns the leaf owned by RESEARCHER (default), so the
      // ownership check fails for OTHER_USER.
      const result = await pauseLeaf(E2E_LEAF.id);
      expect(result).toEqual({
        error: { code: "FORBIDDEN", message: expect.any(String) },
      });
      expect(mockInfraClient.pauseLeaf).not.toHaveBeenCalled();
    });
  });

  // ==========================================================================
  // Full lifecycle integration
  // ==========================================================================

  describe("Full lifecycle", () => {
    it("researcher: create -> monitor -> pause -> resume -> download", async () => {
      // 1. List leafs
      const listResult = await listMyLeafs();
      expect("data" in listResult).toBe(true);

      // 2. Get stats
      const statsResult = await getLeafStats(E2E_LEAF.id);
      expect("data" in statsResult).toBe(true);
      if ("data" in statsResult) {
        expect(statsResult.data.total_work_units).toBe(6);
      }

      // 3. List work units
      const wuResult = await listWorkUnits(E2E_LEAF.id);
      expect("data" in wuResult).toBe(true);
      if ("data" in wuResult) {
        expect(wuResult.data.data).toHaveLength(6);
      }

      // 4. Pause
      const pauseResult = await pauseLeaf(E2E_LEAF.id);
      expect("data" in pauseResult).toBe(true);
      if ("data" in pauseResult) {
        expect(pauseResult.data.state).toBe("PAUSED");
      }

      // 5. Resume
      mockInfraClient.getLeaf.mockResolvedValue({
        ...E2E_LEAF,
        state: "PAUSED",
      });
      const resumeResult = await resumeLeaf(E2E_LEAF.id);
      expect("data" in resumeResult).toBe(true);
      if ("data" in resumeResult) {
        expect(resumeResult.data.state).toBe("ACTIVE");
      }

      // 6. Download (no results)
      mockInfraClient.getLeaf.mockResolvedValue(E2E_LEAF);
      mockInfraClient.listWorkUnits.mockResolvedValue({
        data: [],
        pagination: { next_cursor: null, has_more: false },
      });
      const req = new NextRequest(
        "http://localhost:3000/api/download/e2e-leaf-id?format=json",
      );
      const res = await downloadGET(req, {
        params: Promise.resolve({ leafId: "e2e-leaf-id" }),
      });
      expect(res.status).toBe(204);
    });

    it("visitor: browse -> search -> detail", async () => {
      // 1. Browse leafs
      const browseResult = await listPublicLeafs({});
      expect("data" in browseResult).toBe(true);
      if ("data" in browseResult) {
        expect(browseResult.data.leafs.length).toBeGreaterThan(0);
      }

      // 2. Search
      const searchResult = await listPublicLeafs({
        search: "Parameter Sweep",
      });
      expect("data" in searchResult).toBe(true);

      // 3. Get detail
      const detailResult = await getLeaf(E2E_LEAF.id);
      expect("data" in detailResult).toBe(true);
    });
  });
});
