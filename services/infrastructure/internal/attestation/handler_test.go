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
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
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

func (m *mockAttestationRepo) GetByID(_ context.Context, id types.ID) (*Attestation, error) {
	for _, a := range m.attestations {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, apierror.NotFound("attestation", id.String())
}

func (m *mockAttestationRepo) ListRevocationsOf(_ context.Context, attestationID types.ID) ([]*Attestation, error) {
	var revocations []*Attestation
	for _, a := range m.attestations {
		if a.RevokesAttestationID != nil && *a.RevokesAttestationID == attestationID {
			revocations = append(revocations, a)
		}
	}
	return revocations, nil
}

// --- Test Helpers ---

func testHandlerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func makeTestAttestation(leafID types.ID, pubKey []byte, ts time.Time) *Attestation {
	return &Attestation{
		LeafID:             leafID,
		VolunteerPublicKey: pubKey,
		WorkUnitID:         types.NewID(),
		RawMetrics: map[string]any{
			"wall_clock_seconds": float64(100),
			"cpu_seconds_user":   float64(90),
		},
		ValidationOutcome:    OutcomeAgreed,
		CreditAmount:         1.0,
		AttestationTimestamp: ts,
		Signature:            []byte("test-signature"),
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

// --- Slice-4 helpers: signed v1 / v2 grant / v2 revocation fixtures ---

func makeSignedV1(t *testing.T, signer *Signer, leafID types.ID, pubKey []byte, ts time.Time) *Attestation {
	t.Helper()
	att := &Attestation{
		SchemaVersion:      SchemaVersionV1,
		LeafID:             leafID,
		VolunteerPublicKey: pubKey,
		WorkUnitID:         types.NewID(),
		RawMetrics:         map[string]any{"wall_clock_seconds": float64(100)},
		ValidationOutcome:  OutcomeAgreed,
		CreditAmount:       1.0,
		// v1 signs the float64 credit directly; CreditAmountCanonical stays empty.
		AttestationTimestamp: ts,
	}
	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("sign v1: %v", err)
	}
	att.Signature = sig
	return att
}

func makeSignedV2Grant(t *testing.T, signer *Signer, leafID types.ID, pubKey []byte, ts time.Time) *Attestation {
	t.Helper()
	resultID := types.NewID()
	checksum := strings.Repeat("a", 64) // 64 lowercase hex
	policyVersion := PolicyVersion
	att := &Attestation{
		SchemaVersion:      SchemaVersionV2,
		LeafID:             leafID,
		VolunteerPublicKey: pubKey,
		WorkUnitID:         types.NewID(),
		ResultID:           &resultID,
		OutputChecksum:     &checksum,
		QuorumDescriptor: &QuorumDescriptor{
			AuditRatePPM:            1000,
			GroupSize:               3,
			MinQuorum:               2,
			MinTrustedCorroborators: 1,
			PendingSize:             3,
			TargetCopies:            3,
			TrustFloor:              0,
			TrustedCorroborators:    2,
		},
		PolicyVersion:         &policyVersion,
		RawMetrics:            map[string]any{"wall_clock_seconds": float64(100)},
		ValidationOutcome:     OutcomeAgreed,
		CreditAmount:          1.0,
		CreditAmountCanonical: "1.000000",
		AttestationTimestamp:  ts,
	}
	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("sign v2 grant: %v", err)
	}
	att.Signature = sig
	return att
}

func makeSignedV2Revocation(t *testing.T, signer *Signer, leafID types.ID, pubKey []byte, grantID types.ID, ts time.Time) *Attestation {
	t.Helper()
	resultID := types.NewID()
	adjustmentID := types.NewID()
	reason := "OPERATOR_CLAWBACK"
	att := &Attestation{
		SchemaVersion:         SchemaVersionV2,
		LeafID:                leafID,
		VolunteerPublicKey:    pubKey,
		WorkUnitID:            types.NewID(),
		ResultID:              &resultID,
		RevokesAttestationID:  &grantID,
		AdjustmentID:          &adjustmentID,
		Reason:                &reason,
		RawMetrics:            map[string]any{},
		ValidationOutcome:     OutcomeRevoked,
		CreditAmount:          1.0,
		CreditAmountCanonical: "1.000000",
		AttestationTimestamp:  ts,
	}
	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("sign v2 revocation: %v", err)
	}
	att.Signature = sig
	return att
}

// doVerify POSTs to the verify endpoint for a known-good id and decodes the 200 response.
func doVerify(t *testing.T, mux *http.ServeMux, id types.ID) verifyResponse {
	t.Helper()
	body := fmt.Sprintf(`{"attestation_id":"%s"}`, id)
	req := httptest.NewRequest("POST", "/api/v1/attestations/verify", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("verify status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp verifyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode verify response: %v", err)
	}
	return resp
}

// --- Deliverable 3(a): wire rename regression (BG-06a item 1) ---

func TestListWire_MetricsLabeledUnverified(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)

	repo := newMockAttestationRepo()
	leafID := types.NewID()
	pubKey := make([]byte, 32)
	repo.add(makeSignedV2Grant(t, signer, leafID, pubKey, time.Now().UTC()))

	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/attestations", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// Assert against the raw JSON bytes: the served key is unverified_volunteer_metrics and the
	// old raw_metrics key is gone (hard rename, no alias).
	body := w.Body.String()
	if !strings.Contains(body, "unverified_volunteer_metrics") {
		t.Error("response must carry unverified_volunteer_metrics")
	}
	if strings.Contains(body, "raw_metrics") {
		t.Error("response must NOT carry the old raw_metrics key")
	}
	if !strings.Contains(body, `"schema_version"`) {
		t.Error("response rows must carry schema_version")
	}

	var resp attestationListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SignedFieldsBySchemaVersion == nil {
		t.Fatal("envelope must carry signed_fields_by_schema_version")
	}
	for _, key := range []string{"1", "2", "2-revocation"} {
		if len(resp.SignedFieldsBySchemaVersion[key]) == 0 {
			t.Errorf("signed_fields_by_schema_version missing/empty for key %q", key)
		}
	}
	if resp.VerificationRecipe != "guides/attestation-verification.md" {
		t.Errorf("verification_recipe = %q, want guides/attestation-verification.md", resp.VerificationRecipe)
	}
}

// --- Deliverable 3(b): verify endpoint table ---

func TestVerifyEndpoint_BadRequests(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	handler := NewHandler(newMockAttestationRepo(), signer.PublicKey(), testHandlerLogger())
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	cases := []struct {
		name string
		body string
	}{
		{"malformed json", "{not json"},
		{"missing id", `{}`},
		{"empty id", `{"attestation_id":""}`},
		{"non-uuid id", `{"attestation_id":"not-a-uuid"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/attestations/verify", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
		})
	}
}

func TestVerifyEndpoint_OversizedBody(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	handler := NewHandler(newMockAttestationRepo(), signer.PublicKey(), testHandlerLogger())
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// >1 KiB body: MaxBytesReader trips before the id can be parsed → 400.
	big := `{"attestation_id":"` + strings.Repeat("a", 2048) + `"}`
	req := httptest.NewRequest("POST", "/api/v1/attestations/verify", strings.NewReader(big))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for oversized body", w.Code)
	}
}

func TestVerifyEndpoint_UnknownID(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	handler := NewHandler(newMockAttestationRepo(), signer.PublicKey(), testHandlerLogger())
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	body := fmt.Sprintf(`{"attestation_id":"%s"}`, types.NewID())
	req := httptest.NewRequest("POST", "/api/v1/attestations/verify", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestVerifyEndpoint_ValidV1(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	repo := newMockAttestationRepo()
	leafID := types.NewID()
	pubKey := make([]byte, 32)
	att := makeSignedV1(t, signer, leafID, pubKey, time.Now().UTC())
	repo.add(att)

	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	resp := doVerify(t, mux, att.ID)
	if resp.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", resp.SchemaVersion)
	}
	if resp.Kind != "grant" {
		t.Errorf("kind = %q, want grant", resp.Kind)
	}
	if !resp.SignatureValid {
		t.Error("signature_valid should be true for a correctly signed v1 row")
	}
	if resp.CanonicalPayload == "" {
		t.Error("canonical_payload should be present for a valid row")
	}
	if len(resp.SignedFields) != len(signedFieldsV1) {
		t.Errorf("v1 signed_fields len = %d, want %d", len(resp.SignedFields), len(signedFieldsV1))
	}
	if resp.Revocations == nil {
		t.Error("revocations should be an empty array, not null, for a grant")
	}
	if len(resp.Revocations) != 0 {
		t.Errorf("revocations len = %d, want 0", len(resp.Revocations))
	}
	if resp.Error != "" {
		t.Errorf("error = %q, want empty for a valid row", resp.Error)
	}
}

func TestVerifyEndpoint_ValidV2Grant(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	repo := newMockAttestationRepo()
	leafID := types.NewID()
	pubKey := make([]byte, 32)
	att := makeSignedV2Grant(t, signer, leafID, pubKey, time.Now().UTC())
	repo.add(att)

	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	resp := doVerify(t, mux, att.ID)
	if resp.SchemaVersion != 2 {
		t.Errorf("schema_version = %d, want 2", resp.SchemaVersion)
	}
	if resp.Kind != "grant" {
		t.Errorf("kind = %q, want grant", resp.Kind)
	}
	if !resp.SignatureValid {
		t.Error("signature_valid should be true for a correctly signed v2 grant")
	}
	if len(resp.SignedFields) != len(signedFieldsV2Grant) {
		t.Errorf("v2 grant signed_fields len = %d, want %d", len(resp.SignedFields), len(signedFieldsV2Grant))
	}
	if !strings.Contains(resp.CanonicalPayload, ContextGrantV2) {
		t.Errorf("canonical_payload should carry the v2 grant context %q", ContextGrantV2)
	}
}

func TestVerifyEndpoint_TamperedCredit(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	repo := newMockAttestationRepo()
	leafID := types.NewID()
	pubKey := make([]byte, 32)
	att := makeSignedV2Grant(t, signer, leafID, pubKey, time.Now().UTC())
	// Tamper AFTER signing: the credit a consumer would verify against no longer matches the
	// signed bytes.
	att.CreditAmount = 999.0
	att.CreditAmountCanonical = "999.000000"
	repo.add(att)

	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	resp := doVerify(t, mux, att.ID)
	if resp.SignatureValid {
		t.Error("signature_valid must be false after credit tampering")
	}
	// The row is still well-formed, so the canonical bytes still rebuild (honesty: false, not
	// an error).
	if resp.CanonicalPayload == "" {
		t.Error("canonical_payload should still be present for a well-formed but tampered row")
	}
	if resp.Error != "" {
		t.Errorf("error = %q, want empty for a well-formed row", resp.Error)
	}
}

func TestVerifyEndpoint_GrantWithRevocation(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	repo := newMockAttestationRepo()
	leafID := types.NewID()
	pubKey := make([]byte, 32)

	grant := makeSignedV2Grant(t, signer, leafID, pubKey, time.Now().UTC())
	repo.add(grant)
	rev := makeSignedV2Revocation(t, signer, leafID, pubKey, grant.ID, time.Now().UTC())
	repo.add(rev)

	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// The grant lists its one revocation.
	resp := doVerify(t, mux, grant.ID)
	if resp.Kind != "grant" {
		t.Errorf("grant kind = %q, want grant", resp.Kind)
	}
	if !resp.SignatureValid {
		t.Error("grant signature should be valid")
	}
	if len(resp.Revocations) != 1 {
		t.Fatalf("revocations len = %d, want 1", len(resp.Revocations))
	}
	r0 := resp.Revocations[0]
	if r0.AttestationID != rev.ID {
		t.Errorf("revocation attestation_id = %s, want %s", r0.AttestationID, rev.ID)
	}
	if r0.CreditAmountCanonical != "1.000000" {
		t.Errorf("revocation credit_amount_canonical = %q, want 1.000000", r0.CreditAmountCanonical)
	}
	if r0.CreditAmount != 1.0 {
		t.Errorf("revocation credit_amount = %v, want 1.0", r0.CreditAmount)
	}
	if r0.AttestationTimestamp == "" {
		t.Error("revocation attestation_timestamp should not be empty")
	}

	// The revocation itself verifies as a revocation and lists no revocations of its own.
	revResp := doVerify(t, mux, rev.ID)
	if revResp.Kind != "revocation" {
		t.Errorf("revocation kind = %q, want revocation", revResp.Kind)
	}
	if !revResp.SignatureValid {
		t.Error("revocation signature should be valid")
	}
	if len(revResp.SignedFields) != len(signedFieldsV2Revocation) {
		t.Errorf("revocation signed_fields len = %d, want %d", len(revResp.SignedFields), len(signedFieldsV2Revocation))
	}
	if len(revResp.Revocations) != 0 {
		t.Errorf("a revocation should list no revocations, got %d", len(revResp.Revocations))
	}
}

// TestVerifyEndpoint_MalformedRowIsHonest locks the §8.5 honesty rule: a v2 grant row missing
// a signed field (unverifiable) returns 200 with signature_valid:false + an error, never
// success.
func TestVerifyEndpoint_MalformedRowIsHonest(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	repo := newMockAttestationRepo()
	leafID := types.NewID()
	pubKey := make([]byte, 32)

	// A v2 grant with no result_id: CanonicalJSON cannot rebuild the signed bytes.
	att := &Attestation{
		SchemaVersion:         SchemaVersionV2,
		LeafID:                leafID,
		VolunteerPublicKey:    pubKey,
		WorkUnitID:            types.NewID(),
		ValidationOutcome:     OutcomeAgreed,
		CreditAmount:          1.0,
		CreditAmountCanonical: "1.000000",
		AttestationTimestamp:  time.Now().UTC(),
		Signature:             []byte("unusable"),
	}
	repo.add(att)

	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	resp := doVerify(t, mux, att.ID)
	if resp.SignatureValid {
		t.Error("signature_valid must be false for a malformed (unverifiable) row")
	}
	if resp.CanonicalPayload != "" {
		t.Errorf("canonical_payload = %q, want empty for a malformed row", resp.CanonicalPayload)
	}
	if resp.Error == "" {
		t.Error("error should describe why the row is unverifiable")
	}
}

// --- Deliverable 3(c): unsigned-metrics regression at the endpoint ---

func TestVerifyEndpoint_MetricsUnsignedRegression(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	repo := newMockAttestationRepo()
	leafID := types.NewID()
	pubKey := make([]byte, 32)
	att := makeSignedV2Grant(t, signer, leafID, pubKey, time.Now().UTC())
	repo.add(att)

	// Mutating unsigned volunteer metrics after signing must NOT invalidate the signature —
	// metrics are never part of the canonical bytes (BG-06 posture, v2 heir).
	att.RawMetrics = map[string]any{"wall_clock_seconds": float64(999999), "injected": "attacker"}

	handler := NewHandler(repo, signer.PublicKey(), testHandlerLogger())
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	resp := doVerify(t, mux, att.ID)
	if !resp.SignatureValid {
		t.Error("mutating unsigned metrics must not invalidate the v2 signature (BG-06)")
	}
}
