package config

import (
	"fmt"
	"math"
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

	// --- Automatic standing backpressure (BG-24/BG-24b PR-B) ---
	//
	// The backpressure machine folds every adjudicated result (AGREED/DISAGREED — never
	// EXPIRED/ABANDONED) into a decayed per-volunteer rejection-rate signal (7-day
	// half-life) and drives AUTO-owned standing transitions with hysteresis:
	// OK -> PROBATION at StandingProbationRate, PROBATION -> OK at StandingOKRate,
	// PROBATION -> BENCHED (for StandingBenchMinutes) at StandingBenchRate, all
	// evaluated only once the decayed sample reaches StandingMinSample. OPERATOR-set
	// rows are never auto-changed. Effective rates must order
	// 0 < ok < probation <= bench <= 1.

	// StandingBackpressureEnabled is the master switch. OFF by default: no signal is
	// recorded and standing changes remain operator-only (the legacy lifetime-rate WARN
	// stays). Override via LETTUCE_HEAD_STANDING_BACKPRESSURE_ENABLED.
	StandingBackpressureEnabled bool `yaml:"standing_backpressure_enabled"`
	// StandingProbationRate is the decayed rejection rate at which an OK volunteer
	// enters PROBATION. Unset (0) resolves to the default 0.50. Override via
	// LETTUCE_HEAD_STANDING_PROBATION_RATE.
	StandingProbationRate float64 `yaml:"standing_probation_rate"`
	// StandingOKRate is the decayed rejection rate at or below which a PROBATION
	// volunteer returns to OK — the hysteresis exit, strictly below the entry rate.
	// Unset (0) resolves to the default 0.25. Override via LETTUCE_HEAD_STANDING_OK_RATE.
	StandingOKRate float64 `yaml:"standing_ok_rate"`
	// StandingBenchRate is the decayed rejection rate at which a PROBATION volunteer is
	// BENCHED. Unset (0) resolves to the default 0.75. Override via
	// LETTUCE_HEAD_STANDING_BENCH_RATE.
	StandingBenchRate float64 `yaml:"standing_bench_rate"`
	// StandingMinSample is the minimum decayed sample (good + bad adjudications) at
	// which transitions are evaluated at all, so a newcomer's first unlucky results
	// cannot bench them. Unset (0) resolves to the default 5. Override via
	// LETTUCE_HEAD_STANDING_MIN_SAMPLE.
	StandingMinSample int `yaml:"standing_min_sample"`
	// StandingBenchMinutes is the auto-bench duration; an expired bench resolves to
	// PROBATION, so re-entry to OK goes through the hysteresis exit. Unset (0) resolves
	// to the default 1440 (24h). Override via LETTUCE_HEAD_STANDING_BENCH_MINUTES.
	StandingBenchMinutes int `yaml:"standing_bench_minutes"`

	// --- Registration admission: per-IP creation cap (design §4.1) ---
	//
	// The cap bounds how many NEW volunteer rows a single IP bucket (IPv4 address /
	// IPv6 /64 prefix) may create per UTC day. It gates only the create branch of
	// registration — re-registration of an existing key never pays admission cost —
	// and the counter increments in the same transaction as the volunteer INSERT, so
	// the cap counts exactly the creations that committed. Admission cost is a
	// treadmill slower, never load-bearing.

	// RegistrationCapEnabled is the master switch. OFF by default: registration
	// behaves exactly as before. Override via LETTUCE_HEAD_REGISTRATION_CAP_ENABLED.
	RegistrationCapEnabled bool `yaml:"registration_cap_enabled"`
	// RegistrationCapPerIPPerDay is the maximum volunteer creations per (IP bucket,
	// UTC day). Generous by default: honest labs/universities behind one NAT share a
	// bucket. Unset (0) resolves to the default 10. Override via
	// LETTUCE_HEAD_REGISTRATION_CAP_PER_IP_PER_DAY.
	RegistrationCapPerIPPerDay int `yaml:"registration_cap_per_ip_per_day"`

	// --- Registration admission: proof-of-work (design §4.1) ---
	//
	// When enforced, a registration that would CREATE a new volunteer must carry a
	// valid solution to a server-issued challenge (find a nonce whose
	// SHA-256(challenge || public_key || nonce) has enough leading zero bits).
	// Re-registration of an existing key never pays. Challenge ISSUANCE is always
	// available so clients can be written probe-free; only enforcement is gated.
	// WARNING: do NOT enable before solver-capable clients ship — no current
	// volunteer CLI or dashboard build can solve a challenge, so enforcement would
	// block ALL new-volunteer onboarding (existing volunteers are unaffected).

	// RegistrationPowEnabled is the master switch for ENFORCEMENT. OFF by default.
	// Override via LETTUCE_HEAD_REGISTRATION_POW_ENABLED.
	RegistrationPowEnabled bool `yaml:"registration_pow_enabled"`
	// RegistrationPowDifficultyBits is the required leading zero bits of the solution
	// digest (~2^bits hash attempts; 20 ≈ one second of native single-thread work).
	// Unset (0) resolves to the default 20. Override via
	// LETTUCE_HEAD_REGISTRATION_POW_DIFFICULTY_BITS.
	RegistrationPowDifficultyBits int `yaml:"registration_pow_difficulty_bits"`
	// RegistrationPowChallengeTTLSeconds is how long an issued challenge stays
	// redeemable. Unset (0) resolves to the default 600 (10 minutes). Override via
	// LETTUCE_HEAD_REGISTRATION_POW_CHALLENGE_TTL_SECONDS.
	RegistrationPowChallengeTTLSeconds int `yaml:"registration_pow_challenge_ttl_seconds"`

	// --- BG-25: server-issued host identity — the per-account host cap ---
	//
	// The head mints per-machine host ids at registration (clients never generate
	// them) and hard-caps how many one account may hold; a machine past the cap still
	// works, sharing the per-account fallback bucket. A slot frees when an idle host
	// (unseen past the activity window) is evicted at mint time; a WORKING machine is
	// never evictable (the work path bumps its last-seen).

	// HostCapPerAccount is the hard bound on one account's TOTAL issued host ids.
	// ON BY DEFAULT (nil resolves to 10, per the BG-25 design — this shipped as part
	// of the issuance hard cutover, so secure-by-default is coherent). An explicit 0
	// disables the cap (unlimited hosts; issuance stays server-owned). Override via
	// LETTUCE_HEAD_HOST_CAP_PER_ACCOUNT.
	HostCapPerAccount *int `yaml:"host_cap_per_account"`
	// HostCapActiveDays is the staleness window in DAYS: a host unseen this long is
	// evictable at mint time when the account is at cap. Unset (0) resolves to the
	// default 30. Override via LETTUCE_HEAD_HOST_CAP_ACTIVE_DAYS.
	HostCapActiveDays int `yaml:"host_cap_active_days"`

	// --- Credit settlement: maturation, export kill switch, emission caps ---
	//
	// The settlement layer makes fraudulent credit unwindable before an external
	// consumer can treat it as money: credit younger than the maturation window is
	// excluded from the public export, clawbacks (compensating negative entries in
	// credit_adjustments) net against it per-entry, a per-account daily cap bounds
	// the burst rate any account can mint, and an anomaly check freezes the export
	// when today's global grant rate far exceeds the trailing norm. Every knob
	// defaults to current behavior.

	// CreditMaturationDays is the maturation window in DAYS. 0 (the default)
	// disables maturation: the public fleet feed serves raw lifetime sums exactly
	// as before. > 0: the feed serves per-entry adjustment-net sums over entries at
	// least this many days old. Override via LETTUCE_HEAD_CREDIT_MATURATION_DAYS.
	CreditMaturationDays int `yaml:"credit_maturation_days"`
	// StatsExportEnabled is the export kill switch for the public credit-stats
	// feeds (the fleet feed and the per-volunteer public stats). ON BY DEFAULT
	// (nil resolves to true): set false to freeze the export during an incident —
	// the endpoints answer 503 so a consumer halts payouts instead of ingesting
	// numbers under investigation. Override via LETTUCE_HEAD_STATS_EXPORT_ENABLED.
	StatsExportEnabled *bool `yaml:"stats_export_enabled"`
	// MaxDailyCreditPerAccount caps one account's granted credit over a rolling 24h
	// window (DB clock). 0 (the default) = unlimited. A grant that would exceed the
	// cap is SUPPRESSED (no ledger entry, no RAC; the result stays AGREED and its
	// attestation records credit 0) — the cap bounds emission, not merit. Override
	// via LETTUCE_HEAD_MAX_DAILY_CREDIT_PER_ACCOUNT.
	MaxDailyCreditPerAccount float64 `yaml:"max_daily_credit_per_account"`
	// EmissionAnomalyHaltEnabled arms the global anomaly circuit breaker: the
	// public export self-halts (503) when the last 24h's total granted credit
	// exceeds EmissionAnomalyFactor times the trailing 30-day daily average
	// (SUM over [now()-31d, now()-1d) / 30, armed only once that window has grants
	// on >= 7 distinct days, so a young or sparse head cannot trip on noise). OFF
	// by default. Override via LETTUCE_HEAD_EMISSION_ANOMALY_HALT_ENABLED.
	EmissionAnomalyHaltEnabled bool `yaml:"emission_anomaly_halt_enabled"`
	// EmissionAnomalyFactor is the anomaly multiple. Unset (0) resolves to the
	// default 3.0; the effective value must be > 1 when the halt is enabled (at or
	// below 1 any busier-than-average day would freeze the export). Override via
	// LETTUCE_HEAD_EMISSION_ANOMALY_FACTOR.
	EmissionAnomalyFactor float64 `yaml:"emission_anomaly_factor"`

	// ResultAuditEnabled arms post-hoc result audits: after a work unit VALIDATES,
	// it is sampled (crypto/rand) for re-execution by a registered trusted runner,
	// and the head adjudicates the returned bytes against the accepted output.
	// OFF by default — no sampling, no audit rows. Verdicts in this phase are
	// recorded + logged only (no slash, no clawback). Override via
	// LETTUCE_HEAD_RESULT_AUDIT_ENABLED.
	ResultAuditEnabled bool `yaml:"result_audit_enabled"`
	// ResultAuditRate is the head-default sampling fraction in (0, 1]. Unset (0)
	// resolves to the default 0.01 (1 in 100 validated units). A leaf's
	// validation_config.audit_rate can only RAISE the effective rate above this
	// (max-overlay): leaf creation is self-service and the leaf owner is the
	// primary adversary, so the per-leaf knob must never lower sampling. Override
	// via LETTUCE_HEAD_RESULT_AUDIT_RATE.
	ResultAuditRate float64 `yaml:"result_audit_rate"`

	// AuditEnforcementEnabled arms the consequences on a confirmed audit MISMATCH:
	// slash every agreeing trust subject, claw back the unit's credit plus all
	// unmatured credit of those accounts (with revocation attestations), and
	// retroactively repair honest dissenters (design doc §9). OFF by default —
	// verdicts stay observe-only exactly as slice 2 shipped them. Enforcement acts
	// only on verdicts recorded while this knob was on (stamped per row), and only
	// after a SECOND registered runner independently confirms the mismatch.
	// Requires CreditMaturationDays > 9 (Validate — the enforcement horizon must
	// land inside the maturation window). Override via
	// LETTUCE_HEAD_AUDIT_ENFORCEMENT_ENABLED.
	AuditEnforcementEnabled bool `yaml:"audit_enforcement_enabled"`

	// ContentFetchEnabled arms external-output fetch-and-verify (design doc §10;
	// BG-02b): a result submitted as an external reference (output_data_url) is held
	// out of validation while the head fetches the URL and hashes the served bytes
	// itself; only the head-computed hash may ever be a comparison key. OFF by
	// default — and with it off the front door is closed too: SubmitResult refuses
	// every external-reference submission outright, for opted-in leaves as well.
	// Override via LETTUCE_HEAD_CONTENT_FETCH_ENABLED.
	ContentFetchEnabled bool `yaml:"content_fetch_enabled"`
	// ContentFetchMaxBytes is the global ceiling on how many bytes one external
	// output fetch will read, in bytes. Unset (0) resolves to the default 104857600
	// (100 MB — matching the leaf-side max_output_size_bytes default, so the knob is
	// a pure operator ceiling, not a silent behavior change). The effective per-fetch
	// cap is min(leaf max_output_size_bytes, this) — see
	// EffectiveContentFetchMaxBytes. Override via
	// LETTUCE_HEAD_CONTENT_FETCH_MAX_BYTES.
	ContentFetchMaxBytes int64 `yaml:"content_fetch_max_bytes"`

	// --- Finalization recovery sweep (E1 §4.2/§7.2) ---
	//
	// The recovery sweep is a leader-gated, UNCONDITIONAL reconciler (no enable knob, matching
	// the revocation-reconciler precedent) that re-drives finalization-stalled work units
	// through the idempotent transitioner — the standing re-scan half of finalization liveness.
	// A strand is invisible for at most grace + interval before the sweep re-drives it.

	// FinalizationSweepIntervalSeconds is the sweep ticker cadence. Unset (0) resolves to the
	// default 60. Override via LETTUCE_HEAD_FINALIZATION_SWEEP_INTERVAL_SECONDS.
	FinalizationSweepIntervalSeconds int `yaml:"finalization_sweep_interval_seconds"`
	// FinalizationSweepGraceSeconds is the minimum age before a COMPLETED / REJECTED /
	// QUEUED-at-quorum unit is re-driven — headroom for the natural in-flight Evaluate to land
	// first. Unset (0) resolves to the default 300. Override via
	// LETTUCE_HEAD_FINALIZATION_SWEEP_GRACE_SECONDS.
	FinalizationSweepGraceSeconds int `yaml:"finalization_sweep_grace_seconds"`
	// FinalizationSweepBatch is the maximum units re-driven per tick (oldest first). Unset (0)
	// resolves to the default 100. Override via LETTUCE_HEAD_FINALIZATION_SWEEP_BATCH.
	FinalizationSweepBatch int `yaml:"finalization_sweep_batch"`
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

	// --- Automatic standing-backpressure defaults (operator-accepted 2026-07-04) ---
	// Enter probation at a 50% decayed rejection rate, exit at 25% (the hysteresis
	// band), bench at 75% for 24 hours, and evaluate transitions only once at least
	// 5 adjudications of decayed weight exist.
	defaultStandingProbationRate = 0.50
	defaultStandingOKRate        = 0.25
	defaultStandingBenchRate     = 0.75
	defaultStandingMinSample     = 5
	defaultStandingBenchMinutes  = 1440

	// --- Registration-admission defaults (operator-accepted 2026-07-04) ---
	// defaultRegistrationCapPerIPPerDay is deliberately generous: the cap slows
	// identity minting, and honest volunteers behind one NAT share a bucket.
	defaultRegistrationCapPerIPPerDay = 10
	// defaultRegistrationPowDifficultyBits: ~2^20 ≈ 1M hash attempts, about a second
	// of native single-thread work — the one-time cost the design intends.
	defaultRegistrationPowDifficultyBits = 20
	// defaultRegistrationPowChallengeTTLSeconds matches the identity-challenge expiry.
	defaultRegistrationPowChallengeTTLSeconds = 600
	// Validate bounds for the difficulty: below 8 bits the puzzle is free (no
	// treadmill slowing at all); above 32 bits the expected client work stretches to
	// minutes-to-hours — an operator foot-gun, not a tunable.
	minRegistrationPowDifficultyBits = 8
	maxRegistrationPowDifficultyBits = 32
	// Validate floor for the challenge TTL: a slow browser or a loaded machine must
	// still be able to solve-and-submit within the window.
	minRegistrationPowChallengeTTLSeconds = 60

	// --- BG-25 host-cap defaults (operator-accepted 2026-07-09) ---
	// defaultHostCapPerAccount: design §4.6's "cap hosts per account (default ~10)".
	// Bounds one account's concurrent per-machine quota buckets at cap (+1 shared
	// fallback bucket) while never blocking the machines themselves.
	defaultHostCapPerAccount = 10
	// defaultHostCapActiveDays: a host unseen this long is evictable at mint time.
	// Working machines bump last-seen on the work path, so only genuinely idle
	// machines ever age out — and only when a new machine actually needs the slot.
	defaultHostCapActiveDays = 30

	// --- Credit settlement defaults ---
	// defaultStatsExportEnabled: the public export serves by default; the kill
	// switch exists to be flipped OFF in an incident.
	defaultStatsExportEnabled = true
	// defaultEmissionAnomalyFactor: today's grants must exceed 3x the trailing
	// 30-day daily average before the export freezes itself.
	defaultEmissionAnomalyFactor = 3.0
	// defaultResultAuditRate: 1 in 100 validated units is re-executed when result
	// audits are enabled — the sampling economics baseline (one trusted machine per
	// ~100 volunteer machines).
	defaultResultAuditRate = 0.01
	// defaultContentFetchMaxBytes: the global external-output fetch cap, 100 MB —
	// deliberately equal to the leaf-side max_output_size_bytes default (design doc
	// §10.9, S6).
	defaultContentFetchMaxBytes = int64(104857600)

	// --- Finalization recovery-sweep defaults (E1 §4.2/§7.2) ---
	// defaultFinalizationSweepIntervalSeconds: re-scan every minute on the leader.
	defaultFinalizationSweepIntervalSeconds = 60
	// defaultFinalizationSweepGraceSeconds: a unit must sit stalled for 5 minutes before the
	// sweep re-drives it, leaving headroom for the natural in-flight Evaluate to land first.
	defaultFinalizationSweepGraceSeconds = 300
	// defaultFinalizationSweepBatch: re-drive up to 100 units per tick (oldest first) — drains
	// any plausible alpha backlog in a single tick.
	defaultFinalizationSweepBatch = 100

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

	// --- Automatic standing-backpressure validation ---
	// The three rates, the min-sample, and the bench duration reject only NEGATIVE raw
	// values; a raw 0 means "unset -> use the (positive) default", mirroring the trust
	// and DID knobs, so a minimal config that omits them stays valid and their EFFECTIVE
	// values are always ordered. A rate is a probability, so a set (positive) rate above
	// 1 can never be valid and is rejected as well.
	if h.StandingProbationRate < 0 {
		return fmt.Errorf("head.standing_probation_rate must be >= 0, got %v", h.StandingProbationRate)
	}
	if h.StandingProbationRate > 1 {
		return fmt.Errorf("head.standing_probation_rate must be in (0, 1], got %v", h.StandingProbationRate)
	}
	if h.StandingOKRate < 0 {
		return fmt.Errorf("head.standing_ok_rate must be >= 0, got %v", h.StandingOKRate)
	}
	if h.StandingOKRate > 1 {
		return fmt.Errorf("head.standing_ok_rate must be in (0, 1], got %v", h.StandingOKRate)
	}
	if h.StandingBenchRate < 0 {
		return fmt.Errorf("head.standing_bench_rate must be >= 0, got %v", h.StandingBenchRate)
	}
	if h.StandingBenchRate > 1 {
		return fmt.Errorf("head.standing_bench_rate must be in (0, 1], got %v", h.StandingBenchRate)
	}
	if h.StandingMinSample < 0 {
		return fmt.Errorf("head.standing_min_sample must be >= 0, got %d", h.StandingMinSample)
	}
	if h.StandingBenchMinutes < 0 {
		return fmt.Errorf("head.standing_bench_minutes must be >= 0, got %d", h.StandingBenchMinutes)
	}
	// The AUTO standing machine needs the EFFECTIVE thresholds to form a strictly
	// ascending hysteresis band bounded by 1: the OK (exit) rate strictly below the
	// PROBATION (entry) rate, which is at or below the BENCH rate, all within (0, 1].
	// This is checked on the effective (defaulted) values so a partial override that
	// inverts the band against a default — e.g. only standing_ok_rate set above the
	// default probation rate — is rejected rather than silently producing an
	// unorderable machine. The defaults 0.25/0.50/0.75 satisfy it.
	okRate := h.EffectiveStandingOKRate()
	probationRate := h.EffectiveStandingProbationRate()
	benchRate := h.EffectiveStandingBenchRate()
	if !(0 < okRate && okRate < probationRate && probationRate <= benchRate && benchRate <= 1) {
		return fmt.Errorf("head standing rates must satisfy 0 < ok_rate (%v) < probation_rate (%v) <= bench_rate (%v) <= 1 (effective values)",
			okRate, probationRate, benchRate)
	}
	if h.RegistrationCapPerIPPerDay < 0 {
		return fmt.Errorf("head.registration_cap_per_ip_per_day must be >= 0 (0 = default), got %d", h.RegistrationCapPerIPPerDay)
	}
	if h.RegistrationPowDifficultyBits < 0 {
		return fmt.Errorf("head.registration_pow_difficulty_bits must be >= 0 (0 = default), got %d", h.RegistrationPowDifficultyBits)
	}
	if h.RegistrationPowChallengeTTLSeconds < 0 {
		return fmt.Errorf("head.registration_pow_challenge_ttl_seconds must be >= 0 (0 = default), got %d", h.RegistrationPowChallengeTTLSeconds)
	}
	// Checked on the EFFECTIVE values (the standing-band precedent) so an explicit
	// out-of-band override fails boot instead of silently shipping a free or
	// hours-long puzzle.
	if bits := h.EffectiveRegistrationPowDifficultyBits(); bits < minRegistrationPowDifficultyBits || bits > maxRegistrationPowDifficultyBits {
		return fmt.Errorf("head.registration_pow_difficulty_bits must be in [%d, %d] (effective value, got %d)",
			minRegistrationPowDifficultyBits, maxRegistrationPowDifficultyBits, bits)
	}
	if ttl := h.EffectiveRegistrationPowChallengeTTLSeconds(); ttl < minRegistrationPowChallengeTTLSeconds {
		return fmt.Errorf("head.registration_pow_challenge_ttl_seconds must be >= %d (effective value, got %d)",
			minRegistrationPowChallengeTTLSeconds, ttl)
	}
	if h.HostCapPerAccount != nil && *h.HostCapPerAccount < 0 {
		return fmt.Errorf("head.host_cap_per_account must be >= 0 (0 = unlimited, unset = default %d), got %d",
			defaultHostCapPerAccount, *h.HostCapPerAccount)
	}
	if h.HostCapActiveDays < 0 {
		return fmt.Errorf("head.host_cap_active_days must be >= 0 (0 = default %d), got %d",
			defaultHostCapActiveDays, h.HostCapActiveDays)
	}
	if h.CreditMaturationDays < 0 {
		return fmt.Errorf("head.credit_maturation_days must be >= 0 (0 = disabled), got %d", h.CreditMaturationDays)
	}
	if h.MaxDailyCreditPerAccount < 0 || math.IsNaN(h.MaxDailyCreditPerAccount) || math.IsInf(h.MaxDailyCreditPerAccount, 0) {
		return fmt.Errorf("head.max_daily_credit_per_account must be a finite value >= 0 (0 = unlimited), got %v", h.MaxDailyCreditPerAccount)
	}
	if h.EmissionAnomalyFactor < 0 || math.IsNaN(h.EmissionAnomalyFactor) || math.IsInf(h.EmissionAnomalyFactor, 0) {
		return fmt.Errorf("head.emission_anomaly_factor must be a finite value >= 0 (0 = default %v), got %v",
			defaultEmissionAnomalyFactor, h.EmissionAnomalyFactor)
	}
	// Checked on the EFFECTIVE value (the standing-band precedent): a factor at or
	// below 1 would freeze the export on any busier-than-average day — a foot-gun,
	// not a tunable. Only enforced when the halt is armed at all.
	if h.EmissionAnomalyHaltEnabled {
		if f := h.EffectiveEmissionAnomalyFactor(); f <= 1 {
			return fmt.Errorf("head.emission_anomaly_factor must be > 1 when the anomaly halt is enabled (effective value, got %v)", f)
		}
	}
	if h.ResultAuditRate < 0 || h.ResultAuditRate > 1 || math.IsNaN(h.ResultAuditRate) {
		return fmt.Errorf("head.result_audit_rate must be a fraction in [0, 1] (0 = default %v), got %v",
			defaultResultAuditRate, h.ResultAuditRate)
	}
	// Hard cross-check (design doc §9.9, F-M9 strengthened by audit H2): enforcement
	// claws back credit, so the worst-case enforcement horizon — root audit (3d queue +
	// lease retries) plus up to 3 second-runner confirmation cycles (1d queue + lease
	// retries each) — must land INSIDE the maturation window, or fraud credit matures
	// and exports before the clawback can fire.
	if h.AuditEnforcementEnabled && h.CreditMaturationDays <= 9 {
		return fmt.Errorf("head.credit_maturation_days must be > 9 when audit enforcement is enabled "+
			"(worst-case enforcement horizon: 3d root queue + lease retries, plus up to 3 confirmation "+
			"cycles of 1d queue + lease retries each), got %d", h.CreditMaturationDays)
	}
	if h.ContentFetchMaxBytes < 0 {
		return fmt.Errorf("head.content_fetch_max_bytes must be >= 0 (0 = default %d), got %d",
			defaultContentFetchMaxBytes, h.ContentFetchMaxBytes)
	}
	// Finalization recovery-sweep knobs reject only NEGATIVE raw values; a raw 0 means
	// "unset -> use the (positive) default", mirroring the other worker-cadence knobs, so a
	// minimal config that omits them stays valid.
	if h.FinalizationSweepIntervalSeconds < 0 {
		return fmt.Errorf("head.finalization_sweep_interval_seconds must be >= 0 (0 = default %d), got %d",
			defaultFinalizationSweepIntervalSeconds, h.FinalizationSweepIntervalSeconds)
	}
	if h.FinalizationSweepGraceSeconds < 0 {
		return fmt.Errorf("head.finalization_sweep_grace_seconds must be >= 0 (0 = default %d), got %d",
			defaultFinalizationSweepGraceSeconds, h.FinalizationSweepGraceSeconds)
	}
	if h.FinalizationSweepBatch < 0 {
		return fmt.Errorf("head.finalization_sweep_batch must be >= 0 (0 = default %d), got %d",
			defaultFinalizationSweepBatch, h.FinalizationSweepBatch)
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

// EffectiveStandingProbationRate returns the decayed rejection rate at which an OK
// volunteer enters PROBATION, default 0.50. Unset (<= 0) -> defaultStandingProbationRate.
func (h HeadConfig) EffectiveStandingProbationRate() float64 {
	if h.StandingProbationRate <= 0 {
		return defaultStandingProbationRate
	}
	return h.StandingProbationRate
}

// EffectiveStandingOKRate returns the decayed rejection rate at or below which a
// PROBATION volunteer returns to OK, default 0.25. Unset (<= 0) -> defaultStandingOKRate.
func (h HeadConfig) EffectiveStandingOKRate() float64 {
	if h.StandingOKRate <= 0 {
		return defaultStandingOKRate
	}
	return h.StandingOKRate
}

// EffectiveStandingBenchRate returns the decayed rejection rate at which a PROBATION
// volunteer is BENCHED, default 0.75. Unset (<= 0) -> defaultStandingBenchRate.
func (h HeadConfig) EffectiveStandingBenchRate() float64 {
	if h.StandingBenchRate <= 0 {
		return defaultStandingBenchRate
	}
	return h.StandingBenchRate
}

// EffectiveStandingMinSample returns the minimum decayed sample at which standing
// transitions are evaluated, default 5. Unset (<= 0) -> defaultStandingMinSample.
func (h HeadConfig) EffectiveStandingMinSample() int {
	if h.StandingMinSample <= 0 {
		return defaultStandingMinSample
	}
	return h.StandingMinSample
}

// EffectiveStandingBenchMinutes returns the auto-bench duration in minutes, default
// 1440 (24h). Unset (<= 0) -> defaultStandingBenchMinutes.
func (h HeadConfig) EffectiveStandingBenchMinutes() int {
	if h.StandingBenchMinutes <= 0 {
		return defaultStandingBenchMinutes
	}
	return h.StandingBenchMinutes
}

// EffectiveRegistrationCapPerIPPerDay returns the per-(IP bucket, UTC day) volunteer
// creation cap, default 10. Unset (<= 0) -> defaultRegistrationCapPerIPPerDay.
func (h HeadConfig) EffectiveRegistrationCapPerIPPerDay() int {
	if h.RegistrationCapPerIPPerDay <= 0 {
		return defaultRegistrationCapPerIPPerDay
	}
	return h.RegistrationCapPerIPPerDay
}

// EffectiveRegistrationPowDifficultyBits returns the required leading zero bits of a
// registration proof-of-work solution, default 20. Unset (<= 0) -> the default.
func (h HeadConfig) EffectiveRegistrationPowDifficultyBits() int {
	if h.RegistrationPowDifficultyBits <= 0 {
		return defaultRegistrationPowDifficultyBits
	}
	return h.RegistrationPowDifficultyBits
}

// EffectiveRegistrationPowChallengeTTLSeconds returns how long an issued registration
// challenge stays redeemable, default 600 (10 minutes). Unset (<= 0) -> the default.
func (h HeadConfig) EffectiveRegistrationPowChallengeTTLSeconds() int {
	if h.RegistrationPowChallengeTTLSeconds <= 0 {
		return defaultRegistrationPowChallengeTTLSeconds
	}
	return h.RegistrationPowChallengeTTLSeconds
}

// EffectiveHostCapPerAccount returns the hard bound on one account's TOTAL issued host
// ids (BG-25). ON BY DEFAULT: nil (unset) -> defaultHostCapPerAccount (10); an explicit
// 0 disables the cap (unlimited hosts, issuance still server-owned).
func (h HeadConfig) EffectiveHostCapPerAccount() int {
	if h.HostCapPerAccount == nil {
		return defaultHostCapPerAccount
	}
	return *h.HostCapPerAccount
}

// EffectiveHostCapActiveDays returns the staleness window (days) after which an unseen
// host is evictable at mint time. Unset (<= 0) -> defaultHostCapActiveDays (30).
func (h HeadConfig) EffectiveHostCapActiveDays() int {
	if h.HostCapActiveDays <= 0 {
		return defaultHostCapActiveDays
	}
	return h.HostCapActiveDays
}

// EffectiveStatsExportEnabled reports whether the public credit-stats export serves.
// ON BY DEFAULT: nil (unset) -> defaultStatsExportEnabled (true); an explicit false is
// the incident kill switch (gated endpoints answer 503).
func (h HeadConfig) EffectiveStatsExportEnabled() bool {
	if h.StatsExportEnabled == nil {
		return defaultStatsExportEnabled
	}
	return *h.StatsExportEnabled
}

// EffectiveEmissionAnomalyFactor returns the anomaly multiple the export's circuit
// breaker compares today's grant total against. Unset (<= 0) ->
// defaultEmissionAnomalyFactor (3.0).
func (h HeadConfig) EffectiveEmissionAnomalyFactor() float64 {
	if h.EmissionAnomalyFactor <= 0 {
		return defaultEmissionAnomalyFactor
	}
	return h.EmissionAnomalyFactor
}

// EffectiveResultAuditRate returns the head-default post-hoc audit sampling fraction:
// the configured result_audit_rate, or 0.01 when unset (0). A leaf's audit_rate override
// can only raise the effective rate above this (max-overlay; see the sampling hook).
func (h HeadConfig) EffectiveResultAuditRate() float64 {
	if h.ResultAuditRate <= 0 {
		return defaultResultAuditRate
	}
	return h.ResultAuditRate
}

// EffectiveContentFetchMaxBytes returns the global external-output fetch byte
// ceiling: the configured content_fetch_max_bytes, or 100 MB when unset (0). The
// per-fetch cap is min(leaf max_output_size_bytes, this) — composed at the fetch
// site (design doc §10.5).
func (h HeadConfig) EffectiveContentFetchMaxBytes() int64 {
	if h.ContentFetchMaxBytes <= 0 {
		return defaultContentFetchMaxBytes
	}
	return h.ContentFetchMaxBytes
}

// EffectiveFinalizationSweepIntervalSeconds returns the recovery-sweep ticker cadence in
// seconds, default 60 (unset/0 -> default).
func (h HeadConfig) EffectiveFinalizationSweepIntervalSeconds() int {
	if h.FinalizationSweepIntervalSeconds <= 0 {
		return defaultFinalizationSweepIntervalSeconds
	}
	return h.FinalizationSweepIntervalSeconds
}

// EffectiveFinalizationSweepGraceSeconds returns the minimum age (seconds) before a stalled
// unit is re-driven, default 300 (unset/0 -> default).
func (h HeadConfig) EffectiveFinalizationSweepGraceSeconds() int {
	if h.FinalizationSweepGraceSeconds <= 0 {
		return defaultFinalizationSweepGraceSeconds
	}
	return h.FinalizationSweepGraceSeconds
}

// EffectiveFinalizationSweepBatch returns the max units re-driven per tick, default 100
// (unset/0 -> default).
func (h HeadConfig) EffectiveFinalizationSweepBatch() int {
	if h.FinalizationSweepBatch <= 0 {
		return defaultFinalizationSweepBatch
	}
	return h.FinalizationSweepBatch
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
