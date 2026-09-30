package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/standing"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// An empty work reply names its cause when the cause is the requester's own state — the
// machine's in-flight cap, the account's results, a recent failed copy, the account's
// standing, the speed on record against a deadline — and is UNSPECIFIED otherwise. It is
// worked out over the request's own scope only, and never from a leaf the requester could
// not see.

const (
	reasonUnspecified   = lettucev1.NoWorkReason_NO_WORK_REASON_UNSPECIFIED
	reasonInflightCap   = lettucev1.NoWorkReason_NO_WORK_REASON_INFLIGHT_CAP
	reasonContributed   = lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED
	reasonBenchCooldown = lettucev1.NoWorkReason_NO_WORK_REASON_BENCH_COOLDOWN
	reasonBenched       = lettucev1.NoWorkReason_NO_WORK_REASON_ACCOUNT_BENCHED
	reasonDeadline      = lettucev1.NoWorkReason_NO_WORK_REASON_INFEASIBLE_DEADLINE
)

// reasonCache is a cache with a Debug capturing logger and a fixed clock.
func reasonCache(t *testing.T) (*dispatchCache, *fakeLeafRepo) {
	t.Helper()
	c, leafRepo, _, _ := scopeCache(t)
	return c, leafRepo
}

// emptyHandOut runs one hand-out that must return nothing and returns what its reply states.
func emptyHandOut(t *testing.T, c *dispatchCache, vol types.ID, opts workunit.AssignmentOptions) handOutReply {
	t.Helper()
	res, _, nw := c.HandOutWithReason(vol, opts, 1)
	if len(res) != 0 {
		t.Fatalf("hand-out = %d units, want 0", len(res))
	}
	return nw
}

// holdInflight puts a machine at the given in-flight count.
func holdInflight(c *dispatchCache, host types.ID, n int) {
	c.mu.Lock()
	c.inflight[host] = n
	c.mu.Unlock()
}

// benchAccount puts an account in the standing snapshot as BENCHED until the given time
// (nil = no end time).
func benchAccount(c *dispatchCache, vol types.ID, until *time.Time) {
	c.mu.Lock()
	if c.standingSnapshot == nil {
		c.standingSnapshot = map[types.ID]standing.Entry{}
	}
	c.standingSnapshot[vol] = benchedEntry(until)
	c.mu.Unlock()
}

// withLeafVisibility returns a native leaf with the given visibility.
func withLeafVisibility(id types.ID, v leaf.LeafVisibility) *leaf.Leaf {
	lf := nativeLeaf(id, 2, false, 0)
	lf.Visibility = v
	return lf
}

// A machine holding its cap asks for a leaf that has units
// it could otherwise take. The reply says so, with this machine's own cap and count.
func TestNoWorkReason_InflightCap_CarriesThisMachinesCapAndCount(t *testing.T) {
	c, leafRepo := reasonCache(t)
	lf := types.NewID()
	c.warm(nativeLeaf(lf, 2, false, 0), leafRepo)
	for i := 0; i < 3; i++ {
		c.stageUnit(types.NewID(), lf, 2, 0)
	}
	vol, host, other := types.NewID(), types.NewID(), types.NewID()
	holdInflight(c, host, 10)
	holdInflight(c, other, 7) // another machine's count must not leak into this reply

	opts := hostOpts(vol, host, 10)
	opts.LeafIDs = []types.ID{lf}
	nw := emptyHandOut(t, c, vol, opts)
	if nw.reason != reasonInflightCap {
		t.Fatalf("reason = %v, want INFLIGHT_CAP", nw.reason)
	}
	if nw.inflightCap != 10 || nw.inflightHeld != 10 {
		t.Errorf("cap/held = %d/%d, want 10/10 (this machine's own)", nw.inflightCap, nw.inflightHeld)
	}
	if nw.deadlineSeconds != 0 || nw.estimatedSeconds != 0 || nw.benchedUntil != nil {
		t.Errorf("cap reply carries other classes' figures: %+v", nw)
	}
	resp := &lettucev1.RequestWorkUnitResponse{}
	nw.apply(resp)
	if resp.NoWorkReason != reasonInflightCap || resp.InflightCap != 10 || resp.InflightHeld != 10 {
		t.Errorf("wire reply = %v, want INFLIGHT_CAP 10/10", resp)
	}
}

// The cap is named only when it is what stands between the machine and a unit. At the cap,
// a unit this account already has a result on, or one this machine cannot run, would not
// come when a copy finishes, so the cap is not the reason.
func TestNoWorkReason_InflightCap_OnlyWhenAUnitWouldOtherwiseBeHandedOut(t *testing.T) {
	t.Run("every unit already contributed", func(t *testing.T) {
		c, leafRepo := reasonCache(t)
		lf := types.NewID()
		c.warm(nativeLeaf(lf, 2, false, 0), leafRepo)
		vol, host := types.NewID(), types.NewID()
		// Contributed units are refused before the cap check; a unit refused only by the
		// cap and then re-checked is what matters, so stage the contributed shape only.
		for i := 0; i < 2; i++ {
			c.stageUnitSets(types.NewID(), lf, 2, 1, []types.ID{vol}, nil)
		}
		holdInflight(c, host, 2)
		opts := hostOpts(vol, host, 2)
		opts.LeafIDs = []types.ID{lf}
		// Not the cap. Nor ALREADY_CONTRIBUTED: "every task" is said only on the database's
		// answer, and a machine with no in-flight room is not handed anything from the
		// database, so it is not asked.
		if nw := emptyHandOut(t, c, vol, opts); nw.reason != reasonUnspecified {
			t.Fatalf("reason = %v, want UNSPECIFIED (the cap binds no unit, and the database was not asked)", nw.reason)
		}
	})
	t.Run("the unit does not fit this machine", func(t *testing.T) {
		c, leafRepo := reasonCache(t)
		lf := types.NewID()
		big := nativeLeaf(lf, 2, false, 0)
		big.ResourceRequirements.MinCPUCores = 64
		c.warm(big, leafRepo)
		c.stageUnit(types.NewID(), lf, 2, 0)
		vol, host := types.NewID(), types.NewID()
		holdInflight(c, host, 2)
		opts := hostOpts(vol, host, 2)
		opts.LeafIDs = []types.ID{lf}
		if nw := emptyHandOut(t, c, vol, opts); nw.reason != reasonUnspecified {
			t.Fatalf("reason = %v, want UNSPECIFIED (below the cap the unit would still not fit)", nw.reason)
		}
	})
	t.Run("the unit would be too slow for its deadline", func(t *testing.T) {
		c, leafRepo := reasonCache(t)
		lf := types.NewID()
		slow := nativeLeaf(lf, 2, false, 0)
		slow.ExecutionConfig.RscFpopsEst = 1e12
		c.warm(slow, leafRepo)
		c.stageUnit(types.NewID(), lf, 2, 0)
		c.mu.Lock()
		c.ready[len(c.ready)-1].unit.DeadlineSeconds = 600
		c.mu.Unlock()
		vol, host := types.NewID(), types.NewID()
		holdInflight(c, host, 2)
		opts := hostOpts(vol, host, 2)
		opts.LeafIDs = []types.ID{lf}
		opts.BenchmarkFPOPS = 1e9
		nw := emptyHandOut(t, c, vol, opts)
		if nw.reason != reasonDeadline {
			t.Fatalf("reason = %v, want INFEASIBLE_DEADLINE (below the cap it would still be refused)", nw.reason)
		}
	})
}

// The audit's H1 shape: the request names an empty leaf while another leaf's units all
// carry this account's result. The other leaf is outside the request, so the reply is
// UNSPECIFIED, not ALREADY_CONTRIBUTED.
func TestNoWorkReason_OtherLeavesUnitsNeverProduceAReason(t *testing.T) {
	c, leafRepo := reasonCache(t)
	emptyLeaf, otherLeaf := types.NewID(), types.NewID()
	c.warm(nativeLeaf(emptyLeaf, 2, false, 0), leafRepo)
	c.warm(nativeLeaf(otherLeaf, 2, false, 0), leafRepo)
	vol, host := types.NewID(), types.NewID()
	for i := 0; i < 3; i++ {
		c.stageUnitSets(types.NewID(), otherLeaf, 2, 1, []types.ID{vol}, nil)
	}
	for i := 0; i < 2; i++ {
		c.stageUnit(types.NewID(), otherLeaf, 2, 0)
	}
	holdInflight(c, host, 2) // at the cap too: the other leaf's free units must not name it

	opts := hostOpts(vol, host, 2)
	opts.LeafIDs = []types.ID{emptyLeaf}
	if nw := emptyHandOut(t, c, vol, opts); nw.reason != reasonUnspecified {
		t.Fatalf("reason = %v, want UNSPECIFIED (the requested leaf is empty)", nw.reason)
	}
}

// ALREADY_CONTRIBUTED says "every": it is sent only when every in-scope refusal is this
// account's own result or copy. One unit refused because other volunteers' copies fill it
// makes the reply UNSPECIFIED, so it cannot repeat the "every ready unit refuses this
// account" mislabel.
func TestNoWorkReason_AlreadyContributed_OnlyWhenEveryRefusalIsTheAccountsOwn(t *testing.T) {
	setup := func(t *testing.T) (*dispatchCache, types.ID, types.ID) {
		c, leafRepo := reasonCache(t)
		lf := types.NewID()
		c.warm(nativeLeaf(lf, 2, false, 0), leafRepo)
		vol := types.NewID()
		for i := 0; i < 2; i++ {
			c.stageUnitSets(types.NewID(), lf, 2, 1, []types.ID{vol}, nil)
		}
		return c, lf, vol
	}
	t.Run("results only", func(t *testing.T) {
		c, lf, vol := setup(t)
		opts := capableOpts(vol, 0)
		opts.LeafIDs = []types.ID{lf}
		if nw := emptyHandOut(t, c, vol, opts); nw.reason != reasonContributed {
			t.Fatalf("reason = %v, want ALREADY_CONTRIBUTED", nw.reason)
		}
	})
	t.Run("a copy this account holds counts too", func(t *testing.T) {
		c, lf, vol := setup(t)
		held := types.NewID()
		c.stageUnit(held, lf, 3, 0)
		c.mu.Lock()
		c.reservedInMem[held] = map[types.ID]heldCopy{vol: {reservedUntil: c.now().Add(time.Hour), hostID: vol, subject: requesterSubject(vol, capableOpts(vol, 0))}}
		c.mu.Unlock()
		opts := capableOpts(vol, 0)
		opts.LeafIDs = []types.ID{lf}
		if nw := emptyHandOut(t, c, vol, opts); nw.reason != reasonContributed {
			t.Fatalf("reason = %v, want ALREADY_CONTRIBUTED (self-held counts with results)", nw.reason)
		}
	})
	t.Run("a unit full of other volunteers' copies", func(t *testing.T) {
		c, lf, vol := setup(t)
		c.stageUnit(types.NewID(), lf, 2, 2) // two other copies already cover it
		opts := capableOpts(vol, 0)
		opts.LeafIDs = []types.ID{lf}
		if nw := emptyHandOut(t, c, vol, opts); nw.reason != reasonUnspecified {
			t.Fatalf("reason = %v, want UNSPECIFIED (one unit is refused because of other volunteers)", nw.reason)
		}
	})
}

// A recent failed copy benches the account from that unit for about one deadline.
func TestNoWorkReason_BenchCooldown(t *testing.T) {
	c, leafRepo := reasonCache(t)
	lf := types.NewID()
	c.warm(nativeLeaf(lf, 1, false, 0), leafRepo)
	vol := types.NewID()
	c.stageUnitSets(types.NewID(), lf, 1, 0, nil, []types.ID{vol})
	c.stageUnitSets(types.NewID(), lf, 2, 1, []types.ID{vol}, nil) // lower precedence
	opts := capableOpts(vol, 0)
	opts.LeafIDs = []types.ID{lf}
	if nw := emptyHandOut(t, c, vol, opts); nw.reason != reasonBenchCooldown {
		t.Fatalf("reason = %v, want BENCH_COOLDOWN", nw.reason)
	}
}

// A BENCHED account is told so, with the bench's end when it has one, even when the
// requested leaf is empty — it can take nothing from this head whatever the pool holds.
func TestNoWorkReason_AccountBenched(t *testing.T) {
	t.Run("with an end time", func(t *testing.T) {
		c, leafRepo := reasonCache(t)
		lf := types.NewID()
		c.warm(nativeLeaf(lf, 2, false, 0), leafRepo)
		c.stageUnit(types.NewID(), lf, 2, 0)
		vol := types.NewID()
		until := c.now().Add(3 * time.Hour).Truncate(time.Second)
		benchAccount(c, vol, &until)
		opts := capableOpts(vol, 0)
		opts.LeafIDs = []types.ID{lf}
		nw := emptyHandOut(t, c, vol, opts)
		if nw.reason != reasonBenched {
			t.Fatalf("reason = %v, want ACCOUNT_BENCHED", nw.reason)
		}
		if nw.benchedUntil == nil || !nw.benchedUntil.Equal(until) {
			t.Errorf("benchedUntil = %v, want %v", nw.benchedUntil, until)
		}
		resp := &lettucev1.RequestWorkUnitResponse{}
		nw.apply(resp)
		if resp.BenchedUntilUnix != until.Unix() {
			t.Errorf("benched_until_unix = %d, want %d", resp.BenchedUntilUnix, until.Unix())
		}
	})
	t.Run("indefinite, requested leaf empty", func(t *testing.T) {
		c, leafRepo := reasonCache(t)
		lf := types.NewID()
		c.warm(nativeLeaf(lf, 2, false, 0), leafRepo)
		vol := types.NewID()
		benchAccount(c, vol, nil)
		opts := capableOpts(vol, 0)
		opts.LeafIDs = []types.ID{lf}
		nw := emptyHandOut(t, c, vol, opts)
		if nw.reason != reasonBenched || nw.benchedUntil != nil {
			t.Fatalf("reply = %+v, want ACCOUNT_BENCHED with no end time", nw)
		}
		resp := &lettucev1.RequestWorkUnitResponse{}
		nw.apply(resp)
		if resp.BenchedUntilUnix != 0 {
			t.Errorf("benched_until_unix = %d, want 0 (no end time)", resp.BenchedUntilUnix)
		}
	})
	t.Run("an expired bench is not a bench", func(t *testing.T) {
		c, leafRepo := reasonCache(t)
		lf := types.NewID()
		c.warm(nativeLeaf(lf, 2, false, 0), leafRepo)
		vol := types.NewID()
		past := c.now().Add(-time.Minute)
		benchAccount(c, vol, &past)
		opts := capableOpts(vol, 0)
		opts.LeafIDs = []types.ID{lf}
		if nw := emptyHandOut(t, c, vol, opts); nw.reason != reasonUnspecified {
			t.Fatalf("reason = %v, want UNSPECIFIED (the bench lapsed; the leaf is empty)", nw.reason)
		}
	})
}

// When several reasons apply, the documented precedence decides: ACCOUNT_BENCHED,
// INFLIGHT_CAP, INFEASIBLE_DEADLINE, BENCH_COOLDOWN, ALREADY_CONTRIBUTED. The request
// names two leaves: one whose units this host can finish in time and one whose units it
// cannot (estimate 1e12 FP-ops, benchmark 1e9/s, deadline 600 s).
func TestNoWorkReason_Precedence(t *testing.T) {
	type stage struct{ free, infeasible, cooldown, contributed bool }
	build := func(t *testing.T, s stage, atCap, benched bool) handOutReply {
		c, leafRepo := reasonCache(t)
		fast, slow := types.NewID(), types.NewID()
		c.warm(nativeLeaf(fast, 2, false, 0), leafRepo)
		slowLeaf := nativeLeaf(slow, 2, false, 0)
		slowLeaf.ExecutionConfig.RscFpopsEst = 1e12
		c.warm(slowLeaf, leafRepo)
		vol, host := types.NewID(), types.NewID()
		if s.free {
			c.stageUnit(types.NewID(), fast, 2, 0)
		}
		if s.infeasible {
			c.stageUnit(types.NewID(), slow, 2, 0)
			c.mu.Lock()
			c.ready[len(c.ready)-1].unit.DeadlineSeconds = 600
			c.mu.Unlock()
		}
		if s.cooldown {
			c.stageUnitSets(types.NewID(), fast, 2, 0, nil, []types.ID{vol})
		}
		if s.contributed {
			c.stageUnitSets(types.NewID(), fast, 2, 1, []types.ID{vol}, nil)
		}
		if benched {
			benchAccount(c, vol, nil)
		}
		max := 0
		if atCap {
			max = 3
			holdInflight(c, host, 3)
		}
		opts := hostOpts(vol, host, max)
		opts.LeafIDs = []types.ID{fast, slow}
		opts.BenchmarkFPOPS = 1e9
		return emptyHandOut(t, c, vol, opts)
	}
	all := stage{free: true, infeasible: true, cooldown: true, contributed: true}
	cases := []struct {
		name    string
		s       stage
		atCap   bool
		benched bool
		want    lettucev1.NoWorkReason
	}{
		{"benched beats everything", all, true, true, reasonBenched},
		{"cap beats deadline, cooldown, contributed", all, true, false, reasonInflightCap},
		{"deadline beats cooldown and contributed", stage{infeasible: true, cooldown: true, contributed: true}, false, false, reasonDeadline},
		{"cooldown beats contributed", stage{cooldown: true, contributed: true}, false, false, reasonBenchCooldown},
		{"contributed alone", stage{contributed: true}, false, false, reasonContributed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := build(t, tc.s, tc.atCap, tc.benched); got.reason != tc.want {
				t.Fatalf("reason = %v, want %v", got.reason, tc.want)
			}
		})
	}
}

// A unit of a PRIVATE or UNLISTED leaf the request did not name never produces a reason,
// and a request that names a leaf not known to be PUBLIC from a fresh snapshot gets
// UNSPECIFIED whatever its units' reasons — the reply must not confirm a hidden leaf.
func TestNoWorkReason_HiddenLeavesNeverProduceAReason(t *testing.T) {
	for _, v := range []leaf.LeafVisibility{leaf.VisibilityPrivate, leaf.VisibilityUnlisted} {
		t.Run("any-leaf request, "+string(v)+" units all contributed", func(t *testing.T) {
			c, leafRepo := reasonCache(t)
			hidden := types.NewID()
			c.warm(withLeafVisibility(hidden, v), leafRepo)
			vol := types.NewID()
			for i := 0; i < 2; i++ {
				c.stageUnitSets(types.NewID(), hidden, 2, 1, []types.ID{vol}, nil)
			}
			if nw := emptyHandOut(t, c, vol, capableOpts(vol, 0)); nw.reason != reasonUnspecified {
				t.Fatalf("reason = %v, want UNSPECIFIED (the only units are a hidden leaf's)", nw.reason)
			}
		})
		t.Run("any-leaf request, "+string(v)+" unit at the cap", func(t *testing.T) {
			c, leafRepo := reasonCache(t)
			hidden := types.NewID()
			c.warm(withLeafVisibility(hidden, v), leafRepo)
			c.stageUnit(types.NewID(), hidden, 2, 0)
			vol, host := types.NewID(), types.NewID()
			holdInflight(c, host, 2)
			if nw := emptyHandOut(t, c, vol, hostOpts(vol, host, 2)); nw.reason != reasonUnspecified {
				t.Fatalf("reason = %v, want UNSPECIFIED (the only unit is a hidden leaf's)", nw.reason)
			}
		})
		t.Run("pinned "+string(v)+" leaf, units all contributed", func(t *testing.T) {
			c, leafRepo := reasonCache(t)
			hidden := types.NewID()
			c.warm(withLeafVisibility(hidden, v), leafRepo)
			vol := types.NewID()
			for i := 0; i < 2; i++ {
				c.stageUnitSets(types.NewID(), hidden, 2, 1, []types.ID{vol}, nil)
			}
			opts := capableOpts(vol, 0)
			opts.LeafIDs = []types.ID{hidden}
			if nw := emptyHandOut(t, c, vol, opts); nw.reason != reasonUnspecified {
				t.Fatalf("reason = %v, want UNSPECIFIED (a pinned hidden leaf gets no unit-derived reason)", nw.reason)
			}
		})
	}
	t.Run("pinned leaf with a stale snapshot", func(t *testing.T) {
		c, leafRepo := reasonCache(t)
		lf := types.NewID()
		c.warm(nativeLeaf(lf, 2, false, 0), leafRepo)
		vol := types.NewID()
		c.stageUnitSets(types.NewID(), lf, 2, 1, []types.ID{vol}, nil)
		c.InvalidateLeaf(lf) // reads as maximally stale
		opts := capableOpts(vol, 0)
		opts.LeafIDs = []types.ID{lf}
		if nw := emptyHandOut(t, c, vol, opts); nw.reason != reasonUnspecified {
			t.Fatalf("reason = %v, want UNSPECIFIED (the snapshot cannot vouch the leaf is PUBLIC)", nw.reason)
		}
	})
	t.Run("control: any-leaf request, PUBLIC units all contributed", func(t *testing.T) {
		c, leafRepo := reasonCache(t)
		pub, hidden := types.NewID(), types.NewID()
		c.warm(withLeafVisibility(pub, leaf.VisibilityPublic), leafRepo)
		c.warm(withLeafVisibility(hidden, leaf.VisibilityPrivate), leafRepo)
		vol := types.NewID()
		c.stageUnitSets(types.NewID(), pub, 2, 1, []types.ID{vol}, nil)
		c.stageUnit(types.NewID(), hidden, 2, 0) // a hidden unit it could take must not spoil "every"
		if nw := emptyHandOut(t, c, vol, capableOpts(vol, 0)); nw.reason != reasonContributed {
			t.Fatalf("reason = %v, want ALREADY_CONTRIBUTED (the hidden leaf's unit is outside the scope)", nw.reason)
		}
	})
}

// A reply that carries work carries no reason, even when other units were refused.
func TestNoWorkReason_NotSetWhenWorkIsHandedOut(t *testing.T) {
	c, leafRepo := reasonCache(t)
	lf := types.NewID()
	c.warm(nativeLeaf(lf, 2, false, 0), leafRepo)
	vol := types.NewID()
	c.stageUnitSets(types.NewID(), lf, 2, 1, []types.ID{vol}, nil)
	c.stageUnit(types.NewID(), lf, 2, 0)
	opts := capableOpts(vol, 0)
	opts.LeafIDs = []types.ID{lf}
	res, _, nw := c.HandOutWithReason(vol, opts, 1)
	if len(res) != 1 {
		t.Fatalf("hand-out = %d units, want 1", len(res))
	}
	if nw.reason != reasonUnspecified {
		t.Fatalf("reason = %v on a reply with work, want UNSPECIFIED", nw.reason)
	}
}

// The deadline class end to end through RequestWorkUnit (the audit's H2 shape): benchmark
// 1e9 on the account's stored hardware, estimate 1e12 (about 1000 s), deadline 600 s. The
// reply names the class with both figures, and the head's Info-level WARN — which used to
// be silent at Info — writes its own message rather than the account arm's.
func TestNoWorkReason_InfeasibleDeadline_OnTheWireAndInTheWarn(t *testing.T) {
	svc, pub, volID, buf := reasonService(t, slog.LevelInfo, 1e9, 1e12, 600)
	resp, err := reasonRequest(t, svc, pub, volID)
	if err != nil {
		t.Fatalf("RequestWorkUnit: %v", err)
	}
	if len(resp.GetAssignments()) != 0 {
		t.Fatalf("assignments = %d, want 0", len(resp.GetAssignments()))
	}
	if resp.GetNoWorkReason() != reasonDeadline {
		t.Fatalf("no_work_reason = %v, want INFEASIBLE_DEADLINE", resp.GetNoWorkReason())
	}
	if resp.GetDeadlineSeconds() != 600 || resp.GetEstimatedSeconds() != 1000 {
		t.Errorf("deadline/estimate = %d/%d, want 600/1000", resp.GetDeadlineSeconds(), resp.GetEstimatedSeconds())
	}
	if resp.GetInflightCap() != 0 || resp.GetBenchedUntilUnix() != 0 {
		t.Errorf("reply carries other classes' figures: %v", resp)
	}
	warns := noWorkWarns(buf)
	if len(warns) != 1 {
		t.Fatalf("no-work WARNs = %d, want 1; log:\n%s", len(warns), buf.String())
	}
	msg, _ := warns[0]["msg"].(string)
	if !strings.Contains(msg, "cannot finish before their deadlines") || strings.Contains(msg, "this account specifically") {
		t.Errorf("WARN msg = %q, want the deadline arm", msg)
	}
	if got := recInt(warns[0], "refused_"+rejectInfeasibleDeadline.String()); got != 1 {
		t.Errorf("refused_%s = %d, want 1", rejectInfeasibleDeadline, got)
	}
}

// The deadline controls from the audit get work and no reason: a deadline the host can
// meet, the equal-seconds boundary, no benchmark on record, and no estimate on the leaf.
func TestNoWorkReason_InfeasibleDeadline_FeasibleControlsGetWork(t *testing.T) {
	cases := []struct {
		name            string
		bench, est      float64
		deadlineSeconds int
	}{
		{"1000 s against 1200 s", 1e9, 1e12, 1200},
		{"1000 s against 1000 s", 1e9, 1e12, 1000},
		{"no benchmark", 0, 1e12, 600},
		{"no estimate", 1e9, 0, 600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, pub, volID, _ := reasonService(t, slog.LevelInfo, tc.bench, tc.est, tc.deadlineSeconds)
			resp, err := reasonRequest(t, svc, pub, volID)
			if err != nil {
				t.Fatalf("RequestWorkUnit: %v", err)
			}
			if len(resp.GetAssignments()) != 1 || resp.GetNoWorkReason() != reasonUnspecified {
				t.Fatalf("assignments = %d, reason = %v; want 1 and UNSPECIFIED", len(resp.GetAssignments()), resp.GetNoWorkReason())
			}
		})
	}
}

// An account BENCHED by standing now raises the head's WARN on its own, with its own
// message: it used to show only inside a WARN raised for another reason.
func TestNoWorkWarn_AccountBenchedHasItsOwnArm(t *testing.T) {
	c, leafRepo, buf, _ := scopeCache(t)
	lf := types.NewID()
	c.warm(nativeLeaf(lf, 2, false, 0), leafRepo)
	c.stageUnit(types.NewID(), lf, 2, 0)
	vol := types.NewID()
	benchAccount(c, vol, nil)
	opts := capableOpts(vol, 0)
	opts.LeafIDs = []types.ID{lf}
	emptyHandOut(t, c, vol, opts)
	warns := noWorkWarns(buf)
	if len(warns) != 1 {
		t.Fatalf("no-work WARNs = %d, want 1; log:\n%s", len(warns), buf.String())
	}
	msg, _ := warns[0]["msg"].(string)
	if !strings.Contains(msg, "BENCHED (standing)") || strings.Contains(msg, "this account specifically") {
		t.Errorf("WARN msg = %q, want the standing arm", msg)
	}
	if got := recInt(warns[0], "refused_"+rejectStandingBenched.String()); got != 1 {
		t.Errorf("refused_%s = %d, want 1", rejectStandingBenched, got)
	}
}

// reasonService builds a volunteerService over a dispatch cache with one warmed account
// whose STORED hardware carries benchmarkFPOPS (the figure the deadline check reads), one
// leaf with rsc_fpops_est = fpopsEst, and one staged unit with the given deadline. The
// service and cache log to the returned buffer at level.
func reasonService(t *testing.T, level slog.Level, benchmarkFPOPS, fpopsEst float64, deadlineSeconds int) (*volunteerService, ed25519.PublicKey, types.ID, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level}))
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	volID := types.NewID()
	vol := &volunteer.Volunteer{
		ID:                volID,
		PublicKey:         pub,
		AvailableRuntimes: []string{leaf.RuntimeNative},
		HardwareCapabilities: volunteer.HardwareCapabilities{
			CPUCores: 8, MaxCPUCores: 8, MemoryTotalMB: 8192, MaxMemoryMB: 4096,
			DiskAvailableMB: 100000, MaxDiskMB: 100000, BenchmarkFPOPS: benchmarkFPOPS,
		},
	}
	leafRepo := &fakeLeafRepo{}
	cache := newTestCache(&fakeWURepo{}, leafRepo, &fakeAssignRepo{})
	cache.logger = logger
	cache.deps.volunteerRepo = &fakeVolunteerRepo{vols: map[types.ID]*volunteer.Volunteer{volID: vol}}
	cache.putIdentity(vol)

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 1, false, 0)
	lf.ExecutionConfig.RscFpopsEst = fpopsEst
	cache.warm(lf, leafRepo)
	cache.stageUnit(types.NewID(), leafID, 1, 0)
	cache.mu.Lock()
	cache.ready[len(cache.ready)-1].unit.DeadlineSeconds = deadlineSeconds
	cache.mu.Unlock()

	svc := &volunteerService{
		dispatchCache:      cache,
		logger:             logger,
		now:                time.Now,
		loadEstimator:      newLoadEstimator(defaultLoadEstimatorConfig(), nil),
		maxBatchPerRequest: 5,
	}
	return svc, pub, volID, buf
}

// reasonRequest sends one authenticated any-leaf RequestWorkUnit for the account.
func reasonRequest(t *testing.T, svc *volunteerService, pub ed25519.PublicKey, volID types.ID) (*lettucev1.RequestWorkUnitResponse, error) {
	t.Helper()
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)
	return svc.RequestWorkUnit(ctx, &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    volID.String(),
		PublicKey:      pub,
		MaxAssignments: 1,
	})
}
