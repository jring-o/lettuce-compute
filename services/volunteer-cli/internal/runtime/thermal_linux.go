//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// thermalZoneGlob and hwmonGlob are the two sysfs surfaces that expose
// temperatures. Package vars so tests can point them at a synthetic tree —
// readings cannot otherwise be exercised, since a CI host's sensors are whatever
// that host happens to have, and a container commonly has none at all.
var (
	thermalZoneGlob = "/sys/class/thermal/thermal_zone*"
	hwmonGlob       = "/sys/class/hwmon/hwmon*"
)

// Plausible bounds for a real silicon temperature reading, in Celsius. Anything
// outside is treated as unreadable rather than trusted.
//
// Bogus sysfs temperatures are common and well documented: ACPI zones stuck at a
// constant regardless of load, zones reporting a trip point (200C) instead of a
// live value, and readings that jump wildly enough to have triggered emergency
// shutdowns on some kernels. Since a high reading here suspends ALL work, an
// implausible one must not be believed.
const (
	minPlausibleTempC = 5
	maxPlausibleTempC = 120
)

// cpuZoneTypes are the `type` values Linux uses for zones that actually measure
// CPU/SoC silicon. Matched as a prefix so vendor suffixes (`coretemp-isa-0000`,
// `cpu-thermal0`) are covered.
var cpuZoneTypes = []string{
	"x86_pkg_temp", // Intel package
	"coretemp",     // Intel per-core
	"k10temp",      // AMD
	"zenpower",     // AMD (out-of-tree)
	"cpu-thermal",  // ARM/SoC device trees
	"cpu_thermal",
	"soc_thermal",
	"pkg_temp",
}

// gpuZoneTypes are zones that measure a GPU. Read so a GPU with no
// nvidia-smi/rocm-smi collector still contributes, judged against the GPU
// thresholds rather than the CPU ones.
var gpuZoneTypes = []string{
	"amdgpu",
	"radeon",
	"nouveau",
	"gpu-thermal",
	"gpu_thermal",
	"intel_gpu",
}

// classifyZone maps a sysfs zone `type` to the threshold family it belongs to.
//
// This is the point of the file. The reader used to take the maximum over EVERY
// thermal_zone and hand it to the caller as "the CPU temperature", never opening
// `type` at all (TB-17). Linux exposes plenty of zones that are not the CPU —
// NVMe controllers, WiFi chipsets, the PCH, batteries, ambient ACPI zones — and
// their safe operating ranges differ from a CPU's by tens of degrees. An NVMe
// controller at 85C is inside its normal range; a CPU at 85C is not. One
// threshold applied to all of them froze a volunteer's work for 2 h 28 min on a
// machine whose CPU was fine, and could not clear, because suspending Lettuce
// does not cool a disk that something else is using.
func classifyZone(zoneType string) SensorClass {
	t := strings.ToLower(strings.TrimSpace(zoneType))
	for _, p := range cpuZoneTypes {
		if strings.HasPrefix(t, p) {
			return SensorCPU
		}
	}
	for _, p := range gpuZoneTypes {
		if strings.HasPrefix(t, p) {
			return SensorGPU
		}
	}
	return SensorOther
}

// readMilliCelsius reads a sysfs millidegree file and converts to whole degrees.
// Returns 0, false when unreadable or implausible.
func readMilliCelsius(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	milliC, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	c := milliC / 1000
	if c < minPlausibleTempC || c > maxPlausibleTempC {
		return 0, false
	}
	return c, true
}

// criticalTripC returns the zone's highest vendor-declared danger point, from
// whichever trip point is typed `critical` or `hot`. Returns 0 when the zone
// declares none.
//
// This is the kernel's own opinion about that part, which beats anything we
// could hard-code for a component we cannot identify: a drive declaring
// critical=95 is telling us the number at which IT is in trouble, and that is
// the only defensible threshold for a sensor outside the CPU/GPU families.
func criticalTripC(zoneDir string) int {
	types, err := filepath.Glob(filepath.Join(zoneDir, "trip_point_*_type"))
	if err != nil {
		return 0
	}
	best := 0
	for _, typePath := range types {
		data, err := os.ReadFile(typePath)
		if err != nil {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(string(data)))
		if kind != "critical" && kind != "hot" {
			continue
		}
		tempPath := strings.TrimSuffix(typePath, "_type") + "_temp"
		if c, ok := readMilliCelsius(tempPath); ok && c > best {
			best = c
		}
	}
	return best
}

// readSensors returns every readable thermal zone, classified.
func readSensors() []Sensor {
	dirs, err := filepath.Glob(thermalZoneGlob)
	if err != nil {
		return readHwmonCPUSensors()
	}
	var out []Sensor
	for _, dir := range dirs {
		tempC, ok := readMilliCelsius(filepath.Join(dir, "temp"))
		if !ok {
			continue
		}
		zoneType := ""
		if data, err := os.ReadFile(filepath.Join(dir, "type")); err == nil {
			zoneType = strings.TrimSpace(string(data))
		}
		out = append(out, Sensor{
			Zone:      filepath.Base(dir),
			Kind:      zoneType,
			Class:     classifyZone(zoneType),
			TempC:     tempC,
			CriticalC: criticalTripC(dir),
		})
	}
	return append(out, readHwmonCPUSensors()...)
}

// readHwmonCPUSensors reads CPU temperatures from the hwmon subsystem, which is
// where `coretemp` and `k10temp` land on machines that expose no CPU
// thermal_zone at all. Only CPU drivers are read: hwmon also carries fan speeds,
// voltages and drive temperatures, and this is a fallback for finding the CPU,
// not a second pass over everything.
func readHwmonCPUSensors() []Sensor {
	dirs, err := filepath.Glob(hwmonGlob)
	if err != nil {
		return nil
	}
	var out []Sensor
	for _, dir := range dirs {
		nameBytes, err := os.ReadFile(filepath.Join(dir, "name"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(nameBytes))
		if classifyZone(name) != SensorCPU {
			continue
		}
		inputs, err := filepath.Glob(filepath.Join(dir, "temp*_input"))
		if err != nil {
			continue
		}
		for _, input := range inputs {
			if c, ok := readMilliCelsius(input); ok {
				out = append(out, Sensor{
					Zone:  filepath.Base(dir) + "/" + filepath.Base(input),
					Kind:  name,
					Class: SensorCPU,
					TempC: c,
				})
			}
		}
	}
	return out
}

// readCPUTemperature reports the hottest CPU sensor, or 0 when this machine
// exposes none we can identify.
//
// Returning 0 disables the CPU threshold check entirely (the caller reads it as
// unknown). That is deliberate: a wrong pause costs a volunteer all of their
// throughput with no explanation, so where we cannot tell what we are reading,
// we do not guess.
func readCPUTemperature() int {
	maxTemp := 0
	for _, s := range readSensors() {
		if s.Class == SensorCPU && s.TempC > maxTemp {
			maxTemp = s.TempC
		}
	}
	return maxTemp
}
