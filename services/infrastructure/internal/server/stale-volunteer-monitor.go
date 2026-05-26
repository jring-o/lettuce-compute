package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// StaleVolunteerMonitor periodically marks stale volunteers as inactive.
type StaleVolunteerMonitor struct {
	volunteerRepo volunteer.Repository
	logger        *slog.Logger
	scanInterval  time.Duration
	threshold     time.Duration
}

// NewStaleVolunteerMonitor creates a new StaleVolunteerMonitor.
func NewStaleVolunteerMonitor(volunteerRepo volunteer.Repository, logger *slog.Logger) *StaleVolunteerMonitor {
	return &StaleVolunteerMonitor{
		volunteerRepo: volunteerRepo,
		logger:        logger,
		scanInterval:  60 * time.Second,
		threshold:     30 * time.Minute,
	}
}

// Start begins the background monitoring loop. Returns when ctx is cancelled.
func (m *StaleVolunteerMonitor) Start(ctx context.Context) {
	m.logger.Info("stale volunteer monitor starting", "scan_interval", m.scanInterval, "threshold", m.threshold)
	ticker := time.NewTicker(m.scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("stale volunteer monitor stopping")
			return
		case <-ticker.C:
			count, err := m.volunteerRepo.MarkInactiveOlderThan(ctx, m.threshold)
			if err != nil {
				m.logger.Error("failed to mark stale volunteers inactive", "error", err)
				continue
			}
			if count > 0 {
				m.logger.Info("marked stale volunteers inactive", "count", count)
			}
		}
	}
}
