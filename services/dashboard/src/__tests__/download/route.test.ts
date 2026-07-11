// --- Mocks ---

// Ownership is enforced by the shared requireLeafAccess adapter; mock that seam
// so these tests exercise the download ROUTE (state filter + payload) given an
// allow/deny verdict. requireLeafAccess itself is covered in authz-routes.test.ts.
const mockRequireLeafAccess = jest.fn();
jest.mock("@/lib/authz-routes", () => ({
  requireLeafAccess: (...args: unknown[]) => mockRequireLeafAccess(...args),
}));

const mockGetLeaf = jest.fn();
const mockListWorkUnits = jest.fn();
const mockGetWorkUnit = jest.fn();
const mockListResults = jest.fn();
jest.mock("@/lib/infrastructure-client", () => ({
  infrastructureClient: {
    getLeaf: (...args: unknown[]) => mockGetLeaf(...args),
    listWorkUnits: (...args: unknown[]) => mockListWorkUnits(...args),
    getWorkUnit: (...args: unknown[]) => mockGetWorkUnit(...args),
    listResults: (...args: unknown[]) => mockListResults(...args),
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

const allowed = { ok: true as const, session: { user: { id: "u1", role: "USER" } } };

function denied(status: number, code: string) {
  return {
    ok: false as const,
    response: { status, _jsonData: { error: { code, message: "denied" } } },
  };
}

const validatedSummaries = [
  { id: "wu1", state: "VALIDATED" },
  { id: "wu2", state: "VALIDATED" },
];

const fullUnits: Record<string, { id: string; parameters: Record<string, unknown> }> = {
  wu1: { id: "wu1", parameters: { temperature: 100, pressure: 1.5 } },
  wu2: { id: "wu2", parameters: { temperature: 200, pressure: 2.0 } },
};

const results: Record<string, unknown> = {
  wu1: {
    work_unit_id: "wu1",
    output_data: { energy: 42 },
    output_checksum: "chk1",
    validation_status: "AGREED",
    validated_at: "2026-03-15T00:00:00Z",
  },
  wu2: {
    work_unit_id: "wu2",
    output_data: { energy: 84 },
    output_checksum: "chk2",
    validation_status: "AGREED",
    validated_at: "2026-03-15T01:00:00Z",
  },
};

beforeEach(() => {
  jest.clearAllMocks();
  mockRequireLeafAccess.mockResolvedValue(allowed);
  mockGetLeaf.mockResolvedValue({ id: "p1", slug: "test-leaf", creator_id: "u1" });
  mockListWorkUnits.mockResolvedValue({
    data: validatedSummaries,
    pagination: { next_cursor: null, has_more: false },
  });
  mockGetWorkUnit.mockImplementation((_l: string, id: string) =>
    Promise.resolve(fullUnits[id] ?? null),
  );
  mockListResults.mockImplementation((_l: string, params: { work_unit_id: string }) =>
    Promise.resolve({
      data: results[params.work_unit_id] ? [results[params.work_unit_id]] : [],
      pagination: { next_cursor: null, has_more: false },
    }),
  );
});

function makeRequest(format?: string) {
  const q = format ? `?format=${format}` : "";
  return new NextRequest(`http://localhost:3000/api/download/p1${q}`);
}

describe("GET /api/download/[leafId]", () => {
  it("gates on the leaf via requireLeafAccess", async () => {
    await GET(makeRequest("json"), { params: Promise.resolve({ leafId: "p1" }) });
    expect(mockRequireLeafAccess).toHaveBeenCalledWith("p1");
  });

  it("fetches VALIDATED units (not COMPLETED) — agrees with the button metric", async () => {
    await GET(makeRequest("json"), { params: Promise.resolve({ leafId: "p1" }) });
    expect(mockListWorkUnits).toHaveBeenCalledWith("p1", {
      state: "VALIDATED",
      limit: 200,
      cursor: undefined,
    });
  });

  it("emits each validated unit's result output_data with parameters as context", async () => {
    const res = await GET(makeRequest("json"), {
      params: Promise.resolve({ leafId: "p1" }),
    });

    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("application/json");
    expect(res.headers.get("Content-Disposition")).toContain("test-leaf-results.json");

    const body = JSON.parse(res.body as unknown as string);
    expect(body).toHaveLength(2);
    expect(body[0].work_unit_id).toBe("wu1");
    expect(body[0].parameters.temperature).toBe(100);
    expect(body[0].output_data).toEqual({ energy: 42 });
    expect(body[0].output_checksum).toBe("chk1");
    // requests the AGREED (validated) result only
    expect(mockListResults).toHaveBeenCalledWith("p1", {
      work_unit_id: "wu1",
      validation_status: "AGREED",
      limit: 1,
    });
  });

  it("returns CSV with an output_data column", async () => {
    const res = await GET(makeRequest("csv"), {
      params: Promise.resolve({ leafId: "p1" }),
    });

    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("text/csv");
    const csv = res.body as unknown as string;
    const lines = csv.split("\n");
    expect(lines[0]).toBe(
      "work_unit_id,pressure,temperature,output_data,output_data_ref,output_checksum,validated_at",
    );
    expect(lines).toHaveLength(3);
    // output_data JSON has a comma, so it must be quoted
    expect(csv).toContain('"{""energy"":42}"');
  });

  it("returns the adapter's 401 when unauthenticated", async () => {
    mockRequireLeafAccess.mockResolvedValue(denied(401, "UNAUTHENTICATED"));
    const res = await GET(makeRequest(), { params: Promise.resolve({ leafId: "p1" }) });
    expect(res.status).toBe(401);
    expect(mockListWorkUnits).not.toHaveBeenCalled();
  });

  it("returns the adapter's 403/404 when not the owner", async () => {
    mockRequireLeafAccess.mockResolvedValue(denied(403, "FORBIDDEN"));
    const res = await GET(makeRequest(), { params: Promise.resolve({ leafId: "p1" }) });
    expect(res.status).toBe(403);
    expect(mockListWorkUnits).not.toHaveBeenCalled();
  });

  it("returns 204 when there are no validated units", async () => {
    mockListWorkUnits.mockResolvedValue({
      data: [],
      pagination: { next_cursor: null, has_more: false },
    });
    const res = await GET(makeRequest(), { params: Promise.resolve({ leafId: "p1" }) });
    expect(res.status).toBe(204);
  });

  it("returns 400 for an invalid format parameter", async () => {
    const res = await GET(makeRequest("xml"), { params: Promise.resolve({ leafId: "p1" }) });
    expect(res.status).toBe(400);
  });

  it("defaults to json when no format param is provided", async () => {
    const res = await GET(makeRequest(), { params: Promise.resolve({ leafId: "p1" }) });
    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("application/json");
  });

  it("still downloads (filename falls back) when the leaf lookup for the slug fails", async () => {
    mockGetLeaf.mockRejectedValue(new Error("infra hiccup"));
    const res = await GET(makeRequest("json"), { params: Promise.resolve({ leafId: "p1" }) });
    // Ownership already passed via requireLeafAccess; a slug-only failure is non-fatal.
    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Disposition")).toContain("p1-results.json");
  });

  it("paginates through multiple pages of validated units", async () => {
    mockListWorkUnits
      .mockResolvedValueOnce({
        data: [{ id: "wu1", state: "VALIDATED" }],
        pagination: { next_cursor: "page2", has_more: true },
      })
      .mockResolvedValueOnce({
        data: [{ id: "wu2", state: "VALIDATED" }],
        pagination: { next_cursor: null, has_more: false },
      });

    const res = await GET(makeRequest("json"), {
      params: Promise.resolve({ leafId: "p1" }),
    });

    expect(res.status).toBe(200);
    expect(mockListWorkUnits).toHaveBeenCalledTimes(2);
    expect(mockListWorkUnits).toHaveBeenNthCalledWith(2, "p1", {
      state: "VALIDATED",
      limit: 200,
      cursor: "page2",
    });
    const body = JSON.parse(res.body as unknown as string);
    expect(body).toHaveLength(2);
  });

  it("keeps a unit whose parameters fetch fails but result succeeds", async () => {
    mockGetWorkUnit.mockImplementation((_l: string, id: string) =>
      id === "wu1" ? Promise.reject(new Error("fail")) : Promise.resolve(fullUnits[id]),
    );

    const res = await GET(makeRequest("json"), {
      params: Promise.resolve({ leafId: "p1" }),
    });

    expect(res.status).toBe(200);
    const body = JSON.parse(res.body as unknown as string);
    // wu1 still present (result carried it), parameters empty; wu2 full.
    expect(body).toHaveLength(2);
    const wu1 = body.find((r: { work_unit_id: string }) => r.work_unit_id === "wu1");
    expect(wu1.parameters).toEqual({});
    expect(wu1.output_data).toEqual({ energy: 42 });
  });

  it("drops a unit when both its parameter and result fetches fail", async () => {
    mockGetWorkUnit.mockImplementation((_l: string, id: string) =>
      id === "wu1" ? Promise.reject(new Error("fail")) : Promise.resolve(fullUnits[id]),
    );
    mockListResults.mockImplementation((_l: string, params: { work_unit_id: string }) =>
      params.work_unit_id === "wu1"
        ? Promise.reject(new Error("fail"))
        : Promise.resolve({
            data: [results[params.work_unit_id]],
            pagination: { next_cursor: null, has_more: false },
          }),
    );

    const res = await GET(makeRequest("json"), {
      params: Promise.resolve({ leafId: "p1" }),
    });

    const body = JSON.parse(res.body as unknown as string);
    expect(body).toHaveLength(1);
    expect(body[0].work_unit_id).toBe("wu2");
  });

  it("quotes CSV parameter values containing commas / quotes / newlines", async () => {
    mockListWorkUnits.mockResolvedValue({
      data: [{ id: "wu1", state: "VALIDATED" }],
      pagination: { next_cursor: null, has_more: false },
    });
    mockGetWorkUnit.mockResolvedValue({
      id: "wu1",
      parameters: { a: "v,1", b: 'say "hi"', c: "l1\nl2" },
    });
    mockListResults.mockResolvedValue({
      data: [{ work_unit_id: "wu1", output_data: { ok: true }, output_checksum: "c" }],
      pagination: { next_cursor: null, has_more: false },
    });

    const res = await GET(makeRequest("csv"), {
      params: Promise.resolve({ leafId: "p1" }),
    });
    const csv = res.body as unknown as string;
    expect(csv).toContain('"v,1"');
    expect(csv).toContain('"say ""hi"""');
    expect(csv).toContain('"l1\nl2"');
  });

  it("handles null parameters", async () => {
    mockListWorkUnits.mockResolvedValue({
      data: [{ id: "wu1", state: "VALIDATED" }],
      pagination: { next_cursor: null, has_more: false },
    });
    mockGetWorkUnit.mockResolvedValue({ id: "wu1", parameters: null });
    mockListResults.mockResolvedValue({
      data: [{ work_unit_id: "wu1", output_data: { ok: true } }],
      pagination: { next_cursor: null, has_more: false },
    });

    const res = await GET(makeRequest("json"), {
      params: Promise.resolve({ leafId: "p1" }),
    });
    const body = JSON.parse(res.body as unknown as string);
    expect(body[0].parameters).toEqual({});
    expect(body[0].output_data).toEqual({ ok: true });
  });
});
