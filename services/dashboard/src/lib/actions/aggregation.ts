"use server";

import { infrastructureClient } from "@/lib/infrastructure-client";
import type { AggregationResult } from "@/types/infrastructure";
import { type ActionResult, withOwnership } from "./helpers";

export async function triggerLeafAggregation(
  leafId: string,
): Promise<ActionResult<AggregationResult>> {
  return withOwnership(leafId, async () => {
    const resp = await infrastructureClient.triggerAggregation(leafId);
    return resp.data;
  });
}
