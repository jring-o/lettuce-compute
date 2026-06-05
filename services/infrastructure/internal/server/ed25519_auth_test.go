package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signRequest creates a valid Ed25519 Authorization header for testing.
func signRequest(t *testing.T, privKey ed25519.PrivateKey, pubKey ed25519.PublicKey, method, path, body string, ts int64) string {
	t.Helper()
	bodyHash := sha256.Sum256([]byte(body))
	message := fmt.Sprintf("%d:%s:%s:%s", ts, method, path, hex.EncodeToString(bodyHash[:]))
	sig := ed25519.Sign(privKey, []byte(message))
	pubB64 := base64.RawURLEncoding.EncodeToString(pubKey)
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	return fmt.Sprintf("Ed25519 %s:%s:%d", pubB64, sigB64, ts)
}

func TestEd25519Auth_ValidSignature(t *testing.T) {
	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)
	ts := time.Now().Unix()
	body := `{"test":"data"}`

	handler := ed25519AuthRequired(func(w http.ResponseWriter, r *http.Request) {
		pk, ok := PublicKeyFromContext(r.Context())
		if !ok {
			t.Error("expected public key in context")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !pk.Equal(pubKey) {
			t.Error("public key mismatch")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/request-work", strings.NewReader(body))
	req.Header.Set("Authorization", signRequest(t, privKey, pubKey, "POST", "/api/v1/volunteers/request-work", body, ts))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestEd25519Auth_MissingHeader(t *testing.T) {
	handler := ed25519AuthRequired(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestEd25519Auth_MalformedHeader(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{"wrong scheme", "Bearer sometoken"},
		{"missing components", "Ed25519 onlyonething"},
		{"two components", "Ed25519 one:two"},
	}

	handler := ed25519AuthRequired(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/test", nil)
			req.Header.Set("Authorization", tt.header)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("expected 401, got %d", rec.Code)
			}
		})
	}
}

func TestEd25519Auth_ExpiredTimestamp(t *testing.T) {
	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)
	// 10 minutes ago — outside the 5-minute window.
	ts := time.Now().Add(-10 * time.Minute).Unix()
	body := ""

	handler := ed25519AuthRequired(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	req.Header.Set("Authorization", signRequest(t, privKey, pubKey, "POST", "/test", body, ts))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for expired timestamp, got %d", rec.Code)
	}
}

func TestEd25519Auth_FutureTimestamp(t *testing.T) {
	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)
	// 10 minutes from now — outside the 5-minute window.
	ts := time.Now().Add(10 * time.Minute).Unix()
	body := ""

	handler := ed25519AuthRequired(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	req.Header.Set("Authorization", signRequest(t, privKey, pubKey, "POST", "/test", body, ts))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for future timestamp, got %d", rec.Code)
	}
}

func TestEd25519Auth_WrongSignature(t *testing.T) {
	pubKey, _, _ := ed25519.GenerateKey(rand.Reader)
	_, wrongPrivKey, _ := ed25519.GenerateKey(rand.Reader) // different key
	ts := time.Now().Unix()
	body := `{"hello":"world"}`

	handler := ed25519AuthRequired(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	// Sign with wrong private key but present correct public key.
	req.Header.Set("Authorization", signRequest(t, wrongPrivKey, pubKey, "POST", "/test", body, ts))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong signature, got %d", rec.Code)
	}
}

func TestEd25519Auth_SignatureOverDifferentBody(t *testing.T) {
	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)
	ts := time.Now().Unix()

	handler := ed25519AuthRequired(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Sign over "original body" but send "different body".
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("different body"))
	req.Header.Set("Authorization", signRequest(t, privKey, pubKey, "POST", "/test", "original body", ts))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for body mismatch, got %d", rec.Code)
	}
}

func TestEd25519Auth_EmptyBody(t *testing.T) {
	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)
	ts := time.Now().Unix()

	handler := ed25519AuthRequired(func(w http.ResponseWriter, r *http.Request) {
		_, ok := PublicKeyFromContext(r.Context())
		if !ok {
			t.Error("expected public key in context")
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(""))
	req.Header.Set("Authorization", signRequest(t, privKey, pubKey, "POST", "/test", "", ts))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestEd25519Auth_InvalidPublicKeySize(t *testing.T) {
	handler := ed25519AuthRequired(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Use a 16-byte key instead of 32 bytes.
	shortKey := make([]byte, 16)
	shortKeyB64 := base64.RawURLEncoding.EncodeToString(shortKey)
	sigB64 := base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	auth := fmt.Sprintf("Ed25519 %s:%s:%d", shortKeyB64, sigB64, time.Now().Unix())

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(""))
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong public key size, got %d", rec.Code)
	}
}

func TestEd25519Auth_InvalidSignatureSize(t *testing.T) {
	handler := ed25519AuthRequired(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	pubKey, _, _ := ed25519.GenerateKey(rand.Reader)
	pubKeyB64 := base64.RawURLEncoding.EncodeToString(pubKey)
	// Use a 32-byte signature instead of 64 bytes.
	shortSig := make([]byte, 32)
	sigB64 := base64.RawURLEncoding.EncodeToString(shortSig)
	auth := fmt.Sprintf("Ed25519 %s:%s:%d", pubKeyB64, sigB64, time.Now().Unix())

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(""))
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong signature size, got %d", rec.Code)
	}
}

func TestEd25519Auth_InvalidTimestampFormat(t *testing.T) {
	handler := ed25519AuthRequired(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	pubKey, _, _ := ed25519.GenerateKey(rand.Reader)
	pubKeyB64 := base64.RawURLEncoding.EncodeToString(pubKey)
	sigB64 := base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	auth := fmt.Sprintf("Ed25519 %s:%s:not-a-number", pubKeyB64, sigB64)

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(""))
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid timestamp, got %d", rec.Code)
	}
}

func TestEd25519Auth_InvalidBase64PublicKey(t *testing.T) {
	handler := ed25519AuthRequired(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	sigB64 := base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	auth := fmt.Sprintf("Ed25519 !!!invalid-base64!!!:%s:%d", sigB64, time.Now().Unix())

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(""))
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid base64 pubkey, got %d", rec.Code)
	}
}

func TestEd25519Auth_ReplayedRequestRejected(t *testing.T) {
	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)
	ts := time.Now().Unix()
	body := `{"test":"data"}`
	path := "/api/v1/volunteers/request-work"

	handler := ed25519AuthRequired(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	auth := signRequest(t, privKey, pubKey, "POST", path, body, ts)

	// First request with this signature succeeds.
	req1 := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req1.Header.Set("Authorization", auth)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", rec1.Code)
	}

	// Identical replay (same signature) is rejected.
	req2 := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req2.Header.Set("Authorization", auth)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("replayed request: expected 401, got %d", rec2.Code)
	}

	// A fresh signature (new timestamp -> new signature) still passes.
	freshTs := ts + 1
	freshAuth := signRequest(t, privKey, pubKey, "POST", path, body, freshTs)
	req3 := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req3.Header.Set("Authorization", freshAuth)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("fresh signature: expected 200, got %d", rec3.Code)
	}
}

func TestEd25519Auth_ExpiredTimestampNotCached(t *testing.T) {
	// An expired timestamp must be rejected by the skew check BEFORE the replay
	// cache is touched, so it never depends on (nor populates) the cache.
	store := newInMemReplayStore(ed25519TimestampSkew)
	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)
	ts := time.Now().Add(-10 * time.Minute).Unix()
	body := ""
	path := "/test"

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", signRequest(t, privKey, pubKey, "POST", path, body, ts))

	if _, err := verifyEd25519Auth(req, store); err == nil {
		t.Fatal("expected error for expired timestamp")
	}
	// The expired signature must not have been recorded in the store.
	if len(store.cache.seen) != 0 {
		t.Fatalf("expired request should not populate the replay store, size=%d", len(store.cache.seen))
	}
}

func TestEd25519Auth_ContextHelpers(t *testing.T) {
	// Test PublicKeyFromContext returns false for empty context.
	_, ok := PublicKeyFromContext(context.Background())
	if ok {
		t.Error("expected false for empty context")
	}

	// Test ContextWithEd25519PubKey and PublicKeyFromContext round-trip.
	pubKey, _, _ := ed25519.GenerateKey(rand.Reader)
	ctx := ContextWithEd25519PubKey(context.Background(), pubKey)
	extracted, ok := PublicKeyFromContext(ctx)
	if !ok {
		t.Error("expected true for context with pubkey")
	}
	if !extracted.Equal(pubKey) {
		t.Error("expected extracted pubkey to match")
	}
}
