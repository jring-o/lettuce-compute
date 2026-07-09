package admission

import (
	"strings"
	"testing"
)

// The full solution-rule test suite (golden byte vector, layout pins, pubkey binding,
// LeadingZeroBits table, solve round-trip) lives with the rule itself in the exported
// module-root package `pow` — the single implementation the volunteer CLI also imports.
// This file keeps (a) a thin golden pin THROUGH the admission delegates, guarding the
// delegation itself, and (b) the refusal-message contracts, which are admission-side
// constants.

// TestPowDelegates_GoldenPin runs the golden vector's primary assertions through the
// admission-package names (VerifySolution/Solve/LeadingZeroBits). If a future edit
// pointed these delegates at anything other than the canonical pow package rule, this
// pin breaks even though the pow package's own suite still passes.
func TestPowDelegates_GoldenPin(t *testing.T) {
	const (
		diff16Nonce = uint64(19497)
		diff16LZB   = 17
	)
	challenge := make([]byte, 32)
	publicKey := make([]byte, 32)
	for i := range challenge {
		challenge[i] = byte(i)      // 0x00..0x1f
		publicKey[i] = byte(32 + i) // 0x20..0x3f
	}

	if got := Solve(challenge, publicKey, 16); got != diff16Nonce {
		t.Fatalf("Solve(diff=16) = %d, want %d (delegation drifted from the pow package rule)", got, diff16Nonce)
	}
	if !VerifySolution(challenge, publicKey, diff16Nonce, diff16LZB) {
		t.Errorf("VerifySolution(diff=%d) = false, want true", diff16LZB)
	}
	if VerifySolution(challenge, publicKey, diff16Nonce, diff16LZB+1) {
		t.Errorf("VerifySolution(diff=%d) = true, want false", diff16LZB+1)
	}
	if got := LeadingZeroBits([]byte{0x00, 0x0F}); got != 12 {
		t.Errorf("LeadingZeroBits(000f) = %d, want 12", got)
	}
}

// TestPowRequiredMessage_Contract pins the two-audience wording contract of the
// pow-required refusal, which the head sends as a FailedPrecondition.
//
// Two distinct clients read this refusal:
//   - EXISTING volunteer builds cannot solve challenges and have no prefix matcher; they
//     classify the status via client.IsVolunteerTooOldError (a string match in the
//     SEPARATE services/volunteer-cli module, deliberately NOT imported here) and, when
//     it matches, print an actionable "run update" hint. That classifier returns true
//     unconditionally when the lower-cased message contains "too old" or "outdated", so
//     the message MUST contain one of those. It uses "outdated".
//   - FUTURE solver-capable builds match PowRequiredMessagePrefix to trigger
//     fetch-solve-retry and never display the human text.
//
// Changing either constant silently orphans one of these audiences, so both are pinned.
func TestPowRequiredMessage_Contract(t *testing.T) {
	// (a) The full message must carry the machine-readable prefix future clients match.
	if !strings.HasPrefix(PowRequiredMessage, PowRequiredMessagePrefix) {
		t.Errorf("PowRequiredMessage %q does not start with PowRequiredMessagePrefix %q",
			PowRequiredMessage, PowRequiredMessagePrefix)
	}

	// (b) The message must trigger the old-CLI classifier. Re-derive only its
	// unconditional message branch locally (contains "too old" or "outdated"), matching
	// the style used for CapExceededMessage in admission_test.go.
	msg := strings.ToLower(PowRequiredMessage)
	if !strings.Contains(msg, "too old") && !strings.Contains(msg, "outdated") {
		t.Errorf("PowRequiredMessage %q contains neither \"too old\" nor \"outdated\"; "+
			"existing volunteer builds would show a generic error instead of the update hint",
			PowRequiredMessage)
	}

	// (c) The prefix is the shipped machine contract: pin its exact bytes and that it is
	// already lower-case (future clients lower-case nothing before matching it, so a
	// stray capital would orphan them).
	const wantPrefix = "registration requires proof-of-work"
	if PowRequiredMessagePrefix != wantPrefix {
		t.Errorf("PowRequiredMessagePrefix = %q, want %q (changing it orphans shipped clients)",
			PowRequiredMessagePrefix, wantPrefix)
	}
	if PowRequiredMessagePrefix != strings.ToLower(PowRequiredMessagePrefix) {
		t.Errorf("PowRequiredMessagePrefix %q is not lower-case stable", PowRequiredMessagePrefix)
	}
}
