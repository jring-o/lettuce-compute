//go:build windows

package runtime

// readCPUTemperature on Windows returns 0 (unknown).
//
// Windows does not reliably expose CPU temperature. The WMI class
// MSAcpi_ThermalZoneTemperature requires admin privileges and triggers
// system prompts (DiskPart.exe UAC dialogs) on many machines. Rather
// than risk disruptive system popups, we return 0 which causes the
// thermal monitor to skip the CPU threshold check. GPU thermal
// monitoring (via nvidia-smi/rocm-smi) still works.
func readCPUTemperature() int {
	return 0
}

// readSensors on Windows returns nothing, for the same reason
// readCPUTemperature does: the WMI thermal class needs admin rights and can
// raise UAC prompts, so no temperature is read at all. GPU monitoring via
// nvidia-smi/rocm-smi is unaffected.
func readSensors() []Sensor { return nil }
