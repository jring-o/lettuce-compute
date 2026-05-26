package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
)

// ed25519TimestampSkew is the maximum allowed clock skew for Ed25519 signatures.
const ed25519TimestampSkew = 5 * time.Minute

// ed25519MaxBodySize limits the request body size read during Ed25519 auth.
// Browser volunteer payloads (results with base64 output) should be well under 10 MB.
const ed25519MaxBodySize = 10 * 1024 * 1024 // 10 MB

type ed25519PubKeyContextKey struct{}

// ContextWithEd25519PubKey stores the volunteer's Ed25519 public key in the context.
func ContextWithEd25519PubKey(ctx context.Context, pubKey ed25519.PublicKey) context.Context {
	return context.WithValue(ctx, ed25519PubKeyContextKey{}, pubKey)
}

// PublicKeyFromContext extracts the Ed25519 public key from the request context.
func PublicKeyFromContext(ctx context.Context) (ed25519.PublicKey, bool) {
	pk, ok := ctx.Value(ed25519PubKeyContextKey{}).(ed25519.PublicKey)
	return pk, ok
}

// ed25519ReplayCache is the package-level anti-replay cache shared by every
// Ed25519-protected REST route. It rejects a signature that has already been
// accepted within the clock-skew window (TTL = ed25519TimestampSkew). It reuses
// the same replayCache type as the C1 gRPC auth fix. checkAndAdd evicts expired
// entries on every insert, so the cache stays bounded by the skew window and no
// janitor goroutine is required (and ed25519AuthRequired has no lifecycle to hang
// one off, since it is a free per-route wrapper).
var ed25519ReplayCache = newReplayCache(ed25519TimestampSkew)

// ed25519AuthRequired wraps an http.HandlerFunc to require a valid Ed25519 signature
// in the Authorization header. Format: Ed25519 <base64url-pubkey>:<base64url-signature>:<unix-timestamp>
func ed25519AuthRequired(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pubKey, err := verifyEd25519Auth(r, ed25519ReplayCache)
		if err != nil {
			apierror.WriteError(w, apierror.Unauthorized(err.Error()))
			return
		}
		ctx := ContextWithEd25519PubKey(r.Context(), pubKey)
		next(w, r.WithContext(ctx))
	}
}

// timeNow is a package-level variable for testing.
var timeNow = time.Now

// verifyEd25519Auth parses and verifies the Ed25519 signature from the request.
// The check order is: parse → timestamp-skew → signature verify → anti-replay.
// The replay check runs only AFTER the signature is cryptographically verified so
// that unauthenticated input can never populate the cache.
func verifyEd25519Auth(r *http.Request, cache *replayCache) (ed25519.PublicKey, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, fmt.Errorf("missing Authorization header")
	}

	// Parse "Ed25519 <pubkey>:<signature>:<timestamp>"
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Ed25519") {
		return nil, fmt.Errorf("invalid Authorization header: expected Ed25519 scheme")
	}

	components := strings.SplitN(parts[1], ":", 3)
	if len(components) != 3 {
		return nil, fmt.Errorf("invalid Authorization header: expected <pubkey>:<signature>:<timestamp>")
	}

	pubKeyB64, sigB64, tsStr := components[0], components[1], components[2]

	// Decode public key (base64url, no padding).
	pubKeyBytes, err := base64.RawURLEncoding.DecodeString(pubKeyB64)
	if err != nil {
		return nil, fmt.Errorf("invalid public key encoding: %v", err)
	}
	if len(pubKeyBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid public key: must be %d bytes, got %d", ed25519.PublicKeySize, len(pubKeyBytes))
	}

	// Decode signature (base64url, no padding).
	sigBytes, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, fmt.Errorf("invalid signature encoding: %v", err)
	}
	if len(sigBytes) != ed25519.SignatureSize {
		return nil, fmt.Errorf("invalid signature: must be %d bytes, got %d", ed25519.SignatureSize, len(sigBytes))
	}

	// Parse and validate timestamp.
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp: %v", err)
	}

	now := timeNow()
	reqTime := time.Unix(ts, 0)
	skew := now.Sub(reqTime)
	if skew < -ed25519TimestampSkew || skew > ed25519TimestampSkew {
		return nil, fmt.Errorf("timestamp expired or too far in the future")
	}

	// Read and hash the body (with size limit to prevent memory exhaustion).
	var bodyHash string
	if r.Body != nil {
		limited := io.LimitReader(r.Body, ed25519MaxBodySize+1)
		bodyBytes, readErr := io.ReadAll(limited)
		if readErr != nil {
			return nil, fmt.Errorf("failed to read request body: %v", readErr)
		}
		r.Body.Close()
		if int64(len(bodyBytes)) > ed25519MaxBodySize {
			return nil, fmt.Errorf("request body too large (max %d bytes)", ed25519MaxBodySize)
		}
		// Replace body so downstream handlers can read it.
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		hash := sha256.Sum256(bodyBytes)
		bodyHash = hex.EncodeToString(hash[:])
	} else {
		hash := sha256.Sum256([]byte{})
		bodyHash = hex.EncodeToString(hash[:])
	}

	// Construct signed message: <timestamp>:<method>:<path>:<body-sha256>
	message := fmt.Sprintf("%s:%s:%s:%s", tsStr, r.Method, r.URL.Path, bodyHash)

	// Verify signature.
	pubKey := ed25519.PublicKey(pubKeyBytes)
	if !ed25519.Verify(pubKey, []byte(message), sigBytes) {
		return nil, fmt.Errorf("invalid signature")
	}

	// Anti-replay: reject a signature we have already accepted within the skew
	// window. Runs only after the signature is verified so unauthenticated input
	// cannot populate the cache. now is the same timestamp used for the skew check.
	if !cache.checkAndAdd(sigBytes, now) {
		return nil, fmt.Errorf("replayed request")
	}

	return pubKey, nil
}
