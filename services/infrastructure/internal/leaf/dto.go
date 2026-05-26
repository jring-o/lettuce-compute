package leaf

import (
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// CreateLeafRequest is the JSON body for POST /api/v1/leafs.
type CreateLeafRequest struct {
	Name            string            `json:"name"`
	Description     string            `json:"description"`
	ResearchArea    []string          `json:"research_area"`
	TaskPattern     TaskPattern       `json:"task_pattern"`
	IsOngoing       bool              `json:"is_ongoing"`
	Visibility      LeafVisibility `json:"visibility"`
	CreatorID       *types.ID         `json:"creator_id"`
}

// UpdateLeafRequest is the JSON body for PUT /api/v1/leafs/{leaf_id}.
// All fields are pointers — nil means "not provided" (no change).
type UpdateLeafRequest struct {
	Name                 *string               `json:"name"`
	Description          *string               `json:"description"`
	ResearchArea         *[]string             `json:"research_area"`
	ExecutionConfig      *ExecutionConfig      `json:"execution_config"`
	ValidationConfig     *ValidationConfig     `json:"validation_config"`
	FaultToleranceConfig *FaultToleranceConfig `json:"fault_tolerance_config"`
	DataConfig           *DataConfig           `json:"data_config"`
	CreditConfig         *CreditConfig         `json:"credit_config"`
	ResourceRequirements *ResourceRequirements `json:"resource_requirements"`
	IsOngoing            *bool                 `json:"is_ongoing"`
	Visibility           *LeafVisibility    `json:"visibility"`
	StatsCacheSeconds    *int                  `json:"stats_cache_seconds"`
}

// LeafSummary is the abbreviated response for list endpoints.
type LeafSummary struct {
	ID                   types.ID          `json:"id"`
	Name                 string            `json:"name"`
	Slug                 string            `json:"slug"`
	Description          string            `json:"description"`
	ResearchArea         []string          `json:"research_area"`
	State                LeafState      `json:"state"`
	TaskPattern          TaskPattern       `json:"task_pattern"`
	IsOngoing            bool              `json:"is_ongoing"`
	Visibility           LeafVisibility `json:"visibility"`
	ResourceRequirements resourceSubset    `json:"resource_requirements"`
	Runtime              string            `json:"runtime"`
	StatsCacheSeconds    int               `json:"stats_cache_seconds"`
	ActiveVolunteers     int               `json:"active_volunteers"`
	ProgressPct          *float64          `json:"progress_pct"`
	CreatedAt            time.Time         `json:"created_at"`
}

// resourceSubset is the abbreviated resource requirements for list responses.
// Memory is reported as the container limit (execution_config.max_memory_mb) —
// the single source of truth, also the scheduler's matching floor.
type resourceSubset struct {
	GPURequired  bool `json:"gpu_required"`
	GPUType      string `json:"gpu_type,omitempty"`
	GPUMinVRAMMB int    `json:"gpu_min_vram_mb,omitempty"`
	MinCPUCores  int    `json:"min_cpu_cores"`
	MaxMemoryMB  int    `json:"max_memory_mb"`
}

// ToLeafSummary converts a full Leaf to a LeafSummary.
func ToLeafSummary(p *Leaf) LeafSummary {
	desc := p.Description
	runes := []rune(desc)
	if len(runes) > 200 {
		desc = string(runes[:200]) + "..."
	}

	return LeafSummary{
		ID:           p.ID,
		Name:         p.Name,
		Slug:         p.Slug,
		Description:  desc,
		ResearchArea: p.ResearchArea,
		State:        p.State,
		TaskPattern:  p.TaskPattern,
		IsOngoing:    p.IsOngoing,
		Visibility:   p.Visibility,
		ResourceRequirements: resourceSubset{
			GPURequired:  p.ResourceRequirements.GPURequired,
			GPUType:      p.ExecutionConfig.GPUType,
			GPUMinVRAMMB: p.ExecutionConfig.MinVRAMGB * 1024,
			MinCPUCores:  p.ResourceRequirements.MinCPUCores,
			MaxMemoryMB:  p.ExecutionConfig.MaxMemoryMB,
		},
		Runtime:           p.ExecutionConfig.Runtime,
		StatsCacheSeconds: p.StatsCacheSeconds,
		ActiveVolunteers:  0,   // v0.2: always 0
		ProgressPct:       nil, // v0.2: always null
		CreatedAt:         p.CreatedAt,
	}
}
