"use server";

import { infrastructureClient } from "@/lib/infrastructure-client";
import type {
  LeafStats,
  LeafSummary,
  ListLeafsParams,
  Pagination,
} from "@/types/infrastructure";
import { mapInfraError, type ActionResult } from "./helpers";

export type LeafWithStats = LeafSummary & { stats: LeafStats | null };

export interface PublicLeafsResult {
  leafs: LeafWithStats[];
  pagination: Pagination;
}

export async function listPublicLeafs(params: {
  search?: string;
  research_area?: string;
  sort?: "updated_at" | "created_at";
  cursor?: string;
  limit?: number;
}): Promise<ActionResult<PublicLeafsResult>> {
  try {
    const listParams: ListLeafsParams = {
      state: "ACTIVE",
      sort: params.sort ?? "updated_at",
      order: "desc",
      limit: params.limit ?? 12,
    };
    if (params.search) listParams.search = params.search;
    if (params.research_area) listParams.research_area = params.research_area;
    if (params.cursor) listParams.cursor = params.cursor;

    const result = await infrastructureClient.listLeafs(listParams);

    // Filter out PRIVATE leafs — the infrastructure API filters by state but
    // not visibility, so ACTIVE+PRIVATE leafs could otherwise leak.
    const publicLeafs = result.data.filter(
      (p) => p.visibility === "PUBLIC",
    );

    const leafIds = publicLeafs.map((p) => p.id);
    const batchStats =
      leafIds.length > 0
        ? await infrastructureClient
            .getLeafStatsBatch(leafIds)
            .catch(() => ({}) as Record<string, LeafStats>)
        : {};

    const leafs: LeafWithStats[] = publicLeafs.map((leaf) => ({
      ...leaf,
      stats: batchStats[leaf.id] ?? null,
    }));

    return { data: { leafs, pagination: result.pagination } };
  } catch (err) {
    return mapInfraError(err);
  }
}
