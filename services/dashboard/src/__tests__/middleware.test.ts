/**
 * @jest-environment node
 */

/**
 * Tests for the middleware configuration and its default-deny / redirect logic.
 *
 * The actual middleware (src/middleware.ts) does:
 *   const { auth } = NextAuth(authConfig);
 *   export default auth((req) => { ...default-deny + redirect logic... });
 *
 * `next-auth` is an ESM-only package that jest cannot load untransformed, so we
 * mock it at the boundary (the same pattern used by auth/authorize.test.ts).
 * The mock's default export stands in for `NextAuth()` and returns an `auth`
 * wrapper that captures the callback the middleware passes to it. We then invoke
 * that real callback directly against mock requests to exercise the logic with
 * the real `next/server` `NextResponse`.
 */

type MockReq = {
  auth: { user: { id: string; username: string; role: string } } | null;
  nextUrl: { pathname: string; origin: string };
};
type MiddlewareCallback = (req: MockReq) => Response | undefined;

let capturedCallback: MiddlewareCallback | null = null;

jest.mock("next-auth", () => ({
  __esModule: true,
  default: () => ({
    auth: (cb: MiddlewareCallback) => {
      capturedCallback = cb;
      return cb;
    },
  }),
}));

// Must import after mock setup
import { config } from "@/middleware";

function mockRequest(pathname: string, authenticated: boolean): MockReq {
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
    it("gates the dashboard pages", () => {
      expect(config.matcher).toContain("/dashboard/:path*");
    });

    it("gates the whole /api/* surface (default-deny authentication)", () => {
      expect(config.matcher).toContain("/api/:path*");
    });

    it("gates exactly the dashboard pages and the API surface", () => {
      expect(config.matcher).toEqual(["/dashboard/:path*", "/api/:path*"]);
    });

    it("does not include public page routes like /leafs", () => {
      const matchers = config.matcher as string[];
      const matchesProjects = matchers.some(
        (m) => m === "/leafs" || m === "/leafs/:path*",
      );
      expect(matchesProjects).toBe(false);
    });
  });

  describe("dashboard page redirect logic", () => {
    it("captures the auth callback", () => {
      expect(capturedCallback).not.toBeNull();
    });

    it("redirects unauthenticated users to /sign-in", () => {
      const req = mockRequest("/dashboard/leafs", false);
      const result = capturedCallback!(req);

      expect(result).toBeDefined();
      expect(result!.status).toBe(307);
      expect(result!.headers.get("location")).toContain("/sign-in");
    });

    it("includes callbackUrl in redirect", () => {
      const req = mockRequest("/dashboard/leafs", false);
      const result = capturedCallback!(req);

      expect(result!.headers.get("location")).toContain(
        "callbackUrl=%2Fdashboard%2Fleafs",
      );
    });

    it("does not redirect authenticated users", () => {
      const req = mockRequest("/dashboard/leafs", true);
      const result = capturedCallback!(req);

      expect(result).toBeUndefined();
    });

    it("preserves the full path in callbackUrl", () => {
      const req = mockRequest("/dashboard/leafs/abc-123", false);
      const result = capturedCallback!(req);

      expect(result!.headers.get("location")).toContain(
        "callbackUrl=%2Fdashboard%2Fleafs%2Fabc-123",
      );
    });
  });

  describe("/api/* default-deny", () => {
    it("returns a 401 (not a redirect) for an anonymous data API call", () => {
      const req = mockRequest("/api/viz/results", false);
      const result = capturedCallback!(req);

      expect(result).toBeDefined();
      expect(result!.status).toBe(401);
      // Must NOT be the HTML sign-in redirect meant for page routes.
      expect(result!.headers.get("location")).toBeNull();
    });

    it("allows an authenticated /api/* call through", () => {
      const req = mockRequest("/api/viz/results", true);
      expect(capturedCallback!(req)).toBeUndefined();
    });

    it("allowlists /api/auth/* (sign-in handshake) anonymously", () => {
      const req = mockRequest("/api/auth/session", false);
      expect(capturedCallback!(req)).toBeUndefined();
    });

    it("allowlists /api/viz/bundle/* (public author bundle) anonymously", () => {
      const req = mockRequest("/api/viz/bundle/abc/index.html", false);
      expect(capturedCallback!(req)).toBeUndefined();
    });

    it("does NOT allowlist /api/viz/results via a loose /api/viz prefix (BG-07)", () => {
      const req = mockRequest("/api/viz/results", false);
      const result = capturedCallback!(req);
      expect(result!.status).toBe(401);
    });

    it("does NOT treat a /api/viz/bundle-lookalike segment as allowlisted", () => {
      // "/api/viz/bundlexyz" shares the string prefix but not the segment.
      const req = mockRequest("/api/viz/bundlexyz", false);
      const result = capturedCallback!(req);
      expect(result!.status).toBe(401);
    });

    it("denies an anonymous upload", () => {
      const req = mockRequest("/api/upload", false);
      const result = capturedCallback!(req);
      expect(result!.status).toBe(401);
    });
  });
});
