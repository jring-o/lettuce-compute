import { NextResponse } from "next/server";

import { auth } from "@/lib/auth";
import {
  type AccessDenial,
  type SessionWithUser,
  leafOwnershipVerdict,
} from "@/lib/authz";
import { getFile, type FileInfo } from "@/lib/file-storage";

/**
 * Route-handler adapters for the dashboard /api/* surface (cluster B). These
 * wrap the shared `leafOwnershipVerdict` (lib/authz.ts) and translate a denial
 * into the `NextResponse` a route must return verbatim, so the /api/* routes
 * enforce the SAME per-user predicate the server actions do.
 *
 * Kept in a separate module from lib/authz.ts because it imports `next/server`,
 * which cannot load in the jsdom-environment action tests.
 */

/** Maps a denial code onto the HTTP status a route handler must return. */
function statusForDenial(denial: AccessDenial): number {
  switch (denial.code) {
    case "UNAUTHENTICATED":
      return 401;
    case "FORBIDDEN":
      return 403;
    case "NOT_FOUND":
      return 404;
    case "VALIDATION_ERROR":
      return 400;
    default:
      return 502;
  }
}

function denialResponse(denial: AccessDenial): NextResponse {
  return NextResponse.json({ error: denial }, { status: statusForDenial(denial) });
}

const UNAUTHENTICATED: AccessDenial = {
  code: "UNAUTHENTICATED",
  message: "You must be signed in.",
};

export type LeafRouteAccess =
  | { ok: true; session: SessionWithUser }
  | { ok: false; response: NextResponse };

/**
 * Route-handler adapter for leaf-keyed /api/* routes: requires a session AND
 * the leaf-ownership verdict. Returns the session on success, or the
 * NextResponse (401/403/404/400) the route must return verbatim on denial.
 */
export async function requireLeafAccess(
  leafId: string | null | undefined,
): Promise<LeafRouteAccess> {
  const session = await auth();
  if (!session?.user?.id) {
    return { ok: false, response: denialResponse(UNAUTHENTICATED) };
  }

  if (!leafId) {
    return {
      ok: false,
      response: denialResponse({
        code: "VALIDATION_ERROR",
        message: "leaf_id is required.",
      }),
    };
  }

  const verdict = await leafOwnershipVerdict(leafId, session as SessionWithUser);
  if (!verdict.allowed) {
    return { ok: false, response: denialResponse(verdict.denial) };
  }

  return { ok: true, session: session as SessionWithUser };
}

export type FileRouteAccess =
  | { ok: true; session: SessionWithUser; file: FileInfo }
  | { ok: false; response: NextResponse };

/**
 * Route-handler adapter for the file object. A file is readable by its
 * uploader, the owner of the leaf it belongs to, or an ADMIN. Every denial —
 * including "file does not exist" — is the same 404, so a caller cannot probe
 * which file ids exist.
 */
export async function requireFileAccess(
  fileId: string,
): Promise<FileRouteAccess> {
  const session = await auth();
  if (!session?.user?.id) {
    return { ok: false, response: denialResponse(UNAUTHENTICATED) };
  }
  const user = (session as SessionWithUser).user;

  const file = await getFile(fileId);
  if (!file) {
    return { ok: false, response: fileNotFoundResponse() };
  }

  if (file.uploadedBy === user.id || user.role === "ADMIN") {
    return { ok: true, session: session as SessionWithUser, file };
  }

  // Not the uploader: allowed only if the caller owns the file's leaf.
  const verdict = await leafOwnershipVerdict(
    file.leafId,
    session as SessionWithUser,
  );
  if (!verdict.allowed) {
    return { ok: false, response: fileNotFoundResponse() };
  }

  return { ok: true, session: session as SessionWithUser, file };
}

function fileNotFoundResponse(): NextResponse {
  return NextResponse.json(
    { error: { code: "NOT_FOUND", message: "File not found" } },
    { status: 404 },
  );
}
