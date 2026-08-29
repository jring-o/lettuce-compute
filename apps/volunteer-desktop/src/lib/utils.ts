import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export function formatBytes(mb: number): string {
  if (mb >= 1024) {
    return `${(mb / 1024).toFixed(1)} GB`;
  }
  return `${mb} MB`;
}

export function formatDuration(seconds: number): string {
  if (seconds < 60) {
    return `${seconds}s`;
  }
  if (seconds < 3600) {
    return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
  }
  const hours = Math.floor(seconds / 3600);
  const mins = Math.floor((seconds % 3600) / 60);
  return `${hours}h ${mins}m`;
}

export function formatPercent(pct: number): string {
  return `${Math.round(pct)}%`;
}

export function formatCredit(n: number): string {
  return n.toLocaleString();
}

export function formatTimeAgo(isoString: string): string {
  const diff = Math.floor((Date.now() - new Date(isoString).getTime()) / 1000);
  if (diff < 60) return `${diff}s ago`;
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  return `${Math.floor(diff / 3600)}h ago`;
}

export function detectPlatform(): "windows" | "macos" | "linux" {
  if (navigator.userAgent.includes("Windows")) return "windows";
  if (navigator.userAgent.includes("Mac")) return "macos";
  return "linux";
}

/**
 * Age of an RFC 3339 timestamp in coarse units ("just now", "5m ago",
 * "3h ago", "2d ago"). Unlike `formatTimeAgo` it handles ages over a day and
 * takes the current time as a parameter so tests can pin it. An unparseable
 * timestamp renders as "unknown".
 */
export function formatAge(isoString: string, now: number = Date.now()): string {
  const then = new Date(isoString).getTime();
  if (Number.isNaN(then)) return "unknown";
  const diff = Math.max(0, Math.floor((now - then) / 1000));
  if (diff < 60) return "just now";
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}

/**
 * A size given in MB, rendered in the unit a person would use: whole
 * gigabytes without a decimal ("15 GB"), fractional ones with one ("1.5 GB"),
 * and anything under a gigabyte in MB ("512 MB").
 */
export function formatSizeMb(mb: number): string {
  if (mb < 1024) return `${mb} MB`;
  const gb = mb / 1024;
  return Number.isInteger(gb) ? `${gb} GB` : `${gb.toFixed(1)} GB`;
}
