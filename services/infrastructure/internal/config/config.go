package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Config is the top-level configuration for the Lettuce infrastructure server.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Log      LogConfig      `yaml:"log"`
	TLS      TLSConfig      `yaml:"tls"`
	Signing  SigningConfig  `yaml:"signing"`
	Storage  StorageConfig  `yaml:"storage"`
	Head     HeadConfig     `yaml:"head"`
}

// HeadConfig defines the identity for this Lettuce server (head).
// A head is a single infrastructure deployment — one server = one head = many leafs.
type HeadConfig struct {
	Name                    string         `yaml:"name"`
	Description             string         `yaml:"description"`
	URL                     string         `yaml:"url"`
	DefaultLeafWeights      map[string]int `yaml:"default_leaf_weights"`
	MaxInflightPerVolunteer int            `yaml:"max_inflight_per_volunteer"`

	// ArtifactRetention is the operator's retention policy for the per-leaf artifact
	// version registry (TODO #38): "all" (default — never auto-delete), "last:N" (keep
	// the N most-recently-published versions per leaf), or "current+previous" (keep
	// two). The leader-gated GC sweep enforces it and never deletes the current version
	// or a version pinned by in-flight work. Override via LETTUCE_ARTIFACT_RETENTION.
	ArtifactRetention string `yaml:"artifact_retention"`

	// --- Layer 3: horizontal scale-out (N stateless replicas, one Postgres) ---

	// InstanceID is this head replica's stable identity. It is the dispatch-claim
	// owner (dispatch_claimed_by), the leadership log field, and a log dimension.
	// EMPTY by default: when empty, a process-stable uuid is generated once at
	// boot via EffectiveInstanceID. Each replica MUST have a distinct value — do
	// NOT hardcode the same id across replicas (it would collide claim ownership).
	// Override via LETTUCE_HEAD_INSTANCE_ID.
	InstanceID string `yaml:"instance_id"`
	// RedisURL points at a shared Redis used as the cross-replica replay store and
	// (optionally) the shared rate-limit store. EMPTY by default: when empty the
	// head uses an in-process replay cache and in-process rate-limit buckets
	// (single-replica behavior). Required for N>1 replicas. Override via
	// LETTUCE_REDIS_URL.
	RedisURL string `yaml:"redis_url"`
	// ReplayFailMode controls behavior when the shared replay store errors (e.g. a
	// Redis outage): "open" admits the request (favoring fleet availability and not
	// losing completed compute on SubmitResult) with a loud alarm; "closed" rejects
	// it (favoring strict replay protection). Default "open". Override via
	// LETTUCE_REPLAY_FAIL_MODE.
	ReplayFailMode string `yaml:"replay_fail_mode"`
	// ClaimLeaseSeconds is how long a per-head dispatch claim is held before it
	// expires and the unit becomes re-claimable by any replica. The claim of an
	// actively-held unit is renewed every flush tick, so this is a backstop for a
	// crashed/wedged replica, not the steady-state lifetime. Default 120. Validated
	// against the flush cadence and the volunteer lease (see Validate). Override via
	// LETTUCE_HEAD_CLAIM_LEASE_SECONDS.
	ClaimLeaseSeconds int `yaml:"claim_lease_seconds"`

	// --- Layer 1: work batching, server-directed retry delay, buffer lease ---

	// MaxBatchPerRequest caps how many assignments a single RequestWorkUnit may
	// return (the client may request fewer via max_assignments). Default 8.
	MaxBatchPerRequest int `yaml:"max_batch_per_request"`
	// MinRetryDelaySeconds is the server-directed retry delay handed out when the
	// head is quiet. Default 30.
	MinRetryDelaySeconds int `yaml:"min_retry_delay_seconds"`
	// MaxRetryDelaySeconds is the retry delay under full load. Must stay strictly
	// below the 30-min StaleVolunteerMonitor threshold. Default 900 (15 min).
	MaxRetryDelaySeconds int `yaml:"max_retry_delay_seconds"`
	// RetryDelayJitterPct is the ±fraction of uniform jitter applied server-side
	// to the computed delay so a fleet does not re-contact in lockstep. Default 0.20.
	RetryDelayJitterPct float64 `yaml:"retry_delay_jitter_pct"`
	// TargetRequestRatePerSec is the per-head RequestWorkUnit rate that maps to
	// load=1 for the rate signal. SIMULATOR-CALIBRATED, not a trusted default.
	// Default 500.
	TargetRequestRatePerSec float64 `yaml:"target_request_rate_per_sec"`
	// LeaseSeconds is how long a buffered (reserved) unit is held for a volunteer
	// before the lease lapses and the unit becomes re-reservable. Must stay below
	// the 30-min stale threshold. Default 900 (15 min).
	LeaseSeconds int `yaml:"lease_seconds"`
	// MinSendIntervalSeconds is the minimum number of seconds between successful work
	// hand-outs to a SINGLE volunteer (keyed on its verified Ed25519 identity). It is a
	// server-side HARD FLOOR on a volunteer's work-acquisition cadence that does NOT
	// depend on the client honoring the advisory server-directed retry delay: a
	// self-compiled volunteer that ignores RetryAfterSeconds and polls at the per-pubkey
	// rate-limit ceiling still receives at most one batch per this interval, so it cannot
	// grab a disproportionate share of a scarce queue. ENABLED BY DEFAULT: unset (0)
	// resolves to defaultMinSendIntervalSeconds (30, ~= MinRetryDelaySeconds, so honest
	// clients that wait out the retry delay never trip it). Set NEGATIVE (e.g. -1) to
	// DISABLE (only the advisory retry delay, the per-pubkey/per-IP rate limits, and the
	// inflight cap then apply). A positive value is used verbatim and must be <=
	// MaxRetryDelaySeconds. See EffectiveMinSendIntervalSeconds. Override via
	// LETTUCE_HEAD_MIN_SEND_INTERVAL_SECONDS.
	MinSendIntervalSeconds int `yaml:"min_send_interval_seconds"`

	// --- Layer 2: in-process dispatch cache (per-replica, claim-coordinated) ---
	//
	// The dispatch cache serves RequestWorkUnit from an in-memory pool bulk-refilled
	// from Postgres, flushing reservations asynchronously, so the DB is off the
	// hot path. Each replica runs its OWN cache: Layer 3 makes this safe under N
	// replicas because the refill claims units on its head instance id (claim-on-
	// refill), so two replicas never double-hand the same QUEUED unit. With no head
	// instance id configured the refill is claim-free, which is correct for a single
	// replica.

	// ReadyPoolSize bounds the in-memory pool of pre-fetched, ready-to-assign QUEUED
	// units. Default 2000.
	ReadyPoolSize int `yaml:"ready_pool_size"`
	// RefillBatchSize is how many units one bulk refill (LIMIT N) pulls from
	// Postgres. Default 500.
	RefillBatchSize int `yaml:"refill_batch_size"`
	// DispatchAdmissionCap bounds concurrent CLIENT write-path dispatch-cache DB
	// operations — StartWork / SubmitResult / AbandonWorkUnit gates, the
	// RequestWorkUnit cold-miss identity read, getLeaf, resolveIdentity — so the pool
	// cannot saturate. The background refiller, ticker reservation-flush, and
	// spot-check landing run on the SEPARATE MaintenanceAdmissionCap budget so client
	// writers cannot starve cache restock. Default max(1, MaxConns/2); 0 = derive
	// from the pool.
	DispatchAdmissionCap int `yaml:"dispatch_admission_cap"`
	// MaintenanceAdmissionCap is the reserved admission budget for background restock
	// + landing (refiller, ticker reservation-flush, spot-check flush) so client
	// writes cannot starve them. Default max(1, dispatch_admission_cap/4); 0 = derive.
	MaintenanceAdmissionCap int `yaml:"maintenance_admission_cap"`
	// FlushIntervalMs is the async reservation-flush cadence (milliseconds).
	// Default 100.
	FlushIntervalMs int `yaml:"flush_interval_ms"`
	// FlushBatchSize flushes early once this many pending reservation writes
	// accumulate. Default 200.
	FlushBatchSize int `yaml:"flush_batch_size"`
	// NoDeadlineCeilingSeconds is the synthetic reclaim ceiling applied to NoDeadline
	// leafs so a unit on a vanished volunteer is always reclaimed (heartbeats are
	// gone). This is the DEADLINE, not a lease, so it is NOT bound by the 30-min
	// stale threshold. Default 21600 (6h), operator-tunable.
	NoDeadlineCeilingSeconds int `yaml:"no_deadline_ceiling_seconds"`

	// --- TODO #54: reliability-weighted ADAPTIVE work quota ---
	//
	// ReliabilityQuotaEnabled gates the per-MACHINE adaptive in-flight buffer: with it on,
	// a host's in-flight cap is a function of its MEASURED reliability (validated units grow
	// it, timeouts/abandons/disagreements shrink it) instead of the flat
	// MaxInflightPerVolunteer — the "earn your buffer" generalization of the flat #53 floor,
	// keyed on the #19 host id. ENABLED BY DEFAULT (nil): set false (env
	// LETTUCE_HEAD_RELIABILITY_QUOTA_ENABLED=false) to revert to today's flat per-host cap
	// for everyone (byte-for-byte). A warmed reliable host reaches exactly the flat cap, so
	// established well-behaved volunteers are unaffected in steady state; only brand-new or
	// flaky hosts are throttled toward the floor. Inert when MaxInflightPerVolunteer <= 0
	// (an unbounded cap cannot be shaped).
	ReliabilityQuotaEnabled *bool `yaml:"reliability_quota_enabled"`
	// ReliabilityQuotaFloor is the COLD-START / fully-throttled in-flight buffer a brand-new
	// or unknown host gets: small but non-zero (it never STARVES an honest new contributor —
	// it still runs a couple units while it proves itself), and never the full cap (a fresh
	// key does not get the full quota). An honest host ramps from this floor to the flat cap
	// over a few validated units. Unset (<= 0) -> defaultReliabilityQuotaFloor (2). Override
	// via LETTUCE_HEAD_RELIABILITY_QUOTA_FLOOR.
	ReliabilityQuotaFloor int `yaml:"reliability_quota_floor"`

	// --- Optional ATProto DID identity binding ---
	//
	// A volunteer MAY bind its Ed25519 account to a decentralized identifier (DID)
	// by publishing a key-authorization record in its own ATProto PDS ("Personal
	// Data Server") repo; the head verifies that record, stamps the binding onto the
	// volunteer row, and a worker re-verifies it on a TTL. The binding is OPTIONAL
	// identity metadata layered on top of the existing keypair auth — nothing about
	// keypair authentication changes when it is disabled.

	// DIDBindingEnabled gates the whole feature (the verification endpoint and the
	// recheck worker). OFF by default. Override via LETTUCE_HEAD_DID_BINDING_ENABLED.
	DIDBindingEnabled bool `yaml:"did_binding_enabled"`
	// DIDResolverURL is the base URL of the DID resolver used to resolve a DID to its
	// DID document (the PDS endpoint and signing keys). Default "https://plc.directory".
	// Override via LETTUCE_HEAD_DID_RESOLVER_URL.
	DIDResolverURL string `yaml:"did_resolver_url"`
	// DIDRecheckTTLSeconds is how long a verified binding is trusted before it is due
	// for re-verification. SECURITY PARAMETER: the worst-case latency between a
	// volunteer revoking its authorization record and the head observing the
	// revocation is DIDRecheckTTLSeconds + DIDRecheckIntervalSeconds (the TTL plus one
	// worker sweep). Lower it to tighten revocation latency at the cost of more
	// resolver traffic. Default 3600 (1h); effective value must be > 0. Override via
	// LETTUCE_HEAD_DID_RECHECK_TTL_SECONDS.
	DIDRecheckTTLSeconds int `yaml:"did_recheck_ttl_seconds"`
	// DIDRecheckIntervalSeconds is the recheck worker's sweep cadence — one input to
	// the worst-case revocation latency above. Default 300 (5m); effective value must
	// be > 0. Override via LETTUCE_HEAD_DID_RECHECK_INTERVAL_SECONDS.
	DIDRecheckIntervalSeconds int `yaml:"did_recheck_interval_seconds"`
	// DIDStaleAfterFailures is how many CONSECUTIVE failed re-verification attempts
	// (transient resolve/fetch errors) escalate a binding from OK to STALE. Default 3;
	// effective value must be > 0. Override via LETTUCE_HEAD_DID_STALE_AFTER_FAILURES.
	DIDStaleAfterFailures int `yaml:"did_stale_after_failures"`
	// DIDRotationFreezeHours is the post-rotation cool-down: after a volunteer's DID
	// signing key rotates, its binding is frozen for this many hours before it may
	// re-bind (anti-abuse). Default 72; must be >= 0. Override via
	// LETTUCE_HEAD_DID_ROTATION_FREEZE_HOURS.
	DIDRotationFreezeHours int `yaml:"did_rotation_freeze_hours"`
	// DIDBindingCollection is the ATProto collection (record namespace) the head looks
	// for the key-authorization record under in a volunteer's PDS repo. Default
	// "tech.scios.lettuce.keyAuthorization". NOTE: this namespace is PENDING an
	// operator decision and is overridable until the first production use — do not
	// treat it as stable yet. Override via LETTUCE_HEAD_DID_BINDING_COLLECTION.
	DIDBindingCollection string `yaml:"did_binding_collection"`

	// --- Account-level trust gate (quorum power) ---
	//
	// The trust gate (see internal/trust) hardens redundant validation against Sybil
	// accounts: it requires an agreeing group to contain enough DISTINCT TRUSTED SUBJECTS
	// (K), where a subject is trusted once its submission-time score is at or above a
	// floor (W). Trust is earned by corroborated-clean work and is operator-seedable.

	// TrustGateEnabled is the master switch. OFF by default. When false the gate NEVER
	// blocks validation (the effective K resolves to 0), but submission-time trust
	// stamping and accrual still run — so an operator can turn the head on, let trust
	// accumulate for a while, then enable enforcement without starting cold. Override via
	// LETTUCE_HEAD_TRUST_GATE_ENABLED.
	TrustGateEnabled bool `yaml:"trust_gate_enabled"`
	// TrustMinCorroborators is the head-default K: the number of distinct trusted subjects
	// an agreeing group must contain when the gate is on and a leaf does not override it
	// (leaf.ValidationConfig.MinTrustedCorroborators). Unset (0) resolves to the default 1;
	// effective value must be > 0. Override via LETTUCE_HEAD_TRUST_MIN_CORROBORATORS.
	TrustMinCorroborators int `yaml:"trust_min_corroborators"`
	// TrustFloor is the head-default trust floor W: the submission-time score at or above
	// which a subject counts as trusted when a leaf does not override it
	// (leaf.ValidationConfig.TrustFloor). Because accrual adds 1 per corroborated-clean
	// unit, W is also the ramp LENGTH — a subject reaches quorum power after ~W
	// corroborated-clean units (or an operator seed). Unset (0) resolves to the default 25;
	// effective value must be > 0. Override via LETTUCE_HEAD_TRUST_FLOOR.
	TrustFloor int `yaml:"trust_floor"`
}

// Layer-1 defaults and the stale-volunteer threshold both delays and the lease
// must stay strictly below.
const (
	// defaultMaxBatchPerRequest is a SAFETY CEILING on the per-request batch, not
	// the primary limiter — Layer 2 (#29) makes the client's hours-deficit math the
	// real limiter, so this is raised to 64 to let short-unit leafs fill their
	// work_buffer_hours instead of idling at the old flat cap of 8.
	defaultMaxBatchPerRequest      = 64
	defaultMinRetryDelaySeconds    = 30
	defaultMaxRetryDelaySeconds    = 900
	defaultRetryDelayJitterPct     = 0.20
	defaultTargetRequestRatePerSec = 500.0
	defaultLeaseSeconds            = 900
	// defaultMinSendIntervalSeconds is the per-volunteer minimum work-send interval
	// applied when MinSendIntervalSeconds is left unset (0). It is ENABLED by default
	// (a fairness/anti-abuse floor): it matches defaultMinRetryDelaySeconds so a
	// volunteer that honors the advisory retry delay never trips it, while a client
	// that ignores the delay is still capped to this cadence. Set the field negative to
	// disable. Clamped down to the effective max retry delay so it can never exceed it.
	defaultMinSendIntervalSeconds = 30
	// defaultReliabilityQuotaEnabled is the #54 default: the adaptive per-host quota is ON
	// unless explicitly disabled, consistent with the #53 send-interval floor. Persistence
	// of the score + a refresher prime at start keep established hosts at their earned
	// budget across a head restart, so "on by default" does not disrupt them.
	defaultReliabilityQuotaEnabled = true
	// defaultReliabilityQuotaFloor is the cold-start in-flight buffer for a host with no
	// measured signal yet (#54). Small but non-zero so an honest new host is never starved.
	defaultReliabilityQuotaFloor = 2
	// staleVolunteerThresholdSeconds mirrors StaleVolunteerMonitor's 30-min
	// inactivity threshold; retry delay and lease must stay strictly below it so a
	// throttled-but-healthy volunteer is never marked inactive.
	staleVolunteerThresholdSeconds = 1800

	// --- Layer 2 dispatch-cache defaults ---
	defaultReadyPoolSize            = 2000
	defaultRefillBatchSize          = 500
	defaultFlushIntervalMs          = 100
	defaultFlushBatchSize           = 200
	defaultNoDeadlineCeilingSeconds = 21600 // 6h

	// --- Optional DID identity-binding defaults ---
	defaultDIDResolverURL            = "https://plc.directory"
	defaultDIDRecheckTTLSeconds      = 3600 // 1h
	defaultDIDRecheckIntervalSeconds = 300  // 5m
	defaultDIDStaleAfterFailures     = 3
	defaultDIDRotationFreezeHours    = 72 // 3 days
	// defaultDIDBindingCollection is the record namespace the head looks for a
	// volunteer's key-authorization record under. PENDING an operator decision;
	// overridable until first production use (see HeadConfig.DIDBindingCollection).
	defaultDIDBindingCollection = "tech.scios.lettuce.keyAuthorization"

	// --- Account-level trust-gate defaults ---
	// defaultTrustMinCorroborators is the head-default K when the gate is on and a leaf
	// does not override it: require at least one trusted subject in the agreeing group.
	defaultTrustMinCorroborators = 1
	// defaultTrustFloor is the head-default trust floor W (also the accrual ramp length):
	// a subject reaches quorum power after ~25 corroborated-clean units (or an operator
	// seed to at least this score).
	defaultTrustFloor = 25

	// --- Layer 3 scale-out defaults ---
	defaultClaimLeaseSeconds = 120
	// minClaimLeaseFloorMs is the absolute lower bound (ms) on the dispatch-claim
	// lease so it always vastly exceeds worst-case flush lag even if the flusher is
	// briefly starved under shedding.
	minClaimLeaseFloorMs = 5000
	// replayFailModeOpen / replayFailModeClosed are the two valid replay_fail_mode
	// values.
	replayFailModeOpen   = "open"
	replayFailModeClosed = "closed"
)

// Validate checks HeadConfig for required fields and valid values.
func (h HeadConfig) Validate() error {
	if h.Name == "" {
		return fmt.Errorf("head.name is required")
	}
	if h.URL != "" {
		u, err := url.Parse(h.URL)
		if err != nil {
			return fmt.Errorf("head.url is invalid: %w", err)
		}
		if u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("head.url must include scheme and host (e.g. https://your-domain.com)")
		}
	}
	for slug, weight := range h.DefaultLeafWeights {
		if weight <= 0 {
			return fmt.Errorf("head.default_leaf_weights[%q] must be > 0, got %d", slug, weight)
		}
	}
	if h.MaxInflightPerVolunteer < 0 {
		return fmt.Errorf("head.max_inflight_per_volunteer must be >= 0, got %d", h.MaxInflightPerVolunteer)
	}
	if h.MaxBatchPerRequest < 0 {
		return fmt.Errorf("head.max_batch_per_request must be >= 0, got %d", h.MaxBatchPerRequest)
	}
	if h.MinRetryDelaySeconds < 0 {
		return fmt.Errorf("head.min_retry_delay_seconds must be >= 0, got %d", h.MinRetryDelaySeconds)
	}
	if h.MaxRetryDelaySeconds < 0 {
		return fmt.Errorf("head.max_retry_delay_seconds must be >= 0, got %d", h.MaxRetryDelaySeconds)
	}
	if h.MaxRetryDelaySeconds >= staleVolunteerThresholdSeconds {
		return fmt.Errorf("head.max_retry_delay_seconds must be < %d (the stale-volunteer threshold), got %d",
			staleVolunteerThresholdSeconds, h.MaxRetryDelaySeconds)
	}
	if h.MinRetryDelaySeconds > 0 && h.MaxRetryDelaySeconds > 0 && h.MinRetryDelaySeconds > h.MaxRetryDelaySeconds {
		return fmt.Errorf("head.min_retry_delay_seconds (%d) must be <= max_retry_delay_seconds (%d)",
			h.MinRetryDelaySeconds, h.MaxRetryDelaySeconds)
	}
	if h.RetryDelayJitterPct < 0 || h.RetryDelayJitterPct >= 1 {
		return fmt.Errorf("head.retry_delay_jitter_pct must be in [0, 1), got %v", h.RetryDelayJitterPct)
	}
	if h.TargetRequestRatePerSec < 0 {
		return fmt.Errorf("head.target_request_rate_per_sec must be >= 0, got %v", h.TargetRequestRatePerSec)
	}
	if h.LeaseSeconds < 0 {
		return fmt.Errorf("head.lease_seconds must be >= 0, got %d", h.LeaseSeconds)
	}
	// A negative value is the explicit "disable" sentinel (see
	// EffectiveMinSendIntervalSeconds); only an EXPLICIT positive value is range-checked.
	// A positive send floor above the full-load advisory delay would throttle even
	// retry-delay-honoring clients, so cap it at the max retry delay (raw, guarded by >0
	// so the unset default is resolved/clamped by Effective, not rejected here).
	if h.MinSendIntervalSeconds > 0 && h.MaxRetryDelaySeconds > 0 && h.MinSendIntervalSeconds > h.MaxRetryDelaySeconds {
		return fmt.Errorf("head.min_send_interval_seconds (%d) must be <= max_retry_delay_seconds (%d); set it negative to disable",
			h.MinSendIntervalSeconds, h.MaxRetryDelaySeconds)
	}
	// lease_seconds is only a fallback reservation window for a unit that has no
	// positive deadline. The buffered hold is the unit's head-owned deadline, so it
	// is no longer bounded by the stale-volunteer threshold — a volunteer keeps
	// buffered work until that deadline regardless of how long it is.
	if h.ReadyPoolSize < 0 {
		return fmt.Errorf("head.ready_pool_size must be >= 0, got %d", h.ReadyPoolSize)
	}
	if h.RefillBatchSize < 0 {
		return fmt.Errorf("head.refill_batch_size must be >= 0, got %d", h.RefillBatchSize)
	}
	if h.DispatchAdmissionCap < 0 {
		return fmt.Errorf("head.dispatch_admission_cap must be >= 0, got %d", h.DispatchAdmissionCap)
	}
	if h.MaintenanceAdmissionCap < 0 {
		return fmt.Errorf("head.maintenance_admission_cap must be >= 0, got %d", h.MaintenanceAdmissionCap)
	}
	if h.FlushIntervalMs < 0 {
		return fmt.Errorf("head.flush_interval_ms must be >= 0, got %d", h.FlushIntervalMs)
	}
	if h.FlushBatchSize < 0 {
		return fmt.Errorf("head.flush_batch_size must be >= 0, got %d", h.FlushBatchSize)
	}
	// NoDeadlineCeilingSeconds is a DEADLINE, not a lease, so it is intentionally
	// NOT bound by the 30-min stale threshold (a 6h reclaim ceiling is valid).
	if h.NoDeadlineCeilingSeconds < 0 {
		return fmt.Errorf("head.no_deadline_ceiling_seconds must be >= 0, got %d", h.NoDeadlineCeilingSeconds)
	}

	// --- Layer 3 scale-out validation ---

	// InstanceID is optional; if set it must parse as a UUID so it is a valid
	// claim owner / log identity.
	if h.InstanceID != "" {
		if _, err := uuid.Parse(h.InstanceID); err != nil {
			return fmt.Errorf("head.instance_id must be a valid UUID, got %q: %w", h.InstanceID, err)
		}
	}
	if h.ReplayFailMode != "" && h.ReplayFailMode != replayFailModeOpen && h.ReplayFailMode != replayFailModeClosed {
		return fmt.Errorf("head.replay_fail_mode must be %q or %q, got %q", replayFailModeOpen, replayFailModeClosed, h.ReplayFailMode)
	}
	if h.ClaimLeaseSeconds < 0 {
		return fmt.Errorf("head.claim_lease_seconds must be >= 0, got %d", h.ClaimLeaseSeconds)
	}
	// Enforce the design floors so a claim never expires while its unit is
	// actively held. These compare the EFFECTIVE (defaulted) values, not the raw
	// fields, because an unset claim_lease_seconds still resolves to a concrete
	// 120s lease that must satisfy the invariants — e.g. a small explicit
	// lease_seconds with claim_lease_seconds=0 (default 120) would otherwise
	// silently violate "claim lease < reservation lease":
	//   * the lease must vastly exceed worst-case flush lag (>= max(10*flush, 5s)),
	//     so flush-renewal extends it long before expiry, and
	//   * the lease must stay below the volunteer reservation lease so a stranded
	//     dispatch claim can never outlive the unit's reservation window.
	effClaimLease := h.EffectiveClaimLeaseSeconds()
	leaseMs := effClaimLease * 1000
	flushMs := h.EffectiveFlushIntervalMs()
	floorMs := 10 * flushMs
	if floorMs < minClaimLeaseFloorMs {
		floorMs = minClaimLeaseFloorMs
	}
	if leaseMs < floorMs {
		return fmt.Errorf("head.claim_lease_seconds (effective %d) is too short: must be >= %d ms (max(10*flush_interval_ms, %d))",
			effClaimLease, floorMs, minClaimLeaseFloorMs)
	}
	if effClaimLease >= h.EffectiveLeaseSeconds() {
		return fmt.Errorf("head.claim_lease_seconds (effective %d) must be < lease_seconds (effective %d)",
			effClaimLease, h.EffectiveLeaseSeconds())
	}

	// --- Optional DID identity-binding validation ---
	// The TTL/interval/stale-after knobs reject only NEGATIVE raw values; a raw 0
	// means "unset -> use the (positive) default", mirroring the other dispatch
	// knobs, so a minimal config that omits them stays valid. rotation_freeze_hours
	// may legitimately be 0 (no freeze), so it too only rejects negatives.
	if h.DIDRecheckTTLSeconds < 0 {
		return fmt.Errorf("head.did_recheck_ttl_seconds must be >= 0, got %d", h.DIDRecheckTTLSeconds)
	}
	if h.DIDRecheckIntervalSeconds < 0 {
		return fmt.Errorf("head.did_recheck_interval_seconds must be >= 0, got %d", h.DIDRecheckIntervalSeconds)
	}
	if h.DIDStaleAfterFailures < 0 {
		return fmt.Errorf("head.did_stale_after_failures must be >= 0, got %d", h.DIDStaleAfterFailures)
	}
	if h.DIDRotationFreezeHours < 0 {
		return fmt.Errorf("head.did_rotation_freeze_hours must be >= 0, got %d", h.DIDRotationFreezeHours)
	}
	// The resolver endpoint is a trust anchor for binding verification, so fail fast
	// on a malformed URL rather than discovering it at first recheck.
	if h.DIDResolverURL != "" {
		u, err := url.Parse(h.DIDResolverURL)
		if err != nil {
			return fmt.Errorf("head.did_resolver_url is invalid: %w", err)
		}
		if u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("head.did_resolver_url must include scheme and host (e.g. https://plc.directory)")
		}
	}

	// --- Account-level trust-gate validation ---
	// K and the floor reject only NEGATIVE raw values; a raw 0 means "unset -> use the
	// (positive) default", mirroring the DID and dispatch knobs, so a minimal config that
	// omits them stays valid and their EFFECTIVE values are always > 0.
	if h.TrustMinCorroborators < 0 {
		return fmt.Errorf("head.trust_min_corroborators must be >= 0, got %d", h.TrustMinCorroborators)
	}
	if h.TrustFloor < 0 {
		return fmt.Errorf("head.trust_floor must be >= 0, got %d", h.TrustFloor)
	}
	return nil
}

// EffectiveMaxInflight returns the max inflight WUs per volunteer,
// defaulting to 10 if not set (0).
// EffectiveArtifactRetentionKeep parses ArtifactRetention into the number of newest
// versions to keep per leaf for the GC sweep: 0 means "keep all" (GC disabled).
// Unknown values fail safe to "all" (a typo never auto-deletes artifacts).
func (h HeadConfig) EffectiveArtifactRetentionKeep() int {
	p := strings.TrimSpace(strings.ToLower(h.ArtifactRetention))
	switch {
	case p == "" || p == "all":
		return 0
	case p == "current+previous":
		return 2
	case strings.HasPrefix(p, "last:"):
		n, err := strconv.Atoi(strings.TrimSpace(p[len("last:"):]))
		if err != nil || n < 1 {
			return 0
		}
		return n
	default:
		return 0
	}
}

func (h HeadConfig) EffectiveMaxInflight() int {
	if h.MaxInflightPerVolunteer <= 0 {
		return 10
	}
	return h.MaxInflightPerVolunteer
}

// EffectiveMaxBatch returns the per-request batch cap, defaulting to 8 if unset.
func (h HeadConfig) EffectiveMaxBatch() int {
	if h.MaxBatchPerRequest <= 0 {
		return defaultMaxBatchPerRequest
	}
	return h.MaxBatchPerRequest
}

// EffectiveMinRetryDelaySeconds returns the quiet-load retry delay, default 30.
func (h HeadConfig) EffectiveMinRetryDelaySeconds() int {
	if h.MinRetryDelaySeconds <= 0 {
		return defaultMinRetryDelaySeconds
	}
	return h.MinRetryDelaySeconds
}

// EffectiveMaxRetryDelaySeconds returns the full-load retry delay, default 900.
func (h HeadConfig) EffectiveMaxRetryDelaySeconds() int {
	if h.MaxRetryDelaySeconds <= 0 {
		return defaultMaxRetryDelaySeconds
	}
	return h.MaxRetryDelaySeconds
}

// EffectiveRetryDelayJitterPct returns the jitter fraction, default 0.20.
// A configured 0 is treated as "use the default"; disable jitter is not a
// supported mode (the anti-synchronization property is load-bearing).
func (h HeadConfig) EffectiveRetryDelayJitterPct() float64 {
	if h.RetryDelayJitterPct <= 0 {
		return defaultRetryDelayJitterPct
	}
	return h.RetryDelayJitterPct
}

// EffectiveTargetRequestRatePerSec returns the rate-signal target, default 500.
func (h HeadConfig) EffectiveTargetRequestRatePerSec() float64 {
	if h.TargetRequestRatePerSec <= 0 {
		return defaultTargetRequestRatePerSec
	}
	return h.TargetRequestRatePerSec
}

// EffectiveLeaseSeconds returns the buffer-lease window, default 900.
func (h HeadConfig) EffectiveLeaseSeconds() int {
	if h.LeaseSeconds <= 0 {
		return defaultLeaseSeconds
	}
	return h.LeaseSeconds
}

// EffectiveMinSendIntervalSeconds returns the per-volunteer minimum work-send
// interval in seconds. It is ENABLED BY DEFAULT:
//   - unset (0)      -> defaultMinSendIntervalSeconds (30), clamped to never exceed the
//                       effective max retry delay (so an unusually low max_retry_delay
//                       can never make the default floor nonsensical / block boot).
//   - negative       -> 0 = explicitly DISABLED (only the advisory retry delay, the
//                       per-pubkey/per-IP rate limits, and the inflight cap then apply).
//   - positive       -> that value verbatim (validated <= max_retry_delay_seconds).
func (h HeadConfig) EffectiveMinSendIntervalSeconds() int {
	if h.MinSendIntervalSeconds < 0 {
		return 0 // explicitly disabled via negative sentinel
	}
	if h.MinSendIntervalSeconds == 0 {
		d := defaultMinSendIntervalSeconds
		if m := h.EffectiveMaxRetryDelaySeconds(); d > m {
			d = m
		}
		return d
	}
	return h.MinSendIntervalSeconds
}

// EffectiveReliabilityQuotaEnabled reports whether the adaptive per-host work quota (#54)
// is on. ENABLED BY DEFAULT: nil (unset) -> defaultReliabilityQuotaEnabled (true); an
// explicit false disables it (today's flat per-host cap for everyone).
func (h HeadConfig) EffectiveReliabilityQuotaEnabled() bool {
	if h.ReliabilityQuotaEnabled == nil {
		return defaultReliabilityQuotaEnabled
	}
	return *h.ReliabilityQuotaEnabled
}

// EffectiveReliabilityQuotaFloor returns the cold-start in-flight buffer for a host with no
// measured signal yet (#54): unset (<= 0) -> defaultReliabilityQuotaFloor (2).
func (h HeadConfig) EffectiveReliabilityQuotaFloor() int {
	if h.ReliabilityQuotaFloor <= 0 {
		return defaultReliabilityQuotaFloor
	}
	return h.ReliabilityQuotaFloor
}

// EffectiveReadyPoolSize returns the dispatch-cache ready-pool cap, default 2000.
func (h HeadConfig) EffectiveReadyPoolSize() int {
	if h.ReadyPoolSize <= 0 {
		return defaultReadyPoolSize
	}
	return h.ReadyPoolSize
}

// EffectiveRefillBatchSize returns the bulk-refill LIMIT, default 500.
func (h HeadConfig) EffectiveRefillBatchSize() int {
	if h.RefillBatchSize <= 0 {
		return defaultRefillBatchSize
	}
	return h.RefillBatchSize
}

// EffectiveDispatchAdmissionCap returns the configured admission cap, or 0 to let
// the caller derive it from the DB pool (max(1, MaxConns/2)).
func (h HeadConfig) EffectiveDispatchAdmissionCap() int {
	if h.DispatchAdmissionCap < 0 {
		return 0
	}
	return h.DispatchAdmissionCap
}

// EffectiveMaintenanceAdmissionCap returns the configured reserved maintenance
// admission budget, or 0 to let the caller derive it (max(1, admissionCap/4)).
func (h HeadConfig) EffectiveMaintenanceAdmissionCap() int {
	if h.MaintenanceAdmissionCap < 0 {
		return 0
	}
	return h.MaintenanceAdmissionCap
}

// EffectiveFlushIntervalMs returns the async flush cadence in ms, default 100.
func (h HeadConfig) EffectiveFlushIntervalMs() int {
	if h.FlushIntervalMs <= 0 {
		return defaultFlushIntervalMs
	}
	return h.FlushIntervalMs
}

// EffectiveFlushBatchSize returns the early-flush threshold, default 200.
func (h HeadConfig) EffectiveFlushBatchSize() int {
	if h.FlushBatchSize <= 0 {
		return defaultFlushBatchSize
	}
	return h.FlushBatchSize
}

// EffectiveNoDeadlineCeilingSeconds returns the synthetic reclaim ceiling for
// NoDeadline leafs, default 21600 (6h).
func (h HeadConfig) EffectiveNoDeadlineCeilingSeconds() int {
	if h.NoDeadlineCeilingSeconds <= 0 {
		return defaultNoDeadlineCeilingSeconds
	}
	return h.NoDeadlineCeilingSeconds
}

// EffectiveInstanceID returns this head replica's stable identity as a types.ID
// (uuid). When InstanceID is configured it is parsed and returned; otherwise a
// fresh uuid is generated. Generate ONCE at boot and reuse the result — calling
// this repeatedly with an unset InstanceID yields different ids each time.
func (h HeadConfig) EffectiveInstanceID() uuid.UUID {
	if h.InstanceID != "" {
		// Validate has already verified this parses; ignore the error here and
		// fall back to a generated id on the impossible parse failure.
		if id, err := uuid.Parse(h.InstanceID); err == nil {
			return id
		}
	}
	return uuid.New()
}

// EffectiveReplayFailMode returns the configured replay-store failure policy,
// defaulting to "open" (admit on store error, favoring availability).
func (h HeadConfig) EffectiveReplayFailMode() string {
	if h.ReplayFailMode == "" {
		return replayFailModeOpen
	}
	return h.ReplayFailMode
}

// ReplayFailsOpen reports whether a replay-store error should admit the request
// (fail open) rather than reject it (fail closed).
func (h HeadConfig) ReplayFailsOpen() bool {
	return h.EffectiveReplayFailMode() == replayFailModeOpen
}

// EffectiveClaimLeaseSeconds returns the per-head dispatch-claim lease window in
// seconds, default 120.
func (h HeadConfig) EffectiveClaimLeaseSeconds() int {
	if h.ClaimLeaseSeconds <= 0 {
		return defaultClaimLeaseSeconds
	}
	return h.ClaimLeaseSeconds
}

// EffectiveDIDResolverURL returns the DID resolver base URL, default
// "https://plc.directory".
func (h HeadConfig) EffectiveDIDResolverURL() string {
	if h.DIDResolverURL == "" {
		return defaultDIDResolverURL
	}
	return h.DIDResolverURL
}

// EffectiveDIDRecheckTTLSeconds returns the binding-trust TTL in seconds, default
// 3600 (unset/0 -> default).
func (h HeadConfig) EffectiveDIDRecheckTTLSeconds() int {
	if h.DIDRecheckTTLSeconds <= 0 {
		return defaultDIDRecheckTTLSeconds
	}
	return h.DIDRecheckTTLSeconds
}

// EffectiveDIDRecheckIntervalSeconds returns the recheck worker's sweep cadence in
// seconds, default 300 (unset/0 -> default).
func (h HeadConfig) EffectiveDIDRecheckIntervalSeconds() int {
	if h.DIDRecheckIntervalSeconds <= 0 {
		return defaultDIDRecheckIntervalSeconds
	}
	return h.DIDRecheckIntervalSeconds
}

// EffectiveDIDStaleAfterFailures returns the consecutive-failure count that
// escalates a binding to STALE, default 3 (unset/0 -> default).
func (h HeadConfig) EffectiveDIDStaleAfterFailures() int {
	if h.DIDStaleAfterFailures <= 0 {
		return defaultDIDStaleAfterFailures
	}
	return h.DIDStaleAfterFailures
}

// EffectiveDIDRotationFreezeHours returns the post-rotation re-bind freeze in
// hours, default 72. Unset (0) resolves to the default; an explicit non-zero
// value is used verbatim. Validate rejects negatives.
func (h HeadConfig) EffectiveDIDRotationFreezeHours() int {
	if h.DIDRotationFreezeHours <= 0 {
		return defaultDIDRotationFreezeHours
	}
	return h.DIDRotationFreezeHours
}

// EffectiveDIDBindingCollection returns the ATProto collection the head looks for
// the key-authorization record under, default the tech.scios.lettuce namespace.
func (h HeadConfig) EffectiveDIDBindingCollection() string {
	if h.DIDBindingCollection == "" {
		return defaultDIDBindingCollection
	}
	return h.DIDBindingCollection
}

// EffectiveTrustMinCorroborators returns the head-default trust-gate K (distinct trusted
// subjects required), default 1. Unset (<= 0) -> defaultTrustMinCorroborators. This is
// the head default only; the resolved K is 0 when TrustGateEnabled is false, and a leaf
// may override it via ValidationConfig.MinTrustedCorroborators.
func (h HeadConfig) EffectiveTrustMinCorroborators() int {
	if h.TrustMinCorroborators <= 0 {
		return defaultTrustMinCorroborators
	}
	return h.TrustMinCorroborators
}

// EffectiveTrustFloor returns the head-default trust floor W (the score at or above which
// a subject is trusted, and the accrual ramp length), default 25. Unset (<= 0) ->
// defaultTrustFloor. A leaf may override it via ValidationConfig.TrustFloor.
func (h HeadConfig) EffectiveTrustFloor() int {
	if h.TrustFloor <= 0 {
		return defaultTrustFloor
	}
	return h.TrustFloor
}

// StorageConfig defines local filesystem storage settings.
type StorageConfig struct {
	CheckpointDir string `yaml:"checkpoint_dir"`
}

// SigningConfig defines the Ed25519 signing key used for credit attestations.
type SigningConfig struct {
	PrivateKeyPath string `yaml:"private_key_path"`
	// AutoGenerate, when true, lets the server generate and persist a new
	// ephemeral signing key if the configured key file is missing. This is a
	// development-only convenience: in production the key is the platform's
	// external trust anchor and must be pre-generated. Defaults to false
	// (fail closed). Override via LETTUCE_SIGNING_KEY_AUTOGEN=true.
	AutoGenerate bool `yaml:"autogenerate"`
}

// ServerConfig defines listen addresses for HTTP and gRPC servers.
type ServerConfig struct {
	HTTPAddr    string `yaml:"http_addr"`
	GRPCAddr    string `yaml:"grpc_addr"`
	CORSOrigins string `yaml:"cors_origins"`
	// TrustedProxies is a comma-separated list of CIDRs and/or bare IPs of
	// reverse proxies whose X-Forwarded-For / X-Real-IP headers may be trusted
	// for client-IP extraction. Bare IPs are treated as /32 (IPv4) or /128
	// (IPv6). EMPTY by default: when empty, forwarding headers are never trusted
	// and the direct peer (RemoteAddr) is always used. Override via
	// LETTUCE_TRUSTED_PROXIES.
	TrustedProxies string `yaml:"trusted_proxies"`
}

// ParsedTrustedProxies parses the TrustedProxies string into a slice of
// *net.IPNet. Comma-separated entries may be CIDRs (e.g. "10.0.0.0/8") or bare
// IPs (e.g. "172.18.0.5"), where a bare IP is treated as a /32 (IPv4) or /128
// (IPv6) network. Empty or whitespace-only entries are skipped. Returns an
// error on the first malformed entry. An empty input yields a nil slice
// (the secure default: no header trust).
func (s ServerConfig) ParsedTrustedProxies() ([]*net.IPNet, error) {
	return ParseTrustedProxies(s.TrustedProxies)
}

// ParseTrustedProxies parses a comma-separated list of CIDRs and/or bare IPs
// into *net.IPNet networks. See ServerConfig.ParsedTrustedProxies for semantics.
func ParseTrustedProxies(raw string) ([]*net.IPNet, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var nets []*net.IPNet
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Try CIDR first.
		if _, ipNet, err := net.ParseCIDR(entry); err == nil {
			nets = append(nets, ipNet)
			continue
		}
		// Fall back to a bare IP → /32 or /128.
		ip := net.ParseIP(entry)
		if ip == nil {
			return nil, fmt.Errorf("trusted_proxies: invalid CIDR or IP %q", entry)
		}
		mask := net.CIDRMask(32, 32)
		if ip.To4() == nil {
			mask = net.CIDRMask(128, 128)
		}
		nets = append(nets, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
	}
	return nets, nil
}

// DatabaseConfig defines PostgreSQL connection parameters.
type DatabaseConfig struct {
	Host            string `yaml:"host"`
	Port            int    `yaml:"port"`
	Database        string `yaml:"database"`
	User            string `yaml:"user"`
	Password        string `yaml:"password"`
	SSLMode         string `yaml:"ssl_mode"`
	MaxConns        int    `yaml:"max_conns"`
	MinConns        int    `yaml:"min_conns"`
	MaxConnLifetime string `yaml:"max_conn_lifetime"`
	MaxConnIdleTime string `yaml:"max_conn_idle_time"`
}

// DatabaseURL returns a pgx-compatible connection string.
func (d DatabaseConfig) DatabaseURL() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(d.User, d.Password),
		Host:   fmt.Sprintf("%s:%d", d.Host, d.Port),
		Path:   d.Database,
	}
	q := u.Query()
	q.Set("sslmode", d.SSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}

// LogConfig defines logging behavior.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// TLSConfig defines TLS certificate paths.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	CAFile   string `yaml:"ca_file"`
}
