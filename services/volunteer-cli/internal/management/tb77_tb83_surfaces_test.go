package management

import (
	"net/http"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-83: the yield block round-trips through the config API — GET reports
// it, PUT persists it — so the app's Settings section can write it.
func TestTB83_ConfigAPI_YieldBlock(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "GET", "/api/v1/config", "")
	body := decodeJSON(t, resp)
	y, ok := body["yield"].(map[string]any)
	if !ok {
		t.Fatalf("GET /api/v1/config has no yield block: %v", body)
	}
	if y["enabled"] != false || y["cpu_pause_pct"] != float64(25) || y["cpu_resume_pct"] != float64(15) {
		t.Errorf("default yield block = %v, want off / 25 / 15", y)
	}

	resp = env.doRequest(t, "PUT", "/api/v1/config", `{"yield":{"enabled":true,"cpu_pause_pct":40,"cpu_resume_pct":20}}`)
	body = decodeJSON(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT yield: %d %v", resp.StatusCode, body)
	}
	cfg, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Yield.Enabled || cfg.Yield.CPUPausePct != 40 || cfg.Yield.CPUResumePct != 20 || cfg.Yield.WindowSeconds != 30 {
		t.Errorf("yield on disk = %+v, want enabled 40/20 with the window untouched", cfg.Yield)
	}

	// Validation still applies: a resume threshold at or above the pause one
	// is refused.
	resp = env.doRequest(t, "PUT", "/api/v1/config", `{"yield":{"enabled":true,"cpu_pause_pct":20,"cpu_resume_pct":30}}`)
	if resp.StatusCode == http.StatusOK {
		t.Error("PUT with cpu_resume_pct >= cpu_pause_pct must be refused")
	}
}

// TB-77: the machine capabilities carry where the CPU temperature comes
// from, so the app can say the thresholds have no effect where they do not.
func TestTB77_MachineCaps_ReportCPUTemperatureSource(t *testing.T) {
	orig := runtime.ThermalCapabilityReader
	t.Cleanup(func() { runtime.ThermalCapabilityReader = orig })
	runtime.ThermalCapabilityReader = func() runtime.ThermalCapability {
		return runtime.ThermalCapability{CPUSource: "none", Detail: "no helper", Remedy: "install it"}
	}

	env := setupTestEnv(t)
	// The test env's daemon is not running, so its monitors have not
	// detected anything; the accessor detects on demand in that case.
	caps := env.bridge.MachineCaps()
	if caps.CPUTempReadable || caps.CPUTempSource != "none" || caps.CPUTempDetail != "no helper" || caps.CPUTempRemedy != "install it" {
		t.Errorf("machine caps thermal fields = source %q readable %v detail %q remedy %q; want the stubbed unreadable source",
			caps.CPUTempSource, caps.CPUTempReadable, caps.CPUTempDetail, caps.CPUTempRemedy)
	}
	if !caps.YieldMeasurable {
		t.Error("with yield off, YieldMeasurable must read true (nothing has failed)")
	}

	resp := env.doRequest(t, "GET", "/api/v1/heads", "")
	body := decodeJSON(t, resp)
	m, _ := body["machine"].(map[string]any)
	if m["cpu_temp_readable"] != false || m["cpu_temp_source"] != "none" {
		t.Errorf("GET /api/v1/heads machine = %v, want cpu_temp_source none / cpu_temp_readable false", m)
	}
}
