package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// status lists what each head said when it sent no work and said why; doctor lists it
// per head, as a warning only where the volunteer's account is affected (benched, or
// too slow for a leaf's deadlines).
func TestHeadNoWorkLines(t *testing.T) {
	at := time.Date(2026, 9, 26, 14, 5, 0, 0, time.Local)
	heads := []leafsAPIHead{
		{Name: "lbry", NoWork: &headNoWorkAPI{Reason: "inflight_cap", Message: "lbry lets this machine hold 10 tasks at a time right now, and this machine holds 10.", At: at}},
		{Name: "quiet"},
		{Name: "scios", NoWork: &headNoWorkAPI{Reason: "account_benched", Message: "scios has paused sending work to this account.", At: at}},
	}

	var out bytes.Buffer
	printHeadNoWork(&out, heads)
	want := "Why a head is sending no work:\n" +
		"  - lbry lets this machine hold 10 tasks at a time right now, and this machine holds 10. (14:05)\n" +
		"  - scios has paused sending work to this account. (14:05)\n"
	if out.String() != want {
		t.Errorf("status section =\n%s\nwant\n%s", out.String(), want)
	}

	out.Reset()
	printHeadNoWork(&out, []leafsAPIHead{{Name: "quiet"}})
	if out.Len() != 0 {
		t.Errorf("status printed %q with no stated reason, want nothing", out.String())
	}

	out.Reset()
	rep := &doctorReport{w: &out}
	checkHeadNoWork(rep, heads)
	if rep.warns != 1 || rep.fails != 0 {
		t.Errorf("doctor warns/fails = %d/%d, want 1/0 (only the benched account needs attention)", rep.warns, rep.fails)
	}
	got := out.String()
	for _, part := range []string{"lbry", "sent no work at 14:05: lbry lets this machine hold 10 tasks", "scios", "sent no work at 14:05: scios has paused sending work"} {
		if !strings.Contains(got, part) {
			t.Errorf("doctor output lacks %q:\n%s", part, got)
		}
	}
	if strings.Contains(got, "quiet") {
		t.Errorf("doctor listed a head that gave no reason:\n%s", got)
	}
}
