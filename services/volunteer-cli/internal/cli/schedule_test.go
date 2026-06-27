package cli

import (
	"reflect"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

func TestParseScheduleHour(t *testing.T) {
	ok := map[string]int{"0": 0, "20": 20, "20:00": 20, "06:00": 6, " 23 ": 23}
	for in, want := range ok {
		got, err := parseScheduleHour(in)
		if err != nil {
			t.Errorf("parseScheduleHour(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseScheduleHour(%q) = %d, want %d", in, got, want)
		}
	}
	for _, in := range []string{"24", "-1", "20:30", "abc", "8:5", ""} {
		if _, err := parseScheduleHour(in); err == nil {
			t.Errorf("parseScheduleHour(%q) expected error, got nil", in)
		}
	}
}

func TestParseScheduleDays(t *testing.T) {
	cases := map[string][]int{
		"":            {0, 1, 2, 3, 4, 5, 6}, // default mon-sun
		"mon-sun":     {0, 1, 2, 3, 4, 5, 6},
		"mon-fri":     {0, 1, 2, 3, 4},
		"sat,sun":     {5, 6},
		"mon,wed,fri": {0, 2, 4},
		"fri-mon":     {0, 4, 5, 6}, // wraps the week: Fri,Sat,Sun,Mon -> sorted
		"Tue":         {1},
	}
	for spec, want := range cases {
		got, err := parseScheduleDays(spec)
		if err != nil {
			t.Errorf("parseScheduleDays(%q) unexpected error: %v", spec, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseScheduleDays(%q) = %v, want %v", spec, got, want)
		}
	}
	for _, spec := range []string{"funday", "mon-funday", "13"} {
		if _, err := parseScheduleDays(spec); err == nil {
			t.Errorf("parseScheduleDays(%q) expected error, got nil", spec)
		}
	}
}

func TestBuildScheduleRange(t *testing.T) {
	r, err := buildScheduleRange("19:00", "07:00", "mon-fri")
	if err != nil {
		t.Fatalf("buildScheduleRange: %v", err)
	}
	if r.StartHour != 19 || r.EndHour != 7 {
		t.Errorf("hours = %d-%d, want 19-7", r.StartHour, r.EndHour)
	}
	if !reflect.DeepEqual(r.Days, []int{0, 1, 2, 3, 4}) {
		t.Errorf("days = %v, want [0 1 2 3 4]", r.Days)
	}
	if _, err := buildScheduleRange("", "07:00", "mon-fri"); err == nil {
		t.Error("expected error for missing --from")
	}
}

func TestDescribeRange(t *testing.T) {
	cases := []struct {
		r    config.ScheduleRange
		want string
	}{
		{config.ScheduleRange{Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHour: 20, EndHour: 6}, "20:00–06:00 (overnight) on every day"},
		{config.ScheduleRange{Days: []int{0, 1, 2, 3, 4}, StartHour: 9, EndHour: 17}, "09:00–17:00 on Mon, Tue, Wed, Thu, Fri"},
		{config.ScheduleRange{Days: []int{5, 6}, StartHour: 0, EndHour: 0}, "all day on Sat, Sun"},
	}
	for _, c := range cases {
		if got := describeRange(c.r); got != c.want {
			t.Errorf("describeRange(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}
