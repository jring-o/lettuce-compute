package daemon

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
)

// liveNotice returns the live notice with the given code, if any.
func liveNotice(d *Daemon, code string) *Notice {
	notices, _ := d.Notices().Since(0)
	for i := range notices {
		if notices[i].Code == code && notices[i].ResolvedAt == nil {
			return &notices[i]
		}
	}
	return nil
}

// A "run when idle" volunteer whose machine cannot report idle time never
// starts work. Every surface used to call that "outside your schedule", as if
// the pause would end on its own, and nothing raised a notice.
func TestWhenIdle_UnreadableIdleTimeIsNamedAndNoticed(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "WHEN_IDLE", IdleThresholdMins: 1}, slog.Default())
	idleErr := errors.New("this computer's idle time cannot be read (D-Bus ScreenSaver: exit status 1; xprintidle: not found)")
	scheduler.SetIdleFunc(func() (int, error) { return 0, idleErr })
	d := newTestDaemonWithResources(&mockClient{}, &mockRuntime{canHandle: true}, &testLimiter{}, scheduler)

	// The main loop's gate parks before the resource monitor signals.
	if got := d.PauseReason(); got != "idle_unknown" {
		t.Errorf("PauseReason() before the monitor's signal = %q, want \"idle_unknown\"", got)
	}
	// And after the resource monitor's pause signal.
	d.setAutoPause(pauseSourceResource, true)
	if got := d.PauseReason(); got != "idle_unknown" {
		t.Errorf("PauseReason() after the monitor's signal = %q, want \"idle_unknown\"", got)
	}
	if detail := d.PauseDetail(); !strings.Contains(detail, "cannot tell when this computer is idle") {
		t.Errorf("PauseDetail() = %q, want the explanation and its fix", detail)
	}

	n := liveNotice(d, "idle_detection_unavailable")
	if n == nil {
		t.Fatal("no live idle_detection_unavailable notice while idle time cannot be read")
	}
	if n.Level != NoticeWarn || !strings.Contains(n.Message, `"run when idle" will never start work`) {
		t.Errorf("notice = %+v, want a warning that says work will never start", *n)
	}

	// Idle time readable again, and the machine idle: the notice resolves and
	// the pause lifts.
	scheduler.SetIdleFunc(func() (int, error) { return 600, nil })
	if !scheduler.ShouldRun() {
		t.Fatal("ShouldRun() = false with 10 minutes idle and a 1 minute threshold")
	}
	if n := liveNotice(d, "idle_detection_unavailable"); n != nil {
		t.Errorf("the notice is still live after idle time became readable: %+v", *n)
	}
	d.setAutoPause(pauseSourceResource, false)
	if got := d.PauseReason(); got != "" {
		t.Errorf("PauseReason() once idle = %q, want \"\"", got)
	}
}

// The ordinary case is unchanged: idle time readable, someone at the keyboard.
func TestWhenIdle_ReadableIdleTimeStaysScheduled(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "WHEN_IDLE", IdleThresholdMins: 5}, slog.Default())
	scheduler.SetIdleFunc(func() (int, error) { return 30, nil })
	d := newTestDaemonWithResources(&mockClient{}, &mockRuntime{canHandle: true}, &testLimiter{}, scheduler)

	if got := d.PauseReason(); got != "scheduled" {
		t.Errorf("PauseReason() = %q, want \"scheduled\"", got)
	}
	if detail := d.PauseDetail(); detail != "" {
		t.Errorf("PauseDetail() = %q, want none", detail)
	}
	if n := liveNotice(d, "idle_detection_unavailable"); n != nil {
		t.Errorf("an idle notice was raised while idle time is readable: %+v", *n)
	}
}
