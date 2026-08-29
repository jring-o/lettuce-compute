import { useState, useCallback, useRef, useEffect, useMemo } from "react";
import { Download, Filter, ChevronRight, ChevronDown, Copy, Eye, X } from "lucide-react";
import { useHistory, filtersToParams, type HistoryFilters } from "@/hooks/use-history";
import { useCredit } from "@/hooks/use-credit";
import { useHeads } from "@/hooks/use-heads";
import { useClient } from "@/hooks/use-api";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn, formatDuration, formatCredit } from "@/lib/utils";
import { VizFrame } from "@/components/viz/VizFrame";
import type { HistoryEntry, CreditSummary, ResultEntry } from "@/api/client";

function formatTime(iso: string): string {
  return new Date(iso).toLocaleTimeString([], {
    hour: "numeric",
    minute: "2-digit",
  });
}

function dayLabel(dateStr: string): string {
  const date = new Date(dateStr);
  const today = new Date();
  const yesterday = new Date();
  yesterday.setDate(yesterday.getDate() - 1);

  if (date.toDateString() === today.toDateString()) return "Today";
  if (date.toDateString() === yesterday.toDateString()) return "Yesterday";
  return date.toLocaleDateString(undefined, {
    month: "long",
    day: "numeric",
    year: "numeric",
  });
}

function groupByDay(entries: HistoryEntry[]): Map<string, HistoryEntry[]> {
  const groups = new Map<string, HistoryEntry[]>();
  for (const entry of entries) {
    const dateKey = new Date(entry.completed_at).toDateString();
    if (!groups.has(dateKey)) groups.set(dateKey, []);
    groups.get(dateKey)!.push(entry);
  }
  return groups;
}

const STATUS_STYLES = {
  accepted: "bg-green-500/10 text-green-600 border-green-500/20",
  pending: "bg-yellow-500/10 text-yellow-600 border-yellow-500/20",
  rejected: "bg-red-500/10 text-red-600 border-red-500/20",
} as const;

const STATUS_LABELS = {
  accepted: "Validated",
  pending: "Pending",
  rejected: "Rejected",
} as const;

interface HistoryRowProps {
  entry: HistoryEntry;
  hasResult: boolean;
  onViewResult: (workUnitId: string) => void;
}

function HistoryRow({ entry, hasResult, onViewResult }: HistoryRowProps) {
  const [expanded, setExpanded] = useState(false);
  const [copied, setCopied] = useState(false);
  const pausedSeconds = entry.duration_seconds - entry.cpu_seconds;

  const handleCopy = (e: React.MouseEvent) => {
    e.stopPropagation();
    navigator.clipboard.writeText(entry.work_unit_id);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  const handleView = (e: React.MouseEvent) => {
    e.stopPropagation();
    onViewResult(entry.work_unit_id);
  };

  return (
    <div>
      <div
        className="flex items-center gap-3 py-2 px-3 rounded-md hover:bg-muted/50 transition-colors cursor-pointer"
        onClick={() => setExpanded(!expanded)}
      >
        {expanded ? (
          <ChevronDown className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
        ) : (
          <ChevronRight className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
        )}
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2">
            <span className="font-medium text-sm truncate">
              {entry.leaf_name}
            </span>
            <code className="text-xs text-muted-foreground font-mono shrink-0">
              {entry.work_unit_id.slice(0, 8)}
            </code>
          </div>
          <div className="flex items-center gap-3 text-xs text-muted-foreground mt-0.5">
            <span>{formatTime(entry.completed_at)}</span>
            {entry.head_name && <span>{entry.head_name}</span>}
            <span>{formatDuration(entry.cpu_seconds)}</span>
          </div>
        </div>
        <span className="text-sm font-medium text-primary shrink-0">
          +{formatCredit(entry.credit_earned)}
        </span>
        <span
          className={cn(
            "inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium shrink-0",
            STATUS_STYLES[entry.validation_status]
          )}
        >
          {STATUS_LABELS[entry.validation_status]}
        </span>
      </div>
      {expanded && (
        <div className="bg-muted/30 px-6 py-3 text-xs rounded-md mx-3 mb-1">
          <div className="grid grid-cols-2 gap-x-8 gap-y-1.5">
            <div>
              <span className="text-muted-foreground">CPU Time</span>
              <span className="ml-2 font-medium">{formatDuration(entry.cpu_seconds)}</span>
            </div>
            <div>
              <span className="text-muted-foreground">Wall Clock</span>
              <span className="ml-2 font-medium">{formatDuration(entry.duration_seconds)}</span>
            </div>
            <div>
              <span className="text-muted-foreground">Time Paused</span>
              <span className="ml-2 font-medium">{formatDuration(Math.max(0, pausedSeconds))}</span>
            </div>
            <div>
              <span className="text-muted-foreground">Head</span>
              <span className="ml-2 font-medium">{entry.head_name || "—"}</span>
            </div>
            <div className="col-span-2 flex items-center gap-2">
              <span className="text-muted-foreground">Work Unit ID</span>
              <code className="font-mono font-medium">{entry.work_unit_id}</code>
              <button
                onClick={handleCopy}
                className="inline-flex items-center gap-1 text-muted-foreground hover:text-foreground transition-colors"
              >
                <Copy className="h-3 w-3" />
                <span>{copied ? "Copied" : "Copy"}</span>
              </button>
            </div>
            {hasResult && (
              <div className="col-span-2 pt-1">
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={handleView}
                  className="h-7 text-xs"
                >
                  <Eye className="h-3 w-3 mr-1" />
                  View Visualization
                </Button>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

function SkeletonRow() {
  return (
    <div className="flex items-center gap-3 py-2 px-3 animate-pulse">
      <div className="flex-1 space-y-2">
        <div className="h-4 w-32 bg-muted rounded" />
        <div className="h-3 w-24 bg-muted rounded" />
      </div>
      <div className="h-4 w-16 bg-muted rounded" />
      <div className="h-5 w-16 bg-muted rounded-full" />
    </div>
  );
}

function CreditBreakdown({ credit }: { credit: CreditSummary }) {
  if (credit.by_head && credit.by_head.length > 0) {
    const totalHead = credit.by_head.reduce((s, h) => s + h.credit, 0);
    return (
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm font-medium">Credit by Head</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          {credit.by_head.map((head) => {
            const headPct = totalHead > 0 ? (head.credit / totalHead) * 100 : 0;
            return (
              <div key={head.head_name} className="space-y-2">
                <div className="flex justify-between text-xs">
                  <span className="font-medium">{head.head_name}</span>
                  <span className="text-muted-foreground">
                    {formatCredit(head.credit)} ({Math.round(headPct)}%)
                  </span>
                </div>
                {head.leafs.map((leaf) => {
                  const leafPct =
                    head.credit > 0 ? (leaf.credit / head.credit) * 100 : 0;
                  return (
                    <div key={leaf.leaf_slug} className="space-y-1 pl-3">
                      <div className="flex justify-between text-xs">
                        <span className="truncate">{leaf.leaf_name}</span>
                        <span className="text-muted-foreground shrink-0 ml-2">
                          {formatCredit(leaf.credit)}
                        </span>
                      </div>
                      <div className="h-1.5 bg-secondary rounded-full overflow-hidden">
                        <div
                          className="h-full bg-primary rounded-full transition-all"
                          style={{ width: `${leafPct}%` }}
                        />
                      </div>
                    </div>
                  );
                })}
              </div>
            );
          })}
        </CardContent>
      </Card>
    );
  }

  // Fallback to by_leaf
  const byLeaf = credit.by_leaf;
  if (byLeaf.length === 0) return null;

  const sorted = [...byLeaf].sort((a, b) => b.credit - a.credit);
  const total = sorted.reduce((s, p) => s + p.credit, 0);
  const maxCredit = sorted[0]?.credit ?? 1;

  return (
    <Card>
      <CardHeader className="pb-3">
        <CardTitle className="text-sm font-medium">Credit by Leaf</CardTitle>
      </CardHeader>
      <CardContent className="space-y-2">
        {sorted.map((p) => {
          const pct = total > 0 ? (p.credit / total) * 100 : 0;
          const barPct = maxCredit > 0 ? (p.credit / maxCredit) * 100 : 0;
          return (
            <div key={p.leaf_name} className="space-y-1">
              <div className="flex justify-between text-xs">
                <span className="font-medium truncate">{p.leaf_name}</span>
                <span className="text-muted-foreground shrink-0 ml-2">
                  {formatCredit(p.credit)} ({Math.round(pct)}%)
                </span>
              </div>
              <div className="h-2 bg-secondary rounded-full overflow-hidden">
                <div
                  className="h-full bg-primary rounded-full transition-all"
                  style={{ width: `${barPct}%` }}
                />
              </div>
            </div>
          );
        })}
      </CardContent>
    </Card>
  );
}

interface ReplayModalState {
  open: boolean;
  leafSlug: string;
  vizBundlePath: string;
  replayData: Record<string, unknown> | null;
  loading: boolean;
  error: string | null;
}

export function HistoryPage() {
  const [filters, setFilters] = useState<HistoryFilters>({
    dateRange: "30d",
    validationStatus: "all",
  });
  const { entries, hasMore, isLoading, loadMore, error } = useHistory(filters);
  const { credit } = useCredit(30000);
  const { heads } = useHeads();
  const { client } = useClient();
  const [exporting, setExporting] = useState(false);
  const [exportProgress, setExportProgress] = useState("");
  const scrollRef = useRef<HTMLDivElement>(null);

  // Available results for replay
  const [availableResults, setAvailableResults] = useState<Map<string, ResultEntry>>(new Map());
  const [replayModal, setReplayModal] = useState<ReplayModalState>({
    open: false,
    leafSlug: "",
    vizBundlePath: "",
    replayData: null,
    loading: false,
    error: null,
  });

  // Fetch available results on mount
  useEffect(() => {
    if (!client) return;
    client.results().then((resp) => {
      const map = new Map<string, ResultEntry>();
      for (const r of resp.results) {
        map.set(r.work_unit_id, r);
      }
      setAvailableResults(map);
    }).catch(() => {
      // Results endpoint not available — no replay buttons shown
    });
  }, [client]);

  // Handle View button click
  const handleViewResult = useCallback(async (workUnitId: string) => {
    if (!client) return;

    const resultEntry = availableResults.get(workUnitId);
    if (!resultEntry) return;

    setReplayModal({
      open: true,
      leafSlug: resultEntry.leaf_slug,
      vizBundlePath: resultEntry.viz_bundle_path,
      replayData: null,
      loading: true,
      error: null,
    });

    try {
      const data = await client.resultData(workUnitId);
      setReplayModal((prev) => ({
        ...prev,
        replayData: data,
        loading: false,
      }));
    } catch {
      setReplayModal((prev) => ({
        ...prev,
        loading: false,
        error: "Result no longer available locally.",
      }));
    }
  }, [client, availableResults]);

  // Close modal
  const closeModal = useCallback(() => {
    setReplayModal({
      open: false,
      leafSlug: "",
      vizBundlePath: "",
      replayData: null,
      loading: false,
      error: null,
    });
  }, []);

  // Close on Escape
  useEffect(() => {
    if (!replayModal.open) return;
    const handleKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") closeModal();
    };
    window.addEventListener("keydown", handleKey);
    return () => window.removeEventListener("keydown", handleKey);
  }, [replayModal.open, closeModal]);

  // Infinite scroll
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const observer = new IntersectionObserver(
      (observerEntries) => {
        if (observerEntries[0].isIntersecting && hasMore && !isLoading) {
          loadMore();
        }
      },
      { root: null, threshold: 0.1 }
    );

    const sentinel = el.querySelector("[data-sentinel]");
    if (sentinel) observer.observe(sentinel);
    return () => observer.disconnect();
  }, [hasMore, isLoading, loadMore, entries.length]);

  // Unique leaf names from history
  const leafNames = useMemo(() => {
    const names = new Set<string>();
    for (const e of entries) names.add(e.leaf_name);
    return Array.from(names).sort();
  }, [entries]);

  // Export
  const handleExport = useCallback(
    async (format: "csv" | "json") => {
      if (!client) return;
      setExporting(true);
      try {
        const allEntries: HistoryEntry[] = [];
        let cursor: string | undefined;
        const baseParams = filtersToParams(filters);

        while (true) {
          const resp = await client.history({
            ...baseParams,
            limit: 200,
            cursor,
          });

          let filtered = resp.entries;
          if (filters.validationStatus !== "all") {
            filtered = filtered.filter(
              (e) => e.validation_status === filters.validationStatus
            );
          }
          allEntries.push(...filtered);
          setExportProgress(`Exporting... ${allEntries.length} entries`);

          if (!resp.pagination.has_more) break;
          cursor = resp.pagination.next_cursor;
        }

        let blob: Blob;
        let filename: string;

        if (format === "csv") {
          // Build leaf->head lookup from heads data
          const leafToHead = new Map<string, string>();
          for (const head of heads) {
            for (const leaf of head.leafs) {
              leafToHead.set(leaf.name, head.name);
            }
          }

          const header =
            "work_unit_id,leaf_name,head_name,completed_at,duration_seconds,cpu_seconds,credit_earned,validation_status";
          const rows = allEntries.map(
            (e) =>
              `${e.work_unit_id},"${e.leaf_name}","${e.head_name || (leafToHead.get(e.leaf_name) ?? "")}",${e.completed_at},${e.duration_seconds},${e.cpu_seconds},${e.credit_earned},${e.validation_status}`
          );
          blob = new Blob([header + "\n" + rows.join("\n")], {
            type: "text/csv",
          });
          filename = "lettuce-history.csv";
        } else {
          blob = new Blob([JSON.stringify(allEntries, null, 2)], {
            type: "application/json",
          });
          filename = "lettuce-history.json";
        }

        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = filename;
        a.click();
        URL.revokeObjectURL(url);
      } finally {
        setExporting(false);
        setExportProgress("");
      }
    },
    [client, filters, heads]
  );

  const grouped = groupByDay(entries);

  return (
    <div className="p-6 max-w-4xl mx-auto space-y-4">
      {/* Filters bar */}
      <div className="flex flex-wrap items-center gap-3">
        <Filter className="h-4 w-4 text-muted-foreground" />

        {/* Leaf filter */}
        <select
          value={filters.leafId ?? ""}
          onChange={(e) =>
            setFilters((f) => ({
              ...f,
              leafId: e.target.value || undefined,
            }))
          }
          className="h-9 rounded-md border border-input bg-background px-3 text-sm"
        >
          <option value="">All Leafs</option>
          {leafNames.map((name) => (
            <option key={name} value={name}>
              {name}
            </option>
          ))}
        </select>

        {/* Date range */}
        <select
          value={filters.dateRange}
          onChange={(e) =>
            setFilters((f) => ({
              ...f,
              dateRange: e.target.value as HistoryFilters["dateRange"],
            }))
          }
          className="h-9 rounded-md border border-input bg-background px-3 text-sm"
        >
          <option value="7d">Last 7 days</option>
          <option value="30d">Last 30 days</option>
          <option value="all">All time</option>
          <option value="custom">Custom</option>
        </select>

        {/* Custom date inputs */}
        {filters.dateRange === "custom" && (
          <>
            <input
              type="date"
              value={filters.customFrom?.split("T")[0] ?? ""}
              onChange={(e) =>
                setFilters((f) => ({
                  ...f,
                  customFrom: e.target.value
                    ? new Date(e.target.value).toISOString()
                    : undefined,
                }))
              }
              className="h-9 rounded-md border border-input bg-background px-2 text-sm"
            />
            <span className="text-xs text-muted-foreground">to</span>
            <input
              type="date"
              value={filters.customTo?.split("T")[0] ?? ""}
              onChange={(e) =>
                setFilters((f) => ({
                  ...f,
                  customTo: e.target.value
                    ? new Date(
                        e.target.value + "T23:59:59"
                      ).toISOString()
                    : undefined,
                }))
              }
              className="h-9 rounded-md border border-input bg-background px-2 text-sm"
            />
          </>
        )}

        {/* Validation status */}
        <select
          value={filters.validationStatus}
          onChange={(e) =>
            setFilters((f) => ({
              ...f,
              validationStatus:
                e.target.value as HistoryFilters["validationStatus"],
            }))
          }
          className="h-9 rounded-md border border-input bg-background px-3 text-sm"
        >
          <option value="all">All Status</option>
          <option value="accepted">Validated</option>
          <option value="pending">Pending</option>
          <option value="rejected">Rejected</option>
        </select>

        {/* Export */}
        <div className="ml-auto flex items-center gap-2">
          {exportProgress && (
            <span className="text-xs text-muted-foreground">
              {exportProgress}
            </span>
          )}
          <Button
            variant="outline"
            size="sm"
            disabled={exporting || entries.length === 0}
            onClick={() => handleExport("csv")}
          >
            <Download className="h-3.5 w-3.5 mr-1" />
            CSV
          </Button>
          <Button
            variant="outline"
            size="sm"
            disabled={exporting || entries.length === 0}
            onClick={() => handleExport("json")}
          >
            <Download className="h-3.5 w-3.5 mr-1" />
            JSON
          </Button>
        </div>
      </div>

      <div className="grid gap-4 lg:grid-cols-[1fr_280px]">
        {/* Timeline */}
        <div className="space-y-1">
          {error && (
            <p className="text-sm text-destructive">
              Failed to load history: {error.message}
            </p>
          )}

          {entries.length === 0 && !isLoading && (
            <div className="text-center py-12 text-muted-foreground">
              <p className="text-sm">No completed work units yet.</p>
              <p className="text-xs mt-1">
                Start contributing to see your history here.
              </p>
            </div>
          )}

          {Array.from(grouped.entries()).map(([dateKey, dayEntries]) => (
            <div key={dateKey}>
              <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-wider py-2 px-3 sticky top-0 bg-background/95 backdrop-blur-sm z-10">
                {dayLabel(dayEntries[0].completed_at)}
              </h3>
              <div className="space-y-px">
                {dayEntries.map((entry) => (
                  <HistoryRow
                    key={entry.work_unit_id}
                    entry={entry}
                    hasResult={availableResults.has(entry.work_unit_id)}
                    onViewResult={handleViewResult}
                  />
                ))}
              </div>
            </div>
          ))}

          {/* Loading / sentinel */}
          <div ref={scrollRef}>
            {isLoading && (
              <div className="space-y-1">
                {Array.from({ length: 5 }).map((_, i) => (
                  <SkeletonRow key={i} />
                ))}
              </div>
            )}
            {hasMore && <div data-sentinel className="h-4" />}
          </div>
        </div>

        {/* Credit breakdown sidebar */}
        {credit && (credit.by_head?.length || credit.by_leaf.length > 0) && (
          <div className="hidden lg:block">
            <CreditBreakdown credit={credit} />
          </div>
        )}
      </div>

      {/* Mobile credit breakdown */}
      {credit && (credit.by_head?.length || credit.by_leaf.length > 0) && (
        <div className="lg:hidden">
          <CreditBreakdown credit={credit} />
        </div>
      )}

      {/* Replay Modal */}
      {replayModal.open && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm"
          onClick={(e) => {
            if (e.target === e.currentTarget) closeModal();
          }}
        >
          <div className="relative w-[90vw] max-w-5xl bg-background border rounded-lg shadow-xl overflow-hidden"
               style={{ height: "80vh" }}>
            <div className="absolute top-3 right-3 z-10">
              <Button
                variant="ghost"
                size="sm"
                onClick={closeModal}
                className="h-8 w-8 p-0"
              >
                <X className="h-4 w-4" />
              </Button>
            </div>
            {replayModal.loading && (
              <div className="flex items-center justify-center h-full">
                <p className="text-sm text-muted-foreground">Loading visualization...</p>
              </div>
            )}
            {replayModal.error && (
              <div className="flex items-center justify-center h-full">
                <p className="text-sm text-destructive">{replayModal.error}</p>
              </div>
            )}
            {replayModal.replayData && (
              <VizFrame
                vizBundlePath={replayModal.vizBundlePath}
                leafSlug={replayModal.leafSlug}
                paused={false}
                mode="replay"
                replayData={replayModal.replayData}
              />
            )}
          </div>
        </div>
      )}
    </div>
  );
}
