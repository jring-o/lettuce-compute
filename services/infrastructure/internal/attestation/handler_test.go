package attestation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// --- Mock Repository ---

type mockAttestationRepo struct {
	attestations []*Attestation
}

func newMockAttestationRepo() *mockAttestationRepo {
	return &mockAttestationRepo{}
}

func (m *mockAttestationRepo) add(a *Attestation) {
	a.ID = types.NewID()
	a.CreatedAt = time.Now().UTC()
	m.attestations = append(m.attestations, a)
}

func (m *mockAttestationRepo) Create(_ context.Context, att *Attestation) error {
	att.ID = types.NewID()
	att.CreatedAt = time.Now().UTC()
	m.attestations = append(m.attestations, att)
	return nil
}

func (m *mockAttestationRepo) List(_ context.Context, filters ListFilters, page types.PaginationRequest) ([]*Attestation, types.PaginationResponse, error) {
	var filtered []*Attestation
	for _, a := range m.attestations {
		if filters.LeafID != nil && a.LeafID != *filters.LeafID {
			continue
		}
		if len(filters.VolunteerPublicKey) > 0 && string(a.VolunteerPublicKey) != string(filters.VolunteerPublicKey) {
			continue
		}
		if filters.From != nil {
			fromTime, _ := types.ParseTimestamp(*filters.From)
			if a.AttestationTimestamp.Before(fromTime) {
				continue
			}
		}
		if filters.To != nil {
			toTime, _ := types.ParseTimestamp(*filters.To)
			if a.AttestationTimestamp.After(toTime) {
				continue
			}
		}
		filtered = append(filtered, a)
	}

	pageSize := page.ClampPageSize()
	pagination := types.PaginationResponse{}
	if len(filtered) > pageSize {
		filtered = filtered[:pageSize]
		pagination.HasMore = true
		last := filtered[pageSize-1]
		pagination.NextCursor = types.EncodeCursor(last.AttestationTimestamp, last.ID)
	}

	return filtered, pagination, nil
}

// --- Test Helpers ---

func testHandlerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func makeTestAttestation(leafID types.ID, pubKey []byte, ts time.Time) *Attestation {
	return &Attestation{
		LeafID:          leafID,
		VolunteerPublicKey: pubKey,
		WorkUnitID:         types.NewID(),
		RawMetrics: map[string]any{
			"wall_clock_seconds": float64(100),
			"cpu_seconds_user":   float64(90),
		},
		ValidationOutcome:   OutcomeAgreed,
		CreditAmount:        1.0,
		AttestationTimestamp: ts,
		Signature:           []byte("test-signature"),
	}
}

// --- Tests ---

func TestHandlerListNoFilters(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	repo := newMockAttestationRepo()
	leafID := types.NewID()
	pubKey := make([]byte, 32)
	repo.add(makeTestAttestation(leafID, pubKey, time.Now().UTC()))
	repo.add(makeTestAttestation(leafID, pubKey, time.Now().UTC().Add(-time.Hour)))

	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/attestations", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp attestationListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Data) != 2 {
		t.Errorf("data length = %d, want 2", len(resp.Data))
	}
	if resp.SigningPublicKey == "" {
		t.Error("signing_public_key should be present")
	}
	// Verify it decodes to the correct public key.
	decoded, err := base64.RawURLEncoding.DecodeString(resp.SigningPublicKey)
	if err != nil {
		t.Fatalf("decode signing_public_key: %v", err)
	}
	if string(decoded) != string(signer.PublicKey()) {
		t.Error("signing_public_key does not match")
	}
}

func TestHandlerFilterByLeafID(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	repo := newMockAttestationRepo()
	proj1 := types.NewID()
	proj2 := types.NewID()
	pubKey := make([]byte, 32)
	repo.add(makeTestAttestation(proj1, pubKey, time.Now().UTC()))
	repo.add(makeTestAttestation(proj2, pubKey, time.Now().UTC()))
	repo.add(makeTestAttestation(proj1, pubKey, time.Now().UTC().Add(-time.Hour)))

	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/attestations?leaf_id=%s", proj1), nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp attestationListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Data) != 2 {
		t.Errorf("data length = %d, want 2 (both proj1)", len(resp.Data))
	}
	for _, d := range resp.Data {
		if d.LeafID != proj1 {
			t.Errorf("expected leaf_id %s, got %s", proj1, d.LeafID)
		}
	}
}

func TestHandlerFilterByVolunteerPublicKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	repo := newMockAttestationRepo()
	leafID := types.NewID()
	pubKey1 := make([]byte, 32)
	pubKey1[0] = 1
	pubKey2 := make([]byte, 32)
	pubKey2[0] = 2
	repo.add(makeTestAttestation(leafID, pubKey1, time.Now().UTC()))
	repo.add(makeTestAttestation(leafID, pubKey2, time.Now().UTC()))

	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	encoded := base64.RawURLEncoding.EncodeToString(pubKey1)
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/attestations?volunteer_public_key=%s", encoded), nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp attestationListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Data) != 1 {
		t.Errorf("data length = %d, want 1", len(resp.Data))
	}
}

func TestHandlerFilterByTimeRange(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	repo := newMockAttestationRepo()
	leafID := types.NewID()
	pubKey := make([]byte, 32)

	now := time.Now().UTC()
	repo.add(makeTestAttestation(leafID, pubKey, now))
	repo.add(makeTestAttestation(leafID, pubKey, now.Add(-24*time.Hour)))
	repo.add(makeTestAttestation(leafID, pubKey, now.Add(-48*time.Hour)))

	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	from := types.FormatTimestamp(now.Add(-25 * time.Hour))
	to := types.FormatTimestamp(now.Add(time.Hour))
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/attestations?leaf_id=%s&from=%s&to=%s", leafID, from, to), nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp attestationListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Data) != 2 {
		t.Errorf("data length = %d, want 2 (within time range)", len(resp.Data))
	}
}

func TestHandlerPagination(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	repo := newMockAttestationRepo()
	leafID := types.NewID()
	pubKey := make([]byte, 32)

	for i := 0; i < 5; i++ {
		repo.add(makeTestAttestation(leafID, pubKey, time.Now().UTC().Add(time.Duration(-i)*time.Minute)))
	}

	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/attestations?limit=3", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp attestationListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Data) != 3 {
		t.Errorf("page 1: data length = %d, want 3", len(resp.Data))
	}
	if !resp.Pagination.HasMore {
		t.Error("page 1: has_more should be true")
	}
}

func TestHandlerResponseIncludesSigningPublicKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	repo := newMockAttestationRepo()
	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/attestations", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var resp attestationListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.SigningPublicKey == "" {
		t.Error("signing_public_key should be non-empty")
	}

	keyBytes, err := base64.RawURLEncoding.DecodeString(resp.SigningPublicKey)
	if err != nil {
		t.Fatalf("decode signing key: %v", err)
	}
	if len(keyBytes) != ed25519.PublicKeySize {
		t.Errorf("signing key size = %d, want %d", len(keyBytes), ed25519.PublicKeySize)
	}
}

func TestHandlerInvalidLeafID(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	handler := NewHandler(newMockAttestationRepo(), signer.PublicKey(), testHandlerLogger())

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/attestations?leaf_id=not-a-uuid", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandlerInvalidLimit(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	handler := NewHandler(newMockAttestationRepo(), signer.PublicKey(), testHandlerLogger())

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/attestations?limit=abc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandlerInvalidVolunteerPublicKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	handler := NewHandler(newMockAttestationRepo(), signer.PublicKey(), testHandlerLogger())

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// "!!!" is not valid base64url.
	req := httptest.NewRequest("GET", "/api/v1/attestations?volunteer_public_key=!!!", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// errorListRepo is a mock that returns an error from List.
type errorListRepo struct {
	mockAttestationRepo
}

func (e *errorListRepo) List(_ context.Context, _ ListFilters, _ types.PaginationRequest) ([]*Attestation, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, fmt.Errorf("database connection lost")
}

func TestHandlerRepoError(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	handler := NewHandler(&errorListRepo{}, signer.PublicKey(), testHandlerLogger())

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/attestations", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("status = %d, want non-200 when repo returns error", w.Code)
	}
}

func TestHandlerEmptyResultReturnsEmptyArray(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	repo := newMockAttestationRepo()
	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/attestations", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp attestationListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Data == nil {
		t.Error("data should be an empty array (non-nil), not null")
	}
	if len(resp.Data) != 0 {
		t.Errorf("data length = %d, want 0", len(resp.Data))
	}
}

func TestHandlerWireFormatEncoding(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	repo := newMockAttestationRepo()
	leafID := types.NewID()
	pubKey := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}
	att := makeTestAttestation(leafID, pubKey, time.Now().UTC())
	att.Signature = []byte("test-signature-bytes")
	repo.add(att)

	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/attestations", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp attestationListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Data) != 1 {
		t.Fatalf("data length = %d, want 1", len(resp.Data))
	}

	d := resp.Data[0]

	// volunteer_public_key should be base64url-encoded.
	decodedPubKey, err := base64.RawURLEncoding.DecodeString(d.VolunteerPublicKey)
	if err != nil {
		t.Fatalf("decode volunteer_public_key: %v", err)
	}
	if string(decodedPubKey) != string(pubKey) {
		t.Error("volunteer_public_key does not match")
	}

	// signature should be base64url-encoded.
	decodedSig, err := base64.RawURLEncoding.DecodeString(d.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if string(decodedSig) != string(att.Signature) {
		t.Error("signature does not match")
	}

	// attestation_timestamp should be a valid RFC3339 string.
	if d.AttestationTimestamp == "" {
		t.Error("attestation_timestamp should not be empty")
	}
}
