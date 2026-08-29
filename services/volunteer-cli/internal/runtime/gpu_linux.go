//go:build linux

package runtime

// platformGPUDetectors: the vendor command-line tools are safe to launch on
// Linux (neither requests elevation), so both vendors are probed through
// them.
func platformGPUDetectors() []gpuDetector {
	return []gpuDetector{
		{label: "nvidia", fn: detectNVIDIAGPUs},
		{label: "amd", fn: detectAMDGPUs},
	}
}

// platformDisplayAdapterSource: Linux has no device registry to enumerate;
// GPUs are found through the vendor tools above.
func platformDisplayAdapterSource() (DisplayAdapterReader, error) {
	return nil, ErrDisplayAdaptersUnsupported
}
