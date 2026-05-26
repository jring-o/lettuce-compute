package workunit

import (
	"testing"
)

func TestShouldSpotCheck_ZeroPercent_NeverTriggers(t *testing.T) {
	for i := 0; i < 1000; i++ {
		if ShouldSpotCheck(0) {
			t.Fatal("ShouldSpotCheck(0) returned true")
		}
	}
}

func TestShouldSpotCheck_NegativePercent_NeverTriggers(t *testing.T) {
	for i := 0; i < 1000; i++ {
		if ShouldSpotCheck(-5) {
			t.Fatal("ShouldSpotCheck(-5) returned true")
		}
	}
}

func TestShouldSpotCheck_HundredPercent_AlwaysTriggers(t *testing.T) {
	for i := 0; i < 1000; i++ {
		if !ShouldSpotCheck(100) {
			t.Fatal("ShouldSpotCheck(100) returned false")
		}
	}
}

func TestShouldSpotCheck_OverHundredPercent_AlwaysTriggers(t *testing.T) {
	for i := 0; i < 1000; i++ {
		if !ShouldSpotCheck(200) {
			t.Fatal("ShouldSpotCheck(200) returned false")
		}
	}
}

func TestShouldSpotCheck_FivePercent_ApproximateDistribution(t *testing.T) {
	const trials = 100_000
	const percentage = 5.0
	triggered := 0
	for i := 0; i < trials; i++ {
		if ShouldSpotCheck(percentage) {
			triggered++
		}
	}

	rate := float64(triggered) / float64(trials) * 100.0
	// Allow ±2% tolerance (so 3%-7% for 5% target).
	if rate < 3.0 || rate > 7.0 {
		t.Errorf("ShouldSpotCheck(5): triggered %.1f%% of the time, expected ~5%%", rate)
	}
}

func TestShouldSpotCheck_TwentyPercent_ApproximateDistribution(t *testing.T) {
	const trials = 100_000
	const percentage = 20.0
	triggered := 0
	for i := 0; i < trials; i++ {
		if ShouldSpotCheck(percentage) {
			triggered++
		}
	}

	rate := float64(triggered) / float64(trials) * 100.0
	// Allow ±3% tolerance (so 17%-23% for 20% target).
	if rate < 17.0 || rate > 23.0 {
		t.Errorf("ShouldSpotCheck(20): triggered %.1f%% of the time, expected ~20%%", rate)
	}
}
