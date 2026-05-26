// --- Mocks ---

const mockAuth = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: () => mockAuth(),
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

const authenticatedSession = {
  user: { id: "user-1", username: "admin", role: "USER" },
};

beforeEach(() => {
  jest.clearAllMocks();
  mockAuth.mockResolvedValue(authenticatedSession);
});

describe("GET /api/files/[id]", () => {
  it("returns 401 when not authenticated", async () => {
    mockAuth.mockResolvedValue(null);

    const request = new NextRequest("http://localhost/api/files/file-1");
    const response = await GET(request, {
      params: Promise.resolve({ id: "file-1" }),
    });

    expect(response.status).toBe(401);
    expect(mockGetFileContent).not.toHaveBeenCalled();
  });

  it("returns file content with correct headers", async () => {
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

  it("returns 404 when file not found", async () => {
    mockGetFileContent.mockResolvedValue(null);

    const request = new NextRequest("http://localhost/api/files/missing");
    const response = await GET(request, {
      params: Promise.resolve({ id: "missing" }),
    });

    expect(response.status).toBe(404);
    // Check the JSON body was set
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

  it("passes file id from dynamic route params", async () => {
    mockGetFileContent.mockResolvedValue(null);

    const request = new NextRequest("http://localhost/api/files/specific-uuid");
    await GET(request, {
      params: Promise.resolve({ id: "specific-uuid" }),
    });

    expect(mockGetFileContent).toHaveBeenCalledWith("specific-uuid");
  });

  it("throws when getFileContent encounters an error", async () => {
    mockGetFileContent.mockRejectedValue(new Error("Disk read failure"));

    const request = new NextRequest("http://localhost/api/files/file-err");

    // Route has no try/catch, so the error propagates
    await expect(
      GET(request, { params: Promise.resolve({ id: "file-err" }) }),
    ).rejects.toThrow("Disk read failure");
  });
});
