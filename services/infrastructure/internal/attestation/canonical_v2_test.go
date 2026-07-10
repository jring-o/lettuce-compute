package attestation

import (
	"bytes"
	"crypto/ed25519"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// This file pins the v2 canonical signed forms (design §8.2), the v1 frozen form, and the
// audit findings F-H1 (credit stored == signed), F-M2 (escape-free/float-free bytes), and
// F-L4 (nested descriptor canonicalization must not track Go struct order). The unit-test
// slate it satisfies is design §8.9(i)-(vi). Every fixture uses FIXED literal values so the
// golden byte strings below are a hand-verifiable, frozen wire contract (the recipe's worked
// example). Symbols are cvt*-prefixed to stay collision-free with sibling test files.

// Fixed identifiers shared by every fixture: distinct, literal UUIDs so a transplant across
// fields is visible in the golden bytes.
var (
	cvtTimestamp  = time.Date(2026, 3, 14, 12, 0, 0, 123456000, time.UTC).Truncate(time.Microsecond)
	cvtLeafID     = types.MustParseID("11111111-1111-1111-1111-111111111111")
	cvtWorkUnitID = types.MustParseID("22222222-2222-2222-2222-222222222222")
	cvtResultID   = types.MustParseID("33333333-3333-3333-3333-333333333333")
	cvtRevokesID  = types.MustParseID("44444444-4444-4444-4444-444444444444")
	cvtAdjustID   = types.MustParseID("55555555-5555-5555-5555-555555555555")
)

// cvtChecksum is a literal, lower-hex, 64-char sha256 (16 hex chars repeated four times).
const cvtChecksum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// The frozen golden canonical byte strings. Any change to the canonicalizer that moves a
// byte here is a wire break and must fail these tests. volunteer_public_key is the base64url
// of a 32-byte all-zero key (43 'A' characters, no padding under RawURLEncoding). credit is a
// STRING (v2) but a bare number (v1). policy_version is pinned to 1: when
// attestation.PolicyVersion bumps, this golden and the published recipe's worked example
// update together.
const (
	cvtGoldenV2Grant = `{"attestation_timestamp":"2026-03-14T12:00:00.123456Z","context":"lettuce/credit-attestation/v2","credit_amount":"1.000000","leaf_id":"11111111-1111-1111-1111-111111111111","output_checksum":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","policy_version":1,"quorum_descriptor":{"audit_rate_ppm":10000,"group_size":3,"min_quorum":2,"min_trusted_corroborators":1,"pending_size":5,"target_copies":5,"trust_floor":100,"trusted_corroborators":2},"result_id":"33333333-3333-3333-3333-333333333333","schema_version":2,"validation_outcome":"AGREED","volunteer_public_key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","work_unit_id":"22222222-2222-2222-2222-222222222222"}`

	cvtGoldenV2Revocation = `{"adjustment_id":"55555555-5555-5555-5555-555555555555","attestation_timestamp":"2026-03-14T12:00:00.123456Z","context":"lettuce/credit-attestation-revocation/v2","credit_amount":"1.000000","leaf_id":"11111111-1111-1111-1111-111111111111","reason":"OPERATOR_CLAWBACK","result_id":"33333333-3333-3333-3333-333333333333","revokes_attestation_id":"44444444-4444-4444-4444-444444444444","schema_version":2,"volunteer_public_key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","work_unit_id":"22222222-2222-2222-2222-222222222222"}`

	cvtGoldenV1 = `{"attestation_timestamp":"2026-03-14T12:00:00.123456Z","credit_amount":1,"leaf_id":"11111111-1111-1111-1111-111111111111","validation_outcome":"AGREED","volunteer_public_key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","work_unit_id":"22222222-2222-2222-2222-222222222222"}`
)

// cvtDescriptor is the golden quorum descriptor: all 8 fields nonzero (AuditRatePPM 10000).
func cvtDescriptor() *QuorumDescriptor {
	return &QuorumDescriptor{
		AuditRatePPM:            10000,
		GroupSize:               3,
		MinQuorum:               2,
		MinTrustedCorroborators: 1,
		PendingSize:             5,
		TargetCopies:            5,
		TrustFloor:              100,
		TrustedCorroborators:    2,
	}
}

// cvtV2Grant builds a fully-populated v2 grant (AGREED) attestation from the fixed vectors.
// Fresh pointers each call so a per-subtest mutation cannot leak across table rows.
func cvtV2Grant() *Attestation {
	resultID := cvtResultID
	checksum := cvtChecksum
	policy := 1
	return &Attestation{
		SchemaVersion:         SchemaVersionV2,
		LeafID:                cvtLeafID,
		VolunteerPublicKey:    make([]byte, ed25519.PublicKeySize),
		WorkUnitID:            cvtWorkUnitID,
		ResultID:              &resultID,
		OutputChecksum:        &checksum,
		QuorumDescriptor:      cvtDescriptor(),
		PolicyVersion:         &policy,
		ValidationOutcome:     OutcomeAgreed,
		CreditAmount:          1.0,
		CreditAmountCanonical: "1.000000",
		AttestationTimestamp:  cvtTimestamp,
	}
}

// cvtV2Revocation builds a fully-populated v2 revocation (REVOKED) attestation; the outcome
// routes CanonicalJSON to the revocation form, whose context IS the statement type.
func cvtV2Revocation() *Attestation {
	resultID := cvtResultID
	revokes := cvtRevokesID
	adjust := cvtAdjustID
	reason := "OPERATOR_CLAWBACK"
	return &Attestation{
		SchemaVersion:         SchemaVersionV2,
		LeafID:                cvtLeafID,
		VolunteerPublicKey:    make([]byte, ed25519.PublicKeySize),
		WorkUnitID:            cvtWorkUnitID,
		ResultID:              &resultID,
		RevokesAttestationID:  &revokes,
		AdjustmentID:          &adjust,
		Reason:                &reason,
		ValidationOutcome:     OutcomeRevoked,
		CreditAmount:          1.0,
		CreditAmountCanonical: "1.000000",
		AttestationTimestamp:  cvtTimestamp,
	}
}

// cvtV1 builds a v1 attestation at the given schema version (0 or 1 both take the frozen v1
// form). CreditAmountCanonical is deliberately empty: v1 signs the float64 directly.
func cvtV1(schemaVersion int) *Attestation {
	return &Attestation{
		SchemaVersion:        schemaVersion,
		LeafID:               cvtLeafID,
		VolunteerPublicKey:   make([]byte, ed25519.PublicKeySize),
		WorkUnitID:           cvtWorkUnitID,
		ValidationOutcome:    OutcomeAgreed,
		CreditAmount:         1.0,
		AttestationTimestamp: cvtTimestamp,
	}
}

// TestCanonicalV2_GoldenVector pins the exact v2 grant and revocation canonical bytes
// (design §8.2, §8.9(i)) — the frozen worked example. The literals above ARE the golden; the
// round-trip at the end proves the frozen bytes actually sign and verify.
func TestCanonicalV2_GoldenVector(t *testing.T) {
	gotGrant, err := CanonicalJSON(cvtV2Grant())
	if err != nil {
		t.Fatalf("CanonicalJSON grant: %v", err)
	}
	if string(gotGrant) != cvtGoldenV2Grant {
		t.Errorf("v2 grant canonical bytes drifted\n got: %s\nwant: %s", gotGrant, cvtGoldenV2Grant)
	}

	gotRev, err := CanonicalJSON(cvtV2Revocation())
	if err != nil {
		t.Fatalf("CanonicalJSON revocation: %v", err)
	}
	if string(gotRev) != cvtGoldenV2Revocation {
		t.Errorf("v2 revocation canonical bytes drifted\n got: %s\nwant: %s", gotRev, cvtGoldenV2Revocation)
	}

	signer := testSigner(t)
	for name, att := range map[string]*Attestation{"grant": cvtV2Grant(), "revocation": cvtV2Revocation()} {
		sig, err := signer.Sign(att)
		if err != nil {
			t.Fatalf("Sign %s: %v", name, err)
		}
		att.Signature = sig
		if !VerifyAttestation(signer.PublicKey(), att) {
			t.Errorf("golden %s did not round-trip sign/verify", name)
		}
	}
}

// TestCanonicalV2_NoEscapeBytes asserts the escape-free guarantee (audit F-M2): an
// adversarially-populated grant and revocation contain no 0x5C ('\') byte, so the head's
// manual marshal and any third-party JSON serializer produce identical bytes. Values are
// pushed to their charset/type bounds (all-0xFF key → base64url _-, all-f checksum, max ints,
// negative max-scale credit, reason at the charset extremes).
func TestCanonicalV2_NoEscapeBytes(t *testing.T) {
	maxKey := make([]byte, ed25519.PublicKeySize)
	for i := range maxKey {
		maxKey[i] = 0xFF
	}
	allF := strings.Repeat("f", 64)

	grant := cvtV2Grant()
	grant.VolunteerPublicKey = maxKey
	grant.OutputChecksum = &allF
	grant.CreditAmountCanonical = "-999999.999999"
	grant.QuorumDescriptor = &QuorumDescriptor{
		AuditRatePPM:            1000000,
		GroupSize:               math.MaxInt32,
		MinQuorum:               math.MaxInt32,
		MinTrustedCorroborators: math.MaxInt32,
		PendingSize:             math.MaxInt32,
		TargetCopies:            math.MaxInt32,
		TrustFloor:              math.MaxInt32,
		TrustedCorroborators:    math.MaxInt32,
	}
	gotGrant, err := CanonicalJSON(grant)
	if err != nil {
		t.Fatalf("CanonicalJSON adversarial grant: %v", err)
	}
	if bytes.IndexByte(gotGrant, '\\') != -1 {
		t.Errorf("adversarial grant canonical bytes contain a backslash: %s", gotGrant)
	}

	rev := cvtV2Revocation()
	rev.VolunteerPublicKey = maxKey
	rev.CreditAmountCanonical = "-999999.999999"
	reason := "A0_Z9" // at the ^[A-Z0-9_]{1,64}$ charset bounds
	rev.Reason = &reason
	gotRev, err := CanonicalJSON(rev)
	if err != nil {
		t.Fatalf("CanonicalJSON adversarial revocation: %v", err)
	}
	if bytes.IndexByte(gotRev, '\\') != -1 {
		t.Errorf("adversarial revocation canonical bytes contain a backslash: %s", gotRev)
	}
}

// TestCanonicalV2_TamperedEachField mirrors the v1 suite's
// TestVerifyAttestation_TamperedEachField (§8.9(ii)): sign a v2 grant, then mutate exactly one
// signed field per row and assert verification fails. Coverage spans every top-level signed
// field, every one of the 8 quorum_descriptor subfields, and schema_version→1 (which also
// proves cross-version separation: the v2 signature cannot verify under the v1 form).
func TestCanonicalV2_TamperedEachField(t *testing.T) {
	signer := testSigner(t)

	tests := []struct {
		name   string
		tamper func(att *Attestation)
	}{
		{"attestation_timestamp", func(a *Attestation) { a.AttestationTimestamp = a.AttestationTimestamp.Add(time.Second) }},
		{"credit_amount_canonical", func(a *Attestation) { a.CreditAmountCanonical = "2.000000" }},
		{"leaf_id", func(a *Attestation) { a.LeafID = types.NewID() }},
		{"output_checksum", func(a *Attestation) { c := strings.Repeat("f", 64); a.OutputChecksum = &c }},
		{"policy_version", func(a *Attestation) { p := *a.PolicyVersion + 1; a.PolicyVersion = &p }},
		{"descriptor_audit_rate_ppm", func(a *Attestation) { a.QuorumDescriptor.AuditRatePPM++ }},
		{"descriptor_group_size", func(a *Attestation) { a.QuorumDescriptor.GroupSize++ }},
		{"descriptor_min_quorum", func(a *Attestation) { a.QuorumDescriptor.MinQuorum++ }},
		{"descriptor_min_trusted_corroborators", func(a *Attestation) { a.QuorumDescriptor.MinTrustedCorroborators++ }},
		{"descriptor_pending_size", func(a *Attestation) { a.QuorumDescriptor.PendingSize++ }},
		{"descriptor_target_copies", func(a *Attestation) { a.QuorumDescriptor.TargetCopies++ }},
		{"descriptor_trust_floor", func(a *Attestation) { a.QuorumDescriptor.TrustFloor++ }},
		{"descriptor_trusted_corroborators", func(a *Attestation) { a.QuorumDescriptor.TrustedCorroborators++ }},
		{"result_id", func(a *Attestation) { id := types.NewID(); a.ResultID = &id }},
		{"schema_version_to_v1", func(a *Attestation) { a.SchemaVersion = SchemaVersionV1 }},
		{"validation_outcome", func(a *Attestation) { a.ValidationOutcome = OutcomeDisagreed }},
		{"volunteer_public_key", func(a *Attestation) { k := make([]byte, ed25519.PublicKeySize); k[0] = 1; a.VolunteerPublicKey = k }},
		{"work_unit_id", func(a *Attestation) { a.WorkUnitID = types.NewID() }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			att := cvtV2Grant()
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

// TestCanonicalV2_UnsignedMetricsStayUnsigned is the v2 heir of TestRawMetricsNotSigned
// (§8.9(v), BG-06): volunteer-reported raw_metrics are excluded from the signed bytes, so
// mutating them cannot break verification and they never appear in the canonical JSON. It also
// pins the F-H1 posture: CreditAmount (the float64) is DISPLAY-ONLY — the signed value is the
// fixed-scale CreditAmountCanonical string — so mutating the float alone still verifies.
func TestCanonicalV2_UnsignedMetricsStayUnsigned(t *testing.T) {
	signer := testSigner(t)
	att := cvtV2Grant()
	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	att.Signature = sig

	att.RawMetrics = map[string]any{
		"cpu_seconds_user": float64(9.9e12),
		"peak_memory_mb":   float64(2_000_000_000),
	}
	if !VerifyAttestation(signer.PublicKey(), att) {
		t.Error("mutating unsigned raw_metrics must not break v2 verification")
	}

	canonical, err := CanonicalJSON(att)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if indexOf(string(canonical), `"raw_metrics"`) != -1 {
		t.Errorf("raw_metrics must be absent from v2 signed bytes: %s", canonical)
	}

	// The display float diverges from the signed string; verification tracks the string.
	att.CreditAmount = 999.0
	if !VerifyAttestation(signer.PublicKey(), att) {
		t.Error("mutating the display-only CreditAmount float must not break verification (the signed value is CreditAmountCanonical, F-H1)")
	}
}

// TestCanonicalV2_Transplant is the BG-06a item-2 regression (§8.9(iii)): two v2 grants that
// share the entire v1 six-tuple but differ only in result_id produce different canonical bytes
// and one's signature does not verify the other. The documenting inverse shows the flaw v2
// fixes: two v1 attestations with an identical six-tuple but different result_id canonicalize
// to IDENTICAL bytes (v1 never signed result_id), so a signature transplants between them.
func TestCanonicalV2_Transplant(t *testing.T) {
	signer := testSigner(t)

	a := cvtV2Grant()
	b := cvtV2Grant()
	other := types.MustParseID("99999999-9999-9999-9999-999999999999")
	b.ResultID = &other // the ONLY difference

	ca, err := CanonicalJSON(a)
	if err != nil {
		t.Fatalf("CanonicalJSON a: %v", err)
	}
	cb, err := CanonicalJSON(b)
	if err != nil {
		t.Fatalf("CanonicalJSON b: %v", err)
	}
	if string(ca) == string(cb) {
		t.Fatal("v2 grants differing in result_id must have different canonical bytes")
	}

	sig, err := signer.Sign(a)
	if err != nil {
		t.Fatalf("Sign a: %v", err)
	}
	b.Signature = sig
	if VerifyAttestation(signer.PublicKey(), b) {
		t.Error("a's signature must not verify b (result_id is signed in v2)")
	}

	// Documenting inverse: v1 ignores result_id, so the six-tuple alone fixes the bytes.
	v1a := cvtV1(SchemaVersionV1)
	v1b := cvtV1(SchemaVersionV1)
	rid := types.NewID()
	v1b.ResultID = &rid // ignored by the frozen v1 form
	c1a, err := CanonicalJSON(v1a)
	if err != nil {
		t.Fatalf("CanonicalJSON v1a: %v", err)
	}
	c1b, err := CanonicalJSON(v1b)
	if err != nil {
		t.Fatalf("CanonicalJSON v1b: %v", err)
	}
	if string(c1a) != string(c1b) {
		t.Errorf("v1 six-tuple-identical attestations must canonicalize identically (documents the transplant flaw)\n a: %s\n b: %s", c1a, c1b)
	}
}

// TestCanonicalV2_DomainSeparation pins §8.9(iv): the same logical facts rendered as v1,
// v2-grant, and v2-revocation yield three pairwise-distinct canonical byte strings, and a
// signature made over one form does not verify under another (the context field domain-
// separates the byte spaces).
func TestCanonicalV2_DomainSeparation(t *testing.T) {
	signer := testSigner(t)

	cv1, err := CanonicalJSON(cvtV1(SchemaVersionV1))
	if err != nil {
		t.Fatalf("CanonicalJSON v1: %v", err)
	}
	grant := cvtV2Grant()
	cg, err := CanonicalJSON(grant)
	if err != nil {
		t.Fatalf("CanonicalJSON grant: %v", err)
	}
	rev := cvtV2Revocation()
	cr, err := CanonicalJSON(rev)
	if err != nil {
		t.Fatalf("CanonicalJSON revocation: %v", err)
	}

	if string(cv1) == string(cg) || string(cv1) == string(cr) || string(cg) == string(cr) {
		t.Errorf("v1/v2-grant/v2-revocation canonical bytes must be pairwise distinct\n v1:  %s\n grt: %s\n rev: %s", cv1, cg, cr)
	}

	sigGrant, err := signer.Sign(grant)
	if err != nil {
		t.Fatalf("Sign grant: %v", err)
	}
	sigRev, err := signer.Sign(rev)
	if err != nil {
		t.Fatalf("Sign revocation: %v", err)
	}

	rev.Signature = sigGrant
	if VerifyAttestation(signer.PublicKey(), rev) {
		t.Error("a grant-form signature must not verify a revocation-form payload")
	}
	grant.Signature = sigRev
	if VerifyAttestation(signer.PublicKey(), grant) {
		t.Error("a revocation-form signature must not verify a grant-form payload")
	}
}

// TestCanonicalV1_Frozen pins the v1 form so the v2 work provably did not move v1 bytes.
// SchemaVersion 0 (zero value) and 1 both take the frozen six-field form; the literal is
// derived by hand from canonicalV1 (sorted keys, float credit via json.Marshal → bare 1).
func TestCanonicalV1_Frozen(t *testing.T) {
	for _, sv := range []int{0, SchemaVersionV1} {
		att := cvtV1(sv)
		got, err := CanonicalJSON(att)
		if err != nil {
			t.Fatalf("CanonicalJSON (schema_version %d): %v", sv, err)
		}
		if string(got) != cvtGoldenV1 {
			t.Errorf("v1 canonical bytes drifted at schema_version %d\n got: %s\nwant: %s", sv, got, cvtGoldenV1)
		}
	}
}

// TestCanonicalCreditString pins CanonicalCreditString == strconv.FormatFloat(x,'f',6,64):
// always exactly 6 fractional digits, with a leading '-' for negatives.
func TestCanonicalCreditString(t *testing.T) {
	sixDP := regexp.MustCompile(`^-?[0-9]+\.[0-9]{6}$`)

	exact := []struct {
		in   float64
		want string
	}{
		{1.0, "1.000000"},
		{0, "0.000000"},
		{0.1, "0.100000"},
		{2.5, "2.500000"},
		{-1.5, "-1.500000"},
	}
	for _, c := range exact {
		got := CanonicalCreditString(c.in)
		if got != c.want {
			t.Errorf("CanonicalCreditString(%v) = %q, want %q", c.in, got, c.want)
		}
		if !sixDP.MatchString(got) {
			t.Errorf("CanonicalCreditString(%v) = %q does not match the 6-dp shape", c.in, got)
		}
		if got != strconv.FormatFloat(c.in, 'f', 6, 64) {
			t.Errorf("CanonicalCreditString(%v) = %q diverges from strconv.FormatFloat", c.in, got)
		}
	}

	// A value whose shortest repr carries a 7th fractional digit: the exact rounded string is
	// whatever FormatFloat yields, and CanonicalCreditString must equal it byte-for-byte.
	tie := CanonicalCreditString(0.1234565)
	if want := strconv.FormatFloat(0.1234565, 'f', 6, 64); tie != want {
		t.Errorf("CanonicalCreditString(0.1234565) = %q, want %q", tie, want)
	}
	if !regexp.MustCompile(`^[0-9]+\.[0-9]{6}$`).MatchString(tie) {
		t.Errorf("CanonicalCreditString(0.1234565) = %q does not match the 6-dp shape", tie)
	}
	if got := CanonicalCreditString(-3.25); !strings.HasPrefix(got, "-") {
		t.Errorf("negative input must format with a leading '-', got %q", got)
	}
}

// TestNormalizeOutputChecksum pins the admissibility rule for output_checksum in signed bytes
// (F-L5): 64 lower-hex or empty passes; upper-hex is lowercased; anything else is rejected as
// ("", false). Surrounding whitespace is trimmed before the check.
func TestNormalizeOutputChecksum(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"lower_64_hex", cvtChecksum, cvtChecksum, true},
		{"upper_lowered", strings.ToUpper(cvtChecksum), cvtChecksum, true},
		{"empty", "", "", true},
		{"trimmed", "  " + cvtChecksum + "  ", cvtChecksum, true},
		{"whitespace_only", "   ", "", true},
		{"too_short_63", cvtChecksum[:63], "", false},
		{"too_long_65", cvtChecksum + "a", "", false},
		{"non_hex", strings.Repeat("z", 64), "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeOutputChecksum(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Errorf("NormalizeOutputChecksum(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestCanonicalV2_MalformedRejected pins that CanonicalJSON errors (rather than emits partial
// or unverifiable bytes, §8.9/§8.12) when a required v2 field is missing or malformed: a grant
// missing any signed field or carrying a bad credit string / bad checksum, and a revocation
// with a reason outside ^[A-Z0-9_]{1,64}$ or a non-6dp credit string.
func TestCanonicalV2_MalformedRejected(t *testing.T) {
	grantCases := []struct {
		name   string
		mutate func(att *Attestation)
	}{
		{"nil_result_id", func(a *Attestation) { a.ResultID = nil }},
		{"nil_output_checksum", func(a *Attestation) { a.OutputChecksum = nil }},
		{"nil_quorum_descriptor", func(a *Attestation) { a.QuorumDescriptor = nil }},
		{"nil_policy_version", func(a *Attestation) { a.PolicyVersion = nil }},
		{"bad_credit_two_dp", func(a *Attestation) { a.CreditAmountCanonical = "1.00" }},
		{"bad_checksum_non_hex", func(a *Attestation) { c := strings.Repeat("z", 64); a.OutputChecksum = &c }},
	}
	for _, tc := range grantCases {
		t.Run("grant_"+tc.name, func(t *testing.T) {
			att := cvtV2Grant()
			tc.mutate(att)
			if _, err := CanonicalJSON(att); err == nil {
				t.Errorf("expected CanonicalJSON to reject grant with %s", tc.name)
			}
		})
	}

	revCases := []struct {
		name   string
		mutate func(att *Attestation)
	}{
		{"reason_lowercase", func(a *Attestation) { r := "lower"; a.Reason = &r }},
		{"reason_ampersand", func(a *Attestation) { r := "A&B"; a.Reason = &r }},
		{"bad_credit_two_dp", func(a *Attestation) { a.CreditAmountCanonical = "1.00" }},
		{"nil_adjustment_id", func(a *Attestation) { a.AdjustmentID = nil }},
	}
	for _, tc := range revCases {
		t.Run("revocation_"+tc.name, func(t *testing.T) {
			att := cvtV2Revocation()
			tc.mutate(att)
			if _, err := CanonicalJSON(att); err == nil {
				t.Errorf("expected CanonicalJSON to reject revocation with %s", tc.name)
			}
		})
	}
}

// TestCanonicalV2_DescriptorOrderPinned pins F-L4: the nested quorum_descriptor renders its 8
// keys in exactly the documented alphabetical order (parsed out of the golden bytes), and a
// reflection guard fixes the field count at 8 — so adding a QuorumDescriptor field without
// also updating descriptorKV in signer.go breaks this test rather than silently changing the
// signed bytes.
func TestCanonicalV2_DescriptorOrderPinned(t *testing.T) {
	got, err := CanonicalJSON(cvtV2Grant())
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	s := string(got)

	const marker = `"quorum_descriptor":{`
	start := indexOf(s, marker)
	if start == -1 {
		t.Fatalf("quorum_descriptor object not found in canonical bytes: %s", s)
	}
	sub := s[start+len(marker):]
	end := indexOf(sub, "}")
	if end == -1 {
		t.Fatalf("quorum_descriptor object not closed: %s", s)
	}
	descriptor := sub[:end]

	orderedKeys := []string{
		"audit_rate_ppm",
		"group_size",
		"min_quorum",
		"min_trusted_corroborators",
		"pending_size",
		"target_copies",
		"trust_floor",
		"trusted_corroborators",
	}
	prev := -1
	for _, k := range orderedKeys {
		idx := indexOf(descriptor, `"`+k+`"`)
		if idx == -1 {
			t.Errorf("descriptor key %q missing from canonical bytes", k)
			continue
		}
		if idx <= prev {
			t.Errorf("descriptor key %q at %d is out of alphabetical order (prev %d)", k, idx, prev)
		}
		prev = idx
	}

	if n := reflect.TypeOf(QuorumDescriptor{}).NumField(); n != len(orderedKeys) {
		t.Errorf("QuorumDescriptor has %d fields, expected %d: update descriptorKV in signer.go and this golden when the field set changes", n, len(orderedKeys))
	}
}
