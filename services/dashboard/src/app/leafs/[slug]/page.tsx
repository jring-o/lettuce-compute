import { notFound } from "next/navigation";
import { eq } from "drizzle-orm";
import type { Metadata } from "next";

export const revalidate = 30; // ISR: revalidate leaf detail every 30 seconds

import { infrastructureClient, InfrastructureApiError } from "@/lib/infrastructure-client";
import { db } from "@/lib/db";
import { users } from "@/lib/db/schema";
import { ProjectDetail } from "@/components/projects/project-detail";

export async function generateMetadata({
  params,
}: {
  params: Promise<{ slug: string }>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `${slug} — Lettuce` };
}

export default async function LeafDetailPage({
  params,
}: {
  params: Promise<{ slug: string }>;
}) {
  const { slug } = await params;

  // Fetch leaf by slug (the infrastructure API accepts slugs in the leaf_id path)
  let leaf;
  try {
    leaf = await infrastructureClient.getLeaf(slug);
  } catch (err) {
    if (err instanceof InfrastructureApiError && err.status === 404) {
      notFound();
    }
    throw err;
  }

  // Only show PUBLIC or UNLISTED leafs
  if (leaf.visibility === "PRIVATE") notFound();

  // Fetch stats, creator, aggregation, and check for visualization data in parallel
  const [stats, creator, aggregationResp, validatedWus] = await Promise.all([
    infrastructureClient.getLeafStats(leaf.id).catch(() => null),
    leaf.creator_id
      ? db
          .select({
            username: users.username,
            displayName: users.displayName,
            createdAt: users.createdAt,
          })
          .from(users)
          .where(eq(users.id, leaf.creator_id))
          .limit(1)
          .then(([user]) => user ?? null)
      : null,
    infrastructureClient.getAggregation(leaf.id).catch(() => null),
    infrastructureClient
      .listWorkUnits(leaf.id, { state: "VALIDATED", limit: 1 })
      .catch(() => null),
  ]);
  const aggregation = aggregationResp?.data ?? null;
  const hasVisualization = (validatedWus?.data?.length ?? 0) > 0;

  const platformUrl = process.env.PLATFORM_URL ?? "http://localhost:3000";
  const serverHost = new URL(platformUrl).hostname;

  return (
    <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
      <ProjectDetail
        leaf={leaf}
        stats={stats}
        creator={creator}
        serverHost={serverHost}
        aggregation={aggregation}
        hasVisualization={hasVisualization}
      />
    </div>
  );
}
