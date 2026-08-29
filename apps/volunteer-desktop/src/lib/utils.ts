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

/**
 * Credit for display. Heads report decimals, so whole numbers keep their
 * locale grouping and fractions are cut to two decimal places.
 */
export function formatCredit(n: number): string {
  if (!Number.isFinite(n)) return "0";
  if (Number.isInteger(n)) return n.toLocaleString();
  return n.toLocaleString(undefined, {
    minimumFractionDigits: 0,
    maximumFractionDigits: 2,
  });
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

/** Megabytes as a GB figure with one decimal, e.g. `formatGb(1536)` is "1.5 GB". */
export function formatGb(mb: number): string {
  return `${(mb / 1024).toFixed(1)} GB`;
}

/**
 * Human wording for the daemon's `paused_reason`. "scheduled" is the
 * configured computing hours; other reasons are shown as the daemon sent
 * them, so an unfamiliar value is still visible rather than hidden.
 */
export function pausedLabel(reason: string | null | undefined): string {
  if (!reason) return "Paused";
  if (reason === "scheduled") return "Paused — outside your schedule";
  return `Paused — ${reason}`;
}

/** An RFC 3339 timestamp as a short local date and time, e.g. "Mar 4, 14:05". */
export function formatDateTime(isoString: string): string {
  const d = new Date(isoString);
  if (Number.isNaN(d.getTime())) return isoString;
  return d.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

// ---------------------------------------------------------------------------
// Theme
// ---------------------------------------------------------------------------

export type Theme = "system" | "light" | "dark";

const THEME_STORAGE_KEY = "lettuce-theme";

/** The theme saved by the Settings page, or "system" when none is saved. */
export function readStoredTheme(): Theme {
  try {
    const stored = localStorage.getItem(THEME_STORAGE_KEY);
    if (stored === "light" || stored === "dark" || stored === "system") return stored;
  } catch {
    // localStorage unavailable: fall through to the default
  }
  return "system";
}

export function storeTheme(theme: Theme): void {
  try {
    localStorage.setItem(THEME_STORAGE_KEY, theme);
  } catch {
    // ignore: the theme still applies for this session
  }
}

/**
 * Apply `theme` to the document and keep following the OS preference while
 * the theme is "system". Returns a function that stops following it.
 */
export function applyTheme(theme: Theme): () => void {
  const root = document.documentElement;
  if (theme === "dark") {
    root.classList.add("dark");
    return () => {};
  }
  if (theme === "light") {
    root.classList.remove("dark");
    return () => {};
  }
  const mq = window.matchMedia("(prefers-color-scheme: dark)");
  const apply = () => {
    if (mq.matches) root.classList.add("dark");
    else root.classList.remove("dark");
  };
  apply();
  mq.addEventListener("change", apply);
  return () => mq.removeEventListener("change", apply);
}
