package atproto

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// verifyHeadAuth reimplements the head's Ed25519 REST verification
// (services/infrastructure/internal/server/ed25519_auth.go) so the test proves
// the client's signature is accepted by the exact math the head runs. It returns
// the verified public key or an error.
func verifyHeadAuth(r *http.Request, body []byte) (ed25519.PublicKey, error) {
	authHeader := r.Header.Get("Authorization")
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Ed25519") {
		return nil, fmt.Errorf("expected Ed25519 scheme, got %q", authHeader)
	}
	components := strings.SplitN(parts[1], ":", 3)
	if len(components) != 3 {
		return nil, fmt.Errorf("expected <pubkey>:<signature>:<timestamp>")
	}
	pubB64, sigB64, tsStr := components[0], components[1], components[2]

	pubBytes, err := base64.RawURLEncoding.DecodeString(pubB64)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bad public key encoding")
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return nil, fmt.Errorf("bad signature encoding")
	}

	sum := sha256.Sum256(body)
	message := fmt.Sprintf("%s:%s:%s:%s", tsStr, r.Method, r.URL.Path, hex.EncodeToString(sum[:]))
	pub := ed25519.PublicKey(pubBytes)
	if !ed25519.Verify(pub, []byte(message), sigBytes) {
		return nil, fmt.Errorf("signature does not verify")
	}
	return pub, nil
}

func TestNotifyHeadBindDIDSignatureVerifies(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	var gotBody struct {
		DID       string `json:"did"`
		RecordURI string `json:"record_uri"`
	}

	head := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/identity/bind-did" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)

		verifiedPub, err := verifyHeadAuth(r, body)
		if err != nil {
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}
		if !verifiedPub.Equal(pub) {
			http.Error(w, "wrong key", http.StatusUnauthorized)
			return
		}
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer head.Close()

	err = NotifyHeadBindDID(context.Background(), head.Client(), head.URL,
		"did:plc:abc123", "at://did:plc:abc123/coll/rkey1", pub, priv, time.Now())
	if err != nil {
		t.Fatalf("NotifyHeadBindDID: %v", err)
	}
	if gotBody.DID != "did:plc:abc123" {
		t.Errorf("head received did = %q, want did:plc:abc123", gotBody.DID)
	}
	if gotBody.RecordURI != "at://did:plc:abc123/coll/rkey1" {
		t.Errorf("head received record_uri = %q", gotBody.RecordURI)
	}
}

func TestNotifyHeadBindDIDPropagatesNon2xx(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	head := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"record not found in repo"}`, http.StatusBadRequest)
	}))
	defer head.Close()

	err := NotifyHeadBindDID(context.Background(), head.Client(), head.URL,
		"did:plc:abc123", "at://uri", pub, priv, time.Now())
	if err == nil {
		t.Fatal("expected error on non-2xx head response")
	}
	if !strings.Contains(err.Error(), "record not found in repo") {
		t.Errorf("error should include head response body, got: %v", err)
	}
}

func TestSignEd25519RequestFieldOrderAndEncoding(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body := []byte(`{"did":"did:plc:abc123","record_uri":"at://uri"}`)
	req, err := http.NewRequest(http.MethodPost, "https://head.example.com/api/v1/identity/bind-did", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	SignEd25519Request(req, body, pub, priv, time.Unix(1751544000, 0))

	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Ed25519 ") {
		t.Fatalf("missing Ed25519 scheme prefix: %q", auth)
	}
	components := strings.SplitN(strings.TrimPrefix(auth, "Ed25519 "), ":", 3)
	if len(components) != 3 {
		t.Fatalf("expected 3 colon-separated fields, got %d: %q", len(components), auth)
	}
	// Field order must be <pubkey>:<signature>:<timestamp>.
	if got, want := components[0], base64.RawURLEncoding.EncodeToString(pub); got != want {
		t.Errorf("field 0 (pubkey) = %q, want %q", got, want)
	}
	if components[2] != "1751544000" {
		t.Errorf("field 2 (timestamp) = %q, want 1751544000", components[2])
	}
	// The verification math must accept it.
	if _, err := verifyHeadAuth(req, body); err != nil {
		t.Errorf("self-signed request failed verification: %v", err)
	}
}
