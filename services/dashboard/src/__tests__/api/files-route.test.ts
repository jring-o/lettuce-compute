// --- Mocks ---

// The file download route is gated by requireFileAccess (the shared file-owner
// route adapter). We mock that seam so these tests exercise the ROUTE given an
// allow/deny verdict; requireFileAccess itself is covered in authz-routes.test.ts.
const mockRequireFileAccess = jest.fn();
jest.mock("@/lib/authz-routes", () => ({
  requireFileAccess: (...args: unknown[]) => mockRequireFileAccess(...args),
}));

const mockGetFileContent = jest.fn();
jest.mock("@/lib/file-storage", () => ({
  getFileContent: (...args: unknown[]) => mockGetFileContent(...args),
}));

// Mock next/server — NextRequest, NextResponse, and Response
// Jest jsdom doesn't provide NextRequest/NextResponse natively
jest.mock("next/server", () => {
  class MockNextResponse {
    body: Uint8Array | null;
    status: number;
    headers: Map<string, string>;

    constructor(body: Uint8Array | null, init?: { status?: number; headers?: Record<string, string> }) {
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
    constructor(url: string) {
      this.url = url;
    }
  }

  return {
    NextRequest: MockNextRequest,
    NextResponse: MockNextResponse,
  };
});

import { GET } from "@/app/api/files/[id]/route";
import { NextRequest } from "next/server";

// A denied verdict the way requireFileAccess returns it.
function denied(status: number, code: string, message: string) {
  return {
    ok: false,
    response: {
      status,
      _jsonData: { error: { code, message } },
    },
  };
}

function allowed(fileInfo: Record<string, unknown> = {}) {
  return {
    ok: true as const,
    session: { user: { id: "user-1", role: "USER" } },
    file: {
      path: "/data/uploads/file-1/data.csv",
      filename: "data.csv",
      contentType: "text/csv",
      sizeBytes: 11,
      checksum: "abc",
      leafId: "leaf-1",
      uploadedBy: "user-1",
      ...fileInfo,
    },
  };
}

beforeEach(() => {
  jest.clearAllMocks();
  mockRequireFileAccess.mockResolvedValue(allowed());
});

describe("GET /api/files/[id]", () => {
  it("returns the adapter's 401 when not authenticated (and never reads content)", async () => {
    mockRequireFileAccess.mockResolvedValue(
      denied(401, "UNAUTHENTICATED", "You must be signed in."),
    );

    const request = new NextRequest("http://localhost/api/files/file-1");
    const response = await GET(request, {
      params: Promise.resolve({ id: "file-1" }),
    });

    expect(response.status).toBe(401);
    expect(mockGetFileContent).not.toHaveBeenCalled();
  });

  it("returns 404 when the caller is not authorized (BG-10)", async () => {
    // requireFileAccess collapses not-owner and not-found into the SAME 404.
    mockRequireFileAccess.mockResolvedValue(denied(404, "NOT_FOUND", "File not found"));

    const request = new NextRequest("http://localhost/api/files/someone-elses-file");
    const response = await GET(request, {
      params: Promise.resolve({ id: "someone-elses-file" }),
    });

    expect(response.status).toBe(404);
    expect(mockGetFileContent).not.toHaveBeenCalled();
  });

  it("gates on the file id from the dynamic route params", async () => {
    const request = new NextRequest("http://localhost/api/files/specific-uuid");
    await GET(request, {
      params: Promise.resolve({ id: "specific-uuid" }),
    });

    expect(mockRequireFileAccess).toHaveBeenCalledWith("specific-uuid");
  });

  it("returns file content with correct headers once authorized", async () => {
    const fileBuffer = Buffer.from("hello world");
    mockGetFileContent.mockResolvedValue({
      buffer: fileBuffer,
      filename: "data.csv",
      contentType: "text/csv",
      sizeBytes: fileBuffer.length,
      checksum: "abc123",
    });

    const request = new NextRequest("http://localhost/api/files/file-1");
    const response = await GET(request, {
      params: Promise.resolve({ id: "file-1" }),
    });

    expect(response.status).toBe(200);
    expect(response.headers.get("Content-Type")).toBe("text/csv");
    expect(response.headers.get("Content-Length")).toBe(String(fileBuffer.length));
    expect(response.headers.get("Content-Disposition")).toBe(
      'attachment; filename="data.csv"',
    );
    expect(mockGetFileContent).toHaveBeenCalledWith("file-1");
  });

  it("returns 404 when content vanished after the access check", async () => {
    mockGetFileContent.mockResolvedValue(null);

    const request = new NextRequest("http://localhost/api/files/file-1");
    const response = await GET(request, {
      params: Promise.resolve({ id: "file-1" }),
    });

    expect(response.status).toBe(404);
    expect((response as unknown as Record<string, unknown>)._jsonData).toEqual({
      error: { code: "NOT_FOUND", message: "File not found" },
    });
  });

  it("defaults contentType to application/octet-stream when null", async () => {
    const fileBuffer = Buffer.from("binary data");
    mockGetFileContent.mockResolvedValue({
      buffer: fileBuffer,
      filename: "unknown.bin",
      contentType: null,
      sizeBytes: fileBuffer.length,
      checksum: "def456",
    });

    const request = new NextRequest("http://localhost/api/files/file-2");
    const response = await GET(request, {
      params: Promise.resolve({ id: "file-2" }),
    });

    expect(response.status).toBe(200);
    expect(response.headers.get("Content-Type")).toBe("application/octet-stream");
  });

  it("returns body as Uint8Array", async () => {
    const content = "test file content";
    const fileBuffer = Buffer.from(content);
    mockGetFileContent.mockResolvedValue({
      buffer: fileBuffer,
      filename: "test.txt",
      contentType: "text/plain",
      sizeBytes: fileBuffer.length,
      checksum: "ghi789",
    });

    const request = new NextRequest("http://localhost/api/files/file-3");
    const response = await GET(request, {
      params: Promise.resolve({ id: "file-3" }),
    });

    expect(response.body).toBeInstanceOf(Uint8Array);
  });
});
