package daemon

import (
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-90 regression test, daemon side: a yield change that arrives through
// ApplyConfig — the app's Settings page saving "When other programs need the
// CPU" — reaches the running monitor. Before this change ApplyConfig swapped
// the config pointer and left the monitor on the values it was built with,
// so `config get` showed the new thresholds while `status`, the app's
// "busy" detail and the pause itself kept the old ones until a restart the
// app never asked for.
func TestTB90_ApplyConfigReconfiguresTheYieldMonitor(t *testing.T) {
	d, _, _ := tb63Daemon(t)
	d.cfg.Yield = config.YieldConfig{Enabled: true, CPUPausePct: 25, CPUResumePct: 15, WindowSeconds: 30, PollIntervalSeconds: 5}
	d.yieldMonitor = runtime.NewYieldMonitor(runtime.YieldConfig{
		Enabled:             true,
		CPUPausePct:         25,
		CPUResumePct:        15,
		WindowSeconds:       30,
		PollIntervalSeconds: 5,
	}, make(chan bool, 1), d.logger)

	changed := *d.cfg
	changed.Yield.CPUPausePct = 10
	changed.Yield.CPUResumePct = 5
	d.ApplyConfig(&changed)
	if snap := d.YieldSnapshot(); snap.PausePct != 10 || snap.ResumePct != 5 {
		t.Errorf("monitor thresholds after ApplyConfig = %d/%d, want 10/5 (the saved change must reach the running monitor)",
			snap.PausePct, snap.ResumePct)
	}

	off := changed
	off.Yield.Enabled = false
	d.ApplyConfig(&off)
	if snap := d.YieldSnapshot(); snap.Enabled {
		t.Errorf("monitor still enabled after the setting was turned off through ApplyConfig: %+v", snap)
	}
}
