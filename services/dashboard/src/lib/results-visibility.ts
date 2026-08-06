import type { Leaf } from "@/types/infrastructure";

/**
 * The per-leaf public-visualization opt-in (design §4.7), read from the leaf
 * itself: the head stores results_visibility on the leaf row, set through the
 * audited leaf-update API. This replaces the config-level form of the opt-in —
 * policy lives where the data lives, edits leave a TB-38 audit line, and a
 * future leaf reusing a freed slug can never inherit it.
 *
 * True iff the leaf opted its results PUBLIC and the leaf is not PRIVATE — the
 * opt-in never overrides catalog visibility; a PRIVATE leaf stays hidden even
 * with results_visibility = PUBLIC.
 */
export function isPublicResultsLeaf(
  leaf: Pick<Leaf, "results_visibility" | "visibility">,
): boolean {
  return leaf.visibility !== "PRIVATE" && leaf.results_visibility === "PUBLIC";
}
