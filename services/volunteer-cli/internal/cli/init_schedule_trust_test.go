package cli

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	rtdetect "github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// withContainerBackend makes container detection report a working Docker for the
// duration of the test, so init's container step and the trust consent's
// CONTAINER question are both offered deterministically.
func withContainerBackend(t *testing.T) {
	t.Helper()
	orig := detectContainerBackendFunc
	detectContainerBackendFunc = func(bundledPath string) rtdetect.BackendInfo {
		return rtdetect.BackendInfo{Backend: rtdetect.BackendDocker}
	}
	t.Cleanup(func() { detectContainerBackendFunc = orig })
}

// runInteractiveInit feeds answers to `init` one prompt at a time and returns the
// config it wrote plus everything it printed.
//
// Prompt order for a machine with a container backend and no GPU:
//
//	cpu cores, memory MB, disk GB, scheduling mode, [scheduling answers],
//	leaf mode, enable container?, enable thermal?, server host,
//	[trust: container?, native?]
//
// Any prompt past the supplied answers reads EOF and takes its default.
func runInteractiveInit(t *testing.T, dir string, answers []string) (*config.Config, string) {
	t.Helper()
	cfgFile := filepath.Join(dir, "config.yaml")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	go func() {
		for _, a := range answers {
			w.Write([]byte(a + "\n"))
		}
		w.Close()
	}()

	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--config", cfgFile, "--data-dir", dir})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("init failed: %v", err)
		}
	})

	loaded, err := config.Load(cfgFile)
	if err != nil {
		t.Fatalf("loading written config: %v", err)
	}
	return loaded, out
}

// TestInitScheduledWritesAWindowNotACronExpression is the TB-3 root regression:
// init's "scheduled" step asked for a bare "Cron expression" and stored whatever
// was typed. Anything that was not a 5-field cron — a tester typed the `schedule`
// command's own flags — left a volunteer who looked configured, never ran, and was
// told nothing. Init now speaks the same --from/--to/--days language as `schedule
// set`, so there is no unvalidated string to store.
func TestInitScheduledWritesAWindowNotACronExpression(t *testing.T) {
	withContainerBackend(t)
	dir := t.TempDir()

	loaded, out := runInteractiveInit(t, dir, []string{
		"", "", "", // cpu, memory, disk
		"scheduled",                 // scheduling mode
		"19:00", "07:00", "mon-fri", // the window
		"", "", "", "", // leaf mode, container, thermal, server host (skip)
	})

	if loaded.Scheduling.Mode != "SCHEDULED" {
		t.Fatalf("Scheduling.Mode = %q, want SCHEDULED", loaded.Scheduling.Mode)
	}
	if loaded.Scheduling.CronExpression != "" {
		t.Errorf("init wrote cron_expression = %q; it must no longer produce cron expressions", loaded.Scheduling.CronExpression)
	}
	if len(loaded.Scheduling.ScheduleRanges) != 1 {
		t.Fatalf("schedule_ranges = %v, want exactly the one window", loaded.Scheduling.ScheduleRanges)
	}
	got := loaded.Scheduling.ScheduleRanges[0]
	if got.StartHour != 19 || got.EndHour != 7 || !reflect.DeepEqual(got.Days, []int{0, 1, 2, 3, 4}) {
		t.Errorf("window = %+v, want 19->7 on mon-fri", got)
	}
	// And what init wrote must be a schedule the daemon will actually act on.
	if err := loaded.Scheduling.NeverRuns(); err != nil {
		t.Errorf("init produced a schedule that can never run: %v", err)
	}
	if !strings.Contains(out, "19:00") {
		t.Errorf("init did not confirm the window it recorded:\n%s", out)
	}
}

// TestInitScheduledRejectsAnUnparseableWindowAndReasks: the window prompts refuse
// bad input where it is entered instead of storing it, which is the behavior the
// old free-text cron prompt lacked.
func TestInitScheduledRejectsAnUnparseableWindowAndReasks(t *testing.T) {
	withContainerBackend(t)
	dir := t.TempDir()

	loaded, out := runInteractiveInit(t, dir, []string{
		"", "", "",
		"scheduled",
		"--from 06:00", "04:00", "mon-fri", // the tester's shape of answer: refused
		"20:00", "06:00", "sat,sun", // second attempt: accepted
		"", "", "", "",
	})

	if !strings.Contains(out, "try that again") {
		t.Errorf("init accepted an unparseable window without re-asking:\n%s", out)
	}
	if loaded.Scheduling.CronExpression != "" {
		t.Errorf("cron_expression = %q, want empty", loaded.Scheduling.CronExpression)
	}
	if len(loaded.Scheduling.ScheduleRanges) != 1 {
		t.Fatalf("schedule_ranges = %v, want the one accepted window", loaded.Scheduling.ScheduleRanges)
	}
	if got := loaded.Scheduling.ScheduleRanges[0]; got.StartHour != 20 || got.EndHour != 6 {
		t.Errorf("window = %+v, want 20->6", got)
	}
}

// TestInitAttachedHeadGetsTheTrustConsent is the TB-7 regression: a head entered
// at init's server step never got the "a head is a trust domain" consent that
// `attach` gives. Its entry was written with an empty trusted_runtimes — an
// un-asked, unannounced "WASM only" — so against a head whose leafs are all
// CONTAINER or NATIVE the volunteer fetched nothing at all, indefinitely, with no
// prompt, no warning and no steer to `heads trust`.
func TestInitAttachedHeadGetsTheTrustConsent(t *testing.T) {
	withContainerBackend(t)
	dir := t.TempDir()

	loaded, out := runInteractiveInit(t, dir, []string{
		"", "", "", // cpu, memory, disk
		"",                 // scheduling mode -> always
		"",                 // leaf mode
		"y",                // enable container tasks
		"",                 // thermal
		"head.example.com", // server host
		"y",                // trust: CONTAINER
		"y",                // trust: NATIVE
	})

	if !strings.Contains(out, "trust domain") {
		t.Errorf("init attached a head without asking the trust consent:\n%s", out)
	}
	if len(loaded.Servers) != 1 {
		t.Fatalf("servers = %v, want the one head", loaded.Servers)
	}
	got := loaded.Servers[0].TrustedRuntimes
	want := []string{"CONTAINER", "NATIVE"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("trusted_runtimes = %v, want %v (what was answered must be what is persisted)", got, want)
	}
}

// TestInitAttachedHeadDecliningTrustIsToldWhereToChangeIt: declining is a valid
// answer, but landing on WASM-only silently is the half of TB-7 that left a
// volunteer with an empty work buffer and no explanation.
func TestInitAttachedHeadDecliningTrustIsToldWhereToChangeIt(t *testing.T) {
	withContainerBackend(t)
	dir := t.TempDir()

	loaded, out := runInteractiveInit(t, dir, []string{
		"", "", "",
		"", "", "y", "",
		"head.example.com",
		"n", // trust: no CONTAINER
		"n", // trust: no NATIVE
	})

	if got := loaded.Servers[0].TrustedRuntimes; len(got) != 0 {
		t.Errorf("trusted_runtimes = %v, want the empty (WASM-only) choice", got)
	}
	// Non-nil, so a later load does not treat the choice as a legacy blank and
	// re-seed it from available_runtimes (PB-28).
	if loaded.Servers[0].TrustedRuntimes == nil {
		t.Error("trusted_runtimes is nil; an explicit decline must persist explicitly")
	}
	if !strings.Contains(out, "heads trust") {
		t.Errorf("a WASM-only outcome was not explained:\n%s", out)
	}
}

// TestInitNonInteractiveTrustFlag covers the desktop/scripted path of TB-7: there
// is nobody to prompt, so --trust IS the consent. Without the flag the head lands
// WASM-only, which is now an informed default rather than an un-asked one.
func TestInitNonInteractiveTrustFlag(t *testing.T) {
	withContainerBackend(t)

	t.Run("flag is honored", func(t *testing.T) {
		dir := t.TempDir()
		cfgFile := filepath.Join(dir, "config.yaml")
		cmd := newRootCmd()
		cmd.SetArgs([]string{"init", "--config", cfgFile, "--data-dir", dir,
			"--server", "head.example.com", "--trust", "container,native"})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		captureStdout(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Fatalf("init failed: %v", err)
			}
		})

		loaded, err := config.Load(cfgFile)
		if err != nil {
			t.Fatalf("loading config: %v", err)
		}
		if len(loaded.Servers) != 1 {
			t.Fatalf("servers = %v, want one", loaded.Servers)
		}
		if got, want := loaded.Servers[0].TrustedRuntimes, []string{"CONTAINER", "NATIVE"}; !reflect.DeepEqual(got, want) {
			t.Errorf("trusted_runtimes = %v, want %v", got, want)
		}
	})

	t.Run("unknown runtime is refused", func(t *testing.T) {
		dir := t.TempDir()
		cfgFile := filepath.Join(dir, "config.yaml")
		cmd := newRootCmd()
		cmd.SetArgs([]string{"init", "--config", cfgFile, "--data-dir", dir,
			"--server", "head.example.com", "--trust", "gpu"})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		var err error
		captureStdout(t, func() { err = cmd.Execute() })
		if err == nil {
			t.Error("init accepted --trust gpu; an unknown runtime must be refused, not silently dropped")
		}
	})
}
