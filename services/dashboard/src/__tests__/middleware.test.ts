/**
 * @jest-environment node
 */

/**
 * Tests for the middleware configuration and redirect logic.
 *
 * The actual middleware (src/middleware.ts) does:
 *   const { auth } = NextAuth(authConfig);
 *   export default auth((req) => { ...redirect logic... });
 *
 * `next-auth` is an ESM-only package that jest cannot load untransformed, so we
 * mock it at the boundary (the same pattern used by auth/authorize.test.ts).
 * The mock's default export stands in for `NextAuth()` and returns an `auth`
 * wrapper that captures the callback the middleware passes to it. We then invoke
 * that real callback directly against mock requests to exercise the redirect /
 * allow logic with the real `next/server` `NextResponse`.
 */

// Minimal shape of the request the middleware callback reads off of.
type MockReq = {
  auth: { user: { id: string; username: string; role: string } } | null;
  nextUrl: { pathname: string; origin: string };
};
type MiddlewareCallback = (req: MockReq) => Response | undefined;

// We capture the callback passed to auth() so we can test it in isolation.
let capturedCallback: MiddlewareCallback | null = null;

jest.mock("next-auth", () => ({
  __esModule: true,
  default: () => ({
    // `auth` wraps the middleware callback; capture it and return it unchanged
    // so the module's default export is the bare callback we want to test.
    auth: (cb: MiddlewareCallback) => {
      capturedCallback = cb;
      return cb;
    },
  }),
}));

// Must import after mock setup
import { config } from "@/middleware";

// Helper to create a mock request
function mockRequest(
  pathname: string,
  authenticated: boolean,
): MockReq {
  return {
    auth: authenticated
      ? { user: { id: "1", username: "testuser", role: "USER" } }
      : null,
    nextUrl: {
      pathname,
      origin: "http://localhost:3000",
    },
  };
}

describe("middleware", () => {
  beforeAll(async () => {
    // Force the module to load so auth() is called and capturedCallback is set
    await import("@/middleware");
  });

  describe("matcher config", () => {
    it("matches /dashboard routes", () => {
      expect(config.matcher).toContain("/dashboard/:path*");
    });

    it("only gates /dashboard/:path* and nothing else", () => {
      // The matcher must be exactly the dashboard path so that no other
      // route (public pages, API, auth) is unintentionally gated.
      expect(config.matcher).toEqual(["/dashboard/:path*"]);
    });

    it("does not include public routes like /leafs", () => {
      const matchers = config.matcher as string[];
      const matchesProjects = matchers.some(
        (m) => m === "/leafs" || m === "/leafs/:path*",
      );
      expect(matchesProjects).toBe(false);
    });

    it("does not include auth routes", () => {
      const matchers = config.matcher as string[];
      const matchesAuth = matchers.some(
        (m) => m.includes("sign-in") || m.includes("sign-up"),
      );
      expect(matchesAuth).toBe(false);
    });
  });

  describe("redirect logic", () => {
    it("captures the auth callback", () => {
      expect(capturedCallback).not.toBeNull();
    });

    it("redirects unauthenticated users to /sign-in", () => {
      const req = mockRequest("/dashboard/leafs", false);
      const result = capturedCallback!(req);

      // NextResponse.redirect returns a Response with status 307
      expect(result).toBeDefined();
      expect(result!.status).toBe(307);

      const location = result!.headers.get("location");
      expect(location).toContain("/sign-in");
    });

    it("includes callbackUrl in redirect", () => {
      const req = mockRequest("/dashboard/leafs", false);
      const result = capturedCallback!(req);

      const location = result!.headers.get("location");
      expect(location).toContain("callbackUrl=%2Fdashboard%2Fleafs");
    });

    it("does not redirect authenticated users", () => {
      const req = mockRequest("/dashboard/leafs", true);
      const result = capturedCallback!(req);

      expect(result).toBeUndefined();
    });

    it("preserves the full path in callbackUrl", () => {
      const req = mockRequest("/dashboard/leafs/abc-123", false);
      const result = capturedCallback!(req);

      const location = result!.headers.get("location");
      expect(location).toContain(
        "callbackUrl=%2Fdashboard%2Fleafs%2Fabc-123",
      );
    });
  });
});
