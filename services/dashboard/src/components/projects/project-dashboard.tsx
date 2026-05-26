"use client";

import { useCallback, useEffect, useState, useTransition } from "react";
import { getLeafStats } from "@/lib/actions/stats";
import { triggerLeafAggregation } from "@/lib/actions/aggregation";
import type { AggregationResult, Leaf, LeafStats } from "@/types/infrastructure";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Separator } from "@/components/ui/separator";
import { formatNumber, LEAF_STATE_VARIANTS, TASK_PATTERN_LABELS } from "@/lib/utils";
import { DashboardMetrics } from "./dashboard-metrics";
import { WorkUnitStatus } from "./work-unit-status";
import { WorkUnitTable } from "./work-unit-table";
import { ProjectActions } from "./project-actions";

const REFRESH_INTERVAL_MS = 30_000;

interface ProjectDashboardProps {
  leaf: Leaf;
  initialStats: LeafStats | null;
  aggregation?: AggregationResult | null;
}

export function ProjectDashboard({
  leaf,
  initialStats,
  aggregation: initialAggregation,
}: ProjectDashboardProps) {
  const [stats, setStats] = useState(initialStats);
  const [aggregation, setAggregation] = useState(initialAggregation ?? null);
  const [isPending, startTransition] = useTransition();

  const refreshStats = useCallback(async () => {
    const result = await getLeafStats(leaf.id);
    if ("data" in result) {
      setStats(result.data);
    }
  }, [leaf.id]);

  useEffect(() => {
    if (leaf.state === "ARCHIVED" || leaf.state === "DRAFT") return;

    const interval = setInterval(refreshStats, REFRESH_INTERVAL_MS);
    return () => clearInterval(interval);
  }, [leaf.state, refreshStats]);

  const handleTriggerAggregation = () => {
    startTransition(async () => {
      const result = await triggerLeafAggregation(leaf.id);
      if ("data" in result) {
        setAggregation(result.data);
      }
    });
  };

  const isSetup = leaf.state === "DRAFT" || leaf.state === "CONFIGURING";

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex items-center gap-3">
          <h1 className="text-2xl font-bold tracking-tight">{leaf.name}</h1>
          <Badge variant={LEAF_STATE_VARIANTS[leaf.state] ?? "secondary"}>
            {leaf.state}
          </Badge>
          <Badge variant="outline" className="text-xs">
            {TASK_PATTERN_LABELS[leaf.task_pattern] ?? leaf.task_pattern}
          </Badge>
        </div>
        <ProjectActions
          leafId={leaf.id}
          leafState={leaf.state}
          leafSlug={leaf.slug}
          hasCompletedWorkUnits={(stats?.work_units_validated ?? 0) > 0}
        />
      </div>

      {leaf.description && (
        <p className="text-sm text-muted-foreground">{leaf.description}</p>
      )}

      <Separator />

      {isSetup ? (
        <div className="py-16 text-center">
          <p className="text-lg text-muted-foreground">
            Leaf is being set up...
          </p>
          <p className="mt-1 text-sm text-muted-foreground">
            Metrics will appear once the leaf is active.
          </p>
        </div>
      ) : (
        <>
          {/* Metrics */}
          {stats && (
            <DashboardMetrics stats={stats} isOngoing={leaf.is_ongoing} />
          )}

          {/* Aggregation */}
          <Card data-testid="aggregation-section">
            <CardHeader>
              <div className="flex items-center justify-between">
                <CardTitle className="text-sm font-medium text-muted-foreground">
                  Aggregation
                </CardTitle>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={handleTriggerAggregation}
                  disabled={isPending}
                  data-testid="trigger-aggregation"
                >
                  {isPending ? "Aggregating…" : "Trigger Aggregation"}
                </Button>
              </div>
            </CardHeader>
            <CardContent>
              {aggregation ? (
                <div className="space-y-3">
                  <div className="flex items-center justify-between text-sm">
                    <span className="text-muted-foreground">Status</span>
                    <Badge
                      variant={aggregation.status === "complete" ? "default" : "secondary"}
                      data-testid="aggregation-status"
                    >
                      {aggregation.status}
                    </Badge>
                  </div>
                  <div className="flex items-center justify-between text-sm">
                    <span className="text-muted-foreground">Work Units</span>
                    <span className="font-medium" data-testid="aggregation-count">
                      {formatNumber(aggregation.work_units_aggregated)} / {formatNumber(aggregation.work_units_total)}
                    </span>
                  </div>
                  {aggregation.aggregated_at && (
                    <div className="flex items-center justify-between text-sm">
                      <span className="text-muted-foreground">Last Aggregated</span>
                      <span className="text-xs" data-testid="aggregation-time">
                        {new Date(aggregation.aggregated_at).toLocaleString()}
                      </span>
                    </div>
                  )}
                  {aggregation.status !== "no_aggregation" && (
                    <div className="flex gap-2 pt-2">
                      {aggregation.result && (
                        <a
                          href={`/api/download/${leaf.id}?format=json`}
                          download
                          className="inline-flex items-center rounded-md border px-3 py-1.5 text-xs font-medium hover:bg-accent"
                          data-testid="download-json"
                        >
                          Download JSON
                        </a>
                      )}
                      {aggregation.result_csv && (
                        <a
                          href={`/api/download/${leaf.id}?format=csv`}
                          download
                          className="inline-flex items-center rounded-md border px-3 py-1.5 text-xs font-medium hover:bg-accent"
                          data-testid="download-csv"
                        >
                          Download CSV
                        </a>
                      )}
                    </div>
                  )}
                  {/* Pattern-specific details */}
                  {leaf.task_pattern === "MAP_REDUCE" &&
                    Boolean(leaf.data_config?.aggregation_config?.reducer_type) && (
                      <div className="text-xs text-muted-foreground pt-1">
                        Reducer: {String(leaf.data_config?.aggregation_config?.reducer_type)}
                      </div>
                    )}
                  {leaf.task_pattern === "MONTE_CARLO" &&
                    aggregation.result &&
                    Boolean((aggregation.result as Record<string, unknown>).statistics) && (
                      <div className="text-xs text-muted-foreground pt-1">
                        Statistical summary available in results
                      </div>
                    )}
                </div>
              ) : (
                <p className="text-sm text-muted-foreground">
                  No aggregation results yet. Trigger aggregation once work units are validated.
                </p>
              )}
            </CardContent>
          </Card>

          {/* Status breakdown */}
          {stats && (
            <div>
              <h2 className="mb-3 text-sm font-medium text-muted-foreground">
                Work Unit Distribution
              </h2>
              <WorkUnitStatus stats={stats} />
            </div>
          )}

          <Separator />

          {/* Work unit table */}
          <div>
            <h2 className="mb-3 text-sm font-medium text-muted-foreground">
              Work Units
            </h2>
            <WorkUnitTable leafId={leaf.id} />
          </div>
        </>
      )}
    </div>
  );
}
