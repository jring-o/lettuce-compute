import NextAuth from "next-auth";
import Credentials from "next-auth/providers/credentials";
import bcrypt from "bcryptjs";
import { eq } from "drizzle-orm";
import { db } from "@/lib/db";
import { users } from "@/lib/db/schema";
import { signInSchema } from "@/lib/validations/auth";
import { authConfig } from "./auth.config";

export const { handlers, signIn, signOut, auth } = NextAuth({
  ...authConfig,
  callbacks: {
    ...authConfig.callbacks,
    /**
     * Node-only jwt callback (BG-09, R1.4). This OVERRIDES the edge-safe base
     * callback in auth.config.ts, so it runs wherever the full Node auth()
     * runs — server actions, /api/* route handlers, and server components
     * (every data path) — but NOT in the edge middleware, which cannot reach
     * Postgres. It re-reads the user by id on every call and:
     *   - invalidates the token (returns null) if the account was deactivated,
     *     deleted, or its token_version was bumped (password reset / "sign out
     *     everywhere");
     *   - refreshes the role so a demotion (ADMIN → USER) takes effect at once
     *     instead of persisting for the ~30-day token lifetime.
     * Returning null makes auth() resolve to no session, so revocation is
     * enforced at the data layer within one request.
     */
    async jwt(params) {
      // First delegate to the base callback (identity copy at sign-in).
      const token = authConfig.callbacks.jwt(params);
      if (!token?.id) return token;

      const [current] = await db
        .select({
          role: users.role,
          deactivatedAt: users.deactivatedAt,
          tokenVersion: users.tokenVersion,
        })
        .from(users)
        .where(eq(users.id, token.id))
        .limit(1);

      // Account gone or deactivated → revoke.
      if (!current || current.deactivatedAt) return null;
      // Session predates a token_version bump → revoke.
      if ((current.tokenVersion ?? 0) !== (token.tokenVersion ?? 0)) return null;

      // Reflect current standing (e.g. an ADMIN demoted to USER).
      token.role = current.role;
      return token;
    },
  },
  providers: [
    Credentials({
      credentials: {
        email: {},
        password: {},
      },
      async authorize(credentials) {
        const parsed = signInSchema.safeParse(credentials);
        if (!parsed.success) return null;

        const { email, password } = parsed.data;

        const [user] = await db
          .select()
          .from(users)
          .where(eq(users.email, email.toLowerCase()))
          .limit(1);

        if (!user || !user.passwordHash) return null;
        if (user.deactivatedAt) return null;

        const valid = await bcrypt.compare(password, user.passwordHash);
        if (!valid) return null;

        return {
          id: user.id,
          email: user.email,
          name: user.displayName ?? user.username,
          username: user.username,
          role: user.role,
          tokenVersion: user.tokenVersion,
        };
      },
    }),
  ],
});
