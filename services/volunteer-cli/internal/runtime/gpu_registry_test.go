package runtime

import (
	"encoding/binary"
	"reflect"
	"testing"
)

// fakeAdapterReader is an in-memory DisplayAdapterReader: instance name ->
// value name -> value, where a value is a string, a uint64 (REG_DWORD /
// REG_QWORD) or a []byte (REG_BINARY). A nil map has no instances.
type fakeAdapterReader map[string]map[string]any

func (f fakeAdapterReader) Instances() []string {
	names := make([]string, 0, len(f))
	for n := range f {
		names = append(names, n)
	}
	return names
}

func (f fakeAdapterReader) StringValue(instance, name string) (string, bool) {
	v, ok := f[instance][name].(string)
	return v, ok
}

func (f fakeAdapterReader) IntegerValue(instance, name string) (uint64, bool) {
	v, ok := f[instance][name].(uint64)
	return v, ok
}

func (f fakeAdapterReader) BinaryValue(instance, name string) ([]byte, bool) {
	v, ok := f[instance][name].([]byte)
	return v, ok
}

func le32(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }
func le64(v uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, v); return b }

const gib = 1024 * 1024 * 1024

// The registry-derived adapter list: vendors mapped from ProviderName,
// memory read from qwMemorySize (QWORD) or MemorySize (DWORD / BINARY),
// software renderers and memory-less instances skipped, non-instance
// subkeys ignored, duplicate instances of one card collapsed, and the
// output in instance order.
func TestParseDisplayAdapters(t *testing.T) {
	reader := fakeAdapterReader{
		"0000": {
			"DriverDesc":                       "AMD Radeon RX 7800 XT",
			"ProviderName":                     "Advanced Micro Devices, Inc.",
			"HardwareInformation.qwMemorySize": uint64(16 * gib),
		},
		"0001": {
			"DriverDesc":                       "NVIDIA GeForce RTX 3070",
			"ProviderName":                     "NVIDIA",
			"HardwareInformation.qwMemorySize": le64(8 * gib), // QWORD stored as bytes
		},
		"0002": {
			"DriverDesc":                     "Intel(R) UHD Graphics 770",
			"ProviderName":                   "Intel Corporation",
			"HardwareInformation.MemorySize": uint64(128 * 1024 * 1024), // DWORD
		},
		"0003": {
			"DriverDesc":                     "Radeon RX 580 Series",
			"ProviderName":                   "ATI Technologies Inc.",
			"HardwareInformation.MemorySize": le32(2 * gib), // REG_BINARY, 4 bytes (the 32-bit field cannot hold 4 GiB or more)
		},
		"0004": { // software renderer: skipped by vendor
			"DriverDesc":                       "Microsoft Basic Display Adapter",
			"ProviderName":                     "Microsoft",
			"HardwareInformation.qwMemorySize": uint64(1 * gib),
		},
		"0005": { // known vendor but no dedicated memory: skipped
			"DriverDesc":   "NVIDIA GeForce (stale)",
			"ProviderName": "NVIDIA",
		},
		"0006": { // leftover instance of the same card as 0000: collapsed
			"DriverDesc":                       "AMD Radeon RX 7800 XT",
			"ProviderName":                     "Advanced Micro Devices, Inc.",
			"HardwareInformation.qwMemorySize": uint64(16 * gib),
		},
		"Properties":    {"DriverDesc": "not an adapter"},
		"Configuration": {"DriverDesc": "not an adapter"},
	}

	got := parseDisplayAdapters(reader)
	want := []*GpuDetectionResult{
		{Model: "AMD Radeon RX 7800 XT", Vendor: "amd", VRAMMB: 16 * 1024},
		{Model: "NVIDIA GeForce RTX 3070", Vendor: "nvidia", VRAMMB: 8 * 1024},
		{Model: "Intel(R) UHD Graphics 770", Vendor: "intel", VRAMMB: 128},
		{Model: "Radeon RX 580 Series", Vendor: "amd", VRAMMB: 2 * 1024},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d adapters, want %d: %+v", len(got), len(want), dump(got))
	}
	for i := range want {
		if !reflect.DeepEqual(*got[i], *want[i]) {
			t.Errorf("adapter %d = %+v, want %+v", i, *got[i], *want[i])
		}
	}
}

func TestParseDisplayAdapters_EmptyAndNoInstances(t *testing.T) {
	if got := parseDisplayAdapters(fakeAdapterReader{}); len(got) != 0 {
		t.Errorf("empty registry produced %+v", dump(got))
	}
	if got := parseDisplayAdapters(fakeAdapterReader(nil)); len(got) != 0 {
		t.Errorf("nil registry produced %+v", dump(got))
	}
}

func TestParseDisplayAdapters_ModelFallsBackToProvider(t *testing.T) {
	reader := fakeAdapterReader{
		"0000": {
			"ProviderName":                     "NVIDIA",
			"HardwareInformation.qwMemorySize": uint64(2 * gib),
		},
	}
	got := parseDisplayAdapters(reader)
	if len(got) != 1 || got[0].Model != "NVIDIA GPU" {
		t.Errorf("got %+v, want one adapter named after its provider", dump(got))
	}
}

func TestGPUVendorFromProvider(t *testing.T) {
	cases := map[string]string{
		"NVIDIA":                       "nvidia",
		"NVIDIA Corporation":           "nvidia",
		"Advanced Micro Devices, Inc.": "amd",
		"AMD":                          "amd",
		"ATI Technologies Inc.":        "amd",
		"Intel Corporation":            "intel",
		"Microsoft":                    "",
		"VMware, Inc.":                 "",
		"":                             "",
	}
	for provider, want := range cases {
		if got := gpuVendorFromProvider(provider); got != want {
			t.Errorf("gpuVendorFromProvider(%q) = %q, want %q", provider, got, want)
		}
	}
}

// nvidia-smi is authoritative for NVIDIA cards when it answers; the registry
// fills in only when it does not, and never for other vendors.
func TestMergeRegistryWithNvidiaSmi(t *testing.T) {
	smi := []*GpuDetectionResult{{Model: "NVIDIA GeForce RTX 3070", Vendor: "nvidia", VRAMMB: 8192, ComputeCapability: "8.6"}}
	reg := []*GpuDetectionResult{
		{Model: "NVIDIA GeForce RTX 3070", Vendor: "nvidia", VRAMMB: 8192},
		{Model: "AMD Radeon RX 7800 XT", Vendor: "amd", VRAMMB: 16384},
	}

	got := mergeRegistryWithNvidiaSmi(smi, reg)
	if len(got) != 2 {
		t.Fatalf("got %d GPUs, want 2 (the NVIDIA card once, with compute capability, plus the AMD card): %+v", len(got), dump(got))
	}
	if got[0].Vendor != "nvidia" || got[0].ComputeCapability != "8.6" {
		t.Errorf("first = %+v, want nvidia-smi's entry with its compute capability", *got[0])
	}
	if got[1].Vendor != "amd" {
		t.Errorf("second = %+v, want the registry's AMD card", *got[1])
	}

	// Without nvidia-smi the registry's NVIDIA entry stands.
	got = mergeRegistryWithNvidiaSmi(nil, reg)
	if len(got) != 2 || got[0].Vendor != "nvidia" || got[0].ComputeCapability != "" {
		t.Errorf("without nvidia-smi got %+v, want both registry entries", dump(got))
	}
}

func dump(gpus []*GpuDetectionResult) []GpuDetectionResult {
	out := make([]GpuDetectionResult, 0, len(gpus))
	for _, g := range gpus {
		out = append(out, *g)
	}
	return out
}
