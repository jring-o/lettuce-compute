// --- Mocks ---

const mockAuth = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: () => mockAuth(),
}));

const mockGetLeaf = jest.fn();
const mockListWorkUnits = jest.fn();
const mockGetWorkUnit = jest.fn();
jest.mock("@/lib/infrastructure-client", () => ({
  infrastructureClient: {
    getLeaf: (...args: unknown[]) => mockGetLeaf(...args),
    listWorkUnits: (...args: unknown[]) => mockListWorkUnits(...args),
    getWorkUnit: (...args: unknown[]) => mockGetWorkUnit(...args),
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

import { GET } from "@/app/api/download/[leafId]/route";
import { NextRequest } from "next/server";

const mockLeaf = {
  id: "p1",
  slug: "test-leaf",
  creator_id: "u1",
  state: "ACTIVE",
};

const mockWorkUnitSummaries = [
  { id: "wu1", state: "COMPLETED" },
  { id: "wu2", state: "COMPLETED" },
];

const mockFullWorkUnits = [
  {
    id: "wu1",
    parameters: { temperature: 100, pressure: 1.5 },
    state: "COMPLETED",
    created_at: "2026-03-14T10:00:00Z",
  },
  {
    id: "wu2",
    parameters: { temperature: 200, pressure: 2.0 },
    state: "COMPLETED",
    created_at: "2026-03-14T11:00:00Z",
  },
];

beforeEach(() => {
  jest.clearAllMocks();
  mockAuth.mockResolvedValue({ user: { id: "u1" } });
  mockGetLeaf.mockResolvedValue(mockLeaf);
  mockListWorkUnits.mockResolvedValue({
    data: mockWorkUnitSummaries,
    pagination: { next_cursor: null, has_more: false },
  });
  mockGetWorkUnit.mockImplementation((_, id: string) => {
    const wu = mockFullWorkUnits.find((w) => w.id === id);
    return Promise.resolve(wu);
  });
});

function makeRequest(format = "json") {
  return new NextRequest(`http://localhost:3000/api/download/p1?format=${format}`);
}

describe("GET /api/download/[leafId]", () => {
  it("returns JSON with correct structure", async () => {
    const req = makeRequest("json");
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("application/json");
    expect(res.headers.get("Content-Disposition")).toContain("test-leaf-results.json");

    const body = JSON.parse(res.body as unknown as string);
    expect(body).toHaveLength(2);
    expect(body[0].work_unit_id).toBe("wu1");
    expect(body[0].parameters.temperature).toBe(100);
  });

  it("returns CSV with correct headers for format=csv", async () => {
    const req = makeRequest("csv");
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("text/csv");
    expect(res.headers.get("Content-Disposition")).toContain("test-leaf-results.csv");

    const csv = res.body as unknown as string;
    const lines = csv.split("\n");
    expect(lines[0]).toBe("work_unit_id,pressure,temperature,state,created_at");
    expect(lines).toHaveLength(3); // header + 2 data rows
  });

  it("returns 404 for non-existent leaf", async () => {
    mockGetLeaf.mockRejectedValue(new Error("Not found"));
    const req = makeRequest();
    const res = await GET(req, { params: Promise.resolve({ leafId: "nope" }) });

    expect(res.status).toBe(404);
  });

  it("returns 404 when user is not leaf owner", async () => {
    mockGetLeaf.mockResolvedValue({ ...mockLeaf, creator_id: "other-user" });
    const req = makeRequest();
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(404);
  });

  it("returns 204 when no completed results", async () => {
    mockListWorkUnits.mockResolvedValue({
      data: [],
      pagination: { next_cursor: null, has_more: false },
    });
    const req = makeRequest();
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(204);
  });

  it("rejects unauthenticated requests", async () => {
    mockAuth.mockResolvedValue(null);
    const req = makeRequest();
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(401);
  });

  it("returns 400 for invalid format parameter", async () => {
    const req = makeRequest("xml");
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(400);
  });

  it("defaults to json format when no format param is provided", async () => {
    const req = new NextRequest("http://localhost:3000/api/download/p1");
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("application/json");
    expect(res.headers.get("Content-Disposition")).toContain(".json");
  });

  it("paginates through multiple pages of completed work units", async () => {
    // First page returns cursor, second page finishes
    mockListWorkUnits
      .mockResolvedValueOnce({
        data: [{ id: "wu1", state: "COMPLETED" }],
        pagination: { next_cursor: "cursor-page2", has_more: true },
      })
      .mockResolvedValueOnce({
        data: [{ id: "wu2", state: "COMPLETED" }],
        pagination: { next_cursor: null, has_more: false },
      });

    const req = makeRequest("json");
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(200);
    expect(mockListWorkUnits).toHaveBeenCalledTimes(2);
    // First call: no cursor
    expect(mockListWorkUnits).toHaveBeenNthCalledWith(1, "p1", {
      state: "COMPLETED",
      limit: 200,
      cursor: undefined,
    });
    // Second call: with cursor
    expect(mockListWorkUnits).toHaveBeenNthCalledWith(2, "p1", {
      state: "COMPLETED",
      limit: 200,
      cursor: "cursor-page2",
    });

    const body = JSON.parse(res.body as unknown as string);
    expect(body).toHaveLength(2);
  });

  it("handles work unit fetch failures gracefully (filters out nulls)", async () => {
    // One work unit fetch fails
    mockGetWorkUnit.mockImplementation((_, id: string) => {
      if (id === "wu1") return Promise.reject(new Error("fetch failed"));
      return Promise.resolve(mockFullWorkUnits.find((w) => w.id === id));
    });

    const req = makeRequest("json");
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(200);
    const body = JSON.parse(res.body as unknown as string);
    // Only wu2 should be in results since wu1 fetch failed
    expect(body).toHaveLength(1);
    expect(body[0].work_unit_id).toBe("wu2");
  });

  it("handles CSV values with commas by quoting them", async () => {
    mockGetWorkUnit.mockImplementation((_, id: string) => {
      if (id === "wu1") {
        return Promise.resolve({
          id: "wu1",
          parameters: { name: "value,with,commas" },
          state: "COMPLETED",
          created_at: "2026-03-14T10:00:00Z",
        });
      }
      return Promise.resolve(null);
    });
    mockListWorkUnits.mockResolvedValue({
      data: [{ id: "wu1", state: "COMPLETED" }],
      pagination: { next_cursor: null, has_more: false },
    });

    const req = makeRequest("csv");
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(200);
    const csv = res.body as unknown as string;
    // The value with commas should be quoted
    expect(csv).toContain('"value,with,commas"');
  });

  it("handles work units with null parameters", async () => {
    mockGetWorkUnit.mockResolvedValue({
      id: "wu1",
      parameters: null,
      state: "COMPLETED",
      created_at: "2026-03-14T10:00:00Z",
    });
    mockListWorkUnits.mockResolvedValue({
      data: [{ id: "wu1", state: "COMPLETED" }],
      pagination: { next_cursor: null, has_more: false },
    });

    const req = makeRequest("json");
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(200);
    const body = JSON.parse(res.body as unknown as string);
    expect(body[0].parameters).toEqual({});
  });

  it("handles CSV values with double quotes by escaping them", async () => {
    mockGetWorkUnit.mockImplementation((_, id: string) => {
      if (id === "wu1") {
        return Promise.resolve({
          id: "wu1",
          parameters: { label: 'say "hello"' },
          state: "COMPLETED",
          created_at: "2026-03-14T10:00:00Z",
        });
      }
      return Promise.resolve(null);
    });
    mockListWorkUnits.mockResolvedValue({
      data: [{ id: "wu1", state: "COMPLETED" }],
      pagination: { next_cursor: null, has_more: false },
    });

    const req = makeRequest("csv");
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(200);
    const csv = res.body as unknown as string;
    // Double quotes inside values must be escaped as ""
    expect(csv).toContain('"say ""hello"""');
  });

  it("handles CSV values with newlines by quoting them", async () => {
    mockGetWorkUnit.mockImplementation((_, id: string) => {
      if (id === "wu1") {
        return Promise.resolve({
          id: "wu1",
          parameters: { notes: "line1\nline2" },
          state: "COMPLETED",
          created_at: "2026-03-14T10:00:00Z",
        });
      }
      return Promise.resolve(null);
    });
    mockListWorkUnits.mockResolvedValue({
      data: [{ id: "wu1", state: "COMPLETED" }],
      pagination: { next_cursor: null, has_more: false },
    });

    const req = makeRequest("csv");
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(200);
    const csv = res.body as unknown as string;
    expect(csv).toContain('"line1\nline2"');
  });

  it("handles heterogeneous parameter keys across work units in CSV", async () => {
    mockGetWorkUnit.mockImplementation((_, id: string) => {
      if (id === "wu1") {
        return Promise.resolve({
          id: "wu1",
          parameters: { alpha: 1 },
          state: "COMPLETED",
          created_at: "2026-03-14T10:00:00Z",
        });
      }
      if (id === "wu2") {
        return Promise.resolve({
          id: "wu2",
          parameters: { beta: 2 },
          state: "COMPLETED",
          created_at: "2026-03-14T11:00:00Z",
        });
      }
      return Promise.resolve(null);
    });

    const req = makeRequest("csv");
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(200);
    const csv = res.body as unknown as string;
    const lines = csv.split("\n");

    // Header should contain both alpha and beta columns
    expect(lines[0]).toContain("alpha");
    expect(lines[0]).toContain("beta");

    // wu1 has alpha=1 but no beta (empty)
    // wu2 has beta=2 but no alpha (empty)
    expect(lines).toHaveLength(3); // header + 2 rows
  });

  it("handles CSV parameter values that are undefined", async () => {
    mockGetWorkUnit.mockImplementation((_, id: string) => {
      if (id === "wu1") {
        return Promise.resolve({
          id: "wu1",
          parameters: { temp: undefined, pressure: 1.5 },
          state: "COMPLETED",
          created_at: "2026-03-14T10:00:00Z",
        });
      }
      return Promise.resolve(null);
    });
    mockListWorkUnits.mockResolvedValue({
      data: [{ id: "wu1", state: "COMPLETED" }],
      pagination: { next_cursor: null, has_more: false },
    });

    const req = makeRequest("csv");
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(200);
    // Should not crash and should produce valid CSV
    const csv = res.body as unknown as string;
    const lines = csv.split("\n");
    expect(lines.length).toBeGreaterThanOrEqual(2);
  });

  it("returns 401 when session exists but user is null", async () => {
    mockAuth.mockResolvedValue({ user: null });
    const req = makeRequest();
    const res = await GET(req, { params: Promise.resolve({ leafId: "p1" }) });

    expect(res.status).toBe(401);
  });
});
