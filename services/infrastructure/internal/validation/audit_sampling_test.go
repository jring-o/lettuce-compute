package validation

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/audit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// leaf.PgxRepository is the production implementation of the engine's artifact-version resolver.
var _ artifactVersionResolver = (*leaf.PgxRepository)(nil)

// fakeEnqueuer records the audit jobs the sampling hook enqueues (or returns a forced error).
type fakeEnqueuer struct {
	enqueued []*audit.Audit
	err      error
}

func (f *fakeEnqueuer) Enqueue(_ context.Context, a *audit.Audit) error {
	if f.err != nil {
		return f.err
	}
	f.enqueued = append(f.enqueued, a)
	return nil
}

// fakeVersionResolver resolves pinned artifact versions for the F-M4 execution-snapshot path.
type fakeVersionResolver struct {
	versions map[types.ID]*leaf.ArtifactVersion
	err      error
}

func (f *fakeVersionResolver) GetVersionByID(_ context.Context, id types.ID) (*leaf.ArtifactVersion, error) {
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.versions[id]
	if !ok {
		return nil, fmt.Errorf("artifact version %s not found", id)
	}
	return v, nil
}

// auditEngine builds an engine wired only for the sampling hook — the hook touches no repository.
func auditEngine(enq audit.Enqueuer, enabled bool, headRate float64, versions artifactVersionResolver) *Engine {
	e := NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, testLogger(), nil, transition.TrustPolicy{})
	return e.WithResultAudits(enq, enabled, headRate, versions)
}

func auditLeaf(mode string) *leaf.Leaf {
	return &leaf.Leaf{
		ID: types.NewID(),
		ValidationConfig: leaf.ValidationConfig{
			ComparisonMode: mode, RedundancyFactor: 2, AgreementThreshold: 1.0,
		},
		ExecutionConfig: leaf.ExecutionConfig{Runtime: "NATIVE"},
	}
}

func auditUnit(leafID types.ID, hrClass string) *workunit.WorkUnit {
	wu := &workunit.WorkUnit{ID: types.NewID(), LeafID: leafID, State: workunit.WorkUnitStateValidated}
	if hrClass != "" {
		c := hrClass
		wu.HRClass = &c
	}
	return wu
}

func auditResult(wuID types.ID, checksum string, data json.RawMessage) *result.Result {
	return makeResult(wuID, types.NewID(), checksum, data)
}

// TestSampleAudit_ExactUnpinnedEnqueues: an unpinned non-HR EXACT unit is eligible; the enqueued
// job carries the raw comparison key (no ignore_fields), a nil RequiredHRClass, the representative
// result id, and the leaf's current execution snapshot.
func TestSampleAudit_ExactUnpinnedEnqueues(t *testing.T) {
	enq := &fakeEnqueuer{}
	e := auditEngine(enq, true, 1.0, nil)
	proj := auditLeaf(leaf.ComparisonExact)
	wu := auditUnit(proj.ID, "")
	r := auditResult(wu.ID, "raw-checksum", json.RawMessage(`{"x":1}`))

	e.maybeSampleForAudit(context.Background(), wu, proj, []*result.Result{r})

	if len(enq.enqueued) != 1 {
		t.Fatalf("enqueued %d, want 1", len(enq.enqueued))
	}
	a := enq.enqueued[0]
	if a.AcceptedComparisonKey == nil || *a.AcceptedComparisonKey != "raw-checksum" {
		t.Errorf("accepted key = %v, want the raw checksum", a.AcceptedComparisonKey)
	}
	if a.RequiredHRClass != nil {
		t.Errorf("RequiredHRClass = %v, want nil (unpinned unit)", *a.RequiredHRClass)
	}
	if a.AcceptedResultID != r.ID {
		t.Errorf("AcceptedResultID = %s, want %s", a.AcceptedResultID, r.ID)
	}
	if a.WorkUnitID != wu.ID || a.LeafID != wu.LeafID {
		t.Errorf("unit/leaf ids = %s/%s, want %s/%s", a.WorkUnitID, a.LeafID, wu.ID, wu.LeafID)
	}
	if a.ComparisonSnapshot.ComparisonMode != leaf.ComparisonExact {
		t.Errorf("snapshot mode = %q, want EXACT", a.ComparisonSnapshot.ComparisonMode)
	}
	if a.ExecutionSnapshot.Runtime != "NATIVE" {
		t.Errorf("execution snapshot runtime = %q, want NATIVE (leaf current)", a.ExecutionSnapshot.Runtime)
	}
}

// TestSampleAudit_HRExactPinnedEnqueues: an HR EXACT leaf whose unit carries a pin IS eligible and
// records RequiredHRClass = the unit pin for every mode (F-H2 positive).
func TestSampleAudit_HRExactPinnedEnqueues(t *testing.T) {
	enq := &fakeEnqueuer{}
	e := auditEngine(enq, true, 1.0, nil)
	proj := auditLeaf(leaf.ComparisonExact)
	proj.ValidationConfig.HomogeneousRedundancy = true
	wu := auditUnit(proj.ID, "AppleSilicon/darwin/arm64")
	r := auditResult(wu.ID, "chk", json.RawMessage(`{"x":1}`))

	e.maybeSampleForAudit(context.Background(), wu, proj, []*result.Result{r})

	if len(enq.enqueued) != 1 {
		t.Fatalf("enqueued %d, want 1 (HR EXACT with a pin is eligible)", len(enq.enqueued))
	}
	if a := enq.enqueued[0]; a.RequiredHRClass == nil || *a.RequiredHRClass != "AppleSilicon/darwin/arm64" {
		t.Errorf("RequiredHRClass = %v, want the unit pin", a.RequiredHRClass)
	}
}

// TestSampleAudit_NumericPinnedEnqueues: a pinned NUMERIC unit with inline output is eligible; the
// job carries a NIL accepted key (value-level) and the snapshot numeric tolerance.
func TestSampleAudit_NumericPinnedEnqueues(t *testing.T) {
	enq := &fakeEnqueuer{}
	e := auditEngine(enq, true, 1.0, nil)
	proj := auditLeaf(leaf.ComparisonNumericTolerance)
	tol := 0.25
	proj.ValidationConfig.NumericTolerance = &tol
	wu := auditUnit(proj.ID, "GenuineIntel/linux/amd64")
	r := auditResult(wu.ID, "chk", json.RawMessage(`{"v":1.0}`))

	e.maybeSampleForAudit(context.Background(), wu, proj, []*result.Result{r})

	if len(enq.enqueued) != 1 {
		t.Fatalf("enqueued %d, want 1", len(enq.enqueued))
	}
	a := enq.enqueued[0]
	if a.AcceptedComparisonKey != nil {
		t.Errorf("accepted key = %q, want nil (NUMERIC is value-level)", *a.AcceptedComparisonKey)
	}
	if a.ComparisonSnapshot.NumericTolerance != 0.25 {
		t.Errorf("snapshot tolerance = %v, want 0.25", a.ComparisonSnapshot.NumericTolerance)
	}
	if a.RequiredHRClass == nil || *a.RequiredHRClass != "GenuineIntel/linux/amd64" {
		t.Errorf("RequiredHRClass = %v, want the unit pin", a.RequiredHRClass)
	}
}

// TestSampleAudit_IneligibleLanes: every owner-selectable skip lane (§7.2) drops the unit and bumps
// the per-leaf ineligible counter the fault-monitor probe reads — none enqueue.
func TestSampleAudit_IneligibleLanes(t *testing.T) {
	tests := []struct {
		name  string
		build func() (*leaf.Leaf, *workunit.WorkUnit, *result.Result)
	}{
		{"CUSTOM mode", func() (*leaf.Leaf, *workunit.WorkUnit, *result.Result) {
			proj := auditLeaf(leaf.ComparisonCustom)
			wu := auditUnit(proj.ID, "")
			return proj, wu, auditResult(wu.ID, "chk", json.RawMessage(`{"x":1}`))
		}},
		{"NetworkAccess leaf", func() (*leaf.Leaf, *workunit.WorkUnit, *result.Result) {
			proj := auditLeaf(leaf.ComparisonExact)
			proj.ExecutionConfig.NetworkAccess = true
			wu := auditUnit(proj.ID, "")
			return proj, wu, auditResult(wu.ID, "chk", json.RawMessage(`{"x":1}`))
		}},
		{"HR EXACT without a unit pin", func() (*leaf.Leaf, *workunit.WorkUnit, *result.Result) {
			proj := auditLeaf(leaf.ComparisonExact)
			proj.ValidationConfig.HomogeneousRedundancy = true
			wu := auditUnit(proj.ID, "") // no pin
			return proj, wu, auditResult(wu.ID, "chk", json.RawMessage(`{"x":1}`))
		}},
		{"NUMERIC unpinned", func() (*leaf.Leaf, *workunit.WorkUnit, *result.Result) {
			proj := auditLeaf(leaf.ComparisonNumericTolerance)
			wu := auditUnit(proj.ID, "") // no pin
			return proj, wu, auditResult(wu.ID, "chk", json.RawMessage(`{"v":1.0}`))
		}},
		{"NUMERIC ref-only (no inline output)", func() (*leaf.Leaf, *workunit.WorkUnit, *result.Result) {
			proj := auditLeaf(leaf.ComparisonNumericTolerance)
			wu := auditUnit(proj.ID, "GenuineIntel/linux/amd64") // pinned, but...
			return proj, wu, auditResult(wu.ID, "chk", nil)       // ...no inline bytes
		}},
		{"EXACT canon-empty winner", func() (*leaf.Leaf, *workunit.WorkUnit, *result.Result) {
			proj := auditLeaf(leaf.ComparisonExact)
			proj.ValidationConfig.IgnoreFields = []string{"a"} // strips the only field
			wu := auditUnit(proj.ID, "")
			return proj, wu, auditResult(wu.ID, "chk", json.RawMessage(`{"a":1}`))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enq := &fakeEnqueuer{}
			e := auditEngine(enq, true, 1.0, nil)
			proj, wu, r := tt.build()
			e.maybeSampleForAudit(context.Background(), wu, proj, []*result.Result{r})
			if len(enq.enqueued) != 0 {
				t.Fatalf("enqueued %d, want 0 (ineligible lane)", len(enq.enqueued))
			}
			if got := e.AuditIneligibleCounts()[wu.LeafID.String()]; got != 1 {
				t.Errorf("ineligible count = %d, want 1", got)
			}
		})
	}
}

// TestSampleAudit_DisabledOrNilEnqueuerNoop: with the hook disabled (or no enqueuer) nothing is
// enqueued AND nothing is counted ineligible — the default state is fully inert.
func TestSampleAudit_DisabledOrNilEnqueuerNoop(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		enq     audit.Enqueuer
	}{
		{"disabled", false, &fakeEnqueuer{}},
		{"nil enqueuer", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := auditEngine(tc.enq, tc.enabled, 1.0, nil)
			proj := auditLeaf(leaf.ComparisonExact)
			wu := auditUnit(proj.ID, "")
			r := auditResult(wu.ID, "chk", json.RawMessage(`{"x":1}`))
			e.maybeSampleForAudit(context.Background(), wu, proj, []*result.Result{r})
			if fe, ok := tc.enq.(*fakeEnqueuer); ok && len(fe.enqueued) != 0 {
				t.Errorf("enqueued %d, want 0 (inert)", len(fe.enqueued))
			}
			if len(e.AuditIneligibleCounts()) != 0 {
				t.Errorf("ineligible counts non-empty, want none (inert)")
			}
		})
	}
}

// TestSampleAudit_RateResolutionMaxOverlay: the effective rate is max(leaf, head) (F-H4) — a leaf
// rate can only RAISE sampling, never lower it below the head floor. Uses the deterministic rate
// bounds (0 -> never, 1 -> always) so the assertion needs no probabilistic tolerance.
func TestSampleAudit_RateResolutionMaxOverlay(t *testing.T) {
	tests := []struct {
		name     string
		headRate float64
		leafRate float64
		want     int
	}{
		{"head 1.0, leaf 0 -> sampled", 1.0, 0, 1},
		{"head 0, leaf 0 -> not sampled", 0, 0, 0},
		{"head 0, leaf 1.0 -> leaf raises to sampled", 0, 1.0, 1},
		{"head 1.0, leaf 0.0000001 below head -> head wins, sampled", 1.0, 0.0000001, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enq := &fakeEnqueuer{}
			e := auditEngine(enq, true, tt.headRate, nil)
			proj := auditLeaf(leaf.ComparisonExact)
			proj.ValidationConfig.AuditRate = tt.leafRate
			wu := auditUnit(proj.ID, "")
			r := auditResult(wu.ID, "chk", json.RawMessage(`{"x":1}`))
			e.maybeSampleForAudit(context.Background(), wu, proj, []*result.Result{r})
			if len(enq.enqueued) != tt.want {
				t.Errorf("enqueued %d, want %d (effective rate = max(head=%v, leaf=%v))",
					len(enq.enqueued), tt.want, tt.headRate, tt.leafRate)
			}
		})
	}
}

// TestSampleAudit_VersionedWinnerUsesSnapshotExec: the winner ran a pinned artifact version, so the
// job records THAT version's frozen ExecutionConfig — and the eligibility check reads it too. Here
// the leaf's CURRENT config sets NetworkAccess (which would skip), but the snapshot does not: the
// unit IS eligible and the recorded snapshot is the version's (F-M4).
func TestSampleAudit_VersionedWinnerUsesSnapshotExec(t *testing.T) {
	versionID := types.NewID()
	ver := &leaf.ArtifactVersion{
		ID:              versionID,
		ExecutionConfig: leaf.ExecutionConfig{Runtime: "CONTAINER", NetworkAccess: false, MaxMemoryMB: 2048},
	}
	res := &fakeVersionResolver{versions: map[types.ID]*leaf.ArtifactVersion{versionID: ver}}
	enq := &fakeEnqueuer{}
	e := auditEngine(enq, true, 1.0, res)
	proj := auditLeaf(leaf.ComparisonExact)
	proj.ExecutionConfig = leaf.ExecutionConfig{Runtime: "NATIVE", NetworkAccess: true} // leaf CURRENT would skip
	wu := auditUnit(proj.ID, "")
	r := auditResult(wu.ID, "chk", json.RawMessage(`{"x":1}`))
	r.ArtifactVersionID = &versionID

	e.maybeSampleForAudit(context.Background(), wu, proj, []*result.Result{r})

	if len(enq.enqueued) != 1 {
		t.Fatalf("enqueued %d, want 1 (snapshot has no network access; leaf-current network must not apply)", len(enq.enqueued))
	}
	a := enq.enqueued[0]
	if a.ExecutionSnapshot.Runtime != "CONTAINER" || a.ExecutionSnapshot.NetworkAccess {
		t.Errorf("ExecutionSnapshot = %+v, want the version's frozen config (CONTAINER, no network)", a.ExecutionSnapshot)
	}
	if a.ArtifactVersionID == nil || *a.ArtifactVersionID != versionID {
		t.Errorf("ArtifactVersionID = %v, want %s", a.ArtifactVersionID, versionID)
	}
}

// TestSampleAudit_VersionResolveErrorSkips: a versioned winner whose artifact version cannot be
// resolved (GC race) is skipped best-effort, never enqueued (INCONCLUSIVE-by-construction avoided).
func TestSampleAudit_VersionResolveErrorSkips(t *testing.T) {
	res := &fakeVersionResolver{err: fmt.Errorf("version gone")}
	enq := &fakeEnqueuer{}
	e := auditEngine(enq, true, 1.0, res)
	proj := auditLeaf(leaf.ComparisonExact)
	wu := auditUnit(proj.ID, "")
	versionID := types.NewID()
	r := auditResult(wu.ID, "chk", json.RawMessage(`{"x":1}`))
	r.ArtifactVersionID = &versionID

	e.maybeSampleForAudit(context.Background(), wu, proj, []*result.Result{r})

	if len(enq.enqueued) != 0 {
		t.Fatalf("enqueued %d, want 0 (version resolve error must skip)", len(enq.enqueued))
	}
}

// TestSampleAudit_EnqueueErrorNeverFails: a unique-violation or any enqueue error is best-effort —
// WARNed and dropped, never surfaced (the hook returns no value; the caller cannot fail validation).
func TestSampleAudit_EnqueueErrorNeverFails(t *testing.T) {
	enq := &fakeEnqueuer{err: fmt.Errorf("duplicate open audit")}
	e := auditEngine(enq, true, 1.0, nil)
	proj := auditLeaf(leaf.ComparisonExact)
	wu := auditUnit(proj.ID, "")
	r := auditResult(wu.ID, "chk", json.RawMessage(`{"x":1}`))
	// Must not panic and must record no enqueue.
	e.maybeSampleForAudit(context.Background(), wu, proj, []*result.Result{r})
	if len(enq.enqueued) != 0 {
		t.Errorf("enqueued %d, want 0 (the enqueue errored)", len(enq.enqueued))
	}
}

// TestSampleAudit_WinnerIsSmallestUUID: the representative winner is the AGREED member with the
// lexicographically smallest result UUID.
func TestSampleAudit_WinnerIsSmallestUUID(t *testing.T) {
	enq := &fakeEnqueuer{}
	e := auditEngine(enq, true, 1.0, nil)
	proj := auditLeaf(leaf.ComparisonExact)
	wu := auditUnit(proj.ID, "")
	r1 := auditResult(wu.ID, "chk", json.RawMessage(`{"x":1}`))
	r2 := auditResult(wu.ID, "chk", json.RawMessage(`{"x":1}`))
	want := r1.ID
	if r2.ID.String() < want.String() {
		want = r2.ID
	}

	e.maybeSampleForAudit(context.Background(), wu, proj, []*result.Result{r1, r2})

	if len(enq.enqueued) != 1 {
		t.Fatalf("enqueued %d, want 1", len(enq.enqueued))
	}
	if enq.enqueued[0].AcceptedResultID != want {
		t.Errorf("AcceptedResultID = %s, want the smallest-UUID member %s", enq.enqueued[0].AcceptedResultID, want)
	}
}
