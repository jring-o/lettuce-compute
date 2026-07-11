import { NextRequest, NextResponse } from "next/server";

import { requireLeafAccess } from "@/lib/authz-routes";
import { infrastructureClient } from "@/lib/infrastructure-client";

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
  // leaf's visibility (BG-07). Gate on the target leaf before touching the
  // admin-keyed listResults, through the SAME predicate the server actions use.
  const access = await requireLeafAccess(leafId);
  if (!access.ok) return access.response;

  try {
    // No validation_status filter: the viz replays any submitted result's
    // output_data (validated or not), matching the visualize page's listing of
    // COMPLETED as well as VALIDATED work units.
    const results = await infrastructureClient.listResults(leafId, {
      work_unit_id: workUnitId,
      limit: 1,
      ...(volunteerId ? { volunteer_id: volunteerId } : {}),
    });

    const result = results.data[0] ?? null;
    return NextResponse.json({ result });
  } catch {
    return NextResponse.json({ result: null }, { status: 200 });
  }
}
