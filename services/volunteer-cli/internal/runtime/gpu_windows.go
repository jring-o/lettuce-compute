//go:build windows

package runtime

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// Windows GPU detection.
//
// The vendor command-line tools are not launched here, with one exception.
// C:\Windows\System32\amd-smi.exe ships with AMD's display drivers and
// requests UAC elevation when it starts, so on a machine with AMD drivers
// every hardware probe raised a "DiskPart is requesting your permission"
// prompt and then blocked until the detection timeout killed the call. The
// registry class key for display adapters carries the same facts — model,
// vendor, dedicated memory — and reading it needs no elevation and starts no
// process. nvidia-smi is still run: it launches unelevated and is the only
// source of CUDA compute capability. A card it reports is dropped from the
// registry's list so it is not counted twice.

// displayClassKeyPath is the device class key that holds one instance
// subkey per installed display adapter.
const displayClassKeyPath = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`

func platformGPUDetectors() []gpuDetector {
	return []gpuDetector{{label: "windows", fn: detectWindowsGPUs}}
}

// detectWindowsGPUs runs nvidia-smi (bounded by the per-command timeout)
// and the registry enumeration, and merges them — never rocm-smi or amd-smi.
func detectWindowsGPUs() ([]*GpuDetectionResult, error) {
	smi, _ := detectNVIDIAGPUs() // never returns an error; failures degrade to none
	reg, err := detectRegistryGPUs()
	return mergeRegistryWithNvidiaSmi(smi, reg), err
}

// platformDisplayAdapterSource opens the display adapter class key for
// reading. The key is world-readable, so no elevation is involved.
func platformDisplayAdapterSource() (DisplayAdapterReader, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, displayClassKeyPath, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
	if err != nil {
		return nil, fmt.Errorf("open display adapter class key: %w", err)
	}
	return &registryAdapterReader{class: k}, nil
}

// registryAdapterReader implements DisplayAdapterReader over the live
// registry. Each value read opens the instance subkey for the duration of
// the read; the class key is held until Close, which detectRegistryGPUs
// calls once parsing is done.
type registryAdapterReader struct {
	class registry.Key
}

// Close releases the class key handle.
func (r *registryAdapterReader) Close() error {
	return r.class.Close()
}

func (r *registryAdapterReader) Instances() []string {
	names, err := r.class.ReadSubKeyNames(-1)
	if err != nil {
		return nil
	}
	return names
}

func (r *registryAdapterReader) open(instance string) (registry.Key, bool) {
	k, err := registry.OpenKey(r.class, instance, registry.QUERY_VALUE)
	if err != nil {
		return 0, false
	}
	return k, true
}

func (r *registryAdapterReader) StringValue(instance, name string) (string, bool) {
	k, ok := r.open(instance)
	if !ok {
		return "", false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return "", false
	}
	return v, true
}

func (r *registryAdapterReader) IntegerValue(instance, name string) (uint64, bool) {
	k, ok := r.open(instance)
	if !ok {
		return 0, false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(name) // REG_DWORD or REG_QWORD
	if err != nil {
		return 0, false
	}
	return v, true
}

func (r *registryAdapterReader) BinaryValue(instance, name string) ([]byte, bool) {
	k, ok := r.open(instance)
	if !ok {
		return nil, false
	}
	defer k.Close()
	v, _, err := k.GetBinaryValue(name)
	if err != nil {
		return nil, false
	}
	return v, true
}
