// Package atproto contains the client-side helpers the `bind-did` command uses to
// publish a device-key authorization record into a volunteer's own ATProto
// Personal Data Server (PDS) repository and to notify Lettuce heads about it.
//
// The helpers are split across files by concern: did:key encoding (this file),
// the canonical signing bytes (canonical.go), the XRPC PDS client (client.go),
// and the head bind-did notification (head.go).
package atproto

import (
	"crypto/ed25519"
	"fmt"
	"strings"
)

// base58Alphabet is the Bitcoin base58 alphabet used by base58btc multibase.
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// ed25519MulticodecPrefix is the unsigned-varint encoding of the multicodec code
// 0xed (Ed25519 public key): 0xed is >= 0x80, so it encodes as the two bytes
// {0xed, 0x01}. A did:key for an Ed25519 key is this prefix followed by the raw
// 32-byte public key, base58btc-encoded with the "z" multibase indicator.
var ed25519MulticodecPrefix = []byte{0xed, 0x01}

// didKeyPrefix is the constant scheme + base58btc multibase indicator that
// precedes every Ed25519 did:key.
const didKeyPrefix = "did:key:z"

// EncodeEd25519DIDKey returns the did:key identifier for an Ed25519 public key:
// "did:key:z" followed by the base58btc encoding of the multicodec prefix
// {0xed, 0x01} concatenated with the 32 raw public-key bytes.
func EncodeEd25519DIDKey(pub ed25519.PublicKey) string {
	payload := make([]byte, 0, len(ed25519MulticodecPrefix)+len(pub))
	payload = append(payload, ed25519MulticodecPrefix...)
	payload = append(payload, pub...)
	return didKeyPrefix + base58Encode(payload)
}

// DecodeEd25519DIDKey parses an Ed25519 did:key and returns its raw public key.
// It rejects any identifier that is not an Ed25519 did:key (wrong scheme,
// multibase, multicodec prefix, or key length).
func DecodeEd25519DIDKey(s string) (ed25519.PublicKey, error) {
	if !strings.HasPrefix(s, didKeyPrefix) {
		return nil, fmt.Errorf("not an Ed25519 did:key: missing %q prefix", didKeyPrefix)
	}
	decoded, err := base58Decode(strings.TrimPrefix(s, didKeyPrefix))
	if err != nil {
		return nil, fmt.Errorf("decoding did:key base58: %w", err)
	}
	if len(decoded) < len(ed25519MulticodecPrefix) ||
		decoded[0] != ed25519MulticodecPrefix[0] || decoded[1] != ed25519MulticodecPrefix[1] {
		return nil, fmt.Errorf("did:key is not an Ed25519 key (wrong multicodec prefix)")
	}
	keyBytes := decoded[len(ed25519MulticodecPrefix):]
	if len(keyBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid Ed25519 public key length: got %d, want %d", len(keyBytes), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(keyBytes), nil
}

// base58Encode encodes input using the base58btc (Bitcoin) alphabet. Each
// leading zero byte is emitted as a leading "1", matching the base58 spec.
func base58Encode(input []byte) string {
	zeros := 0
	for zeros < len(input) && input[zeros] == 0 {
		zeros++
	}

	// Repeatedly divide the remaining big-endian number by 58, collecting each
	// remainder as a base58 digit (least significant first).
	buf := make([]byte, len(input))
	copy(buf, input)
	var digits []byte
	start := zeros
	for start < len(buf) {
		remainder := 0
		for i := start; i < len(buf); i++ {
			acc := int(buf[i]) + remainder*256
			buf[i] = byte(acc / 58)
			remainder = acc % 58
		}
		digits = append(digits, base58Alphabet[remainder])
		for start < len(buf) && buf[start] == 0 {
			start++
		}
	}

	for i := 0; i < zeros; i++ {
		digits = append(digits, base58Alphabet[0])
	}

	// digits holds the value least-significant-first with the leading-zero "1"s
	// appended last; reverse it so the most significant digit leads.
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}

// base58Decode decodes a base58btc (Bitcoin alphabet) string to its raw bytes.
func base58Decode(s string) ([]byte, error) {
	var num []byte // big-endian base-256 accumulator
	for _, r := range s {
		idx := strings.IndexRune(base58Alphabet, r)
		if idx < 0 {
			return nil, fmt.Errorf("invalid base58 character %q", string(r))
		}
		carry := idx
		for i := len(num) - 1; i >= 0; i-- {
			carry += int(num[i]) * 58
			num[i] = byte(carry)
			carry >>= 8
		}
		for carry > 0 {
			num = append([]byte{byte(carry)}, num...)
			carry >>= 8
		}
	}

	// Every leading "1" is a leading zero byte.
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	return append(make([]byte, zeros), num...), nil
}
