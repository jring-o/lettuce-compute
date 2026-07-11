import type { NextAuthConfig } from "next-auth";

/**
 * Edge-safe auth config — no Node.js-only imports (bcrypt, pg, etc.).
 * Used by middleware. The full auth.ts re-exports this with the
 * Credentials provider added.
 */
export const authConfig = {
  session: {
    strategy: "jwt",
    // Backstop for BG-09: bound the lifetime of any token even if the Node
    // jwt re-validation (auth.ts) is ever bypassed. maxAge alone does NOT
    // re-check account standing (a re-minted JWT never re-runs authorize), so
    // it is a backstop, not the revocation mechanism.
    maxAge: 7 * 24 * 60 * 60, // 7 days
  },
  pages: {
    signIn: "/sign-in",
  },
  callbacks: {
    // Edge-safe base jwt callback: copies identity at sign-in only. It carries
    // NO database access (this config runs in the edge middleware). The
    // per-request DB re-validation (deactivatedAt / role / token_version) that
    // actually enforces revocation is an OVERRIDING jwt callback in the Node
    // config (auth.ts, R1.4), which runs wherever the full auth() runs: server
    // actions, /api/* route handlers, and server components.
    jwt({ token, user }) {
      if (user) {
        token.id = user.id!;
        token.username = user.username;
        token.role = user.role;
        token.tokenVersion = user.tokenVersion ?? 0;
      }
      return token;
    },
    session({ session, token }) {
      const user = session.user as unknown as {
        id: string;
        username: string;
        role: string;
      };
      user.id = token.id;
      user.username = token.username;
      user.role = token.role;
      return session;
    },
  },
  providers: [], // populated in auth.ts with Credentials
} satisfies NextAuthConfig;
