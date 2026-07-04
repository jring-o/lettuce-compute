package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/admission"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// This file exercises the registration proof-of-work GATE — the CREATE-branch
// enforcement in RegisterVolunteer / handleBrowserRegister and the challenge-issuance
// guards in GetRegistrationChallenge / handleBrowserRegisterChallenge. Live challenge
// ISSUANCE against a real table is DB-backed and out of scope here (covered by the
// storage agents); these tests pin the pure gate wiring with a nil pool. They reuse the
// same-package helpers/fakes from registration_admission_test.go (admissionRecordingVolunteerRepo,
// newAdmissionTestService, admissionCtx, admissionRegisterReq, admissionBrowserDeps,
// admissionBrowserRegisterBody).

// enabledPowPolicy is the standard "enforcement on" policy these tests set. DifficultyBits
// is unused by the gate (the difficulty check happens inside RedeemChallenge, which these
// nil-pool tests never reach — CreateAdmitted is a fake), so any positive value works.
func enabledPowPolicy() admission.PowPolicy {
	return admission.PowPolicy{Enabled: true, DifficultyBits: 20, ChallengeTTL: time.Minute}
}

// browserPowRegisterBody builds a browser register body carrying a proof-of-work solution.
// The nonce is a DECIMAL STRING (the REST contract — JSON numbers cannot carry a full
// uint64). admissionBrowserRegisterBody (reused for the no-solution cases) omits both
// fields; this is the with-solution twin.
func browserPowRegisterBody(pub ed25519.PublicKey, challengeID, nonce string) string {
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	return `{"public_key":"` + pubB64 + `","display_name":"Pow Test",` +
		`"hardware":{"cpu_cores":4,"memory_mb":8192,"available_runtimes":["WASM"]},` +
		`"pow_challenge_id":"` + challengeID + `","pow_nonce":"` + nonce + `"}`
}

func newPowKeypair(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	return pub
}

// --- gRPC path tests ---

// Pow disabled (zero policy): a new registration with no solution succeeds and the gate is
// nil (the cap is off too), byte-for-byte the legacy create path.
func TestRegisterVolunteerPow_DisabledInert(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	svc := newAdmissionTestService(t, repo) // zero-value policy: pow off, cap off

	pub := newPowKeypair(t)
	resp, err := svc.RegisterVolunteer(admissionCtx(pub, ""), admissionRegisterReq(pub))
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	if !resp.Registered {
		t.Error("expected Registered = true for a new volunteer")
	}
	if repo.createCalls != 1 {
		t.Errorf("expected exactly 1 CreateAdmitted call, got %d", repo.createCalls)
	}
	if !repo.gateWasNil || repo.recordedGate != nil {
		t.Errorf("expected a nil admission gate while pow (and the cap) are disabled, got %+v", repo.recordedGate)
	}
}

// Pow enabled + a new registration with NO solution: the pinned pow-required refusal. It is
// a FailedPrecondition carrying admission.PowRequiredMessage EXACTLY (existing CLIs classify
// that text as "update your build"), and CreateAdmitted is never reached.
func TestRegisterVolunteerPow_Required(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	svc := newAdmissionTestService(t, repo)
	SetRegistrationPowPolicy(svc, enabledPowPolicy())

	pub := newPowKeypair(t)
	_, err := svc.RegisterVolunteer(admissionCtx(pub, ""), admissionRegisterReq(pub))
	if err == nil {
		t.Fatal("expected an error when pow is required but no solution was supplied")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected a gRPC status error, got %v", err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition, got %s", st.Code())
	}
	if st.Message() != admission.PowRequiredMessage {
		t.Errorf("expected the pinned message %q, got %q", admission.PowRequiredMessage, st.Message())
	}
	if repo.createCalls != 0 {
		t.Errorf("expected CreateAdmitted to be skipped on a pow-required refusal, got %d calls", repo.createCalls)
	}
}

// Pow enabled but the key already exists: re-registration takes the UPDATE path, which never
// consults pow. No solution is supplied and it still succeeds (Registered = false); the
// create branch (and its pow gate) is never entered.
func TestRegisterVolunteerPow_RequiredOnlyOnCreate(t *testing.T) {
	existing := &volunteer.Volunteer{ID: types.NewID()}
	repo := newAdmissionRecordingVolunteerRepo()
	repo.existingVol = existing
	repo.notFoundBefore = 0 // the FIRST lookup already finds the existing row
	svc := newAdmissionTestService(t, repo)
	SetRegistrationPowPolicy(svc, enabledPowPolicy())

	pub := newPowKeypair(t)
	resp, err := svc.RegisterVolunteer(admissionCtx(pub, ""), admissionRegisterReq(pub))
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	if resp.Registered {
		t.Error("expected Registered = false on the re-registration update path")
	}
	if resp.VolunteerId != existing.ID.String() {
		t.Errorf("expected the existing volunteer id %q, got %q", existing.ID.String(), resp.VolunteerId)
	}
	if repo.createCalls != 0 {
		t.Errorf("expected 0 CreateAdmitted calls (update path never pays pow), got %d", repo.createCalls)
	}
	if repo.updateCalls != 1 {
		t.Errorf("expected exactly 1 Update call, got %d", repo.updateCalls)
	}
}

// Pow enabled + a present-but-unparseable challenge id: InvalidArgument mentioning
// pow_challenge_id, before CreateAdmitted is reached (retrying the same payload cannot help).
func TestRegisterVolunteerPow_BadChallengeID(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	svc := newAdmissionTestService(t, repo)
	SetRegistrationPowPolicy(svc, enabledPowPolicy())

	pub := newPowKeypair(t)
	req := admissionRegisterReq(pub)
	req.PowChallengeId = "not-a-uuid"
	req.PowNonce = 999

	_, err := svc.RegisterVolunteer(admissionCtx(pub, ""), req)
	if err == nil {
		t.Fatal("expected an error for an unparseable pow_challenge_id")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", st.Code())
	}
	if !strings.Contains(st.Message(), "pow_challenge_id") {
		t.Errorf("expected the message to mention pow_challenge_id, got %q", st.Message())
	}
	if repo.createCalls != 0 {
		t.Errorf("expected CreateAdmitted to be skipped on a bad challenge id, got %d calls", repo.createCalls)
	}
}

// Pow enabled + a valid solution: the create path builds a gate carrying the pow redemption
// (challenge id, registering key, nonce) and rides it into CreateAdmitted. The pow_only
// sub-test proves the "gate synthesized when nil" path (cap off); the pow_and_cap sub-test
// proves the gate carries BOTH the cap bucketing AND the pow redemption.
func TestRegisterVolunteerPow_GateCarriesPow(t *testing.T) {
	t.Run("pow_only", func(t *testing.T) {
		repo := newAdmissionRecordingVolunteerRepo()
		svc := newAdmissionTestService(t, repo) // cap off: the gate is nil until pow synthesizes it
		SetRegistrationPowPolicy(svc, enabledPowPolicy())

		pub := newPowKeypair(t)
		challengeID := types.NewID()
		req := admissionRegisterReq(pub)
		req.PowChallengeId = challengeID.String()
		req.PowNonce = 12345

		resp, err := svc.RegisterVolunteer(admissionCtx(pub, ""), req)
		if err != nil {
			t.Fatalf("RegisterVolunteer: %v", err)
		}
		if !resp.Registered {
			t.Error("expected Registered = true")
		}
		if repo.recordedGate == nil {
			t.Fatal("expected a non-nil gate synthesized to carry the pow redemption")
		}
		if repo.recordedGate.Bucket != "" || repo.recordedGate.CapPerDay != 0 {
			t.Errorf("expected an empty cap bucketing on a pow-only gate, got bucket=%q cap=%d",
				repo.recordedGate.Bucket, repo.recordedGate.CapPerDay)
		}
		if repo.recordedGate.Pow == nil {
			t.Fatal("expected the gate to carry a pow redemption")
		}
		if repo.recordedGate.Pow.ChallengeID != challengeID {
			t.Errorf("expected pow challenge id %s, got %s", challengeID, repo.recordedGate.Pow.ChallengeID)
		}
		if repo.recordedGate.Pow.Nonce != 12345 {
			t.Errorf("expected pow nonce 12345, got %d", repo.recordedGate.Pow.Nonce)
		}
		if !bytes.Equal(repo.recordedGate.Pow.PublicKey, pub) {
			t.Error("expected the pow redemption to carry the registering key")
		}
	})

	t.Run("pow_and_cap", func(t *testing.T) {
		repo := newAdmissionRecordingVolunteerRepo()
		svc := newAdmissionTestService(t, repo)
		SetAdmissionPolicy(svc, admission.CapPolicy{Enabled: true, PerDay: 7})
		SetRegistrationPowPolicy(svc, enabledPowPolicy())

		pub := newPowKeypair(t)
		challengeID := types.NewID()
		req := admissionRegisterReq(pub)
		req.PowChallengeId = challengeID.String()
		req.PowNonce = 12345

		resp, err := svc.RegisterVolunteer(admissionCtx(pub, "192.0.2.10"), req)
		if err != nil {
			t.Fatalf("RegisterVolunteer: %v", err)
		}
		if !resp.Registered {
			t.Error("expected Registered = true")
		}
		if repo.recordedGate == nil {
			t.Fatal("expected a non-nil gate carrying both cap bucketing and the pow redemption")
		}
		// Cap bucketing.
		if repo.recordedGate.Bucket != "192.0.2.10" {
			t.Errorf("expected gate bucket %q, got %q", "192.0.2.10", repo.recordedGate.Bucket)
		}
		if repo.recordedGate.CapPerDay != 7 {
			t.Errorf("expected gate CapPerDay 7, got %d", repo.recordedGate.CapPerDay)
		}
		// Pow redemption.
		if repo.recordedGate.Pow == nil {
			t.Fatal("expected the gate to carry a pow redemption alongside the cap bucketing")
		}
		if repo.recordedGate.Pow.ChallengeID != challengeID {
			t.Errorf("expected pow challenge id %s, got %s", challengeID, repo.recordedGate.Pow.ChallengeID)
		}
		if repo.recordedGate.Pow.Nonce != 12345 {
			t.Errorf("expected pow nonce 12345, got %d", repo.recordedGate.Pow.Nonce)
		}
		if !bytes.Equal(repo.recordedGate.Pow.PublicKey, pub) {
			t.Error("expected the pow redemption to carry the registering key")
		}
	})
}

// Pow enabled + CreateAdmitted returns a pow-invalid sentinel (stale/foreign challenge or a
// wrong nonce, surfaced by the transactional redeem): InvalidArgument whose message contains
// "proof-of-work rejected" — distinct from the pow-required signal (retrying the same payload
// cannot succeed; fetch a fresh challenge and re-solve).
func TestRegisterVolunteerPow_InvalidMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"solution_invalid", admission.ErrPowSolutionInvalid},
		{"challenge_invalid", admission.ErrPowChallengeInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newAdmissionRecordingVolunteerRepo()
			repo.createErr = tc.err
			svc := newAdmissionTestService(t, repo)
			SetRegistrationPowPolicy(svc, enabledPowPolicy())

			pub := newPowKeypair(t)
			req := admissionRegisterReq(pub)
			req.PowChallengeId = types.NewID().String()
			req.PowNonce = 42

			_, err := svc.RegisterVolunteer(admissionCtx(pub, ""), req)
			if err == nil {
				t.Fatalf("expected an error for %v", tc.err)
			}
			st, _ := status.FromError(err)
			if st.Code() != codes.InvalidArgument {
				t.Errorf("expected InvalidArgument, got %s", st.Code())
			}
			if !strings.Contains(st.Message(), "proof-of-work rejected") {
				t.Errorf("expected the message to contain %q, got %q", "proof-of-work rejected", st.Message())
			}
		})
	}
}

// GetRegistrationChallenge guards on the bare-test wiring (nil pool / zero policy) and on an
// unauthenticated caller. Live issuance needs a real pool + a DB row and is out of scope here.
func TestGetRegistrationChallenge_Guards(t *testing.T) {
	pub := newPowKeypair(t)

	t.Run("nil_pool_zero_policy_unavailable", func(t *testing.T) {
		svc := newAdmissionTestService(t, newAdmissionRecordingVolunteerRepo()) // nil pool, zero policy
		_, err := svc.GetRegistrationChallenge(admissionCtx(pub, ""), &lettucev1.GetRegistrationChallengeRequest{})
		st, _ := status.FromError(err)
		if st.Code() != codes.Unavailable {
			t.Errorf("expected Unavailable with a nil pool and a zero policy, got %s", st.Code())
		}
	})

	t.Run("nil_pool_nonzero_difficulty_still_unavailable", func(t *testing.T) {
		svc := newAdmissionTestService(t, newAdmissionRecordingVolunteerRepo())
		// A non-zero difficulty is not enough: issuance needs the pool, which is nil here.
		SetRegistrationPowPolicy(svc, admission.PowPolicy{DifficultyBits: 20, ChallengeTTL: time.Minute})
		_, err := svc.GetRegistrationChallenge(admissionCtx(pub, ""), &lettucev1.GetRegistrationChallengeRequest{})
		st, _ := status.FromError(err)
		if st.Code() != codes.Unavailable {
			t.Errorf("expected Unavailable with a nil pool even at a non-zero difficulty, got %s", st.Code())
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		svc := newAdmissionTestService(t, newAdmissionRecordingVolunteerRepo())
		SetRegistrationPowPolicy(svc, enabledPowPolicy())
		// No auth pubkey stashed on a bare context.
		_, err := svc.GetRegistrationChallenge(context.Background(), &lettucev1.GetRegistrationChallengeRequest{})
		st, _ := status.FromError(err)
		if st.Code() != codes.Unauthenticated {
			t.Errorf("expected Unauthenticated without an authed key, got %s", st.Code())
		}
	})
}

// --- REST path tests ---

// Pow disabled: the browser register path passes a nil gate and returns 201 Created.
func TestBrowserRegisterPow_DisabledInert(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	deps := admissionBrowserDeps(repo, admission.CapPolicy{}) // pow off (registrationPow zero), cap off
	handler := handleBrowserRegister(deps)

	pub := newPowKeypair(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", strings.NewReader(admissionBrowserRegisterBody(pub)))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if repo.createCalls != 1 {
		t.Errorf("expected exactly 1 CreateAdmitted call, got %d", repo.createCalls)
	}
	if !repo.gateWasNil || repo.recordedGate != nil {
		t.Errorf("expected a nil admission gate while pow (and the cap) are disabled, got %+v", repo.recordedGate)
	}
}

// Pow enabled + a new registration with no solution: the 403 POW_REQUIRED signal whose
// message is exactly the machine-readable prefix (a future dashboard solver keys on the code).
func TestBrowserRegisterPow_Required(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	deps := admissionBrowserDeps(repo, admission.CapPolicy{})
	deps.registrationPow = enabledPowPolicy()
	handler := handleBrowserRegister(deps)

	pub := newPowKeypair(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", strings.NewReader(admissionBrowserRegisterBody(pub)))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	var body apierror.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if body.Error.Code != "POW_REQUIRED" {
		t.Errorf("expected code POW_REQUIRED, got %q", body.Error.Code)
	}
	if body.Error.Message != admission.PowRequiredMessagePrefix {
		t.Errorf("expected message %q, got %q", admission.PowRequiredMessagePrefix, body.Error.Message)
	}
	if repo.createCalls != 0 {
		t.Errorf("expected CreateAdmitted to be skipped on a pow-required refusal, got %d calls", repo.createCalls)
	}
}

// Pow enabled + a valid challenge id but an unparseable nonce string: a 400 validation error
// mentioning pow_nonce (the nonce is a decimal-string uint64 on the REST surface).
func TestBrowserRegisterPow_BadNonce(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	deps := admissionBrowserDeps(repo, admission.CapPolicy{})
	deps.registrationPow = enabledPowPolicy()
	handler := handleBrowserRegister(deps)

	pub := newPowKeypair(t)
	body := browserPowRegisterBody(pub, types.NewID().String(), "abc")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp apierror.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if !strings.Contains(resp.Error.Message, "pow_nonce") {
		t.Errorf("expected the message to mention pow_nonce, got %q", resp.Error.Message)
	}
	if repo.createCalls != 0 {
		t.Errorf("expected CreateAdmitted to be skipped on a bad nonce, got %d calls", repo.createCalls)
	}
}

// Pow enabled + a max-uint64 nonce as a decimal string: the gate carries the parsed nonce
// unchanged. This pins the decimal-string uint64 contract at its upper boundary (a JSON
// number would have lost precision here).
func TestBrowserRegisterPow_GateCarriesNonce(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	deps := admissionBrowserDeps(repo, admission.CapPolicy{})
	deps.registrationPow = enabledPowPolicy()
	handler := handleBrowserRegister(deps)

	pub := newPowKeypair(t)
	challengeID := types.NewID()
	body := browserPowRegisterBody(pub, challengeID.String(), "18446744073709551615") // math.MaxUint64
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if repo.recordedGate == nil || repo.recordedGate.Pow == nil {
		t.Fatalf("expected the gate to carry a pow redemption, got %+v", repo.recordedGate)
	}
	if repo.recordedGate.Pow.Nonce != math.MaxUint64 {
		t.Errorf("expected the max-uint64 nonce %d, got %d", uint64(math.MaxUint64), repo.recordedGate.Pow.Nonce)
	}
	if repo.recordedGate.Pow.ChallengeID != challengeID {
		t.Errorf("expected pow challenge id %s, got %s", challengeID, repo.recordedGate.Pow.ChallengeID)
	}
	if !bytes.Equal(repo.recordedGate.Pow.PublicKey, pub) {
		t.Error("expected the pow redemption to carry the registering key")
	}
}

// Pow enabled + CreateAdmitted returns a pow-invalid sentinel: the browser path maps it to a
// 400 POW_INVALID (fetch a fresh challenge and re-solve), not the POW_REQUIRED signal.
func TestBrowserRegisterPow_InvalidMapping(t *testing.T) {
	repo := newAdmissionRecordingVolunteerRepo()
	repo.createErr = admission.ErrPowChallengeInvalid
	deps := admissionBrowserDeps(repo, admission.CapPolicy{})
	deps.registrationPow = enabledPowPolicy()
	handler := handleBrowserRegister(deps)

	pub := newPowKeypair(t)
	body := browserPowRegisterBody(pub, types.NewID().String(), "42")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var body2 apierror.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body2); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if body2.Error.Code != "POW_INVALID" {
		t.Errorf("expected code POW_INVALID, got %q", body2.Error.Code)
	}
}

// The challenge-issuance endpoint validates its input and guards the bare-test wiring: a
// malformed public key is a 400; a valid key with a nil pool / zero policy is a 503
// POW_UNAVAILABLE (issuance is not configured on this head). Live issuance is DB-backed and
// out of scope here.
func TestBrowserRegistrationChallenge_Validation(t *testing.T) {
	t.Run("missing_public_key", func(t *testing.T) {
		deps := admissionBrowserDeps(newAdmissionRecordingVolunteerRepo(), admission.CapPolicy{})
		handler := handleBrowserRegisterChallenge(deps)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register-challenge", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for a missing public_key, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("bad_base64", func(t *testing.T) {
		deps := admissionBrowserDeps(newAdmissionRecordingVolunteerRepo(), admission.CapPolicy{})
		handler := handleBrowserRegisterChallenge(deps)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register-challenge", strings.NewReader(`{"public_key":"!!!not-base64!!!"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for a non-base64url public_key, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("wrong_length", func(t *testing.T) {
		deps := admissionBrowserDeps(newAdmissionRecordingVolunteerRepo(), admission.CapPolicy{})
		handler := handleBrowserRegisterChallenge(deps)
		shortKey := base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3}) // 3 bytes, not 32
		req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register-challenge",
			strings.NewReader(`{"public_key":"`+shortKey+`"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for a wrong-length public_key, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("nil_pool_zero_policy_unavailable", func(t *testing.T) {
		// admissionBrowserDeps leaves the pool nil and the pow policy zero.
		deps := admissionBrowserDeps(newAdmissionRecordingVolunteerRepo(), admission.CapPolicy{})
		handler := handleBrowserRegisterChallenge(deps)
		pub := newPowKeypair(t)
		validKey := base64.RawURLEncoding.EncodeToString(pub)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register-challenge",
			strings.NewReader(`{"public_key":"`+validKey+`"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 with a nil pool and a zero policy, got %d: %s", rec.Code, rec.Body.String())
		}
		var body apierror.ErrorResponse
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode error body: %v", err)
		}
		if body.Error.Code != "POW_UNAVAILABLE" {
			t.Errorf("expected code POW_UNAVAILABLE, got %q", body.Error.Code)
		}
	})
}
