package server

// PB-21 / PB-25 regression tests (Phase 3 local campaign). Differential: written
// against pre-fix seams (redisReplayStore/SeenWithin, runRefiller + the fakeWURepo
// dispatch seam), so this file compiles on the pre-fix tree and FAILS there.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	"github.com/redis/go-redis/v9"
)

// hangingSetNXRedis models a Redis OUTAGE the way the campaign observed it: the
// client's dial/DNS retries burn wall-clock until the CALLER's context expires
// ("failed to dial after 5 attempts: lookup redis: i/o timeout" only surfaced at
// ~30s — the full client RPC deadline). SetNX blocks until ctx is done, then
// returns ctx's error.
type hangingSetNXRedis struct{}

func (hangingSetNXRedis) SetNX(ctx context.Context, _ string, _ interface{}, _ time.Duration) *redis.BoolCmd {
	<-ctx.Done()
	cmd := redis.NewBoolCmd(ctx)
	cmd.SetErr(ctx.Err())
	return cmd
}

// TestRedisReplayStore_OutageAnswersWithinInternalTimeout (PB-21): during a Redis
// outage the replay-store call must error within a SHORT internal budget so the
// configured fail-mode policy actually answers inside the client's RPC deadline.
// Pre-fix there was no internal bound — the call inherited the full request
// context, so with a 30s client deadline every signed RPC hung ~30s in BOTH replay
// fail-modes: fail-open only "admitted" after the client had already gone
// (context canceled) and fail-closed's Unavailable never reached the client either.
func TestRedisReplayStore_OutageAnswersWithinInternalTimeout(t *testing.T) {
	store := newRedisReplayStore(hangingSetNXRedis{})

	// The caller's context is effectively unbounded relative to the internal budget
	// (a client RPC deadline of 30s); the store must NOT ride it to the end.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	type outcome struct {
		seen bool
		err  error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		seen, err := store.SeenWithin(ctx, []byte("sig-outage"), ed25519TimestampSkew)
		done <- outcome{seen, err}
	}()

	select {
	case o := <-done:
		elapsed := time.Since(start)
		if o.err == nil {
			t.Fatalf("outage SeenWithin returned nil error (seen=%v); a store failure must surface so the fail-mode policy can decide", o.seen)
		}
		if o.seen {
			t.Fatal("outage SeenWithin fabricated alreadySeen=true; legitimate traffic would be silently rejected during an outage")
		}
		if elapsed > 10*time.Second {
			t.Fatalf("outage SeenWithin took %v; the internal budget must answer well inside the client deadline", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("replay-store call still blocked after 10s of a simulated Redis outage: every signed RPC stalls for the full client deadline and the configured fail-mode policy is unreachable (PB-21)")
	}
}

// errSetNXRedis fails immediately (the transport-error shape the existing
// fail-open/fail-closed policy tests use).
type errSetNXRedis struct{ err error }

func (f errSetNXRedis) SetNX(ctx context.Context, _ string, _ interface{}, _ time.Duration) *redis.BoolCmd {
	cmd := redis.NewBoolCmd(ctx)
	cmd.SetErr(f.err)
	return cmd
}

// TestRedisReplayStore_ImmediateErrorStillSurfaces (PB-21 control): a fast store
// error keeps surfacing unchanged — the internal bound must not swallow it.
func TestRedisReplayStore_ImmediateErrorStillSurfaces(t *testing.T) {
	boom := errors.New("connection refused")
	store := newRedisReplayStore(errSetNXRedis{err: boom})
	seen, err := store.SeenWithin(context.Background(), []byte("sig-fast-err"), ed25519TimestampSkew)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the store error", err)
	}
	if seen {
		t.Fatal("alreadySeen fabricated on error")
	}
}

// recordingHandler captures slog records at every level for log-level assertions.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) watermarkRecords() (warns, debugs int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if !strings.Contains(r.Message, "ready pool below low watermark") {
			continue
		}
		switch {
		case r.Level >= slog.LevelWarn:
			warns++
		case r.Level == slog.LevelDebug:
			debugs++
		}
	}
	return warns, debugs
}

// runRefillerBriefly drives the real refiller loop with a fast tick for ~40 ticks.
func runRefillerBriefly(c *dispatchCache) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	c.runRefiller(ctx, 5*time.Millisecond)
}

// TestRefillerWatermark_IdleHeadDoesNotWarn (PB-25): a healthy, caught-up head —
// empty QUEUED backlog, so the pool sits below the watermark PERMANENTLY — must not
// WARN on every probe tick. Pre-fix this line fired at WARN every ~2s forever
// (~42k/day measured on an idle head), drowning real WARNs and burning the log cap;
// it is Debug in the empty-backlog case now.
func TestRefillerWatermark_IdleHeadDoesNotWarn(t *testing.T) {
	h := &recordingHandler{}
	wuRepo := &fakeWURepo{} // dispatchFn nil: the dispatchable universe is empty
	c := newDispatchCache(dispatchCacheConfig{
		readyPoolSize:   100,
		lowWatermark:    10,
		refillBatchSize: 50,
		admissionCap:    4,
		flushInterval:   time.Hour,
		flushBatchSize:  200,
		leaseSeconds:    900,
	}, dispatchDeps{wuRepo: wuRepo, leafRepo: &fakeLeafRepo{}, assignRepo: &fakeAssignRepo{}}, slog.New(h))

	runRefillerBriefly(c)

	warns, debugs := h.watermarkRecords()
	if warns > 0 {
		t.Fatalf("idle (empty-backlog) head emitted %d WARN 'ready pool below low watermark' lines in ~40 refill ticks: the permanent state of a caught-up head must not be WARN spam (PB-25)", warns)
	}
	if debugs == 0 {
		t.Fatal("watermark probe emitted nothing at Debug: the below-watermark signal must remain observable")
	}
}

// TestRefillerWatermark_StarvedWithWorkStillWarns (PB-25 control): when the
// dispatchable query DOES return work and the pool still cannot reach the
// watermark (demand outpacing refill), the probe must keep WARNING — that is the
// actionable case the line exists for.
func TestRefillerWatermark_StarvedWithWorkStillWarns(t *testing.T) {
	h := &recordingHandler{}
	leafID := types.NewID()
	wuRepo := &fakeWURepo{}
	// Each refill returns ONE fresh candidate: the pool grows far slower than the
	// watermark, so it stays low while work demonstrably exists.
	wuRepo.dispatchFn = func(limit int, excludeIDs, leafIDs []types.ID) ([]workunit.DispatchCandidate, error) {
		return []workunit.DispatchCandidate{{
			WorkUnit: &workunit.WorkUnit{ID: types.NewID(), LeafID: leafID, State: workunit.WorkUnitStateQueued},
			LeafID:   leafID, RedundancyFactor: 1, Runtime: "NATIVE",
		}}, nil
	}
	c := newDispatchCache(dispatchCacheConfig{
		readyPoolSize:   1000,
		lowWatermark:    900,
		refillBatchSize: 1,
		admissionCap:    4,
		flushInterval:   time.Hour,
		flushBatchSize:  200,
		leaseSeconds:    900,
	}, dispatchDeps{wuRepo: wuRepo, leafRepo: &fakeLeafRepo{}, assignRepo: &fakeAssignRepo{}}, slog.New(h))

	runRefillerBriefly(c)

	warns, _ := h.watermarkRecords()
	if warns == 0 {
		t.Fatal("starved-with-waiting-work pool emitted no WARN: the actionable watermark signal was lost")
	}
}
