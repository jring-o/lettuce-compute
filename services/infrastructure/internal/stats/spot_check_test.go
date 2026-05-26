package stats

import (
	"testing"
)

func TestSpotCheckPassRate_ZeroTotal_ReturnsNil(t *testing.T) {
	result := spotCheckPassRate(0, 0)
	if result != nil {
		t.Errorf("expected nil for total=0, got %v", *result)
	}
}

func TestSpotCheckPassRate_NegativeTotal_ReturnsNil(t *testing.T) {
	result := spotCheckPassRate(0, -1)
	if result != nil {
		t.Errorf("expected nil for total=-1, got %v", *result)
	}
}

func TestSpotCheckPassRate_AllPassed(t *testing.T) {
	result := spotCheckPassRate(10, 10)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if *result != 1.0 {
		t.Errorf("expected 1.0, got %v", *result)
	}
}

func TestSpotCheckPassRate_NonePassed(t *testing.T) {
	result := spotCheckPassRate(0, 5)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if *result != 0.0 {
		t.Errorf("expected 0.0, got %v", *result)
	}
}

func TestSpotCheckPassRate_Partial(t *testing.T) {
	result := spotCheckPassRate(3, 4)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if *result != 0.75 {
		t.Errorf("expected 0.75, got %v", *result)
	}
}
