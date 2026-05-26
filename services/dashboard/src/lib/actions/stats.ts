"use server";

import { infrastructureClient } from "@/lib/infrastructure-client";
import type { LeafStats, StatsHistoryParams } from "@/types/infrastructure";

import {
  type ActionResult,
  assertLeafOwnership,
  authError,
  mapInfraError,
  requireAuth,
} from "./helpers";

// These actions are only consumed by the authenticated, owner-gated dashboard
// (ProjectDashboard polls getLeafStats; the public leaf page reads stats via
// the infrastructure client directly, not these actions). They are therefore
// gated by auth + per-user ownership, matching withOwnership. Public stats
// exposure, if ever needed, must go through a separate public-only path.

export async function getLeafStats(
  leafId: string,
): Promise<ActionResult<LeafStats>> {
  const session = await requireAuth();
  if (!session) return authError();

  const ownership = await assertLeafOwnership(leafId, session);
  if (ownership) return ownership;

  try {
    const stats = await infrastructureClient.getLeafStats(leafId);
    return { data: stats };
  } catch (err) {
    return mapInfraError(err);
  }
}

export async function getLeafStatsHistory(
  leafId: string,
  params: StatsHistoryParams,
): Promise<ActionResult<{ data: LeafStats[] }>> {
  const session = await requireAuth();
  if (!session) return authError();

  const ownership = await assertLeafOwnership(leafId, session);
  if (ownership) return ownership;

  try {
    const result = await infrastructureClient.getLeafStatsHistory(
      leafId,
      params,
    );
    return { data: result };
  } catch (err) {
    return mapInfraError(err);
  }
}
