package mapreduce

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// Generate creates map-phase work units by splitting input data according to
// the project's splitting_strategy. Each chunk becomes one work unit with the
// chunk data in input_data (inline) or input_data_ref (external).
func Generate(
	ctx context.Context,
	proj *leaf.Leaf,
	parameterSpace map[string]interface{},
	batchSize int,
	wuRepo workunit.WorkUnitRepository,
	batchRepo workunit.BatchRepository,
) (*workunit.GenerateResult, error) {
	if proj.TaskPattern != leaf.PatternMapReduce {
		return nil, apierror.ValidationError(
			fmt.Sprintf("map-reduce generator requires MAP_REDUCE task pattern, got %q", proj.TaskPattern),
			nil,
		)
	}

	// Extract input data or reference from parameterSpace.
	inputData, hasInlineData := parameterSpace["input_data"]
	inputDataRef, hasRefData := parameterSpace["input_data_ref"]

	if !hasInlineData && !hasRefData {
		return nil, apierror.ValidationError(
			"input_data or input_data_ref is required for map-reduce projects",
			nil,
		)
	}

	// Get splitting strategy from project config.
	if proj.DataConfig.SplittingStrategy == nil || *proj.DataConfig.SplittingStrategy == "" {
		return nil, apierror.ValidationError(
			"splitting_strategy is required in project data_config for map-reduce projects",
			nil,
		)
	}

	splitter, err := NewSplitter(*proj.DataConfig.SplittingStrategy)
	if err != nil {
		return nil, err
	}

	splittingConfig := proj.DataConfig.SplittingConfig
	if splittingConfig == nil {
		splittingConfig = map[string]any{}
	}

	batchSize = generate.ClampBatchSize(batchSize)

	// Resolve work unit field defaults.
	codeArtifactRef := generate.ResolveCodeArtifactRef(proj)
	deadlineSeconds := generate.ResolveDeadlineSeconds(proj)
	maxReassignments := proj.FaultToleranceConfig.MaxReassignments

	var chunks []Chunk

	if hasRefData {
		// External reference mode: generate chunk references.
		refStr, ok := inputDataRef.(string)
		if !ok {
			return nil, apierror.ValidationError("input_data_ref must be a string URL", nil)
		}

		if hasInlineData {
			// If both provided, split inline data but generate references.
			rawData, err := extractRawData(inputData)
			if err != nil {
				return nil, err
			}
			chunks, err = splitter.Split(rawData, splittingConfig)
			if err != nil {
				return nil, err
			}
			// Convert to reference-based chunks.
			for i := range chunks {
				ref := fmt.Sprintf("%s#chunk_%d", refStr, i)
				chunks[i].DataRef = &ref
				chunks[i].Data = nil
			}
		} else {
			// Reference-only: create a single chunk pointing to the external data.
			chunks = []Chunk{{
				Index:   0,
				DataRef: &refStr,
				Metadata: map[string]any{
					"source": refStr,
				},
			}}
		}
	} else {
		// Inline data mode: split the data.
		rawData, err := extractRawData(inputData)
		if err != nil {
			return nil, err
		}
		chunks, err = splitter.Split(rawData, splittingConfig)
		if err != nil {
			return nil, err
		}
	}

	totalChunks := len(chunks)
	if totalChunks == 0 {
		return nil, apierror.ValidationError("splitting produced no chunks", nil)
	}

	// Split into batches.
	numBatches := (totalChunks + batchSize - 1) / batchSize

	result := &workunit.GenerateResult{
		BatchIDs: make([]types.ID, 0, numBatches),
		Status:   "complete",
	}

	// Determine starting sequence number.
	nextSeqNum, err := generate.ResolveNextSequenceNumber(ctx, proj.ID, batchRepo)
	if err != nil {
		return nil, err
	}

	for batchIdx := 0; batchIdx < numBatches; batchIdx++ {
		start := batchIdx * batchSize
		end := start + batchSize
		if end > totalChunks {
			end = totalChunks
		}
		batchChunks := chunks[start:end]

		batch := &workunit.Batch{
			LeafID:      proj.ID,
			SequenceNumber: nextSeqNum + batchIdx,
			TotalWorkUnits: len(batchChunks),
		}
		if err := batchRepo.Create(ctx, batch); err != nil {
			return nil, apierror.Internal(fmt.Sprintf("create batch %d", batchIdx), err)
		}

		wus := make([]*workunit.WorkUnit, len(batchChunks))
		for i, chunk := range batchChunks {
			metadata, err := json.Marshal(chunk.Metadata)
			if err != nil {
				return nil, apierror.Internal("failed to marshal chunk metadata", err)
			}

			wu := &workunit.WorkUnit{
				LeafID:        proj.ID,
				BatchID:          &batch.ID,
				State:            workunit.WorkUnitStateCreated,
				Priority:         workunit.WorkUnitPriorityNormal,
				CodeArtifactRef:  codeArtifactRef,
				Parameters:       metadata,
				DeadlineSeconds:  deadlineSeconds,
				MaxReassignments: maxReassignments,
			}

			if chunk.DataRef != nil {
				wu.InputDataRef = chunk.DataRef
			} else {
				wu.InputData = chunk.Data
			}

			wus[i] = wu
		}

		if err := wuRepo.BulkCreate(ctx, wus); err != nil {
			return nil, apierror.Internal(fmt.Sprintf("bulk create work units for batch %d", batchIdx), err)
		}

		if _, err := wuRepo.BulkTransitionByBatch(ctx, batch.ID, workunit.WorkUnitStateCreated, workunit.WorkUnitStateQueued); err != nil {
			return nil, apierror.Internal(fmt.Sprintf("transition batch %d to queued", batchIdx), err)
		}

		result.BatchIDs = append(result.BatchIDs, batch.ID)
		result.WorkUnitsCreated += len(batchChunks)
	}

	slog.InfoContext(ctx, "map-reduce work units generated",
		"leaf_id", proj.ID,
		"work_units", result.WorkUnitsCreated,
		"batches", len(result.BatchIDs),
		"splitting_strategy", *proj.DataConfig.SplittingStrategy,
	)

	return result, nil
}

// extractRawData converts the input_data value (string or JSON) to raw bytes.
func extractRawData(inputData any) ([]byte, error) {
	switch v := inputData.(type) {
	case string:
		return []byte(v), nil
	case []byte:
		return v, nil
	default:
		// Try to marshal as JSON.
		data, err := json.Marshal(v)
		if err != nil {
			return nil, apierror.ValidationError("input_data must be a string or JSON value", nil)
		}
		return data, nil
	}
}

