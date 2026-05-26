// --- Mocks ---

const mockAuth = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: () => mockAuth(),
}));

const mockSaveFile = jest.fn();
jest.mock("@/lib/file-storage", () => ({
  saveFile: (...args: unknown[]) => mockSaveFile(...args),
}));

// Mock next/server — NextRequest, NextResponse
jest.mock("next/server", () => {
  class MockNextResponse {
    body: unknown;
    status: number;
    _jsonData: unknown;

    constructor(body: unknown, init?: { status?: number }) {
      this.body = body;
      this.status = init?.status ?? 200;
    }

    static json(data: unknown, init?: { status?: number }) {
      const instance = new MockNextResponse(null, init);
      instance._jsonData = data;
      return instance;
    }
  }

  class MockNextRequest {
    url: string;
    _formData: FormData;

    constructor(url: string, init?: { method?: string; body?: FormData }) {
      this.url = url;
      this._formData = init?.body ?? new FormData();
    }

    formData() {
      return Promise.resolve(this._formData);
    }
  }

  return {
    NextRequest: MockNextRequest,
    NextResponse: MockNextResponse,
  };
});

import { POST } from "@/app/api/upload/route";
import { NextRequest } from "next/server";

function createFormData(fields: Record<string, string | File>): FormData {
  const fd = new FormData();
  for (const [key, value] of Object.entries(fields)) {
    fd.set(key, value);
  }
  return fd;
}

function createMockFile(name: string, content: string): File {
  const buffer = Buffer.from(content);
  return new File([buffer], name, { type: "application/octet-stream" });
}

function makeRequest(formData: FormData): NextRequest {
  return new (NextRequest as unknown as new (url: string, init?: { method?: string; body?: FormData }) => NextRequest)(
    "http://localhost/api/upload",
    { method: "POST", body: formData },
  );
}

beforeEach(() => {
  jest.clearAllMocks();
  mockAuth.mockResolvedValue({
    user: { id: "user-1", email: "test@example.com" },
  });
  mockSaveFile.mockResolvedValue({
    fileId: "file-uuid-123",
    storageKey: "leafs/proj-1/CODE_ARTIFACT/app.bin",
    checksum: "abc123def456",
    sizeBytes: 1024,
  });
});

describe("POST /api/upload", () => {
  it("returns 401 when not authenticated", async () => {
    mockAuth.mockResolvedValue(null);
    const file = createMockFile("app.bin", "binary-content");
    const fd = createFormData({ file, platform: "linux_amd64" });

    const response = await POST(makeRequest(fd));
    expect(response.status).toBe(401);
    expect((response as unknown as Record<string, unknown>)._jsonData).toEqual({
      error: { code: "UNAUTHENTICATED", message: "You must be signed in." },
    });
  });

  it("returns 401 when session has no user id", async () => {
    mockAuth.mockResolvedValue({ user: {} });
    const file = createMockFile("app.bin", "binary-content");
    const fd = createFormData({ file, platform: "linux_amd64" });

    const response = await POST(makeRequest(fd));
    expect(response.status).toBe(401);
  });

  it("returns 400 when no file is provided", async () => {
    const fd = new FormData();
    fd.set("platform", "linux_amd64");

    const response = await POST(makeRequest(fd));
    expect(response.status).toBe(400);
    expect((response as unknown as Record<string, unknown>)._jsonData).toEqual({
      error: { code: "VALIDATION_ERROR", message: "File is required." },
    });
  });

  it("returns 400 for invalid platform", async () => {
    const file = createMockFile("app.bin", "binary-content");
    const fd = createFormData({ file, platform: "invalid_platform" });

    const response = await POST(makeRequest(fd));
    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)._jsonData as Record<string, unknown>;
    const error = data.error as Record<string, string>;
    expect(error.code).toBe("VALIDATION_ERROR");
    expect(error.message).toContain("Invalid platform");
  });

  it("returns 201 with file info on successful upload", async () => {
    const file = createMockFile("app.bin", "binary-content");
    const fd = createFormData({
      file,
      platform: "linux_amd64",
      file_type: "CODE_ARTIFACT",
      leaf_id: "leaf-1",
    });

    const response = await POST(makeRequest(fd));
    expect(response.status).toBe(201);

    const data = (response as unknown as Record<string, unknown>)._jsonData as Record<string, unknown>;
    expect(data.file_id).toBe("file-uuid-123");
    expect(data.filename).toBe("app.bin");
    expect(data.size_bytes).toBe(1024);
    expect(data.checksum_sha256).toBe("abc123def456");
    expect(data.platform).toBe("linux_amd64");
  });

  it("calls saveFile with correct arguments", async () => {
    const file = createMockFile("app.bin", "binary-content");
    const fd = createFormData({
      file,
      platform: "linux_amd64",
      file_type: "CODE_ARTIFACT",
      leaf_id: "leaf-1",
    });

    await POST(makeRequest(fd));

    expect(mockSaveFile).toHaveBeenCalledTimes(1);
    expect(mockSaveFile).toHaveBeenCalledWith(
      expect.any(File),
      "leaf-1",
      "CODE_ARTIFACT",
      "user-1",
    );
  });

  it("defaults file_type to CODE_ARTIFACT when not specified", async () => {
    const file = createMockFile("app.bin", "binary-content");
    const fd = createFormData({ file, platform: "linux_amd64" });

    await POST(makeRequest(fd));

    expect(mockSaveFile).toHaveBeenCalledWith(
      expect.any(File),
      expect.any(String),
      "CODE_ARTIFACT",
      "user-1",
    );
  });

  it("returns null for platform when not specified", async () => {
    const file = createMockFile("data.csv", "col1,col2");
    const fd = createFormData({ file });

    const response = await POST(makeRequest(fd));
    expect(response.status).toBe(201);

    const data = (response as unknown as Record<string, unknown>)._jsonData as Record<string, unknown>;
    expect(data.platform).toBeNull();
  });

  it("accepts all valid platforms", async () => {
    const validPlatforms = [
      "linux_amd64",
      "linux_arm64",
      "darwin_amd64",
      "darwin_arm64",
      "windows_amd64",
    ];

    for (const platform of validPlatforms) {
      jest.clearAllMocks();
      mockAuth.mockResolvedValue({ user: { id: "user-1" } });
      mockSaveFile.mockResolvedValue({
        fileId: "file-uuid",
        storageKey: "key",
        checksum: "checksum",
        sizeBytes: 100,
      });

      const file = createMockFile("app.bin", "content");
      const fd = createFormData({ file, platform });

      const response = await POST(makeRequest(fd));
      expect(response.status).toBe(201);
    }
  });

  it("returns 400 when saveFile throws", async () => {
    mockSaveFile.mockRejectedValue(new Error("File exceeds size limit of 500 MB"));
    const file = createMockFile("huge.bin", "x");
    const fd = createFormData({ file, platform: "linux_amd64" });

    const response = await POST(makeRequest(fd));
    expect(response.status).toBe(400);

    const data = (response as unknown as Record<string, unknown>)._jsonData as Record<string, unknown>;
    const error = data.error as Record<string, string>;
    expect(error.code).toBe("UPLOAD_ERROR");
    expect(error.message).toContain("size limit");
  });

  it("returns generic error message for non-Error throws", async () => {
    mockSaveFile.mockRejectedValue("unknown failure");
    const file = createMockFile("app.bin", "content");
    const fd = createFormData({ file, platform: "linux_amd64" });

    const response = await POST(makeRequest(fd));
    expect(response.status).toBe(400);

    const data = (response as unknown as Record<string, unknown>)._jsonData as Record<string, unknown>;
    const error = data.error as Record<string, string>;
    expect(error.message).toBe("Failed to upload file.");
  });

  it("returns 400 when file has zero size", async () => {
    const file = createMockFile("empty.bin", "");
    Object.defineProperty(file, "size", { value: 0 });
    const fd = createFormData({ file, platform: "linux_amd64" });

    const response = await POST(makeRequest(fd));
    expect(response.status).toBe(400);
    expect((response as unknown as Record<string, unknown>)._jsonData).toEqual({
      error: { code: "VALIDATION_ERROR", message: "File is required." },
    });
  });

  it("generates a UUID for leaf_id when not provided", async () => {
    const file = createMockFile("app.bin", "binary-content");
    const fd = createFormData({ file, platform: "linux_amd64" });
    // No leaf_id in form data

    await POST(makeRequest(fd));

    expect(mockSaveFile).toHaveBeenCalledTimes(1);
    // The second argument to saveFile should be a UUID string (generated by randomUUID)
    const leafIdArg = mockSaveFile.mock.calls[0][1] as string;
    expect(leafIdArg).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/,
    );
  });
});
