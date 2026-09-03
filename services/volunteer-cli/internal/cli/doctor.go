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
	"github.com/lettuce-compute/volunteer-cli/internal/cron"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
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

Safe to run any time — it never changes anything and works without the daemon,
though the disk verdicts are most accurate while the daemon runs: doctor then
quotes the daemon's own enforced usage measurement (cached container images
included) and its live per-leaf fetch-gate verdicts, instead of recomputing
them under a fresh-download assumption. Exits non-zero if a check that would
block all work fails.`,
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
	checkIdentity(rep, cfg.KeyFilePath(), cfg.PubKeyFilePath())
	freeDataDirMB := client.DiskAvailableMB(cfg.DataDir)
	checkDisk(rep, cfg.DataDir, freeDataDirMB)
	// Container and daemon are checked before disk usage because the usage
	// verdict depends on both: the running daemon's figure is the enforced one,
	// and a container host without it can only report a partial number (TB-30).
	containerUsable := checkContainer(rep, logger)
	checkDaemon(rep, cfg.DataDir)
	lettuceUsedMB, usedMBKnown := checkDiskUsage(rep, cfg.DataDir, cfg.ResourceLimits.MaxDiskGB, containerUsable)

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
		maxDiskMB:       int64(cfg.ResourceLimits.MaxDiskGB) * 1024,
		maxCPUCores:     cfg.ResourceLimits.MaxCPUCores,
		freeDataDirMB:   freeDataDirMB,
		lettuceUsedMB:   lettuceUsedMB,
		usedMBKnown:     usedMBKnown,
	}
	caps.maxGPUVRAMMB, caps.gpuCardVRAMMB, caps.gpuVRAMPct, caps.gpuVendors, caps.gpuComputeCapabilities =
		volunteerGPUBudget()
	rep.add(docInfo, "memory limit", fmt.Sprintf("%d MB (resource_limits.max_memory_mb) — a head only sends leafs whose per-unit memory fits under this", caps.maxMemoryMB), "")
	// Disk and cores gate dispatch exactly as memory does, and used to be the only
	// two budgets nothing on this machine reported (TB-15). Printed next to the
	// memory line so all three are read together.
	rep.add(docInfo, "disk allowance", fmt.Sprintf("%d MB / %d GB (resource_limits.max_disk_gb) — a head only sends leafs whose required disk fits under this; this is the allowance you set, not free space (see 'disk space' above)",
		caps.maxDiskMB, cfg.ResourceLimits.MaxDiskGB), "")
	rep.add(docInfo, "cpu limit", fmt.Sprintf("%d cores (resource_limits.max_cpu_cores) — a head only sends leafs whose required cores fit under this", caps.maxCPUCores), "")
	checkCPUEnforcement(rep, caps.maxCPUCores)

	fmt.Fprintln(out)
	fmt.Fprintf(out, "Heads (%d configured):\n", len(cfg.Servers))
	checkHeads(cmd.Context(), rep, logger, caps, fetchDaemonLeafDiskGates())

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

	if pub, _, err := identity.LoadKeyPair(cfg.KeyFilePath(), cfg.PubKeyFilePath()); err == nil {
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

	// A schedule that can never become active blocks ALL work, so it is a failure
	// here, not a line of information. Doctor previously echoed an unparseable cron
	// expression verbatim alongside the passing checks, which is what let a
	// silently-idle volunteer read as healthy (TB-3).
	if err := cfg.Scheduling.NeverRuns(); err != nil {
		rep.add(docFail, "schedule", describeSchedule(cfg.Scheduling),
			"the volunteer will never run — set a daily window with `lettuce-volunteer schedule set --from 20:00 --to 06:00`, or `schedule clear` to run always")
		return
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
			if err := cron.Validate(s.CronExpression); err != nil {
				return "SCHEDULED (cron: " + s.CronExpression + ") — NOT a valid cron expression: " + err.Error()
			}
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
// thresholds so the diagnostic and the gate can never disagree (TODO #24).
// Free space is compared against the absolute floor only: what a fetch requires
// free is each LEAF's declared need, never the whole max_disk_gb allowance —
// the earlier allowance-as-floor check meant raising the allowance to qualify
// for a big leaf gated the volunteer's own image downloads (TB-24). Per-leaf
// download verdicts appear in the Heads section, where each leaf's need is
// known.
func checkDisk(rep *doctorReport, dataDir string, availableMB int64) {
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

	if availableMB < daemon.DiskFloorMB {
		rep.add(docFail, "disk space",
			fmt.Sprintf("%d MB free on %s — below the %d MB floor the fetch gate needs to run any work", availableMB, dataDir, daemon.DiskFloorMB),
			"free space, or use --data-dir on a roomier volume")
		return
	}
	rep.add(docOK, "disk space",
		fmt.Sprintf("%d MB free on %s — a work fetch needs the leaf's declared disk requirement free (plus a %d MB floor), shown per leaf in the Heads section", availableMB, dataDir, daemon.DiskFloorMB), "")
}

// metricsAPIResponse mirrors the disk fields of the management API's GET
// /api/v1/metrics — the running daemon's own TTL-cached usage measurement,
// which is the figure the fetch gate actually enforces.
type metricsAPIResponse struct {
	DiskUsedMB      int64 `json:"disk_used_mb"`
	DiskAllowanceMB int64 `json:"disk_allowance_mb"`
	DiskUsageKnown  bool  `json:"disk_usage_known"`
}

// checkDiskUsage reports Lettuce's own measured footprint against the
// max_disk_gb allowance it is budgeted under (TB-24), and returns the figure
// (with whether it is complete) for the per-leaf budget verdicts in the Heads
// section. The running daemon's measurement is quoted whenever it can be had —
// it is the number the fetch gate enforces, data-dir tree PLUS cached
// container images. Measuring the data-dir tree alone and merely labelling the
// missing half read "41 MB of 30720 · all checks passed" on a host the daemon
// had fully disk-gated at 27,723 MB of cached images (TB-30) — so without the
// daemon, a container host's workspace-only figure is reported as partial with
// the budget verdict unknown, never as a pass.
func checkDiskUsage(rep *doctorReport, dataDir string, maxDiskGB int, containerUsable bool) (usedMB int64, known bool) {
	allowanceMB := int64(maxDiskGB) * 1024

	var mr metricsAPIResponse
	if err := managementGet(dataDir, "/api/v1/metrics", &mr); err == nil && mr.DiskUsageKnown {
		// Trust the daemon's allowance over the config file's: the daemon
		// enforces what it was started with.
		reportDiskUsage(rep, mr.DiskUsedMB, mr.DiskAllowanceMB,
			"measured by the running daemon: work folders + cached container images")
		return mr.DiskUsedMB, true
	}

	dirMB, err := daemon.DirSizeMB(dataDir)
	if err != nil {
		rep.add(docInfo, "disk usage",
			fmt.Sprintf("could not measure the data dir (%v); the running daemon budgets its usage against the %d MB allowance", err, allowanceMB), "")
		return 0, false
	}
	if containerUsable {
		// On a container host the dominant term is usually the cached images,
		// which only the daemon sizes against its wanted-image set. A partial
		// figure must not feed a pass verdict.
		rep.add(docWarn, "disk usage",
			fmt.Sprintf("work folders use %d MB of the %d MB allowance, but cached container images count too and are not measured here, so the budget verdict is unknown", dirMB, allowanceMB),
			"start the daemon and re-run doctor — the running daemon reports the enforced figure, images included")
		return dirMB, false
	}
	// No usable container engine: the gate itself cannot count images either
	// (unknown fails open), so the workspace figure is the enforced one.
	reportDiskUsage(rep, dirMB, allowanceMB, "work folders under the data dir")
	return dirMB, true
}

// reportDiskUsage renders the usage-vs-allowance line, warning at or over the
// allowance, where the daemon stops fetching entirely.
func reportDiskUsage(rep *doctorReport, usedMB, allowanceMB int64, source string) {
	level := docInfo
	remedy := ""
	if usedMB >= allowanceMB {
		level = docWarn
		remedy = "free space under the data dir, disable an unused leaf (its cached image leaves the budget), or raise resource_limits.max_disk_gb — at or over the allowance the daemon stops fetching (superseded container images are reclaimed automatically)"
	}
	rep.add(level, "disk usage",
		fmt.Sprintf("Lettuce is using %d MB of its %d MB allowance (%s)", usedMB, allowanceMB, source), remedy)
}

// checkCPUEnforcement reports the CPU cap ACTUALLY in force, next to the
// configured one.
//
// The two can differ silently. On the Linux affinity fallback the cap is applied
// by confining work to a subset of the CPUs this process may use, so when the
// machine already confines Lettuce to fewer CPUs than max_cpu_cores the
// configured number is not the binding constraint — and before TB-16 that
// mismatch produced either a bare "sched_setaffinity failed" or, on a partial
// overlap, no output at all. A volunteer had nowhere to confirm their setting
// was holding. This line is that place.
func checkCPUEnforcement(rep *doctorReport, maxCPUCores int) {
	e := resource.DescribeCPUEnforcement()

	if !e.Confinable {
		rep.add(docInfo, "cpu enforcement",
			fmt.Sprintf("%s — work is not confined to a core count on this platform; max_cpu_cores still decides which leafs a head will send you", e.Mechanism), "")
		return
	}

	if e.PermittedCPUs <= 0 {
		rep.add(docInfo, "cpu enforcement", e.Mechanism, "")
		return
	}

	if e.PermittedCPUs <= maxCPUCores {
		// Not a fault: the machine confines Lettuce more tightly than the
		// volunteer's own budget does. Worth stating plainly, because it is
		// indistinguishable from "my limit is being ignored" from the outside.
		rep.add(docInfo, "cpu enforcement",
			fmt.Sprintf("%s — this machine permits Lettuce on %d CPU(s), at or below your %d-core limit, so work can use at most %d",
				e.Mechanism, e.PermittedCPUs, maxCPUCores, e.PermittedCPUs), "")
		return
	}

	rep.add(docInfo, "cpu enforcement",
		fmt.Sprintf("%s — this machine permits Lettuce on %d CPU(s); work is confined to %d of them",
			e.Mechanism, e.PermittedCPUs, maxCPUCores), "")
}

// detectContainerBackendDoctor is checkContainer's engine-detection seam,
// overridable in tests so the probe-first verdict can be exercised without a
// real engine on the test host.
var detectContainerBackendDoctor = runtime.DetectContainerBackendPreferred

// checkContainer reports whether the container runtime is genuinely usable, and
// returns true only when its socket actually responds. It derives the answer the
// same way the daemon does — a live engine probe — never from a config record:
// the retired available_runtimes key used to short-circuit this check, which let
// doctor call a machine container-incapable while it was executing a container
// unit (TB-25). Detection alone isn't enough either — a rootless Podman socket
// can exist but be permission-denied — so we construct the runtime and Ping it,
// which also finally distinguishes "no engine" from "engine present, socket
// broken" in every config state.
func checkContainer(rep *doctorReport, logger *slog.Logger) (usable bool) {
	info := detectContainerBackendDoctor(runtime.BundledPodmanPath(), runtime.ContainerBackend(cfg.ContainerBackend))
	if info.Backend == runtime.BackendNone {
		rep.add(docInfo, "container", "no container engine found (Docker or Podman) — container leafs need one; native/wasm leafs still run",
			"install Docker or Podman if you want to run container leafs")
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
	if info.Backend == runtime.BackendDocker && info.Engine == "podman" {
		// The Docker socket answered, but Podman is behind it (TB-54).
		desc += " (Docker-compatible socket served by Podman)"
	}
	rep.add(docOK, "container", desc+" — socket reachable", "")

	// Report the image-store filesystem (TODO #31): a container leaf's image is
	// pulled into the engine's store (Docker DockerRootDir / Podman graphroot),
	// NOT under the lettuce data dir, so a roomy data dir can hide a too-small
	// image-store volume. Surface where it lands and whether it has pull headroom.
	if einfo, ierr := cr.Client().Info(ctx); ierr == nil && einfo != nil {
		checkImageStorePaths(rep, einfo)
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
// we surface that and report whichever candidate filesystem has the least room
// (the one a pull would run out of space on first), matching the live disk gate.
func checkImageStorePaths(rep *doctorReport, einfo *runtime.EngineInfo) {
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
	checkImageStore(rep, bindPath, bindFree)
}

// checkImageStore reports free space on the filesystem where the container
// backend stores and extracts images — Docker's DockerRootDir / Podman's
// graphroot — which is NOT the lettuce data dir. A big-image leaf's pull lands
// here, so a roomy data dir paired with a small image-store volume is exactly
// the host the data-dir-only gate used to miss before failing mid-pull with
// ENOSPC (TODO #31). What a pull requires free here is the LEAF's declared disk
// need plus the floor — never the whole max_disk_gb allowance (TB-24) — so this
// line reports the reading and the rule; the per-leaf verdicts live in the
// Heads section where each leaf's need is known.
func checkImageStore(rep *doctorReport, storePath string, availableMB int64) {
	if availableMB <= 0 {
		// Matches the daemon's verdict for the same condition: unknown is not
		// "full", so fetching is NOT gated on this path (normal on Windows/macOS,
		// where a podman machine's graphroot is a VM-internal path the host
		// cannot stat).
		rep.add(docWarn, "image store",
			fmt.Sprintf("container images are stored at %s, but its free space could not be determined from this host — work fetching is not gated on it", storePath),
			"the engine enforces its own storage limits; if a big-image pull fails with ENOSPC, free space there or enlarge the Podman-machine disk")
		return
	}
	rep.add(docOK, "image store",
		fmt.Sprintf("%d MB free at %s (the engine's image store; a fresh pull needs the leaf's declared disk requirement free here, +%d MB floor)",
			availableMB, storePath, daemon.DiskFloorMB), "")
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

// fetchDaemonLeafDiskGates asks the running daemon for its per-leaf disk-gate
// verdicts, keyed by gRPC address then by leaf slug (name when the slug is
// empty) so checkOneHead can match them to the leafs the HEAD reports. nil
// when the daemon is down or unreachable — the per-leaf notes then fall back
// to the conservative local reading, labelled as an assumption (TB-42).
func fetchDaemonLeafDiskGates() map[string]map[string]*leafsAPIDiskGate {
	resp, err := fetchHeadsFromAPI()
	if err != nil {
		return nil
	}
	gates := make(map[string]map[string]*leafsAPIDiskGate)
	for _, h := range resp.Heads {
		byLeaf := make(map[string]*leafsAPIDiskGate)
		for _, l := range h.Leafs {
			if l.DiskGate == nil {
				continue // a daemon predating the field
			}
			key := l.Slug
			if key == "" {
				key = l.Name
			}
			byLeaf[key] = l.DiskGate
		}
		gates[h.GRPCAddress] = byLeaf
	}
	return gates
}

func checkHeads(ctx context.Context, rep *doctorReport, logger *slog.Logger, caps volunteerCaps, daemonGates map[string]map[string]*leafsAPIDiskGate) {
	if len(cfg.Servers) == 0 {
		rep.add(docFail, "(none)", "no heads configured",
			"run: lettuce-volunteer attach --server <host>")
		return
	}

	reachable := 0
	for _, srv := range cfg.Servers {
		if checkOneHead(ctx, rep, logger, srv, caps, daemonGates[srv.GRPCAddress]) {
			reachable++
		}
	}
	// If heads are configured but none could be reached, that blocks all work.
	if reachable == 0 {
		rep.add(docFail, "heads", "no configured head is reachable",
			"check the host/port, your network, and that the head is up")
	}
}

// headProbePolicy controls how hard doctor tries before calling a head down.
type headProbePolicy struct {
	attempts   int           // total tries, including the first
	firstProbe time.Duration // deadline for the FIRST try — short, see below
	perAttempt time.Duration // deadline for every later try — generous
	backoff    time.Duration // pause between tries
}

// probeDeadline returns the deadline for a given 1-based attempt.
//
// The first try is deliberately impatient and the retry patient. The failure
// this policy exists for is a connection whose FIRST RPC stalls and whose second
// answers at once, so waiting out a long deadline on attempt 1 buys nothing —
// the answer is in attempt 2. Escalating means the reported case resolves faster
// than the single long attempt it replaced, while a genuinely slow-but-healthy
// head still gets the full deadline before anyone calls it unreachable.
func (p headProbePolicy) probeDeadline(attempt int) time.Duration {
	if attempt <= 1 && p.firstProbe > 0 {
		return p.firstProbe
	}
	return p.perAttempt
}

// defaultHeadProbePolicy gives a head the same second chance the daemon does.
//
// A single attempt was not enough to justify a blocking failure (TB-12): the
// daemon's connect path retries (client.NewWithRetry, 1s initial backoff) and
// routinely succeeds on attempt 2 against a head whose first RPC stalls, so
// doctor reported "no configured head is reachable" on machines that were
// fetching, computing and submitting work at that very moment. A diagnostic
// that fails on a healthy setup trains testers to ignore it, and it did:
// it masked a real bug (TB-11) for a week.
//
// Two attempts with the daemon's own initial backoff, rather than the daemon's
// unbounded retry, because doctor has to terminate. Each attempt keeps the
// generous per-RPC deadline below.
var defaultHeadProbePolicy = headProbePolicy{
	attempts:   2,
	firstProbe: 8 * time.Second,
	perAttempt: 15 * time.Second,
	backoff:    1 * time.Second,
}

// probeServerStatus calls probe until it succeeds or the policy is exhausted,
// returning the response, the attempt number that produced it (or the number
// spent), and the last error. Each attempt gets its own deadline: a stalled
// first RPC must not eat the budget of the retry that would have succeeded.
func probeServerStatus(
	ctx context.Context,
	p headProbePolicy,
	probe func(context.Context) (*lettucev1.GetServerStatusResponse, error),
) (*lettucev1.GetServerStatusResponse, int, error) {
	if p.attempts < 1 {
		p.attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= p.attempts; attempt++ {
		probeCtx, cancel := context.WithTimeout(ctx, p.probeDeadline(attempt))
		st, err := probe(probeCtx)
		cancel()
		if err == nil {
			return st, attempt, nil
		}
		lastErr = err
		if attempt == p.attempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, attempt, ctx.Err()
		case <-time.After(p.backoff):
		}
	}
	return nil, p.attempts, lastErr
}

// checkOneHead connects to a single head using the public discovery RPCs (no
// identity needed), reports reachability + eligibility, and returns whether it
// was reachable. daemonGates is the running daemon's per-leaf disk-gate
// verdicts for THIS head (nil when the daemon is down).
func checkOneHead(ctx context.Context, rep *doctorReport, logger *slog.Logger, srv config.ServerConfig, caps volunteerCaps, daemonGates map[string]*leafsAPIDiskGate) bool {
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
	// long-lived connection is submitting work fine — and it is retried, so one
	// stalled first RPC is no longer enough to declare a head down (TB-12).
	policy := defaultHeadProbePolicy

	st, statusAttempts, err := probeServerStatus(ctx, policy, gc.GetServerStatus)
	if err != nil {
		tries := ""
		if policy.attempts > 1 {
			tries = fmt.Sprintf(" over %d attempts", policy.attempts)
		}
		// A deadline here usually means "slow/cold connection," not "down" — the
		// daemon's warm connection may be working fine. Don't cry "unreachable".
		if status.Code(err) == codes.DeadlineExceeded {
			rep.add(docWarn, name,
				fmt.Sprintf("slow to respond (no reply within %s%s)", policy.probeDeadline(policy.attempts), tries),
				"the head is reachable but slow — a busy head or a cold connection can exceed this; if work is still flowing, this is usually benign")
		} else {
			rep.add(docWarn, name, fmt.Sprintf("unreachable (%v)%s", err, tries),
				"verify the host/port and that the head is running")
		}
		return false
	}

	// Reached, but not on the first try. Say so rather than staying silent: this
	// is a real anomaly — the daemon shows the same first-RPC stall on every
	// start — and it is the signal TB-12's second half exists to chase. It is
	// deliberately a warning and not a failure, because work does flow.
	if statusAttempts > 1 {
		rep.add(docWarn, name,
			fmt.Sprintf("reachable, but only on attempt %d — the first connection did not answer within %s", statusAttempts, policy.probeDeadline(1)),
			"the head is usable and work will flow; a consistently slow first connection is worth reporting, as it also delays every daemon start")
	}

	// The connection is warm by now (GetServerStatus just succeeded on it), so
	// this second RPC gets the generous deadline outright rather than the
	// impatient first-try one.
	headCtx, cancelHead := context.WithTimeout(ctx, policy.perAttempt)
	defer cancelHead()

	resp, err := gc.GetHeadInfo(headCtx, &lettucev1.GetHeadInfoRequest{})
	if err != nil {
		rep.add(docWarn, name,
			fmt.Sprintf("reachable (server %s) but leaf list failed (%v)", st.GetVersion(), err), "")
		return true
	}

	res := evaluateLeafEligibility(resp.GetLeafs(), caps, srv, daemonGates)
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
		case res.diskBlocked > 0:
			remedy = fmt.Sprintf("raise resource_limits.max_disk_gb (currently %d GB) to cover the per-leaf requirements below, then restart the daemon to re-advertise", caps.maxDiskMB/1024)
		case res.coresBlocked > 0:
			remedy = fmt.Sprintf("raise resource_limits.max_cpu_cores (currently %d) to cover the per-leaf requirements below, then restart the daemon to re-advertise", caps.maxCPUCores)
		case res.vramBlocked > 0:
			remedy = fmt.Sprintf("raise resource_limits.max_gpu_vram_pct (currently %d%%, giving %d MB of your %d MB card) to cover the per-leaf requirements below, then restart the daemon to re-advertise",
				caps.gpuVRAMPct, caps.maxGPUVRAMMB, caps.gpuCardVRAMMB)
		case res.gpuBlocked == res.total:
			remedy = "every leaf here needs a GPU this machine does not have or has not enabled (set resource_limits.max_gpu_vram_pct > 0 if you have one; a leaf may also require a specific make of card)"
		default:
			remedy = "this head has no leafs this volunteer can run — see the per-leaf reasons below"
		}
	}
	// When EVERY eligible leaf is locally fetch-gated, this volunteer fetches
	// nothing from this head right now — the state the daemon reports as
	// "disk-gated ... stays idle". A pass verdict here is the TB-30 failure
	// shape (the diagnostic content while the daemon refuses all work), so the
	// head line escalates to a warning with the notes as the detail.
	if level == docOK && allEligibleFetchGated(res) {
		level = docWarn
		remedy = "the head would send every eligible leaf, but this machine's current disk state blocks fetching all of them — see the per-leaf notes below"
	}
	rep.add(level, name, detail, remedy)

	// Per-leaf requirement breakdown (#30): show exactly which leafs this volunteer
	// can't run and why (memory/GPU/runtime), so the operator can act even when some
	// leafs are still eligible (which keeps the head line a pass). An eligible leaf
	// whose LOCAL disk gate currently refuses it gets its note too (TB-24) — the
	// head would send it, this machine would skip it, and neither said so before.
	for _, le := range res.leaves {
		switch {
		case !le.eligible:
			fmt.Fprintf(rep.w, "                       - %s: %s\n", le.name, le.reason)
		case le.fetchNote != "":
			fmt.Fprintf(rep.w, "                       - %s: %s\n", le.name, le.fetchNote)
		}
	}
	return true
}

// allEligibleFetchGated reports whether every leaf the head would dispatch to
// this volunteer is currently blocked by the machine's LOCAL disk state (its
// fetchNote). That is the TB-30 failure shape — a content diagnostic while the
// daemon refuses all work — so the caller escalates it to a warning.
func allEligibleFetchGated(res eligibilityResult) bool {
	if res.eligible == 0 {
		return false
	}
	gated := 0
	for _, le := range res.leaves {
		if le.eligible && le.fetchNote != "" {
			gated++
		}
	}
	return gated == res.eligible
}

// volunteerCaps is the subset of this volunteer's advertised capabilities that gate
// leaf eligibility in doctor's per-head report.
type volunteerCaps struct {
	maxMemoryMB     int
	containerUsable bool
	hasGPU          bool
	// maxDiskMB and maxCPUCores are the other two budgets the head's dispatch
	// gate matches leafs against. They are in the units the head receives them
	// in — max_disk_gb is advertised as MB — so callers convert once when filling
	// this, never at the comparison (TB-15).
	maxDiskMB   int64
	maxCPUCores int
	// The GPU budgets, the last three dimensions dispatch matches on (TB-21).
	// maxGPUVRAMMB is the ALLOWED VRAM — card capacity * max_gpu_vram_pct / 100,
	// exactly what the head compares a leaf against — never the raw card size;
	// comparing against capacity would report machines eligible that the head
	// refuses, which is the bug. Vendors are uppercase ("NVIDIA").
	// gpuCardVRAMMB and gpuVRAMPct describe the same card maxGPUVRAMMB came from,
	// so a blocked message can name the setting to change and the hardware it
	// applies to.
	maxGPUVRAMMB           int
	gpuCardVRAMMB          int
	gpuVRAMPct             int
	gpuVendors             []string
	gpuComputeCapabilities []string
	// freeDataDirMB is the current free-space reading on the data-dir volume
	// (<= 0 when unknown). Not a dispatch dimension: it feeds the per-leaf
	// LOCAL download-gate note (TB-24), which uses the same thresholds as the
	// daemon's live gate so the two can never disagree.
	freeDataDirMB int64
	// lettuceUsedMB is Lettuce's measured disk usage from checkDiskUsage, valid
	// only when usedMBKnown — a partial (workspace-only, images unmeasured)
	// figure must not feed a budget verdict (TB-30). Feeds the per-leaf
	// usage + need vs allowance arithmetic, the half of the disk gate that
	// fired on a tester's host while every free-space number looked fine
	// (TB-31).
	lettuceUsedMB int64
	usedMBKnown   bool
}

// leafEligibility is the per-leaf verdict doctor prints under a head.
type leafEligibility struct {
	name     string
	eligible bool
	reason   string // why it's ineligible; empty when eligible
	// fetchNote is the LOCAL disk-gate observation (TB-24): the head would
	// dispatch this leaf, but this machine's current free space does not cover
	// the leaf's declared need, so the daemon's fetch gate is (or would be)
	// skipping it right now. Empty when free space covers the need or is
	// unknown. Derived from the daemon's own thresholds (TODO #24).
	fetchNote string
}

// localDiskFetchNote reports whether this machine's CURRENT disk state lets
// the daemon fetch this leaf — the daemon's per-leaf fetch gate, evaluated
// with doctor's readings and the same daemon functions the live gate uses
// (LeafDiskThresholds for free space, DiskBudgetVerdict for the allowance
// budget, EffectiveLeafDiskNeedMB for an undeclared need — the live gate
// applies that fallback, so doctor must too or the two disagree exactly on
// undeclared leafs, TB-31).
//
// This is the FALLBACK reading, used only when the running daemon's own
// verdict could not be fetched (see evaluateLeafEligibility): doctor cannot
// probe which images are already cached, so it assumes a fresh download —
// which charges a cached container leaf up to ~10 GB more than the live gate
// does. Both branches say so for container leafs; omitting the caveat from the
// budget branch is what let doctor contradict a happily-fetching daemon
// indefinitely (TB-42).
func localDiskFetchNote(req leafRequirements, caps volunteerCaps) string {
	need := daemon.EffectiveLeafDiskNeedMB(req.diskMB)
	assumed := ""
	if req.diskMB <= 0 {
		assumed = fmt.Sprintf(" (the leaf declares no disk need; %d MB is the assumed fallback)", need)
	}

	if caps.freeDataDirMB > 0 {
		dataDirMB, _ := daemon.LeafDiskThresholds(need, req.needsContainer, false)
		if caps.freeDataDirMB < dataDirMB {
			if req.needsContainer {
				return fmt.Sprintf("a fresh image download for this leaf is currently disk-gated: it needs %d MB free (+%d MB floor), %d MB available%s — work on an already-downloaded image still runs",
					need, daemon.DiskFloorMB, caps.freeDataDirMB, assumed)
			}
			return fmt.Sprintf("fetching this leaf is currently disk-gated: it needs %d MB free (+%d MB floor), %d MB available%s",
				need, daemon.DiskFloorMB, caps.freeDataDirMB, assumed)
		}
	}

	// The allowance-budget half (TB-30/TB-31): usage + the leaf's need must fit
	// under max_disk_gb. This is the half that gated a tester's host while
	// every free-space figure looked fine, and it needs a COMPLETE usage figure
	// — a workspace-only reading would pass a budget the daemon fails.
	if caps.usedMBKnown && caps.maxDiskMB > 0 {
		if ok, reason := daemon.DiskBudgetVerdict(need, req.needsContainer, false, caps.lettuceUsedMB, caps.maxDiskMB); !ok {
			note := reason + assumed
			if req.needsContainer {
				note += " (assuming a fresh image download — the running daemon knows whether the image is already cached and may charge less; start it and re-run doctor for the enforced verdict)"
			}
			return note
		}
	}
	return ""
}

// eligibilityResult aggregates the per-leaf verdicts for one head.
type eligibilityResult struct {
	total            int
	eligible         int
	containerBlocked int
	trustBlocked     int
	memoryBlocked    int
	diskBlocked      int
	coresBlocked     int
	vramBlocked      int
	gpuBlocked       int
	leaves           []leafEligibility
}

// leafRequirements is what one leaf demands of a machine, reduced from its
// execution spec. Both `doctor` (from GetHeadInfo over the wire) and `leafs list`
// (from the running daemon's management API) reduce a leaf to this before asking
// whether this machine will ever run it, so the two commands cannot answer the
// same question differently (TB-4).
type leafRequirements struct {
	name           string
	needsContainer bool
	nativeCapable  bool
	wasmCapable    bool
	memoryMB       int
	needsGPU       bool
	// diskMB and cpuCores are the leaf's MACHINE requirements
	// (resource_requirements.min_disk_mb / min_cpu_cores), which the head gates
	// dispatch on. Zero means the head did not report them — treated as "no
	// requirement" so an older head degrades to the pre-TB-15 behavior rather
	// than blocking every leaf.
	diskMB   int64
	cpuCores int
	// The GPU requirements, zero/empty when the head did not report them —
	// "unknown", read as no requirement, exactly as diskMB and cpuCores are
	// (TB-21). gpuVRAMMB is matched against the machine's ALLOWED VRAM.
	gpuVRAMMB            int
	gpuType              string
	gpuComputeCapability string
	// The two gpu_required flags kept SEPARATE, because the dispatch predicate keys
	// each GPU sub-gate on a different one and mirroring that exactly is the whole
	// point of this struct: presence and VRAM on either flag, the vendor gate on
	// execution_config.gpu_required alone, compute capability on
	// resource_requirements.gpu_required alone. Collapsing them to one boolean makes
	// the client STRICTER than the head — reporting a machine ineligible for a leaf
	// it would actually be sent, which is the same divergence as the original defect
	// pointing the other way.
	specGPURequired bool
	rrGPURequired   bool
	// raiseDiskGBHint is the RUNNING daemon's answer to "what max_disk_gb lets
	// this machine run this leaf" — computed from live usage and real image
	// cachedness (daemon.DiskAllowanceGBToCover), inputs no recomputation here
	// can supply. 0 when no running daemon provided one; the disk remedy then
	// falls back to covering the leaf requirement alone, which is the number
	// that sent a tester on the 20 → 27 → 43 → 53 GB chase (TB-41).
	raiseDiskGBHint int
}

// leafMachineNeeds carries the machine budgets separately from the execution
// spec, because the field that belongs here (resource_requirements.min_disk_mb)
// sits one struct away from one that must NOT be used (execution_config's
// max_disk_mb, the per-unit sandbox ceiling). A named struct at each call site
// makes picking the wrong one visible instead of positional (TB-15). The GPU
// dimensions joined it for the same reason (TB-21) — min_gpu_vram_mb likewise has
// a near-namesake the client already holds.
type leafMachineNeeds struct {
	diskMB               int64
	cpuCores             int
	gpuVRAMMB            int
	gpuType              string
	gpuComputeCapability string
	// resource_requirements.gpu_required — the OTHER of the two presence flags,
	// ORed with the execution spec's in leafRequirementsFromSpec because that is
	// what the dispatch predicate does. Reading the execution spec's alone reported
	// a GPU-less machine eligible for a leaf that set only this one.
	gpuRequired bool
}

// leafRequirementsFromSpec reduces a leaf's execution spec to its requirements.
// binaries maps platform key -> URL; a "wasm" key means the leaf ships a WASM
// build, any other key means it ships native builds.
func leafRequirementsFromSpec(name, image string, binaries map[string]string, memoryMB int, gpuRequired bool, needs leafMachineNeeds) leafRequirements {
	req := leafRequirements{
		name:                 name,
		needsContainer:       image != "",
		memoryMB:             memoryMB,
		needsGPU:             gpuRequired || needs.gpuRequired,
		specGPURequired:      gpuRequired,
		rrGPURequired:        needs.gpuRequired,
		diskMB:               needs.diskMB,
		cpuCores:             needs.cpuCores,
		gpuVRAMMB:            needs.gpuVRAMMB,
		gpuType:              needs.gpuType,
		gpuComputeCapability: needs.gpuComputeCapability,
	}
	for k := range binaries {
		if strings.EqualFold(k, "wasm") {
			req.wasmCapable = true
		} else {
			req.nativeCapable = true
		}
	}
	return req
}

// runtimeKindOf names the runtime a leaf will be executed with, using the same
// precedence the fetcher uses to pick one (container if an image is set; else
// wasm if a wasm build is present; else native). This is the value `leafs list`
// prints, so what a volunteer reads matches what dispatch would do.
func runtimeKindOf(req leafRequirements) string {
	switch {
	case req.needsContainer:
		return "CONTAINER"
	case req.wasmCapable && !req.nativeCapable:
		return "WASM"
	case req.wasmCapable:
		// Ships both; the fetcher prefers the sandboxed build.
		return "WASM"
	default:
		return "NATIVE"
	}
}

// classifyLeaf decides whether this volunteer can actually run one leaf, applying
// the same gates the daemon applies: runtime availability (a leaf needs the
// container runtime iff its execution spec carries an image), PER-HEAD RUNTIME
// TRUST (the fetcher refuses — and never advertises — CONTAINER/NATIVE work for a
// head the volunteer has not trusted for that runtime; WASM is always trusted),
// the execution_config.max_memory_mb ceiling (the gate that silently fires for a
// default-configured volunteer, #30), the resource_requirements.min_disk_mb and
// min_cpu_cores machine budgets (TB-15), and the four GPU dimensions — presence,
// allowed VRAM, vendor and compute capability (TB-21). Presence is the OR of the
// two gpu_required flags, matching the predicate; the leaf-side flags are combined
// in leafRequirementsFromSpec.
//
// These are every dimension FindNextAssignable matches on, which is the point: a
// leaf this reports eligible is one the head will actually dispatch. Anything
// added to that predicate has to be added here too, or this reports healthy
// machines the head silently refuses — the defect behind both TB-15 and TB-21.
//
// A leaf may be blocked by more than one gate; the first that bites is reported.
// The UNFIXABLE-HARDWARE gates (GPU presence, vendor, compute capability, a
// card too small at any percentage) come before the settings gates (memory,
// disk, cores, VRAM percentage): a settings remedy costs a config change and a
// restart to try, and paying that only to reveal a permanent hardware blocker
// is the trap a real tester walked into — his allowance raise was prompted by
// the disk rows of three GPU leaves his machine can never run (TB-43).
// blocked names the reported dimension, so a caller tallying reasons across a
// head does not have to parse the message.
func classifyLeaf(req leafRequirements, caps volunteerCaps, srv config.ServerConfig) (le leafEligibility, blocked string) {
	head := srv.DisplayName()
	switch {
	case req.needsContainer && !caps.containerUsable:
		return leafEligibility{name: req.name, eligible: false, reason: "needs a container runtime"}, "container"
	case req.needsContainer && !srv.TrustsRuntime("CONTAINER"):
		return leafEligibility{name: req.name, eligible: false, reason:
			fmt.Sprintf("needs the CONTAINER runtime, which you have not trusted this head to run (opt in: lettuce-volunteer heads trust %s container)", head)}, "trust"
	case !req.needsContainer && req.nativeCapable && !req.wasmCapable && !srv.TrustsRuntime("NATIVE"):
		return leafEligibility{name: req.name, eligible: false, reason:
			fmt.Sprintf("needs the NATIVE runtime, which you have not trusted this head to run (opt in: lettuce-volunteer heads trust %s native)", head)}, "trust"
	case req.needsGPU && !caps.hasGPU:
		return leafEligibility{name: req.name, eligible: false, reason: "needs a GPU; none detected/enabled"}, "gpu"
	// Vendor is gated on execution_config.gpu_required ALONE, and compute capability
	// on resource_requirements.gpu_required alone — not on the OR — because that is
	// how the dispatch predicate keys them. A leaf setting only the other flag has
	// that sub-gate skipped head-side, so applying it here would refuse a machine
	// the head would happily send work to. Each is also skipped when the leaf did
	// not state a requirement or the daemon did not report a budget — the
	// unknown-is-not-zero rule, as for disk and cores below.
	case req.specGPURequired && requiresSpecificGPUType(req.gpuType) && len(caps.gpuVendors) > 0 &&
		!containsFold(caps.gpuVendors, req.gpuType):
		return leafEligibility{name: req.name, eligible: false, reason:
			fmt.Sprintf("needs a %s GPU; yours is %s", strings.ToUpper(req.gpuType), strings.Join(caps.gpuVendors, "/"))}, "gpu"
	case req.rrGPURequired && req.gpuComputeCapability != "" && len(caps.gpuComputeCapabilities) > 0 &&
		!containsFold(caps.gpuComputeCapabilities, req.gpuComputeCapability):
		return leafEligibility{name: req.name, eligible: false, reason:
			fmt.Sprintf("needs GPU compute capability %s; yours is %s",
				req.gpuComputeCapability, strings.Join(caps.gpuComputeCapabilities, "/"))}, "gpu"
	// A card too small at 100% is hardware, not a setting, so it is judged here
	// with the other unfixable gates rather than with the VRAM-percentage gate
	// below (same wording either way — vramRemedy detects the too-small card).
	case req.needsGPU && req.gpuVRAMMB > 0 && caps.gpuCardVRAMMB > 0 && req.gpuVRAMMB > caps.gpuCardVRAMMB:
		return leafEligibility{name: req.name, eligible: false, reason: vramBlockedReason(req, caps)}, "vram"
	case req.memoryMB > caps.maxMemoryMB:
		return leafEligibility{name: req.name, eligible: false, reason:
			fmt.Sprintf("needs %d MB memory > your limit %d MB", req.memoryMB, caps.maxMemoryMB)}, "memory"
	// The two budget gates are skipped when the budget itself is unknown (zero).
	// `leafs list` reads these from the RUNNING daemon, so an upgraded binary
	// talking to a daemon started before this change would see 0 and report every
	// disk-requiring leaf blocked — a false alarm worse than the silence it
	// replaces. Absent means unknown here, exactly as it does for the leaf's side
	// of the comparison. Config validation floors max_disk_gb at 1, so a genuinely
	// configured volunteer never lands here.
	case caps.maxDiskMB > 0 && req.diskMB > caps.maxDiskMB:
		raiseGB, sized := diskGBToCover(req.diskMB), ""
		if req.raiseDiskGBHint > 0 {
			// The daemon's number covers today's usage too; the requirement-only
			// number below leaves the volunteer still gated after pasting it (TB-41).
			raiseGB, sized = req.raiseDiskGBHint, " — sized to clear this machine's current usage too"
		}
		return leafEligibility{name: req.name, eligible: false, reason:
			fmt.Sprintf("needs %d MB disk > your allowance %d MB (raise it: lettuce-volunteer config set resource_limits.max_disk_gb %d, then restart%s)",
				req.diskMB, caps.maxDiskMB, raiseGB, sized)}, "disk"
	case caps.maxCPUCores > 0 && req.cpuCores > caps.maxCPUCores:
		return leafEligibility{name: req.name, eligible: false, reason:
			fmt.Sprintf("needs %d CPU cores > your limit %d (raise it: lettuce-volunteer config set resource_limits.max_cpu_cores %d, then restart)",
				req.cpuCores, caps.maxCPUCores, req.cpuCores)}, "cores"
	case req.needsGPU && req.gpuVRAMMB > 0 && caps.maxGPUVRAMMB > 0 && req.gpuVRAMMB > caps.maxGPUVRAMMB:
		return leafEligibility{name: req.name, eligible: false, reason: vramBlockedReason(req, caps)}, "vram"
	default:
		return leafEligibility{name: req.name, eligible: true}, ""
	}
}

// vramBlockedReason renders the VRAM refusal, shared by the too-small-card and
// percentage cases. It names FOUR numbers deliberately: the requirement, what
// this machine offers, the card's actual size and the percentage setting. The
// setting is usually what has to change, and a message naming only the
// shortfall sends people shopping for hardware they already own.
func vramBlockedReason(req leafRequirements, caps volunteerCaps) string {
	return fmt.Sprintf("needs %d MB GPU memory > the %d MB you allow (%s)%s",
		req.gpuVRAMMB, caps.maxGPUVRAMMB, caps.describeVRAMAllowance(),
		vramRemedy(req.gpuVRAMMB, caps))
}

// requiresSpecificGPUType reports whether a leaf's gpu_type constrains the vendor.
// Empty and "ANY" do not — matching the dispatch predicate, which admits both.
func requiresSpecificGPUType(gpuType string) bool {
	t := strings.ToUpper(strings.TrimSpace(gpuType))
	return t != "" && t != "ANY"
}

// describeVRAMAllowance spells out where the allowed figure came from, because
// "you allow 3072 MB" on a machine with a 6 GB card reads as a hardware fault
// rather than as a setting the volunteer chose.
func (c volunteerCaps) describeVRAMAllowance() string {
	if c.gpuCardVRAMMB == 0 || c.gpuVRAMPct == 0 {
		return "your GPU allowance"
	}
	return fmt.Sprintf("%d%% of your %d MB card", c.gpuVRAMPct, c.gpuCardVRAMMB)
}

// vramRemedy names the max_gpu_vram_pct that would clear the requirement on this
// machine's card, rounded UP — a truncated percentage lands the volunteer just
// short, which they would set, restart, and still receive nothing.
//
// When the card itself is too small no percentage helps, and saying so is the
// honest answer: suggesting 100% to someone who would still be refused is the
// mistake the disk story documented, one dimension over.
func vramRemedy(needMB int, caps volunteerCaps) string {
	card := caps.gpuCardVRAMMB
	if card <= 0 {
		return ""
	}
	if needMB > card {
		return "; your card is too small for this leaf whatever percentage you allow"
	}
	pct := (needMB*100 + card - 1) / card
	if pct > 100 {
		pct = 100
	}
	return fmt.Sprintf("; raise it: lettuce-volunteer config set resource_limits.max_gpu_vram_pct %d, then restart", pct)
}

// diskGBToCover converts a leaf's MB disk requirement into the whole max_disk_gb
// value that would cover it, rounding up. The remedy has to be a number the
// volunteer can paste: max_disk_gb is in GB and truncating would suggest a
// setting that still falls short by up to 1023 MB.
func diskGBToCover(mb int64) int {
	return int((mb + 1023) / 1024)
}

// evaluateLeafEligibility runs classifyLeaf over every leaf a head offers,
// tallying each blocking dimension so the caller can print the right remedy.
// Ignoring trust counted leafs the volunteer could never receive as "eligible"
// (PB-5).
//
// daemonGates carries the RUNNING daemon's per-leaf disk-gate verdicts for
// this head, keyed like the leaf names here (slug, falling back to name). When
// a leaf has one, its fetch note QUOTES that verdict and its disk remedy uses
// the daemon's covering allowance — the daemon knows the measured usage and
// image cachedness this code cannot, and recomputing with a guessed input is
// how doctor contradicted a happily-fetching daemon for good (TB-42). nil (or
// a miss) falls back to the conservative local reading, labelled as such.
func evaluateLeafEligibility(leafs []*lettucev1.LeafInfo, caps volunteerCaps, srv config.ServerConfig, daemonGates map[string]*leafsAPIDiskGate) eligibilityResult {
	var res eligibilityResult
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

		// The machine budgets come from resource_requirements, NOT from the
		// execution spec's max_disk_mb — see leafMachineNeeds.
		rr := lf.GetResourceRequirements()
		req := leafRequirementsFromSpec(name, es.GetImage(), es.GetBinaries(), int(es.GetMaxMemoryMb()), es.GetGpuRequired(),
			leafMachineNeeds{
				diskMB:               rr.GetMinDiskMb(),
				cpuCores:             int(rr.GetMinCpuCores()),
				gpuVRAMMB:            int(rr.GetMinGpuVramMb()),
				gpuType:              rr.GetGpuType(),
				gpuComputeCapability: rr.GetGpuComputeCapability(),
				gpuRequired:          rr.GetGpuRequired(),
			})
		gate := daemonGates[name]
		if gate != nil {
			req.raiseDiskGBHint = gate.RaiseToGB
		}
		le, blocked := classifyLeaf(req, caps, srv)
		if gate != nil {
			le.fetchNote = ""
			if gate.Blocked {
				le.fetchNote = "the running daemon is disk-gating this leaf right now: " + gate.Reason
			}
		} else {
			le.fetchNote = localDiskFetchNote(req, caps)
		}
		switch blocked {
		case "container":
			res.containerBlocked++
		case "trust":
			res.trustBlocked++
		case "memory":
			res.memoryBlocked++
		case "disk":
			res.diskBlocked++
		case "cores":
			res.coresBlocked++
		case "vram":
			res.vramBlocked++
		case "gpu":
			res.gpuBlocked++
		default:
			res.eligible++
		}
		res.leaves = append(res.leaves, le)
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

// volunteerGPUBudget reports this machine's GPU capabilities in the terms dispatch
// matches leafs against (TB-21): the largest ALLOWED VRAM across its cards, the
// card and percentage that produced it, and the vendors and compute capabilities
// on offer. Runs the detected GPUs through the same config-application the
// registration path uses (client.ApplyGPUConfig), then reduces them exactly as the
// head does — largest allowed figure wins.
func volunteerGPUBudget() (vramMB, cardVRAMMB, vramPct int, vendors, computeCapabilities []string) {
	for _, g := range client.ApplyGPUConfig(cfg, detectGPUsFunc()) {
		if eff := int(g.GetVramMb()) * int(g.GetMaxVramPct()) / 100; eff > vramMB {
			vramMB, cardVRAMMB, vramPct = eff, int(g.GetVramMb()), int(g.GetMaxVramPct())
		}
		if v := strings.ToUpper(strings.TrimSpace(g.GetVendor())); v != "" {
			vendors = append(vendors, v)
		}
		if cc := g.GetComputeCapability(); cc != "" {
			computeCapabilities = append(computeCapabilities, cc)
		}
	}
	return vramMB, cardVRAMMB, vramPct, vendors, computeCapabilities
}

func statusOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
