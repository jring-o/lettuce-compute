//go:build !amd64

package client

// cpuidVendorString returns "" on architectures without the x86 CPUID
// instruction (notably arm64). Vendor detection there falls back to the CPU
// model/brand string — see detectCPUVendor / cpuVendorFromModel — which is how
// Apple Silicon (darwin/arm64) is classified.
func cpuidVendorString() string { return "" }
