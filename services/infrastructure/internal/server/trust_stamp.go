package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/admission"
	"github.com/lettuce-compute/infrastructure/internal/config"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// TrustPolicyFromHeadConfig builds the head trust-gate policy (see internal/trust) from the
// head configuration. The process wiring (main.go) and the HTTP router both derive the policy
// from this one function, so the gRPC submit path, the browser submit path, the transitioner,
// and the validation engine all enforce identical numbers. A nil config yields the zero value
// (gate off).
func TrustPolicyFromHeadConfig(hc *config.HeadConfig) transition.TrustPolicy {
	if hc == nil {
		return transition.TrustPolicy{}
	}
	return transition.TrustPolicy{
		GateEnabled:             hc.TrustGateEnabled,
		DefaultMinCorroborators: hc.EffectiveTrustMinCorroborators(),
		DefaultFloor:            hc.EffectiveTrustFloor(),
	}
}

// TrustDispatchFromHeadConfig builds the workunit dispatch trust policy (the head-level
// (K, floor) the dispatch queries resolve per-leaf for the trusted-corroborator
// reservation) from the head configuration. It reads the SAME three sources as
// TrustPolicyFromHeadConfig — the validation-side head policy — so the dispatch reservation
// and the validation gate can never resolve different numbers from one config. A nil config
// yields the zero value (gate off, reservation inert). It exists as a distinct type because
// workunit cannot import transition (transition imports workunit), so the two policies are
// field-identical mirrors rather than one shared struct.
func TrustDispatchFromHeadConfig(hc *config.HeadConfig) workunit.TrustDispatchPolicy {
	if hc == nil {
		return workunit.TrustDispatchPolicy{}
	}
	return workunit.TrustDispatchPolicy{
		GateEnabled:             hc.TrustGateEnabled,
		DefaultMinCorroborators: hc.EffectiveTrustMinCorroborators(),
		DefaultFloor:            hc.EffectiveTrustFloor(),
	}
}

// trustDispatchFromPolicy mirrors an already-resolved head trust policy into the workunit
// dispatch policy. The two structs are field-identical (workunit cannot import transition),
// so this is the conversion the tx-scoped dispatch repos use to carry the same numbers the
// volunteer service was constructed with onto their per-request repository.
func trustDispatchFromPolicy(tp transition.TrustPolicy) workunit.TrustDispatchPolicy {
	return workunit.TrustDispatchPolicy{
		GateEnabled:             tp.GateEnabled,
		DefaultMinCorroborators: tp.DefaultMinCorroborators,
		DefaultFloor:            tp.DefaultFloor,
	}
}

// RegistrationCapFromHeadConfig builds the registration admission cap policy (design
// §4.1) from the head configuration. The process wiring (main.go, for the gRPC service
// via SetAdmissionPolicy) and the HTTP router (for the browser register path) both derive
// the policy from this one function, so the two create surfaces can never enforce
// different numbers from one config. A nil config yields the zero value (gate off).
func RegistrationCapFromHeadConfig(hc *config.HeadConfig) admission.CapPolicy {
	if hc == nil || !hc.RegistrationCapEnabled {
		return admission.CapPolicy{}
	}
	return admission.CapPolicy{
		Enabled: true,
		PerDay:  hc.EffectiveRegistrationCapPerIPPerDay(),
	}
}

// RegistrationPowFromHeadConfig builds the registration proof-of-work policy (design
// §4.1) from the head configuration, for both create surfaces (the same single-source
// rule as RegistrationCapFromHeadConfig). Unlike the cap policy, the effective
// difficulty and TTL are populated even while enforcement is OFF, because challenge
// ISSUANCE stays available regardless (probe-free clients). A nil config yields the
// zero value (enforcement off, issuance unconfigured — the bare-test wiring).
func RegistrationPowFromHeadConfig(hc *config.HeadConfig) admission.PowPolicy {
	if hc == nil {
		return admission.PowPolicy{}
	}
	return admission.PowPolicy{
		Enabled:        hc.RegistrationPowEnabled,
		DifficultyBits: hc.EffectiveRegistrationPowDifficultyBits(),
		ChallengeTTL:   time.Duration(hc.EffectiveRegistrationPowChallengeTTLSeconds()) * time.Second,
	}
}

// trustRepoFromPool returns a pgx-backed trust repository, or a genuine nil interface when
// the pool is nil (the gRPC-plumbing / mux-only tests), so stampTrustSnapshot's nil check
// works. It centralizes the "nil pool -> nil repo" idiom the submit paths share.
func trustRepoFromPool(pool *pgxpool.Pool) trust.Repository {
	if pool == nil {
		return nil
	}
	return trust.NewPgxRepository(pool)
}

// stampTrustSnapshot resolves the account-level trust subject, submission-time score, and
// effective account standing to record on a result (see internal/trust and
// internal/standing). It is shared by both live submit paths (the gRPC SubmitResult and the
// browser/WASM handler) so they stamp identically.
//
// The policy is fail-OPEN on work, fail-CLOSED on power:
//   - vol == nil (its row could not be loaded): the submission still succeeds — subject falls
//     back to the per-keypair sentinel, score is 0, and standing is OK. Stamping must never
//     block valid work; a legacy NULL standing stamp already reads as OK, so OK is the
//     behavior-preserving fallback here (trust separately stamps score 0, the fail-closed half).
//   - the volunteer's quorum power is suppressed (a STALE DID binding the head can no longer
//     re-verify, or an active post-rotation freeze): the subject is kept, but the score is
//     forced to 0, so a suppressed principal contributes a copy but no trusted-corroborator
//     power (QuorumPowerSuppressed owns this decision).
//   - trustRepo == nil, or GetScore errors: score is 0 (a WARN is logged on error).
//
// The standing return is the volunteer's EFFECTIVE standing at submit
// (volunteer.EffectiveStanding) and is UNCONDITIONAL — it does not depend on trust
// suppression or on whether any gate is enabled. Like the trust score, snapshots must
// accumulate before enforcement is switched on, so validation has a submission-time standing
// to read.
func stampTrustSnapshot(ctx context.Context, trustRepo trust.Repository, vol *volunteer.Volunteer, volunteerID types.ID, now time.Time, logger *slog.Logger) (subject string, score int, standing string) {
	if vol == nil {
		return trust.SubjectForVolunteerID(volunteerID), 0, volunteer.StandingOK
	}
	// Effective standing is resolved from the loaded row regardless of the trust outcome
	// below, so it is stamped even for a suppressed principal.
	standing = volunteer.EffectiveStanding(vol.Standing, vol.BenchedUntil, now)
	subject = trust.SubjectForVolunteer(vol)
	if trustRepo == nil || trust.QuorumPowerSuppressed(vol, now) {
		return subject, 0, standing
	}
	s, err := trustRepo.GetScore(ctx, subject)
	if err != nil {
		if logger != nil {
			logger.Warn("failed to read trust score at submit; stamping score 0", "subject", subject, "error", err)
		}
		return subject, 0, standing
	}
	return subject, s, standing
}
