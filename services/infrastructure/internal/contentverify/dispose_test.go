package contentverify

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// errTest is the underlying error folded into a synthetic FetchError in these tests.
var errTest = errors.New("simulated fetch error")

const (
	testClaimedHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testServedHash  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testGlobalCap   = int64(104857600) // the 100 MB effective global default
)

// baseSnap is an opted-in, allowlisted, freshly submitted ref on a COMPLETED unit —
// the row that should fetch and promote. Each test perturbs one field.
func baseSnap() rowSnapshot {
	return rowSnapshot{
		resultID:        types.NewID(),
		workUnitID:      types.NewID(),
		leafID:          types.NewID(),
		volunteerID:     types.NewID(),
		outputDataRef:   "https://cdn.example.com/output.json",
		claimedChecksum: testClaimedHash,
		attempts:        0,
		createdAt:       time.Now(),
		unitState:       workunit.WorkUnitStateCompleted,
		valCfg: leaf.ValidationConfig{
			AllowExternalOutput: true,
			ExternalOutputHosts: []string{"cdn.example.com"},
		},
		dataCfg: leaf.DataConfig{MaxOutputSizeBytes: 0},
	}
}

// success/transient/permanent outcomes for the disposition pass.
func okOutcome(hash string) fetchOutcome {
	return fetchOutcome{fetched: true, hash: hash}
}
func errOutcome(fe *FetchError) fetchOutcome {
	return fetchOutcome{fetched: true, err: fe}
}

// TestDecidePrefetchDueRowFetches: a due opted-in row with no outcome yet returns
// actionFetch carrying the composed byte cap.
func TestDecidePrefetchDueRowFetches(t *testing.T) {
	d := decide(baseSnap(), true, testGlobalCap, time.Now(), fetchOutcome{})
	if d.action != actionFetch {
		t.Fatalf("action = %d, want actionFetch", d.action)
	}
	if d.fetchCap != testGlobalCap {
		t.Errorf("fetchCap = %d, want %d (leaf cap 0 → global)", d.fetchCap, testGlobalCap)
	}
}

// TestDecideFetchCap pins min(leaf cap>0, global) — the ref-bypass close.
func TestDecideFetchCap(t *testing.T) {
	cases := []struct {
		name    string
		leafCap int64
		want    int64
	}{
		{"leaf-unset-uses-global", 0, testGlobalCap},
		{"leaf-smaller-wins", 50, 50},
		{"leaf-larger-clamped-to-global", testGlobalCap + 1, testGlobalCap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := baseSnap()
			s.dataCfg.MaxOutputSizeBytes = tc.leafCap
			d := decide(s, true, testGlobalCap, time.Now(), fetchOutcome{})
			if d.action != actionFetch {
				t.Fatalf("action = %d, want actionFetch", d.action)
			}
			if d.fetchCap != tc.want {
				t.Errorf("fetchCap = %d, want %d", d.fetchCap, tc.want)
			}
		})
	}
}

// TestDecideSuccessHashEqualsClaimPromotes: success where the served hash matches the
// claim → promote on the served hash, no mismatch flag.
func TestDecideSuccessHashEqualsClaimPromotes(t *testing.T) {
	d := decide(baseSnap(), true, testGlobalCap, time.Now(), okOutcome(testClaimedHash))
	if d.action != actionPromote {
		t.Fatalf("action = %d, want actionPromote", d.action)
	}
	if d.servedHash != testClaimedHash {
		t.Errorf("servedHash = %s, want %s (votes on the head-computed hash)", d.servedHash, testClaimedHash)
	}
	if d.claimMismatch {
		t.Error("hash == claim must not flag a mismatch")
	}
}

// TestDecidePromotesServedHashOnClaimMismatch is THE audit-F2 regression: a successful
// fetch whose served hash differs from the volunteer's claim ALSO promotes — on the
// SERVED hash, with a diagnostic-only mismatch flag and NO sanction. It fails against
// any claimed-vs-served gate (which would terminate or slash the row instead of
// promoting it).
func TestDecidePromotesServedHashOnClaimMismatch(t *testing.T) {
	d := decide(baseSnap(), true, testGlobalCap, time.Now(), okOutcome(testServedHash))
	if d.action != actionPromote {
		t.Fatalf("action = %d, want actionPromote (a served/claimed divergence is NOT a gate — audit F2)", d.action)
	}
	if d.servedHash != testServedHash {
		t.Errorf("servedHash = %s, want the SERVED hash %s", d.servedHash, testServedHash)
	}
	if !d.claimMismatch {
		t.Error("a served/claimed divergence must set the diagnostic mismatch flag")
	}
	// No sanction: the disposition carries no terminal, no reason code — the ref just
	// votes on the served hash and a wrong result becomes an ordinary DISAGREED later.
	if d.reasonCode != "" {
		t.Errorf("a mismatch promotion must carry no reason code, got %q", d.reasonCode)
	}
}

// TestDecideUnitFinalized: a promotion whose unit already VALIDATED/FAILED terminates
// UNIT_FINALIZED instead (the seat is gone); COMPLETED/QUEUED/REJECTED promote.
func TestDecideUnitFinalized(t *testing.T) {
	finalize := []workunit.WorkUnitState{workunit.WorkUnitStateValidated, workunit.WorkUnitStateFailed}
	for _, st := range finalize {
		s := baseSnap()
		s.unitState = st
		d := decide(s, true, testGlobalCap, time.Now(), okOutcome(testServedHash))
		if d.action != actionTerminal || d.reasonCode != CodeUnitFinalized {
			t.Errorf("unit %s: action=%d reason=%s, want terminal UNIT_FINALIZED", st, d.action, d.reasonCode)
		}
	}
	promote := []workunit.WorkUnitState{workunit.WorkUnitStateCompleted, workunit.WorkUnitStateQueued, workunit.WorkUnitStateRejected}
	for _, st := range promote {
		s := baseSnap()
		s.unitState = st
		d := decide(s, true, testGlobalCap, time.Now(), okOutcome(testServedHash))
		if d.action != actionPromote {
			t.Errorf("unit %s: action=%d, want actionPromote", st, d.action)
		}
	}
}

// TestDecideTransientRetryThenFail: transient failures retry until the budget is spent,
// then terminate FETCH_FAILED. attempts is incremented ONLY on the retry.
func TestDecideTransientRetryThenFail(t *testing.T) {
	transient := &FetchError{Code: CodeNetworkError, Transient: true, Err: errTest}
	cases := []struct {
		attempts   int
		wantAction dispositionAction
		wantAtt    int
		wantReason string
	}{
		{0, actionRetry, 1, CodeNetworkError},
		{1, actionRetry, 2, CodeNetworkError},
		{2, actionTerminal, 0, CodeFetchFailed}, // attempts+1 == maxAttempts(3)
	}
	for _, tc := range cases {
		s := baseSnap()
		s.attempts = tc.attempts
		d := decide(s, true, testGlobalCap, time.Now(), errOutcome(transient))
		if d.action != tc.wantAction || d.reasonCode != tc.wantReason {
			t.Errorf("attempts=%d: action=%d reason=%s, want action=%d reason=%s",
				tc.attempts, d.action, d.reasonCode, tc.wantAction, tc.wantReason)
		}
		if tc.wantAction == actionRetry && d.attempts != tc.wantAtt {
			t.Errorf("attempts=%d: retry stored attempts=%d, want %d", tc.attempts, d.attempts, tc.wantAtt)
		}
	}
}

// TestDecideAttemptsIncrementOnlyOnTransient: neither a success (any hash) nor a
// permanent failure touches the attempts budget — only a transient retry does.
func TestDecideAttemptsIncrementOnlyOnTransient(t *testing.T) {
	s := baseSnap()
	s.attempts = 1

	if d := decide(s, true, testGlobalCap, time.Now(), okOutcome(testServedHash)); d.attempts != 0 {
		t.Errorf("success carried attempts=%d, want 0 (success never consumes the budget)", d.attempts)
	}
	perm := &FetchError{Code: CodeHTTPStatus, Transient: false, Err: errTest}
	if d := decide(s, true, testGlobalCap, time.Now(), errOutcome(perm)); d.attempts != 0 {
		t.Errorf("permanent failure carried attempts=%d, want 0", d.attempts)
	}
	tr := &FetchError{Code: CodeNetworkError, Transient: true, Err: errTest}
	if d := decide(s, true, testGlobalCap, time.Now(), errOutcome(tr)); d.attempts != 2 {
		t.Errorf("transient retry stored attempts=%d, want 2", d.attempts)
	}
}

// TestDecidePermanentFailuresTerminate: each permanent fetch code terminates
// immediately with that code and the underlying detail folded into last_error.
func TestDecidePermanentFailuresTerminate(t *testing.T) {
	codes := []string{CodeRedirectRefused, CodeHTTPStatus, CodeSizeExceeded, CodeDisallowedAddress}
	for _, code := range codes {
		fe := &FetchError{Code: code, Transient: false, Err: errTest}
		d := decide(baseSnap(), true, testGlobalCap, time.Now(), errOutcome(fe))
		if d.action != actionTerminal || d.reasonCode != code {
			t.Errorf("code %s: action=%d reason=%s, want terminal %s", code, d.action, d.reasonCode, code)
		}
		if !strings.HasPrefix(d.lastError, code) {
			t.Errorf("code %s: lastError=%q, want it to start with the code", code, d.lastError)
		}
	}
}

// TestDecideExpiryLane: a row older than the holding lifetime terminates —
// HOLDING_EXPIRED with the knob on, FETCH_DISABLED with it off — regardless of outcome.
func TestDecideExpiryLane(t *testing.T) {
	old := time.Now().Add(-holdingLifetime - time.Hour)

	s := baseSnap()
	s.createdAt = old
	if d := decide(s, true, testGlobalCap, time.Now(), fetchOutcome{}); d.action != actionTerminal || d.reasonCode != CodeHoldingExpired {
		t.Errorf("knob-on expiry: action=%d reason=%s, want terminal HOLDING_EXPIRED", d.action, d.reasonCode)
	}
	if d := decide(s, false, testGlobalCap, time.Now(), fetchOutcome{}); d.action != actionTerminal || d.reasonCode != CodeFetchDisabled {
		t.Errorf("knob-off expiry: action=%d reason=%s, want terminal FETCH_DISABLED", d.action, d.reasonCode)
	}
}

// TestDecideKnobOffYoungLeavesRow: with fetching disabled, a row younger than the
// holding lifetime is left untouched — no fetch, no write.
func TestDecideKnobOffYoungLeavesRow(t *testing.T) {
	d := decide(baseSnap(), false, testGlobalCap, time.Now(), fetchOutcome{})
	if d.action != actionNone {
		t.Fatalf("action = %d, want actionNone (knob off, row young → leave)", d.action)
	}
}

// TestDecideReCheckOptOut: a leaf that opted out after submit terminates
// LEAF_OPTED_OUT before any fetch (the D10 fetch-time re-check).
func TestDecideReCheckOptOut(t *testing.T) {
	s := baseSnap()
	s.valCfg.AllowExternalOutput = false
	d := decide(s, true, testGlobalCap, time.Now(), fetchOutcome{})
	if d.action != actionTerminal || d.reasonCode != CodeLeafOptedOut {
		t.Errorf("action=%d reason=%s, want terminal LEAF_OPTED_OUT", d.action, d.reasonCode)
	}
}

// TestDecideReCheckURLDisallowed: a URL no longer allowed by the CURRENT config
// terminates URL_DISALLOWED, with the validator message as detail.
func TestDecideReCheckURLDisallowed(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		cfg  leaf.ValidationConfig
	}{
		{
			name: "host-removed-from-allowlist",
			ref:  "https://cdn.example.com/output.json",
			cfg:  leaf.ValidationConfig{AllowExternalOutput: true, ExternalOutputHosts: []string{"other.example.com"}},
		},
		{
			name: "scheme-no-longer-https",
			ref:  "http://cdn.example.com/output.json",
			cfg:  leaf.ValidationConfig{AllowExternalOutput: true, ExternalOutputHosts: []string{"cdn.example.com"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := baseSnap()
			s.outputDataRef = tc.ref
			s.valCfg = tc.cfg
			d := decide(s, true, testGlobalCap, time.Now(), fetchOutcome{})
			if d.action != actionTerminal || d.reasonCode != CodeURLDisallowed {
				t.Fatalf("action=%d reason=%s, want terminal URL_DISALLOWED", d.action, d.reasonCode)
			}
			if !strings.HasPrefix(d.lastError, CodeURLDisallowed+": ") {
				t.Errorf("lastError=%q, want the validator message as detail", d.lastError)
			}
		})
	}
}
