package generate

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

func stubGenerator(name string) workunit.GenerateFunc {
	return func(
		ctx context.Context,
		proj *leaf.Leaf,
		parameterSpace map[string]interface{},
		batchSize int,
		sink workunit.BatchSink,
	) (*workunit.GenerateResult, error) {
		return &workunit.GenerateResult{
			BatchIDs:         []types.ID{types.NewID()},
			WorkUnitsCreated: 1,
			Status:           name,
		}, nil
	}
}

func TestRouter_ParameterSweep(t *testing.T) {
	r := NewRouter(stubGenerator("param_sweep"), stubGenerator("map_reduce"), stubGenerator("monte_carlo"), stubGenerator("custom"), slog.Default())
	proj := &leaf.Leaf{TaskPattern: leaf.PatternParameterSweep}

	result, err := r.Generate(context.Background(), proj, nil, 10, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "param_sweep" {
		t.Errorf("expected dispatch to param_sweep, got %q", result.Status)
	}
}

func TestRouter_MapReduce(t *testing.T) {
	r := NewRouter(stubGenerator("param_sweep"), stubGenerator("map_reduce"), stubGenerator("monte_carlo"), stubGenerator("custom"), slog.Default())
	proj := &leaf.Leaf{TaskPattern: leaf.PatternMapReduce}

	result, err := r.Generate(context.Background(), proj, nil, 10, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "map_reduce" {
		t.Errorf("expected dispatch to map_reduce, got %q", result.Status)
	}
}

func TestRouter_MonteCarlo(t *testing.T) {
	r := NewRouter(stubGenerator("param_sweep"), stubGenerator("map_reduce"), stubGenerator("monte_carlo"), stubGenerator("custom"), slog.Default())
	proj := &leaf.Leaf{TaskPattern: leaf.PatternMonteCarlo}

	result, err := r.Generate(context.Background(), proj, nil, 10, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "monte_carlo" {
		t.Errorf("expected dispatch to monte_carlo, got %q", result.Status)
	}
}

func TestRouter_Custom(t *testing.T) {
	r := NewRouter(stubGenerator("param_sweep"), stubGenerator("map_reduce"), stubGenerator("monte_carlo"), stubGenerator("custom"), slog.Default())
	proj := &leaf.Leaf{TaskPattern: leaf.PatternCustom}

	result, err := r.Generate(context.Background(), proj, nil, 10, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "custom" {
		t.Errorf("expected dispatch to custom, got %q", result.Status)
	}
}

func TestRouter_UnknownPattern(t *testing.T) {
	r := NewRouter(stubGenerator("param_sweep"), stubGenerator("map_reduce"), stubGenerator("monte_carlo"), stubGenerator("custom"), slog.Default())
	proj := &leaf.Leaf{TaskPattern: "UNKNOWN"}

	_, err := r.Generate(context.Background(), proj, nil, 10, nil)
	if err == nil {
		t.Fatal("expected error for unknown pattern")
	}
	if !strings.Contains(err.Error(), "unknown task pattern") {
		t.Errorf("expected 'unknown task pattern' error, got: %v", err)
	}
}
