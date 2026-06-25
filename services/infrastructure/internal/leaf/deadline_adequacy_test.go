package leaf

import (
	"strings"
	"testing"
)

func intPtr(v int) *int { return &v }

func TestFaultToleranceConfig_ResolveDeadlineSeconds(t *testing.T) {
	tests := []struct {
		name string
		cfg  FaultToleranceConfig
		want int
	}{
		{
			name: "multiplier path",
			cfg:  FaultToleranceConfig{DeadlineMultiplier: 3.0},
			want: 3 * DefaultWorkUnitDurationSeconds,
		},
		{
			name: "zero multiplier floored to 1",
			cfg:  FaultToleranceConfig{DeadlineMultiplier: 0},
			want: DefaultWorkUnitDurationSeconds,
		},
		{
			name: "explicit deadline overrides multiplier",
			cfg:  FaultToleranceConfig{DeadlineMultiplier: 3.0, DeadlineSeconds: intPtr(86400)},
			want: 86400,
		},
		{
			name: "non-positive explicit deadline falls back to multiplier",
			cfg:  FaultToleranceConfig{DeadlineMultiplier: 2.0, DeadlineSeconds: intPtr(0)},
			want: 2 * DefaultWorkUnitDurationSeconds,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.ResolveDeadlineSeconds(); got != tt.want {
				t.Errorf("ResolveDeadlineSeconds() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDeadlineAdequacyWarnings(t *testing.T) {
	tests := []struct {
		name     string
		leaf     *Leaf
		wantWarn bool
	}{
		{
			name: "deadline shorter than max_cpu_seconds warns",
			leaf: &Leaf{
				ExecutionConfig:      ExecutionConfig{MaxCPUSeconds: 86400}, // 24h budget
				FaultToleranceConfig: FaultToleranceConfig{DeadlineMultiplier: 3.0}, // 3h deadline
			},
			wantWarn: true,
		},
		{
			name: "deadline at least max_cpu_seconds does not warn",
			leaf: &Leaf{
				ExecutionConfig:      ExecutionConfig{MaxCPUSeconds: 7200},
				FaultToleranceConfig: FaultToleranceConfig{DeadlineSeconds: intPtr(18000)},
			},
			wantWarn: false,
		},
		{
			name: "no_deadline never warns even with a tiny implied deadline",
			leaf: &Leaf{
				ExecutionConfig:      ExecutionConfig{MaxCPUSeconds: 86400},
				FaultToleranceConfig: FaultToleranceConfig{NoDeadline: true, DeadlineMultiplier: 1.0},
			},
			wantWarn: false,
		},
		{
			name: "zero max_cpu_seconds is not flagged",
			leaf: &Leaf{
				ExecutionConfig:      ExecutionConfig{MaxCPUSeconds: 0},
				FaultToleranceConfig: FaultToleranceConfig{DeadlineMultiplier: 1.0},
			},
			wantWarn: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings := DeadlineAdequacyWarnings(tt.leaf)
			if got := len(warnings) > 0; got != tt.wantWarn {
				t.Errorf("DeadlineAdequacyWarnings() warned=%v (%v), want %v", got, warnings, tt.wantWarn)
			}
			// When it warns, the message must name both offending fields so an
			// operator can act on it.
			if tt.wantWarn && len(warnings) > 0 {
				if !strings.Contains(warnings[0], "max_cpu_seconds") || !strings.Contains(warnings[0], "deadline") {
					t.Errorf("warning should reference max_cpu_seconds and the deadline, got: %q", warnings[0])
				}
			}
		})
	}
}
