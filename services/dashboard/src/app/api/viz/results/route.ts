import { NextRequest, NextResponse } from "next/server";
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

  try {
    const results = await infrastructureClient.listResults(leafId, {
      work_unit_id: workUnitId,
      validation_status: "AGREED",
      limit: 1,
      ...(volunteerId ? { volunteer_id: volunteerId } : {}),
    });

    const result = results.data[0] ?? null;
    return NextResponse.json({ result });
  } catch {
    return NextResponse.json({ result: null }, { status: 200 });
  }
}
