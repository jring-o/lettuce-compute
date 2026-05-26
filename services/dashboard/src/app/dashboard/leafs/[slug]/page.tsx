import { notFound, redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import {
  infrastructureClient,
  InfrastructureApiError,
} from "@/lib/infrastructure-client";
import { ProjectDashboard } from "@/components/projects/project-dashboard";

export async function generateMetadata({
  params,
}: {
  params: Promise<{ slug: string }>;
}) {
  const { slug } = await params;
  return { title: `${slug} — Lettuce` };
}

export default async function LeafDashboardPage({
  params,
}: {
  params: Promise<{ slug: string }>;
}) {
  const session = await auth();
  if (!session?.user) redirect("/sign-in");

  const { slug } = await params;

  // Fetch the leaf first — ownership check must happen outside try/catch
  // to avoid swallowing Next.js redirect() throws.
  let fullLeaf;
  try {
    fullLeaf = await infrastructureClient.getLeaf(slug);
  } catch (err) {
    if (err instanceof InfrastructureApiError && err.status === 404) {
      notFound();
    }
    console.error(
      `[dashboard/${slug}] Failed to load leaf:`,
      err instanceof Error ? err.message : err,
    );
    return (
      <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
        <div className="rounded-lg border border-yellow-200 bg-yellow-50 p-6 text-center dark:border-yellow-900 dark:bg-yellow-950">
          <h2 className="text-lg font-semibold text-yellow-800 dark:text-yellow-200">
            Infrastructure Unavailable
          </h2>
          <p className="mt-2 text-sm text-yellow-700 dark:text-yellow-300">
            Unable to load leaf data. The infrastructure service may be down.
            Please try again later.
          </p>
        </div>
      </div>
    );
  }

  // Ownership check — outside try/catch so redirect() is never caught
  if (fullLeaf.creator_id !== session.user.id) {
    redirect(`/leafs/${slug}`);
  }

  // Fetch stats and aggregation with graceful degradation
  let stats;
  let aggregation;
  let infraError: string | null = null;

  try {
    const [statsResult, aggResult] = await Promise.all([
      infrastructureClient.getLeafStats(fullLeaf.id).catch(() => null),
      infrastructureClient.getAggregation(fullLeaf.id).catch(() => null),
    ]);

    stats = statsResult;
    aggregation = aggResult?.data ?? null;
  } catch (err) {
    console.error(
      `[dashboard/${slug}] Failed to load stats/aggregation:`,
      err instanceof Error ? err.message : err,
    );
    infraError =
      "Infrastructure service is unavailable. Some data may be missing.";
  }

  return (
    <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
      {infraError && (
        <div className="mb-4 rounded-lg border border-yellow-200 bg-yellow-50 px-4 py-3 text-sm text-yellow-800 dark:border-yellow-900 dark:bg-yellow-950 dark:text-yellow-200">
          {infraError}
        </div>
      )}
      <ProjectDashboard
        leaf={fullLeaf}
        initialStats={stats ?? null}
        aggregation={aggregation}
      />
    </div>
  );
}
