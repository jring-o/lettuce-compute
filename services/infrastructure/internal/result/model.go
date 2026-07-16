package result

import (
	"encoding/json"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// ValidationStatus represents the validation state of a result.
type ValidationStatus string

const (
	ValidationPending   ValidationStatus = "PENDING"
	ValidationAgreed    ValidationStatus = "AGREED"
	ValidationDisagreed ValidationStatus = "DISAGREED"
	// ValidationAwaitingContentVerification holds a ref-only (external output URL)
	// result before it may vote: the head has not yet fetched and hashed the external
	// bytes (design doc §10; BG-02b). Held rows are fail-closed everywhere — every
	// validation_status filter in the repo is positive-form, so a held result cannot
	// vote, count toward coverage or quorum, be repaired, earn credit, or be attested.
	ValidationAwaitingContentVerification ValidationStatus = "AWAITING_CONTENT_VERIFICATION"
	// ValidationContentVerificationFailed is the terminal did-not-become-votable state
	// for a ref-only result whose fetch pipeline ended without promoting it (reason
	// code in content_fetch_last_error: fetch failure, size cap, disallowed URL,
	// holding expiry, unit finalized). Permanently non-votable, same fail-closed
	// posture as the holding state.
	ValidationContentVerificationFailed ValidationStatus = "CONTENT_VERIFICATION_FAILED"
	// ValidationSuperseded marks a result whose work unit reached a terminal state
	// before the result was ever adjudicated — the dead-letter disposal (★BG-21i,
	// migration 00027): a below-quorum PENDING row must not survive its unit's FAILED
	// flip (it would orphan PENDING-under-terminal forever), but it was never compared
	// against anything either, so it is NOT an error signal — unlike DISAGREED it feeds
	// no error-copy count and no reliability penalty. The result-status analogue of a
	// copy's SUPERSEDED assignment outcome.
	ValidationSuperseded ValidationStatus = "SUPERSEDED"
)

// ExecutionMetadata holds self-reported resource metrics from the volunteer.
type ExecutionMetadata struct {
	WallClockSeconds int64   `json:"wall_clock_seconds"`
	CPUSecondsUser   float64 `json:"cpu_seconds_user"`
	CPUSecondsSystem float64 `json:"cpu_seconds_system"`
	CPUCoresUsed     int     `json:"cpu_cores_used"`
	GPUSeconds       float64 `json:"gpu_seconds"`
	GPUModel         string  `json:"gpu_model,omitempty"`
	GPUVRAMUsedMB    int     `json:"gpu_vram_used_mb"`
	PeakMemoryMB     int     `json:"peak_memory_mb"`
	DiskReadMB       int64   `json:"disk_read_mb"`
	DiskWriteMB      int64   `json:"disk_write_mb"`
	NetworkRxMB      int64   `json:"network_rx_mb"`
	NetworkTxMB      int64   `json:"network_tx_mb"`
}

// ExecutionMetadataFromProto converts a protobuf ExecutionMetadata to a Go struct.
func ExecutionMetadataFromProto(pb *lettucev1.ExecutionMetadata) ExecutionMetadata {
	if pb == nil {
		return ExecutionMetadata{}
	}
	return ExecutionMetadata{
		WallClockSeconds: pb.WallClockSeconds,
		CPUSecondsUser:   pb.CpuSecondsUser,
		CPUSecondsSystem: pb.CpuSecondsSystem,
		CPUCoresUsed:     int(pb.CpuCoresUsed),
		GPUSeconds:       pb.GpuSeconds,
		GPUModel:         pb.GpuModel,
		GPUVRAMUsedMB:    int(pb.GpuVramUsedMb),
		PeakMemoryMB:     int(pb.PeakMemoryMb),
		DiskReadMB:       pb.DiskReadMb,
		DiskWriteMB:      pb.DiskWriteMb,
		NetworkRxMB:      pb.NetworkRxMb,
		NetworkTxMB:      pb.NetworkTxMb,
	}
}

// Result is the output of a completed work unit submitted by a volunteer.
type Result struct {
	ID                types.ID          `json:"id"`
	WorkUnitID        types.ID          `json:"work_unit_id"`
	VolunteerID       types.ID          `json:"volunteer_id"`
	OutputData        json.RawMessage   `json:"output_data,omitempty"`
	OutputDataRef     *string           `json:"output_data_ref,omitempty"`
	OutputChecksum    string            `json:"output_checksum"`
	ExecutionMetadata ExecutionMetadata `json:"execution_metadata"`
	ValidationStatus  ValidationStatus  `json:"validation_status"`
	// ArtifactVersionID records which leaf_artifact_versions row produced this result
	// (TODO #38): the unit's pinned version, else the leaf's current version at submit.
	// nil = legacy / unversioned leaf. Lets validation refuse to compare results from
	// different artifact versions and gives per-result version provenance.
	ArtifactVersionID *types.ID `json:"artifact_version_id,omitempty"`
	// HostID attributes the result to the MACHINE that produced it (TODO #19), copied
	// from the volunteer's live copy row at submit. nil = a volunteer that reported no
	// host (per-account fallback).
	HostID *types.ID `json:"host_id,omitempty"`
	// TrustSubject / TrustScoreAtSubmit are the account-level trust snapshot stamped at
	// submission (see internal/trust): the subject resolved for the submitting volunteer
	// and that subject's score AT SUBMIT time. Acceptance decisions must use the
	// submission-time score (not a later, possibly slashed or re-accrued value), so it is
	// snapshotted per result rather than re-read at validation. Both nil = a legacy row
	// created before the trust feature.
	TrustSubject       *string    `json:"trust_subject,omitempty"`
	TrustScoreAtSubmit *int       `json:"trust_score_at_submit,omitempty"`
	// StandingAtSubmit is the submitter's EFFECTIVE account standing at
	// submission time (volunteer.EffectiveStanding), stamped alongside the trust
	// snapshot. Validation counts only OK-stamped results toward quorum and
	// redundancy coverage — the same as-of-submission semantics as the trust
	// score above. nil = a legacy row created before the standing feature (OK).
	StandingAtSubmit *string    `json:"standing_at_submit,omitempty"`
	// VerifiedOutputChecksum is the HEAD-computed sha256 (lowercase hex) of the bytes
	// the head actually fetched from output_data_ref — the ONLY checksum a ref-only
	// result may ever vote on (design doc §10.8). Non-nil means "the head hashed these
	// bytes itself"; nil on every inline result (whose output_checksum is already
	// head-verified at submit) and on every ref result not yet promoted.
	VerifiedOutputChecksum *string `json:"verified_output_checksum,omitempty"`
	// ContentFetchAttempts counts TRANSIENT fetch failures only (§10.6): a successful
	// fetch always promotes on the served hash and never consumes this budget.
	ContentFetchAttempts int `json:"content_fetch_attempts,omitempty"`
	// ContentFetchNextAttemptAt non-nil <=> the row is awaiting a fetch attempt (the
	// worker-scan predicate; set at submit, advanced on transient retry, cleared on
	// promotion and on every terminal disposition).
	ContentFetchNextAttemptAt *time.Time `json:"content_fetch_next_attempt_at,omitempty"`
	// ContentFetchLastError is the machine reason code (+ detail) of the most recent
	// fetch failure or terminal disposition.
	ContentFetchLastError *string    `json:"content_fetch_last_error,omitempty"`
	SubmittedAt           time.Time  `json:"submitted_at"`
	ValidatedAt           *time.Time `json:"validated_at,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}
