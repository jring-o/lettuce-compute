package identity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/admission"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// mockVolunteerRepo implements volunteer.Repository for testing.
type mockVolunteerRepo struct {
	volunteers map[string]*volunteer.Volunteer // keyed by base64url public key
}

func newMockVolunteerRepo() *mockVolunteerRepo {
	return &mockVolunteerRepo{volunteers: make(map[string]*volunteer.Volunteer)}
}

func (r *mockVolunteerRepo) addVolunteer(pub ed25519.PublicKey) *volunteer.Volunteer {
	v := &volunteer.Volunteer{
		ID:        types.NewID(),
		PublicKey: pub,
		IsActive:  true,
	}
	key := base64.RawURLEncoding.EncodeToString(pub)
	r.volunteers[key] = v
	return v
}

func (r *mockVolunteerRepo) GetByPublicKey(ctx context.Context, publicKey []byte) (*volunteer.Volunteer, error) {
	key := base64.RawURLEncoding.EncodeToString(publicKey)
	v, ok := r.volunteers[key]
	if !ok {
		return nil, nil
	}
	return v, nil
}

// Stubs for the rest of the interface.
func (r *mockVolunteerRepo) Create(ctx context.Context, v *volunteer.Volunteer) error    { return nil }
func (r *mockVolunteerRepo) CreateAdmitted(ctx context.Context, v *volunteer.Volunteer, _ *admission.CreateGate) error {
	return nil
}
func (r *mockVolunteerRepo) GetByID(ctx context.Context, id types.ID) (*volunteer.Volunteer, error) {
	return nil, nil
}
func (r *mockVolunteerRepo) GetByUserID(ctx context.Context, uid types.ID) (*volunteer.Volunteer, error) {
	return nil, nil
}
func (r *mockVolunteerRepo) Update(ctx context.Context, v *volunteer.Volunteer) error { return nil }
func (r *mockVolunteerRepo) UpdateLastSeen(ctx context.Context, id types.ID) error    { return nil }
func (r *mockVolunteerRepo) SetActive(ctx context.Context, id types.ID, active bool) error {
	return nil
}
func (r *mockVolunteerRepo) IncrementWorkUnitsCompleted(ctx context.Context, id types.ID) error {
	return nil
}
func (r *mockVolunteerRepo) IncrementWorkUnitsRejected(ctx context.Context, id types.ID) error {
	return nil
}
func (r *mockVolunteerRepo) List(ctx context.Context, f volunteer.VolunteerListFilters, p types.PaginationRequest) ([]*volunteer.Volunteer, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (r *mockVolunteerRepo) MarkInactiveOlderThan(ctx context.Context, d time.Duration) (int, error) {
	return 0, nil
}
func (r *mockVolunteerRepo) SetDIDBinding(ctx context.Context, id types.ID, did, uri, cid string, boundAt time.Time) error {
	return nil
}
func (r *mockVolunteerRepo) ListDIDBindingsForRecheck(ctx context.Context, before time.Time, limit int) ([]*volunteer.Volunteer, error) {
	return nil, nil
}
func (r *mockVolunteerRepo) MarkDIDBindingChecked(ctx context.Context, id types.ID, cid string, checkedAt time.Time) error {
	return nil
}
func (r *mockVolunteerRepo) MarkDIDBindingCheckFailed(ctx context.Context, id types.ID, checkedAt time.Time, staleAfter int) error {
	return nil
}
func (r *mockVolunteerRepo) RevokeDIDBinding(ctx context.Context, id types.ID, revokedAt time.Time) error {
	return nil
}
func (r *mockVolunteerRepo) SetDIDFrozenUntil(ctx context.Context, id types.ID, until time.Time) error {
	return nil
}

func setupTestHandler() (*Handler, *mockChallengeStore, *mockVolunteerRepo, ed25519.PublicKey, ed25519.PrivateKey) {
	store := newMockStore()
	vRepo := newMockVolunteerRepo()
	logger := slog.Default()
	handler := NewHandler(store, vRepo, nil, nil, logger)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	vRepo.addVolunteer(pub)

	return handler, store, vRepo, pub, priv
}

func TestHandleChallenge_Success(t *testing.T) {
	handler, _, _, pub, _ := setupTestHandler()

	body, _ := json.Marshal(challengeRequest{
		PublicKey: base64.RawURLEncoding.EncodeToString(pub),
	})

	req := httptest.NewRequest("POST", "/api/v1/identity/challenge", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.handleChallenge(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp challengeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if resp.ChallengeID == "" {
		t.Error("expected non-empty challenge_id")
	}
	if len(resp.Challenge) != 64 { // 32 bytes = 64 hex chars
		t.Errorf("expected 64-char hex challenge, got %d chars", len(resp.Challenge))
	}
}

func TestHandleChallenge_InvalidPublicKey(t *testing.T) {
	handler, _, _, _, _ := setupTestHandler()

	body, _ := json.Marshal(challengeRequest{PublicKey: "not-valid-base64url!!!"})
	req := httptest.NewRequest("POST", "/api/v1/identity/challenge", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.handleChallenge(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleChallenge_UnknownVolunteer(t *testing.T) {
	handler, _, _, _, _ := setupTestHandler()

	// Generate a key that's NOT registered.
	unknownPub, _, _ := ed25519.GenerateKey(rand.Reader)
	body, _ := json.Marshal(challengeRequest{
		PublicKey: base64.RawURLEncoding.EncodeToString(unknownPub),
	})
	req := httptest.NewRequest("POST", "/api/v1/identity/challenge", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.handleChallenge(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleVerify_ValidSignature(t *testing.T) {
	handler, store, _, pub, priv := setupTestHandler()

	// Create a challenge.
	challenge, _ := store.Create(context.Background(), pub)

	// Sign the challenge.
	sig := ed25519.Sign(priv, challenge.Challenge)

	body, _ := json.Marshal(verifyRequest{
		ChallengeID: challenge.ID.String(),
		PublicKey:    base64.RawURLEncoding.EncodeToString(pub),
		Signature:    base64.RawURLEncoding.EncodeToString(sig),
	})

	req := httptest.NewRequest("POST", "/api/v1/identity/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.handleVerify(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp verifyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Verified {
		t.Error("expected verified to be true")
	}
	if resp.VolunteerID == "" {
		t.Error("expected non-empty volunteer_id")
	}
}

func TestHandleVerify_InvalidSignature(t *testing.T) {
	handler, store, _, pub, _ := setupTestHandler()

	challenge, _ := store.Create(context.Background(), pub)

	// Use a bad signature.
	badSig := make([]byte, 64)
	body, _ := json.Marshal(verifyRequest{
		ChallengeID: challenge.ID.String(),
		PublicKey:    base64.RawURLEncoding.EncodeToString(pub),
		Signature:    base64.RawURLEncoding.EncodeToString(badSig),
	})

	req := httptest.NewRequest("POST", "/api/v1/identity/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.handleVerify(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleVerify_ExpiredChallenge(t *testing.T) {
	handler, store, _, pub, priv := setupTestHandler()

	// Create a challenge and manually expire it.
	challenge, _ := store.Create(context.Background(), pub)
	challenge.ExpiresAt = time.Now().UTC().Add(-1 * time.Minute)

	sig := ed25519.Sign(priv, challenge.Challenge)

	body, _ := json.Marshal(verifyRequest{
		ChallengeID: challenge.ID.String(),
		PublicKey:    base64.RawURLEncoding.EncodeToString(pub),
		Signature:    base64.RawURLEncoding.EncodeToString(sig),
	})

	req := httptest.NewRequest("POST", "/api/v1/identity/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.handleVerify(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for expired challenge, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleVerify_WrongPublicKey(t *testing.T) {
	handler, store, vRepo, pub, _ := setupTestHandler()

	challenge, _ := store.Create(context.Background(), pub)

	// Generate a different keypair.
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	vRepo.addVolunteer(otherPub) // Register so it passes volunteer lookup.

	sig := ed25519.Sign(otherPriv, challenge.Challenge)

	body, _ := json.Marshal(verifyRequest{
		ChallengeID: challenge.ID.String(),
		PublicKey:    base64.RawURLEncoding.EncodeToString(otherPub),
		Signature:    base64.RawURLEncoding.EncodeToString(sig),
	})

	req := httptest.NewRequest("POST", "/api/v1/identity/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.handleVerify(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for wrong public key, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleVerify_NonExistentChallenge(t *testing.T) {
	handler, _, _, pub, priv := setupTestHandler()

	sig := ed25519.Sign(priv, []byte("anything"))

	body, _ := json.Marshal(verifyRequest{
		ChallengeID: types.NewID().String(),
		PublicKey:    base64.RawURLEncoding.EncodeToString(pub),
		Signature:    base64.RawURLEncoding.EncodeToString(sig),
	})

	req := httptest.NewRequest("POST", "/api/v1/identity/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.handleVerify(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// mockCreditRepo implements credit.Repository for testing.
type mockCreditRepo struct {
	countsByVolunteer map[types.ID]map[types.ID]int
}

func newMockCreditRepo() *mockCreditRepo {
	return &mockCreditRepo{
		countsByVolunteer: make(map[types.ID]map[types.ID]int),
	}
}

func (r *mockCreditRepo) Create(ctx context.Context, entry *credit.LedgerEntry) error {
	return nil
}
func (r *mockCreditRepo) GetByResultID(ctx context.Context, resultID types.ID) (*credit.LedgerEntry, error) {
	return nil, nil
}
func (r *mockCreditRepo) SumByVolunteerProject(ctx context.Context, volunteerID, leafID types.ID) (float64, error) {
	return 0, nil
}
func (r *mockCreditRepo) CountByVolunteerPerProject(ctx context.Context, volunteerID types.ID) (map[types.ID]int, error) {
	if counts, ok := r.countsByVolunteer[volunteerID]; ok {
		return counts, nil
	}
	return map[types.ID]int{}, nil
}
func (r *mockCreditRepo) ListByVolunteer(ctx context.Context, volunteerID types.ID, page types.PaginationRequest) ([]*credit.LedgerEntry, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (r *mockCreditRepo) ListByLeaf(ctx context.Context, leafID types.ID, page types.PaginationRequest) ([]*credit.LedgerEntry, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}

func setupTestHandlerWithCredit() (*Handler, *mockChallengeStore, *mockVolunteerRepo, *mockCreditRepo, ed25519.PublicKey, ed25519.PrivateKey) {
	store := newMockStore()
	vRepo := newMockVolunteerRepo()
	cRepo := newMockCreditRepo()
	logger := slog.Default()
	handler := NewHandler(store, vRepo, cRepo, nil, logger)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	vRepo.addVolunteer(pub)

	return handler, store, vRepo, cRepo, pub, priv
}

// NOTE: handleInfo requires a live pgxpool.Pool (for hasVerifiedChallenge)
// which cannot be mocked at the unit test level. The handleInfo path is tested via
// integration tests. Here we test the early validation paths that exit before DB access.

func TestHandleInfo_InvalidPublicKey(t *testing.T) {
	handler, _, _, _, _, _ := setupTestHandlerWithCredit()

	req := httptest.NewRequest("GET", "/api/v1/identity/not-valid-base64!!!", nil)
	req.SetPathValue("public_key", "not-valid-base64!!!")
	w := httptest.NewRecorder()

	handler.handleInfo(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleInfo_UnknownVolunteer(t *testing.T) {
	handler, _, _, _, _, _ := setupTestHandlerWithCredit()

	unknownPub, _, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(unknownPub)
	req := httptest.NewRequest("GET", "/api/v1/identity/"+pubB64, nil)
	req.SetPathValue("public_key", pubB64)
	w := httptest.NewRecorder()

	handler.handleInfo(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleVerify_MissingFields(t *testing.T) {
	handler, _, _, _, _ := setupTestHandler()

	// Missing all required fields.
	body, _ := json.Marshal(verifyRequest{})
	req := httptest.NewRequest("POST", "/api/v1/identity/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.handleVerify(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing fields, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleVerify_InvalidChallengeID(t *testing.T) {
	handler, _, _, pub, _ := setupTestHandler()

	body, _ := json.Marshal(verifyRequest{
		ChallengeID: "not-a-valid-uuid",
		PublicKey:    base64.RawURLEncoding.EncodeToString(pub),
		Signature:    base64.RawURLEncoding.EncodeToString(make([]byte, 64)),
	})
	req := httptest.NewRequest("POST", "/api/v1/identity/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.handleVerify(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid challenge_id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleVerify_InvalidSignatureFormat(t *testing.T) {
	handler, store, _, pub, _ := setupTestHandler()

	challenge, _ := store.Create(context.Background(), pub)

	body, _ := json.Marshal(verifyRequest{
		ChallengeID: challenge.ID.String(),
		PublicKey:    base64.RawURLEncoding.EncodeToString(pub),
		Signature:    "!!!not-valid-base64!!!",
	})
	req := httptest.NewRequest("POST", "/api/v1/identity/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.handleVerify(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid signature format, got %d: %s", w.Code, w.Body.String())
	}
}


func TestHandleChallenge_EmptyBody(t *testing.T) {
	handler, _, _, _, _ := setupTestHandler()

	req := httptest.NewRequest("POST", "/api/v1/identity/challenge", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.handleChallenge(w, req)

	// Empty public_key string should fail base64 decode check.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty public key, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRegisterRoutes(t *testing.T) {
	handler, _, _, _, _ := setupTestHandler()
	mux := http.NewServeMux()

	// Should not panic.
	handler.RegisterRoutes(mux)
}
