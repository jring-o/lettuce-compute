"use client";

import { useCallback, useState, useTransition } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { Button } from "@/components/ui/button";
import type { Pagination } from "@/types/infrastructure";
import {
  listPublicLeafs,
  type LeafWithStats,
} from "@/lib/actions/public-projects";
import type { ResearchArea } from "@/lib/validations/project";
import { BrowserControls } from "./browser-controls";
import { ProjectCard } from "./project-card";
import { ProjectCardSkeleton } from "./project-card-skeleton";

interface ProjectBrowserProps {
  initialLeafs: LeafWithStats[];
  initialPagination: Pagination;
  researchAreas: ResearchArea[];
}

export function ProjectBrowser({
  initialLeafs,
  initialPagination,
  researchAreas,
}: ProjectBrowserProps) {
  const router = useRouter();
  const searchParams = useSearchParams();
  const [isPending, startTransition] = useTransition();

  const search = searchParams.get("search") ?? "";
  const researchArea = searchParams.get("research_area") ?? "";
  const sort = searchParams.get("sort") ?? "updated_at";

  const [leafs, setLeafs] = useState(initialLeafs);
  const [pagination, setPagination] = useState(initialPagination);
  const [isLoadingMore, setIsLoadingMore] = useState(false);

  const updateUrl = useCallback(
    (params: Record<string, string>) => {
      const current = new URLSearchParams(searchParams.toString());
      for (const [key, value] of Object.entries(params)) {
        if (value) {
          current.set(key, value);
        } else {
          current.delete(key);
        }
      }
      const qs = current.toString();
      router.push(qs ? `/leafs?${qs}` : "/leafs");
    },
    [router, searchParams],
  );

  const fetchLeafs = useCallback(
    (overrides: Record<string, string>) => {
      const merged = {
        search: overrides.search ?? search,
        research_area: overrides.research_area ?? researchArea,
        sort: overrides.sort ?? sort,
      };
      startTransition(async () => {
        const result = await listPublicLeafs({
          search: merged.search || undefined,
          research_area: merged.research_area || undefined,
          sort: (merged.sort as "updated_at" | "created_at") || undefined,
          limit: 12,
        });
        if ("data" in result) {
          setLeafs(result.data.leafs);
          setPagination(result.data.pagination);
        }
      });
    },
    [search, researchArea, sort],
  );

  const handleSearchChange = useCallback(
    (value: string) => {
      updateUrl({ search: value });
      fetchLeafs({ search: value });
    },
    [updateUrl, fetchLeafs],
  );

  const handleResearchAreaChange = useCallback(
    (value: string) => {
      updateUrl({ research_area: value });
      fetchLeafs({ research_area: value });
    },
    [updateUrl, fetchLeafs],
  );

  const handleSortChange = useCallback(
    (value: string) => {
      updateUrl({ sort: value === "updated_at" ? "" : value });
      fetchLeafs({ sort: value });
    },
    [updateUrl, fetchLeafs],
  );

  const handleLoadMore = useCallback(async () => {
    if (!pagination.next_cursor) return;
    setIsLoadingMore(true);
    try {
      const result = await listPublicLeafs({
        search: search || undefined,
        research_area: researchArea || undefined,
        sort: (sort as "updated_at" | "created_at") || undefined,
        cursor: pagination.next_cursor,
        limit: 12,
      });
      if ("data" in result) {
        setLeafs((prev) => [...prev, ...result.data.leafs]);
        setPagination(result.data.pagination);
      }
    } finally {
      setIsLoadingMore(false);
    }
  }, [pagination.next_cursor, search, researchArea, sort]);

  const hasActiveFilters = search || researchArea;

  return (
    <div className="space-y-6">
      <BrowserControls
        researchAreas={researchAreas}
        search={search}
        researchArea={researchArea}
        sort={sort}
        onSearchChange={handleSearchChange}
        onResearchAreaChange={handleResearchAreaChange}
        onSortChange={handleSortChange}
      />

      {isPending ? (
        <div
          className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3"
          data-testid="loading-grid"
        >
          {Array.from({ length: 6 }).map((_, i) => (
            <ProjectCardSkeleton key={i} />
          ))}
        </div>
      ) : leafs.length === 0 ? (
        hasActiveFilters ? (
          <div className="py-16 text-center" data-testid="empty-search">
            <p className="text-lg text-muted-foreground">
              No leafs found matching &ldquo;{search || researchArea}&rdquo;.
              Try a different search.
            </p>
          </div>
        ) : (
          <div className="py-16 text-center" data-testid="empty-state">
            <p className="text-lg text-muted-foreground">
              No active leafs yet.
            </p>
          </div>
        )
      ) : (
        <>
          <div
            className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3"
            data-testid="leaf-grid"
          >
            {leafs.map((leaf) => (
              <ProjectCard key={leaf.id} leaf={leaf} />
            ))}
            {isLoadingMore &&
              Array.from({ length: 3 }).map((_, i) => (
                <ProjectCardSkeleton key={`loading-${i}`} />
              ))}
          </div>

          {pagination.has_more && !isLoadingMore && (
            <div className="flex justify-center">
              <Button
                variant="outline"
                onClick={handleLoadMore}
                data-testid="load-more-button"
              >
                Load More Leafs
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  );
}
