import type { Metadata } from "next";
import { unstable_cache } from "next/cache";

import { listPublicLeafs } from "@/lib/actions/public-projects";
import { listResearchAreas } from "@/lib/actions/research-areas";
import { ProjectBrowser } from "@/components/projects/project-browser";

export const revalidate = 60; // ISR: revalidate leaf listings every 60 seconds

export const metadata: Metadata = {
  title: "Leafs — Lettuce",
  description: "Browse distributed compute leafs looking for volunteers",
};

// Cache research areas (changes rarely)
const getCachedResearchAreas = unstable_cache(
  async () => listResearchAreas(),
  ["research-areas"],
  { revalidate: 300 },
);

export default async function LeafsPage() {
  const [result, researchAreas] = await Promise.all([
    listPublicLeafs({}),
    getCachedResearchAreas(),
  ]);

  const { leafs, pagination } = "data" in result
    ? result.data
    : { leafs: [], pagination: { next_cursor: null, has_more: false } };

  return (
    <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
      <div className="mb-8">
        <h1 className="text-2xl font-bold tracking-tight">Leafs</h1>
        <p className="mt-1 text-muted-foreground">
          Browse distributed compute leafs looking for volunteers.
        </p>
      </div>

      <ProjectBrowser
        initialLeafs={leafs}
        initialPagination={pagination}
        researchAreas={researchAreas}
      />
    </div>
  );
}
