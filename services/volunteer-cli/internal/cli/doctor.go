package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/client"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose why this volunteer can or can't do work",
		Long: `Run preflight checks and print a pass/fail report: identity, disk space,
container runtime (actually pinging the socket), and for each attached head
whether it's reachable and how many of its leafs this volunteer can run.

Safe to run any time — it never changes anything and doesn't need the daemon.
Exits non-zero if a check that would block all work fails.`,
		RunE: runDoctor,
	}
}

// docLevel ranks a check outcome.
type docLevel int

const (
	docOK docLevel = iota
	docInfo
	docWarn
	docFail
)

func (l docLevel) tag() string {
	switch l {
	case docOK:
		return "ok  "
	case docInfo:
		return "info"
	case docWarn:
		return "warn"
	case docFail:
		return "fail"
	default:
		return "?   "
	}
}

// doctorReport accumulates check results and counts problems.
type doctorReport struct {
	w     io.Writer
	fails int
	warns int
}

func (r *doctorReport) add(level docLevel, name, detail, remedy string) {
	switch level {
	case docFail:
		r.fails++
	case docWarn:
		r.warns++
	}
	fmt.Fprintf(r.w, "  %s  %-13s %s\n", level.tag(), name, detail)
	if remedy != "" {
		fmt.Fprintf(r.w, "                       -> %s\n", remedy)
	}
}

func runDoctor(cmd *cobra.Command, args []string) error {
	// Quiet logger: doctor prints its own human-readable report; we don't want
	// connection JSON interleaved on stderr. Real errors are surfaced in the
	// check details instead.
	logger := slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	out := os.Stdout
	fmt.Fprintln(out, "lettuce-volunteer doctor")
	fmt.Fprintln(out)

	rep := &doctorReport{w: out}

	fmt.Fprintln(out, "Local:")
	checkAccountInfo(rep)
	checkDataDir(rep, cfg.DataDir)
	checkIdentity(rep, cfg.KeyFile, cfg.PubKeyFile)
	checkDisk(rep, cfg.DataDir, client.DiskAvailableMB(cfg.DataDir), cfg.ResourceLimits.MaxDiskGB)
	containerUsable := checkContainer(rep, logger)
	checkDaemon(rep, cfg.DataDir)

	// Machine capability, honestly derived (BG-12-doctor): WASM always; CONTAINER when a
	// backend is usable; NATIVE only when at least one head is trusted for it. Which heads
	// actually receive each runtime is per-head trust — shown in the Heads section and by
	// `heads list`.
	machineRuntimes := []string{"WASM"}
	if containerUsable {
		machineRuntimes = append(machineRuntimes, "CONTAINER")
	}
	if anyServerTrusts(cfg.Servers, "NATIVE") {
		machineRuntimes = append(machineRuntimes, "NATIVE")
	}
	rep.add(docInfo, "runtimes", fmt.Sprintf("this machine can run: %v (runtime trust is per-head — see `heads list`)", machineRuntimes), "")

	caps := volunteerCaps{
		maxMemoryMB:     cfg.ResourceLimits.MaxMemoryMB,
		containerUsable: containerUsable,
		hasGPU:          volunteerHasGPU(),
	}
	rep.add(docInfo, "memory limit", fmt.Sprintf("%d MB (resource_limits.max_memory_mb) — a head only sends leafs whose per-unit memory fits under this", caps.maxMemoryMB), "")

	fmt.Fprintln(out)
	fmt.Fprintf(out, "Heads (%d configured):\n", len(cfg.Servers))
	checkHeads(cmd.Context(), rep, logger, caps)

	fmt.Fprintln(out)
	switch {
	case rep.fails > 0:
		fmt.Fprintf(out, "Summary: %d failure(s), %d warning(s) — fix the failures above before expecting work.\n", rep.fails, rep.warns)
		return fmt.Errorf("doctor found %d blocking problem(s)", rep.fails)
	case rep.warns > 0:
		fmt.Fprintf(out, "Summary: no blocking failures, %d warning(s) — review them above.\n", rep.warns)
	default:
		fmt.Fprintln(out, "Summary: all checks passed.")
	}
	return nil
}

// checkAccountInfo surfaces the identity and runtime context an operator would
// otherwise have to gather from three separate commands: the build version, the
// account (the Ed25519 key + head-assigned volunteer id), this machine's host id,
// and the schedule. All informational — these never fail the report.
func checkAccountInfo(rep *doctorReport) {
	rep.add(docInfo, "version", version, "")

	if pub, _, err := identity.LoadKeyPair(cfg.KeyFile, cfg.PubKeyFile); err == nil {
		rep.add(docInfo, "account key", base64.RawURLEncoding.EncodeToString(pub)+" (Ed25519 identity; same key = same account on every machine)", "")
	}
	if cfg.VolunteerID != "" {
		rep.add(docInfo, "volunteer id", cfg.VolunteerID+" (account)", "")
	} else {
		rep.add(docInfo, "volunteer id", "not yet assigned — registers on first start", "")
	}

	// Host ids are HEAD-ISSUED and stored per-head (BG-25): the head mints a
	// per-machine id at registration and the client persists it keyed by that head's
	// gRPC address. Report each configured head's id, or 'none yet' before the first
	// registration mints one.
	ids, _ := identity.NewHostIDStore(cfg.HostIDsPath()).All()
	if len(cfg.Servers) == 0 {
		rep.add(docInfo, "host id", "no heads configured — a head issues one on first start", "")
	} else {
		seen := make(map[string]bool, len(cfg.Servers))
		for _, srv := range cfg.Servers {
			if seen[srv.GRPCAddress] {
				continue
			}
			seen[srv.GRPCAddress] = true
			label := "host id (" + srv.DisplayName() + ")"
			if id := ids[srv.GRPCAddress]; id != "" {
				rep.add(docInfo, label, id+" (issued by this head, under the account)", "")
			} else {
				rep.add(docInfo, label, "none yet — minted on first start", "")
			}
		}
	}

	rep.add(docInfo, "schedule", describeSchedule(cfg.Scheduling), "")
}

// describeSchedule renders the scheduling config as a one-line human summary.
func describeSchedule(s config.Scheduling) string {
	mode := s.Mode
	if mode == "" {
		mode = "ALWAYS"
	}
	switch mode {
	case "ALWAYS":
		return "ALWAYS (runs whenever the daemon is started)"
	case "WHEN_IDLE":
		return fmt.Sprintf("WHEN_IDLE (after %d min of machine idle)", s.IdleThresholdMins)
	case "SCHEDULED":
		switch {
		case len(s.ScheduleRanges) > 0:
			parts := make([]string, 0, len(s.ScheduleRanges))
			for _, r := range s.ScheduleRanges {
				parts = append(parts, describeRange(r))
			}
			return "SCHEDULED: " + strings.Join(parts, "; ")
		case s.CronExpression != "":
			return "SCHEDULED (cron: " + s.CronExpression + ")"
		default:
			return "SCHEDULED but no window configured — the volunteer will never run (set one with `schedule set`)"
		}
	default:
		return mode
	}
}

func checkDataDir(rep *doctorReport, dataDir string) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		rep.add(docFail, "data dir", fmt.Sprintf("%s — cannot create (%v)", dataDir, err),
			"choose a writable path with --data-dir")
		return
	}
	probe := filepath.Join(dataDir, ".doctor-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		rep.add(docFail, "data dir", fmt.Sprintf("%s — not writable (%v)", dataDir, err),
			"fix permissions or use --data-dir on a writable volume")
		return
	}
	_ = os.Remove(probe)
	rep.add(docOK, "data dir", fmt.Sprintf("%s (writable)", dataDir), "")
}

func checkIdentity(rep *doctorReport, keyFile, pubKeyFile string) {
	if !identity.KeyPairExists(keyFile, pubKeyFile) {
		rep.add(docFail, "identity", "no keypair found",
			"run: lettuce-volunteer init")
		return
	}
	if _, _, err := identity.LoadKeyPair(keyFile, pubKeyFile); err != nil {
		// The keypair is present but won't load — the data-dir-relocation failure
		// mode (TODO #25). Give an actionable ownership/re-copy remedy; never advise
		// `init` here, which would mint a new identity and abandon this account.
		rep.add(docFail, "identity", fmt.Sprintf("keypair present but unreadable (%v)", err),
			identity.LoadFailureRemedy(err, keyFile, pubKeyFile))
		return
	}
	rep.add(docOK, "identity", "keypair present and valid", "")
}

// checkDisk reports the disk-space verdict the daemon's live fetch gate
// (daemon.shouldFetch) would reach for this volume, derived from the SAME
// thresholds via daemon.ClassifyDiskGate so the diagnostic and the gate can
// never disagree (TODO #24). The earlier check demanded the full max_disk_gb
// allowance free and flagged a hard failure below it, which was a false positive
// on hosts where the gate still fetched work for already-cached images.
func checkDisk(rep *doctorReport, dataDir string, availableMB int64, maxDiskGB int) {
	fullRequiredMB, cachedHeadroomMB := daemon.DiskGateThresholds(maxDiskGB)

	// A non-positive reading means the free-space probe failed (e.g. statfs
	// error). The live gate's CheckDiskSpace would error and block, but doctor
	// has no useful number to show, so surface it as a warning rather than a
	// confident pass or fail.
	if availableMB <= 0 {
		rep.add(docWarn, "disk space",
			fmt.Sprintf("could not determine free space on %s", dataDir),
			"check that the data dir is on a mounted, readable volume")
		return
	}

	switch daemon.ClassifyDiskGate(availableMB, maxDiskGB) {
	case daemon.DiskAmple:
		rep.add(docOK, "disk space",
			fmt.Sprintf("%d MB free on %s (allowance %d MB)", availableMB, dataDir, fullRequiredMB), "")
	case daemon.DiskCachedOnly:
		// Between the cached-image headroom and the full allowance: the gate
		// still runs work for any leaf whose image is already pulled, but a
		// fresh image pull is gated. A warning, not a blocking failure.
		rep.add(docWarn, "disk space",
			fmt.Sprintf("%d MB free on %s — below the %d MB max_disk_gb allowance; work still runs for leafs whose image is already cached (needs %d MB), but pulling a fresh image is gated",
				availableMB, dataDir, fullRequiredMB, cachedHeadroomMB),
			"free disk space or lower resource_limits.max_disk_gb if you need fresh image pulls")
	default: // daemon.DiskBlocked
		floorMB := fullRequiredMB
		if cachedHeadroomMB < floorMB {
			floorMB = cachedHeadroomMB
		}
		rep.add(docFail, "disk space",
			fmt.Sprintf("%d MB free on %s — below the %d MB the fetch gate needs to run any work", availableMB, dataDir, floorMB),
			"free space, lower resource_limits.max_disk_gb, or use --data-dir on a roomier volume")
	}
}

// checkContainer reports whether the container runtime is genuinely usable, and
// returns true only when its socket actually responds. Detection alone isn't
// enough — a rootless Podman socket can exist but be permission-denied — so we
// construct the runtime and Ping it.
func checkContainer(rep *doctorReport, logger *slog.Logger) (usable bool) {
	if !containsRuntime(cfg.AvailableRuntimes, "CONTAINER") {
		rep.add(docInfo, "container", "not enabled (no CONTAINER in available_runtimes) — native/wasm leafs still run", "")
		return false
	}

	info := runtime.DetectContainerBackendPreferred(runtime.BundledPodmanPath(), runtime.ContainerBackend(cfg.ContainerBackend))
	if info.Backend == runtime.BackendNone {
		rep.add(docWarn, "container", "CONTAINER is enabled but no Docker or Podman was found",
			"install Docker or Podman, or remove CONTAINER from available_runtimes")
		return false
	}

	cr, err := runtime.NewContainerRuntimeForBackend(cfg.DataDir, logger, info)
	if err != nil {
		rep.add(docWarn, "container", fmt.Sprintf("%s found but could not be initialized (%v)", info.Backend, err),
			containerRemedy(info.Backend))
		return false
	}
	defer cr.Client().Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cr.Client().Ping(ctx); err != nil {
		rep.add(docWarn, "container", fmt.Sprintf("%s found but its socket is not reachable (%v)", info.Backend, err),
			containerRemedy(info.Backend))
		return false
	}

	desc := string(info.Backend)
	if info.Version != "" {
		desc += " " + info.Version
	}
	rep.add(docOK, "container", desc+" — socket reachable", "")

	// Report the image-store filesystem (TODO #31): a container leaf's image is
	// pulled into the engine's store (Docker DockerRootDir / Podman graphroot),
	// NOT under the lettuce data dir, so a roomy data dir can hide a too-small
	// image-store volume. Surface where it lands and whether it has pull headroom.
	if einfo, ierr := cr.Client().Info(ctx); ierr == nil && einfo != nil {
		checkImageStorePaths(rep, einfo, cfg.ResourceLimits.MaxDiskGB)
	} else {
		rep.add(docInfo, "image store", "could not determine the container image-store path from the engine",
			"if a big-image pull fails with ENOSPC, check free space where the engine stores images (Docker data-root / Podman graphroot)")
	}
	return true
}

// checkImageStorePaths reports free space on the filesystem(s) where the
// container backend actually stores images. Normally that is a single path
// (Docker DockerRootDir / Podman graphroot), but under Docker's containerd
// snapshotter the image content lives under the containerd root (e.g.
// /var/lib/containerd) — a different directory DockerRootDir does not name — so
// we surface that and gate on whichever candidate filesystem has the least room
// (the one a pull would run out of space on first), matching the live disk gate.
func checkImageStorePaths(rep *doctorReport, einfo *runtime.EngineInfo, maxDiskGB int) {
	paths := einfo.ImageStorePaths
	if len(paths) == 0 {
		if einfo.StoragePath == "" {
			rep.add(docInfo, "image store", "could not determine the container image-store path from the engine",
				"if a big-image pull fails with ENOSPC, check free space where the engine stores images (Docker data-root / Podman graphroot)")
			return
		}
		paths = []string{einfo.StoragePath}
	}

	if einfo.Snapshotter {
		rep.add(docInfo, "image store",
			fmt.Sprintf("Docker is using the containerd snapshotter — image content lives under the containerd root, not just %s (checking: %s)",
				einfo.StoragePath, strings.Join(paths, ", ")),
			"to free image-store space or move it to a bigger disk, target the containerd root (default /var/lib/containerd), not /var/lib/docker")
	}

	// Report the binding (least-free) path — the one the disk gate trips on first.
	bindPath, bindFree := paths[0], client.DiskAvailableMB(paths[0])
	for _, p := range paths[1:] {
		f := client.DiskAvailableMB(p)
		if bindFree <= 0 || (f > 0 && f < bindFree) {
			bindPath, bindFree = p, f
		}
	}
	checkImageStore(rep, bindPath, bindFree, maxDiskGB)
}

// checkImageStore reports free space on the filesystem where the container
// backend stores and extracts images — Docker's DockerRootDir / Podman's
// graphroot — which is NOT the lettuce data dir. A big-image leaf's pull lands
// here, so a roomy data dir paired with a small image-store volume is exactly
// the host the data-dir-only gate used to miss before failing mid-pull with
// ENOSPC (TODO #31). Low space here is a warning, not a blocking failure: native
// leafs and already-cached-image leafs still run.
func checkImageStore(rep *doctorReport, storePath string, availableMB int64, maxDiskGB int) {
	fullRequiredMB, _ := daemon.DiskGateThresholds(maxDiskGB)

	if availableMB <= 0 {
		rep.add(docWarn, "image store",
			fmt.Sprintf("container images are stored at %s, but its free space could not be determined", storePath),
			"check that the image-store volume is mounted and readable")
		return
	}
	if availableMB >= int64(fullRequiredMB) {
		rep.add(docOK, "image store",
			fmt.Sprintf("%d MB free at %s (the engine's image store; a fresh big-image pull needs up to %d MB here)",
				availableMB, storePath, fullRequiredMB), "")
		return
	}
	rep.add(docWarn, "image store",
		fmt.Sprintf("%d MB free at %s — below the %d MB allowance a fresh big-image pull can need; the pull would run out of space on this volume even if the data dir has room",
			availableMB, storePath, fullRequiredMB),
		"free space on the image-store volume, repoint the engine's storage (Docker data-root / Podman graphroot) to a roomier disk, or enlarge the Podman-machine disk")
}

func containerRemedy(backend runtime.ContainerBackend) string {
	if backend == runtime.BackendPodman {
		return "start the user socket (rootless Podman: `systemctl --user enable --now podman.socket`) and run lettuce as your normal user, not sudo"
	}
	return "ensure the container daemon is running and your user has permission to use it"
}

func checkDaemon(rep *doctorReport, dataDir string) {
	pid, err := daemon.ReadPID(dataDir)
	if err == nil && daemon.IsProcessRunning(pid) {
		rep.add(docInfo, "daemon", fmt.Sprintf("already running (PID %d)", pid), "")
		return
	}
	rep.add(docInfo, "daemon", "not running", "")
}

func checkHeads(ctx context.Context, rep *doctorReport, logger *slog.Logger, caps volunteerCaps) {
	if len(cfg.Servers) == 0 {
		rep.add(docFail, "(none)", "no heads configured",
			"run: lettuce-volunteer attach --server <host>")
		return
	}

	reachable := 0
	for _, srv := range cfg.Servers {
		if checkOneHead(ctx, rep, logger, srv, caps) {
			reachable++
		}
	}
	// If heads are configured but none could be reached, that blocks all work.
	if reachable == 0 {
		rep.add(docFail, "heads", "no configured head is reachable",
			"check the host/port, your network, and that the head is up")
	}
}

// checkOneHead connects to a single head using the public discovery RPCs (no
// identity needed), reports reachability + eligibility, and returns whether it
// was reachable.
func checkOneHead(ctx context.Context, rep *doctorReport, logger *slog.Logger, srv config.ServerConfig, caps volunteerCaps) bool {
	name := srv.DisplayName()
	gc, err := client.New(client.ClientConfig{
		ServerURL:     srv.GRPCAddress,
		Insecure:      srv.Insecure,
		TLSCertFile:   srv.CACertPath,
		TLSClientCert: srv.CertPath,
		TLSClientKey:  srv.KeyPath,
		ConnTimeout:   15 * time.Second,
		// Identity omitted: GetServerStatus/GetHeadInfo are public RPCs.
	}, logger)
	if err != nil {
		rep.add(docWarn, name, fmt.Sprintf("bad connection config (%v)", err),
			"check ca_cert/cert/key paths in config.yaml")
		return false
	}
	defer gc.Close()

	// Each probe dials a FRESH connection and pays full cold-start cost (DNS +
	// TLS handshake + HTTP/2 setup) before the RPC, so give each public RPC its
	// own generous deadline rather than sharing one tight budget. A busy head or
	// a cold connection can take several seconds even while the daemon's warm,
	// long-lived connection is submitting work fine.
	const probeTimeout = 15 * time.Second

	statusCtx, cancelStatus := context.WithTimeout(ctx, probeTimeout)
	defer cancelStatus()

	st, err := gc.GetServerStatus(statusCtx)
	if err != nil {
		// A deadline here usually means "slow/cold connection," not "down" — the
		// daemon's warm connection may be working fine. Don't cry "unreachable".
		if status.Code(err) == codes.DeadlineExceeded {
			rep.add(docWarn, name,
				fmt.Sprintf("slow to respond (no reply within %s)", probeTimeout),
				"the head is reachable but slow — a busy head or a cold connection can exceed this; if work is still flowing, this is usually benign")
		} else {
			rep.add(docWarn, name, fmt.Sprintf("unreachable (%v)", err),
				"verify the host/port and that the head is running")
		}
		return false
	}

	headCtx, cancelHead := context.WithTimeout(ctx, probeTimeout)
	defer cancelHead()

	resp, err := gc.GetHeadInfo(headCtx, &lettucev1.GetHeadInfoRequest{})
	if err != nil {
		rep.add(docWarn, name,
			fmt.Sprintf("reachable (server %s) but leaf list failed (%v)", st.GetVersion(), err), "")
		return true
	}

	res := evaluateLeafEligibility(resp.GetLeafs(), caps, srv)
	detail := fmt.Sprintf("reachable — server %s, db %s; eligible for %d of %d leafs",
		st.GetVersion(), statusOrUnknown(st.GetDatabaseStatus()), res.eligible, res.total)

	level := docOK
	remedy := ""
	if res.total > 0 && res.eligible == 0 {
		level = docWarn
		switch {
		case res.containerBlocked == res.total && !caps.containerUsable:
			remedy = "every leaf here needs a container runtime — fix the container check above, or attach a head with native leafs"
		case res.trustBlocked == res.total:
			remedy = fmt.Sprintf("every leaf here needs a runtime you have not trusted this head to run — opt in with 'lettuce-volunteer heads trust %s <runtime>' if you accept running its code", name)
		case res.memoryBlocked > 0:
			remedy = fmt.Sprintf("raise resource_limits.max_memory_mb (currently %d MB) to cover the per-leaf requirements below, then restart the daemon to re-advertise", caps.maxMemoryMB)
		case res.gpuBlocked == res.total:
			remedy = "every leaf here needs a GPU; none is detected/enabled (set resource_limits.max_gpu_vram_pct > 0 if you have one)"
		default:
			remedy = "this head has no leafs this volunteer can run — see the per-leaf reasons below"
		}
	}
	rep.add(level, name, detail, remedy)

	// Per-leaf requirement breakdown (#30): show exactly which leafs this volunteer
	// can't run and why (memory/GPU/runtime), so the operator can act even when some
	// leafs are still eligible (which keeps the head line a pass).
	for _, le := range res.leaves {
		if !le.eligible {
			fmt.Fprintf(rep.w, "                       - %s: %s\n", le.name, le.reason)
		}
	}
	return true
}

// volunteerCaps is the subset of this volunteer's advertised capabilities that gate
// leaf eligibility in doctor's per-head report.
type volunteerCaps struct {
	maxMemoryMB     int
	containerUsable bool
	hasGPU          bool
}

// leafEligibility is the per-leaf verdict doctor prints under a head.
type leafEligibility struct {
	name     string
	eligible bool
	reason   string // why it's ineligible; empty when eligible
}

// eligibilityResult aggregates the per-leaf verdicts for one head.
type eligibilityResult struct {
	total            int
	eligible         int
	containerBlocked int
	trustBlocked     int
	memoryBlocked    int
	gpuBlocked       int
	leaves           []leafEligibility
}

// evaluateLeafEligibility decides, for each leaf the head offers, whether this
// volunteer can actually run it — applying the same gates the daemon applies:
// runtime availability (a leaf needs the container runtime iff its execution
// spec carries an image), PER-HEAD RUNTIME TRUST (the fetcher refuses — and
// never advertises — CONTAINER/NATIVE work for a head the volunteer has not
// trusted for that runtime; WASM is always trusted), the
// execution_config.max_memory_mb ceiling (the gate that silently fires for a
// default-configured volunteer, #30), and GPU presence
// (execution_config.gpu_required). Ignoring trust counted leafs the volunteer
// could never receive as "eligible" (PB-5). A leaf may be blocked by more than
// one gate; the first that bites is reported (container, then trust, then
// memory, then GPU) and each blocking dimension is tallied so the caller can
// print the right remedy.
func evaluateLeafEligibility(leafs []*lettucev1.LeafInfo, caps volunteerCaps, srv config.ServerConfig) eligibilityResult {
	var res eligibilityResult
	head := srv.DisplayName()
	for _, lf := range leafs {
		res.total++
		es := lf.GetExecutionSpec() // nil-safe getters below
		name := lf.GetSlug()
		if name == "" {
			name = lf.GetName()
		}
		if name == "" {
			name = lf.GetId()
		}

		needsContainer := es.GetImage() != ""
		leafMemMB := int(es.GetMaxMemoryMb())
		needsGPU := es.GetGpuRequired()
		wasmCapable, nativeCapable := false, false
		for k := range es.GetBinaries() {
			if strings.EqualFold(k, "wasm") {
				wasmCapable = true
			} else {
				nativeCapable = true
			}
		}

		switch {
		case needsContainer && !caps.containerUsable:
			res.containerBlocked++
			res.leaves = append(res.leaves, leafEligibility{name, false, "needs a container runtime"})
		case needsContainer && !srv.TrustsRuntime("CONTAINER"):
			res.trustBlocked++
			res.leaves = append(res.leaves, leafEligibility{name, false,
				fmt.Sprintf("needs the CONTAINER runtime, which you have not trusted this head to run (opt in: lettuce-volunteer heads trust %s container)", head)})
		case !needsContainer && nativeCapable && !wasmCapable && !srv.TrustsRuntime("NATIVE"):
			res.trustBlocked++
			res.leaves = append(res.leaves, leafEligibility{name, false,
				fmt.Sprintf("needs the NATIVE runtime, which you have not trusted this head to run (opt in: lettuce-volunteer heads trust %s native)", head)})
		case leafMemMB > caps.maxMemoryMB:
			res.memoryBlocked++
			res.leaves = append(res.leaves, leafEligibility{name, false,
				fmt.Sprintf("needs %d MB memory > your limit %d MB", leafMemMB, caps.maxMemoryMB)})
		case needsGPU && !caps.hasGPU:
			res.gpuBlocked++
			res.leaves = append(res.leaves, leafEligibility{name, false, "needs a GPU; none detected/enabled"})
		default:
			res.eligible++
			res.leaves = append(res.leaves, leafEligibility{name, true, ""})
		}
	}
	return res
}

// detectGPUsFunc is overridable in tests; defaults to real GPU detection.
var detectGPUsFunc = runtime.DetectGPUs

// volunteerHasGPU reports whether this volunteer can run GPU work: GPU tasks must be
// enabled in config (max_gpu_vram_pct != 0) AND a GPU must actually be present.
func volunteerHasGPU() bool {
	if cfg.ResourceLimits.MaxGPUVRAMPct == 0 {
		return false
	}
	return len(detectGPUsFunc()) > 0
}

func statusOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
