import { NextRequest, NextResponse } from "next/server";

import { requireLeafAccess } from "@/lib/authz-routes";
import { saveFile } from "@/lib/file-storage";
import { SUPPORTED_PLATFORMS } from "@/lib/validations/project";

const VALID_PLATFORMS: readonly string[] = SUPPORTED_PLATFORMS.map((p) => p.key);

export async function POST(request: NextRequest) {
  const formData = await request.formData();
  const file = formData.get("file") as File | null;
  const leafId = formData.get("leaf_id") as string | null;
  const fileType = (formData.get("file_type") as string) || "CODE_ARTIFACT";
  const platform = formData.get("platform") as string | null;

  // A file is planted UNDER a leaf, so the caller must own the TARGET leaf
  // (BG-11d) — the write side of the file-owner boundary whose read side is
  // BG-10. The client no longer gets a free random leaf_id: an upload names a
  // real leaf and is authorized against it. requireLeafAccess also enforces
  // authentication (401) and rejects a missing leaf_id (400).
  const access = await requireLeafAccess(leafId);
  if (!access.ok) return access.response;
  const session = access.session;

  const VALID_FILE_TYPES = ["INPUT_DATA", "CODE_ARTIFACT", "RESULT_DATA", "CHECKPOINT"];
  if (!VALID_FILE_TYPES.includes(fileType)) {
    return NextResponse.json(
      {
        error: {
          code: "VALIDATION_ERROR",
          message: `Invalid file_type. Must be one of: ${VALID_FILE_TYPES.join(", ")}`,
        },
      },
      { status: 400 },
    );
  }

  if (!file || file.size === 0) {
    return NextResponse.json(
      { error: { code: "VALIDATION_ERROR", message: "File is required." } },
      { status: 400 },
    );
  }

  if (platform && !VALID_PLATFORMS.includes(platform)) {
    return NextResponse.json(
      {
        error: {
          code: "VALIDATION_ERROR",
          message: `Invalid platform. Must be one of: ${VALID_PLATFORMS.join(", ")}`,
        },
      },
      { status: 400 },
    );
  }

  try {
    const result = await saveFile(file, leafId!, fileType, session.user.id);
    return NextResponse.json(
      {
        file_id: result.fileId,
        filename: file.name,
        size_bytes: result.sizeBytes,
        checksum_sha256: result.checksum,
        platform: platform ?? null,
      },
      { status: 201 },
    );
  } catch (err) {
    const message =
      err instanceof Error ? err.message : "Failed to upload file.";
    return NextResponse.json(
      { error: { code: "UPLOAD_ERROR", message } },
      { status: 400 },
    );
  }
}
