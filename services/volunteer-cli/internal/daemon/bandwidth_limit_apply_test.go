package daemon

import (
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/netlimit"
)

// resource_limits.max_bandwidth_mbps saved while the daemon runs — the app's
// Network Bandwidth slider, through PUT /api/v1/config — paces transfers from
// that moment, like the other resource limits, and 0 lifts it.
// Before, the figure was only reported to heads and nothing in the process read
// it at all.
func TestApplyConfig_BandwidthLimitPacesTransfersLive(t *testing.T) {
	netlimit.SetMbps(0)
	t.Cleanup(func() { netlimit.SetMbps(0) })
	d, _, _ := tb63Daemon(t)

	limited := *d.cfg
	limited.ResourceLimits.MaxBandwidthMbps = 8
	d.ApplyConfig(&limited)
	if got := netlimit.Mbps(); got != 8 {
		t.Fatalf("after saving max_bandwidth_mbps 8, transfers are paced at %d Mbps; want 8", got)
	}
	if netlimit.Download.Burst() == 0 || netlimit.Upload.Burst() == 0 {
		t.Error("both directions must be paced")
	}

	unlimited := limited
	unlimited.ResourceLimits.MaxBandwidthMbps = 0
	d.ApplyConfig(&unlimited)
	if got := netlimit.Mbps(); got != 0 {
		t.Errorf("after saving max_bandwidth_mbps 0, transfers are still paced at %d Mbps", got)
	}
}
