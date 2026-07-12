/**
 * @jest-environment node
 */

import { readFileSync } from "fs";
import path from "path";
import { globSync } from "glob";

/**
 * R1.3 coverage meta-test — the "third data surface" the route- and action-
 * level checks miss: server components (page.tsx / layout.tsx) that call the
 * shared-service-key `infrastructureClient` DIRECTLY. Such a call bypasses the
 * middleware (a server render is not an /api/* request), the action gate, and
 * the /api route handlers, so a contents read here (aggregate, results, work
 * units, versions) would disclose owner-only data (design §1.3) with no
 * per-user check.
 *
 * This test statically scans every app-tree page/layout for
 * `infrastructureClient.<method>` call sites and asserts each is on the explicit
 * allowlist below. A NEW, unlisted call site FAILS CI — turning an invisible
 * omission into an explicit, reviewable allowlist entry (design §3, §4.6). API
 * route handlers (route.ts) are excluded: they are gated by the middleware
 * default-deny plus their own requireLeafAccess/requireFileAccess adapters and
 * covered by the route tests + middleware test.
 *
 * Two categories of allowed call site:
 *   - "public": anonymous-safe reads — leaf METADATA (getLeaf, with the page's
 *     own PRIVATE-visibility check) and PUBLIC STATS (getLeafStats*).
 *   - "owner-gated": leaf CONTENTS reads that are permitted ONLY because the
 *     page enforces ownership before rendering (the note names the gate).
 */

// method -> the only category it may EVER be used under in a page.
const METHOD_CATEGORY: Record<string, "public" | "owner-gated"> = {
  // Metadata / public stats — anonymous-safe.
  getLeaf: "public",
  getLeafStats: "public",
  getLeafStatsBatch: "public",
  getLeafStatsHistory: "public",
  // Contents — owner-only; only allowed on a page that gates ownership.
  getAggregation: "owner-gated",
  listWorkUnits: "owner-gated",
  listResults: "owner-gated",
  listVersions: "owner-gated",
  getWorkUnit: "owner-gated",
};

// The reviewed allowlist: file (POSIX-relative to src/app) -> permitted methods.
// Every infrastructureClient call site in an app page/layout must appear here.
const ALLOWLIST: Record<string, string[]> = {
  // Public leaf detail — metadata + public stats only (R1.2 stripped the
  // aggregate/work-unit contents reads that used to live here).
  "leafs/[slug]/page.tsx": ["getLeaf", "getLeafStats"],
  // Public visualize page — gated to owner/admin (notFound() otherwise) before
  // it replays results, because output_data is owner-only contents (§1.3).
  "leafs/[slug]/visualize/page.tsx": ["getLeaf", "listWorkUnits", "listResults"],
  // Owner dashboard page — ownership-checked at the top (creator_id ===
  // session.user.id else redirect); keeps the contents reads (R1.2).
  "dashboard/leafs/[slug]/page.tsx": [
    "getLeaf",
    "getLeafStats",
    "getAggregation",
    "listVersions",
  ],
};

const APP_DIR = path.join(__dirname, "..", "app");

// Matches `infrastructureClient.method` and `infrastructureClient\n  .method`.
const CALL_RE = /infrastructureClient\s*\.\s*(\w+)/g;

function relFromApp(absPath: string): string {
  return path.relative(APP_DIR, absPath).split(path.sep).join("/");
}

describe("infrastructureClient usage in app pages (R1.3 coverage)", () => {
  const pageFiles = globSync("**/{page,layout}.{ts,tsx}", { cwd: APP_DIR }).map(
    (p) => path.join(APP_DIR, p),
  );

  it("finds page/layout files to scan", () => {
    expect(pageFiles.length).toBeGreaterThan(0);
  });

  it("every infrastructureClient call site in a page is on the reviewed allowlist", () => {
    const violations: string[] = [];

    for (const file of pageFiles) {
      const rel = relFromApp(file);
      const src = readFileSync(file, "utf8");
      const methods = new Set<string>();
      for (const m of src.matchAll(CALL_RE)) methods.add(m[1]);
      if (methods.size === 0) continue;

      const allowed = ALLOWLIST[rel];
      if (!allowed) {
        violations.push(
          `${rel}: uses infrastructureClient (${[...methods].join(", ")}) but the file is not on the R1.3 allowlist. ` +
            `Server components must not read owner-only contents without a per-user gate — add an ownership check and an explicit allowlist entry.`,
        );
        continue;
      }
      for (const method of methods) {
        if (!allowed.includes(method)) {
          violations.push(
            `${rel}: calls infrastructureClient.${method} (${METHOD_CATEGORY[method] ?? "unknown"}) which is not in this file's allowlist [${allowed.join(", ")}].`,
          );
        }
      }
    }

    expect(violations).toEqual([]);
  });

  it("no owner-gated method is allowlisted on an anonymous-reachable page without an ownership gate", () => {
    // Guard the guard: any file allowlisting an owner-gated method must contain
    // an ownership signal (redirect on non-owner, notFound gate, or the shared
    // leafOwnershipVerdict). This keeps a future edit from allowlisting a
    // contents read on a page that forgot to gate.
    const OWNERSHIP_SIGNALS = [
      "leafOwnershipVerdict",
      "creator_id !== session.user.id",
      "verdict.allowed",
    ];
    const failures: string[] = [];

    for (const [rel, methods] of Object.entries(ALLOWLIST)) {
      const gatedMethods = methods.filter(
        (m) => METHOD_CATEGORY[m] === "owner-gated",
      );
      if (gatedMethods.length === 0) continue;

      const src = readFileSync(path.join(APP_DIR, rel), "utf8");
      const hasGate = OWNERSHIP_SIGNALS.some((sig) => src.includes(sig));
      if (!hasGate) {
        failures.push(
          `${rel}: allowlists owner-gated methods [${gatedMethods.join(", ")}] but contains no recognizable ownership gate.`,
        );
      }
    }

    expect(failures).toEqual([]);
  });
});
