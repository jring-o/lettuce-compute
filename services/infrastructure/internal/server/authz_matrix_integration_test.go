//go:build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lettuce-compute/infrastructure/internal/apikey"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// The DB-backed half of the cluster-B authorization harness (design §4.6, §6.4,
// §9.1–9.3): drives every row of authzRouteTable through the REAL router —
// full middleware chain, real Postgres — as four callers:
//
//	anonymous · authed non-owner (USER key) · owner (USER key) · admin (env key)
//
// and asserts the denial/permit contract of the row's tier. The table encodes
// the design's TARGET state, so on pre-fix code this suite fails exactly on the
// open items (BG-11a aggregate GET, BG-11b analysis reads, ★BG-11c breakdown,
// BG-11c cross-leaf version delete, ★BG-11d-write creator binding) and passes
// once B1 lands — the closeout refutation harness.
//
// Requires LETTUCE_TEST_DB_URL (migrations applied); run with -tags integration
// -p 1 like every DB-backed suite in this repo.

type authzMatrixEnv struct {
	handler   http.Handler
	pool      *pgxpool.Pool
	ownerKey  string
	otherKey  string
	adminKey  string
	ownerID   types.ID
	otherID   types.ID
	leafA     *leaf.Leaf // owned by owner (PUBLIC)
	leafB     *leaf.Leaf // owned by other (PUBLIC)
	leafPriv  *leaf.Leaf // owned by owner (PRIVATE)
	versionB  *leaf.ArtifactVersion // non-current, non-pinned version of leafB
	reqSerial int
}

func setupAuthzMatrix(t *testing.T) *authzMatrixEnv {
	t.Helper()

	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}

	cleanAuthzTables(ctx, pool)
	t.Cleanup(func() {
		cleanAuthzTables(ctx, pool)
		pool.Close()
	})

	const adminKey = "authz-matrix-admin-env-key"
	deps := &Dependencies{
		Pool:        pool,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:     "authz-matrix-test",
		StartTime:   time.Now(),
		AdminAPIKey: adminKey,
		ApiKeyRepo:  apikey.NewPgxRepository(pool),
	}
	handler, rateLimitCleanup := NewRouter(deps)
	t.Cleanup(rateLimitCleanup)

	env := &authzMatrixEnv{handler: handler, pool: pool, adminKey: adminKey}

	env.ownerID = insertAuthzUser(t, pool, "authz-owner")
	env.otherID = insertAuthzUser(t, pool, "authz-other")
	env.ownerKey = insertAuthzAPIKey(t, pool, env.ownerID, "owner-key")
	env.otherKey = insertAuthzAPIKey(t, pool, env.otherID, "other-key")

	leafRepo := leaf.NewPgxRepository(pool)
	env.leafA = insertAuthzLeaf(t, leafRepo, "Authz Matrix Leaf A", env.ownerID, leaf.VisibilityPublic)
	env.leafB = insertAuthzLeaf(t, leafRepo, "Authz Matrix Leaf B", env.otherID, leaf.VisibilityPublic)
	env.leafPriv = insertAuthzLeaf(t, leafRepo, "Authz Matrix Private Leaf", env.ownerID, leaf.VisibilityPrivate)

	// A non-current, never-pinned artifact version of the VICTIM's leaf B: the
	// exact object the BG-11c cross-leaf delete must no longer be able to reach.
	env.versionB = &leaf.ArtifactVersion{
		LeafID:          env.leafB.ID,
		VersionLabel:    "victim-v1",
		RuntimeType:     "WASM",
		ExecutionConfig: env.leafB.ExecutionConfig,
	}
	if err := leafRepo.PublishVersion(ctx, env.versionB); err != nil {
		t.Fatalf("failed to publish victim artifact version: %v", err)
	}

	return env
}

// cleanAuthzTables removes everything this suite seeds (and anything a prior
// crashed run left behind), child tables first. The integration suite runs
// -p 1, so this cannot race another package.
func cleanAuthzTables(ctx context.Context, pool *pgxpool.Pool) {
	for _, stmt := range []string{
		"DELETE FROM credit_attestations",
		"DELETE FROM volunteer_rac",
		"DELETE FROM work_unit_assignment_history",
		"DELETE FROM credit_adjustments",
		"DELETE FROM result_audits",
		"DELETE FROM credit_ledger",
		"DELETE FROM leaf_stats_snapshots",
		"DELETE FROM results",
		"DELETE FROM work_units",
		"DELETE FROM batches",
		"UPDATE leafs SET current_artifact_version_id = NULL",
		"DELETE FROM leaf_artifact_versions",
		"DELETE FROM leafs",
		"DELETE FROM api_keys",
		"DELETE FROM volunteers",
		"DELETE FROM users",
	} {
		_, _ = pool.Exec(ctx, stmt)
	}
}

func insertAuthzUser(t *testing.T, pool *pgxpool.Pool, username string) types.ID {
	t.Helper()
	var raw string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO users (email, email_verified, username, display_name, password_hash, role)
		VALUES ($1, true, $2, $3, 'x', 'USER')
		RETURNING id`,
		username+"@authz.test", username, username,
	).Scan(&raw)
	if err != nil {
		t.Fatalf("failed to insert user %s: %v", username, err)
	}
	id, err := types.ParseID(raw)
	if err != nil {
		t.Fatalf("failed to parse user id: %v", err)
	}
	return id
}

func insertAuthzAPIKey(t *testing.T, pool *pgxpool.Pool, userID types.ID, name string) string {
	t.Helper()
	plaintext, prefix, hash, err := apikey.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate API key: %v", err)
	}
	repo := apikey.NewPgxRepository(pool)
	if err := repo.Create(context.Background(), &apikey.ApiKey{
		UserID:    userID,
		Name:      name,
		KeyPrefix: prefix,
		KeyHash:   hash,
	}); err != nil {
		t.Fatalf("failed to store API key: %v", err)
	}
	return plaintext
}

func insertAuthzLeaf(t *testing.T, repo *leaf.PgxRepository, name string, creator types.ID, vis leaf.LeafVisibility) *leaf.Leaf {
	t.Helper()
	l := &leaf.Leaf{
		Name:        name,
		Description: "Seeded by the cluster-B authorization matrix integration test.",
		State:       leaf.StateDraft,
		TaskPattern: leaf.PatternParameterSweep,
		Visibility:  vis,
		CreatorID:   &creator,
	}
	if err := repo.Create(context.Background(), l); err != nil {
		t.Fatalf("failed to create leaf %s: %v", name, err)
	}
	return l
}

// do sends one request through the full router. Every call gets a unique
// client IP so the anonymous per-IP rate limit can never interfere with the
// matrix; authenticated callers stay far under the per-user limit.
func (env *authzMatrixEnv) do(method, path, key string, body string) *httptest.ResponseRecorder {
	env.reqSerial++
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = fmt.Sprintf("10.42.%d.%d:9999", env.reqSerial/200, env.reqSerial%200+1)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	env.handler.ServeHTTP(w, req)
	return w
}

// pathFor substitutes seeded object ids into a route pattern. Placeholder
// objects that a denied caller can never reach (work units, admin subjects)
// use throwaway ids.
func (env *authzMatrixEnv) pathFor(pattern string) string {
	r := strings.NewReplacer(
		"{leaf_id}", env.leafA.ID.String(),
		"{version_id}", types.NewID().String(),
		"{work_unit_id}", types.NewID().String(),
		"{volunteer_id}", types.NewID().String(),
		"{id}", types.NewID().String(),
		"{subject}", "authz-matrix-subject",
	)
	return r.Replace(pattern)
}

func denied(status int) bool { return status == http.StatusUnauthorized || status == http.StatusForbidden }

func TestAuthzMatrix_RouteTable(t *testing.T) {
	env := setupAuthzMatrix(t)

	for _, row := range authzRouteTable {
		if row.tier == tierVolunteerKey {
			// Ed25519 handler-level auth — covered by ed25519_auth_test.go,
			// and not an API-key surface this cluster changes.
			continue
		}
		row := row
		name := fmt.Sprintf("%s_%s", row.method, strings.ReplaceAll(strings.TrimPrefix(row.pattern, "/api/v1/"), "/", "_"))
		t.Run(name, func(t *testing.T) {
			path := env.pathFor(row.pattern)

			switch row.tier {
			case tierPublic, tierVisibility:
				if w := env.do(row.method, path, "", ""); denied(w.Code) {
					t.Errorf("anonymous %s %s: public/visibility route denied with %d", row.method, path, w.Code)
				}

			case tierAuthed:
				if w := env.do(row.method, path, "", ""); w.Code != http.StatusUnauthorized {
					t.Errorf("anonymous %s %s: want 401, got %d", row.method, path, w.Code)
				}
				if row.probeAllowed {
					if w := env.do(row.method, path, env.otherKey, ""); denied(w.Code) {
						t.Errorf("authed %s %s: want success, got %d", row.method, path, w.Code)
					}
				}

			case tierOwner:
				if w := env.do(row.method, path, "", ""); w.Code != http.StatusUnauthorized {
					t.Errorf("anonymous %s %s: want 401, got %d", row.method, path, w.Code)
				}
				if w := env.do(row.method, path, env.otherKey, ""); w.Code != http.StatusForbidden {
					t.Errorf("non-owner %s %s: want 403, got %d", row.method, path, w.Code)
				}
				if row.probeAllowed {
					if w := env.do(row.method, path, env.ownerKey, ""); denied(w.Code) {
						t.Errorf("owner %s %s: want success, got %d", row.method, path, w.Code)
					}
					if w := env.do(row.method, path, env.adminKey, ""); denied(w.Code) {
						t.Errorf("admin %s %s: want success, got %d", row.method, path, w.Code)
					}
				}

			case tierAdminGate, tierAdminOnly:
				if w := env.do(row.method, path, "", ""); w.Code != http.StatusUnauthorized {
					t.Errorf("anonymous %s %s: want 401, got %d", row.method, path, w.Code)
				}
				if w := env.do(row.method, path, env.otherKey, ""); w.Code != http.StatusForbidden {
					t.Errorf("non-admin USER %s %s: want 403, got %d", row.method, path, w.Code)
				}
				if w := env.do(row.method, path, env.ownerKey, ""); w.Code != http.StatusForbidden {
					t.Errorf("leaf-owning USER %s %s: want 403 (ownership grants no operator access), got %d", row.method, path, w.Code)
				}
				if row.probeAllowed {
					if w := env.do(row.method, path, env.adminKey, ""); denied(w.Code) {
						t.Errorf("admin %s %s: want success, got %d", row.method, path, w.Code)
					}
				}
			}
		})
	}
}

// TestAuthzMatrix_PrivateLeafMetadataVisibility pins the visibility tier
// (already correct today): a PRIVATE leaf's METADATA is invisible to anonymous
// and non-owner callers — same 404 as a missing leaf — while owner and admin
// read it.
func TestAuthzMatrix_PrivateLeafMetadataVisibility(t *testing.T) {
	env := setupAuthzMatrix(t)
	path := "/api/v1/leafs/" + env.leafPriv.ID.String()

	if w := env.do("GET", path, "", ""); w.Code != http.StatusNotFound {
		t.Errorf("anonymous GET private leaf: want 404, got %d", w.Code)
	}
	if w := env.do("GET", path, env.otherKey, ""); w.Code != http.StatusNotFound {
		t.Errorf("non-owner GET private leaf: want 404, got %d", w.Code)
	}
	if w := env.do("GET", path, env.ownerKey, ""); w.Code != http.StatusOK {
		t.Errorf("owner GET private leaf: want 200, got %d", w.Code)
	}
	if w := env.do("GET", path, env.adminKey, ""); w.Code != http.StatusOK {
		t.Errorf("admin GET private leaf: want 200, got %d", w.Code)
	}
}

// TestAuthzMatrix_CrossLeafVersionDelete is the BG-11c regression (§4.4, §9.6):
// the attacker owns leaf A and names a version of victim leaf B in A's path.
// authOwner passes (they DO own A) — the wrong-object hole was that the handler
// then deleted by version id alone. Post-fix every statement is scoped
// AND leaf_id, so the delete finds no row: 404, and B's version survives.
func TestAuthzMatrix_CrossLeafVersionDelete(t *testing.T) {
	env := setupAuthzMatrix(t)

	path := fmt.Sprintf("/api/v1/leafs/%s/versions/%s", env.leafA.ID, env.versionB.ID)
	w := env.do("DELETE", path, env.ownerKey, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("cross-leaf version delete: want 404, got %d", w.Code)
	}

	var exists bool
	err := env.pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM leaf_artifact_versions WHERE id = $1)`, env.versionB.ID).Scan(&exists)
	if err != nil {
		t.Fatalf("failed to check version survival: %v", err)
	}
	if !exists {
		t.Error("cross-leaf version delete REMOVED the victim's artifact version — BG-11c open")
	}
}

// TestAuthzMatrix_CreateLeafBindsCreatorToCaller is ★BG-11d-write (R1.5): a
// USER-key caller must not be able to mint a leaf owned by someone else's
// user id — that would forge the ownership root every other check keys on.
func TestAuthzMatrix_CreateLeafBindsCreatorToCaller(t *testing.T) {
	env := setupAuthzMatrix(t)

	body := fmt.Sprintf(`{
		"name": "Creator Forge Attempt",
		"description": "Attempts to create a leaf owned by another user.",
		"research_area": ["authz-testing"],
		"task_pattern": "PARAMETER_SWEEP",
		"creator_id": %q
	}`, env.otherID)

	w := env.do("POST", "/api/v1/leafs", env.ownerKey, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create leaf: want 201, got %d (body: %s)", w.Code, w.Body.String())
	}

	var created struct {
		ID        string  `json:"id"`
		CreatorID *string `json:"creator_id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("failed to decode created leaf: %v", err)
	}
	if created.CreatorID == nil || *created.CreatorID != env.ownerID.String() {
		got := "<nil>"
		if created.CreatorID != nil {
			got = *created.CreatorID
		}
		t.Errorf("created leaf's creator_id = %s; want the CALLER %s — a client-supplied creator_id must never be honored for non-admin callers", got, env.ownerID)
	}
}
