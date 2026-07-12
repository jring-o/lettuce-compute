import { auth } from "@/lib/auth";
import { leafOwnershipVerdict } from "@/lib/authz";
import { InfrastructureApiError } from "@/lib/infrastructure-client";

export type ActionError = { error: { code: string; message: string } };
export type ActionResult<T> = { data: T } | ActionError;

export function authError(): ActionError {
  return { error: { code: "UNAUTHENTICATED", message: "You must be signed in." } };
}

export function forbiddenError(): ActionError {
  return { error: { code: "FORBIDDEN", message: "You do not have access to this resource." } };
}

export function notFoundError(): ActionError {
  return { error: { code: "NOT_FOUND", message: "Leaf not found." } };
}

export function mapInfraError(err: unknown): ActionError {
  if (err instanceof InfrastructureApiError) {
    return { error: { code: err.code, message: err.message } };
  }
  return { error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." } };
}

export async function requireAuth() {
  const session = await auth();
  if (!session?.user?.id) return null;
  return session;
}

/**
 * Enforces per-user authorization for leaf operations.
 *
 * The dashboard talks to the infrastructure API with a single shared
 * service key (a USER-role key owned by the admin), so the infrastructure's
 * own ownership check always resolves to the service-key owner — NOT the
 * logged-in dashboard user. The dashboard must therefore perform the
 * per-user ownership check itself.
 *
 * Authorization succeeds only if the leaf's creator is the current user, or
 * the current user is an ADMIN. To avoid leaking whether a leaf exists,
 * not-found and not-owned are treated the same (both return FORBIDDEN).
 */
export async function withOwnership<T>(
  leafId: string,
  fn: () => Promise<T>,
): Promise<ActionResult<T>> {
  const session = await requireAuth();
  if (!session) return authError();

  const ownership = await assertLeafOwnership(leafId, session);
  if (ownership) return ownership;

  try {
    const result = await fn();
    return { data: result };
  } catch (err) {
    return mapInfraError(err);
  }
}

/**
 * Verifies the current session may act on the given leaf. Returns an
 * ActionError to short-circuit the caller, or null when access is allowed.
 *
 * Thin wrapper over the shared `leafOwnershipVerdict` (lib/authz.ts) — the
 * single ownership predicate the /api/* route adapters also consume — mapped
 * onto the action-layer ActionError shape. Not-found is collapsed into
 * FORBIDDEN so callers cannot distinguish "missing" from "not yours".
 */
export async function assertLeafOwnership(
  leafId: string,
  session: { user: { id: string; role: string } },
): Promise<ActionError | null> {
  const verdict = await leafOwnershipVerdict(leafId, session);
  if (verdict.allowed) return null;
  return { error: verdict.denial };
}
