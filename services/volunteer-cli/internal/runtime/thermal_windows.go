//go:build windows

package runtime

// readCPUTemperature on Windows returns 0 (unknown).
//
// Windows does not reliably expose CPU temperature. The WMI class
// MSAcpi_ThermalZoneTemperature requires admin privileges and triggers
// system prompts (DiskPart.exe UAC dialogs) on many machines. Rather
// than risk disruptive system popups, we return 0 which causes the
// thermal monitor to skip the CPU threshold check. An NVIDIA card's
// temperature is still read, with nvidia-smi (gpu_thermal.go); no other
// GPU tool is started on Windows.
func readCPUTemperature() int {
	return 0
}

// readSensors on Windows returns nothing, for the same reason
// readCPUTemperature does: the WMI thermal class needs admin rights and can
// raise UAC prompts, so no sensor is read. An NVIDIA card's temperature comes
// from nvidia-smi instead (gpu_thermal.go).
func readSensors() []Sensor { return nil }

// detectThermalCapability on Windows is always "none", and unfixable by the
// volunteer: the decision above not to ask WMI is this client's, so there is
// no remedy to offer (TB-77). Reported as information, not a warning.
func detectThermalCapability() ThermalCapability {
	return ThermalCapability{
		CPUSource: "none",
		Detail:    "Windows only lets programs running as administrator read the CPU temperature, and asking can raise a permissions prompt, so Lettuce does not try",
	}
}
