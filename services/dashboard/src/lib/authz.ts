import {
  InfrastructureApiError,
  infrastructureClient,
} from "@/lib/infrastructure-client";

/**
 * Shared per-user authorization VERDICT for the dashboard (cluster B).
 *
 * The dashboard talks to the infrastructure API with a single shared service
 * key (a USER-role key owned by the admin), so the infrastructure's own
 * ownership check always resolves to the service-key owner — NOT the logged-in
 * dashboard user. Per-user authorization must therefore happen here, in
 * Next.js, and it must happen through ONE predicate: the server actions (via
 * `assertLeafOwnership` in lib/actions/helpers.ts) and the /api/* route
 * handlers (via the adapters in lib/authz-routes.ts) all consume
 * `leafOwnershipVerdict`, so the two surfaces cannot drift apart.
 *
 * This module deliberately imports NO `next/server` symbols so it stays usable
 * from the server-action layer (jsdom-tested) as well as route handlers.
 */

export type AccessDenial = { code: string; message: string };

export type OwnershipVerdict =
  | { allowed: true }
  | { allowed: false; denial: AccessDenial };

/** The minimal session shape the verdict needs (matches the NextAuth session). */
export type SessionWithUser = { user: { id: string; role: string } };

/**
 * Decides whether the session may act on the given leaf: the leaf's creator or
 * an ADMIN. Fetches the leaf via the (shared service-key) client purely to read
 * its creator_id; the leaf itself is never returned, so a denied caller learns
 * nothing. Not-found is collapsed into FORBIDDEN for non-admins so callers
 * cannot distinguish "missing" from "not yours". Admins still get the real
 * error so genuine infrastructure outages surface.
 */
export async function leafOwnershipVerdict(
  leafId: string,
  session: SessionWithUser,
): Promise<OwnershipVerdict> {
  let creatorId: string | null;
  try {
    const leaf = await infrastructureClient.getLeaf(leafId);
    creatorId = leaf.creator_id;
  } catch (err) {
    if (
      session.user.role !== "ADMIN" &&
      err instanceof InfrastructureApiError &&
      err.status === 404
    ) {
      return { allowed: false, denial: forbiddenDenial() };
    }
    return { allowed: false, denial: infraDenial(err) };
  }

  if (session.user.role === "ADMIN" || creatorId === session.user.id) {
    return { allowed: true };
  }

  return { allowed: false, denial: forbiddenDenial() };
}

export function forbiddenDenial(): AccessDenial {
  return {
    code: "FORBIDDEN",
    message: "You do not have access to this resource.",
  };
}

export function infraDenial(err: unknown): AccessDenial {
  if (err instanceof InfrastructureApiError) {
    return { code: err.code, message: err.message };
  }
  return { code: "INTERNAL_ERROR", message: "An unexpected error occurred." };
}
