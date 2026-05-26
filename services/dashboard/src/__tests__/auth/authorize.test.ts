/**
 * Tests for the Credentials authorize callback in auth.ts.
 * Verifies that deactivated users cannot sign in.
 */

const mockSelect = jest.fn();
const mockFrom = jest.fn();
const mockWhere = jest.fn();
const mockLimit = jest.fn();
const mockCompare = jest.fn();

jest.mock("@/lib/db", () => ({
  db: {
    select: (...args: unknown[]) => {
      mockSelect(...args);
      return { from: mockFrom };
    },
  },
}));

jest.mock("@/lib/db/schema", () => ({
  users: { email: "email_col" },
}));

jest.mock("drizzle-orm", () => ({
  eq: (col: unknown, val: unknown) => ({ col, val }),
}));

jest.mock("bcryptjs", () => ({
  compare: (...args: unknown[]) => mockCompare(...args),
}));

jest.mock("./../../lib/auth.config", () => ({
  authConfig: {
    pages: { signIn: "/sign-in" },
    callbacks: {},
  },
}));

// Capture the authorize function from NextAuth Credentials provider
let capturedAuthorize: (credentials: Record<string, unknown>) => Promise<unknown>;

jest.mock("next-auth", () => {
  return {
    __esModule: true,
    default: (config: { providers: Array<{ options: { authorize: typeof capturedAuthorize } }> }) => {
      capturedAuthorize = config.providers[0].options.authorize;
      return { handlers: {}, signIn: jest.fn(), signOut: jest.fn(), auth: jest.fn() };
    },
  };
});

jest.mock("next-auth/providers/credentials", () => {
  return {
    __esModule: true,
    default: (opts: { authorize: typeof capturedAuthorize }) => ({ options: opts }),
  };
});

// Import triggers NextAuth() which captures authorize
require("@/lib/auth");

function setupDbReturn(user: Record<string, unknown> | undefined) {
  mockFrom.mockReturnValue({ where: mockWhere });
  mockWhere.mockReturnValue({ limit: mockLimit });
  mockLimit.mockResolvedValue(user ? [user] : []);
}

describe("authorize callback", () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  it("rejects deactivated users even with valid credentials", async () => {
    setupDbReturn({
      id: "user-1",
      email: "deactivated@example.com",
      passwordHash: "hashed",
      deactivatedAt: new Date("2026-03-20"),
      username: "deactivated",
      displayName: "Deactivated User",
      role: "USER",
    });
    // bcrypt.compare should NOT be called — deactivation check comes first
    mockCompare.mockResolvedValue(true);

    const result = await capturedAuthorize({
      email: "deactivated@example.com",
      password: "validpassword123",
    });

    expect(result).toBeNull();
    // Verify bcrypt was never called — the deactivation check short-circuits
    expect(mockCompare).not.toHaveBeenCalled();
  });

  it("allows active users with valid credentials", async () => {
    setupDbReturn({
      id: "user-2",
      email: "active@example.com",
      passwordHash: "$2a$10$hashed",
      deactivatedAt: null,
      username: "active",
      displayName: "Active User",
      role: "ADMIN",
    });
    mockCompare.mockResolvedValue(true);

    const result = await capturedAuthorize({
      email: "active@example.com",
      password: "validpassword123",
    });

    expect(result).toEqual({
      id: "user-2",
      email: "active@example.com",
      name: "Active User",
      username: "active",
      role: "ADMIN",
    });
  });

  it("rejects users with no password hash", async () => {
    setupDbReturn({
      id: "user-3",
      email: "nohash@example.com",
      passwordHash: null,
      deactivatedAt: null,
      username: "nohash",
    });

    const result = await capturedAuthorize({
      email: "nohash@example.com",
      password: "anypassword123",
    });

    expect(result).toBeNull();
  });

  it("rejects non-existent users", async () => {
    setupDbReturn(undefined);

    const result = await capturedAuthorize({
      email: "nobody@example.com",
      password: "anypassword123",
    });

    expect(result).toBeNull();
  });

  it("rejects invalid credentials", async () => {
    setupDbReturn({
      id: "user-4",
      email: "user@example.com",
      passwordHash: "$2a$10$hashed",
      deactivatedAt: null,
      username: "user",
      displayName: null,
    });
    mockCompare.mockResolvedValue(false);

    const result = await capturedAuthorize({
      email: "user@example.com",
      password: "wrongpassword123",
    });

    expect(result).toBeNull();
  });

  it("uses username as name when displayName is null", async () => {
    setupDbReturn({
      id: "user-5",
      email: "nodisplay@example.com",
      passwordHash: "$2a$10$hashed",
      deactivatedAt: null,
      username: "nodisplay",
      displayName: null,
      role: "USER",
    });
    mockCompare.mockResolvedValue(true);

    const result = await capturedAuthorize({
      email: "nodisplay@example.com",
      password: "validpassword123",
    });

    expect(result).toEqual(
      expect.objectContaining({ name: "nodisplay" }),
    );
  });
});
