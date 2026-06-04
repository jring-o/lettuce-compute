package runtime

import (
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// WorkUnitFromProto converts a single WorkUnitAssignment (one element of a
// RequestWorkUnitResponse's batch) into an internal WorkUnit.
func WorkUnitFromProto(a *lettucev1.WorkUnitAssignment) *WorkUnit {
	if a == nil {
		return nil
	}

	wu := &WorkUnit{
		ID:                        a.GetWorkUnitId(),
		LeafID:                    a.GetLeafId(),
		Runtime:                   a.GetRuntime(),
		InputData:                 a.GetInputData(),
		InputDataURL:              a.GetInputDataUrl(),
		CodeArtifactURL:           a.GetCodeArtifactUrl(),
		ParametersJSON:            a.GetParametersJson(),
		DeadlineSeconds:           a.GetDeadlineSeconds(),
		EnvVars:                   a.GetEnvVars(),
		RscFpopsEst:               a.GetRscFpopsEst(),
		ReservedUntilUnix:         a.GetReservedUntilUnix(),
		HasCheckpoint:             a.GetHasCheckpoint(),
		CheckpointSequence:        a.GetCheckpointSequence(),
		CheckpointIntervalSeconds: a.GetCheckpointIntervalSeconds(),
	}

	if spec := a.GetExecutionSpec(); spec != nil {
		wu.ExecutionSpec = ExecutionSpec{
			Binaries:        spec.GetBinaries(),
			BinaryChecksums: spec.GetBinaryChecksums(),
			Image:           spec.GetImage(),
			GPURequired:     spec.GetGpuRequired(),
			GPUType:         spec.GetGpuType(),
			MaxMemoryMB:     spec.GetMaxMemoryMb(),
			MaxDiskMB:       spec.GetMaxDiskMb(),
			NetworkAccess:   spec.GetNetworkAccess(),
		}
	}

	return wu
}

// MetricsToProto converts internal ExecutionMetrics to a proto ExecutionMetadata.
func MetricsToProto(m *ExecutionMetrics) *lettucev1.ExecutionMetadata {
	if m == nil {
		return nil
	}
	return &lettucev1.ExecutionMetadata{
		WallClockSeconds: m.WallClockSeconds,
		CpuSecondsUser:   m.CPUSecondsUser,
		CpuSecondsSystem: m.CPUSecondsSystem,
		CpuCoresUsed:     m.CPUCoresUsed,
		PeakMemoryMb:     m.PeakMemoryMB,
		DiskReadMb:       m.DiskReadMB,
		DiskWriteMb:      m.DiskWriteMB,
		GpuSeconds:       m.GPUSeconds,
		GpuModel:         m.GPUModel,
		GpuVramUsedMb:    m.GPUVRAMUsedMB,
	}
}
