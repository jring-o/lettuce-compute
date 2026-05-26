package attestation

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Handler handles attestation HTTP requests.
type Handler struct {
	repo      Repository
	publicKey ed25519.PublicKey
	logger    *slog.Logger
}

// NewHandler creates a new attestation Handler.
func NewHandler(repo Repository, publicKey ed25519.PublicKey, logger *slog.Logger) *Handler {
	return &Handler{
		repo:      repo,
		publicKey: publicKey,
		logger:    logger,
	}
}

// RegisterRoutes registers attestation routes on the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/attestations", h.handleList)
}

// attestationWireJSON is the JSON wire format for a single attestation.
type attestationWireJSON struct {
	ID                   types.ID       `json:"id"`
	LeafID            types.ID       `json:"leaf_id"`
	VolunteerPublicKey   string         `json:"volunteer_public_key"`
	WorkUnitID           types.ID       `json:"work_unit_id"`
	RawMetrics           map[string]any `json:"raw_metrics"`
	ValidationOutcome    string         `json:"validation_outcome"`
	CreditAmount         float64        `json:"credit_amount"`
	AttestationTimestamp string         `json:"attestation_timestamp"`
	Signature            string         `json:"signature"`
}

// attestationListResponse is the JSON response for GET /api/v1/attestations.
type attestationListResponse struct {
	Data            []attestationWireJSON    `json:"data"`
	Pagination      types.PaginationResponse `json:"pagination"`
	SigningPublicKey string                  `json:"signing_public_key"`
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)
	q := r.URL.Query()

	// Parse pagination.
	page := types.PaginationRequest{
		Cursor: q.Get("cursor"),
	}
	if limitStr := q.Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid limit: must be an integer", nil))
			return
		}
		page.PageSize = limit
	}

	// Build filters.
	var filters ListFilters

	if leafIDStr := q.Get("leaf_id"); leafIDStr != "" {
		leafID, err := types.ParseID(leafIDStr)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
			return
		}
		filters.LeafID = &leafID
	}

	if volunteerKeyStr := q.Get("volunteer_public_key"); volunteerKeyStr != "" {
		pubKeyBytes, err := base64.RawURLEncoding.DecodeString(volunteerKeyStr)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid volunteer_public_key: must be base64url-encoded", nil))
			return
		}
		filters.VolunteerPublicKey = pubKeyBytes
	}

	if fromStr := q.Get("from"); fromStr != "" {
		filters.From = &fromStr
	}
	if toStr := q.Get("to"); toStr != "" {
		filters.To = &toStr
	}

	attestations, pagination, err := h.repo.List(r.Context(), filters, page)
	if err != nil {
		l.Error("failed to list attestations", "error", err)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	// Build response.
	data := make([]attestationWireJSON, len(attestations))
	for i, a := range attestations {
		data[i] = attestationWireJSON{
			ID:                   a.ID,
			LeafID:            a.LeafID,
			VolunteerPublicKey:   base64.RawURLEncoding.EncodeToString(a.VolunteerPublicKey),
			WorkUnitID:           a.WorkUnitID,
			RawMetrics:           a.RawMetrics,
			ValidationOutcome:    a.ValidationOutcome,
			CreditAmount:         a.CreditAmount,
			AttestationTimestamp: types.FormatTimestamp(a.AttestationTimestamp),
			Signature:            base64.RawURLEncoding.EncodeToString(a.Signature),
		}
	}

	resp := attestationListResponse{
		Data:            data,
		Pagination:      pagination,
		SigningPublicKey: base64.RawURLEncoding.EncodeToString(h.publicKey),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
