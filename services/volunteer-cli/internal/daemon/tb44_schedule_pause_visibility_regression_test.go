package daemon

import (
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
)

// TB-44 regression tests: two code paths pause compute for a schedule window,
// and only one recorded it. The resource monitor's pause signal sets
// d.paused/pauseReason, but the main loop's schedule gate
// (waitForScheduleActive) parks and suspends slots while setting NEITHER — and
// the gate usually wins the race, always on a daemon booted inside a closed
// window (the tester's case: `shouldFetch: scheduler says don't run` at 1 Hz
// while `status` showed state "active" and no pause reason). The status
// accessors must therefore consult live scheduler policy, not just which
// goroutine happened to consume a pause signal.

// closedScheduler builds a SCHEDULED-mode scheduler whose one range matches no
// days, so ShouldRun() is false at any time — the boot-inside-a-closed-window
// state without clock injection.
func closedScheduler(t *testing.T) *resource.Scheduler {
	t.Helper()
	return resource.NewScheduler(&config.Scheduling{
		Mode:           "SCHEDULED",
		ScheduleRanges: []config.ScheduleRange{{Days: []int{}, StartHour: 17, EndHour: 19}},
	}, newTestLogger())
}

// TestTB44_GateParkVisibleToStatusAccessors is the core regression: with the
// scheduler forbidding work — the state in which the gate parks the main loop
// and suspends every slot — IsPaused and PauseReason must say so, whether or
// not the monitor's pause signal was ever drained.
func TestTB44_GateParkVisibleToStatusAccessors(t *testing.T) {
	d := &Daemon{scheduler: closedScheduler(t)}

	if !d.scheduler.ShouldRun() {
		// Sanity only — the range matches no days.
	} else {
		t.Fatal("scheduler unexpectedly active — state does not reproduce the defect")
	}

	if !d.IsPaused() {
		t.Error("IsPaused = false while the scheduler forbids running — the gate-park is invisible to status (TB-44)")
	}
	if got := d.PauseReason(); got != "scheduled" {
		t.Errorf("PauseReason = %q, want %q — status cannot explain the schedule window (TB-44)", got, "scheduled")
	}
}

// TestTB44_OpenScheduleNotPaused is the healthy half: an open schedule (and no
// signal-driven pause) must not read as paused.
func TestTB44_OpenScheduleNotPaused(t *testing.T) {
	d := &Daemon{scheduler: resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, newTestLogger())}

	if d.IsPaused() {
		t.Error("IsPaused = true with an ALWAYS schedule and no pause signal")
	}
	if got := d.PauseReason(); got != "" {
		t.Errorf("PauseReason = %q, want empty", got)
	}
}

// TestTB44_UserPauseOutranksSchedule: a user pause is reported as "user" even
// inside a closed window — the user's own action is the more specific answer,
// and it is the one `resume` undoes.
func TestTB44_UserPauseOutranksSchedule(t *testing.T) {
	d := &Daemon{scheduler: closedScheduler(t), userPaused: true}

	if !d.IsPaused() {
		t.Error("IsPaused = false with a user pause set")
	}
	if got := d.PauseReason(); got != "user" {
		t.Errorf("PauseReason = %q, want %q", got, "user")
	}
}
