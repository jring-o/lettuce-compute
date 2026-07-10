package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/audit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// statusMessage returns the gRPC status message of err (empty if err is nil).
func statusMessage(err error) string {
	st, _ := status.FromError(err)
	return st.Message()
}

// --- Fakes (narrow consumer-side views the AuditService depends on) ---

type fakeAuditVolunteerRepo struct {
	byKey map[string]*volunteer.Volunteer
}

func (f *fakeAuditVolunteerRepo) GetByPublicKey(_ context.Context, key []byte) (*volunteer.Volunteer, error) {
	if v, ok := f.byKey[string(key)]; ok {
		return v, nil
	}
	return nil, apierror.NotFound("volunteer", "public_key")
}

type fakeAuditWURepo struct {
	wus map[types.ID]*workunit.WorkUnit
}

func (f *fakeAuditWURepo) GetByID(_ context.Context, id types.ID) (*workunit.WorkUnit, error) {
	if wu, ok := f.wus[id]; ok {
		return wu, nil
	}
	return nil, apierror.NotFound("work_unit", id.String())
}

type fakeAuditLeafRepo struct {
	leafs map[types.ID]*leaf.Leaf
}

func (f *fakeAuditLeafRepo) GetByID(_ context.Context, id types.ID) (*leaf.Leaf, error) {
	if l, ok := f.leafs[id]; ok {
		return l, nil
	}
	return nil, apierror.NotFound("leaf", id.String())
}

type fakeAuditResultRepo struct {
	byWU map[types.ID][]*result.Result
	err  error
}

func (f *fakeAuditResultRepo) ListByWorkUnit(_ context.Context, wuID types.ID) ([]*result.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byWU[wuID], nil
}

// fakeRunnersRepo implements audit.RunnersRepository; only GetActiveByVolunteerID is
// exercised (registry membership is the audit-surface authorization).
type fakeRunnersRepo struct {
	active map[types.ID]*audit.Runner // keyed by volunteer id
	err    error
}

func (f *fakeRunnersRepo) Register(context.Context, types.ID, string, string) (*audit.Runner, error) {
	return nil, nil
}
func (f *fakeRunnersRepo) Deactivate(context.Context, types.ID) error             { return nil }
func (f *fakeRunnersRepo) List(context.Context) ([]*audit.Runner, error)          { return nil, nil }
func (f *fakeRunnersRepo) ActiveRunnerSubjects(context.Context) ([]string, error) { return nil, nil }
func (f *fakeRunnersRepo) GetActiveByVolunteerID(_ context.Context, volID types.ID) (*audit.Runner, error) {
	if f.err != nil {
		return nil, f.err
	}
	if r, ok := f.active[volID]; ok {
		return r, nil
	}
	return nil, audit.ErrNotRegistered
}

type inconclusiveCall struct {
	id, runner types.ID
	detail     string
}
type verdictCall struct {
	id, runner types.ID
	verdict    audit.Verdict
	detail     string
	output     []byte
	checksum   string
}
type releaseCall struct {
	id, runner types.ID
	errMsg     string
}

// fakeAuditsRepo implements audit.AuditsRepository. Claim() returns claimResults in
// order (a nil element models "nothing claimable"); the completion calls are recorded.
type fakeAuditsRepo struct {
	claimResults []*audit.Audit
	claimIdx     int
	claimErr     error

	rows map[types.ID]*audit.Audit

	inconclusive    []inconclusiveCall
	inconclusiveErr error
	verdicts        []verdictCall
	verdictErr      error
	releases        []releaseCall
	releaseErr      error
}

func (f *fakeAuditsRepo) Enqueue(context.Context, *audit.Audit) error { return nil }
func (f *fakeAuditsRepo) Claim(_ context.Context, _ types.ID, _ string) (*audit.Audit, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if f.claimIdx >= len(f.claimResults) {
		return nil, nil
	}
	a := f.claimResults[f.claimIdx]
	f.claimIdx++
	return a, nil
}
func (f *fakeAuditsRepo) GetByID(_ context.Context, id types.ID) (*audit.Audit, error) {
	if a, ok := f.rows[id]; ok {
		return a, nil
	}
	return nil, apierror.NotFound("audit", id.String())
}
func (f *fakeAuditsRepo) CompleteVerdict(_ context.Context, id, runnerID types.ID, v audit.Verdict, detail string, output []byte, checksum string, _ bool) error {
	if f.verdictErr != nil {
		return f.verdictErr
	}
	f.verdicts = append(f.verdicts, verdictCall{id, runnerID, v, detail, output, checksum})
	return nil
}
func (f *fakeAuditsRepo) CompleteInconclusive(_ context.Context, id, runnerID types.ID, detail string) error {
	if f.inconclusiveErr != nil {
		return f.inconclusiveErr
	}
	f.inconclusive = append(f.inconclusive, inconclusiveCall{id, runnerID, detail})
	return nil
}
func (f *fakeAuditsRepo) ReleaseFailure(_ context.Context, id, runnerID types.ID, errMsg string) error {
	if f.releaseErr != nil {
		return f.releaseErr
	}
	f.releases = append(f.releases, releaseCall{id, runnerID, errMsg})
	return nil
}
func (f *fakeAuditsRepo) SweepLapsedLeases(context.Context) (int, int, error) { return 0, 0, nil }
func (f *fakeAuditsRepo) SweepStaleQueued(context.Context) (int, error)       { return 0, nil }
func (f *fakeAuditsRepo) Stats(context.Context) (audit.Stats, error)          { return audit.Stats{}, nil }
func (f *fakeAuditsRepo) List(context.Context, audit.ListFilter) ([]*audit.Audit, error) {
	return nil, nil
}
func (f *fakeAuditsRepo) EnqueueConfirmation(context.Context, types.ID) (*audit.Audit, error) {
	return nil, nil
}
func (f *fakeAuditsRepo) GetRunnerOutput(context.Context, types.ID) ([]byte, error) {
	return nil, nil
}
func (f *fakeAuditsRepo) ListActionableRoots(context.Context, int) ([]*audit.Audit, error) {
	return nil, nil
}
func (f *fakeAuditsRepo) ConfirmationsForRoot(context.Context, types.ID) ([]*audit.Audit, error) {
	return nil, nil
}
func (f *fakeAuditsRepo) SetEnforcementState(context.Context, types.ID, audit.EnforcementState) (bool, error) {
	return false, nil
}
func (f *fakeAuditsRepo) ClaimRepair(context.Context, types.ID, types.ID) (bool, error) {
	return false, nil
}
func (f *fakeAuditsRepo) FlaggedLeaves(context.Context) ([]audit.FlaggedLeaf, error) {
	return nil, nil
}

// capturingHandler records emitted log records so a test can assert a WARN was made.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) hasWarn(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == slog.LevelWarn && r.Message == msg {
			return true
		}
	}
	return false
}

func stubAdjudicator(verdict audit.Verdict, detail string, err error) audit.Adjudicator {
	return func(_ audit.ComparisonSnapshot, _ string, _ []json.RawMessage, _ []byte) (audit.Verdict, string, error) {
		return verdict, detail, err
	}
}

// --- Harness ---

type auditHarness struct {
	svc      lettucev1.AuditServiceServer
	runners  *fakeRunnersRepo
	audits   *fakeAuditsRepo
	wus      *fakeAuditWURepo
	leafs    *fakeAuditLeafRepo
	vols     *fakeAuditVolunteerRepo
	results  *fakeAuditResultRepo
	logs     *capturingHandler
	pub      ed25519.PublicKey
	volID    types.ID
	runnerID types.ID
}

func newAuditHarness(t *testing.T, adj audit.Adjudicator) *auditHarness {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	volID := types.NewID()
	runnerID := types.NewID()

	vols := &fakeAuditVolunteerRepo{byKey: map[string]*volunteer.Volunteer{
		string(pub): {ID: volID, PublicKey: pub, IsActive: true},
	}}
	runners := &fakeRunnersRepo{active: map[types.ID]*audit.Runner{
		volID: {ID: runnerID, VolunteerID: volID, Active: true},
	}}
	audits := &fakeAuditsRepo{rows: map[types.ID]*audit.Audit{}}
	wus := &fakeAuditWURepo{wus: map[types.ID]*workunit.WorkUnit{}}
	leafs := &fakeAuditLeafRepo{leafs: map[types.ID]*leaf.Leaf{}}
	results := &fakeAuditResultRepo{byWU: map[types.ID][]*result.Result{}}
	logs := &capturingHandler{}

	svc := NewAuditService(runners, audits, wus, leafs, vols, adj, results, false, slog.New(logs))
	return &auditHarness{svc, runners, audits, wus, leafs, vols, results, logs, pub, volID, runnerID}
}

func (h *auditHarness) authedCtx() context.Context {
	return contextWithGRPCAuthPublicKey(context.Background(), h.pub)
}

func intPtr(i int) *int { return &i }

// --- Auth chain ---

func TestAuditClaimJob_Unauthenticated(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))
	// No verified pubkey bound into the context (public method / missing auth).
	_, err := h.svc.ClaimJob(context.Background(), &lettucev1.ClaimAuditJobRequest{})
	if codeOf(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %s: %v", codeOf(err), err)
	}
}

func TestAuditClaimJob_UnknownVolunteer(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))
	// A verified key that no volunteer row matches → Unauthenticated (identity is the key).
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	ctx := contextWithGRPCAuthPublicKey(context.Background(), otherPub)
	_, err := h.svc.ClaimJob(ctx, &lettucev1.ClaimAuditJobRequest{})
	if codeOf(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %s: %v", codeOf(err), err)
	}
}

func TestAuditClaimJob_NotRegistered_PermissionDenied(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))
	// Known volunteer, but no trusted_runners row at all.
	delete(h.runners.active, h.volID)

	_, err := h.svc.ClaimJob(h.authedCtx(), &lettucev1.ClaimAuditJobRequest{})
	if codeOf(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %s: %v", codeOf(err), err)
	}
	if got := statusMessage(err); got != notActiveRunnerMessage {
		t.Fatalf("expected constant message %q, got %q", notActiveRunnerMessage, got)
	}
}

func TestAuditClaimJob_InactiveRunner_PermissionDenied(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))
	// Model an inactive runner: GetActiveByVolunteerID returns only ACTIVE rows, so an
	// inactive account surfaces exactly as ErrNotRegistered.
	h.runners.active = map[types.ID]*audit.Runner{}
	h.runners.err = audit.ErrNotRegistered

	_, err := h.svc.ClaimJob(h.authedCtx(), &lettucev1.ClaimAuditJobRequest{})
	if codeOf(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %s: %v", codeOf(err), err)
	}
	if got := statusMessage(err); got != notActiveRunnerMessage {
		t.Fatalf("expected constant message %q, got %q", notActiveRunnerMessage, got)
	}
}

func TestAuditSubmitResult_Unauthenticated(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))
	_, err := h.svc.SubmitResult(context.Background(), &lettucev1.SubmitAuditResultRequest{AuditId: types.NewID().String()})
	if codeOf(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %s: %v", codeOf(err), err)
	}
}

// --- ClaimJob happy path: snapshot exec config, lease + cleared checkpoints ---

func TestAuditClaimJob_HappyPath_SnapshotAndClearedCheckpoints(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))

	auditID := types.NewID()
	wuID := types.NewID()
	leafID := types.NewID()
	lease := time.Now().Add(20 * time.Minute).UTC().Truncate(time.Second)

	// The leaf's CURRENT config differs from the sampling-time snapshot, and the leaf
	// enables checkpointing — so a correct claim must (a) use the SNAPSHOT runtime and
	// (b) clear the checkpoint fields buildWorkUnitAssignment would otherwise set.
	h.leafs.leafs[leafID] = &leaf.Leaf{
		ID:              leafID,
		ExecutionConfig: leaf.ExecutionConfig{Runtime: "LEAF_CURRENT_RUNTIME"},
		FaultToleranceConfig: leaf.FaultToleranceConfig{
			CheckpointingEnabled:      true,
			CheckpointIntervalSeconds: intPtr(30),
		},
	}
	h.wus.wus[wuID] = &workunit.WorkUnit{
		ID:                     wuID,
		LeafID:                 leafID,
		LastCheckpointSequence: 5, // would set HasCheckpoint / CheckpointSequence
	}
	h.audits.claimResults = []*audit.Audit{{
		ID:                auditID,
		WorkUnitID:        wuID,
		LeafID:            leafID,
		ExecutionSnapshot: leaf.ExecutionConfig{Runtime: "SNAPSHOT_RUNTIME"},
		LeaseExpiresAt:    &lease,
		Status:            audit.StatusClaimed,
	}}

	resp, err := h.svc.ClaimJob(h.authedCtx(), &lettucev1.ClaimAuditJobRequest{})
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	job := resp.GetJob()
	if job == nil {
		t.Fatal("expected a job, got empty response")
	}
	if job.GetAuditId() != auditID.String() {
		t.Errorf("audit_id = %q, want %q", job.GetAuditId(), auditID.String())
	}
	if job.GetLeaseExpiresUnix() != lease.Unix() {
		t.Errorf("lease_expires_unix = %d, want %d", job.GetLeaseExpiresUnix(), lease.Unix())
	}
	a := job.GetAssignment()
	if a == nil {
		t.Fatal("expected an assignment")
	}
	if a.GetRuntime() != "SNAPSHOT_RUNTIME" {
		t.Errorf("runtime = %q, want SNAPSHOT_RUNTIME (must use the sampling-time snapshot, not the leaf's current config)", a.GetRuntime())
	}
	if a.GetReservedUntilUnix() != lease.Unix() {
		t.Errorf("reserved_until_unix = %d, want the audit lease %d", a.GetReservedUntilUnix(), lease.Unix())
	}
	if a.GetHasCheckpoint() {
		t.Error("has_checkpoint must be cleared for an audit re-execution (F-L3)")
	}
	if a.GetCheckpointSequence() != 0 {
		t.Errorf("checkpoint_sequence = %d, want 0 (cleared)", a.GetCheckpointSequence())
	}
	if a.GetCheckpointIntervalSeconds() != 0 {
		t.Errorf("checkpoint_interval_seconds = %d, want 0 (cleared)", a.GetCheckpointIntervalSeconds())
	}
}

// --- ClaimJob loop: a dangling unit is finalized INCONCLUSIVE, next job claimed ---

func TestAuditClaimJob_DanglingUnit_InconclusiveThenNext(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))

	danglingID := types.NewID()
	danglingWU := types.NewID() // deliberately NOT seeded into wus → not found
	danglingLeaf := types.NewID()

	goodID := types.NewID()
	goodWU := types.NewID()
	goodLeaf := types.NewID()
	lease := time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second)

	h.leafs.leafs[goodLeaf] = &leaf.Leaf{ID: goodLeaf, ExecutionConfig: leaf.ExecutionConfig{Runtime: "NATIVE"}}
	h.wus.wus[goodWU] = &workunit.WorkUnit{ID: goodWU, LeafID: goodLeaf}

	h.audits.claimResults = []*audit.Audit{
		{ID: danglingID, WorkUnitID: danglingWU, LeafID: danglingLeaf, ExecutionSnapshot: leaf.ExecutionConfig{Runtime: "NATIVE"}, LeaseExpiresAt: &lease},
		{ID: goodID, WorkUnitID: goodWU, LeafID: goodLeaf, ExecutionSnapshot: leaf.ExecutionConfig{Runtime: "NATIVE"}, LeaseExpiresAt: &lease},
	}

	resp, err := h.svc.ClaimJob(h.authedCtx(), &lettucev1.ClaimAuditJobRequest{})
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if resp.GetJob() == nil || resp.GetJob().GetAuditId() != goodID.String() {
		t.Fatalf("expected the good job %q, got %+v", goodID.String(), resp.GetJob())
	}
	if len(h.audits.inconclusive) != 1 {
		t.Fatalf("expected exactly one CompleteInconclusive call, got %d", len(h.audits.inconclusive))
	}
	ic := h.audits.inconclusive[0]
	if ic.id != danglingID {
		t.Errorf("inconclusive audit id = %v, want dangling %v", ic.id, danglingID)
	}
	if ic.runner != h.runnerID {
		t.Errorf("inconclusive runner id = %v, want %v", ic.runner, h.runnerID)
	}
	if len(ic.detail) < len(audit.ReasonArtifactUnavailable) || ic.detail[:len(audit.ReasonArtifactUnavailable)] != audit.ReasonArtifactUnavailable {
		t.Errorf("inconclusive detail = %q, want prefix %q", ic.detail, audit.ReasonArtifactUnavailable)
	}
}

func TestAuditClaimJob_NothingClaimable_EmptyResponse(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))
	// No claimResults → Claim returns (nil, nil).
	resp, err := h.svc.ClaimJob(h.authedCtx(), &lettucev1.ClaimAuditJobRequest{})
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if resp.GetJob() != nil {
		t.Fatalf("expected empty response, got job %+v", resp.GetJob())
	}
}

// --- SubmitResult: verdict paths ---

// seedVerdictAudit wires an audit row + one AGREED accepted result so the verdict
// path can run. Returns the audit id and the runner's output bytes to submit.
func seedVerdictAudit(h *auditHarness) (types.ID, []byte) {
	auditID := types.NewID()
	wuID := types.NewID()
	acceptedResultID := types.NewID()
	key := "abc123"
	h.audits.rows[auditID] = &audit.Audit{
		ID:                    auditID,
		WorkUnitID:            wuID,
		LeafID:                types.NewID(),
		AcceptedResultID:      acceptedResultID,
		AcceptedComparisonKey: &key,
		ComparisonSnapshot:    audit.ComparisonSnapshot{ComparisonMode: "EXACT"},
		Status:                audit.StatusClaimed,
	}
	h.results.byWU[wuID] = []*result.Result{
		{ID: acceptedResultID, WorkUnitID: wuID, ValidationStatus: result.ValidationAgreed, OutputData: json.RawMessage(`{"x":1}`)},
	}
	return auditID, []byte(`{"x":1}`)
}

func TestAuditSubmitResult_Match_HeadComputedChecksum(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))
	auditID, output := seedVerdictAudit(h)

	resp, err := h.svc.SubmitResult(h.authedCtx(), &lettucev1.SubmitAuditResultRequest{
		AuditId:    auditID.String(),
		OutputData: output,
	})
	if err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatal("expected accepted=true")
	}
	if len(h.audits.verdicts) != 1 {
		t.Fatalf("expected one CompleteVerdict call, got %d", len(h.audits.verdicts))
	}
	vc := h.audits.verdicts[0]
	if vc.verdict != audit.VerdictMatch {
		t.Errorf("verdict = %q, want MATCH", vc.verdict)
	}
	if vc.runner != h.runnerID {
		t.Errorf("verdict runner = %v, want %v", vc.runner, h.runnerID)
	}
	// The checksum stored must be the HEAD's hash of the submitted bytes.
	sum := sha256.Sum256(output)
	want := hex.EncodeToString(sum[:])
	if vc.checksum != want {
		t.Errorf("stored checksum = %q, want head-computed %q", vc.checksum, want)
	}
	if h.logs.hasWarn("result audit mismatch") {
		t.Error("MATCH must not emit the mismatch WARN")
	}
}

func TestAuditSubmitResult_Mismatch_WarnsAndRecordsHeadChecksum(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMismatch, "", nil))
	auditID, output := seedVerdictAudit(h)
	// A client-supplied checksum has no field to live in — prove the head hashes bytes.
	badOutput := append(output, []byte(`  `)...) // different bytes → different hash

	resp, err := h.svc.SubmitResult(h.authedCtx(), &lettucev1.SubmitAuditResultRequest{
		AuditId:    auditID.String(),
		OutputData: badOutput,
	})
	if err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatal("expected accepted=true (submission accepted; the VERDICT is mismatch)")
	}
	if len(h.audits.verdicts) != 1 || h.audits.verdicts[0].verdict != audit.VerdictMismatch {
		t.Fatalf("expected one MISMATCH verdict, got %+v", h.audits.verdicts)
	}
	sum := sha256.Sum256(badOutput)
	if want := hex.EncodeToString(sum[:]); h.audits.verdicts[0].checksum != want {
		t.Errorf("stored checksum = %q, want head-computed %q", h.audits.verdicts[0].checksum, want)
	}
	if !h.logs.hasWarn("result audit mismatch") {
		t.Error("MISMATCH must emit the observe-only WARN")
	}
}

func TestAuditSubmitResult_AdjudicatorError_Inconclusive(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.Verdict(""), "", context.DeadlineExceeded))
	auditID, output := seedVerdictAudit(h)

	if _, err := h.svc.SubmitResult(h.authedCtx(), &lettucev1.SubmitAuditResultRequest{
		AuditId:    auditID.String(),
		OutputData: output,
	}); err != nil {
		t.Fatalf("SubmitResult must not fail the RPC on a compare error: %v", err)
	}
	if len(h.audits.verdicts) != 1 {
		t.Fatalf("expected one CompleteVerdict call, got %d", len(h.audits.verdicts))
	}
	vc := h.audits.verdicts[0]
	if vc.verdict != audit.VerdictInconclusive {
		t.Errorf("verdict = %q, want INCONCLUSIVE on adjudicator error", vc.verdict)
	}
	if len(vc.detail) < len(audit.ReasonCompareError) || vc.detail[:len(audit.ReasonCompareError)] != audit.ReasonCompareError {
		t.Errorf("detail = %q, want prefix %q", vc.detail, audit.ReasonCompareError)
	}
}

func TestAuditSubmitResult_WrongClaimant_FailedPrecondition(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))
	auditID, output := seedVerdictAudit(h)
	h.audits.verdictErr = audit.ErrNotClaimant

	_, err := h.svc.SubmitResult(h.authedCtx(), &lettucev1.SubmitAuditResultRequest{
		AuditId:    auditID.String(),
		OutputData: output,
	})
	if codeOf(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %s: %v", codeOf(err), err)
	}
}

func TestAuditSubmitResult_MissingRow_NotFound(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))
	// A well-formed but unknown audit id (never seeded into rows).
	_, err := h.svc.SubmitResult(h.authedCtx(), &lettucev1.SubmitAuditResultRequest{
		AuditId:    types.NewID().String(),
		OutputData: []byte(`{}`),
	})
	if codeOf(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %s: %v", codeOf(err), err)
	}
}

func TestAuditSubmitResult_MalformedAuditID_InvalidArgument(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))
	_, err := h.svc.SubmitResult(h.authedCtx(), &lettucev1.SubmitAuditResultRequest{
		AuditId:    "not-a-uuid",
		OutputData: []byte(`{}`),
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s: %v", codeOf(err), err)
	}
}

// --- SubmitResult: execution-failure release path ---

func TestAuditSubmitResult_ExecutionFailed_Released(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))
	auditID := types.NewID()

	resp, err := h.svc.SubmitResult(h.authedCtx(), &lettucev1.SubmitAuditResultRequest{
		AuditId:         auditID.String(),
		ExecutionFailed: true,
		ErrorMessage:    "artifact fetch failed",
	})
	if err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatal("expected accepted=true")
	}
	if len(h.audits.releases) != 1 {
		t.Fatalf("expected one ReleaseFailure call, got %d", len(h.audits.releases))
	}
	rc := h.audits.releases[0]
	if rc.id != auditID || rc.runner != h.runnerID || rc.errMsg != "artifact fetch failed" {
		t.Errorf("release call = %+v, want id=%v runner=%v msg=%q", rc, auditID, h.runnerID, "artifact fetch failed")
	}
	// A failure release must never compute a verdict.
	if len(h.audits.verdicts) != 0 {
		t.Errorf("execution_failed must not record a verdict, got %d", len(h.audits.verdicts))
	}
}

func TestAuditSubmitResult_ExecutionFailed_WrongClaimant_FailedPrecondition(t *testing.T) {
	h := newAuditHarness(t, stubAdjudicator(audit.VerdictMatch, "", nil))
	h.audits.releaseErr = audit.ErrNotClaimant

	_, err := h.svc.SubmitResult(h.authedCtx(), &lettucev1.SubmitAuditResultRequest{
		AuditId:         types.NewID().String(),
		ExecutionFailed: true,
		ErrorMessage:    "boom",
	})
	if codeOf(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %s: %v", codeOf(err), err)
	}
}
