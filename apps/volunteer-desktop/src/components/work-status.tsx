import type { HeadCredit, LeafWorkStatus } from "@/api/client";

/** Plain-words labels for the per-leaf counts, in display order. */
const RESULT_ROWS: { key: keyof LeafWorkStatus; label: string }[] = [
  { key: "results_pending", label: "Waiting for validation" },
  { key: "results_agreed", label: "Agreed (credited)" },
  { key: "results_disagreed", label: "Did not agree" },
  { key: "results_awaiting_content_verification", label: "Checking the uploaded output" },
  { key: "results_content_verification_failed", label: "Uploaded output could not be checked" },
  { key: "results_superseded", label: "Not compared (work unit retired)" },
  { key: "runs_stopped", label: "Stopped: enough results arrived while it ran" },
];

export const PENDING_NOTE =
  "A result earns credit only once it is validated. On a leaf that needs agreeing results, " +
  "that takes a matching result from a different account; your own other machines are never " +
  "sent the same work unit.";

function LeafRows({ leaf }: { leaf: LeafWorkStatus }) {
  const rows = RESULT_ROWS.filter((r) => (leaf[r.key] as number) > 0);
  const inProgress = leaf.copies_running > 0 || leaf.copies_waiting_to_start > 0;
  return (
    <div className="space-y-0.5">
      <p className="text-xs font-medium truncate">{leaf.leaf_name || leaf.leaf_id}</p>
      {rows.map((r) => (
        <div key={r.key} className="flex justify-between gap-2 pl-2 text-xs text-muted-foreground">
          <span>{r.label}</span>
          <span className="tabular-nums">{leaf[r.key] as number}</span>
        </div>
      ))}
      {inProgress && (
        <div className="flex justify-between gap-2 pl-2 text-xs text-muted-foreground">
          <span>In progress</span>
          <span className="tabular-nums">
            {leaf.copies_running} running, {leaf.copies_waiting_to_start} waiting to start
          </span>
        </div>
      )}
    </div>
  );
}

/**
 * The account's results by validation state and copies in progress, per head and
 * leaf, from each head's `work_status`. A head that answered without the figures
 * (an older head) says so rather than showing zeros; an unreachable head is left
 * out, since the credit breakdown already marks it. Renders nothing when no head
 * answered.
 */
export function WorkStatusByHead({ heads }: { heads: HeadCredit[] }) {
  const answered = heads.filter((h) => h.available);
  if (answered.length === 0) return null;
  const anyPending = answered.some((h) =>
    (h.work_status?.by_leaf ?? []).some((l) => l.results_pending > 0)
  );

  return (
    <div className="space-y-3" data-testid="work-status">
      {answered.map((head) => (
        <div key={head.head_name} className="space-y-2">
          <p className="text-xs font-medium">{head.head_name}</p>
          {!head.work_status ? (
            <p className="text-xs italic text-muted-foreground">
              Not reported by this head (it runs an older version)
            </p>
          ) : head.work_status.by_leaf.length === 0 ? (
            <p className="text-xs text-muted-foreground">No results or copies on this head yet</p>
          ) : (
            head.work_status.by_leaf.map((leaf) => <LeafRows key={leaf.leaf_id} leaf={leaf} />)
          )}
        </div>
      ))}
      {anyPending && <p className="text-[11px] text-muted-foreground">{PENDING_NOTE}</p>}
    </div>
  );
}
