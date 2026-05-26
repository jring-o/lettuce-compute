import type { LeafStats, LeafSummary, Pagination } from "@/types/infrastructure";

// --- Mocks ---

jest.mock("@/lib/auth", () => ({
  auth: jest.fn(),
}));

const mockClient = {
  listLeafs: jest.fn(),
  getLeafStats: jest.fn(),
  getLeafStatsBatch: jest.fn(),
  getLeaf: jest.fn(),
};

jest.mock("@/lib/infrastructure-client", () => ({
  infrastructureClient: mockClient,
  InfrastructureApiError: class extends Error {
    code: string;
    status: number;
    details?: Record<string, unknown>;
    constructor(
      code: string,
      message: string,
      status: number,
      details?: Record<string, unknown>,
    ) {
      super(message);
      this.code = code;
      this.status = status;
      this.details = details;
    }
  },
}));

import { listPublicLeafs } from "@/lib/actions/public-projects";

// --- Fixtures ---

const baseLeaf: LeafSummary = {
  id: "p1",
  name: "Test Leaf",
  slug: "test-leaf",
  description: "A test leaf",
  research_area: "physics",
  state: "ACTIVE",
  task_pattern: "PARAMETER_SWEEP",
  resource_requirements: null,
  runtime: "NATIVE",
  is_ongoing: false,
  visibility: "PUBLIC",
  stats_cache_seconds: 60,
  active_volunteers: 0,
  progress_pct: null,
  created_at: "2026-01-01T00:00:00Z",
};

const baseStats: LeafStats = {
  id: "00000000-0000-0000-0000-000000000001",
  leaf_id: "p1",
  snapshot_at: "2026-03-14T00:00:00Z",
  total_work_units: 100,
  work_units_queued: 20,
  work_units_assigned: 10,
  work_units_running: 10,
  work_units_completed: 60,
  work_units_validated: 60,
  work_units_failed: 0,
  active_volunteers: 5,
  total_credit_granted: 500,
  avg_completion_seconds: 60,
  agreement_rate: 1.0,
  throughput_per_hour: 10,
  created_at: "2026-03-14T00:00:00Z",
};

const defaultPagination: Pagination = {
  next_cursor: null,
  has_more: false,
};

beforeEach(() => {
  jest.clearAllMocks();
});

describe("listPublicLeafs", () => {
  it("returns leafs with stats via batch endpoint", async () => {
    mockClient.listLeafs.mockResolvedValue({
      data: [baseLeaf],
      pagination: defaultPagination,
    });
    mockClient.getLeafStatsBatch.mockResolvedValue({
      p1: baseStats,
    });

    const result = await listPublicLeafs({});

    expect(result).toEqual({
      data: {
        leafs: [{ ...baseLeaf, stats: baseStats }],
        pagination: defaultPagination,
      },
    });
    expect(mockClient.getLeafStatsBatch).toHaveBeenCalledWith(["p1"]);
  });

  it("uses default sort=updated_at and limit=12", async () => {
    mockClient.listLeafs.mockResolvedValue({
      data: [],
      pagination: defaultPagination,
    });

    await listPublicLeafs({});

    expect(mockClient.listLeafs).toHaveBeenCalledWith({
      state: "ACTIVE",
      sort: "updated_at",
      order: "desc",
      limit: 12,
    });
  });

  it("overrides sort and limit when provided", async () => {
    mockClient.listLeafs.mockResolvedValue({
      data: [],
      pagination: defaultPagination,
    });

    await listPublicLeafs({ sort: "created_at", limit: 6 });

    expect(mockClient.listLeafs).toHaveBeenCalledWith(
      expect.objectContaining({
        sort: "created_at",
        limit: 6,
      }),
    );
  });

  it("passes search param when provided", async () => {
    mockClient.listLeafs.mockResolvedValue({
      data: [],
      pagination: defaultPagination,
    });

    await listPublicLeafs({ search: "climate" });

    expect(mockClient.listLeafs).toHaveBeenCalledWith(
      expect.objectContaining({ search: "climate" }),
    );
  });

  it("passes research_area param when provided", async () => {
    mockClient.listLeafs.mockResolvedValue({
      data: [],
      pagination: defaultPagination,
    });

    await listPublicLeafs({ research_area: "genomics" });

    expect(mockClient.listLeafs).toHaveBeenCalledWith(
      expect.objectContaining({ research_area: "genomics" }),
    );
  });

  it("passes cursor param when provided", async () => {
    mockClient.listLeafs.mockResolvedValue({
      data: [],
      pagination: defaultPagination,
    });

    await listPublicLeafs({ cursor: "cursor-abc" });

    expect(mockClient.listLeafs).toHaveBeenCalledWith(
      expect.objectContaining({ cursor: "cursor-abc" }),
    );
  });

  it("does not include optional params when not provided", async () => {
    mockClient.listLeafs.mockResolvedValue({
      data: [],
      pagination: defaultPagination,
    });

    await listPublicLeafs({});

    const callArgs = mockClient.listLeafs.mock.calls[0][0];
    expect(callArgs).not.toHaveProperty("search");
    expect(callArgs).not.toHaveProperty("research_area");
    expect(callArgs).not.toHaveProperty("cursor");
  });

  it("fetches stats for multiple leafs in a single batch call", async () => {
    const leaf2: LeafSummary = {
      ...baseLeaf,
      id: "p2",
      slug: "leaf-2",
      runtime: "CONTAINER",
    };
    const stats2: LeafStats = { ...baseStats, leaf_id: "p2", active_volunteers: 8 };

    mockClient.listLeafs.mockResolvedValue({
      data: [baseLeaf, leaf2],
      pagination: defaultPagination,
    });
    mockClient.getLeafStatsBatch.mockResolvedValue({
      p1: baseStats,
      p2: stats2,
    });

    const result = await listPublicLeafs({});

    expect(mockClient.getLeafStatsBatch).toHaveBeenCalledTimes(1);
    expect(mockClient.getLeafStatsBatch).toHaveBeenCalledWith(["p1", "p2"]);
    expect("data" in result && result.data.leafs[0].stats).toEqual(baseStats);
    expect("data" in result && result.data.leafs[1].stats).toEqual(stats2);
  });

  it("sets stats to null when batch stats fails entirely", async () => {
    mockClient.listLeafs.mockResolvedValue({
      data: [baseLeaf],
      pagination: defaultPagination,
    });
    mockClient.getLeafStatsBatch.mockRejectedValue(new Error("Stats unavailable"));

    const result = await listPublicLeafs({});

    expect("data" in result).toBe(true);
    if ("data" in result) {
      expect(result.data.leafs[0].stats).toBeNull();
    }
  });

  it("sets stats to null for leafs missing from batch response", async () => {
    const leaf2: LeafSummary = { ...baseLeaf, id: "p2", slug: "leaf-2" };

    mockClient.listLeafs.mockResolvedValue({
      data: [baseLeaf, leaf2],
      pagination: defaultPagination,
    });
    // Only p1 has stats in the response
    mockClient.getLeafStatsBatch.mockResolvedValue({
      p1: baseStats,
    });

    const result = await listPublicLeafs({});

    expect("data" in result).toBe(true);
    if ("data" in result) {
      expect(result.data.leafs[0].stats).toEqual(baseStats);
      expect(result.data.leafs[1].stats).toBeNull();
    }
  });

  it("maps InfrastructureApiError when listLeafs fails", async () => {
    const { InfrastructureApiError } = jest.requireMock("@/lib/infrastructure-client");
    mockClient.listLeafs.mockRejectedValue(
      new InfrastructureApiError("SERVICE_UNAVAILABLE", "Service down", 503),
    );

    const result = await listPublicLeafs({});

    expect(result).toEqual({
      error: { code: "SERVICE_UNAVAILABLE", message: "Service down" },
    });
  });

  it("maps generic error when listLeafs fails", async () => {
    mockClient.listLeafs.mockRejectedValue(new Error("Network failure"));

    const result = await listPublicLeafs({});

    expect(result).toEqual({
      error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
    });
  });

  it("returns pagination data from infrastructure", async () => {
    const paginationWithMore: Pagination = {
      next_cursor: "cursor-next",
      has_more: true,
    };
    mockClient.listLeafs.mockResolvedValue({
      data: [baseLeaf],
      pagination: paginationWithMore,
    });
    mockClient.getLeafStatsBatch.mockResolvedValue({
      p1: baseStats,
    });

    const result = await listPublicLeafs({});

    expect("data" in result).toBe(true);
    if ("data" in result) {
      expect(result.data.pagination).toEqual(paginationWithMore);
    }
  });

  it("returns empty leafs array when no leafs exist", async () => {
    mockClient.listLeafs.mockResolvedValue({
      data: [],
      pagination: defaultPagination,
    });

    const result = await listPublicLeafs({});

    expect("data" in result).toBe(true);
    if ("data" in result) {
      expect(result.data.leafs).toEqual([]);
      expect(mockClient.getLeafStatsBatch).not.toHaveBeenCalled();
    }
  });
});
