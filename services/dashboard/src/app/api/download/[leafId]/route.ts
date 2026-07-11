import { NextRequest, NextResponse } from "next/server";

import { requireLeafAccess } from "@/lib/authz-routes";
import { infrastructureClient } from "@/lib/infrastructure-client";
import type { Result, WorkUnitSummary } from "@/types/infrastructure";

// BG-23: "Download Results" must export the SCIENCE-COMPLETE payload. Before
// this fix the endpoint paginated COMPLETED units while the button keyed on the
// disjoint work_units_validated > 0 (so a finished leaf downloaded nothing),
// and even with data it exported work-unit INPUT parameters, not results. It
// now fetches VALIDATED units — the same state the button keys on — and emits
// each unit's validated result output_data, keeping the input parameters as
// context. Ownership is enforced through the shared requireLeafAccess adapter
// (the result output_data is owner-only contents).

interface ExportRow {
  work_unit_id: string;
  parameters: Record<string, unknown>;
  output_data: Record<string, unknown> | null;
  output_data_ref: string | null;
  output_checksum: string | null;
  validated_at: string | null;
}

export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ leafId: string }> },
) {
  const { leafId } = await params;

  const access = await requireLeafAccess(leafId);
  if (!access.ok) return access.response;

  const format = request.nextUrl.searchParams.get("format") ?? "json";
  if (format !== "csv" && format !== "json") {
    return NextResponse.json(
      { error: { code: "INVALID_FORMAT", message: "Format must be csv or json." } },
      { status: 400 },
    );
  }

  // The leaf is needed only for the download filename; ownership is already
  // enforced above.
  let slug = leafId;
  try {
    const leaf = await infrastructureClient.getLeaf(leafId);
    slug = leaf.slug;
  } catch {
    // Fall back to the id in the filename; not fatal.
  }

  // Paginate through all VALIDATED work units — the science-complete state the
  // "Download Results" button keys on (work_units_validated > 0).
  const validatedUnits: WorkUnitSummary[] = [];
  let cursor: string | undefined;
  do {
    const page = await infrastructureClient.listWorkUnits(leafId, {
      state: "VALIDATED",
      limit: 200,
      cursor,
    });
    validatedUnits.push(...page.data);
    cursor = page.pagination.next_cursor ?? undefined;
  } while (cursor);

  if (validatedUnits.length === 0) {
    return new NextResponse(null, { status: 204 });
  }

  // For each validated unit fetch its parameters (full work unit) and its
  // agreed result's output_data, in parallel.
  const rows = (
    await Promise.all(
      validatedUnits.map(async (wu): Promise<ExportRow | null> => {
        const [fullUnit, results] = await Promise.all([
          infrastructureClient.getWorkUnit(leafId, wu.id).catch(() => null),
          infrastructureClient
            .listResults(leafId, {
              work_unit_id: wu.id,
              validation_status: "AGREED",
              limit: 1,
            })
            .then((r) => r.data[0] ?? null)
            .catch(() => null as Result | null),
        ]);

        if (!fullUnit && !results) return null;

        return {
          work_unit_id: wu.id,
          parameters: fullUnit?.parameters ?? {},
          output_data: results?.output_data ?? null,
          output_data_ref: results?.output_data_ref ?? null,
          output_checksum: results?.output_checksum ?? null,
          validated_at: results?.validated_at ?? null,
        };
      }),
    )
  ).filter((r): r is ExportRow => r !== null);

  const filename = `${slug}-results.${format}`;
  const disposition = `attachment; filename="${filename.replace(/["\r\n]/g, "_")}"`;

  if (format === "json") {
    return new NextResponse(JSON.stringify(rows, null, 2), {
      headers: {
        "Content-Type": "application/json",
        "Content-Disposition": disposition,
      },
    });
  }

  return new NextResponse(toCsv(rows), {
    headers: {
      "Content-Type": "text/csv",
      "Content-Disposition": disposition,
    },
  });
}

// toCsv flattens each row's parameter keys into columns (the input context) and
// carries the result as a JSON-encoded output_data column (output is arbitrary
// nested JSON, not a flat scalar set), plus the checksum and validated_at.
function toCsv(rows: ExportRow[]): string {
  const paramKeys = new Set<string>();
  for (const r of rows) {
    for (const key of Object.keys(r.parameters)) paramKeys.add(key);
  }
  const paramCols = Array.from(paramKeys).sort();
  const headers = [
    "work_unit_id",
    ...paramCols,
    "output_data",
    "output_data_ref",
    "output_checksum",
    "validated_at",
  ];

  const escape = (val: unknown): string => {
    if (val === undefined || val === null) return "";
    const str = typeof val === "string" ? val : JSON.stringify(val);
    if (str.includes(",") || str.includes('"') || str.includes("\n")) {
      return `"${str.replace(/"/g, '""')}"`;
    }
    return str;
  };

  const lines = [headers.join(",")];
  for (const r of rows) {
    lines.push(
      [
        r.work_unit_id,
        ...paramCols.map((k) => escape(r.parameters[k])),
        escape(r.output_data),
        escape(r.output_data_ref),
        escape(r.output_checksum),
        escape(r.validated_at),
      ].join(","),
    );
  }
  return lines.join("\n");
}
