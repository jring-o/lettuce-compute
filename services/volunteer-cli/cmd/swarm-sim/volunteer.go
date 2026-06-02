package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	mrand "math/rand"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// profileKind selects the volunteer behavior model.
type profileKind string

const (
	profileNaive    profileKind = "naive"
	profileBuffered profileKind = "buffered"
)

// simConfig is the resolved, immutable configuration shared by every simulated
// volunteer goroutine.
type simConfig struct {
	headGRPC      string
	profile       profileKind
	leafID        string
	naiveInterval time.Duration
	bufferHours   float64
	maxAssign     int32
	simFpops      float64       // pretend benchmark FLOPS; pretend-compute = rsc_fpops_est/simFpops
	maxCompute    time.Duration // cap on pretend-compute so the run progresses
	logger        *slog.Logger
}

// heldUnit is a reserved-but-not-yet-run descriptor in the buffered profile's
// in-memory client work buffer.
type heldUnit struct {
	id            string
	estCompute    time.Duration
	reservedUntil int64
}

// simVolunteer is one simulated participant: its own keypair, its own signing
// client, and its own buffer state.
type simVolunteer struct {
	cfg    *simConfig
	m      *metrics
	id     string
	pub    ed25519.PublicKey
	priv   ed25519.PrivateKey
	cl     *client.Client
	rng    *mrand.Rand
	buffer []heldUnit
}

func newSimVolunteer(cfg *simConfig, m *metrics, seed int64) (*simVolunteer, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate volunteer key: %w", err)
	}
	cl, err := client.New(client.ClientConfig{
		ServerURL: cfg.headGRPC,
		Insecure:  true,
		Identity:  &client.Identity{PublicKey: pub, PrivateKey: priv},
	}, cfg.logger)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}
	return &simVolunteer{
		cfg:  cfg,
		m:    m,
		pub:  pub,
		priv: priv,
		cl:   cl,
		rng:  mrand.New(mrand.NewSource(seed)),
	}, nil
}

func (v *simVolunteer) close() {
	if v.cl != nil {
		_ = v.cl.Close()
	}
}

// register performs the RegisterVolunteer handshake; returns the assigned ID.
//
// Registration is one-time setup, not part of the steady-state request-rate
// measurement, so it retries through the per-client rate limiter
// (ResourceExhausted): when a large fleet shares one source IP (e.g. a loopback
// load test), the pre-auth per-IP limiter throttles the registration burst.
// Retrying with backoff lets the whole fleet come up without polluting the
// RequestWorkUnit metrics that the profiles are there to compare.
func (v *simVolunteer) register(ctx context.Context, name string) error {
	req := &lettucev1.RegisterVolunteerRequest{
		PublicKey:   v.pub,
		DisplayName: name,
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores:        8,
			CpuModel:        "swarm-sim",
			MaxCpuCores:     8,
			MemoryTotalMb:   16384,
			MaxMemoryMb:     16384,
			DiskAvailableMb: 102400,
			MaxDiskMb:       102400,
			BenchmarkFpops:  v.cfg.simFpops,
		},
		AvailableRuntimes: []string{"NATIVE"},
		SchedulingMode:    "ALWAYS",
	}
	const maxAttempts = 30
	for attempt := 0; attempt < maxAttempts; attempt++ {
		start := time.Now()
		resp, err := v.cl.RegisterVolunteer(ctx, req)
		v.m.record(rpcRegister, time.Since(start), outcomeFor(err))
		if err == nil {
			v.id = resp.GetVolunteerId()
			return nil
		}
		if status.Code(err) != codes.ResourceExhausted {
			return err
		}
		// Throttled by per-client rate limiting; back off (jittered) and retry.
		backoff := time.Duration(500+v.rng.Intn(1500)) * time.Millisecond
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return fmt.Errorf("register %s: still rate-limited after %d attempts", name, maxAttempts)
}

// run drives the volunteer until ctx is cancelled, dispatching to the profile.
//
// A short randomized startup jitter de-synchronizes the fleet's first contact
// so a large swarm does not stampede the head's assignment transaction at the
// identical instant (this also mirrors real fleets, which never wake in
// lockstep).
func (v *simVolunteer) run(ctx context.Context) {
	if !v.jitterStart(ctx) {
		return
	}
	switch v.cfg.profile {
	case profileBuffered:
		v.runBuffered(ctx)
	default:
		v.runNaive(ctx)
	}
}

// runNaive approximates a pre-Layer-1 client: it loops RequestWorkUnit with
// MaxAssignments=1 at a FIXED interval, IGNORING retry_after_seconds, runs each
// returned unit immediately (pretend-compute), then submits. It never renews
// reservations (each unit flips to ASSIGNED at run start almost immediately).
func (v *simVolunteer) runNaive(ctx context.Context) {
	ticker := time.NewTicker(v.cfg.naiveInterval)
	defer ticker.Stop()
	for {
		assigns, _, ok := v.requestWork(ctx, 1)
		if ok {
			for _, a := range assigns {
				v.pretendComputeAndSubmit(ctx, a)
				if ctx.Err() != nil {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runBuffered implements the Layer 1 client model: it keeps an in-memory
// hours-sized buffer of reserved descriptors, OBEYS retry_after_seconds, makes
// ZERO RequestWorkUnit calls while the buffer is full, and asks for batches of
// MaxAssignments units sized to the remaining hours deficit. It drains the
// buffer by running (pretend-compute) and submitting units. ResourceExhausted
// is treated as a fixed jittered local backoff.
//
// NOTE: in the real volunteer a buffered unit's reservation lease
// (reserved_until) is kept alive by PREPARING heartbeats — the head bumps
// reserved_until on each — so there is no separate renewal RPC. This load
// simulator drains each buffered unit within a single loop iteration (well
// inside lease_seconds) and does not model PREPARING heartbeats, so it never
// needs to renew a lease; the reservedUntil field is carried purely so the
// drain order can be lease-aware if a future profile holds units longer.
func (v *simVolunteer) runBuffered(ctx context.Context) {
	var nextContact time.Time
	for {
		if ctx.Err() != nil {
			return
		}

		now := time.Now()
		// Respect the server-directed retry delay before re-contacting.
		if now.Before(nextContact) && !v.bufferFull() {
			if !v.sleepUntil(ctx, nextContact) {
				return
			}
		}

		if !v.bufferFull() {
			need := v.refillNeed()
			if need > 0 {
				assigns, retryAfter, ok := v.requestWork(ctx, need)
				if ok {
					for _, a := range assigns {
						v.buffer = append(v.buffer, heldUnit{
							id:            a.GetWorkUnitId(),
							estCompute:    v.estCompute(a.GetRscFpopsEst()),
							reservedUntil: a.GetReservedUntilUnix(),
						})
					}
				}
				if retryAfter > 0 {
					nextContact = time.Now().Add(time.Duration(retryAfter) * time.Second)
				}
			}
		}

		// Drain one buffered unit (run + submit). If the buffer is empty and we
		// are waiting on a delay, sleep a short beat so we don't spin.
		if len(v.buffer) > 0 {
			h := v.buffer[0]
			v.buffer = v.buffer[1:]
			v.pretendCompute(ctx, h.estCompute)
			if ctx.Err() != nil {
				return
			}
			// Run-start (RUNNING heartbeat) flips the reserved QUEUED unit to
			// ASSIGNED and creates its assignment_history row, then submit.
			v.runAndSubmit(ctx, h.id)
		} else if time.Now().Before(nextContact) {
			if !v.sleepUntil(ctx, nextContact) {
				return
			}
		}
	}
}

// requestWork issues a single RequestWorkUnit, records metrics + the observed
// retry delay, and returns the assignments, retry_after_seconds, and whether
// the call succeeded. ResourceExhausted is recorded as throttled and surfaces a
// fixed jittered local backoff via the returned retry-after seconds.
func (v *simVolunteer) requestWork(ctx context.Context, maxAssign int32) ([]*lettucev1.WorkUnitAssignment, int32, bool) {
	req := &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    v.id,
		PublicKey:      v.pub,
		LeafIds:        []string{v.cfg.leafID},
		MaxAssignments: maxAssign,
	}
	start := time.Now()
	resp, err := v.cl.RequestWorkUnit(ctx, req)
	dur := time.Since(start)
	if err != nil {
		if status.Code(err) == codes.ResourceExhausted {
			v.m.record(rpcRequestWorkUnit, dur, outcomeThrottled)
			return nil, v.resourceExhaustedBackoff(), false
		}
		v.m.record(rpcRequestWorkUnit, dur, outcomeError)
		return nil, 0, false
	}
	v.m.record(rpcRequestWorkUnit, dur, outcomeOK)

	retryAfter := resp.GetRetryAfterSeconds()
	v.m.recordRetryAfter(retryAfter)

	assigns := resp.GetAssignments()
	if n := len(assigns); n > 0 {
		v.m.assignmentsDispatched.Add(int64(n))
	}
	return assigns, retryAfter, true
}

// pretendComputeAndSubmit simulates running a freshly-assigned unit then submits.
func (v *simVolunteer) pretendComputeAndSubmit(ctx context.Context, a *lettucev1.WorkUnitAssignment) {
	v.pretendCompute(ctx, v.estCompute(a.GetRscFpopsEst()))
	if ctx.Err() != nil {
		return
	}
	v.runAndSubmit(ctx, a.GetWorkUnitId())
}

// runAndSubmit models the run-start flow before submitting. A dispatched unit is
// only RESERVED (state=QUEUED) — it has no active assignment_history row, so a
// bare SubmitResult fails the head's precondition with FailedPrecondition
// "no active assignment". The real volunteer's first RUNNING heartbeat flips the
// reserved unit QUEUED->ASSIGNED and creates that history row (the run-start
// transition in the Heartbeat handler); only then can the result be submitted.
// This mirrors that: send one RUNNING heartbeat to start the run, and submit only
// if the head confirms the unit is live (ContinueExecution=true). If the head
// says the unit is no longer active (reservation lapsed / reclaimed), drop it
// without submitting.
func (v *simVolunteer) runAndSubmit(ctx context.Context, wuID string) {
	if !v.startRun(ctx, wuID) {
		return
	}
	if ctx.Err() != nil {
		return
	}
	v.submitResult(ctx, wuID)
}

// startRun sends the RUNNING heartbeat that flips a reserved (QUEUED) unit to
// ASSIGNED at run-start, satisfying SubmitResult's active-assignment precondition.
// Returns true if the unit is live and the result may be submitted.
func (v *simVolunteer) startRun(ctx context.Context, wuID string) bool {
	start := time.Now()
	resp, err := v.cl.Heartbeat(ctx, &lettucev1.HeartbeatRequest{
		WorkUnitId:  wuID,
		VolunteerId: v.id,
		Status:      "RUNNING",
		ProgressPct: 1.0,
	})
	v.m.record(rpcHeartbeat, time.Since(start), outcomeFor(err))
	if err != nil {
		return false
	}
	return resp.GetContinueExecution()
}

func (v *simVolunteer) submitResult(ctx context.Context, wuID string) {
	// Output is stored as JSONB on the head, so it must be valid JSON.
	output := []byte(fmt.Sprintf(`{"work_unit_id":%q,"value":1}`, wuID))
	sum := sha256.Sum256(output)
	start := time.Now()
	_, err := v.cl.SubmitResult(ctx, &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuID,
		VolunteerId:          v.id,
		PublicKey:            v.pub,
		OutputData:           output,
		OutputChecksumSha256: hex.EncodeToString(sum[:]),
		Metadata: &lettucev1.ExecutionMetadata{
			WallClockSeconds: 1,
			CpuSecondsUser:   1,
			CpuCoresUsed:     1,
			PeakMemoryMb:     128,
		},
	})
	v.m.record(rpcSubmitResult, time.Since(start), outcomeFor(err))
	if err == nil {
		v.m.resultsSubmitted.Add(1)
	}
}

// estCompute converts an rsc_fpops_est into a pretend-compute duration, capped.
func (v *simVolunteer) estCompute(rscFpops float64) time.Duration {
	if v.cfg.simFpops <= 0 || rscFpops <= 0 {
		return time.Millisecond
	}
	secs := rscFpops / v.cfg.simFpops
	d := time.Duration(secs * float64(time.Second))
	if d > v.cfg.maxCompute {
		d = v.cfg.maxCompute
	}
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d
}

func (v *simVolunteer) pretendCompute(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// bufferFull reports whether the in-memory buffer holds at least bufferHours of
// estimated work. Each unit is seedRscFpopsEst FLOPs; a full buffer means we
// make zero RequestWorkUnit calls (Layer 1 DoD #2).
func (v *simVolunteer) bufferFull() bool {
	return v.bufferedSeconds() >= v.cfg.bufferHours*3600.0
}

func (v *simVolunteer) bufferedSeconds() float64 {
	var total float64
	for _, h := range v.buffer {
		total += h.estCompute.Seconds()
	}
	return total
}

// refillNeed returns how many units to request to approach the hours target,
// clamped to [1, maxAssign].
func (v *simVolunteer) refillNeed() int32 {
	if v.cfg.simFpops <= 0 {
		return v.cfg.maxAssign
	}
	estPerUnit := v.estCompute(seedRscFpopsEst).Seconds()
	if estPerUnit <= 0 {
		return v.cfg.maxAssign
	}
	deficit := v.cfg.bufferHours*3600.0 - v.bufferedSeconds()
	if deficit <= 0 {
		return 0
	}
	need := int32(math.Ceil(deficit / estPerUnit))
	if need < 1 {
		need = 1
	}
	if need > v.cfg.maxAssign {
		need = v.cfg.maxAssign
	}
	return need
}

// resourceExhaustedBackoff returns a fixed jittered local backoff (default 30s
// ±20%, capped at 900s) in seconds, mirroring the volunteer daemon's treatment
// of codes.ResourceExhausted: a pure local backoff, NOT a server-directed delay.
func (v *simVolunteer) resourceExhaustedBackoff() int32 {
	const base = 30.0
	const cap = 900.0
	jitter := 1.0 + (v.rng.Float64()*0.4 - 0.2)
	d := base * jitter
	if d > cap {
		d = cap
	}
	if d < 1 {
		d = 1
	}
	return int32(d)
}

// jitterStart sleeps a random fraction of the naive interval (capped at 1s) so
// the fleet does not contact the head in lockstep on the first tick. Returns
// false if ctx was cancelled during the sleep.
func (v *simVolunteer) jitterStart(ctx context.Context) bool {
	maxJitter := v.cfg.naiveInterval
	if maxJitter <= 0 || maxJitter > time.Second {
		maxJitter = time.Second
	}
	d := time.Duration(v.rng.Int63n(int64(maxJitter) + 1))
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (v *simVolunteer) sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func outcomeFor(err error) rpcOutcome {
	if err == nil {
		return outcomeOK
	}
	if status.Code(err) == codes.ResourceExhausted {
		return outcomeThrottled
	}
	return outcomeError
}

// errNotRegistered is returned when a volunteer fails to register.
var errNotRegistered = errors.New("volunteer not registered")
