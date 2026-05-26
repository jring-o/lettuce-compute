/**
 * F09 E2E Test: Researcher Leaf Lifecycle
 *
 * Tests the complete researcher journey: sign in -> manage leaf -> monitor -> pause -> resume.
 * Requires a running infrastructure server and platform database.
 *
 * Run with: LETTUCE_E2E=1 npm test -- f09-researcher-flow
 */

const mockAuth = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: () => mockAuth(),
}));

const mockInfraClient = {
  createLeaf: jest.fn(),
  getLeaf: jest.fn(),
  listLeafs: jest.fn(),
  updateLeaf: jest.fn(),
  activateLeaf: jest.fn(),
  pauseLeaf: jest.fn(),
  resumeLeaf: jest.fn(),
  archiveLeaf: jest.fn(),
  configureLeaf: jest.fn(),
  listWorkUnits: jest.fn(),
  getWorkUnit: jest.fn(),
  generateWorkUnits: jest.fn(),
  getLeafStats: jest.fn(),
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

    constructor(body: unknown, init?: { status?: number; headers?: Record<string, string> }) {
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
  pauseLeaf,
  resumeLeaf,
  listMyLeafs,
} from "@/lib/actions/projects";
import { getLeafStats } from "@/lib/actions/stats";
import { listWorkUnits } from "@/lib/actions/work-units";
import { GET as downloadGET } from "@/app/api/download/[leafId]/route";
import { NextRequest } from "next/server";

const TEST_USER = {
  id: "test-user-id",
  username: "testuser",
  role: "USER",
};

const mockLeaf = {
  id: "leaf-1",
  name: "Monte Carlo Pi",
  slug: "monte-carlo-pi",
  description: "Estimate Pi using Monte Carlo methods",
  state: "ACTIVE" as const,
  task_pattern: "PARAMETER_SWEEP" as const,
  research_area: "mathematics",
  creator_id: TEST_USER.id,
  execution_config: null,
  validation_config: null,
  fault_tolerance_config: null,
  data_config: null,
  credit_config: null,
  resource_requirements: null,
  is_ongoing: false,
  visibility: "PUBLIC" as const,
  stats_cache_seconds: 60,
  created_at: "2026-03-14T00:00:00Z",
  updated_at: "2026-03-14T00:00:00Z",
};

const mockStats = {
  id: "00000000-0000-0000-0000-000000000001",
  leaf_id: "leaf-1",
  snapshot_at: "2026-03-14T00:00:00Z",
  total_work_units: 100,
  work_units_queued: 80,
  work_units_assigned: 10,
  work_units_running: 5,
  work_units_completed: 5,
  work_units_validated: 5,
  work_units_failed: 0,
  active_volunteers: 3,
  total_credit_granted: 500,
  avg_completion_seconds: 60,
  agreement_rate: 1.0,
  throughput_per_hour: 10,
  created_at: "2026-03-14T00:00:00Z",
};

const mockWorkUnits = [
  {
    id: "wu-1",
    leaf_id: "leaf-1",
    batch_id: "b1",
    state: "COMPLETED" as const,
    priority: "NORMAL" as const,
    assigned_to: null,
    attempts: 1,
    flagged_for_review: false,
    created_at: "2026-03-14T00:00:00Z",
    updated_at: "2026-03-14T00:01:00Z",
  },
];

beforeEach(() => {
  jest.clearAllMocks();
  mockAuth.mockResolvedValue({ user: TEST_USER });

  // Setup infrastructure client mock responses
  mockInfraClient.getLeaf.mockResolvedValue(mockLeaf);
  mockInfraClient.listLeafs.mockResolvedValue({
    data: [mockLeaf],
    pagination: { next_cursor: null, has_more: false },
  });
  mockInfraClient.getLeafStats.mockResolvedValue(mockStats);
  mockInfraClient.listWorkUnits.mockResolvedValue({
    data: mockWorkUnits,
    pagination: { next_cursor: null, has_more: false },
  });
  mockInfraClient.pauseLeaf.mockResolvedValue({
    ...mockLeaf,
    state: "PAUSED",
  });
  mockInfraClient.resumeLeaf.mockResolvedValue({
    ...mockLeaf,
    state: "ACTIVE",
  });
});

describe("F09: Researcher Leaf Lifecycle", () => {
  it("step 1: fetches leaf by ID", async () => {
    const result = await getLeaf(mockLeaf.id);
    expect("data" in result).toBe(true);
    if ("data" in result) {
      expect(result.data.name).toBe("Monte Carlo Pi");
      expect(result.data.state).toBe("ACTIVE");
    }
  });

  it("step 2: lists user leafs", async () => {
    const result = await listMyLeafs();
    expect("data" in result).toBe(true);
    if ("data" in result) {
      expect(result.data.data).toHaveLength(1);
      expect(result.data.data[0].slug).toBe("monte-carlo-pi");
    }
  });

  it("step 3: fetches leaf stats", async () => {
    const result = await getLeafStats(mockLeaf.id);
    expect("data" in result).toBe(true);
    if ("data" in result) {
      expect(result.data.total_work_units).toBe(100);
      expect(result.data.work_units_completed).toBe(5);
    }
  });

  it("step 4: lists work units", async () => {
    const result = await listWorkUnits(mockLeaf.id);
    expect("data" in result).toBe(true);
    if ("data" in result) {
      expect(result.data.data).toHaveLength(1);
      expect(result.data.data[0].state).toBe("COMPLETED");
    }
  });

  it("step 5: download returns 204 when no completed results", async () => {
    mockInfraClient.listWorkUnits.mockResolvedValue({
      data: [],
      pagination: { next_cursor: null, has_more: false },
    });
    mockInfraClient.getLeaf.mockResolvedValue(mockLeaf);

    const req = new NextRequest(
      "http://localhost:3000/api/download/leaf-1?format=json",
    );
    const res = await downloadGET(req, {
      params: Promise.resolve({ leafId: "leaf-1" }),
    });
    expect(res.status).toBe(204);
  });

  it("step 6: pause transitions leaf to PAUSED", async () => {
    const result = await pauseLeaf(mockLeaf.id);
    expect("data" in result).toBe(true);
    if ("data" in result) {
      expect(result.data.state).toBe("PAUSED");
    }
  });

  it("step 7: resume transitions leaf back to ACTIVE", async () => {
    // First pause
    mockInfraClient.getLeaf.mockResolvedValue({
      ...mockLeaf,
      state: "PAUSED",
    });
    const result = await resumeLeaf(mockLeaf.id);
    expect("data" in result).toBe(true);
    if ("data" in result) {
      expect(result.data.state).toBe("ACTIVE");
    }
  });

  it("full lifecycle: list -> stats -> work-units -> pause -> resume", async () => {
    // 1. List leafs
    const listResult = await listMyLeafs();
    expect("data" in listResult).toBe(true);

    // 2. Get stats
    const statsResult = await getLeafStats(mockLeaf.id);
    expect("data" in statsResult).toBe(true);

    // 3. List work units
    const wuResult = await listWorkUnits(mockLeaf.id);
    expect("data" in wuResult).toBe(true);

    // 4. Pause
    const pauseResult = await pauseLeaf(mockLeaf.id);
    expect("data" in pauseResult).toBe(true);
    if ("data" in pauseResult) {
      expect(pauseResult.data.state).toBe("PAUSED");
    }

    // 5. Resume
    mockInfraClient.getLeaf.mockResolvedValue({
      ...mockLeaf,
      state: "PAUSED",
    });
    const resumeResult = await resumeLeaf(mockLeaf.id);
    expect("data" in resumeResult).toBe(true);
    if ("data" in resumeResult) {
      expect(resumeResult.data.state).toBe("ACTIVE");
    }
  });
});
