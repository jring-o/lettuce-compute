package atproto

import (
	"encoding/json"
	"fmt"
)

// CanonicalBytes returns the deterministic JSON bytes that the device key signs
// to prove it authorized this binding. The signed object contains exactly the
// createdAt, did, and operationalKey fields, plus label only when non-empty.
//
// The bytes are produced by json.Marshal of a map[string]string. Go marshals map
// keys in sorted order, so the field order is deterministic (createdAt, did,
// label, operationalKey) without any dependency on struct field order. The head
// verifies the signature by reconstructing these exact bytes from the published
// record, so this encoding is a wire contract and must not change.
func CanonicalBytes(did, operationalKey, label, createdAt string) ([]byte, error) {
	fields := map[string]string{
		"createdAt":      createdAt,
		"did":            did,
		"operationalKey": operationalKey,
	}
	if label != "" {
		fields["label"] = label
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("marshaling canonical signing bytes: %w", err)
	}
	return b, nil
}
