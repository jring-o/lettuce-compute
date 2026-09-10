package config

import "testing"

// TB-83: the yield block is reachable by `config set` / `config get`. Kept
// apart from the other yield tests so it compiles against the tree before the
// block existed (the red half of the regression evidence).
func TestTB83_YieldByPath(t *testing.T) {
	c := Defaults()
	sets := map[string]string{
		"yield.enabled":               "true",
		"yield.cpu_pause_pct":         "40",
		"yield.cpu_resume_pct":        "20",
		"yield.window_seconds":        "60",
		"yield.poll_interval_seconds": "10",
	}
	for k, v := range sets {
		if err := c.SetByPath(k, v); err != nil {
			t.Fatalf("SetByPath(%s): %v", k, err)
		}
	}
	for k, want := range sets {
		got, err := c.GetByPath(k)
		if err != nil {
			t.Fatalf("GetByPath(%s): %v", k, err)
		}
		if got != want {
			t.Errorf("GetByPath(%s) = %q, want %q", k, got, want)
		}
	}
	if err := c.SetByPath("yield.cpu_pause_pct", "lots"); err == nil {
		t.Error("expected an error setting yield.cpu_pause_pct to a non-integer")
	}
}
