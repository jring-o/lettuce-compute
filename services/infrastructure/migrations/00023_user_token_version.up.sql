-- 00023_user_token_version: session-revocation lever for the dashboard (BG-09,
-- B-dashboard-authz design §4.3/R1.4).
--
-- The dashboard signs users in with a ~stateless NextAuth JWT that, before this
-- cluster, copied identity ONLY at sign-in and was never re-validated — so a
-- deactivation, role demotion, or password reset did not revoke a live session
-- (a ~30-day window). The dashboard's Node auth() jwt callback now re-reads the
-- user every request and kills any token whose token_version is behind the
-- stored value; bumping this integer (password reset, "sign out everywhere")
-- invalidates all of that user's existing sessions at once.
--
-- The users table is created and owned by the head's migrations (00001) and
-- shared with the dashboard over one Postgres instance, so the physical column
-- lives here and applies automatically on head boot alongside every other
-- schema change. The dashboard's drizzle schema (src/lib/db/schema.ts) mirrors
-- it for the ORM. Additive and code-rollback-safe: pre-cluster code ignores the
-- column; legacy rows default to 0.
ALTER TABLE public.users
    ADD COLUMN IF NOT EXISTS token_version integer NOT NULL DEFAULT 0;
