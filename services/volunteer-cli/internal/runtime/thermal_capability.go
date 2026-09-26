package runtime

// ThermalCapability says where this machine's CPU temperature comes from — or
// that it comes from nowhere, which the thermal monitor's threshold check
// otherwise treats exactly like "the CPU is cool" (TB-77).
//
// A reading of 0 has always meant "unknown" to the monitor: it can never pause
// work, and it silently keeps a paused daemon paused only by the GPU or a
// critical sensor. That is the right rule for a threshold check, but it left
// every macOS volunteer without the helper tool, and every Windows volunteer,
// believing the pause-above-85 °C sliders protected them. A tester watched an
// Intel MacBook's die sit at 100 °C under "pause above 85 °C" and asked why the
// pause never came. This type is how the daemon, `doctor`, the management API
// and the desktop app say so instead.
type ThermalCapability struct {
	// CPUSource names the reader: "sysfs" (Linux thermal zones and hwmon),
	// "osx-cpu-temp" (macOS, a helper tool this client shells out to), or
	// "none".
	CPUSource string
	// CPUReadable is true when a CPU temperature can actually be read on this
	// machine right now. When false the CPU thresholds have no effect; the GPU
	// thresholds act or not by GPUReadable, and the hardware's own thermal
	// protection is unaffected either way.
	CPUReadable bool
	// Detail is one sentence for a human: which sensor is read, or why none is.
	Detail string
	// Remedy is what the volunteer can do about an unreadable CPU, or "" when
	// nothing they do will change it. It decides how loudly the condition is
	// reported: a fixable gap is a warning, an unfixable one is information
	// (TI-19's severity split — a box labelled "needs attention" must not hold
	// what no one can act on).
	Remedy string

	// GPUSource names where GPU temperatures come from: the vendor tools that
	// answered ("nvidia-smi", "rocm-smi"), "sysfs" for Linux GPU thermal zones,
	// joined with "+" when there are several, or "none". Filled by the thermal
	// monitor at Start (DetectGPUThermal); the platform reader leaves it empty.
	GPUSource string
	// GPUReadable is true when a GPU temperature was read at start, so the GPU
	// thresholds act on this machine. False on a machine with no GPU, and on one
	// whose cards no tool this client may start reports a temperature for; the
	// GPU thresholds then have no effect, exactly as the CPU ones do when
	// CPUReadable is false.
	GPUReadable bool
	// GPUDetail is one sentence for a human: which cards are read, or why none is.
	GPUDetail string
}

// Fixable reports whether the volunteer can do something about an unreadable
// CPU temperature on this machine.
func (c ThermalCapability) Fixable() bool { return c.Remedy != "" }

// ThermalCapabilityReader detects this machine's CPU temperature source. It is
// a variable so tests can stand in a platform's answer.
var ThermalCapabilityReader = detectThermalCapability
