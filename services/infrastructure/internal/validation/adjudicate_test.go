package validation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/audit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// AdjudicateAudit must satisfy the pinned audit.Adjudicator contract (wired in main.go).
var _ audit.Adjudicator = AdjudicateAudit

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// mustCanonKey computes the "canon:" grouping key a winner with these ignore_fields would carry,
// via the very comparisonKey the sampling hook uses. Used to prove the F-H3 regression: a naive
// key-STRING adjudicator would compare these keys (and false-MISMATCH across the jsonb/raw token
// boundary) where AdjudicateAudit compares VALUES.
func mustCanonKey(t *testing.T, data json.RawMessage, ignore []string) string {
	t.Helper()
	r := &result.Result{ID: types.NewID(), OutputData: data, OutputChecksum: "unused"}
	k, err := comparisonKey(r, ignore)
	if err != nil {
		t.Fatalf("comparisonKey: %v", err)
	}
	if !strings.HasPrefix(k, "canon:") {
		t.Fatalf("expected a canon: key, got %q", k)
	}
	return k
}

// TestAdjudicateAudit_Table exercises the §7.4 adjudication table, dispatching on the ACCEPTED
// KEY'S SHAPE (F-M1). raw entries: []byte(...) runner bytes; canon/NUMERIC entries reuse the
// same-package flattenOutput/numericMatch.
func TestAdjudicateAudit_Table(t *testing.T) {
	rawBytes := []byte(`{"result":42}`)
	rawBinary := []byte{0x00, 0x01, 0xff, 0xfe, 0x7f} // non-JSON

	tests := []struct {
		name    string
		snap    audit.ComparisonSnapshot
		key     string
		accepted []json.RawMessage
		runner  []byte
		want    audit.Verdict
	}{
		// --- raw 64-hex key (EXACT without effective canon; ref-only winners) ---
		{
			name:    "raw key: exact bytes reproduce -> MATCH",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact},
			key:     sha256hex(rawBytes),
			runner:  rawBytes,
			want:    audit.VerdictMatch,
		},
		{
			name:    "raw key: different bytes -> MISMATCH",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact},
			key:     sha256hex(rawBytes),
			runner:  []byte(`{"result":43}`),
			want:    audit.VerdictMismatch,
		},
		{
			name:    "raw key: non-JSON runner bytes still adjudicable (F-M8) -> MATCH",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact},
			key:     sha256hex(rawBinary),
			runner:  rawBinary,
			want:    audit.VerdictMatch,
		},
		{
			// F-M1 + BG-02b catcher: a ref-only winner on an IGNORE_FIELDS leaf still carries a RAW
			// claimed-checksum key. Dispatch is on the key SHAPE (raw), never the snapshot — so the
			// runner's real re-executed bytes are hashed and MISMATCH the fabricated claimed key.
			name:    "raw key on ignore_fields snapshot: fabricated ref-only checksum -> MISMATCH",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact, IgnoreFields: []string{"t"}},
			key:     strings.Repeat("f", 64), // volunteer-claimed checksum, not the hash of real bytes
			runner:  []byte(`{"y":1,"t":9}`),
			want:    audit.VerdictMismatch,
		},

		// --- canon-empty key (defense; sampling excludes these) ---
		{
			name:    "canon-empty key -> INCONCLUSIVE",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact},
			key:     "canon-empty:" + types.NewID().String(),
			accepted: []json.RawMessage{json.RawMessage(`{"x":1}`)},
			runner:  []byte(`{"x":1}`),
			want:    audit.VerdictInconclusive,
		},

		// --- unverified-ref key (defense; sampling excludes these — F4/§10.8) ---
		{
			// A ref not yet head-verified is unadjudicable exactly like canon-empty. Without an
			// explicit case the raw-hex default would sha256 the runner bytes against the literal
			// "unverified-ref:<uuid>" string and fabricate a MISMATCH — slashing an honest submitter
			// under slice-3 enforcement. It must be INCONCLUSIVE even though the runner bytes hash to
			// something entirely different from the key string.
			name:    "unverified-ref key -> INCONCLUSIVE (never a fabricated MISMATCH)",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact},
			key:     "unverified-ref:" + types.NewID().String(),
			runner:  []byte(`{"anything":1}`),
			want:    audit.VerdictInconclusive,
		},

		// --- canon key (EXACT + effective ignore_fields): VALUE-level ---
		{
			name:    "canon key: values equal -> MATCH",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact},
			key:     "canon:deadbeef",
			accepted: []json.RawMessage{json.RawMessage(`{"x":1.0}`)},
			runner:  []byte(`{"x":1.0}`),
			want:    audit.VerdictMatch,
		},
		{
			name:    "canon key: values differ -> MISMATCH",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact},
			key:     "canon:deadbeef",
			accepted: []json.RawMessage{json.RawMessage(`{"x":1.0}`)},
			runner:  []byte(`{"x":2.0}`),
			want:    audit.VerdictMismatch,
		},
		{
			// Snapshot semantics: the snapshot's ignore_fields make a match that would NOT hold if
			// the volatile "t" were compared (proves adjudication reads the SNAPSHOT, not live cfg).
			name:    "canon key: snapshot ignore_fields drops volatile field -> MATCH",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact, IgnoreFields: []string{"t"}},
			key:     "canon:deadbeef",
			accepted: []json.RawMessage{json.RawMessage(`{"x":1.0,"t":5}`)},
			runner:  []byte(`{"x":1.0,"t":9}`),
			want:    audit.VerdictMatch,
		},
		{
			// Same values but NO ignore_fields in the snapshot: the differing "t" now blocks the
			// match (the contrast that makes the row above load-bearing).
			name:    "canon key: without ignore_fields the volatile field blocks -> MISMATCH",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact},
			key:     "canon:deadbeef",
			accepted: []json.RawMessage{json.RawMessage(`{"x":1.0,"t":5}`)},
			runner:  []byte(`{"x":1.0,"t":9}`),
			want:    audit.VerdictMismatch,
		},
		{
			name:    "canon key: accepted output missing -> INCONCLUSIVE",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact},
			key:     "canon:deadbeef",
			accepted: []json.RawMessage{nil},
			runner:  []byte(`{"x":1.0}`),
			want:    audit.VerdictInconclusive,
		},
		{
			name:    "canon key: unparseable runner bytes -> INCONCLUSIVE (never fabricated MISMATCH)",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact},
			key:     "canon:deadbeef",
			accepted: []json.RawMessage{json.RawMessage(`{"x":1.0}`)},
			runner:  []byte(`not json at all`),
			want:    audit.VerdictInconclusive,
		},

		// --- NUMERIC_TOLERANCE (empty key): value-level, MATCH iff within eps of ANY member ---
		{
			// F-M3: within eps of a NON-representative clique member (member[1]), outside eps of the
			// representative (member[0]) -> MATCH (a representative-only compare would false-MISMATCH).
			name:    "numeric: within eps of non-representative member -> MATCH",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonNumericTolerance, NumericTolerance: 0.1},
			key:     "",
			accepted: []json.RawMessage{json.RawMessage(`{"v":1.0}`), json.RawMessage(`{"v":1.15}`)},
			runner:  []byte(`{"v":1.2}`),
			want:    audit.VerdictMatch,
		},
		{
			name:    "numeric: outside eps of every member -> MISMATCH",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonNumericTolerance, NumericTolerance: 0.1},
			key:     "",
			accepted: []json.RawMessage{json.RawMessage(`{"v":1.0}`), json.RawMessage(`{"v":1.15}`)},
			runner:  []byte(`{"v":5.0}`),
			want:    audit.VerdictMismatch,
		},
		{
			// Snapshot tolerance is load-bearing: a wider eps flips the same inputs to MATCH.
			name:    "numeric: snapshot tolerance admits within-eps -> MATCH",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonNumericTolerance, NumericTolerance: 0.5},
			key:     "",
			accepted: []json.RawMessage{json.RawMessage(`{"v":1.0}`)},
			runner:  []byte(`{"v":1.4}`),
			want:    audit.VerdictMatch,
		},
		{
			name:    "numeric: a member that fails to flatten is skipped, another matches -> MATCH",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonNumericTolerance, NumericTolerance: 0},
			key:     "",
			accepted: []json.RawMessage{json.RawMessage(`not json`), json.RawMessage(`{"v":1.0}`)},
			runner:  []byte(`{"v":1.0}`),
			want:    audit.VerdictMatch,
		},
		{
			name:    "numeric: unparseable runner bytes -> INCONCLUSIVE",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonNumericTolerance, NumericTolerance: 0.1},
			key:     "",
			accepted: []json.RawMessage{json.RawMessage(`{"v":1.0}`)},
			runner:  []byte(`garbage`),
			want:    audit.VerdictInconclusive,
		},
		{
			name:    "numeric: no comparable member -> INCONCLUSIVE",
			snap:    audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonNumericTolerance, NumericTolerance: 0.1},
			key:     "",
			accepted: []json.RawMessage{nil, nil},
			runner:  []byte(`{"v":1.0}`),
			want:    audit.VerdictInconclusive,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, detail, err := AdjudicateAudit(tt.snap, tt.key, tt.accepted, tt.runner)
			if err != nil {
				t.Fatalf("AdjudicateAudit returned error: %v", err)
			}
			if v != tt.want {
				t.Fatalf("verdict = %q (detail %q), want %q", v, detail, tt.want)
			}
			if v == audit.VerdictInconclusive && !strings.HasPrefix(detail, audit.ReasonCompareError) {
				t.Errorf("INCONCLUSIVE detail = %q, want a %s prefix", detail, audit.ReasonCompareError)
			}
		})
	}
}

// TestAdjudicateAudit_CanonExponentFormRegression is the F-H3 regression: the winner's stored
// output is jsonb-normalized (Postgres re-rendered 1e-07 -> 0.0000001) while the runner returns
// raw bytes preserving 1e-07. A key-STRING adjudicator would compare two DIFFERENT canon keys and
// false-MISMATCH a byte-honest re-execution; value-level comparison collapses both tokens to the
// same float64 and MATCHES.
func TestAdjudicateAudit_CanonExponentFormRegression(t *testing.T) {
	ignore := []string{"t"}
	stored := json.RawMessage(`{"x":0.0000001,"t":123}`) // jsonb-normalized winner output
	runner := []byte(`{"x":1e-07,"t":999}`)              // raw runner bytes, exponent token preserved

	// The premise: the two canon KEYS genuinely differ, so a naive key-string compare mismatches.
	storedKey := mustCanonKey(t, stored, ignore)
	runnerKey := mustCanonKey(t, json.RawMessage(runner), ignore)
	if storedKey == runnerKey {
		t.Fatalf("regression premise broken: canon keys identical (%q) — the token boundary is gone", storedKey)
	}

	snap := audit.ComparisonSnapshot{ComparisonMode: leaf.ComparisonExact, IgnoreFields: ignore}
	v, detail, err := AdjudicateAudit(snap, storedKey, []json.RawMessage{stored}, runner)
	if err != nil {
		t.Fatalf("AdjudicateAudit: %v", err)
	}
	if v != audit.VerdictMatch {
		t.Fatalf("verdict = %q (%s), want MATCH — value-level compare must ignore the jsonb/raw token boundary", v, detail)
	}
}
