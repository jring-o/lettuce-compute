import { notFound } from "next/navigation";
import Link from "next/link";
import type { Metadata } from "next";
import { ArrowLeft } from "lucide-react";

import { infrastructureClient, InfrastructureApiError } from "@/lib/infrastructure-client";
import { VisualizationPage } from "@/components/visualization/VisualizationPage";

export const revalidate = 60;

export async function generateMetadata({
  params,
}: {
  params: Promise<{ slug: string }>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `Visualize ${slug} — Lettuce` };
}

export default async function VisualizePage({
  params,
  searchParams,
}: {
  params: Promise<{ slug: string }>;
  searchParams: Promise<{ [key: string]: string | string[] | undefined }>;
}) {
  const { slug } = await params;
  const resolvedSearchParams = await searchParams;
  const volunteerFilter = typeof resolvedSearchParams.volunteer === "string"
    ? resolvedSearchParams.volunteer
    : undefined;

  // Fetch leaf
  let leaf;
  try {
    leaf = await infrastructureClient.getLeaf(slug);
  } catch (err) {
    if (err instanceof InfrastructureApiError && err.status === 404) {
      notFound();
    }
    throw err;
  }

  if (leaf.visibility === "PRIVATE") notFound();

  // Check for viz bundle
  const vizBundleUrl = leaf.execution_config?.binaries?.viz;
  if (!vizBundleUrl) {
    return (
      <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
        <div className="mb-6">
          <Link
            href={`/leafs/${slug}`}
            className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors"
          >
            <ArrowLeft className="size-4" />
            Back to {leaf.name}
          </Link>
        </div>
        <div className="flex items-center justify-center h-64 text-muted-foreground">
          This leaf does not include visualization.
        </div>
      </div>
    );
  }

  // Fetch work units that have results to replay. Visualization is a
  // presentation feature, not a scientific claim, so it does NOT require the
  // redundancy/validation gate: any COMPLETED unit (its redundant runs are in)
  // is replayable, alongside VALIDATED ones. A unit is in exactly one state, so
  // the two sets are disjoint; we dedupe by id defensively and show newest first.
  const [validatedWus, completedWus] = await Promise.all([
    infrastructureClient.listWorkUnits(leaf.id, { state: "VALIDATED", limit: 50 }),
    infrastructureClient.listWorkUnits(leaf.id, { state: "COMPLETED", limit: 50 }),
  ]);
  const seen = new Set<string>();
  const workUnits = [...validatedWus.data, ...completedWus.data]
    .filter((wu) => (seen.has(wu.id) ? false : (seen.add(wu.id), true)))
    .sort((a, b) => (a.updated_at < b.updated_at ? 1 : -1))
    .slice(0, 50);

  // Fetch the first WU's result for initial render (avoids client-side loading
  // flash). No validation_status filter — an unvalidated result still carries
  // the replay data in output_data.
  let initialResult = null;
  if (workUnits.length > 0) {
    try {
      const results = await infrastructureClient.listResults(leaf.id, {
        work_unit_id: workUnits[0].id,
        limit: 1,
        ...(volunteerFilter ? { volunteer_id: volunteerFilter } : {}),
      });
      initialResult = results.data[0] ?? null;
    } catch {
      // Non-fatal — client will retry
    }
  }

  return (
    <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
      <div className="mb-6">
        <Link
          href={`/leafs/${slug}`}
          className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors"
        >
          <ArrowLeft className="size-4" />
          Back to {leaf.name}
        </Link>
        <h1 className="mt-2 text-2xl font-bold tracking-tight">
          {leaf.name} — Visualization
        </h1>
        <p className="text-sm text-muted-foreground mt-1">
          Replay visualization from completed work units.
        </p>
      </div>

      <VisualizationPage
        vizBundleUrl={vizBundleUrl}
        vizOrigin={process.env.VIZ_ORIGIN ?? ""}
        leafSlug={slug}
        leafId={leaf.id}
        workUnits={workUnits}
        initialResult={initialResult}
        volunteerFilter={volunteerFilter}
      />
    </div>
  );
}
