"use client";

import { useRouter, usePathname } from "next/navigation";
import type { WorkUnitSummary } from "@/types/infrastructure";

interface WorkUnitSelectorProps {
  workUnits: WorkUnitSummary[];
  selectedId: string | null;
  onSelect: (id: string) => void;
  loading?: boolean;
  volunteerFilter?: string;
}

function formatDate(dateStr: string): string {
  const d = new Date(dateStr);
  return d.toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function WorkUnitSelector({
  workUnits,
  selectedId,
  onSelect,
  loading,
  volunteerFilter,
}: WorkUnitSelectorProps) {
  const router = useRouter();
  const pathname = usePathname();
  if (workUnits.length === 0) {
    return (
      <div className="text-sm text-muted-foreground">
        No completed work units available.
      </div>
    );
  }

  return (
    <div className="space-y-2">
      {volunteerFilter && (
        <div className="inline-flex items-center gap-1.5 rounded-full bg-blue-100 px-3 py-1 text-xs font-medium text-blue-800 dark:bg-blue-900/30 dark:text-blue-300">
          Filtered to volunteer {volunteerFilter.slice(0, 8)}...
          <button
            onClick={() => router.push(pathname)}
            className="ml-1 rounded-full p-0.5 hover:bg-blue-200 dark:hover:bg-blue-800 transition-colors"
            aria-label="Clear volunteer filter"
          >
            <svg className="size-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
              <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
            </svg>
          </button>
        </div>
      )}
      <div className="flex items-center gap-3">
      <label htmlFor="wu-select" className="text-sm font-medium whitespace-nowrap">
        Work Unit
      </label>
      <select
        id="wu-select"
        value={selectedId ?? ""}
        onChange={(e) => onSelect(e.target.value)}
        disabled={loading}
        className="flex h-8 w-full max-w-xs items-center rounded-lg border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50 dark:bg-input/30"
      >
        {workUnits.map((wu) => (
          <option key={wu.id} value={wu.id}>
            {wu.id.slice(0, 8)} — {formatDate(wu.updated_at)}
            {wu.state === "VALIDATED" ? " · validated" : " · unvalidated"}
          </option>
        ))}
      </select>
      {loading && (
        <span className="text-xs text-muted-foreground animate-pulse">
          Loading...
        </span>
      )}
      </div>
    </div>
  );
}
