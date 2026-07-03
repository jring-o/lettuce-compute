package identity

// did_recheck.go implements the DID-binding re-check worker.
//
// A DID binding is optional identity metadata a volunteer attaches by publishing a
// key-authorization record in its ATProto Personal Data Server (PDS). The binding is
// only as trustworthy as the record behind it, and that record can disappear or change
// at any time: a volunteer may DELETE it to repudiate the binding, the account may be
// deactivated or taken down, or the DID's signing key may be rotated. A one-time check
// at bind time therefore is not enough — a stamped binding could outlive the record that
// justified it.
//
// This worker closes that gap. On a leader-only ticker it re-verifies each binding whose
// last check is older than the trust TTL, and REVOKES any binding whose record no longer
// authorizes the key. The worst-case latency between a volunteer revoking its record and
// the head observing it is DIDRecheckTTLSeconds + DIDRecheckIntervalSeconds (the TTL plus
// one sweep): that latency is a SECURITY PARAMETER, not merely a performance knob — it
// bounds how long a repudiated identity keeps its binding.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/atproto"
	"github.com/lettuce-compute/infrastructure/internal/config"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// didRecheckBatchLimit caps how many overdue bindings one sweep processes, bounding the
// resolver/PDS traffic and DB work a single tick can generate. Bindings not reached this
// sweep are simply picked up (oldest-checked first) on the next one.
const didRecheckBatchLimit = 100

// DIDRecheckWorker is a leader-gated singleton that re-verifies due DID bindings on a
// TTL and revokes those whose authorization record is gone or repudiated.
type DIDRecheckWorker struct {
	client        *atproto.Client
	volunteerRepo volunteer.Repository
	cfg           config.HeadConfig
	logger        *slog.Logger

	interval   time.Duration
	ttl        time.Duration
	batchLimit int
}

// NewDIDRecheckWorker builds the re-check worker. The interval and TTL come from the DID
// config knobs (their effective values); client must be non-nil (the caller constructs
// it only when DID binding is enabled).
func NewDIDRecheckWorker(client *atproto.Client, volunteerRepo volunteer.Repository, cfg config.HeadConfig, logger *slog.Logger) *DIDRecheckWorker {
	return &DIDRecheckWorker{
		client:        client,
		volunteerRepo: volunteerRepo,
		cfg:           cfg,
		logger:        logger,
		interval:      time.Duration(cfg.EffectiveDIDRecheckIntervalSeconds()) * time.Second,
		ttl:           time.Duration(cfg.EffectiveDIDRecheckTTLSeconds()) * time.Second,
		batchLimit:    didRecheckBatchLimit,
	}
}

// Start runs one sweep immediately on election, then on the interval ticker until ctx is
// cancelled (leadership lost or head shutdown). It matches the artifact-GC pattern.
func (w *DIDRecheckWorker) Start(ctx context.Context) {
	w.logger.Info("DID recheck worker started",
		"interval", w.interval.String(), "trust_ttl", w.ttl.String(), "batch_limit", w.batchLimit)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	w.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("DID recheck worker stopping")
			return
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

// sweep re-checks every binding whose last check predates now - ttl, up to the batch
// limit. A per-row error never aborts the batch.
func (w *DIDRecheckWorker) sweep(ctx context.Context) {
	checkedBefore := types.Now().Add(-w.ttl)
	rows, err := w.volunteerRepo.ListDIDBindingsForRecheck(ctx, checkedBefore, w.batchLimit)
	if err != nil {
		w.logger.Warn("DID recheck: failed to list bindings due for recheck", "error", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	for _, vol := range rows {
		select {
		case <-ctx.Done():
			return
		default:
		}
		w.recheckOne(ctx, vol)
	}
	w.logger.Info("DID recheck sweep complete", "processed", len(rows))
}

// recheckOne re-verifies a single binding and applies exactly one authoritative outcome:
// re-verify (MarkDIDBindingChecked), revoke (RevokeDIDBinding), or record a transient
// failure (MarkDIDBindingCheckFailed). The rotation freeze is a best-effort side effect
// layered on top of a successful re-verification and never changes that outcome.
func (w *DIDRecheckWorker) recheckOne(ctx context.Context, vol *volunteer.Volunteer) {
	now := types.Now()
	l := w.logger.With("volunteer_id", vol.ID.String())

	// ListDIDBindingsForRecheck only returns bound rows; guard defensively regardless.
	if vol.DID == nil || vol.DIDBindingURI == nil {
		l.Warn("DID recheck: row is missing did or binding uri; skipping")
		return
	}
	did := *vol.DID

	// The stored URI was validated at bind time; the stored DID is authoritative, so we
	// only need the collection and rkey from it to fetch the record.
	_, collection, rkey, err := atproto.ParseATURI(*vol.DIDBindingURI)
	if err != nil {
		l.Error("DID recheck: stored binding uri is unparseable", "uri", *vol.DIDBindingURI, "error", err)
		w.markFailed(ctx, l, vol.ID, now)
		return
	}

	// Resolve the DID to its current PDS endpoint.
	ident, err := w.client.ResolveDID(ctx, did)
	if err != nil {
		if errors.Is(err, atproto.ErrDIDNotFound) {
			l.Warn("DID recheck: DID no longer resolves; revoking", "did", did)
			w.revoke(ctx, l, vol.ID, now)
			return
		}
		l.Warn("DID recheck: resolve failed (transient)", "did", did, "error", err)
		w.markFailed(ctx, l, vol.ID, now)
		return
	}

	// Fetch the key-authorization record.
	rec, err := w.client.GetRecord(ctx, ident.PDSEndpoint, did, collection, rkey)
	if err != nil {
		if errors.Is(err, atproto.ErrRecordNotFound) {
			w.handleRecordNotFound(ctx, l, vol.ID, ident.PDSEndpoint, did, now)
			return
		}
		l.Warn("DID recheck: record fetch failed (transient)", "did", did, "error", err)
		w.markFailed(ctx, l, vol.ID, now)
		return
	}

	// The record exists: verify it still authorizes the row's stored device key.
	var kar atproto.KeyAuthorizationRecord
	if err := json.Unmarshal(rec.Value, &kar); err != nil {
		// A record that no longer parses is ambiguous (a lexicon change, a PDS serving
		// garbage), not a definitive repudiation — treat it as a transient failure so it
		// degrades to STALE rather than hard-revoking on a parse quirk.
		l.Warn("DID recheck: key-authorization record is malformed (transient)", "did", did, "error", err)
		w.markFailed(ctx, l, vol.ID, now)
		return
	}
	if err := atproto.VerifyKeyAuthorization(&kar, did, ed25519.PublicKey(vol.PublicKey), now); err != nil {
		if isKeyAuthorizationSentinel(err) {
			// The record no longer authorizes this key (wrong DID/key, expired, or bad
			// signature) — authoritative repudiation.
			l.Warn("DID recheck: record no longer authorizes key; revoking", "did", did, "error", err)
			w.revoke(ctx, l, vol.ID, now)
			return
		}
		l.Warn("DID recheck: verification errored (non-sentinel, transient)", "did", did, "error", err)
		w.markFailed(ctx, l, vol.ID, now)
		return
	}

	// Verified: refresh the pinned CID and clear any STALE state.
	if err := w.volunteerRepo.MarkDIDBindingChecked(ctx, vol.ID, rec.CID, now); err != nil {
		l.Error("DID recheck: failed to mark binding checked", "error", err)
		return
	}

	// Best-effort rotation check, layered on top of the successful re-verification.
	w.checkRotation(ctx, l, vol, did, now)
}

// handleRecordNotFound resolves the ambiguity of a missing record: a live repo means the
// volunteer deleted the record (the designed revocation act); a gone/gated account is
// authoritative too; but if repo liveness cannot be established the record's absence may
// be a transient outage and must not be read as deletion.
func (w *DIDRecheckWorker) handleRecordNotFound(ctx context.Context, l *slog.Logger, id types.ID, pdsURL, did string, now time.Time) {
	alive, err := w.client.RepoAlive(ctx, pdsURL, did)
	switch {
	case err == nil && alive:
		l.Info("DID recheck: authorization record deleted from a live repo; revoking", "did", did)
		w.revoke(ctx, l, id, now)
	case err == nil && !alive:
		l.Info("DID recheck: account gone or gated; revoking", "did", did)
		w.revoke(ctx, l, id, now)
	default:
		l.Warn("DID recheck: record missing but repo liveness uncertain (transient)", "did", did, "error", err)
		w.markFailed(ctx, l, id, now)
	}
}

// checkRotation records a re-bind freeze when the DID's PLC audit log shows a signing-key
// or PDS-endpoint change that post-dates this binding and still falls inside the freeze
// window. It is best-effort and did:plc-only: any failure here is logged and NEVER
// affects the binding outcome decided in recheckOne.
//
// The recorded freeze currently has NO enforcement consumer — the trust gate that will
// read did_frozen_until lands in a later phase. Recording it now is deliberate: it
// captures the rotation event at the moment we observe it, so the future gate has an
// accurate deadline rather than one back-filled after the fact.
//
// DEVIATION FROM THE DESIGN SPEC: the spec freezes a PDS migration "until re-verified"
// but a key rotation for a fixed window. v1 applies the SAME fixed time-window freeze
// (DIDRotationFreezeHours) to both a key change and a PDS change. This is simpler and
// strictly more conservative — a "until re-verified" freeze is never longer than a fixed
// window that also elapses once re-verification succeeds.
func (w *DIDRecheckWorker) checkRotation(ctx context.Context, l *slog.Logger, vol *volunteer.Volunteer, did string, now time.Time) {
	if !strings.HasPrefix(did, "did:plc:") {
		return
	}
	ops, err := w.client.PLCAuditLog(ctx, did)
	if err != nil {
		if !errors.Is(err, atproto.ErrNoAuditLog) {
			l.Debug("DID recheck: PLC audit log fetch failed (best-effort)", "did", did, "error", err)
		}
		return
	}

	// Find the NEWEST operation whose signing key or PDS endpoint differs from its
	// immediate predecessor's — that is the most recent rotation.
	var rotatedAt time.Time
	found := false
	for i := 1; i < len(ops); i++ {
		prev, cur := ops[i-1], ops[i]
		if cur.ATProtoSigningKey != prev.ATProtoSigningKey || cur.PDSEndpoint != prev.PDSEndpoint {
			if !found || cur.CreatedAt.After(rotatedAt) {
				rotatedAt = cur.CreatedAt
				found = true
			}
		}
	}
	if !found {
		return
	}

	// Only a rotation that happened AFTER this binding was created is relevant.
	var boundAt time.Time
	if vol.DIDBoundAt != nil {
		boundAt = *vol.DIDBoundAt
	}
	if !rotatedAt.After(boundAt) {
		return
	}

	freezeUntil := rotatedAt.Add(time.Duration(w.cfg.EffectiveDIDRotationFreezeHours()) * time.Hour)
	// The freeze window has already elapsed: nothing to record.
	if !now.Before(freezeUntil) {
		return
	}
	// An existing freeze already covers this deadline: leave it be.
	if vol.DIDFrozenUntil != nil && !vol.DIDFrozenUntil.Before(freezeUntil) {
		return
	}
	if err := w.volunteerRepo.SetDIDFrozenUntil(ctx, vol.ID, freezeUntil); err != nil {
		l.Error("DID recheck: failed to record rotation freeze (best-effort)", "did", did, "error", err)
		return
	}
	l.Warn("DID recheck: DID rotation detected; froze binding",
		"did", did,
		"rotated_at", types.FormatTimestamp(rotatedAt),
		"frozen_until", types.FormatTimestamp(freezeUntil))
}

func (w *DIDRecheckWorker) markFailed(ctx context.Context, l *slog.Logger, id types.ID, now time.Time) {
	if err := w.volunteerRepo.MarkDIDBindingCheckFailed(ctx, id, now, w.cfg.EffectiveDIDStaleAfterFailures()); err != nil {
		l.Error("DID recheck: failed to record check failure", "error", err)
	}
}

func (w *DIDRecheckWorker) revoke(ctx context.Context, l *slog.Logger, id types.ID, now time.Time) {
	if err := w.volunteerRepo.RevokeDIDBinding(ctx, id, now); err != nil {
		l.Error("DID recheck: failed to revoke binding", "error", err)
	}
}

// isKeyAuthorizationSentinel reports whether err is one of the VerifyKeyAuthorization
// sentinels that authoritatively mean the record no longer authorizes the key (as opposed
// to a transport or parse failure).
func isKeyAuthorizationSentinel(err error) bool {
	return errors.Is(err, atproto.ErrDIDMismatch) ||
		errors.Is(err, atproto.ErrKeyMismatch) ||
		errors.Is(err, atproto.ErrExpired) ||
		errors.Is(err, atproto.ErrBadSignature) ||
		errors.Is(err, atproto.ErrInvalidExpiresAt)
}
