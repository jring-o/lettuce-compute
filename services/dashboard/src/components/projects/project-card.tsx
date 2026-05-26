import Link from "next/link";
import { Box, Cpu, FileCode, Gpu, Users } from "lucide-react";
import type { TaskPattern } from "@/types/infrastructure";
import { formatMemory } from "@/lib/utils";

import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import type { LeafWithStats } from "@/lib/actions/public-projects";

function truncate(text: string, max: number): string {
  if (text.length <= max) return text;
  return text.slice(0, max) + "\u2026";
}

function getHealthBadge(
  state: string,
  createdAt: string,
): { label: string; className: string } {
  if (state === "PAUSED") {
    return { label: "Paused", className: "bg-yellow-100 text-yellow-800 dark:bg-yellow-900 dark:text-yellow-200" };
  }
  const sevenDaysAgo = Date.now() - 7 * 24 * 60 * 60 * 1000;
  if (new Date(createdAt).getTime() > sevenDaysAgo) {
    return { label: "New", className: "bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200" };
  }
  return { label: "Active", className: "bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-200" };
}

function formatResources(
  requirements: LeafWithStats["resource_requirements"],
): { icon: typeof Cpu; text: string } {
  if (!requirements) {
    return { icon: Cpu, text: "CPU" };
  }
  if (requirements.gpu_required) {
    const vram = requirements.gpu_min_vram_mb
      ? ` \u00b7 ${requirements.gpu_min_vram_mb} MB VRAM`
      : "";
    return { icon: Gpu, text: `GPU${vram}` };
  }
  const cores = requirements.min_cpu_cores
    ? `${requirements.min_cpu_cores} core${requirements.min_cpu_cores > 1 ? "s" : ""}`
    : "";
  const memory = requirements.max_memory_mb
    ? `${formatMemory(requirements.max_memory_mb)} RAM`
    : "";
  const parts = ["CPU", cores, memory].filter(Boolean);
  return { icon: Cpu, text: parts.join(" \u00b7 ") };
}

const PATTERN_BADGES: Record<TaskPattern, { label: string; className: string }> = {
  PARAMETER_SWEEP: { label: "Parameter Sweep", className: "bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200" },
  MAP_REDUCE: { label: "Map-Reduce", className: "bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-200" },
  MONTE_CARLO: { label: "Monte Carlo", className: "bg-purple-100 text-purple-800 dark:bg-purple-900 dark:text-purple-200" },
  CUSTOM: { label: "Custom", className: "bg-gray-100 text-gray-800 dark:bg-gray-900 dark:text-gray-200" },
};

interface ProjectCardProps {
  leaf: LeafWithStats;
}

export function ProjectCard({ leaf }: ProjectCardProps) {
  const health = getHealthBadge(leaf.state, leaf.created_at);
  const resources = formatResources(leaf.resource_requirements);
  const ResourceIcon = resources.icon;
  const stats = leaf.stats;
  const progressPct =
    stats && stats.total_work_units > 0
      ? Math.round((stats.work_units_validated / stats.total_work_units) * 1000) / 10
      : 0;

  return (
    <Link href={`/leafs/${leaf.slug}`} className="block" data-testid="leaf-card">
      <Card className="h-full transition-shadow hover:shadow-md">
        <CardHeader>
          <div className="flex items-start justify-between gap-2">
            <CardTitle className="line-clamp-1">{leaf.name}</CardTitle>
            <span
              data-testid="health-badge"
              className={`inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-medium ${health.className}`}
            >
              {health.label}
            </span>
          </div>
          <div className="flex flex-wrap gap-1 mt-1">
            {leaf.task_pattern && (
              <span
                data-testid="pattern-badge"
                className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${PATTERN_BADGES[leaf.task_pattern]?.className ?? PATTERN_BADGES.CUSTOM.className}`}
              >
                {PATTERN_BADGES[leaf.task_pattern]?.label ?? leaf.task_pattern}
              </span>
            )}
            {leaf.research_area && (
              <Badge variant="secondary" data-testid="research-area-badge">
                {leaf.research_area}
              </Badge>
            )}
            <Badge variant="outline" className="text-xs" data-testid="runtime-badge">
              {leaf.runtime === "CONTAINER" ? (
                <><Box className="size-3 mr-1" /> Container</>
              ) : (
                <><FileCode className="size-3 mr-1" /> Native</>
              )}
            </Badge>
          </div>
        </CardHeader>

        <CardContent className="flex-1 space-y-3">
          <p className="text-sm text-muted-foreground" data-testid="description">
            {truncate(leaf.description, 150)}
          </p>
          <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <ResourceIcon className="size-3.5" />
            <span data-testid="resource-requirements">{resources.text}</span>
          </div>
        </CardContent>

        <CardFooter className="gap-4 text-xs text-muted-foreground">
          <div className="flex items-center gap-1">
            <Users className="size-3.5" />
            <span data-testid="volunteer-count">
              {stats ? stats.active_volunteers : "\u2014"}
            </span>
          </div>
          <div className="flex flex-1 items-center gap-2">
            {leaf.is_ongoing ? (
              <span data-testid="progress-label">ongoing</span>
            ) : (
              <>
                <Progress value={progressPct} className="flex-1" data-testid="progress-bar" />
                <span data-testid="progress-label">{progressPct}%</span>
              </>
            )}
          </div>
        </CardFooter>
      </Card>
    </Link>
  );
}
