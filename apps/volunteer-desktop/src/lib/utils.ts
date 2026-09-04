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

/**
 * Format a credit amount. Credit is a decimal (a head credits a unit by its
 * leaf's per-unit figure, which need not be a whole number), so keep up to two
 * decimals and drop trailing zeros: 1250 -> "1,250", 0.5 -> "0.5",
 * 12.3456 -> "12.35". Non-finite input renders as "0".
 */
export function formatCreditAmount(n: number): string {
  if (!Number.isFinite(n)) return "0";
  return n.toLocaleString(undefined, { maximumFractionDigits: 2 });
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

/**
 * A requirement beside an allowance, as two labels a reader can act on.
 * `formatSizeMb` rounds to one decimal of a gigabyte, so a leaf declaring
 * 7000 MB and a machine allowing 6912 MB both printed "6.8 GB": the card
 * contradicted itself ("6.8 GB RAM (you allow 6.8 GB)") while the head
 * refused the machine by 88 MB (TB-66). A shortfall is the one place the
 * numbers are acted on, so whenever either figure would be rounded both are
 * printed in MB — the unit the daemon and the head compare in; whole
 * gigabytes keep their short form ("16 GB", "8 GB").
 */
export function formatSizePairMb(need: number, have: number): [string, string] {
  const exact = (mb: number) => mb < 1024 || Number.isInteger(mb / 1024);
  if (exact(need) && exact(have)) return [formatSizeMb(need), formatSizeMb(have)];
  return [`${need} MB`, `${have} MB`];
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
