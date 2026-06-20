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
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
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

// ed25519ReplayStore is the package-level anti-replay store shared by every
// Ed25519-protected REST route (browser / WASM volunteer path). It rejects a
// signature already accepted within the clock-skew window (TTL =
// ed25519TimestampSkew). Layer 3 makes this a swappable replayStore: by default it
// is an in-process in-mem store (single-replica behavior, all existing tests
// green); at boot SetEd25519ReplayStore replaces it with the SHARED store (Redis,
// or one in-mem store shared with the gRPC path) so a signature accepted by one
// replica is rejected by another. The dedup key is the SIGNATURE ALONE, GLOBAL.
var ed25519ReplayStore replayStore = newInMemReplayStore(ed25519TimestampSkew)

// SetEd25519ReplayStore swaps the REST-path replay store. Call once at startup,
// before serving, to install the shared cross-replica store. A nil store is
// ignored (keeps the default in-mem store) so callers can pass an unconditionally-
// constructed value.
func SetEd25519ReplayStore(store replayStore) {
	if store != nil {
		ed25519ReplayStore = store
	}
}

// ed25519ReplayDetectionEnabled gates the REST anti-replay check, mirroring
// grpcReplayDetectionEnabled. It is true in production; the integration-only test
// seam flips it to false so e2e tests can replay byte-identical signed requests,
// and the cross-replica replay test toggles it true for its duration.
var ed25519ReplayDetectionEnabled = true

// ed25519AuthRequired wraps an http.HandlerFunc to require a valid Ed25519 signature
// in the Authorization header. Format: Ed25519 <base64url-pubkey>:<base64url-signature>:<unix-timestamp>
func ed25519AuthRequired(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pubKey, err := verifyEd25519Auth(r, ed25519ReplayStore)
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
// that unauthenticated input can never populate the store.
func verifyEd25519Auth(r *http.Request, replay replayStore) (ed25519.PublicKey, error) {
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

	// Anti-replay (Layer 3: SHARED across replicas). Reject a signature already
	// accepted within the skew window. The dedup key is the SIGNATURE ALONE, GLOBAL,
	// so a request accepted by replica A is rejected by replica B. Runs only after
	// the signature is verified so unauthenticated input cannot populate the store.
	if ed25519ReplayDetectionEnabled {
		alreadySeen, serr := replay.SeenWithin(r.Context(), sigBytes, ed25519TimestampSkew)
		if serr != nil {
			// Store failure (e.g. Redis outage): apply the configured policy.
			// Fail-open admits with a loud alarm (default); fail-closed rejects.
			if !replayFailsOpen {
				return nil, fmt.Errorf("replay store unavailable")
			}
			logging.LoggerFromContext(r.Context(), slog.Default()).Error(
				"replay store error; admitting request (fail-open)",
				"path", r.URL.Path, "error", serr)
		} else if alreadySeen {
			// H-4: a genuine replay must be visible (only the fail-open store error
			// logged before). Short pubkey prefix (8 hex chars) — never the full key.
			logging.LoggerFromContext(r.Context(), slog.Default()).Warn("rejected replayed signature",
				"path", r.URL.Path, "pubkey_prefix", hex.EncodeToString(pubKeyBytes[:4]))
			return nil, fmt.Errorf("replayed request")
		}
	}

	return pubKey, nil
}
