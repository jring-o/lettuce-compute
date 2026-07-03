package identity

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/atproto"
	"github.com/lettuce-compute/infrastructure/internal/config"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// BindHandler serves POST /api/v1/identity/bind-did: a volunteer proves it controls
// an ATProto decentralized identifier (DID) by pointing the head at a key-authorization
// record published in the DID's Personal Data Server (PDS) repo. The head resolves the
// DID, fetches that record, verifies it authorizes the caller's operational Ed25519 key,
// and — only then — stamps the (optional) identity binding onto the volunteer row.
//
// The caller is authenticated by the same Ed25519 HTTP scheme the browser/WASM volunteer
// endpoints use; the router wraps this handler with that middleware and passes the
// AUTHENTICATED public key in explicitly, so the record must authorize the very key that
// signed the request — a volunteer cannot bind a DID to a key it does not hold.
type BindHandler struct {
	client        *atproto.Client
	volunteerRepo volunteer.Repository
	cfg           config.HeadConfig
	logger        *slog.Logger
}

// NewBindHandler builds the bind-DID handler. client must be non-nil; the router only
// constructs this handler when DID binding is enabled (which is also when the atproto
// client is constructed).
func NewBindHandler(client *atproto.Client, volunteerRepo volunteer.Repository, cfg config.HeadConfig, logger *slog.Logger) *BindHandler {
	return &BindHandler{
		client:        client,
		volunteerRepo: volunteerRepo,
		cfg:           cfg,
		logger:        logger,
	}
}

type bindDIDRequest struct {
	DID       string `json:"did"`
	RecordURI string `json:"record_uri"`
}

type bindDIDResponse struct {
	DID           string `json:"did"`
	BindingStatus string `json:"binding_status"`
	BoundAt       string `json:"bound_at"`
}

// Handle verifies and persists a DID binding for the AUTHENTICATED device key. The
// authenticated Ed25519 public key is supplied by the router's Ed25519 auth wrapper;
// this handler never trusts a key from the request body. Nothing is persisted unless
// every verification step passes.
func (h *BindHandler) Handle(w http.ResponseWriter, r *http.Request, authedKey ed25519.PublicKey) {
	l := logging.LoggerFromContext(r.Context(), h.logger)
	ctx := r.Context()

	// Defense in depth: the route is only registered when binding is enabled, but a
	// misconfiguration must never expose the endpoint. A disabled head has no such
	// endpoint, so report it as not found.
	if !h.cfg.DIDBindingEnabled {
		apierror.WriteError(w, apierror.NotFound("endpoint", r.URL.Path))
		return
	}

	var req bindDIDRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}
	if req.DID == "" || req.RecordURI == "" {
		apierror.WriteError(w, apierror.ValidationError("did and record_uri are required", nil))
		return
	}

	// 1. Parse the AT-URI and require it to name the same DID and the head's configured
	//    key-authorization collection. A caller cannot point us at an arbitrary record.
	uriDID, collection, rkey, err := atproto.ParseATURI(req.RecordURI)
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid record_uri: "+err.Error(), nil))
		return
	}
	if uriDID != req.DID {
		apierror.WriteError(w, apierror.ValidationError("record_uri authority does not match did", nil))
		return
	}
	if want := h.cfg.EffectiveDIDBindingCollection(); collection != want {
		apierror.WriteError(w, apierror.ValidationError("record_uri collection is not the head's key-authorization collection", nil))
		return
	}

	// 2. Resolve the DID to its DID document (which carries the PDS endpoint).
	ident, err := h.client.ResolveDID(ctx, req.DID)
	if err != nil {
		if errors.Is(err, atproto.ErrDIDNotFound) {
			apierror.WriteError(w, apierror.ValidationError("did could not be resolved", nil))
			return
		}
		l.Warn("bind-did: DID resolution failed", "did", req.DID, "error", err)
		apierror.WriteError(w, upstreamError("DID_RESOLVE_FAILED", "failed to resolve did; try again later"))
		return
	}

	// 3. Fetch the key-authorization record from the DID's PDS.
	rec, err := h.client.GetRecord(ctx, ident.PDSEndpoint, req.DID, collection, rkey)
	if err != nil {
		if errors.Is(err, atproto.ErrRecordNotFound) {
			apierror.WriteError(w, apierror.ValidationError("key-authorization record not found in the PDS", nil))
			return
		}
		l.Warn("bind-did: key-authorization record fetch failed", "did", req.DID, "error", err)
		apierror.WriteError(w, upstreamError("RECORD_FETCH_FAILED", "failed to fetch key-authorization record; try again later"))
		return
	}

	// 4. Unmarshal and verify the record authorizes THIS device key, right now.
	var kar atproto.KeyAuthorizationRecord
	if err := json.Unmarshal(rec.Value, &kar); err != nil {
		apierror.WriteError(w, apierror.ValidationError("key-authorization record is malformed", nil))
		return
	}
	if err := atproto.VerifyKeyAuthorization(&kar, req.DID, authedKey, time.Now().UTC()); err != nil {
		apierror.WriteError(w, keyAuthorizationError(err))
		return
	}

	// 5. The verified record authorizes the authenticated key; find that volunteer.
	vol, err := h.volunteerRepo.GetByPublicKey(ctx, authedKey)
	if err != nil {
		l.Error("bind-did: volunteer lookup failed", "error", err)
		apierror.WriteError(w, apierror.Internal("internal server error", err))
		return
	}
	if vol == nil {
		apierror.WriteError(w, apierror.NotFound("volunteer", "register this device key before binding a DID"))
		return
	}

	now := types.Now()

	// 6. An identity move (this key was already bound to a DIFFERENT DID) is allowed but
	//    inherits nothing quietly: freeze the binding for the rotation cool-down and log
	//    a WARN anomaly. The freeze deadline is recorded now; its trust-gate enforcement
	//    consumer lands in a later phase (see did_recheck.go).
	if vol.DID != nil && *vol.DID != req.DID {
		freezeUntil := now.Add(time.Duration(h.cfg.EffectiveDIDRotationFreezeHours()) * time.Hour)
		if ferr := h.volunteerRepo.SetDIDFrozenUntil(ctx, vol.ID, freezeUntil); ferr != nil {
			l.Error("bind-did: failed to freeze binding on identity move", "error", ferr)
			apierror.WriteError(w, apierror.Internal("internal server error", ferr))
			return
		}
		l.Warn("bind-did: volunteer identity move; froze binding",
			"volunteer_id", vol.ID.String(),
			"old_did", *vol.DID,
			"new_did", req.DID,
			"frozen_until", types.FormatTimestamp(freezeUntil))
	}

	// 7. Persist the verified binding (status OK, CID pinned, failure counter cleared).
	if err := h.volunteerRepo.SetDIDBinding(ctx, vol.ID, req.DID, req.RecordURI, rec.CID, now); err != nil {
		l.Error("bind-did: failed to persist binding", "error", err)
		apierror.WriteError(w, apierror.Internal("internal server error", err))
		return
	}

	resp := bindDIDResponse{
		DID:           req.DID,
		BindingStatus: volunteer.DIDBindingStatusOK,
		BoundAt:       types.FormatTimestamp(now),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// keyAuthorizationError maps a VerifyKeyAuthorization sentinel to a distinct client
// error. A DID or expiresAt problem is the caller's malformed input (400); a key or
// signature mismatch is an authorization failure (403). No state is persisted before
// this point, so every branch is a clean rejection.
func keyAuthorizationError(err error) *apierror.APIError {
	switch {
	case errors.Is(err, atproto.ErrDIDMismatch):
		return apierror.ValidationError("key-authorization record is for a different did", nil)
	case errors.Is(err, atproto.ErrInvalidExpiresAt):
		return apierror.ValidationError("key-authorization record has an invalid expiresAt", nil)
	case errors.Is(err, atproto.ErrKeyMismatch):
		return apierror.Forbidden("key-authorization record does not authorize this device key")
	case errors.Is(err, atproto.ErrExpired):
		return apierror.Forbidden("key-authorization record has expired")
	case errors.Is(err, atproto.ErrBadSignature):
		return apierror.Forbidden("key-authorization record signature is invalid")
	default:
		return apierror.ValidationError("key-authorization record failed verification", nil)
	}
}

// upstreamError builds a 502 for a transient DID-resolver or PDS failure — the request
// was well-formed but an upstream the head does not control could not be reached, so the
// client should retry rather than treat its input as invalid.
func upstreamError(code, message string) *apierror.APIError {
	return &apierror.APIError{
		Code:       code,
		Message:    message,
		HTTPStatus: http.StatusBadGateway,
	}
}
