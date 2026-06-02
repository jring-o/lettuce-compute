package runtime

import (
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

func TestWorkUnitFromProto(t *testing.T) {
	resp := &lettucev1.WorkUnitAssignment{
		WorkUnitId:      "wu-123",
		LeafId:          "proj-456",
		Runtime:         "native",
		InputData:       []byte("test input"),
		InputDataUrl:    "https://example.com/input.dat",
		CodeArtifactUrl: "https://example.com/binary",
		ParametersJson:  `{"key":"value"}`,
		DeadlineSeconds: 3600,
		EnvVars:         map[string]string{"FOO": "bar"},
		ExecutionSpec: &lettucev1.ExecutionSpec{
			Binaries:        map[string]string{"linux_amd64": "https://example.com/bin"},
			BinaryChecksums: map[string]string{"linux_amd64": "abc123"},
			Image:           "alpine:latest",
			GpuRequired:     true,
			GpuType:         "nvidia",
			MaxMemoryMb:     4096,
			MaxDiskMb:       10240,
			NetworkAccess:   true,
		},
	}

	wu := WorkUnitFromProto(resp)

	if wu.ID != "wu-123" {
		t.Errorf("ID = %q, want %q", wu.ID, "wu-123")
	}
	if wu.LeafID != "proj-456" {
		t.Errorf("LeafID = %q, want %q", wu.LeafID, "proj-456")
	}
	if wu.Runtime != "native" {
		t.Errorf("Runtime = %q, want %q", wu.Runtime, "native")
	}
	if string(wu.InputData) != "test input" {
		t.Errorf("InputData = %q, want %q", wu.InputData, "test input")
	}
	if wu.InputDataURL != "https://example.com/input.dat" {
		t.Errorf("InputDataURL = %q", wu.InputDataURL)
	}
	if wu.CodeArtifactURL != "https://example.com/binary" {
		t.Errorf("CodeArtifactURL = %q", wu.CodeArtifactURL)
	}
	if wu.ParametersJSON != `{"key":"value"}` {
		t.Errorf("ParametersJSON = %q", wu.ParametersJSON)
	}
	if wu.DeadlineSeconds != 3600 {
		t.Errorf("DeadlineSeconds = %d, want 3600", wu.DeadlineSeconds)
	}
	if wu.EnvVars["FOO"] != "bar" {
		t.Errorf("EnvVars[FOO] = %q, want %q", wu.EnvVars["FOO"], "bar")
	}
	if wu.ExecutionSpec.Binaries["linux_amd64"] != "https://example.com/bin" {
		t.Errorf("Binaries[linux_amd64] = %q", wu.ExecutionSpec.Binaries["linux_amd64"])
	}
	if wu.ExecutionSpec.BinaryChecksums["linux_amd64"] != "abc123" {
		t.Errorf("BinaryChecksums[linux_amd64] = %q, want %q", wu.ExecutionSpec.BinaryChecksums["linux_amd64"], "abc123")
	}
	if !wu.ExecutionSpec.GPURequired {
		t.Error("GPURequired = false, want true")
	}
	if wu.ExecutionSpec.GPUType != "nvidia" {
		t.Errorf("GPUType = %q, want %q", wu.ExecutionSpec.GPUType, "nvidia")
	}
	if wu.ExecutionSpec.MaxMemoryMB != 4096 {
		t.Errorf("MaxMemoryMB = %d, want 4096", wu.ExecutionSpec.MaxMemoryMB)
	}
	if wu.ExecutionSpec.MaxDiskMB != 10240 {
		t.Errorf("MaxDiskMB = %d, want 10240", wu.ExecutionSpec.MaxDiskMB)
	}
	if !wu.ExecutionSpec.NetworkAccess {
		t.Error("NetworkAccess = false, want true")
	}
}

func TestWorkUnitFromProto_Nil(t *testing.T) {
	if wu := WorkUnitFromProto(nil); wu != nil {
		t.Errorf("expected nil, got %v", wu)
	}
}

func TestWorkUnitFromProto_NilSpec(t *testing.T) {
	resp := &lettucev1.WorkUnitAssignment{
		WorkUnitId: "wu-1",
		Runtime:    "native",
	}
	wu := WorkUnitFromProto(resp)
	if wu.ID != "wu-1" {
		t.Errorf("ID = %q, want %q", wu.ID, "wu-1")
	}
	if len(wu.ExecutionSpec.Binaries) != 0 {
		t.Errorf("expected empty Binaries, got %v", wu.ExecutionSpec.Binaries)
	}
}

func TestWorkUnitFromProto_CheckpointFields(t *testing.T) {
	resp := &lettucev1.WorkUnitAssignment{
		WorkUnitId:                "wu-ckp",
		LeafId:                    "proj-1",
		Runtime:                   "native",
		HasCheckpoint:             true,
		CheckpointSequence:        5,
		CheckpointIntervalSeconds: 120,
	}

	wu := WorkUnitFromProto(resp)

	if !wu.HasCheckpoint {
		t.Error("HasCheckpoint = false, want true")
	}
	if wu.CheckpointSequence != 5 {
		t.Errorf("CheckpointSequence = %d, want 5", wu.CheckpointSequence)
	}
	if wu.CheckpointIntervalSeconds != 120 {
		t.Errorf("CheckpointIntervalSeconds = %d, want 120", wu.CheckpointIntervalSeconds)
	}
}

func TestWorkUnitFromProto_CheckpointFieldsDefault(t *testing.T) {
	// When checkpoint fields are not set, they should be zero-valued.
	resp := &lettucev1.WorkUnitAssignment{
		WorkUnitId: "wu-no-ckp",
		Runtime:    "native",
	}

	wu := WorkUnitFromProto(resp)

	if wu.HasCheckpoint {
		t.Error("HasCheckpoint = true, want false")
	}
	if wu.CheckpointSequence != 0 {
		t.Errorf("CheckpointSequence = %d, want 0", wu.CheckpointSequence)
	}
	if wu.CheckpointIntervalSeconds != 0 {
		t.Errorf("CheckpointIntervalSeconds = %d, want 0", wu.CheckpointIntervalSeconds)
	}
}

func TestMetricsToProto(t *testing.T) {
	m := &ExecutionMetrics{
		WallClockSeconds: 120,
		CPUSecondsUser:   95.5,
		CPUSecondsSystem: 10.2,
		CPUCoresUsed:     2,
		PeakMemoryMB:     512,
		DiskReadMB:       100,
		DiskWriteMB:      50,
		GPUSeconds:       12.5,
		GPUModel:         "NVIDIA RTX 3080",
		GPUVRAMUsedMB:    4096,
	}

	proto := MetricsToProto(m)

	if proto.WallClockSeconds != 120 {
		t.Errorf("WallClockSeconds = %d, want 120", proto.WallClockSeconds)
	}
	if proto.CpuSecondsUser != 95.5 {
		t.Errorf("CpuSecondsUser = %f, want 95.5", proto.CpuSecondsUser)
	}
	if proto.CpuSecondsSystem != 10.2 {
		t.Errorf("CpuSecondsSystem = %f, want 10.2", proto.CpuSecondsSystem)
	}
	if proto.CpuCoresUsed != 2 {
		t.Errorf("CpuCoresUsed = %d, want 2", proto.CpuCoresUsed)
	}
	if proto.PeakMemoryMb != 512 {
		t.Errorf("PeakMemoryMb = %d, want 512", proto.PeakMemoryMb)
	}
	if proto.DiskReadMb != 100 {
		t.Errorf("DiskReadMb = %d, want 100", proto.DiskReadMb)
	}
	if proto.DiskWriteMb != 50 {
		t.Errorf("DiskWriteMb = %d, want 50", proto.DiskWriteMb)
	}
	if proto.GpuSeconds != 12.5 {
		t.Errorf("GpuSeconds = %f, want 12.5", proto.GpuSeconds)
	}
	if proto.GpuModel != "NVIDIA RTX 3080" {
		t.Errorf("GpuModel = %q, want %q", proto.GpuModel, "NVIDIA RTX 3080")
	}
	if proto.GpuVramUsedMb != 4096 {
		t.Errorf("GpuVramUsedMb = %d, want 4096", proto.GpuVramUsedMb)
	}
}

func TestMetricsToProto_Nil(t *testing.T) {
	if proto := MetricsToProto(nil); proto != nil {
		t.Errorf("expected nil, got %v", proto)
	}
}
