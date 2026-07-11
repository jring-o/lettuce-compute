/**
 * @jest-environment node
 */

// --- Mocks ---

const mockAuth = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: () => mockAuth(),
}));

const mockGetLeaf = jest.fn();
jest.mock("@/lib/infrastructure-client", () => ({
  infrastructureClient: { getLeaf: (...a: unknown[]) => mockGetLeaf(...a) },
  InfrastructureApiError: class extends Error {
    code: string;
    status: number;
    constructor(code: string, message: string, status: number) {
      super(message);
      this.code = code;
      this.status = status;
    }
  },
}));

const mockGetFile = jest.fn();
jest.mock("@/lib/file-storage", () => ({
  getFile: (...a: unknown[]) => mockGetFile(...a),
}));

// next/server's NextResponse.json — capture status + body.
jest.mock("next/server", () => ({
  NextResponse: {
    json: (data: unknown, init?: { status?: number }) => ({
      status: init?.status ?? 200,
      _jsonData: data,
    }),
  },
}));

import { requireFileAccess, requireLeafAccess } from "@/lib/authz-routes";
import { InfrastructureApiError } from "@/lib/infrastructure-client";

const userSession = { user: { id: "user-1", role: "USER" } };
const adminSession = { user: { id: "admin-1", role: "ADMIN" } };

function statusOf(r: unknown): number {
  return (r as { response: { status: number } }).response.status;
}
function codeOf(r: unknown): string {
  const body = (r as { response: { _jsonData: { error: { code: string } } } })
    .response._jsonData;
  return body.error.code;
}

beforeEach(() => {
  jest.clearAllMocks();
});

describe("requireLeafAccess", () => {
  it("401s an unauthenticated caller before any infra read", async () => {
    mockAuth.mockResolvedValue(null);
    const res = await requireLeafAccess("leaf-1");
    expect(res.ok).toBe(false);
    expect(statusOf(res)).toBe(401);
    expect(mockGetLeaf).not.toHaveBeenCalled();
  });

  it("400s a missing leaf_id", async () => {
    mockAuth.mockResolvedValue(userSession);
    const res = await requireLeafAccess(null);
    expect(res.ok).toBe(false);
    expect(statusOf(res)).toBe(400);
    expect(mockGetLeaf).not.toHaveBeenCalled();
  });

  it("allows the leaf's creator", async () => {
    mockAuth.mockResolvedValue(userSession);
    mockGetLeaf.mockResolvedValue({ id: "leaf-1", creator_id: "user-1" });
    const res = await requireLeafAccess("leaf-1");
    expect(res.ok).toBe(true);
  });

  it("allows an ADMIN over someone else's leaf", async () => {
    mockAuth.mockResolvedValue(adminSession);
    mockGetLeaf.mockResolvedValue({ id: "leaf-1", creator_id: "user-1" });
    const res = await requireLeafAccess("leaf-1");
    expect(res.ok).toBe(true);
  });

  it("403s a non-owner", async () => {
    mockAuth.mockResolvedValue(userSession);
    mockGetLeaf.mockResolvedValue({ id: "leaf-1", creator_id: "someone-else" });
    const res = await requireLeafAccess("leaf-1");
    expect(res.ok).toBe(false);
    expect(statusOf(res)).toBe(403);
  });

  it("collapses a missing leaf into 403 for a non-admin (no existence leak)", async () => {
    mockAuth.mockResolvedValue(userSession);
    mockGetLeaf.mockRejectedValue(
      new InfrastructureApiError("NOT_FOUND", "not found", 404),
    );
    const res = await requireLeafAccess("leaf-1");
    expect(res.ok).toBe(false);
    expect(statusOf(res)).toBe(403);
  });
});

describe("requireFileAccess", () => {
  const file = {
    path: "/p",
    filename: "f.bin",
    contentType: null,
    sizeBytes: 1,
    checksum: "c",
    leafId: "leaf-1",
    uploadedBy: "uploader-1",
  };

  it("401s an unauthenticated caller before any file read", async () => {
    mockAuth.mockResolvedValue(null);
    const res = await requireFileAccess("file-1");
    expect(res.ok).toBe(false);
    expect(statusOf(res)).toBe(401);
    expect(mockGetFile).not.toHaveBeenCalled();
  });

  it("404s a missing file (no existence leak)", async () => {
    mockAuth.mockResolvedValue(userSession);
    mockGetFile.mockResolvedValue(null);
    const res = await requireFileAccess("file-1");
    expect(res.ok).toBe(false);
    expect(statusOf(res)).toBe(404);
    expect(codeOf(res)).toBe("NOT_FOUND");
  });

  it("allows the uploader without a leaf lookup", async () => {
    mockAuth.mockResolvedValue({ user: { id: "uploader-1", role: "USER" } });
    mockGetFile.mockResolvedValue(file);
    const res = await requireFileAccess("file-1");
    expect(res.ok).toBe(true);
    expect(mockGetLeaf).not.toHaveBeenCalled();
  });

  it("allows an ADMIN over any file without a leaf lookup", async () => {
    mockAuth.mockResolvedValue(adminSession);
    mockGetFile.mockResolvedValue(file);
    const res = await requireFileAccess("file-1");
    expect(res.ok).toBe(true);
    expect(mockGetLeaf).not.toHaveBeenCalled();
  });

  it("allows a non-uploader who owns the file's leaf", async () => {
    mockAuth.mockResolvedValue(userSession); // user-1, not the uploader
    mockGetFile.mockResolvedValue(file);
    mockGetLeaf.mockResolvedValue({ id: "leaf-1", creator_id: "user-1" });
    const res = await requireFileAccess("file-1");
    expect(res.ok).toBe(true);
  });

  it("404s a non-uploader who does not own the leaf (same as missing, BG-10)", async () => {
    mockAuth.mockResolvedValue(userSession);
    mockGetFile.mockResolvedValue(file);
    mockGetLeaf.mockResolvedValue({ id: "leaf-1", creator_id: "someone-else" });
    const res = await requireFileAccess("file-1");
    expect(res.ok).toBe(false);
    expect(statusOf(res)).toBe(404);
  });
});
