package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
)

const defaultArtifactGCInterval = 1 * time.Hour

// ArtifactVersionGC is a leader-gated singleton sweep that enforces the operator's
// artifact-version retention policy (TODO #38): it deletes superseded versions beyond
// the keep-newest-N bound per leaf. It never deletes a leaf's current version or any
// version pinned by an in-flight work unit (the repo enforces both). keep <= 0
// disables the sweep ("keep all", the default) so nothing is ever auto-deleted unless
// the operator opts in via LETTUCE_ARTIFACT_RETENTION.
type ArtifactVersionGC struct {
	repo     leaf.ArtifactVersionRepository
	keep     int
	interval time.Duration
	logger   *slog.Logger
}

// NewArtifactVersionGC builds the GC. keep comes from
// HeadConfig.EffectiveArtifactRetentionKeep().
func NewArtifactVersionGC(repo leaf.ArtifactVersionRepository, keep int, logger *slog.Logger) *ArtifactVersionGC {
	return &ArtifactVersionGC{
		repo:     repo,
		keep:     keep,
		interval: defaultArtifactGCInterval,
		logger:   logger,
	}
}

// Start runs the sweep once on election and then on a ticker until ctx is cancelled.
// It is a no-op when keep <= 0 (retention "all") or the repo is nil (versioning
// unsupported).
func (g *ArtifactVersionGC) Start(ctx context.Context) {
	if g.keep <= 0 || g.repo == nil {
		g.logger.Info("artifact version GC disabled (retention=all)")
		return
	}
	g.logger.Info("artifact version GC started", "keep_per_leaf", g.keep, "interval", g.interval.String())
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	g.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.sweep(ctx)
		}
	}
}

func (g *ArtifactVersionGC) sweep(ctx context.Context) {
	deleted, err := g.repo.PruneAllVersions(ctx, g.keep)
	if err != nil {
		g.logger.Warn("artifact version GC sweep failed", "error", err)
		return
	}
	if deleted > 0 {
		g.logger.Info("artifact version GC pruned superseded versions", "deleted", deleted, "keep_per_leaf", g.keep)
	}
}
