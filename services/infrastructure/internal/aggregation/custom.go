package aggregation

// aggregateCustom applies optional built-in reducer for custom pattern.
// If no reducer_type is configured, returns a no_aggregation result.
func aggregateCustom(pairs []aggregatedWorkUnit, config map[string]any) (*AggregateResult, error) {
	reducerType, _ := config["reducer_type"].(string)

	if reducerType == "" {
		return &AggregateResult{
			Status:             "no_aggregation",
			Message:            "This project has no automatic aggregation configured. Use GET /api/v1/leafs/{leaf_id}/results to retrieve individual results.",
			WorkUnitsValidated: len(pairs),
			WorkUnitsAggregated: len(pairs),
		}, nil
	}

	// Reuse map-reduce reducers for custom pattern.
	mrConfig := map[string]any{
		"reducer_type": reducerType,
	}
	if v, ok := config["reducer_field"]; ok {
		mrConfig["reducer_field"] = v
	}
	if v, ok := config["merge_strategy"]; ok {
		mrConfig["merge_strategy"] = v
	}

	return aggregateMapReduce(pairs, mrConfig)
}