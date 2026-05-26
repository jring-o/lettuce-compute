"use client";

import { useCallback, useEffect, useState } from "react";
import { listWorkUnits } from "@/lib/actions/work-units";
import type {
  ListWorkUnitsParams,
  WorkUnitPriority,
  WorkUnitState,
  WorkUnitSummary,
} from "@/types/infrastructure";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";

const ALL_STATES: WorkUnitState[] = [
  "PENDING",
  "ASSIGNED",
  "RUNNING",
  "COMPLETED",
  "VALIDATED",
  "FAILED",
  "CANCELLED",
  "EXPIRED",
];

const ALL_PRIORITIES: WorkUnitPriority[] = ["LOW", "NORMAL", "HIGH", "CRITICAL"];

const STATE_BADGE_VARIANT: Record<
  WorkUnitState,
  "default" | "secondary" | "outline" | "destructive"
> = {
  PENDING: "secondary",
  ASSIGNED: "outline",
  RUNNING: "default",
  COMPLETED: "secondary",
  VALIDATED: "default",
  FAILED: "destructive",
  CANCELLED: "outline",
  EXPIRED: "outline",
};

const PRIORITY_BADGE_VARIANT: Record<
  WorkUnitPriority,
  "default" | "secondary" | "outline" | "destructive"
> = {
  LOW: "outline",
  NORMAL: "secondary",
  HIGH: "default",
  CRITICAL: "destructive",
};

type SortField = "state" | "priority" | "created_at";

interface WorkUnitTableProps {
  leafId: string;
}

export function WorkUnitTable({ leafId }: WorkUnitTableProps) {
  const [workUnits, setWorkUnits] = useState<WorkUnitSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [hasMore, setHasMore] = useState(false);
  const [loadingMore, setLoadingMore] = useState(false);

  // Filters
  const [stateFilter, setStateFilter] = useState<WorkUnitState | "ALL">("ALL");
  const [priorityFilter, setPriorityFilter] = useState<
    WorkUnitPriority | "ALL"
  >("ALL");
  const [flaggedOnly, setFlaggedOnly] = useState(false);

  // Sort
  const [sortField, setSortField] = useState<SortField>("created_at");
  const [sortOrder, setSortOrder] = useState<"asc" | "desc">("desc");

  const fetchWorkUnits = useCallback(
    async (cursor?: string) => {
      const params: ListWorkUnitsParams = { limit: 50 };
      if (stateFilter !== "ALL") params.state = stateFilter;
      if (priorityFilter !== "ALL") params.priority = priorityFilter;
      if (flaggedOnly) params.flagged_for_review = true;
      if (cursor) params.cursor = cursor;

      const result = await listWorkUnits(leafId, params);
      if ("data" in result) {
        return result.data;
      }
      return null;
    },
    [leafId, stateFilter, priorityFilter, flaggedOnly],
  );

  useEffect(() => {
    let cancelled = false;
    // eslint-disable-next-line react-compiler/react-compiler -- intentional loading toggle when filters change; deps stable so no cascade
    setLoading(true);
    fetchWorkUnits().then((result) => {
      if (cancelled) return;
      if (result) {
        setWorkUnits(result.data);
        setNextCursor(result.pagination.next_cursor);
        setHasMore(result.pagination.has_more);
      }
      setLoading(false);
    });
    return () => {
      cancelled = true;
    };
  }, [fetchWorkUnits]);

  const loadMore = async () => {
    if (!nextCursor || loadingMore) return;
    setLoadingMore(true);
    const result = await fetchWorkUnits(nextCursor);
    if (result) {
      setWorkUnits((prev) => [...prev, ...result.data]);
      setNextCursor(result.pagination.next_cursor);
      setHasMore(result.pagination.has_more);
    }
    setLoadingMore(false);
  };

  const handleSort = (field: SortField) => {
    if (sortField === field) {
      setSortOrder((prev) => (prev === "asc" ? "desc" : "asc"));
    } else {
      setSortField(field);
      setSortOrder("asc");
    }
  };

  const sortedUnits = [...workUnits].sort((a, b) => {
    const dir = sortOrder === "asc" ? 1 : -1;
    if (sortField === "created_at") {
      return (
        (new Date(a.created_at).getTime() - new Date(b.created_at).getTime()) *
        dir
      );
    }
    return a[sortField].localeCompare(b[sortField]) * dir;
  });

  const sortIndicator = (field: SortField) => {
    if (sortField !== field) return null;
    return sortOrder === "asc" ? " \u2191" : " \u2193";
  };

  return (
    <div data-testid="work-unit-table" className="space-y-4">
      {/* Filters */}
      <div className="flex flex-wrap items-center gap-3">
        <div className="flex items-center gap-2">
          <span className="text-sm text-muted-foreground">State:</span>
          <select
            value={stateFilter}
            onChange={(e) =>
              setStateFilter(e.target.value as WorkUnitState | "ALL")
            }
            className="rounded-md border border-border bg-background px-2 py-1 text-sm"
          >
            <option value="ALL">All</option>
            {ALL_STATES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </div>

        <div className="flex items-center gap-2">
          <span className="text-sm text-muted-foreground">Priority:</span>
          <select
            value={priorityFilter}
            onChange={(e) =>
              setPriorityFilter(e.target.value as WorkUnitPriority | "ALL")
            }
            className="rounded-md border border-border bg-background px-2 py-1 text-sm"
          >
            <option value="ALL">All</option>
            {ALL_PRIORITIES.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </select>
        </div>

        <label className="flex items-center gap-1.5 text-sm text-muted-foreground">
          <input
            type="checkbox"
            checked={flaggedOnly}
            onChange={(e) => setFlaggedOnly(e.target.checked)}
            className="rounded border-border"
          />
          Flagged for review
        </label>
      </div>

      {/* Table */}
      {loading ? (
        <div className="space-y-2">
          {Array.from({ length: 5 }).map((_, i) => (
            <Skeleton key={i} className="h-10 w-full" />
          ))}
        </div>
      ) : sortedUnits.length === 0 ? (
        <p className="py-8 text-center text-muted-foreground">
          {stateFilter !== "ALL" || priorityFilter !== "ALL" || flaggedOnly
            ? "No work units match filters"
            : "No work units yet"}
        </p>
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead
                  className="cursor-pointer select-none"
                  onClick={() => handleSort("state")}
                >
                  State{sortIndicator("state")}
                </TableHead>
                <TableHead
                  className="cursor-pointer select-none"
                  onClick={() => handleSort("priority")}
                >
                  Priority{sortIndicator("priority")}
                </TableHead>
                <TableHead>Volunteer</TableHead>
                <TableHead>Attempts</TableHead>
                <TableHead
                  className="cursor-pointer select-none"
                  onClick={() => handleSort("created_at")}
                >
                  Created{sortIndicator("created_at")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {sortedUnits.map((wu) => (
                <TableRow key={wu.id}>
                  <TableCell className="font-mono text-xs">
                    {wu.id.substring(0, 8)}
                  </TableCell>
                  <TableCell>
                    <Badge variant={STATE_BADGE_VARIANT[wu.state]}>
                      {wu.state}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <Badge variant={PRIORITY_BADGE_VARIANT[wu.priority]}>
                      {wu.priority}
                    </Badge>
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {wu.assigned_to
                      ? wu.assigned_to.substring(0, 8)
                      : "\u2014"}
                  </TableCell>
                  <TableCell>{wu.attempts}</TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {new Date(wu.created_at).toLocaleString()}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>

          {hasMore && (
            <div className="flex justify-center">
              <Button
                variant="outline"
                onClick={loadMore}
                disabled={loadingMore}
              >
                {loadingMore ? "Loading..." : "Load More"}
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  );
}
