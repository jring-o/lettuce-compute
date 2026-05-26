import type { NextAuthConfig } from "next-auth";

/**
 * Edge-safe auth config — no Node.js-only imports (bcrypt, pg, etc.).
 * Used by middleware. The full auth.ts re-exports this with the
 * Credentials provider added.
 */
export const authConfig = {
  session: { strategy: "jwt" },
  pages: {
    signIn: "/sign-in",
  },
  callbacks: {
    jwt({ token, user }) {
      if (user) {
        token.id = user.id!;
        token.username = user.username;
        token.role = user.role;
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
