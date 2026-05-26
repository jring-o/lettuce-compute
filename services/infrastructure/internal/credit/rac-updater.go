package credit

import (
	"context"
	"log/slog"
	"time"
)

// RACUpdater runs periodic RAC decay on all volunteer_rac rows.
type RACUpdater struct {
	racRepo  RACRepository
	logger   *slog.Logger
	interval time.Duration
}

// NewRACUpdater creates a new RACUpdater that runs decay every hour.
func NewRACUpdater(racRepo RACRepository, logger *slog.Logger) *RACUpdater {
	return &RACUpdater{
		racRepo:  racRepo,
		logger:   logger,
		interval: 1 * time.Hour,
	}
}

// Start begins the background decay loop. Returns when ctx is cancelled.
func (u *RACUpdater) Start(ctx context.Context) {
	u.logger.Info("rac updater starting", "interval", u.interval)
	ticker := time.NewTicker(u.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			u.logger.Info("rac updater stopping")
			return
		case <-ticker.C:
			rows, err := u.racRepo.DecayAll(ctx)
			if err != nil {
				u.logger.Error("rac decay failed", "error", err)
			} else if rows > 0 {
				u.logger.Info("rac decay applied", "rows_affected", rows)
			}
		}
	}
}
