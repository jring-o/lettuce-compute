package volunteer

import (
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// SchedulingMode represents when a volunteer is available for work.
type SchedulingMode string

const (
	ScheduleAlways   SchedulingMode = "ALWAYS"
	ScheduleWhenIdle SchedulingMode = "WHEN_IDLE"
	ScheduleScheduled SchedulingMode = "SCHEDULED"
)

// GpuInfo describes a single GPU available on a volunteer machine.
type GpuInfo struct {
	Model             string `json:"model"`
	Vendor            string `json:"vendor"`
	VRAMMB            int    `json:"vram_mb"`
	MaxVRAMPct        int    `json:"max_vram_pct"`
	ComputeCapability string `json:"compute_capability"`
}

// HardwareCapabilities describes the hardware a volunteer makes available.
type HardwareCapabilities struct {
	CPUCores         int       `json:"cpu_cores"`
	CPUModel         string    `json:"cpu_model"`
	MaxCPUCores      int       `json:"max_cpu_cores"`
	MemoryTotalMB    int       `json:"memory_total_mb"`
	MaxMemoryMB      int       `json:"max_memory_mb"`
	DiskAvailableMB  int64     `json:"disk_available_mb"`
	MaxDiskMB        int64     `json:"max_disk_mb"`
	MaxBandwidthMbps int       `json:"max_bandwidth_mbps"`
	GPUs             []GpuInfo `json:"gpus"`
	BenchmarkFPOPS   float64   `json:"benchmark_fpops,omitempty"` // CPU benchmark (FP ops/sec)
	// Hardware-class inputs for Homogeneous Redundancy (HR). See HRClass.
	OS               string    `json:"os,omitempty"`         // GOOS: linux, darwin, windows
	CPUArch          string    `json:"cpu_arch,omitempty"`   // GOARCH: amd64, arm64
	CPUVendor        string    `json:"cpu_vendor,omitempty"` // GenuineIntel, AuthenticAMD, Apple, ...
}

// HRClass returns the volunteer's Homogeneous-Redundancy hardware class — a coarse
// "<vendor>/<os>/<arch>" key (e.g. "GenuineIntel/linux/amd64"). All redundant copies of a
// work unit are pinned to one class so their floating-point results are bit-comparable even
// for engines that are not portably deterministic. Missing components collapse to "unknown"
// so a class is always well-formed. Granularity is deliberately coarse to start (vendor +
// OS + arch); it can be tightened later (e.g. CPU microarchitecture) without schema change.
func (hw HardwareCapabilities) HRClass() string {
	vendor, os, arch := hw.CPUVendor, hw.OS, hw.CPUArch
	if vendor == "" {
		vendor = "unknown"
	}
	if os == "" {
		os = "unknown"
	}
	if arch == "" {
		arch = "unknown"
	}
	return vendor + "/" + os + "/" + arch
}

// Volunteer is a compute contributor identified by an Ed25519 keypair.
type Volunteer struct {
	ID                       types.ID             `json:"id"`
	NumericID                int                  `json:"numeric_id"`
	PublicKey                []byte               `json:"public_key"`
	UserID                   *types.ID            `json:"user_id,omitempty"`
	DisplayName              *string              `json:"display_name,omitempty"`
	HardwareCapabilities     HardwareCapabilities `json:"hardware_capabilities"`
	AvailableRuntimes        []string             `json:"available_runtimes"`
	SchedulingMode           SchedulingMode       `json:"scheduling_mode"`
	ScheduleConfig           map[string]any       `json:"schedule_config,omitempty"`
	IsActive                 bool                 `json:"is_active"`
	LastSeenAt               *time.Time           `json:"last_seen_at,omitempty"`
	TotalWorkUnitsCompleted  int                  `json:"total_work_units_completed"`
	TotalWorkUnitsRejected   int                  `json:"total_work_units_rejected"`
	RegisteredAt             time.Time            `json:"registered_at"`
	CreatedAt                time.Time            `json:"created_at"`
	UpdatedAt                time.Time            `json:"updated_at"`
}

// HardwareCapabilitiesFromProto converts a protobuf HardwareCapabilities message to a Go struct.
func HardwareCapabilitiesFromProto(pb *lettucev1.HardwareCapabilities) HardwareCapabilities {
	if pb == nil {
		return HardwareCapabilities{}
	}

	hw := HardwareCapabilities{
		CPUCores:         int(pb.CpuCores),
		CPUModel:         pb.CpuModel,
		MaxCPUCores:      int(pb.MaxCpuCores),
		MemoryTotalMB:    int(pb.MemoryTotalMb),
		MaxMemoryMB:      int(pb.MaxMemoryMb),
		DiskAvailableMB:  pb.DiskAvailableMb,
		MaxDiskMB:        pb.MaxDiskMb,
		MaxBandwidthMbps: int(pb.MaxBandwidthMbps),
		BenchmarkFPOPS:   pb.BenchmarkFpops,
		OS:               pb.Os,
		CPUArch:          pb.CpuArch,
		CPUVendor:        pb.CpuVendor,
	}

	for _, g := range pb.Gpus {
		hw.GPUs = append(hw.GPUs, GpuInfo{
			Model:             g.Model,
			Vendor:            g.Vendor,
			VRAMMB:            int(g.VramMb),
			MaxVRAMPct:        int(g.MaxVramPct),
			ComputeCapability: g.ComputeCapability,
		})
	}

	return hw
}

// HardwareCapabilitiesToProto converts a Go HardwareCapabilities struct to a protobuf message.
func HardwareCapabilitiesToProto(hw HardwareCapabilities) *lettucev1.HardwareCapabilities {
	pb := &lettucev1.HardwareCapabilities{
		CpuCores:         int32(hw.CPUCores),
		CpuModel:         hw.CPUModel,
		MaxCpuCores:      int32(hw.MaxCPUCores),
		MemoryTotalMb:    int32(hw.MemoryTotalMB),
		MaxMemoryMb:      int32(hw.MaxMemoryMB),
		DiskAvailableMb:  hw.DiskAvailableMB,
		MaxDiskMb:        hw.MaxDiskMB,
		MaxBandwidthMbps: int32(hw.MaxBandwidthMbps),
		BenchmarkFpops:   hw.BenchmarkFPOPS,
		Os:               hw.OS,
		CpuArch:          hw.CPUArch,
		CpuVendor:        hw.CPUVendor,
	}

	for _, g := range hw.GPUs {
		pb.Gpus = append(pb.Gpus, &lettucev1.GpuInfo{
			Model:             g.Model,
			Vendor:            g.Vendor,
			VramMb:            int32(g.VRAMMB),
			MaxVramPct:        int32(g.MaxVRAMPct),
			ComputeCapability: g.ComputeCapability,
		})
	}

	return pb
}
