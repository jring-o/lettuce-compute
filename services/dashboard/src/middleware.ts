import NextAuth from "next-auth";
import { NextResponse } from "next/server";
import { authConfig } from "@/lib/auth.config";

const { auth } = NextAuth(authConfig);

/**
 * Anonymous allowlist for the /api/* surface. EXACT-SEGMENT prefixes only:
 * a request path must be the prefix itself or continue with a "/" — never a
 * bare string prefix. A loose "/api/viz/" prefix would re-admit
 * "/api/viz/results" and reopen BG-07; this list names "/api/viz/bundle"
 * (public, origin-isolated author code) but NOT "/api/viz".
 */
const API_ANON_ALLOWLIST = ["/api/auth", "/api/viz/bundle"];

function isAllowlistedApi(pathname: string): boolean {
  return API_ANON_ALLOWLIST.some(
    (prefix) => pathname === prefix || pathname.startsWith(prefix + "/"),
  );
}

export default auth((req) => {
  const { pathname } = req.nextUrl;
  const isApi = pathname.startsWith("/api/");

  if (req.auth) return; // authenticated — allow

  // Anonymous from here down.
  if (isApi) {
    if (isAllowlistedApi(pathname)) return; // public API surface
    // Default-deny every other /api/* route with a 401 JSON body — API clients
    // must NOT receive the HTML /sign-in redirect meant for page routes.
    return NextResponse.json(
      { error: { code: "UNAUTHENTICATED", message: "You must be signed in." } },
      { status: 401 },
    );
  }

  // Page routes (/dashboard/*): redirect to sign-in, preserving the target.
  const signInUrl = new URL("/sign-in", req.nextUrl.origin);
  signInUrl.searchParams.set("callbackUrl", pathname);
  return NextResponse.redirect(signInUrl);
});

export const config = {
  // Authentication default-deny at the edge over both the dashboard pages and
  // the whole /api/* surface. Object-level authorization still lives in each
  // route/action (the middleware cannot know which object a request names).
  matcher: ["/dashboard/:path*", "/api/:path*"],
};
