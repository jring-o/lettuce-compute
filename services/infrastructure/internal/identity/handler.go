package identity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// Handler provides HTTP handlers for identity verification.
type Handler struct {
	store         ChallengeStore
	volunteerRepo volunteer.Repository
	creditRepo    credit.Repository
	pool          *pgxpool.Pool
	logger        *slog.Logger
}

// NewHandler creates a new identity Handler.
func NewHandler(
	store ChallengeStore,
	volunteerRepo volunteer.Repository,
	creditRepo credit.Repository,
	pool *pgxpool.Pool,
	logger *slog.Logger,
) *Handler {
	return &Handler{
		store:         store,
		volunteerRepo: volunteerRepo,
		creditRepo:    creditRepo,
		pool:          pool,
		logger:        logger,
	}
}

// RegisterRoutes registers identity endpoints on the mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/identity/challenge", h.handleChallenge)
	mux.HandleFunc("POST /api/v1/identity/verify", h.handleVerify)
	mux.HandleFunc("GET /api/v1/identity/{public_key}", h.handleInfo)
}

// --- Challenge endpoint ---

type challengeRequest struct {
	PublicKey string `json:"public_key"`
}

type challengeResponse struct {
	ChallengeID string `json:"challenge_id"`
	Challenge   string `json:"challenge"`
	ExpiresAt   string `json:"expires_at"`
}

func (h *Handler) handleChallenge(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	var req challengeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}

	pubKeyBytes, err := base64.RawURLEncoding.DecodeString(req.PublicKey)
	if err != nil || len(pubKeyBytes) != ed25519.PublicKeySize {
		apierror.WriteError(w, apierror.ValidationError("invalid public key: must be base64url-encoded Ed25519 public key (32 bytes)", nil))
		return
	}

	vol, err := h.volunteerRepo.GetByPublicKey(r.Context(), pubKeyBytes)
	if err != nil {
		l.Error("failed to look up volunteer", "error", err)
		apierror.WriteError(w, apierror.Internal("internal server error", err))
		return
	}
	if vol == nil {
		apierror.WriteError(w, apierror.NotFound("volunteer", req.PublicKey))
		return
	}

	challenge, err := h.store.Create(r.Context(), pubKeyBytes)
	if err != nil {
		l.Error("failed to create challenge", "error", err)
		apierror.WriteError(w, apierror.Internal("internal server error", err))
		return
	}

	resp := challengeResponse{
		ChallengeID: challenge.ID.String(),
		Challenge:   challenge.ChallengeHex(),
		ExpiresAt:   types.FormatTimestamp(challenge.ExpiresAt),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// --- Verify endpoint ---

type verifyRequest struct {
	ChallengeID string `json:"challenge_id"`
	PublicKey   string `json:"public_key"`
	Signature   string `json:"signature"`
}

type verifyResponse struct {
	Verified    bool   `json:"verified"`
	VolunteerID string `json:"volunteer_id"`
}

func (h *Handler) handleVerify(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	var req verifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}

	if req.ChallengeID == "" || req.PublicKey == "" || req.Signature == "" {
		apierror.WriteError(w, apierror.ValidationError("challenge_id, public_key, and signature are required", nil))
		return
	}

	challengeID, err := types.ParseID(req.ChallengeID)
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid challenge_id", nil))
		return
	}

	pubKeyBytes, err := base64.RawURLEncoding.DecodeString(req.PublicKey)
	if err != nil || len(pubKeyBytes) != ed25519.PublicKeySize {
		apierror.WriteError(w, apierror.ValidationError("invalid public key format", nil))
		return
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(req.Signature)
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid signature format: must be base64url-encoded", nil))
		return
	}

	challenge, err := h.store.Get(r.Context(), challengeID)
	if err != nil {
		l.Error("failed to get challenge", "error", err)
		apierror.WriteError(w, apierror.Internal("internal server error", err))
		return
	}
	if challenge == nil {
		apierror.WriteError(w, apierror.NotFound("challenge", req.ChallengeID))
		return
	}

	if types.Now().After(challenge.ExpiresAt) {
		apierror.WriteError(w, apierror.NotFound("challenge", req.ChallengeID+" (expired)"))
		return
	}

	if !bytes.Equal(challenge.PublicKey, pubKeyBytes) {
		apierror.WriteError(w, &apierror.APIError{
			Code:       "VERIFICATION_FAILED",
			Message:    "public key does not match the challenge",
			HTTPStatus: 403,
		})
		return
	}

	// Verify Ed25519 signature over the raw challenge bytes.
	if !ed25519.Verify(ed25519.PublicKey(pubKeyBytes), challenge.Challenge, sigBytes) {
		apierror.WriteError(w, &apierror.APIError{
			Code:       "VERIFICATION_FAILED",
			Message:    "Ed25519 signature does not match the challenge",
			HTTPStatus: 403,
		})
		return
	}

	if err := h.store.Verify(r.Context(), challengeID); err != nil {
		l.Error("failed to mark challenge verified", "error", err)
		apierror.WriteError(w, apierror.Internal("internal server error", err))
		return
	}

	vol, err := h.volunteerRepo.GetByPublicKey(r.Context(), pubKeyBytes)
	if err != nil || vol == nil {
		l.Error("failed to find volunteer for verified challenge", "error", err)
		apierror.WriteError(w, apierror.Internal("internal server error", err))
		return
	}

	resp := verifyResponse{
		Verified:    true,
		VolunteerID: vol.ID.String(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// --- Info endpoint ---

type infoResponse struct {
	PublicKey            string  `json:"public_key"`
	VolunteerID          string  `json:"volunteer_id"`
	Verified             bool    `json:"verified"`
	DisplayName          *string `json:"display_name"`
	TotalCredit          float64 `json:"total_credit"`
	ProjectsContributing int     `json:"projects_contributing"`
}

func (h *Handler) handleInfo(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	pubKeyB64 := r.PathValue("public_key")
	pubKeyBytes, err := base64.RawURLEncoding.DecodeString(pubKeyB64)
	if err != nil || len(pubKeyBytes) != ed25519.PublicKeySize {
		apierror.WriteError(w, apierror.ValidationError("invalid public key", nil))
		return
	}

	vol, err := h.volunteerRepo.GetByPublicKey(r.Context(), pubKeyBytes)
	if err != nil {
		l.Error("failed to look up volunteer", "error", err)
		apierror.WriteError(w, apierror.Internal("internal server error", err))
		return
	}
	if vol == nil {
		apierror.WriteError(w, apierror.NotFound("volunteer", pubKeyB64))
		return
	}

	// Check for verified challenge.
	verified := h.hasVerifiedChallenge(r.Context(), pubKeyBytes)

	// Credit stats are sourced from the authoritative append-only credit ledger
	// (the same ComputeVolunteerBreakdown that backs GetMyContribution and the
	// volunteer stats endpoint), so every credit surface reports one consistent
	// number that reflects the configured credit-per-work-unit rather than a raw
	// row count.
	bd, err := credit.ComputeVolunteerBreakdown(r.Context(), h.pool, vol.ID)
	if err != nil {
		l.Error("failed to compute credit breakdown", "error", err)
		bd = &credit.VolunteerBreakdown{}
	}

	resp := infoResponse{
		PublicKey:            pubKeyB64,
		VolunteerID:          vol.ID.String(),
		Verified:             verified,
		DisplayName:          vol.DisplayName,
		TotalCredit:          bd.TotalCredit,
		ProjectsContributing: len(bd.ByLeaf),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// --- Helpers ---

func (h *Handler) hasVerifiedChallenge(ctx context.Context, publicKey []byte) bool {
	var exists bool
	_ = h.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM identity_challenges WHERE public_key = $1 AND verified = true)",
		publicKey,
	).Scan(&exists)
	return exists
}
