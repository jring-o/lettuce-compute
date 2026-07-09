package attestation

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

func testSigner(t *testing.T) *Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return NewSigner(priv)
}

func testAttestation() *Attestation {
	return &Attestation{
		LeafID:          types.NewID(),
		VolunteerPublicKey: make([]byte, ed25519.PublicKeySize),
		WorkUnitID:         types.NewID(),
		RawMetrics: map[string]any{
			"wall_clock_seconds": float64(3600),
			"cpu_seconds_user":   float64(3200),
			"cpu_seconds_system": float64(50),
			"cpu_cores_used":     float64(4),
			"gpu_seconds":        float64(0),
			"gpu_vram_used_mb":   float64(0),
			"peak_memory_mb":     float64(2048),
			"disk_read_mb":       float64(500),
			"disk_write_mb":      float64(100),
			"network_rx_mb":      float64(0),
			"network_tx_mb":      float64(0),
		},
		ValidationOutcome:   OutcomeAgreed,
		CreditAmount:        1.0,
		AttestationTimestamp: time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC),
	}
}

func TestSignVerifyRoundtrip(t *testing.T) {
	signer := testSigner(t)
	att := testAttestation()

	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	att.Signature = sig

	if !VerifyAttestation(signer.PublicKey(), att) {
		t.Error("expected signature verification to succeed")
	}
}

func TestTamperedAttestationFailsVerification(t *testing.T) {
	signer := testSigner(t)
	att := testAttestation()

	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	att.Signature = sig

	// Tamper with credit amount.
	att.CreditAmount = 999.0

	if VerifyAttestation(signer.PublicKey(), att) {
		t.Error("expected signature verification to fail after tampering")
	}
}

func TestCanonicalJSONDeterministic(t *testing.T) {
	att := testAttestation()

	// Call CanonicalJSON multiple times and verify identical output.
	first, err := CanonicalJSON(att)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}

	for i := 0; i < 100; i++ {
		other, err := CanonicalJSON(att)
		if err != nil {
			t.Fatalf("iteration %d: CanonicalJSON: %v", i, err)
		}
		if string(first) != string(other) {
			t.Fatalf("iteration %d: canonical JSON not deterministic\nfirst:  %s\nother:  %s", i, first, other)
		}
	}
}

func TestDifferentAttestationsProduceDifferentSignatures(t *testing.T) {
	signer := testSigner(t)

	att1 := testAttestation()
	att2 := testAttestation()
	att2.WorkUnitID = types.NewID() // Different work unit.

	sig1, err := signer.Sign(att1)
	if err != nil {
		t.Fatalf("Sign att1: %v", err)
	}
	sig2, err := signer.Sign(att2)
	if err != nil {
		t.Fatalf("Sign att2: %v", err)
	}

	if string(sig1) == string(sig2) {
		t.Error("expected different signatures for different attestations")
	}
}

func TestVerifyWithWrongKeyFails(t *testing.T) {
	signer := testSigner(t)
	otherSigner := testSigner(t)
	att := testAttestation()

	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	att.Signature = sig

	if VerifyAttestation(otherSigner.PublicKey(), att) {
		t.Error("expected verification to fail with wrong public key")
	}
}

func TestCanonicalJSONSortedKeys(t *testing.T) {
	att := testAttestation()
	canonical, err := CanonicalJSON(att)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}

	s := string(canonical)
	// Verify keys are in sorted order. raw_metrics is intentionally NOT among the
	// signed keys (self-reported, head-unverified — see CanonicalJSON):
	// attestation_timestamp, credit_amount, leaf_id, validation_outcome,
	// volunteer_public_key, work_unit_id
	keys := []string{
		`"attestation_timestamp"`,
		`"credit_amount"`,
		`"leaf_id"`,
		`"validation_outcome"`,
		`"volunteer_public_key"`,
		`"work_unit_id"`,
	}
	prevIdx := -1
	for _, key := range keys {
		idx := indexOf(s, key)
		if idx == -1 {
			t.Errorf("key %s not found in canonical JSON", key)
			continue
		}
		if idx <= prevIdx {
			t.Errorf("key %s at position %d is not after previous key at position %d", key, idx, prevIdx)
		}
		prevIdx = idx
	}
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func TestPublicKeyReturnsCorrectKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	pub := signer.PublicKey()

	expected := priv.Public().(ed25519.PublicKey)
	if string(pub) != string(expected) {
		t.Error("PublicKey() does not match private key's public key")
	}
}

func TestCanonicalJSON_NilMetrics(t *testing.T) {
	att := testAttestation()
	att.RawMetrics = nil

	canonical, err := CanonicalJSON(att)
	if err != nil {
		t.Fatalf("CanonicalJSON with nil metrics: %v", err)
	}

	// raw_metrics is never part of the signed bytes, nil or not.
	s := string(canonical)
	if indexOf(s, `"raw_metrics"`) != -1 {
		t.Errorf("raw_metrics must be excluded from signed bytes, got: %s", s)
	}
}

func TestCanonicalJSON_EmptyMetrics(t *testing.T) {
	att := testAttestation()
	att.RawMetrics = map[string]any{}

	canonical, err := CanonicalJSON(att)
	if err != nil {
		t.Fatalf("CanonicalJSON with empty metrics: %v", err)
	}

	s := string(canonical)
	if indexOf(s, `"raw_metrics"`) != -1 {
		t.Errorf("raw_metrics must be excluded from signed bytes, got: %s", s)
	}
}

func TestCanonicalJSON_EmptyVolunteerPublicKey(t *testing.T) {
	att := testAttestation()
	att.VolunteerPublicKey = nil

	canonical, err := CanonicalJSON(att)
	if err != nil {
		t.Fatalf("CanonicalJSON with nil volunteer_public_key: %v", err)
	}

	// Should contain an empty base64 string for volunteer_public_key.
	s := string(canonical)
	if indexOf(s, `"volunteer_public_key":""`) == -1 {
		t.Errorf("expected empty volunteer_public_key, got: %s", s)
	}
}

func TestCanonicalJSON_ZeroCreditAmount(t *testing.T) {
	att := testAttestation()
	att.CreditAmount = 0

	canonical, err := CanonicalJSON(att)
	if err != nil {
		t.Fatalf("CanonicalJSON with zero credit: %v", err)
	}

	s := string(canonical)
	if indexOf(s, `"credit_amount":0`) == -1 {
		t.Errorf("expected credit_amount:0, got: %s", s)
	}
}

func TestSignVerify_NilMetrics(t *testing.T) {
	signer := testSigner(t)
	att := testAttestation()
	att.RawMetrics = nil

	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	att.Signature = sig

	if !VerifyAttestation(signer.PublicKey(), att) {
		t.Error("verification should succeed with nil metrics")
	}
}

func TestSignVerify_AllOutcomes(t *testing.T) {
	outcomes := []string{OutcomeAgreed, OutcomeDisagreed, OutcomeExpired}

	for _, outcome := range outcomes {
		t.Run(outcome, func(t *testing.T) {
			signer := testSigner(t)
			att := testAttestation()
			att.ValidationOutcome = outcome

			sig, err := signer.Sign(att)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			att.Signature = sig

			if !VerifyAttestation(signer.PublicKey(), att) {
				t.Errorf("verification should succeed for outcome %s", outcome)
			}
		})
	}
}

func TestVerifyAttestation_CorruptedSignature(t *testing.T) {
	signer := testSigner(t)
	att := testAttestation()

	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	att.Signature = sig

	// Corrupt the signature by flipping bits.
	att.Signature[0] ^= 0xFF
	att.Signature[len(att.Signature)-1] ^= 0xFF

	if VerifyAttestation(signer.PublicKey(), att) {
		t.Error("expected verification to fail with corrupted signature bytes")
	}
}

func TestVerifyAttestation_TruncatedSignature(t *testing.T) {
	signer := testSigner(t)
	att := testAttestation()

	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	att.Signature = sig[:len(sig)/2] // Truncate to half length.

	if VerifyAttestation(signer.PublicKey(), att) {
		t.Error("expected verification to fail with truncated signature")
	}
}

func TestVerifyAttestation_EmptySignature(t *testing.T) {
	signer := testSigner(t)
	att := testAttestation()

	att.Signature = []byte{}

	if VerifyAttestation(signer.PublicKey(), att) {
		t.Error("expected verification to fail with empty signature")
	}
}

func TestVerifyAttestation_NilSignature(t *testing.T) {
	signer := testSigner(t)
	att := testAttestation()

	att.Signature = nil

	if VerifyAttestation(signer.PublicKey(), att) {
		t.Error("expected verification to fail with nil signature")
	}
}

func TestVerifyAttestation_TamperedEachField(t *testing.T) {
	signer := testSigner(t)

	// Test that tampering with each signed field causes verification to fail.
	tests := []struct {
		name   string
		tamper func(att *Attestation)
	}{
		{"leaf_id", func(att *Attestation) { att.LeafID = types.NewID() }},
		{"work_unit_id", func(att *Attestation) { att.WorkUnitID = types.NewID() }},
		{"volunteer_public_key", func(att *Attestation) { att.VolunteerPublicKey = []byte{99, 99} }},
		{"validation_outcome", func(att *Attestation) { att.ValidationOutcome = OutcomeDisagreed }},
		{"attestation_timestamp", func(att *Attestation) {
			att.AttestationTimestamp = att.AttestationTimestamp.Add(time.Second)
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			att := testAttestation()
			sig, err := signer.Sign(att)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			att.Signature = sig

			tc.tamper(att)

			if VerifyAttestation(signer.PublicKey(), att) {
				t.Errorf("expected verification to fail after tampering with %s", tc.name)
			}
		})
	}
}

// TestRawMetricsNotSigned documents the contract: raw_metrics are volunteer
// self-reported and head-unverified, so they are excluded from the signed bytes.
// Mutating them (as a malicious volunteer's fabricated numbers would) must NOT
// invalidate an otherwise-valid signature, and the metrics must not appear in the
// canonical JSON at all.
func TestRawMetricsNotSigned(t *testing.T) {
	signer := testSigner(t)
	att := testAttestation()

	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	att.Signature = sig

	// A volunteer swaps in absurd resource numbers after the head signed.
	att.RawMetrics = map[string]any{
		"cpu_seconds_user": float64(9.9e12),
		"peak_memory_mb":   float64(2_000_000_000),
	}
	if !VerifyAttestation(signer.PublicKey(), att) {
		t.Error("mutating unsigned raw_metrics must not break signature verification")
	}

	// And the metrics never appear in the signed bytes.
	canonical, err := CanonicalJSON(att)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if indexOf(string(canonical), `"raw_metrics"`) != -1 {
		t.Errorf("raw_metrics must not be present in signed canonical JSON: %s", canonical)
	}
}
