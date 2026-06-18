"use server";

import { revalidatePath } from "next/cache";
import { infrastructureClient } from "@/lib/infrastructure-client";
import type {
  ArtifactVersion,
  PublishVersionRequest,
} from "@/types/infrastructure";
import { type ActionResult, withOwnership } from "./helpers";

export async function listLeafVersions(
  leafId: string,
): Promise<ActionResult<ArtifactVersion[]>> {
  return withOwnership(leafId, async () => {
    const resp = await infrastructureClient.listVersions(leafId);
    return resp.data;
  });
}

export async function publishLeafVersion(
  leafId: string,
  slug: string,
  data: PublishVersionRequest,
): Promise<ActionResult<ArtifactVersion>> {
  return withOwnership(leafId, async () => {
    const v = await infrastructureClient.publishVersion(leafId, data);
    revalidatePath(`/dashboard/leafs/${slug}`);
    return v;
  });
}

export async function activateLeafVersion(
  leafId: string,
  slug: string,
  versionId: string,
): Promise<ActionResult<ArtifactVersion>> {
  return withOwnership(leafId, async () => {
    const v = await infrastructureClient.activateVersion(leafId, versionId);
    revalidatePath(`/dashboard/leafs/${slug}`);
    return v;
  });
}

export async function deleteLeafVersion(
  leafId: string,
  slug: string,
  versionId: string,
): Promise<ActionResult<void>> {
  return withOwnership(leafId, async () => {
    await infrastructureClient.deleteVersion(leafId, versionId);
    revalidatePath(`/dashboard/leafs/${slug}`);
  });
}
