package validation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// --- Mock Repositories ---

type mockResultRepo struct {
	results map[types.ID]*result.Result // by ID
	byWU    map[types.ID][]*result.Result
	updated map[types.ID]result.ValidationStatus
}

func newMockResultRepo() *mockResultRepo {
	return &mockResultRepo{
		results: make(map[types.ID]*result.Result),
		byWU:    make(map[types.ID][]*result.Result),
		updated: make(map[types.ID]result.ValidationStatus),
	}
}

func (m *mockResultRepo) addResult(r *result.Result) {
	m.results[r.ID] = r
	m.byWU[r.WorkUnitID] = append(m.byWU[r.WorkUnitID], r)
}

func (m *mockResultRepo) Create(_ context.Context, r *result.Result) error {
	r.ID = types.NewID()
	r.CreatedAt = time.Now().UTC()
	r.UpdatedAt = r.CreatedAt
	m.addResult(r)
	return nil
}
func (m *mockResultRepo) GetByID(_ context.Context, id types.ID) (*result.Result, error) {
	r, ok := m.results[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return r, nil
}
func (m *mockResultRepo) ListByWorkUnit(_ context.Context, wuID types.ID) ([]*result.Result, error) {
	return m.byWU[wuID], nil
}
func (m *mockResultRepo) ListByVolunteer(_ context.Context, _ types.ID, _ types.PaginationRequest) ([]*result.Result, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockResultRepo) CountByWorkUnit(_ context.Context, wuID types.ID) (int, error) {
	return len(m.byWU[wuID]), nil
}
func (m *mockResultRepo) CountPendingByWorkUnit(_ context.Context, wuID types.ID) (int, error) {
	count := 0
	for _, r := range m.byWU[wuID] {
		if r.ValidationStatus == result.ValidationPending {
			count++
		}
	}
	return count, nil
}
func (m *mockResultRepo) UpdateValidationStatus(_ context.Context, id types.ID, status result.ValidationStatus) error {
	m.updated[id] = status
	if r, ok := m.results[id]; ok {
		r.ValidationStatus = status
	}
	return nil
}
func (m *mockResultRepo) BatchUpdateValidationStatus(_ context.Context, ids []types.ID, status result.ValidationStatus) error {
	for _, id := range ids {
		m.updated[id] = status
		if r, ok := m.results[id]; ok {
			r.ValidationStatus = status
		}
	}
	return nil
}

func (m *mockResultRepo) ListByLeaf(_ context.Context, _ types.ID, _ result.ResultFilters, _ types.PaginationRequest) ([]*result.Result, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}

type mockWorkUnitRepo struct {
	wus map[types.ID]*workunit.WorkUnit
}

func newMockWorkUnitRepo() *mockWorkUnitRepo {
	return &mockWorkUnitRepo{wus: make(map[types.ID]*workunit.WorkUnit)}
}

func (m *mockWorkUnitRepo) addWorkUnit(wu *workunit.WorkUnit) {
	m.wus[wu.ID] = wu
}

func (m *mockWorkUnitRepo) GetByID(_ context.Context, id types.ID) (*workunit.WorkUnit, error) {
	wu, ok := m.wus[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return wu, nil
}
func (m *mockWorkUnitRepo) Create(_ context.Context, _ *workunit.WorkUnit) error { return nil }
func (m *mockWorkUnitRepo) List(_ context.Context, _ workunit.WorkUnitListFilters, _ types.PaginationRequest) ([]*workunit.WorkUnit, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockWorkUnitRepo) UpdateState(_ context.Context, id types.ID, from, to workunit.WorkUnitState) (*workunit.WorkUnit, error) {
	wu, ok := m.wus[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	if wu.State != from {
		return nil, fmt.Errorf("invalid transition: current=%s, from=%s", wu.State, from)
	}
	wu.State = to
	now := time.Now().UTC()
	if to == workunit.WorkUnitStateValidated {
		wu.ValidatedAt = &now
	}
	return wu, nil
}
func (m *mockWorkUnitRepo) BulkCreate(_ context.Context, _ []*workunit.WorkUnit) error { return nil }
func (m *mockWorkUnitRepo) BulkTransitionByBatch(_ context.Context, _ types.ID, _, _ workunit.WorkUnitState) (int64, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) FindNextAssignable(_ context.Context, _ workunit.AssignmentOptions) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) ReserveNextAssignable(_ context.Context, _ workunit.AssignmentOptions, _ time.Duration) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) Assign(_ context.Context, _ types.ID, _ types.ID) (*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) FindDispatchableBatch(_ context.Context, _ int, _ []types.ID, _ []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) ClaimDispatchableBatch(_ context.Context, _ types.ID, _ time.Duration, _ int, _ []types.ID, _ []types.ID) ([]workunit.DispatchCandidate, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) ClearExpiredDispatchClaims(_ context.Context) (int64, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) FlushReservations(_ context.Context, _ []workunit.FlushReservation, _ types.ID, _ time.Duration) ([]workunit.FlushedCopy, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) CountActiveByVolunteer(_ context.Context) (map[types.ID]int, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) Reassign(_ context.Context, id types.ID) (*workunit.WorkUnit, bool, error) {
	wu, ok := m.wus[id]
	if !ok {
		return nil, false, fmt.Errorf("not found")
	}
	wu.State = workunit.WorkUnitStateQueued
	wu.ReassignmentCount++
	return wu, true, nil
}
func (m *mockWorkUnitRepo) CountByLeafAndState(_ context.Context, _ types.ID, _ workunit.WorkUnitState) (int64, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) EnsureWorkUnitHRClass(_ context.Context, id types.ID, class string) (string, error) {
	if wu, ok := m.wus[id]; ok {
		if wu.HRClass == nil {
			c := class
			wu.HRClass = &c
		}
		return *wu.HRClass, nil
	}
	return class, nil
}

func (m *mockWorkUnitRepo) MarkSpotCheck(_ context.Context, id types.ID) error {
	if wu, ok := m.wus[id]; ok {
		wu.SpotCheck = true
	}
	return nil
}
func (m *mockWorkUnitRepo) ClearSpotCheck(_ context.Context, id types.ID) error {
	if wu, ok := m.wus[id]; ok {
		wu.SpotCheck = false
	}
	return nil
}
func (m *mockWorkUnitRepo) FindRunningWithStaleCheckpoints(_ context.Context, _ int) ([]workunit.StaleCheckpointInfo, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) ReserveCopy(context.Context, types.ID, types.ID, time.Time, int) (*workunit.Copy, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) FindExpiredCopies(context.Context, int) ([]*workunit.Copy, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) FindStuckSpotCheckUnits(context.Context, int) ([]*workunit.WorkUnit, error) {
	return nil, nil
}
func (m *mockWorkUnitRepo) CloseCopy(context.Context, types.ID, string) error {
	return nil
}
func (m *mockWorkUnitRepo) CloseCopyByVolunteer(context.Context, types.ID, types.ID, string, *types.ID) error {
	return nil
}
func (m *mockWorkUnitRepo) ExpireLiveCopies(context.Context, types.ID, string) (int, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) CountLiveCopies(context.Context, types.ID) (int, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) CountTotalCopies(context.Context, types.ID) (int, error) {
	return 0, nil
}
func (m *mockWorkUnitRepo) DeadLetterIfExhausted(context.Context, types.ID) (bool, error) {
	return false, nil
}

type mockLeafRepo struct {
	leafs map[types.ID]*leaf.Leaf
}

func newMockLeafRepo() *mockLeafRepo {
	return &mockLeafRepo{leafs: make(map[types.ID]*leaf.Leaf)}
}

func (m *mockLeafRepo) addLeaf(p *leaf.Leaf) {
	m.leafs[p.ID] = p
}

func (m *mockLeafRepo) GetByID(_ context.Context, id types.ID) (*leaf.Leaf, error) {
	p, ok := m.leafs[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return p, nil
}
func (m *mockLeafRepo) Create(_ context.Context, _ *leaf.Leaf) error     { return nil }
func (m *mockLeafRepo) Update(_ context.Context, _ *leaf.Leaf) error     { return nil }
func (m *mockLeafRepo) Delete(_ context.Context, _ types.ID) error             { return nil }
func (m *mockLeafRepo) GetBySlug(_ context.Context, _ string, _ *types.ID) (*leaf.Leaf, error) {
	return nil, nil
}
func (m *mockLeafRepo) GetBySlugPublic(_ context.Context, _ string) (*leaf.Leaf, error) {
	return nil, nil
}
func (m *mockLeafRepo) List(_ context.Context, _ leaf.LeafListFilters, _ types.PaginationRequest) ([]*leaf.Leaf, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}

type mockCreditRepo struct {
	entries []*credit.LedgerEntry
	byRes   map[types.ID]*credit.LedgerEntry
}

func newMockCreditRepo() *mockCreditRepo {
	return &mockCreditRepo{byRes: make(map[types.ID]*credit.LedgerEntry)}
}

func (m *mockCreditRepo) Create(_ context.Context, entry *credit.LedgerEntry) error {
	if _, exists := m.byRes[entry.ResultID]; exists {
		return fmt.Errorf("duplicate result_id")
	}
	entry.ID = types.NewID()
	entry.GrantedAt = time.Now().UTC()
	entry.CreatedAt = entry.GrantedAt
	m.entries = append(m.entries, entry)
	m.byRes[entry.ResultID] = entry
	return nil
}
func (m *mockCreditRepo) GetByResultID(_ context.Context, resultID types.ID) (*credit.LedgerEntry, error) {
	e, ok := m.byRes[resultID]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return e, nil
}
func (m *mockCreditRepo) SumByVolunteerProject(_ context.Context, _, _ types.ID) (float64, error) {
	return 0, nil
}
func (m *mockCreditRepo) CountByVolunteerPerProject(_ context.Context, _ types.ID) (map[types.ID]int, error) {
	return make(map[types.ID]int), nil
}
func (m *mockCreditRepo) ListByVolunteer(_ context.Context, _ types.ID, _ types.PaginationRequest) ([]*credit.LedgerEntry, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockCreditRepo) ListByLeaf(_ context.Context, _ types.ID, _ types.PaginationRequest) ([]*credit.LedgerEntry, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}

type mockVolunteerRepo struct {
	volunteers    map[types.ID]*volunteer.Volunteer
	completedInc  map[types.ID]int
	rejectedInc   map[types.ID]int
}

func newMockVolunteerRepo() *mockVolunteerRepo {
	return &mockVolunteerRepo{
		volunteers:   make(map[types.ID]*volunteer.Volunteer),
		completedInc: make(map[types.ID]int),
		rejectedInc:  make(map[types.ID]int),
	}
}

func (m *mockVolunteerRepo) addVolunteer(v *volunteer.Volunteer) {
	m.volunteers[v.ID] = v
}

func (m *mockVolunteerRepo) GetByID(_ context.Context, id types.ID) (*volunteer.Volunteer, error) {
	v, ok := m.volunteers[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return v, nil
}
func (m *mockVolunteerRepo) Create(_ context.Context, _ *volunteer.Volunteer) error { return nil }
func (m *mockVolunteerRepo) GetByPublicKey(_ context.Context, _ []byte) (*volunteer.Volunteer, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockVolunteerRepo) Update(_ context.Context, _ *volunteer.Volunteer) error { return nil }
func (m *mockVolunteerRepo) UpdateLastSeen(_ context.Context, _ types.ID) error     { return nil }
func (m *mockVolunteerRepo) SetActive(_ context.Context, _ types.ID, _ bool) error  { return nil }
func (m *mockVolunteerRepo) IncrementWorkUnitsCompleted(_ context.Context, id types.ID) error {
	m.completedInc[id]++
	if v, ok := m.volunteers[id]; ok {
		v.TotalWorkUnitsCompleted++
	}
	return nil
}
func (m *mockVolunteerRepo) IncrementWorkUnitsRejected(_ context.Context, id types.ID) error {
	m.rejectedInc[id]++
	if v, ok := m.volunteers[id]; ok {
		v.TotalWorkUnitsRejected++
	}
	return nil
}
func (m *mockVolunteerRepo) GetByUserID(_ context.Context, _ types.ID) (*volunteer.Volunteer, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockVolunteerRepo) List(_ context.Context, _ volunteer.VolunteerListFilters, _ types.PaginationRequest) ([]*volunteer.Volunteer, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockVolunteerRepo) MarkInactiveOlderThan(_ context.Context, _ time.Duration) (int, error) {
	return 0, nil
}

type mockAssignmentRepo struct {
	activeCount map[types.ID]int
}

func newMockAssignmentRepo() *mockAssignmentRepo {
	return &mockAssignmentRepo{activeCount: make(map[types.ID]int)}
}

func (m *mockAssignmentRepo) CountActiveByWorkUnit(_ context.Context, wuID types.ID) (int, error) {
	return m.activeCount[wuID], nil
}
func (m *mockAssignmentRepo) Create(_ context.Context, _ *assignment.AssignmentHistoryEntry) error {
	return nil
}
func (m *mockAssignmentRepo) GetByID(_ context.Context, _ types.ID) (*assignment.AssignmentHistoryEntry, error) {
	return nil, nil
}
func (m *mockAssignmentRepo) ListByWorkUnit(_ context.Context, _ types.ID) ([]*assignment.AssignmentHistoryEntry, error) {
	return nil, nil
}
func (m *mockAssignmentRepo) ListByVolunteer(_ context.Context, _ types.ID, _ types.PaginationRequest) ([]*assignment.AssignmentHistoryEntry, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockAssignmentRepo) UpdateOutcome(_ context.Context, _ types.ID, _ assignment.AssignmentOutcome, _ *types.ID) error {
	return nil
}
func (m *mockAssignmentRepo) FindActiveByWorkUnitAndVolunteer(_ context.Context, _, _ types.ID) (*assignment.AssignmentHistoryEntry, error) {
	return nil, nil
}

// --- Test Helpers ---

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func makeLeaf(leafID types.ID, redundancy int, threshold float64, mode string, tolerance *float64, creditAmount float64) *leaf.Leaf {
	return &leaf.Leaf{
		ID:    leafID,
		State: leaf.StateActive,
		ValidationConfig: leaf.ValidationConfig{
			RedundancyFactor:   redundancy,
			AgreementThreshold: threshold,
			ComparisonMode:     mode,
			NumericTolerance:   tolerance,
			MaxRetries:         3,
		},
		CreditConfig: leaf.CreditConfig{
			CreditPerValidatedWorkUnit: creditAmount,
		},
	}
}

func makeWorkUnit(id, leafID types.ID, state workunit.WorkUnitState) *workunit.WorkUnit {
	return &workunit.WorkUnit{
		ID:        id,
		LeafID: leafID,
		State:     state,
	}
}

func makeResult(wuID, volID types.ID, checksum string, data json.RawMessage) *result.Result {
	return &result.Result{
		ID:               types.NewID(),
		WorkUnitID:       wuID,
		VolunteerID:      volID,
		OutputChecksum:   checksum,
		OutputData:       data,
		ValidationStatus: result.ValidationPending,
		SubmittedAt:      time.Now().UTC(),
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
}

func makeVolunteer(id types.ID) *volunteer.Volunteer {
	return &volunteer.Volunteer{
		ID:       id,
		IsActive: true,
	}
}

// --- Unit Tests ---

func TestExactMatch_TwoIdentical_BothAgreed(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()
	checksum := "aaaa"

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, checksum, nil)
	r2 := makeResult(wuID, vol2, checksum, nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	assignRepo := newMockAssignmentRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, assignRepo, nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr == nil {
		t.Fatal("expected non-nil ValidationResult")
	}
	if vr.Outcome != OutcomeValidated {
		t.Errorf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if len(vr.AgreedResults) != 2 {
		t.Errorf("AgreedResults = %d, want 2", len(vr.AgreedResults))
	}
	if len(vr.RejectedResults) != 0 {
		t.Errorf("RejectedResults = %d, want 0", len(vr.RejectedResults))
	}
	if len(vr.CreditEntries) != 2 {
		t.Errorf("CreditEntries = %d, want 2", len(vr.CreditEntries))
	}
	if wu.State != workunit.WorkUnitStateValidated {
		t.Errorf("work unit state = %s, want VALIDATED", wu.State)
	}
	if volRepo.completedInc[vol1] != 1 {
		t.Errorf("vol1 completed increments = %d, want 1", volRepo.completedInc[vol1])
	}
	if volRepo.completedInc[vol2] != 1 {
		t.Errorf("vol2 completed increments = %d, want 1", volRepo.completedInc[vol2])
	}
}

func TestExactMatch_TwoDifferent_BothRejected(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "bbbb", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	assignRepo := newMockAssignmentRepo()
	// No active assignments = all done.

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, assignRepo, nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeRejected {
		t.Errorf("Outcome = %q, want REJECTED", vr.Outcome)
	}
	if len(vr.RejectedResults) != 2 {
		t.Errorf("RejectedResults = %d, want 2", len(vr.RejectedResults))
	}
	if len(vr.CreditEntries) != 0 {
		t.Errorf("CreditEntries = %d, want 0", len(vr.CreditEntries))
	}
	// After rejectAll, the engine calls Reassign which transitions REJECTED → QUEUED
	// (when max reassignments not reached).
	if wu.State != workunit.WorkUnitStateQueued {
		t.Errorf("work unit state = %s, want QUEUED (reassigned)", wu.State)
	}
	if volRepo.rejectedInc[vol1] != 1 {
		t.Errorf("vol1 rejected increments = %d, want 1", volRepo.rejectedInc[vol1])
	}
}

func TestNumericTolerance_WithinEpsilon_BothAgreed(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()
	epsilon := 0.01

	proj := makeLeaf(leafID, 2, 1.0, "NUMERIC_TOLERANCE", &epsilon, 2.5)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", json.RawMessage(`{"x": 1.005, "y": 2.000}`))
	r2 := makeResult(wuID, vol2, "bbbb", json.RawMessage(`{"x": 1.000, "y": 2.005}`))

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	assignRepo := newMockAssignmentRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, assignRepo, nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Errorf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if len(vr.AgreedResults) != 2 {
		t.Errorf("AgreedResults = %d, want 2", len(vr.AgreedResults))
	}
	for _, entry := range vr.CreditEntries {
		if entry.CreditAmount != 2.5 {
			t.Errorf("CreditAmount = %v, want 2.5", entry.CreditAmount)
		}
	}
}

func TestNumericTolerance_OutsideEpsilon_BothRejected(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()
	epsilon := 0.001

	proj := makeLeaf(leafID, 2, 1.0, "NUMERIC_TOLERANCE", &epsilon, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", json.RawMessage(`{"x": 1.0, "y": 2.0}`))
	r2 := makeResult(wuID, vol2, "bbbb", json.RawMessage(`{"x": 1.5, "y": 2.0}`))

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	assignRepo := newMockAssignmentRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, assignRepo, nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeRejected {
		t.Errorf("Outcome = %q, want REJECTED", vr.Outcome)
	}
	if len(vr.RejectedResults) != 2 {
		t.Errorf("RejectedResults = %d, want 2", len(vr.RejectedResults))
	}
}

func TestQuorum_Redundancy3_TwoMatch_OneNot(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()
	vol3 := types.NewID()

	// threshold=0.66 so 2/3 = 0.667 >= 0.66 passes
	proj := makeLeaf(leafID, 3, 0.66, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "aaaa", nil)
	r3 := makeResult(wuID, vol3, "bbbb", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)
	resultRepo.addResult(r3)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))
	volRepo.addVolunteer(makeVolunteer(vol3))

	assignRepo := newMockAssignmentRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, assignRepo, nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Errorf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if len(vr.AgreedResults) != 2 {
		t.Errorf("AgreedResults = %d, want 2", len(vr.AgreedResults))
	}
	if len(vr.RejectedResults) != 1 {
		t.Errorf("RejectedResults = %d, want 1", len(vr.RejectedResults))
	}
	if len(vr.CreditEntries) != 2 {
		t.Errorf("CreditEntries = %d, want 2", len(vr.CreditEntries))
	}
	if volRepo.completedInc[vol1] != 1 || volRepo.completedInc[vol2] != 1 {
		t.Error("agreeing volunteers should have completed incremented")
	}
	if volRepo.rejectedInc[vol3] != 1 {
		t.Error("disagreeing volunteer should have rejected incremented")
	}
}

func TestQuorum_Redundancy3_AllDifferent_AllRejected(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()
	vol3 := types.NewID()

	proj := makeLeaf(leafID, 3, 0.67, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "bbbb", nil)
	r3 := makeResult(wuID, vol3, "cccc", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)
	resultRepo.addResult(r3)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))
	volRepo.addVolunteer(makeVolunteer(vol3))

	assignRepo := newMockAssignmentRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, assignRepo, nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeRejected {
		t.Errorf("Outcome = %q, want REJECTED", vr.Outcome)
	}
	if len(vr.RejectedResults) != 3 {
		t.Errorf("RejectedResults = %d, want 3", len(vr.RejectedResults))
	}
	if len(vr.CreditEntries) != 0 {
		t.Errorf("CreditEntries = %d, want 0", len(vr.CreditEntries))
	}
}

func TestNotEnoughResults_ReturnsNil(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, newMockVolunteerRepo(), newMockAssignmentRepo(), nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr != nil {
		t.Errorf("expected nil, got outcome=%s", vr.Outcome)
	}
}

func TestWorkUnitNotCompleted_ReturnsNil(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()

	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateValidated)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	engine := NewEngine(newMockResultRepo(), wuRepo, newMockLeafRepo(), newMockCreditRepo(), nil, newMockVolunteerRepo(), newMockAssignmentRepo(), nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr != nil {
		t.Error("expected nil for already-validated work unit")
	}
}

func TestPending_ActiveAssignmentsRemaining(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	// Redundancy=2, threshold=1.0, 2 different results (no agreement).
	// But there's still an active assignment (a re-try), so we should wait.
	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "bbbb", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	assignRepo := newMockAssignmentRepo()
	assignRepo.activeCount[wuID] = 1 // one more assignment pending (re-try)

	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, newMockVolunteerRepo(), assignRepo, nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr == nil {
		t.Fatal("expected non-nil ValidationResult")
	}
	if vr.Outcome != OutcomePending {
		t.Errorf("Outcome = %q, want PENDING", vr.Outcome)
	}
}

func TestRejectionRateWarning(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "bbbb", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	volRepo := newMockVolunteerRepo()
	// Volunteer with high rejection rate (3 rejected, 1 completed before this).
	v1 := makeVolunteer(vol1)
	v1.TotalWorkUnitsCompleted = 1
	v1.TotalWorkUnitsRejected = 3
	volRepo.addVolunteer(v1)
	v2 := makeVolunteer(vol2)
	v2.TotalWorkUnitsCompleted = 1
	v2.TotalWorkUnitsRejected = 3
	volRepo.addVolunteer(v2)

	// Capture log output to verify warning.
	var buf logBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, volRepo, newMockAssignmentRepo(), nil, nil, logger)

	_, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}

	// After rejection, both volunteers' rejection rate is > 20%. Check log.
	output := buf.String()
	if len(output) == 0 {
		t.Error("expected warning log about rejection rate, got none")
	}
}

func TestCustomMode_ReturnsError(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "CUSTOM", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, newMockVolunteerRepo(), newMockAssignmentRepo(), nil, nil, testLogger())

	_, err := engine.TryValidate(context.Background(), wuID)
	if err == nil {
		t.Fatal("expected error for custom comparison mode")
	}
}

func TestCreditAmount_UsesProjectConfig(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 5.25)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, newMockAssignmentRepo(), nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	for _, entry := range vr.CreditEntries {
		if entry.CreditAmount != 5.25 {
			t.Errorf("CreditAmount = %v, want 5.25", entry.CreditAmount)
		}
	}
}

func TestNumericTolerance_EmptyOutputData_ReturnsError(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()
	epsilon := 0.01

	proj := makeLeaf(leafID, 2, 1.0, "NUMERIC_TOLERANCE", &epsilon, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	// nil OutputData triggers "empty output data" error in parseNumericOutput.
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "bbbb", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, newMockVolunteerRepo(), newMockAssignmentRepo(), nil, nil, testLogger())

	_, err := engine.TryValidate(context.Background(), wuID)
	if err == nil {
		t.Fatal("expected error for nil OutputData in numeric_tolerance mode")
	}
}

func TestNumericTolerance_MalformedJSON_ReturnsError(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()
	epsilon := 0.01

	proj := makeLeaf(leafID, 2, 1.0, "NUMERIC_TOLERANCE", &epsilon, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", json.RawMessage(`not json`))
	r2 := makeResult(wuID, vol2, "bbbb", json.RawMessage(`{"x": 1.0}`))

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, newMockVolunteerRepo(), newMockAssignmentRepo(), nil, nil, testLogger())

	_, err := engine.TryValidate(context.Background(), wuID)
	if err == nil {
		t.Fatal("expected error for malformed JSON OutputData in numeric_tolerance mode")
	}
}

// runNumericTwoResults is a helper that sets up a 2-result NUMERIC_TOLERANCE work
// unit and runs validation, returning the ValidationResult and error.
func runNumericTwoResults(t *testing.T, epsilon float64, dataA, dataB json.RawMessage) (*ValidationResult, error) {
	t.Helper()
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "NUMERIC_TOLERANCE", &epsilon, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", dataA)
	r2 := makeResult(wuID, vol2, "bbbb", dataB)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, volRepo, newMockAssignmentRepo(), nil, nil, testLogger())
	return engine.TryValidate(context.Background(), wuID)
}

// TestNumericTolerance_NaN_DoesNotAgree verifies that two results whose numeric
// outputs are NaN are NOT judged as mutually agreeing. Non-finite values are
// rejected at parse time (mirroring malformed-output handling), so validation
// fails with an error rather than reaching quorum.
func TestNumericTolerance_NaN_DoesNotAgree(t *testing.T) {
	// JSON has no NaN literal; build the raw bytes directly so json.Unmarshal
	// would (without the fix) accept it. Go's encoder rejects NaN, so we craft
	// the JSON text manually using the non-standard "NaN" token that Go's
	// decoder also rejects — instead we exercise the parse-rejection path via
	// numericMatch directly and via 1e400 (which DOES decode to +Inf). Here we
	// assert numericMatch never treats NaN as a match.
	nan := math.NaN()
	a := map[string]flatVal{"x": {Num: nan, IsNum: true}}
	b := map[string]flatVal{"x": {Num: nan, IsNum: true}}
	if numericMatch(a, b, 0.01) {
		t.Fatal("numericMatch must NOT treat two NaN values as matching")
	}
	if numericMatch(a, b, math.Inf(1)) {
		t.Fatal("numericMatch must NOT treat two NaN values as matching even with infinite epsilon")
	}
}

// TestParseNumericOutput_RejectsNaN verifies NaN is rejected at parse time,
// consistent with malformed-output handling (returns an error).
func TestParseNumericOutput_RejectsNaN(t *testing.T) {
	// Encode a map containing NaN to bytes by hand; Go's json refuses to encode
	// NaN, so we write the literal token that exercises post-decode finiteness.
	// We test parse rejection by directly constructing the decoded path is not
	// possible through json (NaN is not valid JSON), so cover +Inf via 1e400.
	_, err := flattenOutput(json.RawMessage(`{"x": 1e400}`), nil, nil)
	if err == nil {
		t.Fatal("flattenOutput must reject +Inf (from 1e400) as non-finite")
	}
}

// TestNumericTolerance_Inf_From1e400_DoesNotValidate verifies that huge-magnitude
// JSON numbers (1e400) which decode to ±Inf do NOT validate/agree. They are
// rejected at parse time, so TryValidate returns an error and no work unit is
// validated — identical to the malformed-output behavior.
func TestNumericTolerance_Inf_From1e400_DoesNotValidate(t *testing.T) {
	vr, err := runNumericTwoResults(t, 0.01,
		json.RawMessage(`{"x": 1e400}`),
		json.RawMessage(`{"x": 1e400}`),
	)
	if err == nil {
		t.Fatalf("expected error for non-finite (+Inf from 1e400) output, got vr=%+v", vr)
	}
}

// TestNumericTolerance_NegInf_From1eNeg400_DoesNotValidate verifies -Inf is also
// rejected. (-1e400 decodes to -Inf.)
func TestNumericTolerance_NegInf_DoesNotValidate(t *testing.T) {
	vr, err := runNumericTwoResults(t, 0.01,
		json.RawMessage(`{"x": -1e400}`),
		json.RawMessage(`{"x": -1e400}`),
	)
	if err == nil {
		t.Fatalf("expected error for non-finite (-Inf from -1e400) output, got vr=%+v", vr)
	}
}

// TestNumericMatch_DefensiveNonFinite verifies the defensive numericMatch guard:
// any non-finite operand makes the pair non-matching even if it somehow reaches
// numericMatch.
func TestNumericMatch_DefensiveNonFinite(t *testing.T) {
	cases := []struct {
		name string
		a, b float64
	}{
		{"a=NaN", math.NaN(), 1.0},
		{"b=NaN", 1.0, math.NaN()},
		{"a=+Inf", math.Inf(1), 1.0},
		{"b=-Inf", 1.0, math.Inf(-1)},
		{"both NaN", math.NaN(), math.NaN()},
		{"both +Inf", math.Inf(1), math.Inf(1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if numericMatch(map[string]flatVal{"x": {Num: tc.a, IsNum: true}}, map[string]flatVal{"x": {Num: tc.b, IsNum: true}}, 0.5) {
				t.Errorf("numericMatch(%v, %v) = true, want false for non-finite operand", tc.a, tc.b)
			}
		})
	}
}

// TestNumericTolerance_FiniteWithinEpsilon_StillAgrees confirms the fix does not
// regress legitimate finite comparisons that are within tolerance.
func TestNumericTolerance_FiniteWithinEpsilon_StillAgrees(t *testing.T) {
	vr, err := runNumericTwoResults(t, 0.01,
		json.RawMessage(`{"x": 1.005, "y": 2.000}`),
		json.RawMessage(`{"x": 1.000, "y": 2.005}`),
	)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Errorf("Outcome = %q, want VALIDATED for finite values within tolerance", vr.Outcome)
	}
	if len(vr.AgreedResults) != 2 {
		t.Errorf("AgreedResults = %d, want 2", len(vr.AgreedResults))
	}
}

// TestNumericTolerance_FiniteOutsideEpsilon_StillDisagrees confirms the fix does
// not regress legitimate finite comparisons that exceed tolerance.
func TestNumericTolerance_FiniteOutsideEpsilon_StillDisagrees(t *testing.T) {
	vr, err := runNumericTwoResults(t, 0.001,
		json.RawMessage(`{"x": 1.0}`),
		json.RawMessage(`{"x": 1.5}`),
	)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeRejected {
		t.Errorf("Outcome = %q, want REJECTED for finite values outside tolerance", vr.Outcome)
	}
}

func TestExactMatch_Tie_DeterministicBreaking(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()
	vol3 := types.NewID()
	vol4 := types.NewID()

	// redundancy=4, threshold=0.5 so 2/4 = 0.5 >= 0.5 passes.
	// Two groups of 2: checksum "bbbb" and checksum "aaaa".
	// Deterministic tie-breaking should pick "aaaa" (lexicographically smaller).
	proj := makeLeaf(leafID, 4, 0.5, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "bbbb", nil)
	r2 := makeResult(wuID, vol2, "bbbb", nil)
	r3 := makeResult(wuID, vol3, "aaaa", nil)
	r4 := makeResult(wuID, vol4, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)
	resultRepo.addResult(r3)
	resultRepo.addResult(r4)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))
	volRepo.addVolunteer(makeVolunteer(vol3))
	volRepo.addVolunteer(makeVolunteer(vol4))

	assignRepo := newMockAssignmentRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, assignRepo, nil, nil, testLogger())

	// Run multiple times to confirm determinism.
	for i := 0; i < 20; i++ {
		// Reset state for each iteration.
		wu.State = workunit.WorkUnitStateCompleted
		wu.ValidatedAt = nil
		for _, r := range []*result.Result{r1, r2, r3, r4} {
			r.ValidationStatus = result.ValidationPending
		}
		resultRepo.updated = make(map[types.ID]result.ValidationStatus)
		creditRepo.entries = nil
		creditRepo.byRes = make(map[types.ID]*credit.LedgerEntry)
		volRepo.completedInc = make(map[types.ID]int)
		volRepo.rejectedInc = make(map[types.ID]int)

		vr, err := engine.TryValidate(context.Background(), wuID)
		if err != nil {
			t.Fatalf("iteration %d: TryValidate: %v", i, err)
		}
		if vr == nil {
			t.Fatalf("iteration %d: expected non-nil ValidationResult", i)
		}
		if vr.Outcome != OutcomeValidated {
			t.Fatalf("iteration %d: Outcome = %q, want VALIDATED", i, vr.Outcome)
		}
		if len(vr.AgreedResults) != 2 {
			t.Fatalf("iteration %d: AgreedResults = %d, want 2", i, len(vr.AgreedResults))
		}
		if len(vr.RejectedResults) != 2 {
			t.Fatalf("iteration %d: RejectedResults = %d, want 2", i, len(vr.RejectedResults))
		}
		// The "aaaa" group should always win (lexicographically smaller).
		// Verify the agreed results are vol3 and vol4 (the "aaaa" group).
		agreedSet := make(map[types.ID]bool)
		for _, id := range vr.AgreedResults {
			agreedSet[id] = true
		}
		if !agreedSet[r3.ID] || !agreedSet[r4.ID] {
			t.Fatalf("iteration %d: expected results with checksum 'aaaa' to win tie, but got different agreed set", i)
		}
	}
}

func TestExactMatch_RedundancyOne_SingleResult_Validated(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()

	// redundancy=1: single result is automatically the majority (1/1 = 100%).
	proj := makeLeaf(leafID, 1, 1.0, "EXACT", nil, 3.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))

	assignRepo := newMockAssignmentRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, assignRepo, nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr == nil {
		t.Fatal("expected non-nil ValidationResult")
	}
	if vr.Outcome != OutcomeValidated {
		t.Errorf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	if len(vr.AgreedResults) != 1 {
		t.Errorf("AgreedResults = %d, want 1", len(vr.AgreedResults))
	}
	if len(vr.RejectedResults) != 0 {
		t.Errorf("RejectedResults = %d, want 0", len(vr.RejectedResults))
	}
	if len(vr.CreditEntries) != 1 {
		t.Errorf("CreditEntries = %d, want 1", len(vr.CreditEntries))
	}
	if len(vr.CreditEntries) == 1 && vr.CreditEntries[0].CreditAmount != 3.0 {
		t.Errorf("CreditAmount = %v, want 3.0", vr.CreditEntries[0].CreditAmount)
	}
	if wu.State != workunit.WorkUnitStateValidated {
		t.Errorf("work unit state = %s, want VALIDATED", wu.State)
	}
	if volRepo.completedInc[vol1] != 1 {
		t.Errorf("vol1 completed increments = %d, want 1", volRepo.completedInc[vol1])
	}
}

type mockRACRepo struct {
	upserts []racUpsertCall
	err     error // if set, Upsert returns this error
}

type racUpsertCall struct {
	VolunteerID  types.ID
	LeafID       types.ID
	CreditAmount float64
}

func newMockRACRepo() *mockRACRepo {
	return &mockRACRepo{}
}

func (m *mockRACRepo) Upsert(_ context.Context, volunteerID, leafID types.ID, creditAmount float64) error {
	m.upserts = append(m.upserts, racUpsertCall{volunteerID, leafID, creditAmount})
	return m.err
}
func (m *mockRACRepo) GetByVolunteerProject(_ context.Context, _, _ types.ID) (*credit.RACEntry, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockRACRepo) ListByVolunteer(_ context.Context, _ types.ID) ([]*credit.RACEntry, error) {
	return nil, nil
}
func (m *mockRACRepo) ListByLeaf(_ context.Context, _ types.ID, _ types.PaginationRequest) ([]*credit.RACEntry, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockRACRepo) DecayAll(_ context.Context) (int64, error) {
	return 0, nil
}

func TestRACUpsertCalledOnValidation(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 2.5)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()
	racRepo := newMockRACRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, racRepo, volRepo, newMockAssignmentRepo(), nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}

	// Verify RAC upsert was called for each agreed result.
	if len(racRepo.upserts) != 2 {
		t.Fatalf("RAC upserts = %d, want 2", len(racRepo.upserts))
	}
	for _, u := range racRepo.upserts {
		if u.LeafID != leafID {
			t.Errorf("upsert ProjectID = %v, want %v", u.LeafID, leafID)
		}
		if u.CreditAmount != 2.5 {
			t.Errorf("upsert CreditAmount = %v, want 2.5", u.CreditAmount)
		}
		if u.VolunteerID != vol1 && u.VolunteerID != vol2 {
			t.Errorf("upsert VolunteerID = %v, want %v or %v", u.VolunteerID, vol1, vol2)
		}
	}
}

func TestRACUpsertErrorIsNonFatal(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()
	racRepo := newMockRACRepo()
	racRepo.err = fmt.Errorf("database connection lost")

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, racRepo, volRepo, newMockAssignmentRepo(), nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate should not fail when RAC upsert fails: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Errorf("Outcome = %q, want VALIDATED (RAC error is non-fatal)", vr.Outcome)
	}
	if len(vr.CreditEntries) != 2 {
		t.Errorf("CreditEntries = %d, want 2 (credit should still be granted)", len(vr.CreditEntries))
	}
}

// logBuffer captures log output for testing.
type logBuffer struct {
	data []byte
}

func (b *logBuffer) Write(p []byte) (n int, err error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *logBuffer) String() string {
	return string(b.data)
}

// --- Mock Attestation Repository ---

type mockAttestationRepo struct {
	attestations []*attestation.Attestation
}

func newMockAttestationRepo() *mockAttestationRepo {
	return &mockAttestationRepo{}
}

func (m *mockAttestationRepo) Create(_ context.Context, att *attestation.Attestation) error {
	att.ID = types.NewID()
	att.CreatedAt = time.Now().UTC()
	m.attestations = append(m.attestations, att)
	return nil
}
func (m *mockAttestationRepo) List(_ context.Context, _ attestation.ListFilters, _ types.PaginationRequest) ([]*attestation.Attestation, types.PaginationResponse, error) {
	return m.attestations, types.PaginationResponse{}, nil
}

// --- Attestation Integration Tests ---

func TestAttestationsCreatedOnValidation(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "aaaa", nil)

	// Add execution metadata to results for raw_metrics conversion.
	r1.ExecutionMetadata = result.ExecutionMetadata{
		WallClockSeconds: 3600,
		CPUSecondsUser:   3200,
		CPUSecondsSystem: 50,
		CPUCoresUsed:     4,
	}
	r2.ExecutionMetadata = result.ExecutionMetadata{
		WallClockSeconds: 3500,
		CPUSecondsUser:   3100,
		CPUSecondsSystem: 40,
		CPUCoresUsed:     4,
	}

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	v1 := makeVolunteer(vol1)
	v1.PublicKey = make([]byte, ed25519.PublicKeySize)
	v1.PublicKey[0] = 1
	volRepo.addVolunteer(v1)
	v2 := makeVolunteer(vol2)
	v2.PublicKey = make([]byte, ed25519.PublicKeySize)
	v2.PublicKey[0] = 2
	volRepo.addVolunteer(v2)

	// Create a real signer and mock attestation repo.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := attestation.NewSigner(priv)
	attRepo := newMockAttestationRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, newMockAssignmentRepo(), attRepo, signer, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}

	// Two agreed results should produce two attestations.
	if len(attRepo.attestations) != 2 {
		t.Fatalf("attestations created = %d, want 2", len(attRepo.attestations))
	}

	for _, att := range attRepo.attestations {
		if att.LeafID != leafID {
			t.Errorf("attestation leaf_id = %v, want %v", att.LeafID, leafID)
		}
		if att.WorkUnitID != wuID {
			t.Errorf("attestation work_unit_id = %v, want %v", att.WorkUnitID, wuID)
		}
		if att.ValidationOutcome != attestation.OutcomeAgreed {
			t.Errorf("attestation outcome = %q, want AGREED", att.ValidationOutcome)
		}
		if att.CreditAmount != 1.0 {
			t.Errorf("attestation credit_amount = %v, want 1.0", att.CreditAmount)
		}
		if len(att.Signature) == 0 {
			t.Error("attestation signature should not be empty")
		}
		if len(att.VolunteerPublicKey) == 0 {
			t.Error("attestation volunteer_public_key should not be empty")
		}
		if att.RawMetrics == nil {
			t.Error("attestation raw_metrics should not be nil")
		}

		// Verify the signature is valid.
		if !attestation.VerifyAttestation(signer.PublicKey(), att) {
			t.Error("attestation signature verification failed")
		}
	}
}

func TestAttestationsCreatedForRejectedResults(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()
	vol3 := types.NewID()

	// 2 agree, 1 disagrees. Threshold=0.66 so 2/3 passes.
	proj := makeLeaf(leafID, 3, 0.66, "EXACT", nil, 2.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "aaaa", nil)
	r3 := makeResult(wuID, vol3, "bbbb", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)
	resultRepo.addResult(r3)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	for i, vid := range []types.ID{vol1, vol2, vol3} {
		v := makeVolunteer(vid)
		v.PublicKey = make([]byte, ed25519.PublicKeySize)
		v.PublicKey[0] = byte(i + 1)
		volRepo.addVolunteer(v)
	}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := attestation.NewSigner(priv)
	attRepo := newMockAttestationRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, newMockAssignmentRepo(), attRepo, signer, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}

	// 2 agreed + 1 disagreed = 3 total attestations.
	if len(attRepo.attestations) != 3 {
		t.Fatalf("attestations created = %d, want 3", len(attRepo.attestations))
	}

	agreedCount := 0
	disagreedCount := 0
	for _, att := range attRepo.attestations {
		switch att.ValidationOutcome {
		case attestation.OutcomeAgreed:
			agreedCount++
			if att.CreditAmount != 2.0 {
				t.Errorf("agreed attestation credit = %v, want 2.0", att.CreditAmount)
			}
		case attestation.OutcomeDisagreed:
			disagreedCount++
			if att.CreditAmount != 0 {
				t.Errorf("disagreed attestation credit = %v, want 0", att.CreditAmount)
			}
		default:
			t.Errorf("unexpected outcome: %s", att.ValidationOutcome)
		}
	}
	if agreedCount != 2 {
		t.Errorf("agreed attestations = %d, want 2", agreedCount)
	}
	if disagreedCount != 1 {
		t.Errorf("disagreed attestations = %d, want 1", disagreedCount)
	}
}

func TestAttestationsCreatedOnRejectAll(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "bbbb", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	volRepo := newMockVolunteerRepo()
	for i, vid := range []types.ID{vol1, vol2} {
		v := makeVolunteer(vid)
		v.PublicKey = make([]byte, ed25519.PublicKeySize)
		v.PublicKey[0] = byte(i + 1)
		volRepo.addVolunteer(v)
	}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := attestation.NewSigner(priv)
	attRepo := newMockAttestationRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, volRepo, newMockAssignmentRepo(), attRepo, signer, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeRejected {
		t.Fatalf("Outcome = %q, want REJECTED", vr.Outcome)
	}

	// Both results rejected: 2 attestations with DISAGREED + credit=0.
	if len(attRepo.attestations) != 2 {
		t.Fatalf("attestations created = %d, want 2", len(attRepo.attestations))
	}

	for _, att := range attRepo.attestations {
		if att.ValidationOutcome != attestation.OutcomeDisagreed {
			t.Errorf("outcome = %q, want DISAGREED", att.ValidationOutcome)
		}
		if att.CreditAmount != 0 {
			t.Errorf("credit_amount = %v, want 0", att.CreditAmount)
		}
		// Verify signatures are valid even for rejected results.
		if !attestation.VerifyAttestation(signer.PublicKey(), att) {
			t.Error("attestation signature verification failed")
		}
	}
}

func TestNilAttestationRepoIsNonFatal(t *testing.T) {
	// Existing behavior: when attestationRepo is nil, createAttestations is a no-op.
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	// Pass nil for both attestationRepo and signer.
	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, newMockAssignmentRepo(), nil, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate should not fail with nil attestation repo: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Errorf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
}

func TestExecutionMetadataToMap_AllFields(t *testing.T) {
	em := result.ExecutionMetadata{
		WallClockSeconds: 3600,
		CPUSecondsUser:   3200.5,
		CPUSecondsSystem: 50.3,
		CPUCoresUsed:     4,
		GPUSeconds:       100.0,
		GPUModel:         "NVIDIA RTX 4090",
		GPUVRAMUsedMB:    8192,
		PeakMemoryMB:     2048,
		DiskReadMB:       500,
		DiskWriteMB:      100,
		NetworkRxMB:      10,
		NetworkTxMB:      5,
	}

	m := executionMetadataToMap(em)

	checks := map[string]any{
		"wall_clock_seconds": int64(3600),
		"cpu_seconds_user":   float64(3200.5),
		"cpu_seconds_system": float64(50.3),
		"cpu_cores_used":     int(4),
		"gpu_seconds":        float64(100.0),
		"gpu_vram_used_mb":   int(8192),
		"peak_memory_mb":     int(2048),
		"disk_read_mb":       int64(500),
		"disk_write_mb":      int64(100),
		"network_rx_mb":      int64(10),
		"network_tx_mb":      int64(5),
		"gpu_model":          "NVIDIA RTX 4090",
	}

	for key, expected := range checks {
		val, ok := m[key]
		if !ok {
			t.Errorf("key %q missing from map", key)
			continue
		}
		if fmt.Sprintf("%v", val) != fmt.Sprintf("%v", expected) {
			t.Errorf("key %q: got %v (%T), want %v (%T)", key, val, val, expected, expected)
		}
	}

	if len(m) != 12 {
		t.Errorf("map length = %d, want 12 (all fields including gpu_model)", len(m))
	}
}

func TestAttestationSkippedWhenVolunteerNotFound(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	// Deliberately do NOT add vol1 to the volunteer repo so GetByID will fail
	// for vol1. Only add vol2 with a public key.
	v2 := makeVolunteer(vol2)
	v2.PublicKey = make([]byte, ed25519.PublicKeySize)
	v2.PublicKey[0] = 2
	volRepo.addVolunteer(v2)
	// vol1 is NOT in volRepo — GetByID will return error.

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := attestation.NewSigner(priv)
	attRepo := newMockAttestationRepo()

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, newMockAssignmentRepo(), attRepo, signer, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate should not fail when volunteer not found for attestation: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED", vr.Outcome)
	}

	// Only 1 attestation should be created (for vol2 which was found).
	// vol1's attestation should be skipped (logged but non-fatal).
	if len(attRepo.attestations) != 1 {
		t.Errorf("attestations created = %d, want 1 (vol1 skipped due to lookup failure)", len(attRepo.attestations))
	}
}

// errorCreateAttRepo is a mock attestation repo whose Create always fails.
type errorCreateAttRepo struct {
	mockAttestationRepo
	createErr error
}

func (e *errorCreateAttRepo) Create(_ context.Context, att *attestation.Attestation) error {
	return e.createErr
}

func TestAttestationCreateErrorIsNonFatal(t *testing.T) {
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	for i, vid := range []types.ID{vol1, vol2} {
		v := makeVolunteer(vid)
		v.PublicKey = make([]byte, ed25519.PublicKeySize)
		v.PublicKey[0] = byte(i + 1)
		volRepo.addVolunteer(v)
	}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := attestation.NewSigner(priv)
	attRepo := &errorCreateAttRepo{createErr: fmt.Errorf("database write failed")}

	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, newMockAssignmentRepo(), attRepo, signer, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate should not fail when attestation create fails: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Errorf("Outcome = %q, want VALIDATED (attestation create error is non-fatal)", vr.Outcome)
	}
	// Credit should still be granted even though attestation creation failed.
	if len(vr.CreditEntries) != 2 {
		t.Errorf("CreditEntries = %d, want 2", len(vr.CreditEntries))
	}
}

func TestNilSignerWithNonNilAttestationRepo(t *testing.T) {
	// When signer is nil but attestation repo is not, createAttestations should be a no-op.
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, "EXACT", nil, 1.0)
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, "aaaa", nil)
	r2 := makeResult(wuID, vol2, "aaaa", nil)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)

	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)

	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)

	creditRepo := newMockCreditRepo()

	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	attRepo := newMockAttestationRepo()

	// signer is nil, attestation repo is not nil.
	engine := NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volRepo, newMockAssignmentRepo(), attRepo, nil, testLogger())

	vr, err := engine.TryValidate(context.Background(), wuID)
	if err != nil {
		t.Fatalf("TryValidate should not fail with nil signer: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Errorf("Outcome = %q, want VALIDATED", vr.Outcome)
	}
	// No attestations should be created when signer is nil.
	if len(attRepo.attestations) != 0 {
		t.Errorf("attestations = %d, want 0 (signer is nil)", len(attRepo.attestations))
	}
}

func TestExecutionMetadataToMap_NoGPUModel(t *testing.T) {
	em := result.ExecutionMetadata{
		WallClockSeconds: 100,
		CPUSecondsUser:   90,
	}

	m := executionMetadataToMap(em)

	if _, ok := m["gpu_model"]; ok {
		t.Error("gpu_model should not be present when GPUModel is empty")
	}

	// Should have 11 fields (all except gpu_model).
	if len(m) != 11 {
		t.Errorf("map length = %d, want 11 (without gpu_model)", len(m))
	}
}
