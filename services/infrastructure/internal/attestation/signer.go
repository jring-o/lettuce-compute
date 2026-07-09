package attestation

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
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

// Sign creates a canonical JSON representation of the attestation fields and
// signs it with Ed25519. Returns the signature bytes.
func (s *Signer) Sign(att *Attestation) ([]byte, error) {
	canonical, err := CanonicalJSON(att)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(s.privateKey, canonical), nil
}

// VerifyAttestation verifies the Ed25519 signature on an attestation.
func VerifyAttestation(publicKey ed25519.PublicKey, att *Attestation) bool {
	canonical, err := CanonicalJSON(att)
	if err != nil {
		return false
	}
	return ed25519.Verify(publicKey, canonical, att.Signature)
}

// CanonicalJSON produces a deterministic JSON representation of the signed
// attestation fields. Keys are sorted alphabetically. This is the exact byte
// sequence that is signed/verified.
//
// raw_metrics are DELIBERATELY EXCLUDED from the signed bytes: they are volunteer
// self-reported resource numbers the head never independently verifies, so signing them
// would let a consumer treat attacker-chosen values as head-certified fact. The signature
// therefore covers only head-derived facts (outcome, credit, identities, timestamp); the
// metrics remain stored and served (see the Attestation struct / list handler) as UNSIGNED
// provenance.
func CanonicalJSON(att *Attestation) ([]byte, error) {
	// Build the canonical map with sorted keys (Go map iteration order is random,
	// so we use a slice of key-value pairs marshaled manually).
	canonical := []kv{
		{"attestation_timestamp", att.AttestationTimestamp.UTC().Format("2006-01-02T15:04:05.000000Z")},
		{"credit_amount", att.CreditAmount},
		{"leaf_id", att.LeafID.String()},
		{"validation_outcome", att.ValidationOutcome},
		{"volunteer_public_key", base64.RawURLEncoding.EncodeToString(att.VolunteerPublicKey)},
		{"work_unit_id", att.WorkUnitID.String()},
	}

	return marshalSortedKV(canonical)
}

// kv is a key-value pair for deterministic JSON marshaling.
type kv struct {
	Key   string
	Value any
}

// marshalSortedKV marshals a pre-sorted slice of key-value pairs as a JSON object.
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
		valBytes, err := json.Marshal(pair.Value)
		if err != nil {
			return nil, err
		}
		buf = append(buf, valBytes...)
	}
	buf = append(buf, '}')
	return buf, nil
}

