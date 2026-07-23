package leaf

import (
	"context"
	"log/slog"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// WarnActiveConfigFootguns sweeps every ACTIVE leaf once and WARN-logs each one whose
// validation config would be refused by the CURRENT rules (PB-36). Configure-time gates
// only reach configs written after the gate exists; a leaf activated before a rule was
// added keeps its footgun invisibly — the Phase 3 campaign's unscoped NUMERIC_TOLERANCE
// leaves being the proven case (honest results nondeterministically rejected on runtime
// metadata). This scan is ADVISORY ONLY: it never mutates a leaf and never blocks boot —
// the enforcement moments stay the configure PUT and any transition into ACTIVE, both of
// which re-run the same validation. Run it once at startup (leader-gated with the other
// background jobs); a clean fleet logs nothing.
func WarnActiveConfigFootguns(ctx context.Context, repo Repository, logger *slog.Logger) {
	state := StateActive
	filters := LeafListFilters{State: &state}
	page := types.PaginationRequest{PageSize: 200}
	flagged := 0
	for {
		leafs, pagination, err := repo.List(ctx, filters, page)
		if err != nil {
			logger.Warn("active-leaf config scan aborted", "error", err)
			return
		}
		for _, p := range leafs {
			if verr := ValidateValidationConfig(&p.ValidationConfig); verr != nil {
				flagged++
				logger.Warn("ACTIVE leaf's validation config fails current validation rules; it predates a rule added since it was activated — fix via a config PUT (the same error will refuse a pause/resume until then)",
					"leaf_id", p.ID,
					"leaf_name", p.Name,
					"error", verr.Message)
			}
		}
		if !pagination.HasMore {
			break
		}
		page.Cursor = pagination.NextCursor
	}
	if flagged > 0 {
		logger.Warn("active-leaf config scan complete", "flagged_leafs", flagged)
	}
}
