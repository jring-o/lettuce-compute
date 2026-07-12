import { createHash, randomUUID } from "crypto";
import { mkdir, readFile, rm, stat, writeFile } from "fs/promises";
import path from "path";

import { eq } from "drizzle-orm";

import { db } from "@/lib/db";
import { fileUploads } from "@/lib/db/schema";

const DATA_DIR = process.env.LETTUCE_DATA_DIR ?? "./data";

// Size limits in bytes
const SIZE_LIMITS: Record<string, number> = {
  CODE_ARTIFACT: 500 * 1024 * 1024, // 500 MB
  INPUT_DATA: 1024 * 1024 * 1024, // 1 GB
  RESULT_DATA: 1024 * 1024 * 1024, // 1 GB
  CHECKPOINT: 1024 * 1024 * 1024, // 1 GB
};

export interface SaveFileResult {
  fileId: string;
  storageKey: string;
  checksum: string;
  sizeBytes: number;
}

export interface FileInfo {
  path: string;
  filename: string;
  contentType: string | null;
  sizeBytes: number;
  checksum: string;
  /** The leaf this file belongs to; authorization keys on its owner. */
  leafId: string;
  /** The uploading user, or null for legacy rows uploaded before tracking. */
  uploadedBy: string | null;
}

export async function saveFile(
  file: File,
  leafId: string,
  fileType: string,
  uploadedBy?: string,
): Promise<SaveFileResult> {
  const maxSize = SIZE_LIMITS[fileType];
  if (maxSize && file.size > maxSize) {
    throw new Error(
      `File exceeds size limit of ${Math.round(maxSize / (1024 * 1024))} MB for ${fileType}`,
    );
  }

  if (file.size <= 0) {
    throw new Error("File must not be empty");
  }

  const fileId = randomUUID();
  const safeName = path.basename(file.name) || "upload";
  const storageKey = `leafs/${leafId}/${fileType}/${safeName}`;
  const dir = path.join(DATA_DIR, "uploads", fileId);
  await mkdir(dir, { recursive: true });

  const buffer = Buffer.from(await file.arrayBuffer());
  const filePath = path.join(dir, safeName);
  await writeFile(filePath, buffer);

  const checksum = createHash("sha256").update(buffer).digest("hex");

  await db.insert(fileUploads).values({
    id: fileId,
    leafId,
    fileType,
    filename: safeName,
    storageKey,
    sizeBytes: file.size,
    contentType: file.type || null,
    checksumSha256: checksum,
    uploadedBy: uploadedBy ?? null,
  });

  return { fileId, storageKey, checksum, sizeBytes: file.size };
}

export async function getFile(fileId: string): Promise<FileInfo | null> {
  const [record] = await db
    .select()
    .from(fileUploads)
    .where(eq(fileUploads.id, fileId))
    .limit(1);

  if (!record) return null;

  const filePath = path.join(DATA_DIR, "uploads", fileId, path.basename(record.filename));

  try {
    await stat(filePath);
  } catch {
    return null;
  }

  return {
    path: path.resolve(filePath),
    filename: record.filename,
    contentType: record.contentType,
    sizeBytes: record.sizeBytes,
    checksum: record.checksumSha256,
    leafId: record.leafId,
    uploadedBy: record.uploadedBy,
  };
}

export async function deleteFile(fileId: string): Promise<void> {
  const dir = path.join(DATA_DIR, "uploads", fileId);

  await db.delete(fileUploads).where(eq(fileUploads.id, fileId));

  try {
    await rm(dir, { recursive: true, force: true });
  } catch {
    // Directory may not exist; ignore
  }
}

export async function getFileContent(fileId: string): Promise<{
  buffer: Buffer;
  filename: string;
  contentType: string | null;
  sizeBytes: number;
  checksum: string;
} | null> {
  const info = await getFile(fileId);
  if (!info) return null;

  const buffer = await readFile(info.path);
  return {
    buffer,
    filename: info.filename,
    contentType: info.contentType,
    sizeBytes: info.sizeBytes,
    checksum: info.checksum,
  };
}
