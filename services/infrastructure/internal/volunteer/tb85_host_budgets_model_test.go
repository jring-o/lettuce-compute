package volunteer

import (
	"encoding/json"
	"strings"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestTB85_HostBudgetsSurviveTheStoredHardware: the host budgets a client
// advertises (TB-85) are kept through the conversion the head stores a
// host's hardware with — JSON in hosts.hardware_capabilities — and back to
// the wire, so the dispatch gate that reads the stored hardware sees them.
func TestTB85_HostBudgetsSurviveTheStoredHardware(t *testing.T) {
	pb := &lettucev1.HardwareCapabilities{CpuCores: 8, MaxCpuCores: 2, MemoryTotalMb: 8192, MaxMemoryMb: 768,
		HostMaxCpuCores: 4, HostMaxMemoryMb: 1024}
	hw := HardwareCapabilitiesFromProto(pb)
	raw, err := json.Marshal(hw)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"host_max_memory_mb":1024`, `"host_max_cpu_cores":4`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("stored hardware lacks %s: %s", want, raw)
		}
	}
	var back HardwareCapabilities
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	out := HardwareCapabilitiesToProto(back)
	if out.HostMaxMemoryMb != 1024 || out.HostMaxCpuCores != 4 || out.MaxMemoryMb != 768 || out.MaxCpuCores != 2 {
		t.Errorf("round trip = max %d MB / %d cores, host %d MB / %d cores; want 768 / 2, 1024 / 4",
			out.MaxMemoryMb, out.MaxCpuCores, out.HostMaxMemoryMb, out.HostMaxCpuCores)
	}
}
