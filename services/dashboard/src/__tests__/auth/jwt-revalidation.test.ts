// Mark this file a module (not a global script) so its top-level mock consts
// don't collide with authorize.test.ts under a whole-project tsc.
export {};

/**
 * BG-09 (R1.4): the Node-only jwt callback in auth.ts re-reads the user from
 * the DB every request and revokes the token when the account is deactivated /
 * gone, its token_version was bumped (password reset / "sign out everywhere"),
 * and refreshes a changed role (demotion). This exercises that callback in
 * isolation, the same capture pattern as authorize.test.ts.
 */

const mockSelect = jest.fn();
const mockFrom = jest.fn();
const mockWhere = jest.fn();
const mockLimit = jest.fn();

jest.mock("@/lib/db", () => ({
  db: {
    select: (...args: unknown[]) => {
      mockSelect(...args);
      return { from: mockFrom };
    },
  },
}));

jest.mock("@/lib/db/schema", () => ({
  users: {
    id: "id_col",
    email: "email_col",
    role: "role_col",
    deactivatedAt: "deactivated_col",
    tokenVersion: "token_version_col",
  },
}));

jest.mock("drizzle-orm", () => ({
  eq: (col: unknown, val: unknown) => ({ col, val }),
}));

jest.mock("bcryptjs", () => ({ compare: jest.fn() }));

// A real base jwt callback (edge config): copies identity at sign-in.
jest.mock("./../../lib/auth.config", () => ({
  authConfig: {
    pages: { signIn: "/sign-in" },
    session: { strategy: "jwt", maxAge: 604800 },
    callbacks: {
      jwt: ({
        token,
        user,
      }: {
        token: Record<string, unknown>;
        user?: Record<string, unknown>;
      }) => {
        if (user) {
          token.id = user.id;
          token.username = user.username;
          token.role = user.role;
          token.tokenVersion = user.tokenVersion ?? 0;
        }
        return token;
      },
    },
  },
}));

let capturedJwt: (params: {
  token: Record<string, unknown>;
  user?: Record<string, unknown>;
}) => Promise<Record<string, unknown> | null>;

jest.mock("next-auth", () => ({
  __esModule: true,
  default: (config: {
    callbacks: { jwt: typeof capturedJwt };
  }) => {
    capturedJwt = config.callbacks.jwt;
    return { handlers: {}, signIn: jest.fn(), signOut: jest.fn(), auth: jest.fn() };
  },
}));

jest.mock("next-auth/providers/credentials", () => ({
  __esModule: true,
  default: (opts: unknown) => ({ options: opts }),
}));

// eslint-disable-next-line @typescript-eslint/no-require-imports
require("@/lib/auth");

function dbReturns(row: Record<string, unknown> | undefined) {
  mockFrom.mockReturnValue({ where: mockWhere });
  mockWhere.mockReturnValue({ limit: mockLimit });
  mockLimit.mockResolvedValue(row ? [row] : []);
}

describe("Node jwt re-validation (BG-09)", () => {
  beforeEach(() => jest.clearAllMocks());

  it("keeps an active, unchanged token and refreshes role from the DB", async () => {
    dbReturns({ role: "USER", deactivatedAt: null, tokenVersion: 0 });
    const token = { id: "user-1", role: "USER", tokenVersion: 0 };

    const result = await capturedJwt({ token });
    expect(result).not.toBeNull();
    expect(result!.role).toBe("USER");
  });

  it("revokes (null) when the account is deactivated", async () => {
    dbReturns({ role: "USER", deactivatedAt: new Date("2026-05-01"), tokenVersion: 0 });
    const result = await capturedJwt({ token: { id: "user-1", role: "USER", tokenVersion: 0 } });
    expect(result).toBeNull();
  });

  it("revokes (null) when the account no longer exists", async () => {
    dbReturns(undefined);
    const result = await capturedJwt({ token: { id: "ghost", role: "USER", tokenVersion: 0 } });
    expect(result).toBeNull();
  });

  it("revokes (null) when token_version was bumped (password reset / sign-out-everywhere)", async () => {
    dbReturns({ role: "USER", deactivatedAt: null, tokenVersion: 3 });
    const result = await capturedJwt({ token: { id: "user-1", role: "USER", tokenVersion: 2 } });
    expect(result).toBeNull();
  });

  it("downgrades a stale ADMIN token to the current USER role", async () => {
    dbReturns({ role: "USER", deactivatedAt: null, tokenVersion: 0 });
    const token = { id: "user-1", role: "ADMIN", tokenVersion: 0 };

    const result = await capturedJwt({ token });
    expect(result).not.toBeNull();
    expect(result!.role).toBe("USER");
  });

  it("treats a missing stored token_version as 0 (legacy rows)", async () => {
    dbReturns({ role: "USER", deactivatedAt: null, tokenVersion: undefined });
    const result = await capturedJwt({ token: { id: "user-1", role: "USER", tokenVersion: 0 } });
    expect(result).not.toBeNull();
  });

  it("does not hit the DB when the token has no id", async () => {
    const result = await capturedJwt({ token: {} });
    expect(result).toEqual({});
    expect(mockSelect).not.toHaveBeenCalled();
  });
});
