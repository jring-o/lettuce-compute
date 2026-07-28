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
// All fields are pointers — nil means "not provided" (no change). The handler
// gates each field on its pointer being non-nil; omitempty keeps a partial
// update (e.g. a Go client marshaling this struct) from emitting a wall of
// explicit nulls on the wire. (The handler also treats an explicit null as
// "not provided", so omitempty is hygiene, not the correctness boundary.)
type UpdateLeafRequest struct {
	Name                 *string               `json:"name,omitempty"`
	Description          *string               `json:"description,omitempty"`
	ResearchArea         *[]string             `json:"research_area,omitempty"`
	ExecutionConfig      *ExecutionConfig      `json:"execution_config,omitempty"`
	ValidationConfig     *ValidationConfig     `json:"validation_config,omitempty"`
	FaultToleranceConfig *FaultToleranceConfig `json:"fault_tolerance_config,omitempty"`
	DataConfig           *DataConfig           `json:"data_config,omitempty"`
	CreditConfig         *CreditConfig         `json:"credit_config,omitempty"`
	ResourceRequirements *ResourceRequirements `json:"resource_requirements,omitempty"`
	IsOngoing            *bool                 `json:"is_ongoing,omitempty"`
	Visibility           *LeafVisibility    `json:"visibility,omitempty"`
	StatsCacheSeconds    *int                  `json:"stats_cache_seconds,omitempty"`
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
	// ActiveHosts counts distinct active MACHINES (a volunteer on N machines under
	// one identity key is 1 active volunteer but N active hosts).
	ActiveHosts int      `json:"active_hosts"`
	ProgressPct *float64 `json:"progress_pct"`
	CreatedAt   time.Time `json:"created_at"`
}

// resourceSubset is the abbreviated resource requirements for list responses.
//
// The abbreviation is in WHICH fields appear — never in which copy of a field is
// reported. Every field here is sourced from the one dispatch actually gates on,
// so the catalog cannot advertise a requirement the scheduler does not apply:
// memory from execution_config.max_memory_mb (FindNextAssignable's max_memory_mb
// predicate), VRAM from resource_requirements.min_gpu_vram_mb, and gpu_required
// from EITHER gpu_required flag, matching the presence gate. TB-20: VRAM was
// derived from execution_config.min_vram_gb — a field no dispatch path reads —
// so a leaf gating at 4000 MB published 4096, and gpu_required read only
// resource_requirements, so a leaf that set just the execution_config flag was
// published as a CPU leaf while reaching no GPU-less volunteer.
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
			GPURequired:  p.ResourceRequirements.GPURequired || p.ExecutionConfig.GPURequired,
			GPUType:      p.ExecutionConfig.GPUType,
			GPUMinVRAMMB: p.ResourceRequirements.MinGPUVRAMMB,
			MinCPUCores:  p.ResourceRequirements.MinCPUCores,
			MaxMemoryMB:  p.ExecutionConfig.MaxMemoryMB,
		},
		Runtime:           p.ExecutionConfig.Runtime,
		StatsCacheSeconds: p.StatsCacheSeconds,
		// ActiveVolunteers is populated by the list handler (handleList) from the
		// live rolling-window count; the pure DTO conversion has no DB access.
		ActiveVolunteers: 0,
		ProgressPct:      nil, // per-leaf aggregate progress not yet implemented
		CreatedAt:        p.CreatedAt,
	}
}
