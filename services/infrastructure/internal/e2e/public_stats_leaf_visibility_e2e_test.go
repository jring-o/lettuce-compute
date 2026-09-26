//go:build integration

package e2e_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apikey"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/server"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestPublicPerAccountStats_NamePublicLeafsOnly drives the production router with no
// Authorization header, as any visitor would. A volunteer earns credit through the real
// dispatch and submit flow on a PUBLIC, an UNLISTED and a PRIVATE leaf (the last two
// pinned by id, the only way they are served). The public per-account stats then name
// the PUBLIC leaf only, while their totals, the public fleet feed, the operator
// breakdown and the volunteer's own GetMyContribution still count all three, and the
// PRIVATE leaf stays unreadable by id.
func TestPublicPerAccountStats_NamePublicLeafsOnly(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "pubstats")
	pub := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, pub, "pubstats-A")

	byVis := map[leaf.LeafVisibility]leaf.Leaf{}
	for _, vis := range []leaf.LeafVisibility{leaf.VisibilityPublic, leaf.VisibilityUnlisted, leaf.VisibilityPrivate} {
		opts := hlDefaultLeafOpts("Stats Visibility " + string(vis) + " Leaf")
		opts.Visibility = vis
		lf := createHLLeaf(t, env, ctx, userID, opts)
		generateLeafWUs(t, env, lf.ID, 1)
		a := requestWUFromLeafs(t, env, ctx, volID, pub, []string{lf.ID.String()})
		submitWUResult(t, env, ctx, volID, pub, a.WorkUnitId, []byte(`{"result":"pubstats","value":1.0}`))
		byVis[vis] = lf
	}
	waitLedgerRows(t, env, ctx, volID, 3)

	const adminKey = "pubstats-admin-key"
	handler, rlCleanup := server.NewRouter(&server.Dependencies{
		Pool:        env.pool,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:     "pubstats-test",
		StartTime:   time.Now(),
		AdminAPIKey: adminKey,
		ApiKeyRepo:  apikey.NewPgxRepository(env.pool),
	})
	defer rlCleanup()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	get := func(path, bearer string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}

	publicLeaf := byVis[leaf.VisibilityPublic]
	hidden := []leaf.Leaf{byVis[leaf.VisibilityUnlisted], byVis[leaf.VisibilityPrivate]}

	// The public per-account stats, anonymously.
	code, body := get("/api/v1/volunteers/"+volID+"/stats", "")
	if code != http.StatusOK {
		t.Fatalf("per-account stats: status %d, body %s", code, body)
	}
	for _, lf := range hidden {
		if strings.Contains(string(body), lf.ID.String()) || strings.Contains(string(body), lf.Name) {
			t.Errorf("per-account stats name the %s leaf %q (%s) to an anonymous caller: %s", lf.Visibility, lf.Name, lf.ID, body)
		}
	}
	var stats credit.VolunteerStatsResponse
	if err := json.Unmarshal(body, &stats); err != nil {
		t.Fatalf("decode per-account stats: %v; body %s", err, body)
	}
	if len(stats.Leafs) != 1 || stats.Leafs[0].LeafID != publicLeaf.ID || stats.Leafs[0].LeafName != publicLeaf.Name {
		t.Errorf("per-account leafs = %+v, want only the PUBLIC leaf %q", stats.Leafs, publicLeaf.Name)
	}
	if stats.TotalCredit != 3 || stats.TotalWorkUnitsCompleted != 3 {
		t.Errorf("per-account totals = credit %v, units %d; want 3 and 3 (every leaf counted)", stats.TotalCredit, stats.TotalWorkUnitsCompleted)
	}

	// The public fleet feed is unchanged: it lists the account with the same total.
	code, body = get("/api/v1/volunteers/stats", "")
	if code != http.StatusOK {
		t.Fatalf("fleet feed: status %d, body %s", code, body)
	}
	var feed credit.AllVolunteerStatsResponse
	if err := json.Unmarshal(body, &feed); err != nil {
		t.Fatalf("decode fleet feed: %v; body %s", err, body)
	}
	var feedCredit float64 = -1
	for _, v := range feed.Volunteers {
		if v.VolunteerID.String() == volID {
			feedCredit = v.TotalCredit
		}
	}
	if feedCredit != stats.TotalCredit {
		t.Errorf("fleet feed total for the account = %v, want %v (the per-account total)", feedCredit, stats.TotalCredit)
	}

	// The PRIVATE leaf is still unreadable by id.
	if code, body = get("/api/v1/leafs/"+byVis[leaf.VisibilityPrivate].ID.String(), ""); code != http.StatusNotFound {
		t.Errorf("anonymous GET of the PRIVATE leaf: status %d, want 404; body %s", code, body)
	}

	// The operator breakdown (admin only) lists all three leafs.
	code, body = get("/api/v1/volunteers/"+volID+"/credit/breakdown", adminKey)
	if code != http.StatusOK {
		t.Fatalf("operator breakdown: status %d, body %s", code, body)
	}
	var bd credit.VolunteerBreakdown
	if err := json.Unmarshal(body, &bd); err != nil {
		t.Fatalf("decode operator breakdown: %v; body %s", err, body)
	}
	opLeafs := map[string]bool{}
	for _, lc := range bd.ByLeaf {
		opLeafs[lc.LeafID.String()] = true
	}
	for _, lf := range byVis {
		if !opLeafs[lf.ID.String()] {
			t.Errorf("operator breakdown omits the %s leaf %q", lf.Visibility, lf.Name)
		}
	}

	// The volunteer's own view lists all three leafs.
	mine, err := env.grpc.GetMyContribution(signFor(t, ctx, pub), &lettucev1.GetMyContributionRequest{})
	if err != nil {
		t.Fatalf("GetMyContribution: %v", err)
	}
	ownLeafs := map[string]bool{}
	for _, lc := range mine.GetByLeaf() {
		ownLeafs[lc.GetLeafId()] = true
	}
	for _, lf := range byVis {
		if !ownLeafs[lf.ID.String()] {
			t.Errorf("GetMyContribution omits the %s leaf %q", lf.Visibility, lf.Name)
		}
	}
}

// waitLedgerRows waits until the volunteer has want credit_ledger rows.
func waitLedgerRows(t *testing.T, env *headsLeafsEnv, ctx context.Context, volID string, want int) {
	t.Helper()
	var n int
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if err := env.pool.QueryRow(ctx, "SELECT COUNT(*) FROM credit_ledger WHERE volunteer_id = $1", volID).Scan(&n); err != nil {
			t.Fatalf("count credit rows: %v", err)
		}
		if n >= want {
			return
		}
	}
	t.Fatalf("volunteer has %d credit rows, want %d", n, want)
}
