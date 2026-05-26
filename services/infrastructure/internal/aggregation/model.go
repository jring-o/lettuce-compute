package aggregation

import (
	"encoding/json"
	"time"
)

// AggregateResult is the output of an aggregation run.
type AggregateResult struct {
	Status              string          `json:"status"`                         // complete, partial, no_aggregation
	Format              string          `json:"format,omitempty"`               // json, csv
	Result              json.RawMessage `json:"result,omitempty"`               // aggregated output (JSON)
	ResultCSV           string          `json:"result_csv,omitempty"`           // aggregated output (CSV)
	Message             string          `json:"message,omitempty"`              // for no_aggregation status
	WorkUnitsAggregated int             `json:"work_units_aggregated"`          // count of aggregated work units
	WorkUnitsValidated  int             `json:"work_units_validated,omitempty"` // for no_aggregation
	WorkUnitsTotal      int             `json:"work_units_total"`               // total work units in project
	AggregatedAt        time.Time       `json:"aggregated_at,omitempty"`
}

// AggregateOptions configures an aggregation run.
type AggregateOptions struct {
	BatchID *string // optional: limit to specific batch (UUID string)
	Format  string  // override format: json or csv
	Force   bool    // re-aggregate even if cached
}

// aggregatedWorkUnit pairs a work unit's parameters with its agreed result.
type aggregatedWorkUnit struct {
	Parameters json.RawMessage
	OutputData json.RawMessage
}
