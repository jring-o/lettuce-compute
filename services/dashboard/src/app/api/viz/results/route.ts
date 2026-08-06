import { NextRequest, NextResponse } from "next/server";

import { requireLeafAccess } from "@/lib/authz-routes";
import { infrastructureClient } from "@/lib/infrastructure-client";
import { isPublicResultsLeaf } from "@/lib/results-visibility";
import {
  anonVizRateLimiter,
  clientIpFrom,
  publicLeafVerdictCache,
  publicResultCache,
} from "@/lib/public-viz-guard";

/**
 * Denied-caller fallback (design §4.7): a leaf whose results_visibility is
 * PUBLIC (and whose visibility is not PRIVATE) is replayable without a
 * session. The verdict comes from the leaf itself — the head stores the
 * opt-in on the leaf row, set through the audited leaf-update API — and is
 * cached briefly so anonymous traffic does not fetch the leaf per request.
 * Any lookup failure means "not public": the caller gets the original denial.
 */
async function allowedByPublicResults(leafId: string): Promise<boolean> {
  const cached = publicLeafVerdictCache.get(leafId);
  if (cached !== undefined) return cached;
  let verdict = false;
  try {
    verdict = isPublicResultsLeaf(await infrastructureClient.getLeaf(leafId));
  } catch {
    verdict = false;
  }
  publicLeafVerdictCache.set(leafId, verdict);
  return verdict;
}

export async function GET(request: NextRequest) {
  const searchParams = request.nextUrl.searchParams;
  const leafId = searchParams.get("leafId");
  const workUnitId = searchParams.get("workUnitId");
  const volunteerId = searchParams.get("volunteerId");

  if (!leafId || !workUnitId) {
    return NextResponse.json(
      { error: { code: "MISSING_PARAMS", message: "leafId and workUnitId are required" } },
      { status: 400 },
    );
  }

  // A result's output_data is leaf CONTENTS — owner-only regardless of the
  // leaf's visibility (BG-07), UNLESS the leaf's results_visibility opted it
  // PUBLIC (design §4.7). Gate on the target leaf before touching the
  // admin-keyed listResults, through the SAME predicate the server actions
  // use; only a denied caller falls through to the per-leaf opt-in check. The
  // middleware admits anonymous callers to this route precisely so that
  // per-leaf policy can run here.
  const access = await requireLeafAccess(leafId);
  let publicCacheKey: string | null = null;
  if (!access.ok) {
    // The public path drives admin-keyed calls into the head, so it is rate
    // limited per IP and served from a short-TTL cache (results are immutable
    // once submitted); see lib/public-viz-guard.ts. Authenticated owner/admin
    // traffic never reaches this branch.
    if (!anonVizRateLimiter.allow(clientIpFrom(request.headers))) {
      return NextResponse.json(
        { error: { code: "RATE_LIMITED", message: "Too many requests — retry shortly." } },
        { status: 429 },
      );
    }
    if (!(await allowedByPublicResults(leafId))) {
      return access.response;
    }
    publicCacheKey = `${leafId}|${workUnitId}|${volunteerId ?? ""}`;
    const cached = publicResultCache.get(publicCacheKey);
    if (cached !== undefined) {
      return NextResponse.json(cached);
    }
  }

  try {
    // No validation_status filter: the viz replays any submitted result's
    // output_data (validated or not), matching the visualize page's listing of
    // COMPLETED as well as VALIDATED work units.
    const results = await infrastructureClient.listResults(leafId, {
      work_unit_id: workUnitId,
      limit: 1,
      ...(volunteerId ? { volunteer_id: volunteerId } : {}),
    });

    const payload = { result: results.data[0] ?? null };
    if (publicCacheKey !== null) {
      publicResultCache.set(publicCacheKey, payload);
    }
    return NextResponse.json(payload);
  } catch {
    return NextResponse.json({ result: null }, { status: 200 });
  }
}
