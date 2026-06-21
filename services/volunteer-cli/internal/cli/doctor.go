package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	checkDataDir(rep, cfg.DataDir)
	checkIdentity(rep, cfg.KeyFile, cfg.PubKeyFile)
	checkDisk(rep, cfg.DataDir, cfg.ResourceLimits.MaxDiskGB)
	containerUsable := checkContainer(rep, logger)
	checkDaemon(rep, cfg.DataDir)

	advertised := []string{"NATIVE", "WASM"}
	if containerUsable {
		advertised = append(advertised, "CONTAINER")
	}
	rep.add(docInfo, "runtimes", fmt.Sprintf("this volunteer can run: %v", advertised), "")

	fmt.Fprintln(out)
	fmt.Fprintf(out, "Heads (%d configured):\n", len(cfg.Servers))
	checkHeads(cmd.Context(), rep, logger, containerUsable)

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
		rep.add(docFail, "identity", fmt.Sprintf("keypair present but unreadable (%v)", err),
			"re-run: lettuce-volunteer init")
		return
	}
	rep.add(docOK, "identity", "keypair present and valid", "")
}

func checkDisk(rep *doctorReport, dataDir string, maxDiskGB int) {
	requiredMB := maxDiskGB * 1024
	if requiredMB <= 0 {
		requiredMB = 1024
	}
	availableMB := client.DiskAvailableMB(dataDir)
	detail := fmt.Sprintf("%d MB free on %s (work needs %d MB free)", availableMB, dataDir, requiredMB)
	if availableMB > 0 && availableMB < int64(requiredMB) {
		rep.add(docFail, "disk space", detail,
			"free space, lower resource_limits.max_disk_gb, or use --data-dir on a roomier volume")
		return
	}
	rep.add(docOK, "disk space", detail, "")
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
	return true
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

func checkHeads(ctx context.Context, rep *doctorReport, logger *slog.Logger, containerUsable bool) {
	if len(cfg.Servers) == 0 {
		rep.add(docFail, "(none)", "no heads configured",
			"run: lettuce-volunteer attach --server <host>")
		return
	}

	reachable := 0
	for _, srv := range cfg.Servers {
		if checkOneHead(ctx, rep, logger, srv, containerUsable) {
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
func checkOneHead(ctx context.Context, rep *doctorReport, logger *slog.Logger, srv config.ServerConfig, containerUsable bool) bool {
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

	total, eligible, containerBlocked := countEligibleLeafs(resp.GetLeafs(), containerUsable)
	detail := fmt.Sprintf("reachable — server %s, db %s; eligible for %d of %d leafs",
		st.GetVersion(), statusOrUnknown(st.GetDatabaseStatus()), eligible, total)

	if total > 0 && eligible == 0 {
		remedy := "this head has no leafs this volunteer can run"
		if containerBlocked == total && !containerUsable {
			remedy = "every leaf here needs a container runtime — fix the container check above, or attach a head with native leafs"
		}
		rep.add(docWarn, name, detail, remedy)
		return true
	}
	rep.add(docOK, name, detail, "")
	return true
}

// countEligibleLeafs counts how many leafs the volunteer can run. A leaf needs
// the container runtime iff its execution spec carries an image; everything else
// runs on the always-present native/wasm runtimes.
func countEligibleLeafs(leafs []*lettucev1.LeafInfo, containerUsable bool) (total, eligible, containerBlocked int) {
	for _, lf := range leafs {
		total++
		es := lf.GetExecutionSpec()
		needsContainer := es != nil && es.GetImage() != ""
		if needsContainer && !containerUsable {
			containerBlocked++
			continue
		}
		eligible++
	}
	return total, eligible, containerBlocked
}

func statusOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
