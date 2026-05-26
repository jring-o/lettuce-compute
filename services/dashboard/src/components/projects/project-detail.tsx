import { BarChart3, Box, Clock, Cpu, Eye, FileCode, Gpu, HardDrive, MemoryStick, Users } from "lucide-react";
import Link from "next/link";
import type { AggregationResult, TaskPattern } from "@/types/infrastructure";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Separator } from "@/components/ui/separator";
import { formatDate, formatMemory, formatNumber, LEAF_STATE_VARIANTS, TASK_PATTERN_LABELS } from "@/lib/utils";
import type { Leaf, LeafStats } from "@/types/infrastructure";
import { MarkdownRenderer } from "./markdown-renderer";
import { ContributeSection } from "./contribute-section";

interface CreatorInfo {
  username: string;
  displayName: string | null;
  createdAt: Date;
}

interface ProjectDetailProps {
  leaf: Leaf;
  stats: LeafStats | null;
  creator: CreatorInfo | null;
  serverHost: string;
  aggregation?: AggregationResult | null;
  hasVisualization?: boolean;
}

const PATTERN_DESCRIPTIONS: Record<TaskPattern, string> = {
  PARAMETER_SWEEP: "Explores a defined parameter space by running each combination as an independent work unit.",
  MAP_REDUCE: "Splits input data into chunks, processes each in parallel, then reduces results.",
  MONTE_CARLO: "Runs many independent random trials and aggregates statistical results.",
  CUSTOM: "Uses researcher-uploaded work units with custom input data.",
};

function formatDuration(seconds: number | null): string {
  if (seconds == null || seconds <= 0) return "\u2014";
  if (seconds < 60) return `${Math.round(seconds)}s`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.round((seconds % 3600) / 60);
  return `${hours}h ${minutes}m`;
}

export function ProjectDetail({
  leaf,
  stats,
  creator,
  serverHost,
  aggregation,
  hasVisualization,
}: ProjectDetailProps) {
  const agreementRate = stats?.agreement_rate ?? null;
  const isContainer = leaf.execution_config?.runtime === "CONTAINER";

  return (
    <div data-testid="leaf-detail" className="grid gap-8 lg:grid-cols-3">
      {/* Main content */}
      <div className="space-y-6 lg:col-span-2">
        <div>
          <div className="flex flex-wrap items-center gap-3">
            <h1 className="text-3xl font-bold tracking-tight" data-testid="leaf-name">
              {leaf.name}
            </h1>
            <Badge
              variant={LEAF_STATE_VARIANTS[leaf.state] ?? "secondary"}
              data-testid="state-badge"
            >
              {leaf.state}
            </Badge>
            {hasVisualization && (
              <Link
                href={`/leafs/${leaf.slug}/visualize`}
                className="inline-flex items-center gap-1.5 rounded-lg bg-primary px-2.5 py-1 text-sm font-medium text-primary-foreground hover:bg-primary/80 transition-colors"
                data-testid="visualize-link"
              >
                <Eye className="size-4" />
                Visualize
              </Link>
            )}
          </div>
          {leaf.research_area && (
            <div className="mt-2 flex flex-wrap gap-1">
              <Badge variant="secondary" data-testid="research-area-badge">
                {leaf.research_area}
              </Badge>
            </div>
          )}
        </div>

        <Separator />

        <MarkdownRenderer content={leaf.description} />

        <Separator />

        <ContributeSection serverHost={serverHost} />
      </div>

      {/* Sidebar */}
      <div className="space-y-4">
        {/* Creator card */}
        {creator && (
          <Card data-testid="creator-card">
            <CardHeader>
              <CardTitle className="text-sm font-medium text-muted-foreground">
                Created by
              </CardTitle>
            </CardHeader>
            <CardContent>
              <span
                className="text-sm font-medium"
                data-testid="creator-username"
              >
                @{creator.username}
              </span>
              {creator.displayName && (
                <p className="text-sm text-muted-foreground" data-testid="creator-display-name">
                  {creator.displayName}
                </p>
              )}
              <p className="mt-1 text-xs text-muted-foreground" data-testid="creator-member-since">
                Member since {formatDate(creator.createdAt)}
              </p>
            </CardContent>
          </Card>
        )}

        {/* Runtime info card */}
        <Card data-testid="runtime-card">
          <CardHeader>
            <CardTitle className="text-sm font-medium text-muted-foreground">
              Runtime
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-2">
            <div className="flex items-center gap-2 text-sm">
              {isContainer ? (
                <>
                  <Box className="size-4 text-muted-foreground" />
                  <span data-testid="runtime-type">Container</span>
                </>
              ) : (
                <>
                  <FileCode className="size-4 text-muted-foreground" />
                  <span data-testid="runtime-type">Native Binary</span>
                </>
              )}
            </div>
            {isContainer &&
              leaf.execution_config?.image && (
                <div className="text-sm text-muted-foreground" data-testid="container-image">
                  <code className="rounded bg-muted px-1.5 py-0.5 text-xs">
                    {leaf.execution_config.image}
                  </code>
                </div>
              )}
            {leaf.execution_config?.gpu_required && (
              <div className="flex items-center gap-2 text-sm">
                <Gpu className="size-4 text-muted-foreground" />
                <span data-testid="gpu-requirement">
                  GPU Required
                  {leaf.execution_config.gpu_type !== "ANY"
                    ? ` (${leaf.execution_config.gpu_type})`
                    : ""}
                  {leaf.execution_config.min_vram_gb > 0
                    ? ` · ${leaf.execution_config.min_vram_gb * 1024} MB VRAM`
                    : ""}
                </span>
              </div>
            )}
          </CardContent>
        </Card>

        {/* Resource requirements card */}
        <Card data-testid="resource-requirements-card">
          <CardHeader>
            <CardTitle className="text-sm font-medium text-muted-foreground">
              Resource Requirements
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-2">
            <div className="flex items-center gap-2 text-sm">
              <Cpu className="size-4 text-muted-foreground" />
              <span data-testid="cpu-requirement">
                {leaf.resource_requirements?.min_cpu_cores ?? 1} CPU core
                {(leaf.resource_requirements?.min_cpu_cores ?? 1) !== 1 ? "s" : ""}
              </span>
            </div>
            <div className="flex items-center gap-2 text-sm">
              <MemoryStick className="size-4 text-muted-foreground" />
              <span data-testid="memory-requirement">
                {leaf.execution_config?.max_memory_mb
                  ? `${formatMemory(leaf.execution_config.max_memory_mb)} RAM`
                  : "—"}
              </span>
            </div>
            <div className="flex items-center gap-2 text-sm">
              <HardDrive className="size-4 text-muted-foreground" />
              <span data-testid="disk-requirement">
                {leaf.resource_requirements?.min_disk_mb ?? 1024} MB disk
              </span>
            </div>
          </CardContent>
        </Card>

        {/* Statistics card */}
        {stats && (
          <Card data-testid="statistics-card">
            <CardHeader>
              <CardTitle className="text-sm font-medium text-muted-foreground">
                Statistics
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-2 text-sm">
              <div className="flex items-center justify-between">
                <span className="flex items-center gap-2 text-muted-foreground">
                  <Users className="size-4" />
                  Active Volunteers
                </span>
                <span className="font-medium" data-testid="stat-volunteers">
                  {formatNumber(stats.active_volunteers)}
                </span>
              </div>
              <div className="flex items-center justify-between">
                <span className="text-muted-foreground">Work Units Completed</span>
                <span className="font-medium" data-testid="stat-completed">
                  {formatNumber(stats.work_units_validated)} / {formatNumber(stats.total_work_units)}
                </span>
              </div>
              <div className="flex items-center justify-between">
                <span className="text-muted-foreground">Total Credit</span>
                <span className="font-medium" data-testid="stat-credit">
                  {formatNumber(stats.total_credit_granted)}
                </span>
              </div>
              <div className="flex items-center justify-between">
                <span className="flex items-center gap-2 text-muted-foreground">
                  <Clock className="size-4" />
                  Avg. Completion Time
                </span>
                <span className="font-medium" data-testid="stat-avg-time">
                  {formatDuration(stats.avg_completion_seconds)}
                </span>
              </div>
              {agreementRate !== null && (
                <div className="flex items-center justify-between">
                  <span className="text-muted-foreground">Agreement Rate</span>
                  <span className="font-medium" data-testid="stat-agreement">
                    {(agreementRate * 100).toFixed(1)}%
                  </span>
                </div>
              )}
            </CardContent>
          </Card>
        )}

        {/* Task pattern card */}
        <Card data-testid="pattern-card">
          <CardHeader>
            <CardTitle className="text-sm font-medium text-muted-foreground">
              Task Pattern
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-2">
            <div className="flex items-center gap-2 text-sm">
              <BarChart3 className="size-4 text-muted-foreground" />
              <span className="font-medium" data-testid="pattern-name">
                {TASK_PATTERN_LABELS[leaf.task_pattern] ?? leaf.task_pattern}
              </span>
            </div>
            <p className="text-xs text-muted-foreground" data-testid="pattern-description">
              {PATTERN_DESCRIPTIONS[leaf.task_pattern] ?? ""}
            </p>
            {leaf.task_pattern === "MAP_REDUCE" && leaf.data_config?.splitting_strategy && (
              <div className="text-xs text-muted-foreground">
                Splitting: <span className="font-medium">{leaf.data_config.splitting_strategy}</span>
                {Boolean(leaf.data_config.aggregation_config?.reducer_type) && (
                  <> · Reducer: <span className="font-medium">{String(leaf.data_config.aggregation_config?.reducer_type)}</span></>
                )}
              </div>
            )}
            {leaf.task_pattern === "MONTE_CARLO" && leaf.data_config?.aggregation_config && (
              <div className="text-xs text-muted-foreground">
                {Boolean(leaf.data_config.splitting_config?.num_trials) && (
                  <>Trials: <span className="font-medium">{String(leaf.data_config.splitting_config?.num_trials)}</span> · </>
                )}
                Seed strategy: <span className="font-medium">{String(leaf.data_config.splitting_config?.seed_strategy ?? "hash")}</span>
              </div>
            )}
            {leaf.task_pattern === "CUSTOM" && (
              <div className="text-xs text-muted-foreground">
                Custom work unit upload via API
              </div>
            )}
          </CardContent>
        </Card>

        {/* Aggregation results */}
        {aggregation && aggregation.status !== "no_aggregation" && (
          <Card data-testid="aggregation-card">
            <CardHeader>
              <CardTitle className="text-sm font-medium text-muted-foreground">
                Results
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-2 text-sm">
              <div className="flex items-center justify-between">
                <span className="text-muted-foreground">Status</span>
                <Badge variant={aggregation.status === "complete" ? "default" : "secondary"}>
                  {aggregation.status}
                </Badge>
              </div>
              <div className="flex items-center justify-between">
                <span className="text-muted-foreground">Aggregated</span>
                <span className="font-medium">
                  {formatNumber(aggregation.work_units_aggregated)} / {formatNumber(aggregation.work_units_total)}
                </span>
              </div>
            </CardContent>
          </Card>
        )}
      </div>
    </div>
  );
}
