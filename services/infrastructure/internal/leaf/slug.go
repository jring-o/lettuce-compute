package leaf

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

var (
	nonAlphanumeric    = regexp.MustCompile(`[^a-z0-9]+`)
	leadingTrailingDash = regexp.MustCompile(`^-+|-+$`)
)

// GenerateSlug creates a URL-safe slug from a leaf name.
// Lowercase, replace non-alphanumeric with hyphens, collapse consecutive hyphens,
// trim leading/trailing hyphens, truncate to 100 chars.
// Returns "untitled" if the result would be empty.
func GenerateSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = nonAlphanumeric.ReplaceAllString(s, "-")
	s = leadingTrailingDash.ReplaceAllString(s, "")

	if len(s) > 100 {
		s = s[:100]
		// Don't leave a trailing hyphen after truncation.
		s = strings.TrimRight(s, "-")
	}

	if s == "" {
		return "untitled"
	}
	return s
}

// GenerateUniqueSlug generates a slug and resolves collisions within the same creator
// by appending -2, -3, etc.
func GenerateUniqueSlug(ctx context.Context, pool *pgxpool.Pool, name string, creatorID *types.ID) (string, error) {
	base := GenerateSlug(name)

	// Check if base slug is available.
	exists, err := slugExists(ctx, pool, base, creatorID)
	if err != nil {
		return "", err
	}
	if !exists {
		return base, nil
	}

	// Append -2, -3, etc. until we find an available slug.
	for i := 2; i <= 1000; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		exists, err := slugExists(ctx, pool, candidate, creatorID)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("leaf: could not generate unique slug for %q after 1000 attempts", name)
}

// slugExists checks whether a slug already exists for the given creator.
func slugExists(ctx context.Context, pool *pgxpool.Pool, slug string, creatorID *types.ID) (bool, error) {
	var exists bool
	var err error

	if creatorID != nil {
		err = pool.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM leafs WHERE slug = $1 AND creator_id = $2)",
			slug, *creatorID,
		).Scan(&exists)
	} else {
		err = pool.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM leafs WHERE slug = $1 AND creator_id IS NULL)",
			slug,
		).Scan(&exists)
	}

	if err != nil {
		return false, fmt.Errorf("leaf: slug existence check failed: %w", err)
	}
	return exists, nil
}
