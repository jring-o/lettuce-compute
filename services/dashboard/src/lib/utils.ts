import { clsx, type ClassValue } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

export function formatNumber(n: number): string {
  return n.toLocaleString();
}

export function formatDate(date: Date): string {
  return date.toLocaleDateString("en-US", { month: "long", year: "numeric" });
}

export function formatShortDate(date: Date | string | null): string {
  if (!date) return "\u2014";
  return new Date(date).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

// formatMemory renders a megabyte count as GB (1 decimal) once it reaches 1 GB,
// otherwise as MB. Used for leaf memory requirements.
export function formatMemory(mb: number): string {
  if (mb >= 1024) return `${(mb / 1024).toFixed(1)} GB`;
  return `${mb} MB`;
}

/**
 * Badge variant mapping for leaf states.
 * Shared across leaf list page and leaf dashboard.
 */
export const LEAF_STATE_VARIANTS: Record<
  string,
  "default" | "secondary" | "outline" | "destructive"
> = {
  DRAFT: "secondary",
  CONFIGURING: "secondary",
  ACTIVE: "default",
  PAUSED: "outline",
  COMPLETED: "secondary",
  ARCHIVED: "outline",
};

/**
 * Human-readable labels for task patterns.
 * Shared across leaf card, detail, and dashboard components.
 */
export const TASK_PATTERN_LABELS: Record<string, string> = {
  PARAMETER_SWEEP: "Parameter Sweep",
  MAP_REDUCE: "Map-Reduce",
  MONTE_CARLO: "Monte Carlo",
  CUSTOM: "Custom",
};
