package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
	"unicode/utf8"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/client"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// submitSafetyMargin is reserved out of the audit lease so the SubmitAuditResult RPC
// still has time to land before the head reclaims the job. The execution context
// deadline is the lease expiry minus this margin.
const submitSafetyMargin = 30 * time.Second

// maxErrorMessageBytes bounds the error_message reported to the head on an execution
// failure (~1 KB, spec §7.8) — enough for a useful diagnostic without shipping an
// unbounded stack/stderr dump over the wire.
const maxErrorMessageBytes = 1024

// Per-job outcome labels for the one-line structured log emitted per audit job.
const (
	outcomeSubmitted       = "submitted"        // output bytes accepted by the head
	outcomeExecutionFailed = "execution-failed" // reported execution_failed to the head
	outcomeAlreadySettled  = "already-settled"  // FailedPrecondition: job completed/reclaimed
	outcomeSubmitFailed    = "submit-failed"    // submit exhausted retries; reclaim will cover it
	outcomeLeaseSkipped    = "lease-skipped"    // lease unusable; skipped, reclaim will requeue
)

// auditSubmitInitialBackoff is the first inter-retry delay for a failed (non-terminal)
// SubmitAuditResult. It is a var, not a const, only so tests can shrink it.
var auditSubmitInitialBackoff = 500 * time.Millisecond

// auditClient is the narrow consumer-side view of the two AuditService RPCs the runner
// loop needs. Defining it here (rather than depending on the concrete *client.Client)
// keeps the loop unit-testable against a fake. *client.Client satisfies it.
type auditClient interface {
	ClaimAuditJob(ctx context.Context, req *lettucev1.ClaimAuditJobRequest) (*lettucev1.ClaimAuditJobResponse, error)
	SubmitAuditResult(ctx context.Context, req *lettucev1.SubmitAuditResultRequest) (*lettucev1.SubmitAuditResultResponse, error)
}

func newAuditRunnerCmd() *cobra.Command {
	var (
		once         bool
		pollInterval time.Duration
	)

	cmd := &cobra.Command{
		Use:   "audit-runner",
		Short: "Run re-execution audit jobs for a head this account is a trusted runner for",
		Long: `Claim and re-execute result-audit jobs from a head that has registered this
account as a trusted runner.

The head samples a fraction of validated work units for post-hoc corroboration and
queues them as audit jobs. This command claims those jobs one at a time, re-executes
each unit with the same runtimes a normal volunteer uses, and returns the raw output
bytes. The head hashes the bytes and computes the verdict itself — this runner never
adjudicates and never sees the accepted output it is compared against.

It is authenticated with the same identity key as the volunteer daemon and only works
against a head where the operator has registered this account as an active trusted
runner. It never touches the daemon's work buffer, checkpoints, or normal work RPCs.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuditRunner(cmd, once, pollInterval)
		},
	}

	cmd.Flags().BoolVar(&once, "once", false, "run until the audit queue is empty, then exit 0")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", 30*time.Second, "wait between polls when no job is available")

	return cmd
}

func runAuditRunner(cmd *cobra.Command, once bool, pollInterval time.Duration) error {
	if pollInterval <= 0 {
		return fmt.Errorf("--poll-interval must be positive")
	}

	// Same PB-30 invariant as the daemon: the sandbox dirs created under the data
	// dir are shielded from other local users only by its 0o700 mode.
	dataDirTightened, err := daemon.EnsureDataDirPrivate(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("securing data directory: %w", err)
	}

	// Load the identity keypair — the same account key the daemon authenticates with.
	pub, priv, err := identity.LoadKeyPair(cfg.KeyFilePath(), cfg.PubKeyFilePath())
	if err != nil {
		if identity.KeyPairExists(cfg.KeyFilePath(), cfg.PubKeyFilePath()) {
			return fmt.Errorf("loading identity: %w\n%s", err, identity.LoadFailureRemedy(err, cfg.KeyFilePath(), cfg.PubKeyFilePath()))
		}
		return fmt.Errorf("loading identity: %w (run 'lettuce-volunteer init' first)", err)
	}

	if len(cfg.Servers) == 0 {
		return fmt.Errorf("no servers configured. Run `lettuce-volunteer attach --server <host>` first")
	}

	logger, closeLogger := newLogger(cfg)
	defer closeLogger()

	pubFP := "unknown"
	if len(pub) >= 4 {
		pubFP = fmt.Sprintf("%x", pub[:4])
	}
	logger.Info("audit runner starting", "version", version, "pubkey_fp", pubFP, "once", once, "poll_interval", pollInterval)

	if dataDirTightened {
		logger.Warn("data directory had group/other access; tightened to 0700 — the sandbox dirs beneath it rely on this mode to keep other local users out (PB-30)",
			"data_dir", cfg.DataDir)
	}

	// WASM is always available (wazero is embedded); make sure the registry can serve
	// wasm-runtime audit jobs even if the config predates WASM.
	if !containsRuntime(cfg.AvailableRuntimes, "WASM") {
		cfg.AvailableRuntimes = append(cfg.AvailableRuntimes, "WASM")
	}

	// Build the runtime registry exactly as the daemon does, then replicate the
	// daemon's resource-limiter + process-group hookup so a re-executed leaf binary
	// runs under the same OS-level limits and child-process containment a normal work
	// unit would (NewDaemon does this internally; the audit-runner is not a daemon).
	registry, machineManager := buildRuntimeRegistry(cfg, logger)
	logger.Info("runtimes available", "advertised", advertisedRuntimes(registry))
	pg := wireRuntimeResourceLimits(registry, cfg, logger)
	if pg != nil {
		defer pg.Close()
	}
	// Same PB-27 ownership rule as the daemon: only undo a machine THIS process
	// started; one that was already running is left exactly as found.
	if machineManager != nil {
		defer stopMachineIfDaemonStarted(machineManager, logger)
	}

	// The audit-runner drives a single head: the first configured one. Entries
	// are one-per-head (config.Load merges legacy duplicates, PB-16).
	servers := cfg.Servers
	srv := servers[0]
	if len(servers) > 1 {
		logger.Warn("multiple heads configured; audit-runner uses the first", "server", srv.DisplayName())
	}

	grpcClient, err := client.ConnectWithRetry(cmd.Context(), client.ClientConfig{
		ServerURL:     srv.GRPCAddress,
		Insecure:      srv.Insecure,
		TLSCertFile:   srv.CACertPath,
		TLSClientCert: srv.CertPath,
		TLSClientKey:  srv.KeyPath,
		Identity:      &client.Identity{PublicKey: pub, PrivateKey: priv},
	}, client.RetryConfig{MaxRetries: 3}, logger)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", srv.DisplayName(), err)
	}
	defer grpcClient.Close()

	// Detect hardware once (os/cpu_arch/cpu_vendor are what the head keys the audit
	// hardware-class match on) — the same detection RequestWorkUnit uses.
	hardware := client.DetectHardware(cfg)

	// Cancel the loop on SIGINT/SIGTERM so an in-flight job finishes and the runner
	// exits cleanly rather than being killed mid-execution.
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logger.Info("received signal, finishing current audit job before exit", "signal", sig)
		fmt.Fprintf(os.Stderr, "\nReceived %s. Finishing current audit job before exiting...\n", sig)
		cancel()
	}()

	runner := &auditRunner{
		client:       grpcClient,
		registry:     registry,
		hardware:     hardware,
		logger:       logger,
		pollInterval: pollInterval,
		once:         once,
	}

	fmt.Printf("Audit runner started against %s (account %s).\n", srv.DisplayName(), pubFP)
	processed, runErr := runner.run(ctx)
	fmt.Printf("Audit runner stopped after processing %d job(s).\n", processed)

	// A clean context cancellation (Ctrl-C, or --once draining the queue) is not an
	// error; anything else is.
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	return nil
}

// wireRuntimeResourceLimits replicates NewDaemon's resource-limiter + process-group
// wiring for the audit-runner (which executes leaf binaries outside the daemon). It
// attaches the limiter/process-group hooks to the registry's NativeRuntime and returns
// the process group (nil if unavailable) for the caller to Close on shutdown.
func wireRuntimeResourceLimits(registry *daemon.RuntimeRegistry, cfg *config.Config, logger *slog.Logger) daemon.ProcessGroup {
	limiter := resource.NewLimiter(logger)
	limits := &cfg.ResourceLimits

	pg, pgErr := daemon.NewProcessGroup(logger)
	if pgErr != nil {
		logger.Warn("failed to create process group; child processes may outlive the runner", "error", pgErr)
		pg = nil
	}

	nr, ok := registry.GetRuntime("native").(*runtime.NativeRuntime)
	if !ok {
		return pg
	}
	perUnitLimits := func(declaredMemMB int) *config.ResourceLimits {
		l := *limits
		l.MaxMemoryMB = runtime.BookedMemMB(declaredMemMB, limits.MaxMemoryMB)
		return &l
	}
	nr.SetCommandModifier(func(cmd *exec.Cmd, declaredMemMB int) error {
		if pg != nil {
			pg.ConfigureCommand(cmd)
		}
		return limiter.Apply(cmd, perUnitLimits(declaredMemMB))
	})
	nr.SetProcessNotifier(func(pid int, declaredMemMB int) (func(), error) {
		if pg != nil {
			if err := pg.Add(pid); err != nil {
				logger.Warn("failed to add process to group", "pid", pid, "error", err)
			}
		}
		return limiter.Enforce(pid, perUnitLimits(declaredMemMB))
	})
	return pg
}

// auditRunner drives the claim/execute/submit loop for one head.
type auditRunner struct {
	client       auditClient
	registry     *daemon.RuntimeRegistry
	hardware     *lettucev1.HardwareCapabilities
	logger       *slog.Logger
	pollInterval time.Duration
	once         bool

	// clock is injectable for tests; nil means time.Now.
	clock func() time.Time
}

func (r *auditRunner) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// run claims and processes audit jobs until the context is cancelled or, in --once
// mode, until the head reports an empty queue. It returns the number of jobs processed.
func (r *auditRunner) run(ctx context.Context) (int, error) {
	processed := 0
	for {
		if err := ctx.Err(); err != nil {
			return processed, err
		}

		resp, err := r.client.ClaimAuditJob(ctx, &lettucev1.ClaimAuditJobRequest{Hardware: r.hardware})
		if err != nil {
			// Not an active registered runner — retrying won't help; surface it.
			if status.Code(err) == codes.PermissionDenied {
				return processed, fmt.Errorf("not an active trusted runner on this head (ask the operator to register this account): %w", err)
			}
			if ctx.Err() != nil {
				return processed, ctx.Err()
			}
			// Transient claim failure: back off and retry (a claim error is not an
			// empty queue, so --once keeps trying until it drains or is cancelled).
			r.logger.Warn("claim audit job failed; backing off", "error", err)
			if !sleepCtx(ctx, r.pollInterval) {
				return processed, ctx.Err()
			}
			continue
		}

		job := resp.GetJob()
		if job == nil {
			if r.once {
				r.logger.Info("audit queue empty; exiting (--once)", "processed", processed)
				return processed, nil
			}
			if !sleepCtx(ctx, r.pollInterval) {
				return processed, ctx.Err()
			}
			continue
		}

		r.processJob(ctx, job)
		processed++
	}
}

// processJob re-executes one audit job and submits the outcome. It never returns an
// error: a per-job failure is reported to the head (execution_failed) or absorbed by
// the reclaim machinery, and the loop moves on.
func (r *auditRunner) processJob(ctx context.Context, job *lettucev1.AuditJob) {
	start := r.now()
	wu := runtime.WorkUnitFromProto(job.GetAssignment())

	logFields := []any{"audit_id", job.GetAuditId()}
	if wu != nil {
		logFields = append(logFields, "unit_id", wu.ID, "runtime", wu.Runtime)
	}

	deadline, ok := auditExecDeadline(job.GetLeaseExpiresUnix(), r.now())
	if !ok {
		// No usable execution window before the lease lapses — don't run something we
		// can't report in time. Leave it CLAIMED; the head's reclaim sweep requeues it.
		r.logger.Warn("audit job lease unusable; skipping (reclaim will requeue)",
			append(logFields, "lease_expires_unix", job.GetLeaseExpiresUnix())...)
		r.logOutcome(logFields, outcomeLeaseSkipped, r.now().Sub(start))
		return
	}

	execCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var req *lettucev1.SubmitAuditResultRequest
	result, execErr := runAuditExecution(execCtx, r.registry, wu, r.logger)
	if execErr != nil {
		r.logger.Warn("audit execution failed", append(logFields, "error", execErr)...)
		req = &lettucev1.SubmitAuditResultRequest{
			AuditId:         job.GetAuditId(),
			ExecutionFailed: true,
			ErrorMessage:    truncateForWire(execErr.Error(), maxErrorMessageBytes),
		}
	} else {
		req = &lettucev1.SubmitAuditResultRequest{
			AuditId:    job.GetAuditId(),
			OutputData: result.OutputData,
		}
	}

	// Submit on the parent ctx (not execCtx): the safety margin we reserved out of the
	// lease is exactly the window for this RPC, so a job that ran to the exec deadline
	// still reports its result.
	outcome := submitAuditResultWithRetry(ctx, r.client, req, r.logger)
	r.logOutcome(logFields, outcome, r.now().Sub(start))
}

func (r *auditRunner) logOutcome(baseFields []any, outcome string, dur time.Duration) {
	fields := make([]any, 0, len(baseFields)+4)
	fields = append(fields, baseFields...)
	fields = append(fields, "outcome", outcome, "duration_ms", dur.Milliseconds())
	r.logger.Info("audit job processed", fields...)
}

// runAuditExecution selects the runtime for wu and runs Prepare/Execute/Cleanup,
// returning the execution result. Cleanup always runs. It mirrors the slot's
// execution sequence minus the daemon-only concerns (checkpoints, run-start,
// suspend/resume) an audit does not use.
func runAuditExecution(ctx context.Context, registry *daemon.RuntimeRegistry, wu *runtime.WorkUnit, logger *slog.Logger) (*runtime.ExecutionResult, error) {
	if wu == nil {
		return nil, fmt.Errorf("audit job carried no work unit assignment")
	}

	rt, err := registry.SelectRuntime(wu)
	if err != nil {
		return nil, fmt.Errorf("select runtime: %w", err)
	}

	prep, err := rt.Prepare(ctx, wu)
	if err != nil {
		if prep != nil {
			if cerr := rt.Cleanup(prep); cerr != nil {
				logger.Warn("audit cleanup after prepare failure failed", "unit_id", wu.ID, "error", cerr)
			}
		}
		return nil, fmt.Errorf("prepare: %w", err)
	}
	defer func() {
		if cerr := rt.Cleanup(prep); cerr != nil {
			logger.Warn("audit cleanup failed", "unit_id", wu.ID, "error", cerr)
		}
	}()

	result, err := rt.Execute(ctx, wu, prep)
	if err != nil {
		return nil, fmt.Errorf("execute: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("runtime returned no result")
	}
	return result, nil
}

// submitAuditResultWithRetry submits the audit result, returning the terminal outcome
// for the per-job log line. A FailedPrecondition means the job is no longer ours
// (completed or reclaimed) — treated as job-done and never retried (spec F-L2). Other
// errors are retried a couple of times with short backoff; if they persist the loss is
// left to the head's lease/reclaim machinery.
func submitAuditResultWithRetry(ctx context.Context, cl auditClient, req *lettucev1.SubmitAuditResultRequest, logger *slog.Logger) string {
	const maxAttempts = 3
	backoff := auditSubmitInitialBackoff

	for attempt := 1; ; attempt++ {
		_, err := cl.SubmitAuditResult(ctx, req)
		if err == nil {
			if req.GetExecutionFailed() {
				return outcomeExecutionFailed
			}
			return outcomeSubmitted
		}
		if status.Code(err) == codes.FailedPrecondition {
			logger.Info("audit job already settled (completed or reclaimed)", "audit_id", req.GetAuditId())
			return outcomeAlreadySettled
		}
		if attempt >= maxAttempts {
			logger.Warn("audit submit failed after retries; leaving to reclaim",
				"audit_id", req.GetAuditId(), "attempts", attempt, "error", err)
			return outcomeSubmitFailed
		}
		logger.Warn("audit submit failed; retrying", "audit_id", req.GetAuditId(), "attempt", attempt, "error", err)
		if !sleepCtx(ctx, backoff) {
			return outcomeSubmitFailed
		}
		backoff *= 2
	}
}

// auditExecDeadline derives the execution-context deadline from an audit lease expiry,
// reserving submitSafetyMargin for the submit RPC. It returns ok=false when the lease
// is unset or already leaves no usable execution window, so the caller skips the job
// rather than run with an already-expired context.
func auditExecDeadline(leaseExpiresUnix int64, now time.Time) (time.Time, bool) {
	if leaseExpiresUnix <= 0 {
		return time.Time{}, false
	}
	deadline := time.Unix(leaseExpiresUnix, 0).Add(-submitSafetyMargin)
	if !deadline.After(now) {
		return time.Time{}, false
	}
	return deadline, true
}

// sleepCtx waits for d or until ctx is cancelled. It returns true if the full duration
// elapsed, false if the context was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// truncateForWire caps s at maxBytes, backing off to a valid UTF-8 boundary so the
// truncated string stays well-formed.
func truncateForWire(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	b := []byte(s)[:maxBytes]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
