package validation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/lettuce-compute/infrastructure/internal/audit"
)

// AdjudicateAudit computes the head-side verdict for a re-executed audit output (spec §7.4).
// Every comparison is server-side under the snapshot semantics pinned at sampling time — a
// runner returns only output bytes and never self-adjudicates (D6). It is assignable to
// audit.Adjudicator and wired into the AuditService submit handler in main.go, so neither the
// audit package nor the validation package imports the other's handler code.
//
// Case selection dispatches on the ACCEPTED KEY'S SHAPE, never on snapshot fields alone
// (audit F-M1): a ref-only winner on an ignore_fields leaf carries a RAW claimed-checksum key
// because comparisonKey falls back to the raw checksum whenever inline bytes are absent, so
// snapshot-keyed dispatch would canon-key the runner side against a raw key and fabricate a
// MISMATCH. The shapes:
//
//   - "canon-empty:<uuid>" — the winner's ignore_fields stripped every leaf, so its key
//     embeds its own result UUID and is unadjudicable against runner bytes by construction.
//     Sampling already excludes these (F-M2); this branch is defense-in-depth and returns
//     INCONCLUSIVE rather than a meaningless MATCH/MISMATCH.
//   - "canon:<hex>" — EXACT with effective ignore_fields. Adjudicated by VALUE, never by
//     key-string across the raw/stored boundary (audit F-H3): the winner's canon key was
//     computed from jsonb-NORMALIZED stored bytes (Postgres re-renders numeric tokens, e.g.
//     1e-07 -> 0.0000001) while the runner returns raw bytes whose tokens canon-keying
//     preserves verbatim, so a key-vs-key compare would false-MISMATCH byte-honest
//     re-executions of any exponent-form emitter. Both sides are flattened under the snapshot
//     ignore_fields and compared with numericMatch(epsilon = 0).
//   - empty key — NUMERIC_TOLERANCE (its accepted key is NULL). Value-level within the
//     snapshot tolerance, MATCH iff within epsilon of ANY AGREED member (audit F-M3).
//   - otherwise a raw 64-hex checksum — EXACT without effective canon, INCLUDING ref-only
//     winners on any EXACT leaf. sha256 of the runner bytes vs the accepted key. Symmetric
//     (both hashes are over RAW bytes), works for non-JSON runner bytes (F-M8), and catches
//     the BG-02b fabricated-checksum shape: a ref-only quorum's accepted key is the
//     volunteer-CLAIMED checksum, which MISMATCHes real re-executed bytes.
//
// Unadjudicable inputs (accepted output missing, runner bytes unparseable where a value
// compare is required, no comparable NUMERIC member) yield VerdictInconclusive with a
// ReasonCompareError detail — NEVER a fabricated MISMATCH, because slice-3 enforcement acts on
// MISMATCH. The returned error is always nil: every failure mode is expressed as a verdict +
// detail; the error is retained only to satisfy the audit.Adjudicator contract.
func AdjudicateAudit(snap audit.ComparisonSnapshot, acceptedKey string, acceptedOutputs []json.RawMessage, runnerOutput []byte) (audit.Verdict, string, error) {
	switch {
	case strings.HasPrefix(acceptedKey, "canon-empty:"):
		return audit.VerdictInconclusive,
			audit.ReasonCompareError + ": accepted key is canon-empty (winner stripped every comparable leaf)", nil
	case strings.HasPrefix(acceptedKey, "canon:"):
		return adjudicateCanonValue(snap, acceptedOutputs, runnerOutput)
	case acceptedKey == "":
		return adjudicateNumeric(snap, acceptedOutputs, runnerOutput)
	default:
		return adjudicateRawChecksum(acceptedKey, runnerOutput)
	}
}

// adjudicateRawChecksum is the raw-64-hex-key case: hex(sha256(runnerOutput)) vs the accepted
// key. The accepted key is the submit-time HEAD-computed checksum (or, for a ref-only winner,
// the volunteer-claimed checksum), so this compares two raw-byte hashes on a symmetric channel
// and is adjudicable even for non-JSON runner bytes.
func adjudicateRawChecksum(acceptedKey string, runnerOutput []byte) (audit.Verdict, string, error) {
	sum := sha256.Sum256(runnerOutput)
	if hex.EncodeToString(sum[:]) == acceptedKey {
		return audit.VerdictMatch, "raw output checksum matches the accepted key", nil
	}
	return audit.VerdictMismatch, "raw output checksum differs from the accepted key", nil
}

// adjudicateCanonValue is the canon-key (EXACT + effective ignore_fields) case: flatten the
// winner's stored output and the runner bytes under the snapshot ignore_fields, then compare
// value-for-value with numericMatch(epsilon = 0) — never key-string across the jsonb-normalized
// boundary (F-H3). acceptedOutputs[0] is the representative winner's stored output_data.
func adjudicateCanonValue(snap audit.ComparisonSnapshot, acceptedOutputs []json.RawMessage, runnerOutput []byte) (audit.Verdict, string, error) {
	if len(acceptedOutputs) == 0 || len(acceptedOutputs[0]) == 0 {
		return audit.VerdictInconclusive,
			audit.ReasonCompareError + ": accepted output unavailable for value comparison", nil
	}
	// EXACT never uses compare_fields, so the value compare passes nil (ignore_fields only).
	acc, err := flattenOutput(acceptedOutputs[0], snap.IgnoreFields, nil)
	if err != nil {
		return audit.VerdictInconclusive,
			audit.ReasonCompareError + ": accepted output failed to flatten: " + err.Error(), nil
	}
	run, err := flattenOutput(json.RawMessage(runnerOutput), snap.IgnoreFields, nil)
	if err != nil {
		return audit.VerdictInconclusive,
			audit.ReasonCompareError + ": runner output failed to flatten: " + err.Error(), nil
	}
	if numericMatch(acc, run, 0) {
		return audit.VerdictMatch, "canonical output value matches the accepted winner", nil
	}
	return audit.VerdictMismatch, "canonical output value differs from the accepted winner", nil
}

// adjudicateNumeric is the NUMERIC_TOLERANCE (empty accepted key) case: MATCH iff the runner
// output is within the snapshot tolerance of ANY AGREED member (F-M3 — validation accepted a
// pairwise-epsilon clique, so an honest runner can sit up to ~2*epsilon from the representative
// while within epsilon of a different member). A member whose stored output fails to flatten is
// skipped; if the RUNNER output fails to flatten, or NO member was comparable, the result is
// INCONCLUSIVE — never a fabricated MISMATCH.
func adjudicateNumeric(snap audit.ComparisonSnapshot, acceptedOutputs []json.RawMessage, runnerOutput []byte) (audit.Verdict, string, error) {
	run, err := flattenOutput(json.RawMessage(runnerOutput), snap.IgnoreFields, snap.CompareFields)
	if err != nil {
		return audit.VerdictInconclusive,
			audit.ReasonCompareError + ": runner output failed to flatten: " + err.Error(), nil
	}
	comparable := 0
	for _, out := range acceptedOutputs {
		if len(out) == 0 {
			continue // a ref-only / nil member has no stored bytes to compare against
		}
		acc, err := flattenOutput(out, snap.IgnoreFields, snap.CompareFields)
		if err != nil {
			continue // a member whose stored output fails to flatten is skipped
		}
		comparable++
		if numericMatch(acc, run, snap.NumericTolerance) {
			return audit.VerdictMatch, "runner output within tolerance of an accepted member", nil
		}
	}
	if comparable == 0 {
		return audit.VerdictInconclusive,
			audit.ReasonCompareError + ": no accepted member had comparable stored output", nil
	}
	return audit.VerdictMismatch, "runner output outside tolerance of every accepted member", nil
}
