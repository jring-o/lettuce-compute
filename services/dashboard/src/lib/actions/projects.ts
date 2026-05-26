"use server";

import { z } from "zod";

import { infrastructureClient } from "@/lib/infrastructure-client";
import type {
  Leaf,
  LeafSummary,
  PaginatedResponse,
  UpdateLeafRequest,
} from "@/types/infrastructure";

import {
  type ActionResult,
  authError,
  notFoundError,
  mapInfraError,
  requireAuth,
  withOwnership,
} from "./helpers";

// --- Validation ---

const createLeafSchema = z.object({
  name: z.string().min(3, "Name must be at least 3 characters").max(100),
  description: z.string().min(10, "Description must be at least 10 characters").max(10000),
  task_pattern: z.enum(["PARAMETER_SWEEP", "MAP_REDUCE", "MONTE_CARLO", "CUSTOM"]),
  research_area: z.string().optional(),
  is_ongoing: z
    .string()
    .transform((v) => v === "true")
    .optional(),
  visibility: z.enum(["PUBLIC", "UNLISTED", "PRIVATE"]).optional(),
});

// --- Actions ---

export async function createLeaf(
  formData: FormData,
): Promise<ActionResult<Leaf>> {
  const session = await requireAuth();
  if (!session) return authError();

  const raw = Object.fromEntries(formData.entries());
  const parsed = createLeafSchema.safeParse(raw);
  if (!parsed.success) {
    const firstIssue = parsed.error.issues[0];
    return {
      error: {
        code: "VALIDATION_ERROR",
        message: firstIssue?.message ?? "Invalid input.",
      },
    };
  }

  try {
    const leaf = await infrastructureClient.createLeaf({
      ...parsed.data,
      creator_id: session.user.id,
    });
    return { data: leaf };
  } catch (err) {
    return mapInfraError(err);
  }
}

export async function getLeaf(
  leafId: string,
): Promise<ActionResult<Leaf>> {
  const session = await requireAuth();
  if (!session) return authError();

  try {
    const leaf = await infrastructureClient.getLeaf(leafId);

    // Enforce visibility. The dashboard talks to infrastructure with a single
    // shared service key, so the infra read returns ANY leaf regardless of the
    // caller. A PRIVATE leaf may only be read by its creator or an ADMIN; for
    // everyone else collapse it into NOT_FOUND so we never leak its existence
    // or contents (description, execution config, binary URLs). PUBLIC and
    // UNLISTED leafs stay readable by any authenticated user.
    if (
      leaf.visibility === "PRIVATE" &&
      leaf.creator_id !== session.user.id &&
      session.user.role !== "ADMIN"
    ) {
      return notFoundError();
    }

    return { data: leaf };
  } catch (err) {
    return mapInfraError(err);
  }
}

export async function listMyLeafs(
  cursor?: string,
): Promise<ActionResult<PaginatedResponse<LeafSummary>>> {
  const session = await requireAuth();
  if (!session) return authError();

  try {
    const result = await infrastructureClient.listLeafs({
      creator_id: session.user.id,
      cursor,
    });
    return { data: result };
  } catch (err) {
    return mapInfraError(err);
  }
}

export async function updateLeaf(
  leafId: string,
  data: UpdateLeafRequest,
): Promise<ActionResult<Leaf>> {
  return withOwnership(leafId, () =>
    infrastructureClient.updateLeaf(leafId, data),
  );
}

export async function deleteLeaf(
  leafId: string,
): Promise<ActionResult<void>> {
  return withOwnership(leafId, () =>
    infrastructureClient.deleteLeaf(leafId),
  );
}

export async function activateLeaf(
  leafId: string,
): Promise<ActionResult<Leaf>> {
  return withOwnership(leafId, () =>
    infrastructureClient.activateLeaf(leafId),
  );
}

export async function pauseLeaf(
  leafId: string,
): Promise<ActionResult<Leaf>> {
  return withOwnership(leafId, () =>
    infrastructureClient.pauseLeaf(leafId),
  );
}

export async function resumeLeaf(
  leafId: string,
): Promise<ActionResult<Leaf>> {
  return withOwnership(leafId, () =>
    infrastructureClient.resumeLeaf(leafId),
  );
}

export async function archiveLeaf(
  leafId: string,
): Promise<ActionResult<Leaf>> {
  return withOwnership(leafId, () =>
    infrastructureClient.archiveLeaf(leafId),
  );
}

export async function configureLeaf(
  leafId: string,
): Promise<ActionResult<Leaf>> {
  return withOwnership(leafId, () =>
    infrastructureClient.configureLeaf(leafId),
  );
}
