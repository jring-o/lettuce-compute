package validation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// refResult builds a ref-only result (no inline output bytes, only an external output URL) that
// carries a volunteer-claimed checksum and — when verified != "" — the head-computed hash that
// promotion stamps into verified_output_checksum.
func refResult(claimed, verified string) *result.Result {
	r := &result.Result{ID: types.NewID(), OutputChecksum: claimed}
	if verified != "" {
		v := verified
		r.VerifiedOutputChecksum = &v
	}
	return r
}

// inlineResult builds an inline result with real output bytes and a checksum.
func inlineResult(checksum string, data json.RawMessage) *result.Result {
	return &result.Result{ID: types.NewID(), OutputChecksum: checksum, OutputData: data}
}

func mustComparisonKey(t *testing.T, r *result.Result, ignore []string) string {
	t.Helper()
	k, err := comparisonKey(r, ignore)
	if err != nil {
		t.Fatalf("comparisonKey: %v", err)
	}
	return k
}

// TestComparisonKey_VoteKeyRule pins the §10.8 vote-key rule (BG-02b): a volunteer-CLAIMED
// checksum can never be a comparison key. Verified refs vote on the head-computed hash; unverified
// refs get a per-result non-grouping key; inline results are unchanged.
func TestComparisonKey_VoteKeyRule(t *testing.T) {
	// (a) ref + verified -> the verified hash, exactly.
	t.Run("ref+verified keys on the verified hash exactly", func(t *testing.T) {
		verified := sha256hex([]byte("the real fetched bytes"))
		r := refResult("a-fabricated-claim", verified)
		if got := mustComparisonKey(t, r, nil); got != verified {
			t.Fatalf("key = %q, want the verified hash %q (the ONLY key a ref may vote on)", got, verified)
		}
	})

	// (b) THE BG-02b REGRESSION: two ref-only results sharing an identical fabricated claimed
	// checksum with nil VerifiedOutputChecksum must get DISTINCT keys, neither equal to the claimed
	// checksum. On PRE-FIX code both returned r.OutputChecksum and GROUPED — the money hole (a
	// fabricated-checksum quorum validating on bytes the head never saw). This subtest FAILS on
	// pre-fix code.
	t.Run("BG-02b: two refs sharing a fabricated claimed checksum get distinct non-grouping keys", func(t *testing.T) {
		claimed := strings.Repeat("f", 64) // a well-formed 64-hex checksum, never actually served
		r1 := refResult(claimed, "")
		r2 := refResult(claimed, "")
		k1 := mustComparisonKey(t, r1, nil)
		k2 := mustComparisonKey(t, r2, nil)
		if k1 == k2 {
			t.Fatalf("both refs keyed to %q and would GROUP — pre-fix behavior; the BG-02b hole", k1)
		}
		if k1 == claimed || k2 == claimed {
			t.Fatalf("a claimed checksum became a comparison key (k1=%q k2=%q) — a claim may never be a key", k1, k2)
		}
		if !strings.HasPrefix(k1, "unverified-ref:") || !strings.HasPrefix(k2, "unverified-ref:") {
			t.Fatalf("keys = %q / %q, want per-result unverified-ref: keys", k1, k2)
		}
	})

	// (c) inline results are unchanged from historical behavior.
	t.Run("inline without ignore_fields keys on the raw output_checksum", func(t *testing.T) {
		r := inlineResult("inline-ck", json.RawMessage(`{"x":1}`))
		if got := mustComparisonKey(t, r, nil); got != "inline-ck" {
			t.Fatalf("key = %q, want the raw checksum (head-verified at submit)", got)
		}
	})
	t.Run("inline with ignore_fields keys on a canon: hash", func(t *testing.T) {
		r := inlineResult("inline-ck", json.RawMessage(`{"x":1,"t":9}`))
		if got := mustComparisonKey(t, r, []string{"t"}); !strings.HasPrefix(got, "canon:") {
			t.Fatalf("key = %q, want a canon: key", got)
		}
	})

	// (d) ignore_fields set + a verified ref: still the verified hash. The ref branch precedes the
	// canon path — canonicalization cannot apply without inline bytes (the F-M1 raw-key lane, now
	// over a head-verified hash).
	t.Run("ignore_fields + ref+verified stays on the verified hash (no bytes to canonicalize)", func(t *testing.T) {
		verified := sha256hex([]byte("fetched bytes"))
		r := refResult("claimed", verified)
		if got := mustComparisonKey(t, r, []string{"t"}); got != verified {
			t.Fatalf("key = %q, want the verified hash %q (ref branch precedes canon)", got, verified)
		}
	})

	// (e) two PROMOTED refs of identical bytes group: promotion overwrites output_checksum with the
	// head hash, so claimed == verified, and both key to that same verified hash.
	t.Run("two promoted refs of identical bytes share the verified key", func(t *testing.T) {
		verified := sha256hex([]byte("identical fetched bytes"))
		r1 := refResult(verified, verified)
		r2 := refResult(verified, verified)
		k1 := mustComparisonKey(t, r1, nil)
		k2 := mustComparisonKey(t, r2, nil)
		if k1 != verified || k2 != verified {
			t.Fatalf("keys = %q / %q, want both = the verified hash %q", k1, k2, verified)
		}
		if k1 != k2 {
			t.Fatalf("promoted refs of identical bytes did not group: %q vs %q", k1, k2)
		}
	})
}

// TestSampleAudit_UnverifiedRefWinnerSkipped mirrors the canon-empty enqueue skip (F4, §10.8): a
// winner whose comparisonKey is an unverified-ref key is ineligible for sampling — never enqueued —
// because that key embeds the winner's UUID and is unadjudicable against runner bytes. Unreachable
// on production paths (a sampled ref winner is promoted-verified, so its key is 64-hex) but
// defense-in-depth exactly like the canon-empty lane.
func TestSampleAudit_UnverifiedRefWinnerSkipped(t *testing.T) {
	enq := &fakeEnqueuer{}
	e := auditEngine(enq, true, 1.0, nil)
	proj := auditLeaf(leaf.ComparisonExact)
	wu := auditUnit(proj.ID, "")
	// ref-only winner: no inline bytes and not head-verified -> comparisonKey is "unverified-ref:".
	r := auditResult(wu.ID, "a-claimed-checksum", nil)

	e.maybeSampleForAudit(context.Background(), wu, proj, []*result.Result{r})

	if len(enq.enqueued) != 0 {
		t.Fatalf("enqueued %d, want 0 (an unverified-ref winner is ineligible)", len(enq.enqueued))
	}
	if got := e.AuditIneligibleCounts()[wu.LeafID.String()]; got != 1 {
		t.Errorf("ineligible count = %d, want 1", got)
	}
}
