package attestation

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Signer creates Ed25519 signatures over attestation data.
type Signer struct {
	privateKey ed25519.PrivateKey
}

// NewSigner creates a new Signer with the given Ed25519 private key.
func NewSigner(privateKey ed25519.PrivateKey) *Signer {
	return &Signer{privateKey: privateKey}
}

// PublicKey returns the signing public key for verification.
func (s *Signer) PublicKey() ed25519.PublicKey {
	return s.privateKey.Public().(ed25519.PublicKey)
}

// Sign creates the canonical JSON representation of the attestation's signed fields and
// signs it with Ed25519. Returns the signature bytes.
func (s *Signer) Sign(att *Attestation) ([]byte, error) {
	canonical, err := CanonicalJSON(att)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(s.privateKey, canonical), nil
}

// VerifyAttestation verifies the Ed25519 signature on an attestation, rebuilding the
// canonical bytes under the rule the attestation's schema version (and outcome) selects.
func VerifyAttestation(publicKey ed25519.PublicKey, att *Attestation) bool {
	canonical, err := CanonicalJSON(att)
	if err != nil {
		return false
	}
	return ed25519.Verify(publicKey, canonical, att.Signature)
}

// CanonicalCreditString renders a credit amount in the exact fixed-scale decimal form
// ("1.000000") that v2 canonical forms sign and the repository stores. Six fractional
// digits match the credit_amount column's numeric(18,6) scale, so the stored value and the
// signed bytes agree by construction rather than by two independent rounding steps.
func CanonicalCreditString(amount float64) string {
	return strconv.FormatFloat(amount, 'f', 6, 64)
}

// timestampFormat is the fixed-microsecond form every canonical version signs. types.Now()
// truncates to microseconds and timestamptz stores microseconds, so the round-trip through
// the database reproduces these bytes exactly.
const timestampFormat = "2006-01-02T15:04:05.000000Z"

// NormalizeOutputChecksum lowercases a result's output checksum and reports whether it is
// admissible in a signed v2 payload: empty (no adjudicable checksum) or exactly 64 lowercase
// hex characters. Anything else — reachable only through a ref-only submission's CLAIMED
// checksum until fetch-and-verify lands — must not enter signed bytes; the caller attests ""
// and WARNs instead.
func NormalizeOutputChecksum(checksum string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(checksum))
	if !hexChecksumRe.MatchString(normalized) {
		return "", false
	}
	return normalized, true
}

var (
	creditCanonicalRe = regexp.MustCompile(`^-?[0-9]+\.[0-9]{6}$`)
	reasonRe          = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)
	hexChecksumRe     = regexp.MustCompile(`^(?:[0-9a-f]{64})?$`)
)

// CanonicalJSON produces the deterministic byte sequence that is signed/verified for an
// attestation, dispatching on the schema version and, within v2, on the outcome:
//
//   - schema_version <= 1 (including the zero value): the FROZEN v1 form — six fields, no
//     context. Pre-cutover rows must verify forever; this branch must never change.
//   - v2 + OutcomeRevoked: the revocation form under ContextRevocationV2.
//   - v2 otherwise: the grant/reject form under ContextGrantV2.
//
// Every v2 string value is charset-constrained (UUIDs, lower-hex, base64url-no-pad, the
// fixed timestamp format, the context literals, the reason code charset), and every v2
// number is an integer or a fixed-scale decimal STRING — so the manual marshal below and
// any third-party JSON serializer produce identical bytes: nothing in the payload can be
// escaped or float-formatted differently.
func CanonicalJSON(att *Attestation) ([]byte, error) {
	switch {
	case att.SchemaVersion <= SchemaVersionV1:
		return canonicalV1(att)
	case att.ValidationOutcome == OutcomeRevoked:
		return canonicalV2Revocation(att)
	default:
		return canonicalV2Grant(att)
	}
}

// canonicalV1 is the pre-cutover signed form: exactly six alphabetically-sorted fields.
// raw_metrics are DELIBERATELY EXCLUDED from the signed bytes: they are volunteer
// self-reported resource numbers the head never independently verifies, so signing them
// would let a consumer treat attacker-chosen values as head-certified fact (BG-06). FROZEN:
// rows signed under this form exist in the wild; any edit here breaks their verification.
func canonicalV1(att *Attestation) ([]byte, error) {
	canonical := []kv{
		{"attestation_timestamp", att.AttestationTimestamp.UTC().Format(timestampFormat)},
		{"credit_amount", att.CreditAmount},
		{"leaf_id", att.LeafID.String()},
		{"validation_outcome", att.ValidationOutcome},
		{"volunteer_public_key", base64.RawURLEncoding.EncodeToString(att.VolunteerPublicKey)},
		{"work_unit_id", att.WorkUnitID.String()},
	}

	return marshalSortedKV(canonical)
}

// canonicalV2Grant is the v2 signed form for grant (AGREED) and reject (DISAGREED)
// attestations. Volunteer metrics remain outside the signature (the v1/BG-06 posture).
func canonicalV2Grant(att *Attestation) ([]byte, error) {
	if att.ResultID == nil || att.OutputChecksum == nil ||
		att.QuorumDescriptor == nil || att.PolicyVersion == nil {
		return nil, fmt.Errorf("v2 grant attestation is missing signed fields (result_id/output_checksum/quorum_descriptor/policy_version)")
	}
	if !creditCanonicalRe.MatchString(att.CreditAmountCanonical) {
		return nil, fmt.Errorf("v2 attestation credit_amount canonical string %q is not a fixed 6-fractional-digit decimal", att.CreditAmountCanonical)
	}
	if !hexChecksumRe.MatchString(*att.OutputChecksum) {
		return nil, fmt.Errorf("v2 attestation output_checksum is neither empty nor 64 lowercase hex characters")
	}

	canonical := []kv{
		{"attestation_timestamp", att.AttestationTimestamp.UTC().Format(timestampFormat)},
		{"context", ContextGrantV2},
		{"credit_amount", att.CreditAmountCanonical},
		{"leaf_id", att.LeafID.String()},
		{"output_checksum", *att.OutputChecksum},
		{"policy_version", *att.PolicyVersion},
		{"quorum_descriptor", descriptorKV(att.QuorumDescriptor)},
		{"result_id", att.ResultID.String()},
		{"schema_version", att.SchemaVersion},
		{"validation_outcome", att.ValidationOutcome},
		{"volunteer_public_key", base64.RawURLEncoding.EncodeToString(att.VolunteerPublicKey)},
		{"work_unit_id", att.WorkUnitID.String()},
	}

	return marshalSortedKV(canonical)
}

// canonicalV2Revocation is the v2 signed form for revocation attestations. The distinct
// context string domain-separates "the head attests it granted" from "the head attests it
// revoked"; the outcome needs no field of its own — the context IS the statement type.
func canonicalV2Revocation(att *Attestation) ([]byte, error) {
	if att.ResultID == nil || att.RevokesAttestationID == nil ||
		att.AdjustmentID == nil || att.Reason == nil {
		return nil, fmt.Errorf("revocation attestation is missing signed fields (result_id/revokes_attestation_id/adjustment_id/reason)")
	}
	if !creditCanonicalRe.MatchString(att.CreditAmountCanonical) {
		return nil, fmt.Errorf("revocation credit_amount canonical string %q is not a fixed 6-fractional-digit decimal", att.CreditAmountCanonical)
	}
	if !reasonRe.MatchString(*att.Reason) {
		return nil, fmt.Errorf("revocation reason %q is not a machine code matching ^[A-Z0-9_]{1,64}$", *att.Reason)
	}

	canonical := []kv{
		{"adjustment_id", att.AdjustmentID.String()},
		{"attestation_timestamp", att.AttestationTimestamp.UTC().Format(timestampFormat)},
		{"context", ContextRevocationV2},
		{"credit_amount", att.CreditAmountCanonical},
		{"leaf_id", att.LeafID.String()},
		{"reason", *att.Reason},
		{"result_id", att.ResultID.String()},
		{"revokes_attestation_id", att.RevokesAttestationID.String()},
		{"schema_version", att.SchemaVersion},
		{"volunteer_public_key", base64.RawURLEncoding.EncodeToString(att.VolunteerPublicKey)},
		{"work_unit_id", att.WorkUnitID.String()},
	}

	return marshalSortedKV(canonical)
}

// descriptorKV lays the quorum descriptor out as an explicit alphabetically-sorted kv list
// for the recursive marshal. The listing is deliberately manual: canonical key order is a
// wire contract and must never silently track Go struct field order or reflection.
func descriptorKV(d *QuorumDescriptor) []kv {
	return []kv{
		{"audit_rate_ppm", d.AuditRatePPM},
		{"group_size", d.GroupSize},
		{"min_quorum", d.MinQuorum},
		{"min_trusted_corroborators", d.MinTrustedCorroborators},
		{"pending_size", d.PendingSize},
		{"target_copies", d.TargetCopies},
		{"trust_floor", d.TrustFloor},
		{"trusted_corroborators", d.TrustedCorroborators},
	}
}

// kv is a key-value pair for deterministic JSON marshaling.
type kv struct {
	Key   string
	Value any
}

// marshalSortedKV marshals a pre-sorted slice of key-value pairs as a JSON object. A value
// that is itself a []kv is marshaled recursively as a nested object, so nested structures
// share the same explicit-ordering guarantee as the top level.
func marshalSortedKV(pairs []kv) ([]byte, error) {
	buf := []byte{'{'}
	for i, pair := range pairs {
		if i > 0 {
			buf = append(buf, ',')
		}
		keyBytes, err := json.Marshal(pair.Key)
		if err != nil {
			return nil, err
		}
		buf = append(buf, keyBytes...)
		buf = append(buf, ':')
		if nested, ok := pair.Value.([]kv); ok {
			nestedBytes, err := marshalSortedKV(nested)
			if err != nil {
				return nil, err
			}
			buf = append(buf, nestedBytes...)
			continue
		}
		valBytes, err := json.Marshal(pair.Value)
		if err != nil {
			return nil, err
		}
		buf = append(buf, valBytes...)
	}
	buf = append(buf, '}')
	return buf, nil
}
