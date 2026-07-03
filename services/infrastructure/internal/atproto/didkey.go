package atproto

import (
	"crypto/ed25519"
	"fmt"
	"math/big"
	"strings"
)

// did:key encodes a public key as a multibase base58btc string of a multicodec
// prefix followed by the raw key bytes. For Ed25519 the multicodec is the varint
// 0xed 0x01 and the key is 32 raw bytes, so the encoding is:
//
//	"did:key:z" + base58btc(0xed, 0x01, <32 key bytes>)
//
// The leading "z" is the multibase tag for base58btc.
const (
	didKeyPrefix = "did:key:z"
)

// ed25519Multicodec is the unsigned-varint multicodec identifier for an
// Ed25519 public key (code 0xed).
var ed25519Multicodec = []byte{0xed, 0x01}

// EncodeEd25519DIDKey encodes an Ed25519 public key as its did:key form.
func EncodeEd25519DIDKey(pub ed25519.PublicKey) (string, error) {
	if len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("encode did:key: public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	buf := make([]byte, 0, len(ed25519Multicodec)+ed25519.PublicKeySize)
	buf = append(buf, ed25519Multicodec...)
	buf = append(buf, pub...)
	return didKeyPrefix + base58Encode(buf), nil
}

// DecodeEd25519DIDKey decodes a did:key string back into an Ed25519 public key.
// It is strict: the value must carry the did:key:z prefix, the Ed25519
// multicodec 0xed 0x01, and exactly 32 key bytes.
func DecodeEd25519DIDKey(s string) (ed25519.PublicKey, error) {
	if !strings.HasPrefix(s, didKeyPrefix) {
		return nil, fmt.Errorf("decode did:key: missing %q prefix", didKeyPrefix)
	}
	raw, err := base58Decode(s[len(didKeyPrefix):])
	if err != nil {
		return nil, fmt.Errorf("decode did:key: %w", err)
	}
	want := len(ed25519Multicodec) + ed25519.PublicKeySize
	if len(raw) != want {
		return nil, fmt.Errorf("decode did:key: got %d bytes, want %d", len(raw), want)
	}
	if raw[0] != ed25519Multicodec[0] || raw[1] != ed25519Multicodec[1] {
		return nil, fmt.Errorf("decode did:key: multicodec is 0x%02x 0x%02x, want 0xed 0x01", raw[0], raw[1])
	}
	pub := make(ed25519.PublicKey, ed25519.PublicKeySize)
	copy(pub, raw[len(ed25519Multicodec):])
	return pub, nil
}

// base58Alphabet is the Bitcoin base58 alphabet used by base58btc multibase.
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Encode encodes bytes using base58btc. Leading zero bytes are preserved
// as leading '1' characters, matching the standard scheme.
func base58Encode(input []byte) string {
	zeros := 0
	for zeros < len(input) && input[zeros] == 0 {
		zeros++
	}

	num := new(big.Int).SetBytes(input)
	radix := big.NewInt(58)
	mod := new(big.Int)

	var out []byte
	for num.Sign() > 0 {
		num.DivMod(num, radix, mod)
		out = append(out, base58Alphabet[mod.Int64()])
	}
	for i := 0; i < zeros; i++ {
		out = append(out, base58Alphabet[0])
	}

	// The digits were produced least-significant first; reverse them.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// base58Decode decodes a base58btc string. Leading '1' characters are restored
// as leading zero bytes. It rejects any character outside the alphabet.
func base58Decode(s string) ([]byte, error) {
	result := new(big.Int)
	radix := big.NewInt(58)

	for i := 0; i < len(s); i++ {
		idx := strings.IndexByte(base58Alphabet, s[i])
		if idx < 0 {
			return nil, fmt.Errorf("invalid base58 character %q at index %d", s[i], i)
		}
		result.Mul(result, radix)
		result.Add(result, big.NewInt(int64(idx)))
	}

	decoded := result.Bytes()

	zeros := 0
	for zeros < len(s) && s[zeros] == base58Alphabet[0] {
		zeros++
	}

	out := make([]byte, zeros+len(decoded))
	copy(out[zeros:], decoded)
	return out, nil
}
