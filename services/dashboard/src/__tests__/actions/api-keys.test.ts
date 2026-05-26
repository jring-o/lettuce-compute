// --- Mocks ---

const mockAuth = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: () => mockAuth(),
}));

// Mock crypto so generateKey() is deterministic
const mockRandomBytes = jest.fn();
const mockCreateHash = jest.fn();
jest.mock("node:crypto", () => ({
  randomBytes: (...args: unknown[]) => mockRandomBytes(...args),
  createHash: (...args: unknown[]) => mockCreateHash(...args),
}));

// Drizzle query chain mocks
const mockReturning = jest.fn();
const mockValues = jest.fn().mockReturnValue({ returning: mockReturning });
const mockInsert = jest.fn().mockReturnValue({ values: mockValues });

const mockOrderBy = jest.fn();
const mockSelectWhere = jest.fn().mockReturnValue({ orderBy: mockOrderBy });
const mockSelectFrom = jest.fn().mockReturnValue({ where: mockSelectWhere });
const mockSelect = jest.fn().mockReturnValue({ from: mockSelectFrom });

const mockUpdateReturning = jest.fn();
const mockUpdateWhere = jest.fn().mockReturnValue({ returning: mockUpdateReturning });
const mockUpdateSet = jest.fn().mockReturnValue({ where: mockUpdateWhere });
const mockUpdate = jest.fn().mockReturnValue({ set: mockUpdateSet });

jest.mock("@/lib/db", () => ({
  db: {
    insert: (...args: unknown[]) => mockInsert(...args),
    select: (...args: unknown[]) => mockSelect(...args),
    update: (...args: unknown[]) => mockUpdate(...args),
  },
}));

jest.mock("@/lib/db/schema", () => ({
  apiKeys: {
    id: "id_col",
    userId: "user_id_col",
    name: "name_col",
    keyPrefix: "key_prefix_col",
    keyHash: "key_hash_col",
    createdAt: "created_at_col",
    lastUsedAt: "last_used_at_col",
    revokedAt: "revoked_at_col",
  },
}));

jest.mock("drizzle-orm", () => ({
  eq: jest.fn((...args: unknown[]) => ({ type: "eq", args })),
  and: jest.fn((...args: unknown[]) => ({ type: "and", args })),
  isNull: jest.fn((col: unknown) => ({ type: "isNull", col })),
}));

import { createApiKey, listApiKeys, revokeApiKey } from "@/lib/actions/api-keys";

// --- Fixtures ---

const authenticatedSession = {
  user: { id: "user-1", username: "alice", role: "USER" },
};

const now = new Date("2026-01-15T12:00:00Z");

const mockInsertedRow = {
  id: "key-1",
  name: "My API Key",
  keyPrefix: "lk_ABCDEFGHI",
  createdAt: now,
  lastUsedAt: null,
  revokedAt: null,
};

// --- Helpers ---

function setupCryptoMocks() {
  // Return a deterministic 32-byte buffer
  const fakeBytes = Buffer.alloc(32, 0xab);
  mockRandomBytes.mockReturnValue(fakeBytes);

  const fakeHash = Buffer.alloc(32, 0xcd);
  mockCreateHash.mockReturnValue({
    update: jest.fn().mockReturnValue({
      digest: jest.fn().mockReturnValue(fakeHash),
    }),
  });
}

function resetChainMocks() {
  mockInsert.mockReturnValue({ values: mockValues });
  mockValues.mockReturnValue({ returning: mockReturning });

  mockSelect.mockReturnValue({ from: mockSelectFrom });
  mockSelectFrom.mockReturnValue({ where: mockSelectWhere });
  mockSelectWhere.mockReturnValue({ orderBy: mockOrderBy });

  mockUpdate.mockReturnValue({ set: mockUpdateSet });
  mockUpdateSet.mockReturnValue({ where: mockUpdateWhere });
  mockUpdateWhere.mockReturnValue({ returning: mockUpdateReturning });
}

beforeEach(() => {
  jest.clearAllMocks();
  resetChainMocks();
  setupCryptoMocks();
});

// --- Tests ---

describe("API Key Server Actions", () => {
  // --- Authentication ---

  describe("authentication", () => {
    it("createApiKey returns UNAUTHENTICATED when not signed in", async () => {
      mockAuth.mockResolvedValue(null);

      const result = await createApiKey("My Key");

      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: expect.any(String) },
      });
      expect(mockInsert).not.toHaveBeenCalled();
    });

    it("listApiKeys returns UNAUTHENTICATED when not signed in", async () => {
      mockAuth.mockResolvedValue(null);

      const result = await listApiKeys();

      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: expect.any(String) },
      });
      expect(mockSelect).not.toHaveBeenCalled();
    });

    it("revokeApiKey returns UNAUTHENTICATED when not signed in", async () => {
      mockAuth.mockResolvedValue(null);

      const result = await revokeApiKey("key-1");

      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: expect.any(String) },
      });
      expect(mockUpdate).not.toHaveBeenCalled();
    });

    it("returns UNAUTHENTICATED when session has no user id", async () => {
      mockAuth.mockResolvedValue({ user: {} });

      const result = await createApiKey("My Key");

      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: expect.any(String) },
      });
    });
  });

  // --- createApiKey ---

  describe("createApiKey", () => {
    it("creates key and returns plaintext + key info", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockReturning.mockResolvedValue([mockInsertedRow]);

      const result = await createApiKey("My API Key");

      expect(result).toHaveProperty("data");
      const data = (result as { data: { key: typeof mockInsertedRow; plaintextKey: string } }).data;
      expect(data.key.id).toBe("key-1");
      expect(data.key.name).toBe("My API Key");
      expect(data.key.keyPrefix).toBe("lk_ABCDEFGHI");
      expect(data.key.revokedAt).toBeNull();
      expect(data.plaintextKey).toMatch(/^lk_/);
    });

    it("passes userId from session to db insert", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockReturning.mockResolvedValue([mockInsertedRow]);

      await createApiKey("Test Key");

      expect(mockValues).toHaveBeenCalledWith(
        expect.objectContaining({
          userId: "user-1",
          name: "Test Key",
        }),
      );
    });

    it("generates sha256 hash of the plaintext key", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockReturning.mockResolvedValue([mockInsertedRow]);

      await createApiKey("Test Key");

      expect(mockCreateHash).toHaveBeenCalledWith("sha256");
      // The hash is stored as keyHash in the values
      expect(mockValues).toHaveBeenCalledWith(
        expect.objectContaining({
          keyHash: expect.any(Buffer),
        }),
      );
    });

    it("trims whitespace from name", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockReturning.mockResolvedValue([mockInsertedRow]);

      await createApiKey("  My Key  ");

      expect(mockValues).toHaveBeenCalledWith(
        expect.objectContaining({
          name: "My Key",
        }),
      );
    });

    it("returns VALIDATION_ERROR for empty name", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);

      const result = await createApiKey("");

      expect(result).toEqual({
        error: { code: "VALIDATION_ERROR", message: expect.any(String) },
      });
      expect(mockInsert).not.toHaveBeenCalled();
    });

    it("returns VALIDATION_ERROR for whitespace-only name", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);

      const result = await createApiKey("   ");

      expect(result).toEqual({
        error: { code: "VALIDATION_ERROR", message: expect.any(String) },
      });
      expect(mockInsert).not.toHaveBeenCalled();
    });

    it("returns VALIDATION_ERROR for name exceeding 100 characters", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);

      const result = await createApiKey("a".repeat(101));

      expect(result).toEqual({
        error: { code: "VALIDATION_ERROR", message: expect.any(String) },
      });
      expect(mockInsert).not.toHaveBeenCalled();
    });

    it("accepts name of exactly 100 characters", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockReturning.mockResolvedValue([mockInsertedRow]);

      const result = await createApiKey("a".repeat(100));

      expect(result).toHaveProperty("data");
    });

    it("key prefix starts with lk_ followed by 9 characters", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockReturning.mockResolvedValue([mockInsertedRow]);

      await createApiKey("Test Key");

      const callArgs = mockValues.mock.calls[0][0];
      expect(callArgs.keyPrefix).toMatch(/^lk_.{9}$/);
    });
  });

  // --- listApiKeys ---

  describe("listApiKeys", () => {
    it("returns list of keys for the authenticated user", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const rows = [
        { id: "key-1", name: "Key 1", keyPrefix: "lk_ABC", createdAt: now, lastUsedAt: null, revokedAt: null },
        { id: "key-2", name: "Key 2", keyPrefix: "lk_DEF", createdAt: now, lastUsedAt: now, revokedAt: null },
      ];
      mockOrderBy.mockResolvedValue(rows);

      const result = await listApiKeys();

      expect(result).toEqual({ data: rows });
    });

    it("returns empty array when user has no keys", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockOrderBy.mockResolvedValue([]);

      const result = await listApiKeys();

      expect(result).toEqual({ data: [] });
    });

    it("calls db.select with expected column projections", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockOrderBy.mockResolvedValue([]);

      await listApiKeys();

      // select() is called with a column projection object
      expect(mockSelect).toHaveBeenCalledWith(
        expect.objectContaining({
          id: expect.anything(),
          name: expect.anything(),
          keyPrefix: expect.anything(),
          createdAt: expect.anything(),
          lastUsedAt: expect.anything(),
          revokedAt: expect.anything(),
        }),
      );
    });

    it("filters by authenticated user's ID", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockOrderBy.mockResolvedValue([]);

      await listApiKeys();

      // where() is called with eq(apiKeys.userId, session.user.id)
      expect(mockSelectWhere).toHaveBeenCalled();
    });
  });

  // --- revokeApiKey ---

  describe("revokeApiKey", () => {
    it("revokes key and returns updated key info", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const revokedRow = {
        id: "key-1",
        name: "My Key",
        keyPrefix: "lk_ABC",
        createdAt: now,
        lastUsedAt: null,
        revokedAt: new Date("2026-01-20T00:00:00Z"),
      };
      mockUpdateReturning.mockResolvedValue([revokedRow]);

      const result = await revokeApiKey("key-1");

      expect(result).toEqual({ data: revokedRow });
    });

    it("sets revokedAt to current timestamp", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockUpdateReturning.mockResolvedValue([mockInsertedRow]);

      await revokeApiKey("key-1");

      expect(mockUpdateSet).toHaveBeenCalledWith(
        expect.objectContaining({
          revokedAt: expect.any(Date),
        }),
      );
    });

    it("returns NOT_FOUND when key does not exist", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockUpdateReturning.mockResolvedValue([]);

      const result = await revokeApiKey("nonexistent-key");

      expect(result).toEqual({
        error: { code: "NOT_FOUND", message: expect.any(String) },
      });
    });

    it("returns NOT_FOUND when key is already revoked", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      // The WHERE clause includes isNull(revokedAt), so an already-revoked key
      // would return no rows from the UPDATE
      mockUpdateReturning.mockResolvedValue([]);

      const result = await revokeApiKey("already-revoked-key");

      expect(result).toEqual({
        error: { code: "NOT_FOUND", message: "API key not found or already revoked." },
      });
    });

    it("returns NOT_FOUND when key belongs to different user", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      // The WHERE clause includes eq(userId, session.user.id), so another
      // user's key would return no rows
      mockUpdateReturning.mockResolvedValue([]);

      const result = await revokeApiKey("other-users-key");

      expect(result).toEqual({
        error: { code: "NOT_FOUND", message: expect.any(String) },
      });
    });

    it("uses compound WHERE with keyId, userId, and isNull(revokedAt)", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockUpdateReturning.mockResolvedValue([mockInsertedRow]);

      await revokeApiKey("key-1");

      // update() called with apiKeys table
      expect(mockUpdate).toHaveBeenCalled();
      // set() called with revokedAt
      expect(mockUpdateSet).toHaveBeenCalled();
      // where() called (compound condition)
      expect(mockUpdateWhere).toHaveBeenCalled();
    });

    it("returns key info projection in returning clause", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const revokedRow = {
        id: "key-1",
        name: "My Key",
        keyPrefix: "lk_ABC",
        createdAt: now,
        lastUsedAt: null,
        revokedAt: new Date(),
      };
      mockUpdateReturning.mockResolvedValue([revokedRow]);

      const result = await revokeApiKey("key-1");

      const data = (result as { data: typeof revokedRow }).data;
      expect(data).toHaveProperty("id");
      expect(data).toHaveProperty("name");
      expect(data).toHaveProperty("keyPrefix");
      expect(data).toHaveProperty("createdAt");
      expect(data).toHaveProperty("lastUsedAt");
      expect(data).toHaveProperty("revokedAt");
    });
  });

  // --- Database error handling ---

  describe("database error handling", () => {
    it("createApiKey returns INTERNAL_ERROR on database error", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockReturning.mockRejectedValue(new Error("connection refused"));

      const result = await createApiKey("Test Key");
      expect(result).toEqual({
        error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
      });
    });

    it("listApiKeys returns INTERNAL_ERROR on database error", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockOrderBy.mockRejectedValue(new Error("connection refused"));

      const result = await listApiKeys();
      expect(result).toEqual({
        error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
      });
    });

    it("revokeApiKey returns INTERNAL_ERROR on database error", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockUpdateReturning.mockRejectedValue(new Error("connection refused"));

      const result = await revokeApiKey("key-1");
      expect(result).toEqual({
        error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
      });
    });

    it("createApiKey returns INTERNAL_ERROR on unique constraint violation", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const dbError = new Error("duplicate key value violates unique constraint");
      (dbError as unknown as Record<string, unknown>).code = "23505";
      mockReturning.mockRejectedValue(dbError);

      const result = await createApiKey("Test Key");
      expect(result).toEqual({
        error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
      });
    });
  });
});
