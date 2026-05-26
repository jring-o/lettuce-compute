package runtime

import (
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// WorkUnitFromProto converts a RequestWorkUnitResponse into an internal WorkUnit.
func WorkUnitFromProto(resp *lettucev1.RequestWorkUnitResponse) *WorkUnit {
	if resp == nil {
		return nil
	}

	wu := &WorkUnit{
		ID:                        resp.GetWorkUnitId(),
		LeafID:                    resp.GetProjectId(),
		Runtime:                   resp.GetRuntime(),
		InputData:                 resp.GetInputData(),
		InputDataURL:              resp.GetInputDataUrl(),
		CodeArtifactURL:           resp.GetCodeArtifactUrl(),
		ParametersJSON:            resp.GetParametersJson(),
		DeadlineSeconds:           resp.GetDeadlineSeconds(),
		EnvVars:                   resp.GetEnvVars(),
		RscFpopsEst:               resp.GetRscFpopsEst(),
		HasCheckpoint:             resp.GetHasCheckpoint(),
		CheckpointSequence:        resp.GetCheckpointSequence(),
		CheckpointIntervalSeconds: resp.GetCheckpointIntervalSeconds(),
	}

	if spec := resp.GetExecutionSpec(); spec != nil {
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
