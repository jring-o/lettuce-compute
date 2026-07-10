package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/audit"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// This file adapts the concrete repositories onto the enforcement worker's narrow seams
// (design doc §9.3). The adapters live in package main — the one place that may import
// every involved package — mirroring how the adjudicator closure is wired.

// creditEnforcer adapts the credit repositories onto audit.CreditEnforcer.
type creditEnforcer struct {
	ledger *credit.PgxRepository
	adj    credit.AdjustmentsRepository
	rac    credit.RACAdjuster
	logger *slog.Logger
}

func (c *creditEnforcer) ClawbackEntryForAudit(ctx context.Context, resultID, auditID types.ID, reason string) (*audit.EnforcementAdjustment, error) {
	entry, err := c.ledger.GetByResultID(ctx, resultID)
	if err != nil {
		if isNotFoundAPIError(err) {
			// Cap-suppressed or legacy result: no ledger entry — nothing to claw.
			return nil, nil
		}
		return nil, err
	}
	adj, err := c.adj.ClawbackForAudit(ctx, entry.ID, auditID, reason)
	if err != nil {
		if errors.Is(err, credit.ErrAdjustmentExhausted) {
			// F17: already fully clawed (a prior pass or the operator) — idempotent no-op.
			return nil, nil
		}
		return nil, err
	}
	return toEnforcementAdjustment(adj), nil
}

func (c *creditEnforcer) ClawbackUnmaturedForAudit(ctx context.Context, volunteerID, auditID types.ID, maturationDays int, reason string) ([]*audit.EnforcementAdjustment, error) {
	entryIDs, err := c.adj.ListUnmaturedEntryIDs(ctx, volunteerID, maturationDays)
	if err != nil {
		return nil, err
	}
	var out []*audit.EnforcementAdjustment
	for _, entryID := range entryIDs {
		adj, err := c.adj.ClawbackForAudit(ctx, entryID, auditID, reason)
		if err != nil {
			if errors.Is(err, credit.ErrAdjustmentExhausted) {
				continue // already cancelled — idempotent no-op (F17)
			}
			return out, err
		}
		out = append(out, toEnforcementAdjustment(adj))
	}
	return out, nil
}

func (c *creditEnforcer) ApplyRACAdjustment(ctx context.Context, adjustmentID types.ID) (bool, error) {
	return c.rac.ApplyAdjustment(ctx, adjustmentID)
}

func toEnforcementAdjustment(a *credit.Adjustment) *audit.EnforcementAdjustment {
	return &audit.EnforcementAdjustment{
		ID:          a.ID,
		VolunteerID: a.VolunteerID,
		LeafID:      a.LeafID,
		Magnitude:   -a.Amount,
	}
}

// isNotFoundAPIError reports whether err is an apierror 404 (the GetByResultID
// "result has no ledger entry" signal — cap-suppressed or legacy grants).
func isNotFoundAPIError(err error) bool {
	var apiErr *apierror.APIError
	return errors.As(err, &apiErr) && apiErr.HTTPStatus == http.StatusNotFound
}

// fraudSetLoader adapts the result repository onto audit.FraudSetLoader.
type fraudSetLoader struct {
	results result.Repository
}

func (f *fraudSetLoader) LoadFraudSet(ctx context.Context, workUnitID types.ID) ([]audit.FraudResult, error) {
	all, err := f.results.ListByWorkUnit(ctx, workUnitID)
	if err != nil {
		return nil, err
	}
	var out []audit.FraudResult
	for _, r := range all {
		if r.ValidationStatus != result.ValidationAgreed {
			continue
		}
		out = append(out, audit.FraudResult{
			ResultID:    r.ID,
			VolunteerID: r.VolunteerID,
			// Submit-time stamped subject with the vol:<uuid> sentinel fallback for
			// pre-00013 legacy rows — the same rule acceptance uses.
			Subject: transition.SubjectForResult(r),
		})
	}
	return out, nil
}

func (f *fraudSetLoader) FlipToDisagreed(ctx context.Context, resultIDs []types.ID) error {
	return f.results.BatchUpdateValidationStatus(ctx, resultIDs, result.ValidationDisagreed)
}

// enforcementUnitLocker adapts the transition Locker onto audit.UnitLocker via the
// exported same-key helper, so the enforcement pass serializes against the transitioner.
type enforcementUnitLocker struct {
	locker transition.Locker
}

func (l *enforcementUnitLocker) WithUnitLock(ctx context.Context, workUnitID types.ID, fn func() error) error {
	return transition.WithUnitLock(ctx, l.locker, workUnitID, fn)
}

// newEnforcementBudgetResolver computes the §9.7 refund inputs for a demoted unit: a
// FULL fresh dead-letter budget (leaf override when set, else the derived target+margin
// default — deliberately IGNORING the unit's own max_total_copies, which a prior
// demotion may have materialized as an absolute number), and the resolved error ceiling
// (unit else leaf, 0 = unlimited stays 0).
func newEnforcementBudgetResolver(leafRepo leaf.Repository, trustPolicy transition.TrustPolicy) func(ctx context.Context, wu *workunit.WorkUnit) (int, int, error) {
	return func(ctx context.Context, wu *workunit.WorkUnit) (int, int, error) {
		lf, err := leafRepo.GetByID(ctx, wu.LeafID)
		if err != nil {
			return 0, 0, err
		}
		policy := transition.ResolvePolicyWithTrust(lf, wu, trustPolicy)
		freshTotal := lf.ValidationConfig.MaxTotalCopies
		if freshTotal <= 0 {
			// The derived default a brand-new unit would get: target + retry margin
			// (EffectiveMaxTotalCopies on a zeroed unit ignores any materialized
			// per-unit override).
			freshTotal = (&workunit.WorkUnit{}).EffectiveMaxTotalCopies(policy.TargetCopies)
		}
		return freshTotal, policy.MaxErrorCopies, nil
	}
}
