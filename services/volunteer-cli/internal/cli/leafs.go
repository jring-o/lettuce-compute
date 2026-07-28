package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/management"
	"github.com/spf13/cobra"
)

func newLeafsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "leafs",
		Short: "Manage leaf preferences (list, enable, disable, weight, reset)",
		Args:  noStrayArgs,
		// See newHeadsCmd: a non-runnable parent never reaches its Args
		// constraint, so the help is served from RunE instead (TB-6).
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	cmd.AddCommand(
		newLeafsListCmd(),
		newLeafsEnableCmd(),
		newLeafsDisableCmd(),
		newLeafsWeightCmd(),
		newLeafsResetCmd(),
	)

	return cmd
}

// --- leafs list ---

func newLeafsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available leafs from all connected servers",
		RunE:  runLeafsList,
	}
}

// leafsAPIResponse is the shape of GET /api/v1/heads from the management API.
type leafsAPIResponse struct {
	Heads   []leafsAPIHead  `json:"heads"`
	Machine leafsAPIMachine `json:"machine"`
}

// leafsAPIMachine is what the RUNNING daemon says this machine can do. Taking it
// from the daemon rather than re-deriving it here means the table answers for the
// configuration actually loaded, and costs no second hardware probe (TB-4).
type leafsAPIMachine struct {
	Runtimes    []string `json:"runtimes"`
	HasGPU      bool     `json:"has_gpu"`
	MaxMemoryMB int      `json:"max_memory_mb"`
	MaxDiskMB   int64    `json:"max_disk_mb"`
	MaxCPUCores int      `json:"max_cpu_cores"`
	// The GPU budgets (TB-21). MaxGPUVRAMMB is the allowed share, not the card.
	MaxGPUVRAMMB           int      `json:"max_gpu_vram_mb"`
	GPUCardVRAMMB          int      `json:"gpu_card_vram_mb"`
	GPUVRAMPct             int      `json:"gpu_vram_pct"`
	GPUVendors             []string `json:"gpu_vendors"`
	GPUComputeCapabilities []string `json:"gpu_compute_capabilities"`
}

type leafsAPIHead struct {
	Name string `json:"name"`
	// GRPCAddress identifies which configured server this head is, independent of
	// the display name (which the head itself supplies and can change).
	GRPCAddress string         `json:"grpc_address"`
	Weight      int            `json:"weight"`
	Leafs       []leafsAPILeaf `json:"leafs"`
	// LeafsRefreshedAt is when this head's QUEUED/VOLUNTEERS/HOSTS figures were
	// last fetched. Zero when nothing has been cached yet (TB-14).
	LeafsRefreshedAt time.Time `json:"leafs_refreshed_at"`
}

type leafsAPILeaf struct {
	Slug             string                 `json:"slug"`
	Name             string                 `json:"name"`
	State            string                 `json:"state"`
	QueuedWorkUnits  int                    `json:"queued_work_units"`
	ActiveVolunteers int                    `json:"active_volunteers"`
	ActiveHosts      int                    `json:"active_hosts"`
	EffectiveWeight  int                    `json:"effective_weight"`
	Enabled          bool                   `json:"enabled"`
	ExecutionSpec    *leafsAPIExecutionSpec `json:"execution_spec"`
	// ResourceRequirements is what the head requires of this MACHINE before it
	// dispatches this leaf. nil from a head too old to report it (TB-15).
	ResourceRequirements *leafsAPIResourceRequirements `json:"resource_requirements"`
	Failures             *leafsAPILeafFailures         `json:"failures"`
}

// leafsAPIExecutionSpec carries the fields that decide a leaf's runtime and
// whether this machine meets its requirements.
type leafsAPIExecutionSpec struct {
	Binaries    map[string]string `json:"binaries"`
	Image       string            `json:"image"`
	GPURequired bool              `json:"gpu_required"`
	MaxMemoryMB int32             `json:"max_memory_mb"`
}

// leafsAPIResourceRequirements carries the machine budgets the head's dispatch
// gate matches against. Deliberately separate from leafsAPIExecutionSpec, whose
// max_disk_mb is a different number and the wrong one to gate on (TB-15).
type leafsAPIResourceRequirements struct {
	MinDiskMB   int64 `json:"min_disk_mb"`
	MinCPUCores int32 `json:"min_cpu_cores"`
	// GPU dimensions (TB-21). MinGPUVRAMMB is matched against the machine's
	// allowed VRAM, not its card capacity.
	MinGPUVRAMMB         int32  `json:"min_gpu_vram_mb"`
	GPUType              string `json:"gpu_type"`
	GPUComputeCapability string `json:"gpu_compute_capability"`
	GPURequired          bool   `json:"gpu_required"`
}

// leafsAPILeafFailures is a leaf's local failure record (TB-10).
type leafsAPILeafFailures struct {
	ConsecutiveFailures int  `json:"consecutive_failures"`
	TotalFailures       int  `json:"total_failures"`
	Paused              bool `json:"paused"`
}

func runLeafsList(cmd *cobra.Command, args []string) error {
	if len(cfg.Servers) == 0 {
		fmt.Println("No servers configured. Run `lettuce-volunteer attach --server <host>` first.")
		return nil
	}

	// Query the running daemon's local management API for live per-head state.
	// On any failure, fall back to config-only info but name the REAL reason
	// instead of always claiming "not running" (TODO #21).
	resp, err := fetchHeadsFromAPI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Showing config-only info (%v).\n\n", err)
		return printLeafsFromConfig()
	}

	printLeafsTable(os.Stdout, resp, cfg.Servers)
	return nil
}

// printLeafsTable renders the live leaf table.
//
// STATE is the HEAD's leaf lifecycle state and ENABLED is the volunteer's own
// preference toggle; neither says whether this machine will ever fetch the leaf.
// Before TB-4 that was the whole table, so a NATIVE leaf on a container-only
// machine displayed exactly like one that was being computed — "active ✓" — and
// a tester reasonably read it as "my machine runs this". RUNTIME and WILL FETCH
// answer the question the other columns look like they are answering, using the
// same gates the daemon applies, with the blocking reason spelled out beneath.
func printLeafsTable(out io.Writer, resp *leafsAPIResponse, servers []config.ServerConfig) {
	caps := volunteerCaps{
		maxMemoryMB:            resp.Machine.MaxMemoryMB,
		containerUsable:        containsFold(resp.Machine.Runtimes, "container"),
		hasGPU:                 resp.Machine.HasGPU,
		maxDiskMB:              resp.Machine.MaxDiskMB,
		maxCPUCores:            resp.Machine.MaxCPUCores,
		maxGPUVRAMMB:           resp.Machine.MaxGPUVRAMMB,
		gpuCardVRAMMB:          resp.Machine.GPUCardVRAMMB,
		gpuVRAMPct:             resp.Machine.GPUVRAMPct,
		gpuVendors:             resp.Machine.GPUVendors,
		gpuComputeCapabilities: resp.Machine.GPUComputeCapabilities,
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "SERVER\tSLUG\tNAME\tRUNTIME\tSTATE\tQUEUED\tVOLUNTEERS\tHOSTS\tWEIGHT\tENABLED\tWILL FETCH\n")

	// Blocking reasons are collected and printed under the table so the rows stay
	// aligned and scannable.
	var notes []string
	// Per-head snapshot ages, same treatment: an eleventh column would repeat the
	// same value on every row of a head (TB-14).
	var ages []string
	for _, h := range resp.Heads {
		srv, known := serverConfigFor(servers, h)
		if len(h.Leafs) > 0 {
			ages = append(ages, "  "+describeSnapshotAge(h.Name, h.LeafsRefreshedAt, time.Now()))
		}
		for _, l := range h.Leafs {
			spec := l.ExecutionSpec
			if spec == nil {
				spec = &leafsAPIExecutionSpec{}
			}
			label := l.Slug
			if label == "" {
				label = l.Name
			}
			// Machine budgets come from resource_requirements, never from the
			// execution spec's max_disk_mb — see leafMachineNeeds.
			var needs leafMachineNeeds
			if rr := l.ResourceRequirements; rr != nil {
				needs = leafMachineNeeds{
					diskMB:               rr.MinDiskMB,
					cpuCores:             int(rr.MinCPUCores),
					gpuVRAMMB:            int(rr.MinGPUVRAMMB),
					gpuType:              rr.GPUType,
					gpuComputeCapability: rr.GPUComputeCapability,
					gpuRequired:          rr.GPURequired,
				}
			}
			req := leafRequirementsFromSpec(label, spec.Image, spec.Binaries, int(spec.MaxMemoryMB), spec.GPURequired, needs)

			enabled := "✓"
			if !l.Enabled {
				enabled = "✗"
			}

			willFetch, note := willFetchVerdict(req, caps, srv, known, l)
			if note != "" {
				notes = append(notes, fmt.Sprintf("  %s / %s: %s", h.Name, label, note))
			}

			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\n",
				h.Name, l.Slug, l.Name, runtimeKindOf(req), l.State,
				l.QueuedWorkUnits, l.ActiveVolunteers, l.ActiveHosts,
				l.EffectiveWeight, enabled, willFetch,
			)
		}
	}
	w.Flush()

	if len(ages) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "QUEUED / VOLUNTEERS / HOSTS come from the head, as of:")
		for _, a := range ages {
			fmt.Fprintln(out, a)
		}
	}

	if len(notes) > 0 {
		fmt.Fprintln(out)
		for _, n := range notes {
			fmt.Fprintln(out, n)
		}
	}
}

// staleSnapshotAfter is when a leaf snapshot is old enough to explain itself
// rather than just date itself. There is no refresh timer to compare against —
// the daemon refreshes only when it goes looking for work — so this is a
// judgement about when a reader would be misled, set well above the 5-minute
// minimum refresh interval so ordinary fetching never trips it.
const staleSnapshotAfter = 15 * time.Minute

// describeSnapshotAge renders one head's leaf-figure vintage. The age is
// unbounded by design: the refresh lives inside the fetch path, so a machine that
// is not asking for work carries the same numbers indefinitely. Saying so turns
// two wrong readings a tester reached unaided — a one-minute refresh clock, and
// the head mis-grouping hosts — into a fact the table states itself (TB-14).
func describeSnapshotAge(head string, refreshedAt, now time.Time) string {
	if refreshedAt.IsZero() {
		return fmt.Sprintf("%s — never refreshed; the daemon has not fetched head info yet", head)
	}
	age := now.Sub(refreshedAt)
	if age < 0 {
		age = 0
	}
	line := fmt.Sprintf("%s — %s (%s ago)", head, refreshedAt.Format("15:04:05"), roundAge(age))
	if age >= staleSnapshotAfter {
		// The cause is itself the answer to the question this reader is usually
		// about to ask, so it is worth the extra clause.
		line += "; this machine has not asked that head for work since, so the figures are frozen, not wrong"
	}
	return line
}

// roundAge renders a duration the way someone reading a table wants it: whole
// seconds under a minute, whole minutes under an hour, then hours and minutes.
func roundAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

// willFetchVerdict answers, for one leaf, whether this machine will fetch it —
// and when it will not, why. The order matters: a leaf the volunteer disabled is
// reported as their own choice rather than as a capability problem, and a leaf
// paused by the failure breaker is reported as failing rather than as ineligible,
// because those call for different actions.
func willFetchVerdict(req leafRequirements, caps volunteerCaps, srv config.ServerConfig, known bool, l leafsAPILeaf) (verdict, note string) {
	if !known {
		// The daemon reported a head this config does not list (renamed or
		// detached mid-run). Say so rather than guessing at its trust settings.
		return "?", "this head is not in your config.yaml, so per-head runtime trust could not be checked"
	}
	if !l.Enabled {
		return "no", "you disabled this leaf (`lettuce-volunteer leafs enable " + l.Slug + "`)"
	}
	if !strings.EqualFold(l.State, "ACTIVE") {
		return "no", fmt.Sprintf("the head has this leaf in state %s, so it dispatches no work", l.State)
	}
	le, _ := classifyLeaf(req, caps, srv)
	if !le.eligible {
		return "no", le.reason
	}
	if l.Failures != nil && l.Failures.Paused {
		return "paused", fmt.Sprintf("this leaf's work reached this machine and failed %d times in a row, so requests for it are paused; it retries automatically (see `lettuce-volunteer status`)",
			l.Failures.ConsecutiveFailures)
	}
	if l.Failures != nil && l.Failures.TotalFailures > 0 {
		return "yes", fmt.Sprintf("note: %d of this leaf's units have failed on this machine (see `lettuce-volunteer status`)", l.Failures.TotalFailures)
	}
	return "yes", ""
}

// serverConfigFor matches a head from the management API to its configured
// server. It keys on the gRPC address, not the display name: the name in the API
// response is the one the HEAD supplies via GetHeadInfo, which need not equal the
// local config's name for it.
func serverConfigFor(servers []config.ServerConfig, h leafsAPIHead) (config.ServerConfig, bool) {
	for _, srv := range servers {
		if h.GRPCAddress != "" && srv.GRPCAddress == h.GRPCAddress {
			return srv, true
		}
	}
	for _, srv := range servers {
		if srv.DisplayName() == h.Name {
			return srv, true
		}
	}
	return config.ServerConfig{}, false
}

// containsFold reports whether s contains val, case-insensitively.
func containsFold(s []string, val string) bool {
	for _, v := range s {
		if strings.EqualFold(v, val) {
			return true
		}
	}
	return false
}

// fetchHeadsFromAPI queries the running daemon's local management API for live
// per-head leaf state. It reads the port AND the bearer token from daemon.json and
// authenticates the request: the management API rejects unauthenticated calls with
// 401 (which is why this command previously ALWAYS showed the config-only fallback
// even while the daemon was running — TODO #21). The request targets
// 127.0.0.1:<port> so it also satisfies the management API's Host-header allowlist.
// Errors are returned verbatim so the caller can show the real reason.
func fetchHeadsFromAPI() (*leafsAPIResponse, error) {
	info, err := management.ReadDaemonInfo(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("daemon not running (no daemon.json)")
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/heads", info.Port)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+info.Token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("management API returned status %d", resp.StatusCode)
	}

	var result leafsAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func printLeafsFromConfig() error {
	// This table is the volunteer's stored PREFERENCES, not live head state. It
	// cannot show a leaf's runtime or whether this machine would fetch it —
	// both need the daemon — so it says so rather than letting the columns it
	// does have be read as the whole answer (TB-4).
	fmt.Println("Without a running daemon this shows your saved leaf preferences only —")
	fmt.Println("not each leaf's runtime, nor whether this machine would actually fetch it.")
	fmt.Println("Start the daemon (`lettuce-volunteer start`) for that, or run `lettuce-volunteer doctor`.")
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "SERVER\tMODE\tWEIGHT\tENABLED SLUGS\tDISABLED SLUGS\n")
	for _, srv := range cfg.Servers {
		name := srv.DisplayName()
		mode := srv.LeafPreferences.Mode
		if mode == "" {
			mode = "ALL"
		}
		weight := srv.Weight
		if weight <= 0 {
			weight = 100
		}
		enabled := "-"
		if len(srv.LeafPreferences.Enabled) > 0 {
			enabled = fmt.Sprintf("%v", srv.LeafPreferences.Enabled)
		}
		disabled := "-"
		if len(srv.LeafPreferences.Disabled) > 0 {
			disabled = fmt.Sprintf("%v", srv.LeafPreferences.Disabled)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", name, mode, weight, enabled, disabled)
	}
	w.Flush()
	return nil
}

// --- leafs enable ---

func newLeafsEnableCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enable <slug>",
		Short: "Enable a leaf on a server",
		Args:  cobra.ExactArgs(1),
		RunE:  runLeafsEnable,
	}
	cmd.Flags().String("server", "", "server name (applies to all if omitted)")
	return cmd
}

func runLeafsEnable(cmd *cobra.Command, args []string) error {
	slug := args[0]
	serverFilter, _ := cmd.Flags().GetString("server")

	modified := false
	for i := range cfg.Servers {
		name := cfg.Servers[i].DisplayName()
		if serverFilter != "" && name != serverFilter {
			continue
		}

		lp := &cfg.Servers[i].LeafPreferences
		mode := lp.Mode
		if mode == "" {
			mode = "ALL"
		}

		switch mode {
		case "ALL":
			// Switch to BLOCKLIST and ensure slug is not in Disabled.
			lp.Mode = "BLOCKLIST"
			lp.Disabled = removeFromSlice(lp.Disabled, slug)
		case "BLOCKLIST":
			lp.Disabled = removeFromSlice(lp.Disabled, slug)
		case "SPECIFIC":
			if !contains(lp.Enabled, slug) {
				lp.Enabled = append(lp.Enabled, slug)
			}
		}
		modified = true
		fmt.Printf("Enabled leaf %q on server %q (mode: %s)\n", slug, name, lp.Mode)
	}

	if !modified {
		return fmt.Errorf("no matching server found")
	}

	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	return nil
}

// --- leafs disable ---

func newLeafsDisableCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "disable <slug>",
		Short: "Disable a leaf on a server",
		Args:  cobra.ExactArgs(1),
		RunE:  runLeafsDisable,
	}
	cmd.Flags().String("server", "", "server name (applies to all if omitted)")
	return cmd
}

func runLeafsDisable(cmd *cobra.Command, args []string) error {
	slug := args[0]
	serverFilter, _ := cmd.Flags().GetString("server")

	modified := false
	for i := range cfg.Servers {
		name := cfg.Servers[i].DisplayName()
		if serverFilter != "" && name != serverFilter {
			continue
		}

		lp := &cfg.Servers[i].LeafPreferences
		mode := lp.Mode
		if mode == "" {
			mode = "ALL"
		}

		switch mode {
		case "ALL":
			lp.Mode = "BLOCKLIST"
			if !contains(lp.Disabled, slug) {
				lp.Disabled = append(lp.Disabled, slug)
			}
		case "BLOCKLIST":
			if !contains(lp.Disabled, slug) {
				lp.Disabled = append(lp.Disabled, slug)
			}
		case "SPECIFIC":
			lp.Enabled = removeFromSlice(lp.Enabled, slug)
		}
		modified = true
		fmt.Printf("Disabled leaf %q on server %q (mode: %s)\n", slug, name, lp.Mode)
	}

	if !modified {
		return fmt.Errorf("no matching server found")
	}

	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	return nil
}

// --- leafs weight ---

func newLeafsWeightCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "weight <slug> <weight>",
		Short: "Set custom weight for a leaf",
		Args:  cobra.ExactArgs(2),
		RunE:  runLeafsWeight,
	}
	cmd.Flags().String("server", "", "server name (applies to all if omitted)")
	return cmd
}

func runLeafsWeight(cmd *cobra.Command, args []string) error {
	slug := args[0]
	weight, err := strconv.Atoi(args[1])
	if err != nil || weight <= 0 {
		return fmt.Errorf("weight must be a positive integer, got %q", args[1])
	}
	serverFilter, _ := cmd.Flags().GetString("server")

	modified := false
	for i := range cfg.Servers {
		name := cfg.Servers[i].DisplayName()
		if serverFilter != "" && name != serverFilter {
			continue
		}

		lp := &cfg.Servers[i].LeafPreferences
		if lp.Weights == nil {
			lp.Weights = make(map[string]int)
		}
		// An absent key reads as Go's zero value, but an unweighted leaf is not
		// weighted 0 — the daemon selects it at the default 100. Printing the
		// raw map value made a first-time `leafs weight` claim the leaf had
		// previously been ignored (TB-9); normalize the way `heads list` does.
		oldWeight := lp.Weights[slug]
		if oldWeight <= 0 {
			oldWeight = 100
		}
		lp.Weights[slug] = weight
		modified = true
		fmt.Printf("Set weight for leaf %q on server %q: %d → %d\n", slug, name, oldWeight, weight)
	}

	if !modified {
		return fmt.Errorf("no matching server found")
	}

	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	return nil
}

// --- leafs reset ---

func newLeafsResetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset leaf preferences to researcher defaults",
		RunE:  runLeafsReset,
	}
	cmd.Flags().String("server", "", "server name (applies to all if omitted)")
	return cmd
}

func runLeafsReset(cmd *cobra.Command, args []string) error {
	serverFilter, _ := cmd.Flags().GetString("server")

	modified := false
	for i := range cfg.Servers {
		name := cfg.Servers[i].DisplayName()
		if serverFilter != "" && name != serverFilter {
			continue
		}

		cfg.Servers[i].LeafPreferences = config.LeafPreferences{Mode: "ALL"}
		modified = true
		fmt.Printf("Reset leaf preferences for server %q to ALL (researcher defaults)\n", name)
	}

	if !modified {
		return fmt.Errorf("no matching server found")
	}

	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	return nil
}

// --- helpers ---

func removeFromSlice(s []string, val string) []string {
	var result []string
	for _, v := range s {
		if v != val {
			result = append(result, v)
		}
	}
	return result
}

func contains(s []string, val string) bool {
	for _, v := range s {
		if v == val {
			return true
		}
	}
	return false
}
