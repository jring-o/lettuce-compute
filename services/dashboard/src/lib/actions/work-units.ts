"use server";

import { infrastructureClient } from "@/lib/infrastructure-client";
import type {
  GenerateWorkUnitsRequest,
  GenerateWorkUnitsResponse,
  ListWorkUnitsParams,
  PaginatedResponse,
  WorkUnit,
  WorkUnitSummary,
} from "@/types/infrastructure";

import { type ActionResult, withOwnership } from "./helpers";

export async function listWorkUnits(
  leafId: string,
  params?: ListWorkUnitsParams,
): Promise<ActionResult<PaginatedResponse<WorkUnitSummary>>> {
  return withOwnership(leafId, () =>
    infrastructureClient.listWorkUnits(leafId, params),
  );
}

export async function getWorkUnit(
  leafId: string,
  workUnitId: string,
): Promise<ActionResult<WorkUnit>> {
  return withOwnership(leafId, () =>
    infrastructureClient.getWorkUnit(leafId, workUnitId),
  );
}

export async function generateWorkUnits(
  leafId: string,
  data?: GenerateWorkUnitsRequest,
): Promise<ActionResult<GenerateWorkUnitsResponse>> {
  return withOwnership(leafId, () =>
    infrastructureClient.generateWorkUnits(leafId, data),
  );
}
