package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"

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

// notActiveRunnerMessage is the CONSTANT-shape PermissionDenied message the audit
// surface returns when the authenticated volunteer is not an ACTIVE registered
// runner. It is deliberately identical for "no registry row" and "inactive row" so
// the response never distinguishes the two: registry membership IS the authorization
// (§7.3), and a differentiated message would leak whether a key was ever vouched for.
const notActiveRunnerMessage = "not an active trusted runner"

// maxAuditClaimAttempts bounds the ClaimJob loop that skips past audits whose
// sampled unit/leaf was cascade-deleted out from under the claim. Each miss is
// finalized INCONCLUSIVE and the next job is tried; the bound stops a pathological
// delete storm from spinning the handler. Generous — the backlog is ~1% of units.
const maxAuditClaimAttempts = 10

// The audit service consumes only one or two methods of the volunteer, work-unit,
// leaf, and result repositories, so it depends on narrow consumer-side interfaces
// (the concrete pgx repos satisfy them) rather than the full repository surfaces.

// auditVolunteerLookup resolves the verified Ed25519 public key to a volunteer
// account (the only method of volunteer.Repository the audit surface needs).
type auditVolunteerLookup interface {
	GetByPublicKey(ctx context.Context, publicKey []byte) (*volunteer.Volunteer, error)
}

// auditWorkUnitLookup loads the sampled unit a claimed audit re-executes.
type auditWorkUnitLookup interface {
	GetByID(ctx context.Context, id types.ID) (*workunit.WorkUnit, error)
}

// auditLeafLookup loads the leaf backing a claimed audit's unit.
type auditLeafLookup interface {
	GetByID(ctx context.Context, id types.ID) (*leaf.Leaf, error)
}

// auditResultLister reads the AGREED outputs a runner's re-execution is
// adjudicated against.
type auditResultLister interface {
	ListByWorkUnit(ctx context.Context, workUnitID types.ID) ([]*result.Result, error)
}

// auditServiceServer implements lettucev1.AuditServiceServer: the operator-vetted
// trusted-runner surface for post-hoc result audits. Both RPCs authenticate off the
// verified per-request Ed25519 signature (never a request field) and require an
// ACTIVE trusted_runners row; every verdict is computed head-side (a runner never
// self-adjudicates). This is the observe-only phase — a MISMATCH is recorded and
// WARNed, nothing more (design §7.3, §7.4).
type auditServiceServer struct {
	lettucev1.UnimplementedAuditServiceServer
	runnersRepo   audit.RunnersRepository
	auditsRepo    audit.AuditsRepository
	wuRepo        auditWorkUnitLookup
	leafRepo      auditLeafLookup
	volunteerRepo auditVolunteerLookup
	adjudicator   audit.Adjudicator
	resultRepo    auditResultLister
	logger        *slog.Logger
}

// NewAuditService creates the AuditService gRPC implementation. The adjudicator is
// the head-side verdict function (validation.AdjudicateAudit, wired in main.go); the
// repositories are the trusted-runner registry, the audit job store, and the narrow
// read views the claim/submit handlers need.
func NewAuditService(
	runnersRepo audit.RunnersRepository,
	auditsRepo audit.AuditsRepository,
	wuRepo auditWorkUnitLookup,
	leafRepo auditLeafLookup,
	volunteerRepo auditVolunteerLookup,
	adjudicator audit.Adjudicator,
	resultRepo auditResultLister,
	logger *slog.Logger,
) lettucev1.AuditServiceServer {
	return &auditServiceServer{
		runnersRepo:   runnersRepo,
		auditsRepo:    auditsRepo,
		wuRepo:        wuRepo,
		leafRepo:      leafRepo,
		volunteerRepo: volunteerRepo,
		adjudicator:   adjudicator,
		resultRepo:    resultRepo,
		logger:        logger,
	}
}

// authenticateRunner runs the fail-closed auth chain both RPCs share, each step
// failing closed: (1) the Ed25519 interceptor must have bound a verified public key
// into ctx (missing → Unauthenticated); (2) that key must resolve to a known
// volunteer (unknown → Unauthenticated); (3) that volunteer must have an ACTIVE
// trusted_runners row (absent/inactive → PermissionDenied, constant message).
// Identity comes ONLY from the verified signature — the proto requests deliberately
// carry no volunteer_id/public_key fields.
func (s *auditServiceServer) authenticateRunner(ctx context.Context, method string) (*audit.Runner, error) {
	pubKey, ok := GRPCAuthPublicKeyFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "request is not authenticated")
	}

	vol, err := s.volunteerRepo.GetByPublicKey(ctx, pubKey)
	if err != nil {
		if isNotFound(err) {
			return nil, status.Error(codes.Unauthenticated, "authenticated volunteer not found")
		}
		s.logger.Error("failed to look up authenticated volunteer", "method", method, "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	runner, err := s.runnersRepo.GetActiveByVolunteerID(ctx, vol.ID)
	if err != nil {
		if errors.Is(err, audit.ErrNotRegistered) {
			return nil, status.Error(codes.PermissionDenied, notActiveRunnerMessage)
		}
		s.logger.Error("failed to resolve trusted runner", "method", method, "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	return runner, nil
}

// ClaimJob claims the next queued audit job the runner's hardware class is eligible
// for and returns the re-execution assignment (rebuilt from the sampling-time
// execution snapshot). An empty response (no job) means nothing is claimable.
func (s *auditServiceServer) ClaimJob(ctx context.Context, req *lettucev1.ClaimAuditJobRequest) (*lettucev1.ClaimAuditJobResponse, error) {
	runner, err := s.authenticateRunner(ctx, "ClaimJob")
	if err != nil {
		return nil, err
	}

	// Derive the runner's HR class server-side from the reported hardware, the same
	// path RequestWorkUnit uses (HardwareCapabilitiesFromProto → HRClass). A nil
	// Hardware yields the empty class, which matches only NULL-requirement jobs (a
	// pinned job would never be handed to a runner whose class is unknown).
	hrClass := ""
	if req.GetHardware() != nil {
		hrClass = volunteer.HardwareCapabilitiesFromProto(req.GetHardware()).HRClass()
	}

	for i := 0; i < maxAuditClaimAttempts; i++ {
		auditRow, err := s.auditsRepo.Claim(ctx, runner.ID, hrClass)
		if err != nil {
			s.logger.Error("failed to claim audit job", "method", "ClaimJob", "runner_id", runner.ID, "error", err)
			return nil, status.Error(codes.Internal, "internal error")
		}
		if auditRow == nil {
			// Nothing claimable for this runner's class right now.
			return &lettucev1.ClaimAuditJobResponse{}, nil
		}

		wu, lf, gone, err := s.loadClaimedArtifacts(ctx, auditRow)
		if err != nil {
			// A genuine load failure (not a missing row): leave the row CLAIMED so the
			// reclaim sweep requeues it when the lease lapses, and surface the error.
			s.logger.Error("failed to load audit artifacts", "method", "ClaimJob", "audit_id", auditRow.ID, "error", err)
			return nil, status.Error(codes.Internal, "internal error")
		}
		if gone {
			// Unit or leaf cascade-deleted out from under the claim: finalize the row
			// INCONCLUSIVE and try the next job rather than hand out an unrunnable one.
			detail := audit.ReasonArtifactUnavailable + ": unit or leaf deleted"
			if cErr := s.auditsRepo.CompleteInconclusive(ctx, auditRow.ID, runner.ID, detail); cErr != nil {
				s.logger.Warn("failed to finalize dangling audit inconclusive",
					"method", "ClaimJob", "audit_id", auditRow.ID, "error", cErr)
			}
			continue
		}

		// Build the assignment from the SAMPLING-TIME execution snapshot (never a
		// claim-time resolution of owner-mutable leaf config — F-M4), then apply the
		// two audit overrides: the audit lease governs the reservation, and the
		// checkpoint fields are cleared because audits always re-execute from scratch
		// (F-L3: the original unit's checkpoints were deleted on VALIDATE).
		var leaseUnix int64
		if auditRow.LeaseExpiresAt != nil {
			leaseUnix = auditRow.LeaseExpiresAt.Unix()
		}
		wuAssignment := buildWorkUnitAssignment(wu, lf, &auditRow.ExecutionSnapshot)
		wuAssignment.ReservedUntilUnix = leaseUnix
		wuAssignment.HasCheckpoint = false
		wuAssignment.CheckpointSequence = 0
		wuAssignment.CheckpointIntervalSeconds = 0

		return &lettucev1.ClaimAuditJobResponse{
			Job: &lettucev1.AuditJob{
				AuditId:          auditRow.ID.String(),
				Assignment:       wuAssignment,
				LeaseExpiresUnix: leaseUnix,
			},
		}, nil
	}

	// Exhausted the dangling-row budget in one call: report no work (the reclaim sweep
	// EXPIREs rows we could not resolve here). Empty is the safe, retryable answer.
	s.logger.Warn("audit claim loop exhausted resolving dangling rows",
		"method", "ClaimJob", "runner_id", runner.ID)
	return &lettucev1.ClaimAuditJobResponse{}, nil
}

// loadClaimedArtifacts loads the work unit and leaf a claimed audit re-executes.
// gone is true when either has been deleted (a missing row — a cascaded delete
// racing the claim); err is non-nil only for a genuine load failure.
func (s *auditServiceServer) loadClaimedArtifacts(ctx context.Context, a *audit.Audit) (wu *workunit.WorkUnit, lf *leaf.Leaf, gone bool, err error) {
	wu, err = s.wuRepo.GetByID(ctx, a.WorkUnitID)
	if err != nil {
		if isNotFound(err) {
			return nil, nil, true, nil
		}
		return nil, nil, false, err
	}
	if wu == nil {
		return nil, nil, true, nil
	}
	lf, err = s.leafRepo.GetByID(ctx, a.LeafID)
	if err != nil {
		if isNotFound(err) {
			return nil, nil, true, nil
		}
		return nil, nil, false, err
	}
	if lf == nil {
		return nil, nil, true, nil
	}
	return wu, lf, false, nil
}

// SubmitResult finalizes a claimed audit: either an execution failure (released for
// another attempt) or the re-executed output bytes, which the head hashes and
// adjudicates server-side under the pinned snapshot semantics.
func (s *auditServiceServer) SubmitResult(ctx context.Context, req *lettucev1.SubmitAuditResultRequest) (*lettucev1.SubmitAuditResultResponse, error) {
	runner, err := s.authenticateRunner(ctx, "SubmitResult")
	if err != nil {
		return nil, err
	}

	auditID, err := types.ParseID(req.GetAuditId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid audit_id: %v", err)
	}

	// Execution-failure path: the runner never produced output. Release the job for
	// another attempt (or EXPIRE it at the attempt budget); no verdict is computed.
	if req.GetExecutionFailed() {
		if err := s.auditsRepo.ReleaseFailure(ctx, auditID, runner.ID, req.GetErrorMessage()); err != nil {
			if errors.Is(err, audit.ErrNotClaimant) {
				return nil, status.Error(codes.FailedPrecondition, audit.ErrNotClaimant.Error())
			}
			s.logger.Error("failed to release audit job after execution failure", "method", "SubmitResult", "audit_id", auditID, "error", err)
			return nil, status.Error(codes.Internal, "internal error")
		}
		return &lettucev1.SubmitAuditResultResponse{Accepted: true}, nil
	}

	// Empty output without an execution-failure flag is ambiguous — adjudicating it
	// would all but fabricate a MISMATCH (sha256 of zero bytes vs the accepted key),
	// which the observe-only contract forbids. Fail closed and make the runner say
	// which it meant.
	if len(req.GetOutputData()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "output_data is required unless execution_failed is set")
	}

	// Verdict path: load the row for its pinned comparison semantics + accepted result.
	auditRow, err := s.auditsRepo.GetByID(ctx, auditID)
	if err != nil {
		if isNotFound(err) {
			return nil, status.Error(codes.NotFound, "audit not found")
		}
		s.logger.Error("failed to load audit row", "method", "SubmitResult", "audit_id", auditID, "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	// The head computes the checksum over exactly the bytes the runner returned —
	// never a runner-claimed value (D6: a runner never self-adjudicates; the proto
	// deliberately carries no checksum field).
	sum := sha256.Sum256(req.GetOutputData())
	checksum := hex.EncodeToString(sum[:])

	// Assemble the accepted outputs the runner is adjudicated against: every AGREED
	// result on the unit, the accepted winner FIRST (the adjudicator's representative-
	// first contract). Entries may be nil for ref-only results.
	acceptedOutputs, err := s.acceptedOutputs(ctx, auditRow)
	if err != nil {
		s.logger.Error("failed to load accepted outputs for audit", "method", "SubmitResult", "audit_id", auditID, "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	// Adjudicate entirely head-side under the snapshot semantics. A compare bug must
	// never fail the RPC or fabricate a MISMATCH: it lands INCONCLUSIVE + Error log.
	verdict, detail, adjErr := s.adjudicator(auditRow.ComparisonSnapshot, derefString(auditRow.AcceptedComparisonKey), acceptedOutputs, req.GetOutputData())
	if adjErr != nil {
		s.logger.Error("audit adjudication failed", "method", "SubmitResult", "audit_id", auditID, "error", adjErr)
		verdict = audit.VerdictInconclusive
		detail = audit.ReasonCompareError + ": " + adjErr.Error()
	}

	if err := s.auditsRepo.CompleteVerdict(ctx, auditID, runner.ID, verdict, detail, req.GetOutputData(), checksum); err != nil {
		if errors.Is(err, audit.ErrNotClaimant) {
			return nil, status.Error(codes.FailedPrecondition, audit.ErrNotClaimant.Error())
		}
		s.logger.Error("failed to record audit verdict", "method", "SubmitResult", "audit_id", auditID, "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	// MISMATCH is the observe-only signal (slice 2): recorded above + WARNed here, with
	// no slash / clawback / result flip / trust or standing effect (those are slice 3).
	if verdict == audit.VerdictMismatch {
		s.logger.Warn("result audit mismatch",
			"audit_id", auditRow.ID,
			"work_unit_id", auditRow.WorkUnitID,
			"leaf_id", auditRow.LeafID,
			"runner_id", runner.ID,
			"accepted_key", derefString(auditRow.AcceptedComparisonKey),
			"runner_checksum", checksum,
		)
	}

	return &lettucev1.SubmitAuditResultResponse{Accepted: true}, nil
}

// acceptedOutputs returns the stored output_data of every AGREED result on the
// audited unit, with the accepted winner (AcceptedResultID) FIRST — the order the
// adjudicator contract requires (representative first). Entries may be nil for
// ref-only results.
func (s *auditServiceServer) acceptedOutputs(ctx context.Context, a *audit.Audit) ([]json.RawMessage, error) {
	results, err := s.resultRepo.ListByWorkUnit(ctx, a.WorkUnitID)
	if err != nil {
		return nil, err
	}
	outputs := make([]json.RawMessage, 0, len(results))
	// The accepted winner first.
	for _, r := range results {
		if r.ValidationStatus == result.ValidationAgreed && r.ID == a.AcceptedResultID {
			outputs = append(outputs, r.OutputData)
		}
	}
	// Then the remaining AGREED members, in list order.
	for _, r := range results {
		if r.ValidationStatus == result.ValidationAgreed && r.ID != a.AcceptedResultID {
			outputs = append(outputs, r.OutputData)
		}
	}
	return outputs, nil
}
