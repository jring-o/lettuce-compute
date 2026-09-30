import type { HeadInfo } from "@/api/client";

/**
 * The reasons a head gives for sending no work that ask nothing of the
 * volunteer: this account already has a result on, or a copy of, every task
 * the head has ready for a leaf, or a recent failed copy of its own keeps it
 * off a task for a while. The daemon raises them as information, never under
 * "Needs attention"; the app says them where the machine's idleness is shown.
 */
const INFORMATIONAL = new Set(["already_contributed", "bench_cooldown"]);

/** One head's line on why it is sending no work, for a volunteer to read. */
export interface NoWorkLine {
  /** The head's gRPC address, which identifies it. */
  head: string;
  reason: string;
  message: string;
  /** When the head last said it (RFC 3339). */
  at: string;
}

/** The heads' informational lines, one per head that has one. */
export function informationalNoWorkLines(heads: HeadInfo[]): NoWorkLine[] {
  const lines: NoWorkLine[] = [];
  for (const h of heads) {
    const nw = h.no_work;
    if (nw && INFORMATIONAL.has(nw.reason) && nw.message) {
      lines.push({ head: h.grpc_address || h.name, reason: nw.reason, message: nw.message, at: nw.at });
    }
  }
  return lines;
}

/**
 * True when every head says this account has done every task it has ready:
 * nothing is running because there is nothing new, not because anything is
 * wrong. False with no heads, or while any head says anything else or nothing.
 */
export function doneEverythingAvailable(heads: HeadInfo[]): boolean {
  return heads.length > 0 && heads.every((h) => h.no_work?.reason === "already_contributed");
}

/** Where the app lists the other leafs a volunteer can run. */
export const OTHER_LEAFS_HINT = "The Projects page lists the other leafs you can run.";
