package cli

import "testing"

func TestFormatDurationSeconds(t *testing.T) {
	cases := []struct {
		secs int
		want string
	}{
		{-5, "0s"},
		{0, "0s"},
		{45, "45s"},
		{60, "1m00s"},
		{750, "12m30s"},
		{3600, "1h00m"},
		{11100, "3h05m"},
	}
	for _, c := range cases {
		if got := formatDurationSeconds(c.secs); got != c.want {
			t.Errorf("formatDurationSeconds(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}

func TestLabelOrDash(t *testing.T) {
	if got := labelOrDash(""); got != "—" {
		t.Errorf("labelOrDash(\"\") = %q, want em dash", got)
	}
	if got := labelOrDash("  "); got != "—" {
		t.Errorf("labelOrDash(spaces) = %q, want em dash", got)
	}
	if got := labelOrDash("native"); got != "native" {
		t.Errorf("labelOrDash(\"native\") = %q, want \"native\"", got)
	}
}
