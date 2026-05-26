import { NextRequest, NextResponse } from "next/server";

import { auth } from "@/lib/auth";
import { getFileContent } from "@/lib/file-storage";

function sanitizeFilename(name: string): string {
  return name.replace(/["\r\n]/g, "_");
}

export async function GET(
  _request: NextRequest,
  { params }: { params: Promise<{ id: string }> },
) {
  const session = await auth();
  if (!session?.user) {
    return NextResponse.json(
      { error: { code: "UNAUTHENTICATED", message: "You must be signed in." } },
      { status: 401 },
    );
  }

  const { id } = await params;
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
