import { auth } from "@/lib/auth";
import {
  InfrastructureApiError,
  infrastructureClient,
} from "@/lib/infrastructure-client";

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
 * Fetches the leaf via the (shared service-key) client purely to read its
 * creator_id; the result is not returned to avoid leaking leaf data on
 * unauthorized access. Not-found is collapsed into FORBIDDEN so callers
 * cannot distinguish "missing" from "not yours".
 */
export async function assertLeafOwnership(
  leafId: string,
  session: { user: { id: string; role: string } },
): Promise<ActionError | null> {
  let creatorId: string | null;
  try {
    const leaf = await infrastructureClient.getLeaf(leafId);
    creatorId = leaf.creator_id;
  } catch (err) {
    // Don't reveal whether the leaf exists; treat any lookup failure as
    // a denied access for non-admins. Admins still get a real error so
    // genuine infra outages surface.
    if (
      session.user.role !== "ADMIN" &&
      err instanceof InfrastructureApiError &&
      err.status === 404
    ) {
      return forbiddenError();
    }
    return mapInfraError(err);
  }

  if (session.user.role === "ADMIN" || creatorId === session.user.id) {
    return null;
  }

  return forbiddenError();
}
