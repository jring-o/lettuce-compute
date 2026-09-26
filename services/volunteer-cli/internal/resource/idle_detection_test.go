package resource

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// IdleDetectionError and the change callback are how the daemon names the
// pause and raises its notice: the callback fires on each start and end of a
// failure, never on the readings in between.
func TestScheduler_IdleDetectionErrorAndChanges(t *testing.T) {
	logger, _ := debugLogger()
	s := NewScheduler(&config.Scheduling{Mode: "WHEN_IDLE", IdleThresholdMins: 1}, logger)
	var readErr error
	s.idleFunc = func() (int, error) { return 0, readErr }
	var changes []error
	s.OnIdleDetectionChange(func(err error) { changes = append(changes, err) })

	s.ShouldRun()
	if s.IdleDetectionError() != nil || len(changes) != 0 {
		t.Fatalf("a working reading: IdleDetectionError() = %v, changes = %v; want nil and none", s.IdleDetectionError(), changes)
	}

	readErr = errors.New("no idle source answered")
	for i := 0; i < 5; i++ {
		s.ShouldRun()
	}
	if s.IdleDetectionError() == nil {
		t.Fatal("IdleDetectionError() = nil while readings fail")
	}
	if len(changes) != 1 || changes[0] == nil {
		t.Fatalf("changes after five failed readings = %v, want exactly one carrying the error", changes)
	}

	readErr = nil
	s.ShouldRun()
	s.ShouldRun()
	if s.IdleDetectionError() != nil {
		t.Errorf("IdleDetectionError() = %v after a working reading, want nil", s.IdleDetectionError())
	}
	if len(changes) != 2 || changes[1] != nil {
		t.Errorf("changes after recovery = %v, want a second, nil entry", changes)
	}
}

// ShouldRun is called from several goroutines at once. The change callbacks
// must still arrive in the order the readings were recorded — strictly
// alternating, ending on the scheduler's own state — or the daemon's notice
// could be left raised while readings work.
func TestScheduler_IdleChangesStayOrderedUnderConcurrentReadings(t *testing.T) {
	logger, _ := debugLogger()
	s := NewScheduler(&config.Scheduling{Mode: "WHEN_IDLE", IdleThresholdMins: 1}, logger)
	var calls atomic.Int64
	s.idleFunc = func() (int, error) {
		if calls.Add(1)%3 == 0 {
			return 0, errors.New("no idle source answered")
		}
		return 600, nil
	}
	var mu sync.Mutex
	var changes []bool // true = started failing
	s.OnIdleDetectionChange(func(err error) {
		mu.Lock()
		changes = append(changes, err != nil)
		mu.Unlock()
	})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				s.ShouldRun()
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(changes) == 0 {
		t.Fatal("no change was reported although readings alternated")
	}
	for i, failing := range changes {
		if want := i%2 == 0; failing != want {
			t.Fatalf("change %d says failing=%v, want %v: changes do not alternate", i, failing, want)
		}
	}
	if last := changes[len(changes)-1]; last != (s.IdleDetectionError() != nil) {
		t.Errorf("last change says failing=%v, but IdleDetectionError() = %v", last, s.IdleDetectionError())
	}
}

// Modes that never read idle time never report an idle failure.
func TestScheduler_NoIdleErrorOutsideWhenIdle(t *testing.T) {
	logger, _ := debugLogger()
	s := NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, logger)
	s.idleFunc = func() (int, error) { return 0, errors.New("must not be asked") }
	s.ShouldRun()
	if err := s.IdleDetectionError(); err != nil {
		t.Errorf("ALWAYS mode: IdleDetectionError() = %v, want nil", err)
	}
}

func TestFirstIdleReading(t *testing.T) {
	works := func(secs int) func() (int, error) { return func() (int, error) { return secs, nil } }
	fails := func(msg string) func() (int, error) { return func() (int, error) { return 0, errors.New(msg) } }

	secs, err := firstIdleReading([]idleSource{{"a", fails("a is missing")}, {"b", works(42)}})
	if err != nil || secs != 42 {
		t.Errorf("first source fails, second answers: got (%d, %v), want (42, nil)", secs, err)
	}

	secs, err = firstIdleReading([]idleSource{{"a", works(7)}, {"b", fails("unused")}})
	if err != nil || secs != 7 {
		t.Errorf("first source answers: got (%d, %v), want (7, nil)", secs, err)
	}

	_, err = firstIdleReading([]idleSource{{"D-Bus ScreenSaver", fails("no bus")}, {"xprintidle", fails("no display")}})
	if !errors.Is(err, ErrIdleUnknown) {
		t.Fatalf("every source fails: err = %v, want one wrapping ErrIdleUnknown", err)
	}
	for _, want := range []string{"D-Bus ScreenSaver: no bus", "xprintidle: no display"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// macOS: ioreg output with no HIDIdleTime used to read as 0 seconds idle.
func TestParseHIDIdleTime(t *testing.T) {
	out := `    | |   "HIDIdleTime" = 125000000000
    | |   "HIDKeyboardModifierMappingPairs" = ()`
	if secs, err := parseHIDIdleTime(out); err != nil || secs != 125 {
		t.Errorf("parseHIDIdleTime(with a value) = (%d, %v), want (125, nil)", secs, err)
	}
	if _, err := parseHIDIdleTime(`+-o Root  <class IORegistryEntry>`); err == nil {
		t.Error("parseHIDIdleTime(no HIDIdleTime) = nil error, want one: no value is not zero seconds idle")
	}
}

func TestDescribeIdleUnavailable(t *testing.T) {
	msg := DescribeIdleUnavailable()
	for _, want := range []string{"cannot tell when this computer is idle", "never start work", "schedule clear"} {
		if !strings.Contains(msg, want) {
			t.Errorf("DescribeIdleUnavailable() = %q, want it to contain %q", msg, want)
		}
	}
	if IdleDetectionRemedy != "" && !strings.Contains(msg, IdleDetectionRemedy) {
		t.Errorf("DescribeIdleUnavailable() = %q, want this platform's remedy %q", msg, IdleDetectionRemedy)
	}
}
