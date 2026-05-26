package aggregation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// paramSweepRow is one row in the aggregated param sweep output.
type paramSweepRow struct {
	Parameters json.RawMessage `json:"parameters"`
	Result     json.RawMessage `json:"result"`
}

// aggregateParamSweep collects (parameters, output_data) pairs into a structured dataset.
func aggregateParamSweep(pairs []aggregatedWorkUnit, format string) (*AggregateResult, error) {
	rows := make([]paramSweepRow, 0, len(pairs))
	for _, p := range pairs {
		rows = append(rows, paramSweepRow{
			Parameters: p.Parameters,
			Result:     p.OutputData,
		})
	}

	aggResult := &AggregateResult{
		Status:              "complete",
		Format:              format,
		WorkUnitsAggregated: len(pairs),
	}

	switch format {
	case "csv":
		csv, err := paramSweepToCSV(pairs)
		if err != nil {
			return nil, err
		}
		aggResult.ResultCSV = csv
	default:
		data, err := json.Marshal(rows)
		if err != nil {
			return nil, fmt.Errorf("marshal param sweep result: %w", err)
		}
		aggResult.Result = data
	}

	return aggResult, nil
}

// paramSweepToCSV converts parameter sweep results to CSV format.
// Auto-detects scalar fields from parameters and output for CSV columns;
// nested objects are serialized as JSON strings.
func paramSweepToCSV(pairs []aggregatedWorkUnit) (string, error) {
	if len(pairs) == 0 {
		return "", nil
	}

	// Collect all parameter and result keys from the first pair to establish columns.
	paramKeys, resultKeys, err := extractKeys(pairs[0])
	if err != nil {
		return "", err
	}

	var sb strings.Builder

	// Header row.
	allKeys := make([]string, 0, len(paramKeys)+len(resultKeys))
	allKeys = append(allKeys, paramKeys...)
	allKeys = append(allKeys, resultKeys...)
	sb.WriteString(strings.Join(allKeys, ","))
	sb.WriteString("\n")

	// Data rows.
	for _, p := range pairs {
		var params map[string]interface{}
		if err := json.Unmarshal(p.Parameters, &params); err != nil {
			return "", fmt.Errorf("unmarshal parameters for CSV: %w", err)
		}

		var output map[string]interface{}
		if err := json.Unmarshal(p.OutputData, &output); err != nil {
			return "", fmt.Errorf("unmarshal output for CSV: %w", err)
		}

		vals := make([]string, 0, len(allKeys))
		for _, k := range paramKeys {
			vals = append(vals, formatCSVValue(params[k]))
		}
		for _, k := range resultKeys {
			vals = append(vals, formatCSVValue(output[k]))
		}
		sb.WriteString(strings.Join(vals, ","))
		sb.WriteString("\n")
	}

	return sb.String(), nil
}

// extractKeys gets sorted parameter keys and result keys from a pair.
func extractKeys(p aggregatedWorkUnit) (paramKeys, resultKeys []string, err error) {
	var params map[string]interface{}
	if err := json.Unmarshal(p.Parameters, &params); err != nil {
		return nil, nil, fmt.Errorf("unmarshal parameters: %w", err)
	}
	for k := range params {
		paramKeys = append(paramKeys, k)
	}
	sort.Strings(paramKeys)

	var output map[string]interface{}
	if err := json.Unmarshal(p.OutputData, &output); err != nil {
		return nil, nil, fmt.Errorf("unmarshal output: %w", err)
	}
	for k := range output {
		resultKeys = append(resultKeys, k)
	}
	sort.Strings(resultKeys)

	return paramKeys, resultKeys, nil
}

// formatCSVValue formats a value for CSV output.
func formatCSVValue(v interface{}) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case float64:
		return fmt.Sprintf("%g", val)
	case string:
		if strings.ContainsAny(val, ",\"\n") {
			return fmt.Sprintf("%q", val)
		}
		return val
	case bool:
		return fmt.Sprintf("%t", val)
	default:
		// Nested objects/arrays → JSON string.
		b, _ := json.Marshal(val)
		return fmt.Sprintf("%q", string(b))
	}
}
