package leaf

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// PgxRepository implements Repository using pgx.
type PgxRepository struct {
	pool *pgxpool.Pool
}

// NewPgxRepository creates a new PgxRepository.
func NewPgxRepository(pool *pgxpool.Pool) *PgxRepository {
	return &PgxRepository{pool: pool}
}

// scanLeaf scans a leaf row into a Leaf struct.
// The column order must match the SELECT used in queries.
func scanLeaf(row pgx.Row) (*Leaf, error) {
	var p Leaf
	err := row.Scan(
		&p.ID,
		&p.Name,
		&p.Slug,
		&p.Description,
		&p.ResearchArea,
		&p.CreatorID,
		&p.CreatorPublicKey,
		&p.State,
		&p.TaskPattern,
		&p.ExecutionConfig,
		&p.ValidationConfig,
		&p.FaultToleranceConfig,
		&p.DataConfig,
		&p.CreditConfig,
		&p.ResourceRequirements,
		&p.IsOngoing,
		&p.Visibility,
		&p.StatsCacheSeconds,
		&p.CreatedAt,
		&p.UpdatedAt,
		&p.CurrentArtifactVersionID,
		&p.GenerationCursor,
	)
	return &p, err
}

// leafColumns is the standard column list for SELECT queries.
// generation_cursor is the durable lazy-generation cursor (migration 00026); it is read
// on every leaf load but written ONLY by UpdateGenerationCursorTx, never by Update — that
// isolation is the point (owner config edits can no longer clobber generation state).
const leafColumns = `id, name, slug, description, research_area,
	creator_id, creator_public_key, state, task_pattern,
	execution_config, validation_config, fault_tolerance_config,
	data_config, credit_config, resource_requirements,
	is_ongoing, visibility, stats_cache_seconds, created_at, updated_at,
	current_artifact_version_id, generation_cursor`

// Create inserts a new leaf. The slug is generated automatically from the name.
// On return, p is populated with the DB-generated id, slug, and timestamps.
func (r *PgxRepository) Create(ctx context.Context, p *Leaf) error {
	slug, err := GenerateUniqueSlug(ctx, r.pool, p.Name, p.CreatorID)
	if err != nil {
		return apierror.Internal("failed to generate slug", err)
	}

	row := r.pool.QueryRow(ctx, `
		INSERT INTO leafs (
			name, slug, description, research_area,
			creator_id, creator_public_key, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, stats_cache_seconds
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11,
			$12, $13, $14,
			$15, $16, $17
		) RETURNING `+leafColumns,
		p.Name, slug, p.Description, p.ResearchArea,
		p.CreatorID, p.CreatorPublicKey, p.State, p.TaskPattern,
		p.ExecutionConfig, p.ValidationConfig, p.FaultToleranceConfig,
		p.DataConfig, p.CreditConfig, p.ResourceRequirements,
		p.IsOngoing, p.Visibility, p.StatsCacheSeconds,
	)

	result, err := scanLeaf(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return apierror.Conflict(
				"leaf slug already exists (concurrent creation)",
				map[string]string{"constraint": pgErr.ConstraintName},
			)
		}
		return apierror.Internal("failed to create leaf", err)
	}
	*p = *result
	return nil
}

// GetByID retrieves a leaf by its UUID.
func (r *PgxRepository) GetByID(ctx context.Context, id types.ID) (*Leaf, error) {
	return GetByIDTx(ctx, r.pool, id)
}

// rowQuerier is the minimal read surface (QueryRow) shared by *pgxpool.Pool and
// pgx.Tx, so a leaf can be read on a caller-supplied transaction connection
// instead of acquiring a second pool connection.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// GetByIDTx retrieves a leaf by its UUID using the supplied querier, which may be
// a *pgxpool.Pool or an open pgx.Tx. Reading the leaf on an already-held tx
// connection (rather than via the pool-backed Repository) lets a handler that is
// mid-transaction avoid acquiring a SECOND pool connection — the cause of the
// pool starvation / self-deadlock under concurrent batched RequestWorkUnit calls,
// where each handler held its reserve-loop tx connection while blocking on a
// fresh getLeaf connection that never freed.
func GetByIDTx(ctx context.Context, db rowQuerier, id types.ID) (*Leaf, error) {
	row := db.QueryRow(ctx,
		"SELECT "+leafColumns+" FROM leafs WHERE id = $1", id)

	p, err := scanLeaf(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("leaf", id.String())
		}
		return nil, apierror.Internal("failed to get leaf", err)
	}
	return p, nil
}

// execQuerier is the minimal write surface (Exec) shared by *pgxpool.Pool and pgx.Tx, so the
// generation cursor advance can run either standalone on the pool (the exhaustion-flag write)
// or inside the batch's own transaction (the atomic per-batch advance) via the same code.
type execQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// UpdateGenerationCursorTx performs the guarded, optimistic advance of a leaf's
// generation_cursor (design §4.8, BG-22c / E1-3) using the supplied executor, which may be a
// *pgxpool.Pool or an open pgx.Tx. The advance succeeds only when the row's CURRENT
// total_generated equals expectedPrevTotalGenerated; otherwise it affects zero rows and returns
// (false, nil) — another writer advanced concurrently (e.g. a leadership-failover overlap), and
// the caller must abort its work rather than double-emit. total_generated is THE guard key: every
// pattern generator's cursor delta MUST advance it monotonically, whatever pattern-specific
// fields the cursor also carries, so no future generator can opt out of the guard by omission.
// The ::bigint cast gives costless headroom over int32. cursor is the full replacement cursor
// JSON (not a delta); this function never merges.
func UpdateGenerationCursorTx(ctx context.Context, db execQuerier, leafID types.ID, cursor []byte, expectedPrevTotalGenerated int64) (bool, error) {
	tag, err := db.Exec(ctx, `
		UPDATE leafs
		SET generation_cursor = $2
		WHERE id = $1
		  AND COALESCE((generation_cursor->>'total_generated')::bigint, 0) = $3`,
		leafID, cursor, expectedPrevTotalGenerated,
	)
	if err != nil {
		return false, apierror.Internal("failed to update generation cursor", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetBySlug retrieves a leaf by slug and creator_id.
// If creatorID is nil, matches leafs with NULL creator_id.
func (r *PgxRepository) GetBySlug(ctx context.Context, slug string, creatorID *types.ID) (*Leaf, error) {
	var row pgx.Row
	if creatorID != nil {
		row = r.pool.QueryRow(ctx,
			"SELECT "+leafColumns+" FROM leafs WHERE slug = $1 AND creator_id = $2",
			slug, *creatorID)
	} else {
		row = r.pool.QueryRow(ctx,
			"SELECT "+leafColumns+" FROM leafs WHERE slug = $1 AND creator_id IS NULL",
			slug)
	}

	p, err := scanLeaf(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("leaf", slug)
		}
		return nil, apierror.Internal("failed to get leaf by slug", err)
	}
	return p, nil
}

// GetBySlugPublic retrieves a leaf by slug without filtering by creator_id.
func (r *PgxRepository) GetBySlugPublic(ctx context.Context, slug string) (*Leaf, error) {
	row := r.pool.QueryRow(ctx,
		"SELECT "+leafColumns+" FROM leafs WHERE slug = $1", slug)

	p, err := scanLeaf(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("leaf", slug)
		}
		return nil, apierror.Internal("failed to get leaf by slug", err)
	}
	return p, nil
}

// List retrieves leafs with optional filters and cursor-based pagination.
func (r *PgxRepository) List(ctx context.Context, filters LeafListFilters, page types.PaginationRequest) ([]*Leaf, types.PaginationResponse, error) {
	pageSize := page.ClampPageSize()

	var conditions []string
	var args []any
	argIdx := 1

	// Apply filters.
	if filters.State != nil {
		conditions = append(conditions, fmt.Sprintf("state = $%d", argIdx))
		args = append(args, *filters.State)
		argIdx++
	}
	if filters.CreatorID != nil {
		conditions = append(conditions, fmt.Sprintf("creator_id = $%d", argIdx))
		args = append(args, *filters.CreatorID)
		argIdx++
	}
	if filters.Visibility != nil {
		conditions = append(conditions, fmt.Sprintf("visibility = $%d", argIdx))
		args = append(args, *filters.Visibility)
		argIdx++
	}
	if filters.ResearchArea != nil {
		conditions = append(conditions, fmt.Sprintf("$%d = ANY(research_area)", argIdx))
		args = append(args, *filters.ResearchArea)
		argIdx++
	}
	if filters.Search != nil && *filters.Search != "" {
		escaped := strings.NewReplacer("%", "\\%", "_", "\\_").Replace(*filters.Search)
		pattern := "%" + escaped + "%"
		conditions = append(conditions, fmt.Sprintf("(name ILIKE $%d OR description ILIKE $%d)", argIdx, argIdx))
		args = append(args, pattern)
		argIdx++
	}

	// Cursor-based pagination: always keyset on (created_at, id).
	if page.Cursor != "" {
		cursorTime, cursorID, err := types.DecodeCursor(page.Cursor)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.ValidationError("invalid cursor", nil)
		}
		conditions = append(conditions, fmt.Sprintf("(created_at, id) < ($%d, $%d)", argIdx, argIdx+1))
		args = append(args, cursorTime, cursorID)
		argIdx += 2
	}

	// Build WHERE clause.
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Sort field: default to created_at DESC for cursor pagination consistency.
	sortCol := "created_at"
	switch filters.Sort {
	case SortUpdatedAt:
		sortCol = "updated_at"
	case SortName:
		sortCol = "name"
	}
	order := "DESC"
	if filters.Order == OrderAsc {
		order = "ASC"
	}

	// For cursor pagination to work correctly, we always add (created_at DESC, id DESC)
	// as tie-breaker when sorting by a non-created_at column.
	var orderClause string
	if sortCol == "created_at" {
		orderClause = fmt.Sprintf("ORDER BY created_at %s, id %s", order, order)
	} else {
		orderClause = fmt.Sprintf("ORDER BY %s %s, created_at DESC, id DESC", sortCol, order)
	}

	// Fetch one extra row to determine if there are more results.
	query := fmt.Sprintf("SELECT %s FROM leafs %s %s LIMIT $%d",
		leafColumns, where, orderClause, argIdx)
	args = append(args, pageSize+1)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to list leafs", err)
	}
	defer rows.Close()

	var leafs []*Leaf
	for rows.Next() {
		p, err := scanLeaf(rows)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.Internal("failed to scan leaf", err)
		}
		leafs = append(leafs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to iterate leafs", err)
	}

	// Determine pagination response.
	pagination := types.PaginationResponse{}
	if len(leafs) > pageSize {
		leafs = leafs[:pageSize]
		last := leafs[pageSize-1]
		pagination.HasMore = true
		pagination.NextCursor = types.EncodeCursor(last.CreatedAt, last.ID)
	}

	return leafs, pagination, nil
}

// Update modifies an existing leaf. All mutable fields are overwritten.
// The slug is NOT regenerated — it stays the same even if the name changes.
func (r *PgxRepository) Update(ctx context.Context, p *Leaf) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE leafs SET
			name = $2,
			description = $3,
			research_area = $4,
			state = $5,
			task_pattern = $6,
			execution_config = $7,
			validation_config = $8,
			fault_tolerance_config = $9,
			data_config = $10,
			credit_config = $11,
			resource_requirements = $12,
			is_ongoing = $13,
			visibility = $14,
			stats_cache_seconds = $15
		WHERE id = $1`,
		p.ID,
		p.Name,
		p.Description,
		p.ResearchArea,
		p.State,
		p.TaskPattern,
		p.ExecutionConfig,
		p.ValidationConfig,
		p.FaultToleranceConfig,
		p.DataConfig,
		p.CreditConfig,
		p.ResourceRequirements,
		p.IsOngoing,
		p.Visibility,
		p.StatsCacheSeconds,
	)
	if err != nil {
		return apierror.Internal("failed to update leaf", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("leaf", p.ID.String())
	}

	// Re-read to get updated_at from the trigger.
	updated, err := r.GetByID(ctx, p.ID)
	if err != nil {
		return err
	}
	*p = *updated
	return nil
}

// Delete removes a leaf by ID.
// Returns NotFound if the leaf doesn't exist.
// Returns Conflict if a FK constraint (e.g., credit_ledger) prevents deletion.
func (r *PgxRepository) Delete(ctx context.Context, id types.ID) error {
	tag, err := r.pool.Exec(ctx, "DELETE FROM leafs WHERE id = $1", id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return apierror.Conflict(
				"cannot delete leaf: referenced by other records",
				map[string]string{"constraint": pgErr.ConstraintName},
			)
		}
		return apierror.Internal("failed to delete leaf", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("leaf", id.String())
	}
	return nil
}
