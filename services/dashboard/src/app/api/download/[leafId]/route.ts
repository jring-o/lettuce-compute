import { NextRequest, NextResponse } from "next/server";
import { auth } from "@/lib/auth";
import { infrastructureClient } from "@/lib/infrastructure-client";
import type { WorkUnitSummary } from "@/types/infrastructure";

// TODO(v0.6): Add results list endpoint to infrastructure API for full result data download.
// For Alpha, we export work unit summary data (id, state, priority, attempts, timestamps).
// Parameters require fetching full work units individually — we batch those fetches here.

export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ leafId: string }> },
) {
  const session = await auth();
  if (!session?.user) {
    return NextResponse.json(
      { error: { code: "UNAUTHENTICATED", message: "You must be signed in." } },
      { status: 401 },
    );
  }

  const { leafId } = await params;
  const format = request.nextUrl.searchParams.get("format") ?? "json";

  if (format !== "csv" && format !== "json") {
    return NextResponse.json(
      { error: { code: "INVALID_FORMAT", message: "Format must be csv or json." } },
      { status: 400 },
    );
  }

  // Fetch leaf and verify ownership
  let leaf;
  try {
    leaf = await infrastructureClient.getLeaf(leafId);
  } catch {
    return NextResponse.json(
      { error: { code: "NOT_FOUND", message: "Leaf not found." } },
      { status: 404 },
    );
  }

  if (leaf.creator_id !== session.user.id) {
    return NextResponse.json(
      { error: { code: "NOT_FOUND", message: "Leaf not found." } },
      { status: 404 },
    );
  }

  // Paginate through all COMPLETED work units
  const completedUnits: WorkUnitSummary[] = [];
  let cursor: string | undefined;

  do {
    const result = await infrastructureClient.listWorkUnits(leafId, {
      state: "COMPLETED",
      limit: 200,
      cursor,
    });
    completedUnits.push(...result.data);
    cursor = result.pagination.next_cursor ?? undefined;
  } while (cursor);

  if (completedUnits.length === 0) {
    return new NextResponse(null, { status: 204 });
  }

  // Fetch full work units to get parameters (batch in parallel)
  const fullUnits = await Promise.all(
    completedUnits.map((wu) =>
      infrastructureClient.getWorkUnit(leafId, wu.id).catch(() => null),
    ),
  );

  const results = fullUnits
    .filter((wu) => wu !== null)
    .map((wu) => ({
      work_unit_id: wu.id,
      parameters: wu.parameters ?? {},
      state: wu.state,
      created_at: wu.created_at,
    }));

  const filename = `${leaf.slug}-results.${format}`;

  if (format === "json") {
    return new NextResponse(JSON.stringify(results, null, 2), {
      headers: {
        "Content-Type": "application/json",
        "Content-Disposition": `attachment; filename="${filename.replace(/["\r\n]/g, "_")}"`,
      },
    });
  }

  // CSV format
  // Build header from parameter keys of first result
  const paramKeys = new Set<string>();
  for (const r of results) {
    for (const key of Object.keys(r.parameters)) {
      paramKeys.add(key);
    }
  }
  const paramCols = Array.from(paramKeys).sort();
  const headers = ["work_unit_id", ...paramCols, "state", "created_at"];

  const rows = [headers.join(",")];
  for (const r of results) {
    const values = [
      r.work_unit_id,
      ...paramCols.map((k) => {
        const val = r.parameters[k];
        if (val === undefined || val === null) return "";
        const str = String(val);
        // Escape CSV values with commas or quotes
        if (str.includes(",") || str.includes('"') || str.includes("\n")) {
          return `"${str.replace(/"/g, '""')}"`;
        }
        return str;
      }),
      r.state,
      r.created_at,
    ];
    rows.push(values.join(","));
  }

  return new NextResponse(rows.join("\n"), {
    headers: {
      "Content-Type": "text/csv",
      "Content-Disposition": `attachment; filename="${filename.replace(/["\r\n]/g, "_")}"`,
    },
  });
}
