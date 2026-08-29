package runtime

import (
	"encoding/binary"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

// Display-adapter enumeration without a subprocess.
//
// Windows keeps one registry key per installed display adapter instance under
// HKLM\SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}
// ("0000", "0001", ...), each carrying the driver's description (the card
// model), its provider (the vendor) and the adapter's dedicated memory. Reading
// it needs no elevation and starts no process, so it cannot raise a UAC prompt
// the way AMD's amd-smi does. The reader interface below is what the Windows
// build implements over the real registry (gpu_windows.go); the parser is
// platform-independent so it can be tested everywhere with a fake reader.

// DisplayAdapterReader reads display-adapter instances from the platform's
// device registry. Instances returns the instance names ("0000", ...);
// the value readers return ok=false when the value is absent or of another
// type — IntegerValue covers REG_DWORD and REG_QWORD, BinaryValue REG_BINARY.
type DisplayAdapterReader interface {
	Instances() []string
	StringValue(instance, name string) (string, bool)
	IntegerValue(instance, name string) (uint64, bool)
	BinaryValue(instance, name string) ([]byte, bool)
}

// ErrDisplayAdaptersUnsupported is returned by DisplayAdapterSource on
// platforms without a device registry to read.
var ErrDisplayAdaptersUnsupported = errors.New("display adapter enumeration is not supported on this platform")

// DisplayAdapterSource opens the platform's display-adapter registry. It is
// the Windows build's real reader and an unsupported stub elsewhere;
// override in tests so detection never touches the machine's registry.
var DisplayAdapterSource = platformDisplayAdapterSource

// Registry value names under each display adapter instance key.
const (
	adapterValueDriverDesc   = "DriverDesc"
	adapterValueProviderName = "ProviderName"
	// HardwareInformation.qwMemorySize is the 64-bit dedicated memory size
	// written by current drivers; HardwareInformation.MemorySize is the
	// older 32-bit value (REG_BINARY or REG_DWORD) some drivers still write.
	adapterValueMemoryQword = "HardwareInformation.qwMemorySize"
	adapterValueMemory      = "HardwareInformation.MemorySize"
)

// detectRegistryGPUs enumerates display adapters through DisplayAdapterSource.
func detectRegistryGPUs() ([]*GpuDetectionResult, error) {
	reader, err := DisplayAdapterSource()
	if err != nil {
		if errors.Is(err, ErrDisplayAdaptersUnsupported) {
			return nil, nil
		}
		return nil, err
	}
	if c, ok := reader.(interface{ Close() error }); ok {
		defer c.Close()
	}
	return parseDisplayAdapters(reader), nil
}

// parseDisplayAdapters turns the adapter instances a reader exposes into
// GPUs. An instance is reported when its provider maps to a known GPU vendor
// and it declares a non-zero dedicated memory; software renderers
// (Microsoft's basic and remote display adapters, virtual-machine adapters)
// and instances with no memory are skipped. Instances that describe the
// same card — a leftover key from a driver upgrade is the usual cause — are
// collapsed to one. Instances are read in name order so the output is stable.
func parseDisplayAdapters(reader DisplayAdapterReader) []*GpuDetectionResult {
	instances := reader.Instances()
	sort.Strings(instances)

	var results []*GpuDetectionResult
	seen := make(map[string]bool)
	for _, inst := range instances {
		if !isAdapterInstanceName(inst) {
			continue // "Properties", "Configuration", ... are not adapters
		}
		provider, _ := reader.StringValue(inst, adapterValueProviderName)
		vendor := gpuVendorFromProvider(provider)
		model, _ := reader.StringValue(inst, adapterValueDriverDesc)
		model = strings.TrimSpace(model)
		if vendor == "" {
			slog.Debug("display adapter skipped: not a supported GPU vendor", "instance", inst, "provider", provider, "model", model)
			continue
		}
		vramMB := adapterMemoryMB(reader, inst)
		if vramMB <= 0 {
			slog.Debug("display adapter skipped: no dedicated memory reported", "instance", inst, "model", model)
			continue
		}
		if model == "" {
			model = strings.TrimSpace(provider) + " GPU"
		}
		key := vendor + "|" + model + "|" + strconv.Itoa(int(vramMB))
		if seen[key] {
			continue
		}
		seen[key] = true
		results = append(results, &GpuDetectionResult{
			Model:  model,
			Vendor: vendor,
			VRAMMB: vramMB,
		})
	}
	return results
}

// isAdapterInstanceName reports whether a subkey name is an adapter instance
// ("0000", "0001", ...) rather than one of the class key's other children.
func isAdapterInstanceName(name string) bool {
	if len(name) != 4 {
		return false
	}
	for _, c := range name {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// gpuVendorFromProvider maps a driver ProviderName to the vendor token the
// head matches leaf GPU requirements against; "" for anything else.
func gpuVendorFromProvider(provider string) string {
	p := strings.ToLower(provider)
	switch {
	case strings.Contains(p, "nvidia"):
		return "nvidia"
	case strings.Contains(p, "advanced micro devices"), strings.Contains(p, "amd"), strings.Contains(p, "ati technologies"):
		return "amd"
	case strings.Contains(p, "intel"):
		return "intel"
	default:
		return ""
	}
}

// adapterMemoryMB reads an adapter's dedicated memory in MB, preferring the
// 64-bit qwMemorySize and falling back to the 32-bit MemorySize, each of
// which drivers have written as an integer or as little-endian bytes.
func adapterMemoryMB(reader DisplayAdapterReader, inst string) int32 {
	for _, name := range []string{adapterValueMemoryQword, adapterValueMemory} {
		if v, ok := reader.IntegerValue(inst, name); ok && v > 0 {
			return bytesToMB(v)
		}
		if b, ok := reader.BinaryValue(inst, name); ok {
			if v := littleEndianUint(b); v > 0 {
				return bytesToMB(v)
			}
		}
	}
	return 0
}

// littleEndianUint decodes a 4- or 8-byte little-endian registry value; 0
// for any other length.
func littleEndianUint(b []byte) uint64 {
	switch len(b) {
	case 4:
		return uint64(binary.LittleEndian.Uint32(b))
	case 8:
		return binary.LittleEndian.Uint64(b)
	default:
		return 0
	}
}

// bytesToMB converts a byte count to whole MB, saturating at the int32 range.
func bytesToMB(bytes uint64) int32 {
	mb := bytes / (1024 * 1024)
	if mb > uint64(^uint32(0)>>1) {
		return int32(^uint32(0) >> 1)
	}
	return int32(mb)
}

// mergeRegistryWithNvidiaSmi combines nvidia-smi's results with the
// registry's. nvidia-smi enumerates every NVIDIA card and is the only source
// of compute capability and exact memory, so when it reported anything the
// registry's NVIDIA entries are the same cards seen again and are dropped;
// when it reported nothing (driver tool absent) the registry's NVIDIA
// entries stand. Non-NVIDIA entries always pass through.
func mergeRegistryWithNvidiaSmi(smi, registry []*GpuDetectionResult) []*GpuDetectionResult {
	out := append([]*GpuDetectionResult(nil), smi...)
	for _, g := range registry {
		if g.Vendor == "nvidia" && len(smi) > 0 {
			continue
		}
		out = append(out, g)
	}
	return out
}
