import { NextRequest, NextResponse } from "next/server";

import { requireFileAccess } from "@/lib/authz-routes";
import { getFileContent } from "@/lib/file-storage";

function sanitizeFilename(name: string): string {
  return name.replace(/["\r\n]/g, "_");
}

export async function GET(
  _request: NextRequest,
  { params }: { params: Promise<{ id: string }> },
) {
  const { id } = await params;

  // A file blob is readable by its uploader, the owner of its leaf, or an
  // ADMIN (BG-10). requireFileAccess resolves the file and enforces that
  // predicate; every denial — including "no such file" — is the same 404, so a
  // caller cannot probe which file ids exist.
  const access = await requireFileAccess(id);
  if (!access.ok) return access.response;

  const file = await getFileContent(id);
  if (!file) {
    return NextResponse.json(
      { error: { code: "NOT_FOUND", message: "File not found" } },
      { status: 404 },
    );
  }

  const safeName = sanitizeFilename(file.filename);

  return new NextResponse(new Uint8Array(file.buffer), {
    status: 200,
    headers: {
      "Content-Type": file.contentType ?? "application/octet-stream",
      "Content-Length": String(file.sizeBytes),
      "Content-Disposition": `attachment; filename="${safeName}"`,
    },
  });
}
