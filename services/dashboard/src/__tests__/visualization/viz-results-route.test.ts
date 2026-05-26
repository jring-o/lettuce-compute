// --- Mocks ---

const mockListResults = jest.fn();

jest.mock("@/lib/infrastructure-client", () => ({
  infrastructureClient: {
    listResults: (...args: unknown[]) => mockListResults(...args),
  },
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

    constructor(url: string) {
      this.url = url;
      this.nextUrl = { searchParams: new URL(url).searchParams };
    }
  }

  return {
    NextRequest: MockNextRequest,
    NextResponse: MockNextResponse,
  };
});

import { GET } from "@/app/api/viz/results/route";
import { NextRequest } from "next/server";

beforeEach(() => {
  jest.clearAllMocks();
});

describe("GET /api/viz/results", () => {
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
      validation_status: "AGREED",
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
      validation_status: "AGREED",
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
      validation_status: "AGREED",
      limit: 1,
    });
  });
});
