// --- Mocks ---

const mockAuth = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: () => mockAuth(),
}));

const mockClient = {
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

import {
  authError,
  forbiddenError,
  mapInfraError,
  requireAuth,
  withOwnership,
} from "@/lib/actions/helpers";
import { InfrastructureApiError } from "@/lib/infrastructure-client";

// --- Fixtures ---

const authenticatedSession = {
  user: { id: "user-1", username: "alice", role: "USER" },
};

const adminSession = {
  user: { id: "admin-1", username: "admin", role: "ADMIN" },
};

// A minimal leaf shape — withOwnership only reads creator_id.
const ownedLeaf = { id: "leaf-1", creator_id: "user-1" };
const othersLeaf = { id: "leaf-1", creator_id: "someone-else" };

beforeEach(() => {
  jest.clearAllMocks();
});

// --- Pure helpers ---

describe("authError", () => {
  it("returns UNAUTHENTICATED error shape", () => {
    const result = authError();
    expect(result).toEqual({
      error: { code: "UNAUTHENTICATED", message: expect.any(String) },
    });
  });
});

describe("forbiddenError", () => {
  it("returns FORBIDDEN error shape", () => {
    const result = forbiddenError();
    expect(result).toEqual({
      error: { code: "FORBIDDEN", message: expect.any(String) },
    });
  });
});

describe("mapInfraError", () => {
  it("maps InfrastructureApiError to ActionError preserving code and message", () => {
    const err = new InfrastructureApiError("NOT_FOUND", "Leaf not found", 404);
    const result = mapInfraError(err);
    expect(result).toEqual({
      error: { code: "NOT_FOUND", message: "Leaf not found" },
    });
  });

  it("maps unknown errors to INTERNAL_ERROR", () => {
    const result = mapInfraError(new Error("something broke"));
    expect(result).toEqual({
      error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
    });
  });

  it("maps non-Error values to INTERNAL_ERROR", () => {
    const result = mapInfraError("string error");
    expect(result).toEqual({
      error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
    });
  });
});

// --- requireAuth ---

describe("requireAuth", () => {
  it("returns session when authenticated", async () => {
    mockAuth.mockResolvedValue(authenticatedSession);
    const result = await requireAuth();
    expect(result).toEqual(authenticatedSession);
  });

  it("returns null when no session", async () => {
    mockAuth.mockResolvedValue(null);
    const result = await requireAuth();
    expect(result).toBeNull();
  });

  it("returns null when session has no user id", async () => {
    mockAuth.mockResolvedValue({ user: {} });
    const result = await requireAuth();
    expect(result).toBeNull();
  });
});

// --- withOwnership ---
// The dashboard uses a single shared service key, so it must perform the
// per-user ownership check itself: it fetches the leaf, then allows the
// operation only for the owner or an ADMIN.

describe("withOwnership", () => {
  it("returns UNAUTHENTICATED when not signed in", async () => {
    mockAuth.mockResolvedValue(null);
    const fn = jest.fn();

    const result = await withOwnership("leaf-1", fn);

    expect(result).toEqual({
      error: { code: "UNAUTHENTICATED", message: expect.any(String) },
    });
    expect(fn).not.toHaveBeenCalled();
    expect(mockClient.getLeaf).not.toHaveBeenCalled();
  });

  it("fetches the leaf and runs fn for the owner", async () => {
    mockAuth.mockResolvedValue(authenticatedSession);
    mockClient.getLeaf.mockResolvedValue(ownedLeaf);
    const fn = jest.fn().mockResolvedValue({ ok: true });

    const result = await withOwnership("leaf-1", fn);

    expect(result).toEqual({ data: { ok: true } });
    expect(mockClient.getLeaf).toHaveBeenCalledWith("leaf-1");
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it("allows an ADMIN to act on a leaf owned by someone else", async () => {
    mockAuth.mockResolvedValue(adminSession);
    mockClient.getLeaf.mockResolvedValue(othersLeaf);
    const fn = jest.fn().mockResolvedValue({ ok: true });

    const result = await withOwnership("leaf-1", fn);

    expect(result).toEqual({ data: { ok: true } });
    expect(mockClient.getLeaf).toHaveBeenCalledWith("leaf-1");
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it("returns FORBIDDEN when a non-owner non-admin acts on another's leaf", async () => {
    mockAuth.mockResolvedValue(authenticatedSession);
    mockClient.getLeaf.mockResolvedValue(othersLeaf);
    const fn = jest.fn();

    const result = await withOwnership("leaf-1", fn);

    expect(result).toEqual({
      error: { code: "FORBIDDEN", message: expect.any(String) },
    });
    expect(fn).not.toHaveBeenCalled();
  });

  it("returns FORBIDDEN (not NOT_FOUND) when the leaf is missing for a non-admin", async () => {
    mockAuth.mockResolvedValue(authenticatedSession);
    mockClient.getLeaf.mockRejectedValue(
      new InfrastructureApiError("NOT_FOUND", "Leaf not found", 404),
    );
    const fn = jest.fn();

    const result = await withOwnership("leaf-1", fn);

    expect(result).toEqual({
      error: { code: "FORBIDDEN", message: expect.any(String) },
    });
    expect(fn).not.toHaveBeenCalled();
  });

  it("maps infrastructure errors from the ownership fetch", async () => {
    mockAuth.mockResolvedValue(authenticatedSession);
    mockClient.getLeaf.mockRejectedValue(
      new InfrastructureApiError("INTERNAL_ERROR", "infra down", 503),
    );
    const fn = jest.fn();

    const result = await withOwnership("leaf-1", fn);

    expect(result).toEqual({
      error: { code: "INTERNAL_ERROR", message: "infra down" },
    });
    expect(fn).not.toHaveBeenCalled();
  });

  it("maps infrastructure errors from the callback", async () => {
    mockAuth.mockResolvedValue(authenticatedSession);
    mockClient.getLeaf.mockResolvedValue(ownedLeaf);
    const fn = jest.fn().mockRejectedValue(
      new InfrastructureApiError("CONFLICT", "State conflict", 409),
    );

    const result = await withOwnership("leaf-1", fn);

    expect(result).toEqual({
      error: { code: "CONFLICT", message: "State conflict" },
    });
  });

  it("maps unknown errors to INTERNAL_ERROR", async () => {
    mockAuth.mockResolvedValue(authenticatedSession);
    mockClient.getLeaf.mockResolvedValue(ownedLeaf);
    const fn = jest.fn().mockRejectedValue(new Error("boom"));

    const result = await withOwnership("leaf-1", fn);

    expect(result).toEqual({
      error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
    });
  });
});
