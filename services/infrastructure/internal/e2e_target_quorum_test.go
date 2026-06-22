//go:build integration

package internal_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestE2E_TargetQuorum_ValidateAtQuorumAndSupersede drives the headline Phase 2 feature
// (TODO #50) through the FULL gRPC + REST flow: a leaf with target_copies=3, min_quorum=2 is
// dispatched to THREE volunteers in parallel, validates as soon as TWO agree (without waiting
// for the third), and the third still-running copy is closed SUPERSEDED (not EXPIRED), so its
// host is never charged a bad reliability outcome for superseded work.
func TestE2E_TargetQuorum_ValidateAtQuorumAndSupersede(t *testing.T) {
	pool, grpcClient, httpURL, cleanup := setupF05Server(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// User + leaf.
	userID := types.NewID()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		userID,
		fmt.Sprintf("e2e-tq-%s@test.example.com", uuid.New().String()[:8]),
		fmt.Sprintf("e2e-tq-%s", uuid.New().String()[:8]),
		"E2E TQ User", "$argon2id$v=19$m=65536,t=3,p=4$x$y",
	); err != nil {
		t.Fatalf("create user: %v", err)
	}

	createReq := leaf.CreateLeafRequest{
		Name: "E2E Target>Quorum", Description: "target=3 quorum=2",
		ResearchArea: []string{"physics"}, TaskPattern: leaf.PatternParameterSweep,
		Visibility: leaf.VisibilityPublic, CreatorID: &userID,
	}
	resp := e2eRequest(t, "POST", httpURL+"/api/v1/leafs", createReq)
	e2eRequireStatus(t, resp, http.StatusCreated, "create leaf")
	var proj leaf.Leaf
	e2eDecode(t, resp, &proj)
	leafURL := httpURL + "/api/v1/leafs/" + proj.ID.String()

	resp = e2eRequest(t, "POST", leafURL+"/configure", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "configure")

	execCfg := leaf.ExecutionConfig{
		Runtime:         "NATIVE",
		Binaries:        map[string]string{"linux-amd64": "https://example.com/bin/linux-amd64"},
		BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
		GPUType:         "ANY", MaxMemoryMB: 4096, MaxDiskMB: 10240, MaxCPUSeconds: 86400,
	}
	valCfg := leaf.ValidationConfig{
		RedundancyFactor: 2, TargetCopies: 3, MinQuorum: 2,
		AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3,
	}
	ftCfg := leaf.FaultToleranceConfig{DeadlineMultiplier: 3.0, MaxReassignments: 3}
	dataCfg := leaf.DataConfig{
		TransferStrategy: "INLINE", AggregationFormat: "JSON",
		MaxInputSizeBytes: 1048576, MaxOutputSizeBytes: 104857600,
		SplittingConfig: map[string]interface{}{"x": []interface{}{float64(1)}},
	}
	resp = e2eRequest(t, "PUT", leafURL, leaf.UpdateLeafRequest{
		ExecutionConfig: &execCfg, ValidationConfig: &valCfg,
		FaultToleranceConfig: &ftCfg, DataConfig: &dataCfg,
	})
	e2eRequireStatus(t, resp, http.StatusOK, "update configs")
	resp = e2eRequest(t, "POST", leafURL+"/activate", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "activate")

	// One work unit.
	resp = e2eRequest(t, "POST", leafURL+"/work-units/generate", workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{"x": []interface{}{float64(1)}},
	})
	e2eRequireStatus(t, resp, http.StatusAccepted, "generate")
	var genResp workunit.GenerateResponse
	e2eDecode(t, resp, &genResp)
	if genResp.WorkUnitsCreated != 1 {
		t.Fatalf("work_units_created = %d, want 1", genResp.WorkUnitsCreated)
	}

	// Three volunteers each reserve + run-start a copy of the one unit (over-dispatch to
	// target=3), so three copies are live before any result lands.
	type vol struct {
		id   string
		key  e2eVolunteerKey
		pub  []byte
		wuID string
	}
	vols := make([]vol, 3)
	for i := range vols {
		k := newE2EVolunteerKey(t)
		pub := []byte(k.pub)
		reg, err := grpcClient.RegisterVolunteer(k.sign(ctx), &lettucev1.RegisterVolunteerRequest{
			PublicKey: pub, DisplayName: fmt.Sprintf("TQ Vol %d", i),
			Hardware:          &lettucev1.HardwareCapabilities{CpuCores: 8, MaxCpuCores: 4, MemoryTotalMb: 32768, MaxMemoryMb: 16384},
			AvailableRuntimes: []string{"NATIVE"}, SchedulingMode: "ALWAYS",
		})
		if err != nil {
			t.Fatalf("register vol %d: %v", i, err)
		}
		wr, err := grpcClient.RequestWorkUnit(k.sign(ctx), &lettucev1.RequestWorkUnitRequest{VolunteerId: reg.VolunteerId, PublicKey: pub})
		if err != nil {
			t.Fatalf("request vol %d: %v", i, err)
		}
		if len(wr.Assignments) != 1 {
			t.Fatalf("vol %d: expected 1 assignment (over-dispatch to target 3), got %d", i, len(wr.Assignments))
		}
		vols[i] = vol{id: reg.VolunteerId, key: k, pub: pub, wuID: wr.Assignments[0].WorkUnitId}
		if _, err := grpcClient.StartWork(k.sign(ctx), &lettucev1.StartWorkRequest{WorkUnitId: vols[i].wuID, VolunteerId: reg.VolunteerId}); err != nil {
			t.Fatalf("start vol %d: %v", i, err)
		}
	}
	wuID, _ := types.ParseID(vols[0].wuID)

	// All three got copies of the SAME unit, and three copies are live.
	for i := 1; i < 3; i++ {
		if vols[i].wuID != vols[0].wuID {
			t.Fatalf("vols got different units (%s vs %s); expected the single unit", vols[i].wuID, vols[0].wuID)
		}
	}
	if live := countLive(t, ctx, pool, wuID); live != 3 {
		t.Fatalf("expected 3 live copies (over-dispatch to target), got %d", live)
	}

	// Identical output so all agree under EXACT.
	output := []byte(`{"result":"ok","value":42}`)
	sum := sha256.Sum256(output)
	checksum := hex.EncodeToString(sum[:])
	submit := func(v vol) *lettucev1.SubmitResultResponse {
		r, err := grpcClient.SubmitResult(v.key.sign(ctx), &lettucev1.SubmitResultRequest{
			WorkUnitId: v.wuID, VolunteerId: v.id, PublicKey: v.pub,
			OutputData: output, OutputChecksumSha256: checksum,
			Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 10, CpuCoresUsed: 4},
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		return r
	}

	// First submit: quorum (2) not yet met -> unit stays uncompleted.
	submit(vols[0])
	if s := wuState(t, ctx, pool, wuID); s == "VALIDATED" {
		t.Fatalf("validated after a single result; min_quorum=2")
	}

	// Second submit: quorum reached and the two agree -> VALIDATE AT QUORUM, without waiting
	// for the third copy.
	submit(vols[1])
	if s := wuState(t, ctx, pool, wuID); s != "VALIDATED" {
		t.Fatalf("state after 2 agreeing results = %q, want VALIDATED (validate-at-quorum)", s)
	}

	// The third (still-running) copy is closed SUPERSEDED, not EXPIRED/ABANDONED — so its host
	// is not charged a bad reliability outcome.
	var supersededCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id=$1 AND outcome='SUPERSEDED'`,
		wuID).Scan(&supersededCount); err != nil {
		t.Fatalf("query superseded: %v", err)
	}
	if supersededCount != 1 {
		t.Fatalf("expected exactly 1 SUPERSEDED copy (the over-dispatch extra), got %d", supersededCount)
	}
	if live := countLive(t, ctx, pool, wuID); live != 0 {
		t.Fatalf("expected 0 live copies after validate+supersede, got %d", live)
	}

	// Exactly two results were credited (the quorum), and the unit has a credit ledger entry.
	var agreed int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM results WHERE work_unit_id=$1 AND validation_status='AGREED'`, wuID).Scan(&agreed); err != nil {
		t.Fatalf("query agreed: %v", err)
	}
	if agreed != 2 {
		t.Fatalf("expected 2 AGREED results (quorum), got %d", agreed)
	}
}

func countLive(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wuID types.ID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id=$1 AND outcome IS NULL`, wuID).Scan(&n); err != nil {
		t.Fatalf("count live: %v", err)
	}
	return n
}

func wuState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wuID types.ID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT state FROM work_units WHERE id=$1`, wuID).Scan(&s); err != nil {
		t.Fatalf("query state: %v", err)
	}
	return s
}
