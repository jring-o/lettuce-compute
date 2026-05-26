// --- Mocks ---

const mockAuth = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: () => mockAuth(),
}));

// Mock bcryptjs so hashing is deterministic and fast
const mockHash = jest.fn();
jest.mock("bcryptjs", () => ({
  hash: (...args: unknown[]) => mockHash(...args),
}));

// Drizzle query chain mocks
const mockInsertReturning = jest.fn();
const mockInsertValues = jest
  .fn()
  .mockReturnValue({ returning: mockInsertReturning });
const mockInsert = jest.fn().mockReturnValue({ values: mockInsertValues });

const mockSelectOrderBy = jest.fn();
const mockSelectFrom = jest
  .fn()
  .mockReturnValue({ orderBy: mockSelectOrderBy });
const mockSelect = jest.fn().mockReturnValue({ from: mockSelectFrom });

const mockUpdateReturning = jest.fn();
const mockUpdateWhere = jest
  .fn()
  .mockReturnValue({ returning: mockUpdateReturning });
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
  users: {
    id: "id_col",
    username: "username_col",
    email: "email_col",
    displayName: "display_name_col",
    passwordHash: "password_hash_col",
    role: "role_col",
    createdAt: "created_at_col",
    updatedAt: "updated_at_col",
    deactivatedAt: "deactivated_at_col",
  },
}));

jest.mock("drizzle-orm", () => ({
  eq: jest.fn((...args: unknown[]) => ({ type: "eq", args })),
}));

import {
  createUser,
  listUsers,
  deactivateUser,
  reactivateUser,
  updateUserRole,
  resetUserPassword,
} from "@/lib/actions/admin";

// --- Fixtures ---

const adminSession = {
  user: { id: "admin-1", username: "admin", role: "ADMIN" },
};

const userSession = {
  user: { id: "user-1", username: "alice", role: "USER" },
};

const now = new Date("2026-03-15T12:00:00Z");

const validCreateUserData = {
  username: "newuser",
  email: "newuser@example.com",
  password: "securepassword123",
  role: "USER" as const,
};

// --- Helpers ---

function resetChainMocks() {
  mockInsert.mockReturnValue({ values: mockInsertValues });
  mockInsertValues.mockReturnValue({ returning: mockInsertReturning });

  mockSelect.mockReturnValue({ from: mockSelectFrom });
  mockSelectFrom.mockReturnValue({ orderBy: mockSelectOrderBy });

  mockUpdate.mockReturnValue({ set: mockUpdateSet });
  mockUpdateSet.mockReturnValue({ where: mockUpdateWhere });
  mockUpdateWhere.mockReturnValue({ returning: mockUpdateReturning });
}

beforeEach(() => {
  jest.clearAllMocks();
  resetChainMocks();
  mockHash.mockResolvedValue("hashed_password");
});

// --- Tests ---

describe("Admin Server Actions", () => {
  // --- Authentication & Authorization ---

  describe("authentication and authorization", () => {
    const actions = [
      { name: "createUser", fn: () => createUser(validCreateUserData) },
      { name: "listUsers", fn: () => listUsers() },
      { name: "deactivateUser", fn: () => deactivateUser("target-user") },
      { name: "reactivateUser", fn: () => reactivateUser("target-user") },
      {
        name: "updateUserRole",
        fn: () => updateUserRole("target-user", "ADMIN"),
      },
      {
        name: "resetUserPassword",
        fn: () => resetUserPassword("target-user", "newpassword123"),
      },
    ];

    describe.each(actions)("$name", ({ fn }) => {
      it("returns FORBIDDEN when not signed in", async () => {
        mockAuth.mockResolvedValue(null);

        const result = await fn();

        expect(result).toEqual({
          error: { code: "FORBIDDEN", message: "Admin access required." },
        });
      });

      it("returns FORBIDDEN when session has no user id", async () => {
        mockAuth.mockResolvedValue({ user: {} });

        const result = await fn();

        expect(result).toEqual({
          error: { code: "FORBIDDEN", message: "Admin access required." },
        });
      });

      it("returns FORBIDDEN for non-admin users", async () => {
        mockAuth.mockResolvedValue(userSession);

        const result = await fn();

        expect(result).toEqual({
          error: { code: "FORBIDDEN", message: "Admin access required." },
        });
      });
    });
  });

  // --- createUser ---

  describe("createUser", () => {
    it("creates user and returns id + username", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockInsertReturning.mockResolvedValue([
        { id: "new-user-id", username: "newuser" },
      ]);

      const result = await createUser(validCreateUserData);

      expect(result).toEqual({
        data: { id: "new-user-id", username: "newuser" },
      });
    });

    it("hashes the password with bcrypt", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockInsertReturning.mockResolvedValue([
        { id: "new-user-id", username: "newuser" },
      ]);

      await createUser(validCreateUserData);

      expect(mockHash).toHaveBeenCalledWith("securepassword123", 10);
    });

    it("passes correct values to db insert", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockInsertReturning.mockResolvedValue([
        { id: "new-user-id", username: "newuser" },
      ]);

      await createUser({
        ...validCreateUserData,
        displayName: "New User",
      });

      expect(mockInsertValues).toHaveBeenCalledWith(
        expect.objectContaining({
          email: "newuser@example.com",
          passwordHash: "hashed_password",
          username: "newuser",
          displayName: "New User",
          role: "USER",
        }),
      );
    });

    it("lowercases email before insert", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockInsertReturning.mockResolvedValue([
        { id: "new-user-id", username: "newuser" },
      ]);

      await createUser({
        ...validCreateUserData,
        email: "NewUser@EXAMPLE.COM",
      });

      expect(mockInsertValues).toHaveBeenCalledWith(
        expect.objectContaining({
          email: "newuser@example.com",
        }),
      );
    });

    it("sets displayName to null when not provided", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockInsertReturning.mockResolvedValue([
        { id: "new-user-id", username: "newuser" },
      ]);

      await createUser(validCreateUserData);

      expect(mockInsertValues).toHaveBeenCalledWith(
        expect.objectContaining({
          displayName: null,
        }),
      );
    });

    it("allows creating admin users", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockInsertReturning.mockResolvedValue([
        { id: "new-admin-id", username: "newadmin" },
      ]);

      const result = await createUser({
        ...validCreateUserData,
        username: "newadmin",
        role: "ADMIN",
      });

      expect(result).toEqual({
        data: { id: "new-admin-id", username: "newadmin" },
      });
      expect(mockInsertValues).toHaveBeenCalledWith(
        expect.objectContaining({ role: "ADMIN" }),
      );
    });

    // --- Validation ---

    describe("validation", () => {
      it("rejects username shorter than 3 characters", async () => {
        mockAuth.mockResolvedValue(adminSession);

        const result = await createUser({
          ...validCreateUserData,
          username: "ab",
        });

        expect(result).toEqual({
          error: { code: "VALIDATION_ERROR", message: expect.any(String) },
        });
        expect(mockInsert).not.toHaveBeenCalled();
      });

      it("rejects username longer than 50 characters", async () => {
        mockAuth.mockResolvedValue(adminSession);

        const result = await createUser({
          ...validCreateUserData,
          username: "a".repeat(51),
        });

        expect(result).toEqual({
          error: { code: "VALIDATION_ERROR", message: expect.any(String) },
        });
        expect(mockInsert).not.toHaveBeenCalled();
      });

      it("rejects username starting with a number", async () => {
        mockAuth.mockResolvedValue(adminSession);

        const result = await createUser({
          ...validCreateUserData,
          username: "1baduser",
        });

        expect(result).toEqual({
          error: { code: "VALIDATION_ERROR", message: expect.any(String) },
        });
        expect(mockInsert).not.toHaveBeenCalled();
      });

      it("rejects username with uppercase letters", async () => {
        mockAuth.mockResolvedValue(adminSession);

        const result = await createUser({
          ...validCreateUserData,
          username: "BadUser",
        });

        expect(result).toEqual({
          error: { code: "VALIDATION_ERROR", message: expect.any(String) },
        });
        expect(mockInsert).not.toHaveBeenCalled();
      });

      it("rejects username with consecutive hyphens", async () => {
        mockAuth.mockResolvedValue(adminSession);

        const result = await createUser({
          ...validCreateUserData,
          username: "bad--user",
        });

        expect(result).toEqual({
          error: { code: "VALIDATION_ERROR", message: expect.any(String) },
        });
        expect(mockInsert).not.toHaveBeenCalled();
      });

      it("rejects username ending with a hyphen", async () => {
        mockAuth.mockResolvedValue(adminSession);

        const result = await createUser({
          ...validCreateUserData,
          username: "baduser-",
        });

        expect(result).toEqual({
          error: { code: "VALIDATION_ERROR", message: expect.any(String) },
        });
        expect(mockInsert).not.toHaveBeenCalled();
      });

      it("accepts valid username with hyphens and numbers", async () => {
        mockAuth.mockResolvedValue(adminSession);
        mockInsertReturning.mockResolvedValue([
          { id: "id", username: "good-user-1" },
        ]);

        const result = await createUser({
          ...validCreateUserData,
          username: "good-user-1",
        });

        expect(result).toHaveProperty("data");
      });

      it("rejects invalid email", async () => {
        mockAuth.mockResolvedValue(adminSession);

        const result = await createUser({
          ...validCreateUserData,
          email: "not-an-email",
        });

        expect(result).toEqual({
          error: { code: "VALIDATION_ERROR", message: expect.any(String) },
        });
        expect(mockInsert).not.toHaveBeenCalled();
      });

      it("rejects empty email", async () => {
        mockAuth.mockResolvedValue(adminSession);

        const result = await createUser({
          ...validCreateUserData,
          email: "",
        });

        expect(result).toEqual({
          error: { code: "VALIDATION_ERROR", message: expect.any(String) },
        });
        expect(mockInsert).not.toHaveBeenCalled();
      });

      it("rejects password shorter than 8 characters", async () => {
        mockAuth.mockResolvedValue(adminSession);

        const result = await createUser({
          ...validCreateUserData,
          password: "short",
        });

        expect(result).toEqual({
          error: { code: "VALIDATION_ERROR", message: expect.any(String) },
        });
        expect(mockInsert).not.toHaveBeenCalled();
      });

      it("accepts password of exactly 8 characters", async () => {
        mockAuth.mockResolvedValue(adminSession);
        mockInsertReturning.mockResolvedValue([
          { id: "id", username: "newuser" },
        ]);

        const result = await createUser({
          ...validCreateUserData,
          password: "12345678",
        });

        expect(result).toHaveProperty("data");
      });
    });

    // --- Conflict handling ---

    describe("conflict handling", () => {
      it("returns CONFLICT when email already exists", async () => {
        mockAuth.mockResolvedValue(adminSession);
        const dbError = new Error(
          "duplicate key value violates unique constraint on email",
        );
        mockInsertReturning.mockRejectedValue(dbError);

        const result = await createUser(validCreateUserData);

        expect(result).toEqual({
          error: {
            code: "CONFLICT",
            message: "A user with this email already exists.",
          },
        });
      });

      it("returns CONFLICT when username already exists", async () => {
        mockAuth.mockResolvedValue(adminSession);
        const dbError = new Error(
          "duplicate key value violates unique constraint on username",
        );
        mockInsertReturning.mockRejectedValue(dbError);

        const result = await createUser(validCreateUserData);

        expect(result).toEqual({
          error: {
            code: "CONFLICT",
            message: "This username is already taken.",
          },
        });
      });

      it("returns generic CONFLICT for unspecified unique constraint", async () => {
        mockAuth.mockResolvedValue(adminSession);
        const dbError = new Error(
          "duplicate key value violates unique constraint",
        );
        mockInsertReturning.mockRejectedValue(dbError);

        const result = await createUser(validCreateUserData);

        expect(result).toEqual({
          error: {
            code: "CONFLICT",
            message:
              "A user with this email or username already exists.",
          },
        });
      });

      it("returns INTERNAL_ERROR for non-unique database errors", async () => {
        mockAuth.mockResolvedValue(adminSession);
        mockInsertReturning.mockRejectedValue(
          new Error("connection refused"),
        );

        const result = await createUser(validCreateUserData);

        expect(result).toEqual({
          error: {
            code: "INTERNAL_ERROR",
            message: "An unexpected error occurred.",
          },
        });
      });
    });
  });

  // --- listUsers ---

  describe("listUsers", () => {
    it("returns list of all users", async () => {
      mockAuth.mockResolvedValue(adminSession);
      const rows = [
        {
          id: "user-1",
          username: "alice",
          email: "alice@example.com",
          displayName: "Alice",
          role: "USER",
          createdAt: now,
          deactivatedAt: null,
        },
        {
          id: "admin-1",
          username: "admin",
          email: "admin@example.com",
          displayName: null,
          role: "ADMIN",
          createdAt: now,
          deactivatedAt: null,
        },
      ];
      mockSelectOrderBy.mockResolvedValue(rows);

      const result = await listUsers();

      expect(result).toEqual({ data: rows });
    });

    it("returns empty array when no users exist", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockSelectOrderBy.mockResolvedValue([]);

      const result = await listUsers();

      expect(result).toEqual({ data: [] });
    });

    it("calls db.select with expected column projections", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockSelectOrderBy.mockResolvedValue([]);

      await listUsers();

      expect(mockSelect).toHaveBeenCalledWith(
        expect.objectContaining({
          id: expect.anything(),
          username: expect.anything(),
          email: expect.anything(),
          displayName: expect.anything(),
          role: expect.anything(),
          createdAt: expect.anything(),
          deactivatedAt: expect.anything(),
        }),
      );
    });

    it("includes deactivated users in the list", async () => {
      mockAuth.mockResolvedValue(adminSession);
      const rows = [
        {
          id: "user-1",
          username: "active",
          email: "active@example.com",
          displayName: null,
          role: "USER",
          createdAt: now,
          deactivatedAt: null,
        },
        {
          id: "user-2",
          username: "deactivated",
          email: "deactivated@example.com",
          displayName: null,
          role: "USER",
          createdAt: now,
          deactivatedAt: new Date("2026-03-10T00:00:00Z"),
        },
      ];
      mockSelectOrderBy.mockResolvedValue(rows);

      const result = await listUsers();

      expect(result).toEqual({ data: rows });
      expect(
        (result as { data: typeof rows }).data[1].deactivatedAt,
      ).not.toBeNull();
    });
  });

  // --- deactivateUser ---

  describe("deactivateUser", () => {
    it("deactivates a user and returns success", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([{ id: "target-user" }]);

      const result = await deactivateUser("target-user");

      expect(result).toEqual({ data: undefined });
    });

    it("sets deactivatedAt to current date", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([{ id: "target-user" }]);

      await deactivateUser("target-user");

      expect(mockUpdateSet).toHaveBeenCalledWith(
        expect.objectContaining({
          deactivatedAt: expect.any(Date),
        }),
      );
    });

    it("prevents admin from deactivating themselves", async () => {
      mockAuth.mockResolvedValue(adminSession);

      const result = await deactivateUser("admin-1");

      expect(result).toEqual({
        error: {
          code: "FORBIDDEN",
          message: "You cannot deactivate your own account.",
        },
      });
      expect(mockUpdate).not.toHaveBeenCalled();
    });

    it("returns NOT_FOUND when user does not exist", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([]);

      const result = await deactivateUser("nonexistent-user");

      expect(result).toEqual({
        error: { code: "NOT_FOUND", message: "User not found." },
      });
    });
  });

  // --- reactivateUser ---

  describe("reactivateUser", () => {
    it("reactivates a user and returns success", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([{ id: "target-user" }]);

      const result = await reactivateUser("target-user");

      expect(result).toEqual({ data: undefined });
    });

    it("sets deactivatedAt to null", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([{ id: "target-user" }]);

      await reactivateUser("target-user");

      expect(mockUpdateSet).toHaveBeenCalledWith(
        expect.objectContaining({
          deactivatedAt: null,
        }),
      );
    });

    it("returns NOT_FOUND when user does not exist", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([]);

      const result = await reactivateUser("nonexistent-user");

      expect(result).toEqual({
        error: { code: "NOT_FOUND", message: "User not found." },
      });
    });
  });

  // --- updateUserRole ---

  describe("updateUserRole", () => {
    it("updates role and returns success", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([{ id: "target-user" }]);

      const result = await updateUserRole("target-user", "ADMIN");

      expect(result).toEqual({ data: undefined });
    });

    it("passes the new role to db update", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([{ id: "target-user" }]);

      await updateUserRole("target-user", "ADMIN");

      expect(mockUpdateSet).toHaveBeenCalledWith(
        expect.objectContaining({
          role: "ADMIN",
        }),
      );
    });

    it("prevents admin from changing their own role", async () => {
      mockAuth.mockResolvedValue(adminSession);

      const result = await updateUserRole("admin-1", "USER");

      expect(result).toEqual({
        error: {
          code: "FORBIDDEN",
          message: "You cannot change your own role.",
        },
      });
      expect(mockUpdate).not.toHaveBeenCalled();
    });

    it("returns NOT_FOUND when user does not exist", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([]);

      const result = await updateUserRole("nonexistent-user", "ADMIN");

      expect(result).toEqual({
        error: { code: "NOT_FOUND", message: "User not found." },
      });
    });

    it("can downgrade admin to user", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([{ id: "other-admin" }]);

      const result = await updateUserRole("other-admin", "USER");

      expect(result).toEqual({ data: undefined });
      expect(mockUpdateSet).toHaveBeenCalledWith(
        expect.objectContaining({ role: "USER" }),
      );
    });
  });

  // --- resetUserPassword ---

  describe("resetUserPassword", () => {
    it("resets password and returns success", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([{ id: "target-user" }]);

      const result = await resetUserPassword(
        "target-user",
        "newpassword123",
      );

      expect(result).toEqual({ data: undefined });
    });

    it("hashes the new password with bcrypt", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([{ id: "target-user" }]);

      await resetUserPassword("target-user", "newpassword123");

      expect(mockHash).toHaveBeenCalledWith("newpassword123", 10);
    });

    it("passes hashed password to db update", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockHash.mockResolvedValue("new_hashed_password");
      mockUpdateReturning.mockResolvedValue([{ id: "target-user" }]);

      await resetUserPassword("target-user", "newpassword123");

      expect(mockUpdateSet).toHaveBeenCalledWith(
        expect.objectContaining({
          passwordHash: "new_hashed_password",
        }),
      );
    });

    it("rejects password shorter than 8 characters", async () => {
      mockAuth.mockResolvedValue(adminSession);

      const result = await resetUserPassword("target-user", "short");

      expect(result).toEqual({
        error: {
          code: "VALIDATION_ERROR",
          message: "Password must be at least 8 characters.",
        },
      });
      expect(mockHash).not.toHaveBeenCalled();
      expect(mockUpdate).not.toHaveBeenCalled();
    });

    it("accepts password of exactly 8 characters", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([{ id: "target-user" }]);

      const result = await resetUserPassword("target-user", "12345678");

      expect(result).toEqual({ data: undefined });
    });

    it("returns NOT_FOUND when user does not exist", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockUpdateReturning.mockResolvedValue([]);

      const result = await resetUserPassword(
        "nonexistent-user",
        "newpassword123",
      );

      expect(result).toEqual({
        error: { code: "NOT_FOUND", message: "User not found." },
      });
    });
  });
});
