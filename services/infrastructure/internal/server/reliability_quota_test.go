package server

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/reliability"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// fakeReliabilityRepo is a stand-in reliability store for the budget refresher tests. It
// records RecordOutcome calls and returns canned ListBudgetInputs.
type fakeReliabilityRepo struct {
	inputs    []reliability.BudgetInput
	listErr   error
	recordErr error
	recorded  []recordedOutcome
}

type recordedOutcome struct {
	hostID types.ID
	good   bool
}

func (f *fakeReliabilityRepo) RecordOutcome(_ context.Context, hostID types.ID, good bool) error {
	f.recorded = append(f.recorded, recordedOutcome{hostID: hostID, good: good})
	return f.recordErr
}

func (f *fakeReliabilityRepo) ListBudgetInputs(_ context.Context) ([]reliability.BudgetInput, error) {
	return f.inputs, f.listErr
}

// newQuotaCache builds a cache wired for the #54 adaptive in-flight quota with deterministic
// tunables and background goroutines NOT started.
func newQuotaCache(enabled bool, floor, flatCap int, relRepo reliability.Repository) (*dispatchCache, *fakeWURepo, *fakeLeafRepo) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newDispatchCache(dispatchCacheConfig{
		readyPoolSize:           100,
		lowWatermark:            10,
		refillBatchSize:         50,
		admissionCap:            4,
		flushInterval:           time.Hour,
		flushBatchSize:          200,
		leaseSeconds:            900,
		maxInflightPerVolunteer: flatCap,
		reliabilityQuotaEnabled: enabled,
		reliabilityFloor:        floor,
	}, dispatchDeps{wuRepo: wuRepo, leafRepo: leafRepo, assignRepo: &fakeAssignRepo{}, reliabilityRepo: relRepo}, testLogger())
	return c, wuRepo, leafRepo
}

// TestEffectiveInflightCap covers the per-host cap resolution: disabled -> flat cap;
// enabled+miss -> cold-start floor (bounded by the flat cap); enabled+hit -> the warmed
// budget; unbounded flat cap -> inert.
func TestEffectiveInflightCap(t *testing.T) {
	host := types.NewID()

	// Disabled: always the flat cap, regardless of any warmed budget.
	cDis, _, _ := newQuotaCache(false, 2, 10, nil)
	cDis.hostBudgetCache[host] = 4 // even if present, ignored when disabled
	if got := cDis.effectiveInflightCap(host, 10); got != 10 {
		t.Errorf("disabled effectiveInflightCap = %d, want flat cap 10", got)
	}

	// Enabled, no warmed budget -> cold-start floor.
	cEn, _, _ := newQuotaCache(true, 2, 10, nil)
	if got := cEn.effectiveInflightCap(host, 10); got != 2 {
		t.Errorf("enabled+miss effectiveInflightCap = %d, want floor 2 (cold start)", got)
	}

	// Enabled, warmed budget -> that budget.
	cEn.hostBudgetCache[host] = 7
	if got := cEn.effectiveInflightCap(host, 10); got != 7 {
		t.Errorf("enabled+hit effectiveInflightCap = %d, want warmed budget 7", got)
	}

	// Enabled but floor above the flat cap is bounded by the cap on a miss.
	cHi, _, _ := newQuotaCache(true, 20, 5, nil)
	if got := cHi.effectiveInflightCap(types.NewID(), 5); got != 5 {
		t.Errorf("floor>cap miss = %d, want flat cap 5", got)
	}

	// Unbounded flat cap (<=0): the quota is inert.
	if got := cEn.effectiveInflightCap(types.NewID(), 0); got != 0 {
		t.Errorf("unbounded flat cap = %d, want 0 (inert)", got)
	}
}

// TestHandOut_ReliabilityQuota_CapsInflight verifies the adaptive cap actually binds in the
// hand-out hot path: with the quota on, a host with no measured signal is held to the
// cold-start floor; a host with a warmed budget gets that many; and with the quota off the
// flat cap applies (today's behavior).
func TestHandOut_ReliabilityQuota_CapsInflight(t *testing.T) {
	stage := func(c *dispatchCache, leafRepo *fakeLeafRepo, n int) types.ID {
		leafID := types.NewID()
		c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
		for i := 0; i < n; i++ {
			c.stageUnit(types.NewID(), leafID, 1, 0)
		}
		return leafID
	}

	// Enabled, cold start: floor 2 binds even though 20 units are available and the flat
	// cap is 10.
	cCold, _, leafRepoCold := newQuotaCache(true, 2, 10, nil)
	stage(cCold, leafRepoCold, 20)
	vCold := types.NewID()
	resCold, _ := cCold.HandOut(vCold, capableOpts(vCold, 10), 20)
	if len(resCold) != 2 {
		t.Fatalf("cold-start hand-out = %d, want 2 (cold-start floor binds)", len(resCold))
	}

	// Enabled, warmed budget 5: exactly 5 handed out.
	cWarm, _, leafRepoWarm := newQuotaCache(true, 2, 10, nil)
	stage(cWarm, leafRepoWarm, 20)
	vWarm := types.NewID()
	cWarm.hostBudgetCache[vWarm] = 5 // meterID(vol, nil) == vol when no host reported
	resWarm, _ := cWarm.HandOut(vWarm, capableOpts(vWarm, 10), 20)
	if len(resWarm) != 5 {
		t.Fatalf("warmed-budget hand-out = %d, want 5 (adaptive budget binds)", len(resWarm))
	}

	// Disabled: the flat cap 10 applies (byte-for-byte today's behavior).
	cOff, _, leafRepoOff := newQuotaCache(false, 2, 10, nil)
	stage(cOff, leafRepoOff, 20)
	vOff := types.NewID()
	resOff, _ := cOff.HandOut(vOff, capableOpts(vOff, 10), 20)
	if len(resOff) != 10 {
		t.Fatalf("disabled hand-out = %d, want flat cap 10", len(resOff))
	}
}

// TestRefreshBudgetsOnce_DerivesAndSwaps verifies the off-hot-path refresher turns reliability
// scores into per-host budgets and swaps the map in.
func TestRefreshBudgetsOnce_DerivesAndSwaps(t *testing.T) {
	reliableHost := types.NewID()
	newHost := types.NewID()
	repo := &fakeReliabilityRepo{inputs: []reliability.BudgetInput{
		{HostID: reliableHost, Score: reliability.DefaultRampUnits * 2}, // well past ramp -> full cap
		{HostID: newHost, Score: 0},                                     // no earned score -> floor
	}}
	c, _, _ := newQuotaCache(true, 2, 10, repo)

	c.refreshBudgetsOnce(context.Background())

	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	if got := c.hostBudgetCache[reliableHost]; got != 10 {
		t.Errorf("reliable host budget = %d, want cap 10", got)
	}
	if got := c.hostBudgetCache[newHost]; got != 2 {
		t.Errorf("zero-score host budget = %d, want floor 2", got)
	}
}

// TestRefreshBudgetsOnce_KeepsStaleOnError verifies a failed list does not clobber the
// existing (good) budget map.
func TestRefreshBudgetsOnce_KeepsStaleOnError(t *testing.T) {
	host := types.NewID()
	repo := &fakeReliabilityRepo{listErr: context.DeadlineExceeded}
	c, _, _ := newQuotaCache(true, 2, 10, repo)
	c.hostBudgetCache[host] = 8 // a previously-computed budget

	c.refreshBudgetsOnce(context.Background())

	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	if got := c.hostBudgetCache[host]; got != 8 {
		t.Errorf("budget after failed refresh = %d, want preserved 8", got)
	}
}

// TestRunBudgetRefresher_NoopWhenDisabled verifies the refresher exits immediately (does not
// read the repo) when the quota is disabled.
func TestRunBudgetRefresher_NoopWhenDisabled(t *testing.T) {
	repo := &fakeReliabilityRepo{inputs: []reliability.BudgetInput{{HostID: types.NewID(), Score: 5}}}
	c, _, _ := newQuotaCache(false, 2, 10, repo)
	// Returns immediately (disabled); if it didn't, a Background ctx would block forever.
	c.runBudgetRefresher(context.Background(), time.Hour)
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	if len(c.hostBudgetCache) != 0 {
		t.Errorf("disabled refresher populated %d budgets, want 0", len(c.hostBudgetCache))
	}
}
