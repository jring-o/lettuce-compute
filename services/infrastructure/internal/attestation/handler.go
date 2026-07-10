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

// verificationRecipePath points consumers at the public, self-contained recipe for verifying
// attestation signatures without trusting the head (see design §8.7). Served in the list
// envelope so the bulk read surface is in-band self-describing.
const verificationRecipePath = "guides/attestation-verification.md"

// verifyMaxBody caps the verify request body: a single {"attestation_id": "<uuid>"} is well
// under 1 KiB, and the endpoint is unauthenticated (browser-volunteer-handlers.go:194 precedent).
const verifyMaxBody = 1024

// signedFieldsV1 lists, in canonical (signed) order, the keys covered by a v1 attestation's
// Ed25519 signature — mirrors canonicalV1 in signer.go. unverified_volunteer_metrics is
// deliberately absent: volunteer metrics are never signed (BG-06).
var signedFieldsV1 = []string{
	"attestation_timestamp",
	"credit_amount",
	"leaf_id",
	"validation_outcome",
	"volunteer_public_key",
	"work_unit_id",
}

// signedFieldsV2Grant mirrors canonicalV2Grant's key order.
var signedFieldsV2Grant = []string{
	"attestation_timestamp",
	"context",
	"credit_amount",
	"leaf_id",
	"output_checksum",
	"policy_version",
	"quorum_descriptor",
	"result_id",
	"schema_version",
	"validation_outcome",
	"volunteer_public_key",
	"work_unit_id",
}

// signedFieldsV2Revocation mirrors canonicalV2Revocation's key order.
var signedFieldsV2Revocation = []string{
	"adjustment_id",
	"attestation_timestamp",
	"context",
	"credit_amount",
	"leaf_id",
	"reason",
	"result_id",
	"revokes_attestation_id",
	"schema_version",
	"volunteer_public_key",
	"work_unit_id",
}

// signedFieldsBySchemaVersion maps each canonical form (keyed by schema version, with the v2
// revocation form under "2-revocation") to its exact signed key list, in signed order.
func signedFieldsBySchemaVersion() map[string][]string {
	return map[string][]string{
		"1":            signedFieldsV1,
		"2":            signedFieldsV2Grant,
		"2-revocation": signedFieldsV2Revocation,
	}
}

// signedFieldsFor returns the signed key list for a specific row, dispatching exactly as
// CanonicalJSON does (schema version, then outcome for the revocation form).
func signedFieldsFor(att *Attestation) []string {
	switch {
	case att.SchemaVersion <= SchemaVersionV1:
		return signedFieldsV1
	case att.ValidationOutcome == OutcomeRevoked:
		return signedFieldsV2Revocation
	default:
		return signedFieldsV2Grant
	}
}

// Handler handles attestation HTTP requests.
type Handler struct {
	repo      Reader
	publicKey ed25519.PublicKey
	logger    *slog.Logger
}

// NewHandler creates a new attestation Handler.
func NewHandler(repo Reader, publicKey ed25519.PublicKey, logger *slog.Logger) *Handler {
	return &Handler{
		repo:      repo,
		publicKey: publicKey,
		logger:    logger,
	}
}

// RegisterRoutes registers attestation routes on the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/attestations", h.handleList)
	mux.HandleFunc("POST /api/v1/attestations/verify", h.handleVerify)
}

// attestationWireJSON is the JSON wire format for a single attestation. Volunteer-reported
// metrics are served under unverified_volunteer_metrics (never a signed field — BG-06a item 1);
// consumers verifying a v2 row MUST use credit_amount_canonical (the exact signed string), not
// the float credit_amount (kept for display).
type attestationWireJSON struct {
	ID                         types.ID          `json:"id"`
	SchemaVersion              int               `json:"schema_version"`
	LeafID                     types.ID          `json:"leaf_id"`
	VolunteerPublicKey         string            `json:"volunteer_public_key"`
	WorkUnitID                 types.ID          `json:"work_unit_id"`
	ResultID                   *types.ID         `json:"result_id,omitempty"`
	OutputChecksum             *string           `json:"output_checksum,omitempty"`
	QuorumDescriptor           *QuorumDescriptor `json:"quorum_descriptor,omitempty"`
	PolicyVersion              *int              `json:"policy_version,omitempty"`
	RevokesAttestationID       *types.ID         `json:"revokes_attestation_id,omitempty"`
	AdjustmentID               *types.ID         `json:"adjustment_id,omitempty"`
	Reason                     *string           `json:"reason,omitempty"`
	UnverifiedVolunteerMetrics map[string]any    `json:"unverified_volunteer_metrics"`
	ValidationOutcome          string            `json:"validation_outcome"`
	CreditAmount               float64           `json:"credit_amount"`
	CreditAmountCanonical      string            `json:"credit_amount_canonical,omitempty"`
	AttestationTimestamp       string            `json:"attestation_timestamp"`
	Signature                  string            `json:"signature"`
}

// attestationListResponse is the JSON response for GET /api/v1/attestations. The envelope
// carries the signed-field manifest (per canonical form) and the verification-recipe pointer,
// so the bulk read surface is self-describing without a per-row verify call (design §8.6).
type attestationListResponse struct {
	Data                        []attestationWireJSON    `json:"data"`
	Pagination                  types.PaginationResponse `json:"pagination"`
	SigningPublicKey            string                   `json:"signing_public_key"`
	SignedFieldsBySchemaVersion map[string][]string      `json:"signed_fields_by_schema_version"`
	VerificationRecipe          string                   `json:"verification_recipe"`
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
			ID:                         a.ID,
			SchemaVersion:              a.SchemaVersion,
			LeafID:                     a.LeafID,
			VolunteerPublicKey:         base64.RawURLEncoding.EncodeToString(a.VolunteerPublicKey),
			WorkUnitID:                 a.WorkUnitID,
			ResultID:                   a.ResultID,
			OutputChecksum:             a.OutputChecksum,
			QuorumDescriptor:           a.QuorumDescriptor,
			PolicyVersion:              a.PolicyVersion,
			RevokesAttestationID:       a.RevokesAttestationID,
			AdjustmentID:               a.AdjustmentID,
			Reason:                     a.Reason,
			UnverifiedVolunteerMetrics: a.RawMetrics,
			ValidationOutcome:          a.ValidationOutcome,
			CreditAmount:               a.CreditAmount,
			CreditAmountCanonical:      a.CreditAmountCanonical,
			AttestationTimestamp:       types.FormatTimestamp(a.AttestationTimestamp),
			Signature:                  base64.RawURLEncoding.EncodeToString(a.Signature),
		}
	}

	resp := attestationListResponse{
		Data:                        data,
		Pagination:                  pagination,
		SigningPublicKey:            base64.RawURLEncoding.EncodeToString(h.publicKey),
		SignedFieldsBySchemaVersion: signedFieldsBySchemaVersion(),
		VerificationRecipe:          verificationRecipePath,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// verifyRequest is the POST /api/v1/attestations/verify request body.
type verifyRequest struct {
	AttestationID string `json:"attestation_id"`
}

// revocationSummaryJSON summarizes one revocation attestation clawing back a grant. Consumers
// net a grant's live credit as credit_amount − Σ revocations.credit_amount_canonical.
type revocationSummaryJSON struct {
	AttestationID         types.ID `json:"attestation_id"`
	CreditAmount          float64  `json:"credit_amount"`
	CreditAmountCanonical string   `json:"credit_amount_canonical"`
	AttestationTimestamp  string   `json:"attestation_timestamp"`
}

// verifyResponse is the POST /api/v1/attestations/verify 200 body. signature_valid is an
// ANSWER, not an error condition: an invalid signature (tampered or a malformed stored row)
// is still a 200 (design §8.5).
type verifyResponse struct {
	AttestationID    types.ID                `json:"attestation_id"`
	SchemaVersion    int                     `json:"schema_version"`
	Kind             string                  `json:"kind"`
	SignatureValid   bool                    `json:"signature_valid"`
	SignedFields     []string                `json:"signed_fields"`
	CanonicalPayload string                  `json:"canonical_payload"`
	Error            string                  `json:"error,omitempty"`
	Revocations      []revocationSummaryJSON `json:"revocations"`
	SigningPublicKey string                  `json:"signing_public_key"`
}

// handleVerify verifies a single attestation by id and reports the signed bytes, the signed
// field set for its canonical form, and (for grants) its revocation chain. Verification is
// dispatched by the row's schema_version and outcome (the same dispatch CanonicalJSON uses),
// so v1 rows verify under the frozen v1 recipe and v2 rows under §8.2/§8.4.
func (h *Handler) handleVerify(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	r.Body = http.MaxBytesReader(w, r.Body, verifyMaxBody)

	var req verifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}

	id, err := types.ParseID(req.AttestationID)
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid attestation_id: must be a valid UUID", nil))
		return
	}

	att, err := h.repo.GetByID(r.Context(), id)
	if err != nil {
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	kind := "grant"
	if att.ValidationOutcome == OutcomeRevoked {
		kind = "revocation"
	}

	resp := verifyResponse{
		AttestationID:    att.ID,
		SchemaVersion:    att.SchemaVersion,
		Kind:             kind,
		SignedFields:     signedFieldsFor(att),
		SigningPublicKey: base64.RawURLEncoding.EncodeToString(h.publicKey),
		Revocations:      []revocationSummaryJSON{},
	}

	// Rebuild the canonical bytes and verify. If the stored row is malformed (a signed column
	// is NULL/invalid), CanonicalJSON errors — report that honestly as an unverifiable answer
	// (signature_valid:false + an error string), never as success (§8.5).
	canonicalBytes, canonErr := CanonicalJSON(att)
	if canonErr != nil {
		resp.CanonicalPayload = ""
		resp.SignatureValid = false
		resp.Error = canonErr.Error()
	} else {
		resp.CanonicalPayload = string(canonicalBytes)
		resp.SignatureValid = VerifyAttestation(h.publicKey, att)
	}

	// Revocations belong to grants only; a grant lists every revocation against it (partial
	// clawbacks produce one row each), empty-but-non-null when there are none.
	if kind == "grant" {
		revs, revErr := h.repo.ListRevocationsOf(r.Context(), att.ID)
		if revErr != nil {
			l.Error("failed to list revocations", "error", revErr, "attestation_id", att.ID)
			apierror.WriteError(w, apierror.FromError(revErr))
			return
		}
		summaries := make([]revocationSummaryJSON, len(revs))
		for i, rev := range revs {
			summaries[i] = revocationSummaryJSON{
				AttestationID:         rev.ID,
				CreditAmount:          rev.CreditAmount,
				CreditAmountCanonical: rev.CreditAmountCanonical,
				AttestationTimestamp:  types.FormatTimestamp(rev.AttestationTimestamp),
			}
		}
		resp.Revocations = summaries
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
