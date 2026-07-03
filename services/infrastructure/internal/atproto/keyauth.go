package atproto

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Distinct verification failures so callers can react to each cause — a wrong
// DID, a wrong key, an expired binding, or a bad signature — with errors.Is.
var (
	// ErrDIDMismatch means the record's did does not match the expected DID.
	ErrDIDMismatch = errors.New("atproto: key authorization DID mismatch")
	// ErrKeyMismatch means the record's operationalKey does not decode to the
	// expected device key.
	ErrKeyMismatch = errors.New("atproto: key authorization operational key mismatch")
	// ErrExpired means the record's expiresAt is at or before the reference time.
	ErrExpired = errors.New("atproto: key authorization expired")
	// ErrInvalidExpiresAt means expiresAt is present but not valid RFC3339.
	ErrInvalidExpiresAt = errors.New("atproto: key authorization has invalid expiresAt")
	// ErrBadSignature means the Ed25519 signature over the canonical bytes did
	// not verify against the expected key.
	ErrBadSignature = errors.New("atproto: key authorization signature invalid")
)

// Bytes is a byte string carried in ATProto lexicon JSON, where the canonical
// encoding wraps standard base64 in a {"$bytes":"…"} envelope. Its custom
// (un)marshaling produces and accepts that envelope; unmarshaling also tolerates
// a bare base64 string for robustness.
type Bytes []byte

// MarshalJSON encodes the bytes as the {"$bytes":"<base64-std>"} envelope.
func (b Bytes) MarshalJSON() ([]byte, error) {
	env := struct {
		Bytes string `json:"$bytes"`
	}{Bytes: base64.StdEncoding.EncodeToString(b)}
	return json.Marshal(env)
}

// UnmarshalJSON accepts the {"$bytes":"…"} envelope, a bare base64 string, or
// JSON null (decoded to nil).
func (b *Bytes) UnmarshalJSON(data []byte) error {
	if string(bytes.TrimSpace(data)) == "null" {
		*b = nil
		return nil
	}

	var env struct {
		Bytes *string `json:"$bytes"`
	}
	if err := json.Unmarshal(data, &env); err == nil && env.Bytes != nil {
		decoded, err := base64.StdEncoding.DecodeString(*env.Bytes)
		if err != nil {
			return fmt.Errorf("decode $bytes base64: %w", err)
		}
		*b = decoded
		return nil
	}

	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		decoded, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return fmt.Errorf("decode bytes base64 string: %w", err)
		}
		*b = decoded
		return nil
	}

	return errors.New(`bytes: expected {"$bytes":"<base64>"} envelope or base64 string`)
}

// KeyAuthorizationRecord is the binding a volunteer publishes in their PDS: it
// names the volunteer's DID and their device (operational) key, and carries the
// device key's signature over the canonical binding fields. Unknown JSON fields,
// including the lexicon "$type", are tolerated and ignored.
type KeyAuthorizationRecord struct {
	DID            string `json:"did"`
	OperationalKey string `json:"operationalKey"`
	KeySignature   Bytes  `json:"keySignature"`
	Label          string `json:"label,omitempty"`
	CreatedAt      string `json:"createdAt"`
	ExpiresAt      string `json:"expiresAt,omitempty"`
}

// CanonicalKeyAuthorizationBytes returns the exact bytes the device key signs
// and the head verifies. It is the JSON object of the binding fields with keys
// createdAt, did, operationalKey, and label — label included only when non-empty.
// Determinism comes from encoding/json sorting map keys, so the byte sequence is
// stable across encoders that share that behavior.
func CanonicalKeyAuthorizationBytes(did, operationalKey, label, createdAt string) []byte {
	m := map[string]string{
		"createdAt":      createdAt,
		"did":            did,
		"operationalKey": operationalKey,
	}
	if label != "" {
		m["label"] = label
	}
	out, _ := json.Marshal(m)
	return out
}

// VerifyKeyAuthorization checks a record against the DID and device key the head
// expects, at reference time now. It verifies, in order: the record's DID, that
// the operationalKey decodes to expectedKey byte-for-byte, that any expiresAt is
// still in the future, and that the signature over the canonical bytes verifies.
// Each failure returns a distinct wrapped error (matchable with errors.Is).
func VerifyKeyAuthorization(rec *KeyAuthorizationRecord, expectedDID string, expectedKey ed25519.PublicKey, now time.Time) error {
	if rec.DID != expectedDID {
		return fmt.Errorf("%w: record did %q != expected %q", ErrDIDMismatch, rec.DID, expectedDID)
	}

	opKey, err := DecodeEd25519DIDKey(rec.OperationalKey)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrKeyMismatch, err)
	}
	if !bytes.Equal(opKey, expectedKey) {
		return fmt.Errorf("%w: operationalKey does not match expected device key", ErrKeyMismatch)
	}

	if rec.ExpiresAt != "" {
		exp, err := time.Parse(time.RFC3339, rec.ExpiresAt)
		if err != nil {
			return fmt.Errorf("%w: %q: %v", ErrInvalidExpiresAt, rec.ExpiresAt, err)
		}
		if !exp.After(now) {
			return fmt.Errorf("%w: expired at %s", ErrExpired, rec.ExpiresAt)
		}
	}

	canonical := CanonicalKeyAuthorizationBytes(rec.DID, rec.OperationalKey, rec.Label, rec.CreatedAt)
	if !ed25519.Verify(expectedKey, canonical, rec.KeySignature) {
		return fmt.Errorf("%w", ErrBadSignature)
	}
	return nil
}

// ParseATURI parses a strict AT-URI of the form at://<did>/<collection>/<rkey>.
// It rejects any extra path segments and requires the authority to be a DID.
func ParseATURI(uri string) (did, collection, rkey string, err error) {
	const scheme = "at://"
	if !strings.HasPrefix(uri, scheme) {
		return "", "", "", fmt.Errorf("at-uri %q: missing at:// scheme", uri)
	}

	parts := strings.Split(uri[len(scheme):], "/")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("at-uri %q: want at://<did>/<collection>/<rkey>", uri)
	}
	did, collection, rkey = parts[0], parts[1], parts[2]

	if !strings.HasPrefix(did, "did:") {
		return "", "", "", fmt.Errorf("at-uri %q: authority %q is not a DID", uri, did)
	}
	if collection == "" || rkey == "" {
		return "", "", "", fmt.Errorf("at-uri %q: empty collection or rkey", uri)
	}
	return did, collection, rkey, nil
}
