"use client";

import { useState, useCallback, useEffect } from "react";
import { VizIframe } from "./VizIframe";
import { WorkUnitSelector } from "./WorkUnitSelector";
import type { WorkUnitSummary, Result } from "@/types/infrastructure";

interface VisualizationPageProps {
  vizBundleUrl: string;
  vizOrigin: string;
  platformUrl: string;
  leafSlug: string;
  leafId: string;
  workUnits: WorkUnitSummary[];
  initialResult: Result | null;
  volunteerFilter?: string;
}

export function VisualizationPage({
  vizBundleUrl,
  vizOrigin,
  platformUrl,
  leafSlug,
  leafId,
  workUnits,
  initialResult,
  volunteerFilter,
}: VisualizationPageProps) {
  const [selectedWuId, setSelectedWuId] = useState<string | null>(
    workUnits[0]?.id ?? null,
  );
  const [currentResult, setCurrentResult] = useState<Result | null>(initialResult);
  const [loading, setLoading] = useState(false);
  const [fetchError, setFetchError] = useState(false);

  // Fetch result when WU selection changes
  useEffect(() => {
    if (!selectedWuId) return;
    if (currentResult && currentResult.work_unit_id === selectedWuId) return;

    let cancelled = false;
    setLoading(true);
    setFetchError(false);

    const vizUrl = `/api/viz/results?leafId=${leafId}&workUnitId=${selectedWuId}${volunteerFilter ? `&volunteerId=${volunteerFilter}` : ""}`;
    fetch(vizUrl)
      .then((res) => res.json())
      .then((data) => {
        if (!cancelled && data.result) {
          setCurrentResult(data.result);
        }
      })
      .catch(() => {
        if (!cancelled) setFetchError(true);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });

    return () => {
      cancelled = true;
    };
    // currentResult is intentionally excluded — we only want to fetch when
    // selectedWuId changes, not when the fetch itself updates currentResult.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedWuId, leafId, volunteerFilter]);

  const handleWuSelect = useCallback((id: string) => {
    setSelectedWuId(id);
    setCurrentResult(null);
    setFetchError(false);
  }, []);

  if (workUnits.length === 0) {
    return (
      <div className="flex items-center justify-center h-64 text-muted-foreground">
        No completed work units with visualization data available.
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <WorkUnitSelector
        workUnits={workUnits}
        selectedId={selectedWuId}
        onSelect={handleWuSelect}
        loading={loading}
        volunteerFilter={volunteerFilter}
      />

      <div
        className="rounded-lg overflow-hidden border border-border"
        style={{ height: 600 }}
      >
        {loading ? (
          <div className="flex items-center justify-center h-full bg-[#0a0a0f] text-muted-foreground">
            <span className="animate-pulse">Loading visualization data...</span>
          </div>
        ) : fetchError ? (
          <div className="flex items-center justify-center h-full bg-[#0a0a0f] text-muted-foreground">
            Failed to load visualization data. Try selecting a different work unit.
          </div>
        ) : (
          <VizIframe
            vizBundleUrl={vizBundleUrl}
            vizOrigin={vizOrigin}
            platformUrl={platformUrl}
            leafSlug={leafSlug}
            resultOutputData={currentResult?.output_data ?? null}
          />
        )}
      </div>
    </div>
  );
}
