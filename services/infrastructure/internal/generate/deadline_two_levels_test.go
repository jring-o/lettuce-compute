package generate

import (
	"encoding/json"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
)

// TestResolveDeadlineSeconds_TwoSources pins what every generation path stamps on
// a new unit: the leaf's deadline_seconds when set, else the head's default (6 h
// unless the operator changes it). The leaf is decoded from JSON, as a stored leaf
// is, so the retired keys a stored config may still carry are present and must be
// ignored.
func TestResolveDeadlineSeconds_TwoSources(t *testing.T) {
	const headDefault = 21600
	tests := []struct {
		name   string
		stored string
		want   int
	}{
		{"no leaf deadline", `{"max_reassignments":3}`, headDefault},
		{"a GREP leaf: no_deadline with a multiplier", `{"no_deadline":true,"deadline_multiplier":3}`, headDefault},
		{"a multiplier alone is not a source", `{"deadline_multiplier":3}`, headDefault},
		{"the leaf's deadline wins", `{"deadline_multiplier":3,"deadline_seconds":10800}`, 10800},
		{"the leaf's deadline wins over a stored no_deadline", `{"no_deadline":true,"deadline_seconds":900}`, 900},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var proj leaf.Leaf
			if err := json.Unmarshal([]byte(`{"fault_tolerance_config":`+tt.stored+`}`), &proj); err != nil {
				t.Fatalf("decoding leaf: %v", err)
			}
			if got := ResolveDeadlineSeconds(&proj); got != tt.want {
				t.Errorf("ResolveDeadlineSeconds(%s) = %d, want %d", tt.stored, got, tt.want)
			}
		})
	}
}
