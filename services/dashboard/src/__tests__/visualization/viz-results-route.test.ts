// --- Mocks ---

const mockListResults = jest.fn();
const mockGetLeaf = jest.fn();

jest.mock("@/lib/infrastructure-client", () => ({
  infrastructureClient: {
    listResults: (...args: unknown[]) => mockListResults(...args),
    getLeaf: (...args: unknown[]) => mockGetLeaf(...args),
  },
}));

// The route now gates on requireLeafAccess (result output_data is owner-only,
// BG-07). Mock that seam: default to allowed so the existing behavior tests
// hold; the denial is asserted explicitly below.
const mockRequireLeafAccess = jest.fn();
jest.mock("@/lib/authz-routes", () => ({
  requireLeafAccess: (...args: unknown[]) => mockRequireLeafAccess(...args),
}));

// Mock next/server — NextRequest, NextResponse
jest.mock("next/server", () => {
  class MockNextResponse {
    body: Uint8Array | null;
    status: number;
    headers: Map<string, string>;

    constructor(
      body: Uint8Array | null,
      init?: { status?: number; headers?: Record<string, string> },
    ) {
      this.body = body;
      this.status = init?.status ?? 200;
      this.headers = new Map(Object.entries(init?.headers ?? {}));
    }

    static json(data: unknown, init?: { status?: number }) {
      const instance = new MockNextResponse(null, { status: init?.status });
      (instance as unknown as Record<string, unknown>)._jsonData = data;
      return instance;
    }
  }

  class MockNextRequest {
    url: string;
    nextUrl: { searchParams: URLSearchParams };
    headers: { get(name: string): string | null };

    constructor(url: string, init?: { headers?: Record<string, string> }) {
      this.url = url;
      this.nextUrl = { searchParams: new URL(url).searchParams };
      const headerMap = new Map(
        Object.entries(init?.headers ?? {}).map(([k, v]) => [k.toLowerCase(), v]),
      );
      this.headers = { get: (name: string) => headerMap.get(name.toLowerCase()) ?? null };
    }
  }

  return {
    NextRequest: MockNextRequest,
    NextResponse: MockNextResponse,
  };
});

import { GET } from "@/app/api/viz/results/route";
import { NextRequest } from "next/server";
import { resetPublicVizGuard } from "@/lib/public-viz-guard";

beforeEach(() => {
  jest.clearAllMocks();
  // The public path keeps per-process rate-limit and cache state — drop it so
  // no test's traffic or cached verdicts bleed into another's expectations.
  resetPublicVizGuard();
  mockRequireLeafAccess.mockResolvedValue({
    ok: true,
    session: { user: { id: "user-1", role: "USER" } },
  });
});

describe("GET /api/viz/results", () => {
  // --- Authorization (BG-07) ---

  it("returns the adapter's denial and never lists results for a non-owner", async () => {
    mockRequireLeafAccess.mockResolvedValue({
      ok: false,
      response: {
        status: 403,
        _jsonData: { error: { code: "FORBIDDEN", message: "no" } },
      },
    });
    // The denied-caller fallback consults the leaf's results_visibility; an
    // OWNER_ONLY leaf keeps the exact BG-07 denial.
    mockGetLeaf.mockResolvedValue({
      id: "leaf-abc",
      slug: "secret-leaf",
      visibility: "PUBLIC",
      results_visibility: "OWNER_ONLY",
    });

    const request = new NextRequest(
      "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-123",
    );
    const response = await GET(request);

    expect(response.status).toBe(403);
    expect(mockListResults).not.toHaveBeenCalled();
    expect(mockRequireLeafAccess).toHaveBeenCalledWith("leaf-abc");
  });

  // --- Public visualization opt-in (leaf.results_visibility, design §4.7) ---

  describe("public results opt-in", () => {
    const DENIAL = {
      ok: false,
      response: {
        status: 401,
        _jsonData: { error: { code: "UNAUTHENTICATED", message: "no" } },
      },
    };

    beforeEach(() => {
      mockRequireLeafAccess.mockResolvedValue(DENIAL);
      mockListResults.mockResolvedValue({
        data: [{ id: "result-1", work_unit_id: "wu-123", output_data: {} }],
        pagination: { next_cursor: null, has_more: false },
      });
    });

    it("serves results to a denied caller when the leaf's results_visibility is PUBLIC", async () => {
      mockGetLeaf.mockResolvedValue({
        id: "leaf-abc",
        slug: "public-leaf",
        visibility: "PUBLIC",
        results_visibility: "PUBLIC",
      });

      const request = new NextRequest(
        "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-123",
      );
      const response = await GET(request);

      expect(response.status).toBe(200);
      expect(mockGetLeaf).toHaveBeenCalledWith("leaf-abc");
      expect(mockListResults).toHaveBeenCalledWith("leaf-abc", {
        work_unit_id: "wu-123",
        limit: 1,
      });
    });

    it("serves an UNLISTED leaf's results when results_visibility is PUBLIC", async () => {
      mockGetLeaf.mockResolvedValue({
        id: "leaf-abc",
        slug: "public-leaf",
        visibility: "UNLISTED",
        results_visibility: "PUBLIC",
      });

      const request = new NextRequest(
        "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-123",
      );
      const response = await GET(request);

      expect(response.status).toBe(200);
    });

    it("returns the denial when results_visibility is OWNER_ONLY", async () => {
      mockGetLeaf.mockResolvedValue({
        id: "leaf-abc",
        slug: "secret-leaf",
        visibility: "PUBLIC",
        results_visibility: "OWNER_ONLY",
      });

      const request = new NextRequest(
        "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-123",
      );
      const response = await GET(request);

      expect(response.status).toBe(401);
      expect(mockListResults).not.toHaveBeenCalled();
    });

    it("returns the denial for a PRIVATE leaf even when results_visibility is PUBLIC", async () => {
      mockGetLeaf.mockResolvedValue({
        id: "leaf-abc",
        slug: "public-leaf",
        visibility: "PRIVATE",
        results_visibility: "PUBLIC",
      });

      const request = new NextRequest(
        "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-123",
      );
      const response = await GET(request);

      expect(response.status).toBe(401);
      expect(mockListResults).not.toHaveBeenCalled();
    });

    it("returns the denial when the leaf lookup fails", async () => {
      mockGetLeaf.mockRejectedValue(new Error("Connection refused"));

      const request = new NextRequest(
        "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-123",
      );
      const response = await GET(request);

      expect(response.status).toBe(401);
      expect(mockListResults).not.toHaveBeenCalled();
    });

    it("never consults the leaf's opt-in for an allowed owner", async () => {
      mockRequireLeafAccess.mockResolvedValue({
        ok: true,
        session: { user: { id: "user-1", role: "USER" } },
      });

      const request = new NextRequest(
        "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-123",
      );
      const response = await GET(request);

      expect(response.status).toBe(200);
      expect(mockGetLeaf).not.toHaveBeenCalled();
    });

    // --- The public path's guard rails (lib/public-viz-guard.ts) ---

    it("caches the public verdict and payload: a repeat request re-calls neither getLeaf nor listResults", async () => {
      mockGetLeaf.mockResolvedValue({
        id: "leaf-abc",
        slug: "public-leaf",
        visibility: "PUBLIC",
        results_visibility: "PUBLIC",
      });

      const url =
        "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-123";
      const first = await GET(new NextRequest(url));
      expect(first.status).toBe(200);
      const second = await GET(new NextRequest(url));
      expect(second.status).toBe(200);

      expect(mockGetLeaf).toHaveBeenCalledTimes(1);
      expect(mockListResults).toHaveBeenCalledTimes(1);
      const firstData = (first as unknown as Record<string, unknown>)._jsonData;
      const secondData = (second as unknown as Record<string, unknown>)._jsonData;
      expect(secondData).toEqual(firstData);
    });

    it("rate limits an anonymous IP with a 429 and stops driving head calls", async () => {
      mockGetLeaf.mockResolvedValue({
        id: "leaf-abc",
        slug: "public-leaf",
        visibility: "PUBLIC",
        results_visibility: "PUBLIC",
      });

      // Distinct work units defeat the response cache, so each allowed request
      // is a real head call; the 121st request in the window must be refused.
      const makeReq = (i: number) =>
        new NextRequest(
          `http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-${i}`,
          { headers: { "x-forwarded-for": "203.0.113.7" } },
        );
      for (let i = 0; i < 120; i++) {
        const res = await GET(makeReq(i));
        expect(res.status).toBe(200);
      }
      const listCallsBefore = mockListResults.mock.calls.length;

      const refused = await GET(makeReq(120));
      expect(refused.status).toBe(429);
      expect(mockListResults.mock.calls.length).toBe(listCallsBefore);
    });

    it("never rate limits an authenticated owner", async () => {
      mockRequireLeafAccess.mockResolvedValue({
        ok: true,
        session: { user: { id: "user-1", role: "USER" } },
      });

      for (let i = 0; i < 130; i++) {
        const res = await GET(
          new NextRequest(
            `http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-${i}`,
            { headers: { "x-forwarded-for": "203.0.113.7" } },
          ),
        );
        expect(res.status).toBe(200);
      }
    });
  });

  // --- Missing params ---

  it("returns 400 when leafId is missing", async () => {
    const request = new NextRequest(
      "http://localhost/api/viz/results?workUnitId=wu-123",
    );
    const response = await GET(request);

    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)
      ._jsonData as Record<string, unknown>;
    expect((data.error as Record<string, unknown>).code).toBe("MISSING_PARAMS");
  });

  it("returns 400 when workUnitId is missing", async () => {
    const request = new NextRequest(
      "http://localhost/api/viz/results?leafId=leaf-abc",
    );
    const response = await GET(request);

    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)
      ._jsonData as Record<string, unknown>;
    expect((data.error as Record<string, unknown>).code).toBe("MISSING_PARAMS");
  });

  it("returns 400 when both params are missing", async () => {
    const request = new NextRequest("http://localhost/api/viz/results");
    const response = await GET(request);

    expect(response.status).toBe(400);
  });

  // --- Successful result fetch ---

  it("returns result when infrastructure client has a matching result", async () => {
    const mockResult = {
      id: "result-1",
      work_unit_id: "wu-123",
      volunteer_id: "vol-1",
      output_data: { test_key: "test_value", numeric: 42 },
      validation_status: "AGREED",
    };

    mockListResults.mockResolvedValue({
      data: [mockResult],
      pagination: { next_cursor: null, has_more: false },
    });

    const request = new NextRequest(
      "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-123",
    );
    const response = await GET(request);

    expect(response.status).toBe(200);
    const data = (response as unknown as Record<string, unknown>)
      ._jsonData as { result: typeof mockResult };
    expect(data.result).toEqual(mockResult);

    // Verify the infrastructure client was called with correct params
    expect(mockListResults).toHaveBeenCalledWith("leaf-abc", {
      work_unit_id: "wu-123",
      limit: 1,
    });
  });

  it("returns null result when no matching results exist", async () => {
    mockListResults.mockResolvedValue({
      data: [],
      pagination: { next_cursor: null, has_more: false },
    });

    const request = new NextRequest(
      "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-empty",
    );
    const response = await GET(request);

    expect(response.status).toBe(200);
    const data = (response as unknown as Record<string, unknown>)
      ._jsonData as { result: null };
    expect(data.result).toBeNull();
  });

  it("returns null result (200) when infrastructure client throws", async () => {
    mockListResults.mockRejectedValue(new Error("Connection refused"));

    const request = new NextRequest(
      "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-err",
    );
    const response = await GET(request);

    // Route catches errors and returns { result: null } with 200
    expect(response.status).toBe(200);
    const data = (response as unknown as Record<string, unknown>)
      ._jsonData as { result: null };
    expect(data.result).toBeNull();
  });

  it("returns only the first result when multiple exist", async () => {
    const result1 = {
      id: "result-1",
      work_unit_id: "wu-123",
      output_data: { first: true },
      validation_status: "AGREED",
    };
    const result2 = {
      id: "result-2",
      work_unit_id: "wu-123",
      output_data: { second: true },
      validation_status: "AGREED",
    };

    mockListResults.mockResolvedValue({
      data: [result1, result2],
      pagination: { next_cursor: null, has_more: false },
    });

    const request = new NextRequest(
      "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-123",
    );
    const response = await GET(request);

    expect(response.status).toBe(200);
    const data = (response as unknown as Record<string, unknown>)
      ._jsonData as { result: typeof result1 };
    expect(data.result.id).toBe("result-1");
    expect(data.result.output_data).toEqual({ first: true });
  });

  // --- S109: volunteerId passthrough ---

  it("passes volunteerId to listResults when provided", async () => {
    mockListResults.mockResolvedValue({
      data: [],
      pagination: { next_cursor: null, has_more: false },
    });

    const request = new NextRequest(
      "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-123&volunteerId=vol-xyz",
    );
    const response = await GET(request);

    expect(response.status).toBe(200);
    expect(mockListResults).toHaveBeenCalledWith("leaf-abc", {
      work_unit_id: "wu-123",
      limit: 1,
      volunteer_id: "vol-xyz",
    });
  });

  it("does not include volunteer_id when volunteerId param is absent", async () => {
    mockListResults.mockResolvedValue({
      data: [],
      pagination: { next_cursor: null, has_more: false },
    });

    const request = new NextRequest(
      "http://localhost/api/viz/results?leafId=leaf-abc&workUnitId=wu-123",
    );
    const response = await GET(request);

    expect(response.status).toBe(200);
    expect(mockListResults).toHaveBeenCalledWith("leaf-abc", {
      work_unit_id: "wu-123",
      limit: 1,
    });
  });
});
