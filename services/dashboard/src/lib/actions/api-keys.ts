"use server";

import { randomBytes, createHash } from "node:crypto";
import { eq, and, isNull } from "drizzle-orm";
import { db } from "@/lib/db";
import { apiKeys } from "@/lib/db/schema";
import { type ActionResult, authError, mapInfraError, requireAuth } from "./helpers";

const BASE62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";

function base62Encode(data: Buffer): string {
  let n = BigInt("0x" + data.toString("hex"));
  if (n === BigInt(0)) return "0";
  const base = BigInt(62);
  const chars: string[] = [];
  while (n > BigInt(0)) {
    chars.push(BASE62[Number(n % base)]);
    n = n / base;
  }
  return chars.reverse().join("");
}

function generateKey(): { plaintextKey: string; keyPrefix: string; keyHash: Buffer } {
  const raw = randomBytes(32);
  const encoded = base62Encode(raw);
  const plaintextKey = "lk_" + encoded;
  const keyPrefix = "lk_" + encoded.slice(0, 9);
  const hash = createHash("sha256").update(plaintextKey).digest();
  return { plaintextKey, keyPrefix, keyHash: hash };
}

export type ApiKeyInfo = {
  id: string;
  name: string;
  keyPrefix: string;
  createdAt: Date;
  lastUsedAt: Date | null;
  revokedAt: Date | null;
};

export async function createApiKey(
  name: string,
): Promise<ActionResult<{ key: ApiKeyInfo; plaintextKey: string }>> {
  const session = await requireAuth();
  if (!session) return authError();

  const trimmed = name.trim();
  if (!trimmed || trimmed.length > 100) {
    return { error: { code: "VALIDATION_ERROR", message: "Name must be 1-100 characters." } };
  }

  const { plaintextKey, keyPrefix, keyHash } = generateKey();

  try {
    const [inserted] = await db
      .insert(apiKeys)
      .values({
        userId: session.user.id,
        name: trimmed,
        keyPrefix,
        keyHash,
      })
      .returning();

    return {
      data: {
        key: {
          id: inserted.id,
          name: inserted.name,
          keyPrefix: inserted.keyPrefix,
          createdAt: inserted.createdAt,
          lastUsedAt: inserted.lastUsedAt,
          revokedAt: inserted.revokedAt,
        },
        plaintextKey,
      },
    };
  } catch (err) {
    return mapInfraError(err);
  }
}

export async function listApiKeys(): Promise<ActionResult<ApiKeyInfo[]>> {
  const session = await requireAuth();
  if (!session) return authError();

  try {
    const rows = await db
      .select({
        id: apiKeys.id,
        name: apiKeys.name,
        keyPrefix: apiKeys.keyPrefix,
        createdAt: apiKeys.createdAt,
        lastUsedAt: apiKeys.lastUsedAt,
        revokedAt: apiKeys.revokedAt,
      })
      .from(apiKeys)
      .where(eq(apiKeys.userId, session.user.id))
      .orderBy(apiKeys.createdAt);

    return { data: rows };
  } catch (err) {
    return mapInfraError(err);
  }
}

export async function revokeApiKey(
  keyId: string,
): Promise<ActionResult<ApiKeyInfo>> {
  const session = await requireAuth();
  if (!session) return authError();

  try {
    const [updated] = await db
      .update(apiKeys)
      .set({ revokedAt: new Date() })
      .where(
        and(
          eq(apiKeys.id, keyId),
          eq(apiKeys.userId, session.user.id),
          isNull(apiKeys.revokedAt),
        ),
      )
      .returning({
        id: apiKeys.id,
        name: apiKeys.name,
        keyPrefix: apiKeys.keyPrefix,
        createdAt: apiKeys.createdAt,
        lastUsedAt: apiKeys.lastUsedAt,
        revokedAt: apiKeys.revokedAt,
      });

    if (!updated) {
      return { error: { code: "NOT_FOUND", message: "API key not found or already revoked." } };
    }

    return { data: updated };
  } catch (err) {
    return mapInfraError(err);
  }
}
