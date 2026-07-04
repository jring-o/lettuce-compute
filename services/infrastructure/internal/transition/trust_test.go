package transition

import (
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

func TestResolveTrust_Table(t *testing.T) {
	tests := []struct {
		name      string
		tp        TrustPolicy
		vc        leaf.ValidationConfig
		minQuorum int
		wantK     int
		wantFloor int
	}{
		{
			name:      "gate disabled -> K 0, floor still resolved",
			tp:        TrustPolicy{GateEnabled: false, DefaultMinCorroborators: 1, DefaultFloor: 25},
			vc:        leaf.ValidationConfig{},
			minQuorum: 3,
			wantK:     0,
			wantFloor: 25,
		},
		{
			name:      "gate disabled but leaf floor override still honored (accrual needs it)",
			tp:        TrustPolicy{GateEnabled: false, DefaultMinCorroborators: 1, DefaultFloor: 25},
			vc:        leaf.ValidationConfig{TrustFloor: 40},
			minQuorum: 3,
			wantK:     0,
			wantFloor: 40,
		},
		{
			name:      "gate enabled -> head default K + floor",
			tp:        TrustPolicy{GateEnabled: true, DefaultMinCorroborators: 1, DefaultFloor: 25},
			vc:        leaf.ValidationConfig{},
			minQuorum: 3,
			wantK:     1,
			wantFloor: 25,
		},
		{
			name:      "leaf overrides both K and floor",
			tp:        TrustPolicy{GateEnabled: true, DefaultMinCorroborators: 1, DefaultFloor: 25},
			vc:        leaf.ValidationConfig{MinTrustedCorroborators: 2, TrustFloor: 40},
			minQuorum: 3,
			wantK:     2,
			wantFloor: 40,
		},
		{
			name:      "leaf K override clamps to min_quorum",
			tp:        TrustPolicy{GateEnabled: true, DefaultMinCorroborators: 1, DefaultFloor: 25},
			vc:        leaf.ValidationConfig{MinTrustedCorroborators: 5},
			minQuorum: 2,
			wantK:     2,
			wantFloor: 25,
		},
		{
			name:      "head default K clamps to min_quorum",
			tp:        TrustPolicy{GateEnabled: true, DefaultMinCorroborators: 5, DefaultFloor: 25},
			vc:        leaf.ValidationConfig{},
			minQuorum: 2,
			wantK:     2,
			wantFloor: 25,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, floor := tt.tp.ResolveTrust(tt.vc, tt.minQuorum)
			if k != tt.wantK {
				t.Errorf("K = %d, want %d", k, tt.wantK)
			}
			if floor != tt.wantFloor {
				t.Errorf("floor = %d, want %d", floor, tt.wantFloor)
			}
		})
	}
}

// TestResolvePolicyWithTrust_ZeroPolicyIsPlainPolicy: a zero-value TrustPolicy (gate off)
// resolves the exact same redundancy numbers as ResolvePolicy, plus MinTrustedCorroborators
// == 0 (the auto-pass) and a resolved floor of 0. This is the deploy-safety default.
func TestResolvePolicyWithTrust_ZeroPolicyIsPlainPolicy(t *testing.T) {
	lf := leafWith(leaf.ValidationConfig{RedundancyFactor: 2, TargetCopies: 3, MinQuorum: 2})
	wu := &workunit.WorkUnit{}
	plain := ResolvePolicy(lf, wu)
	withTrust := ResolvePolicyWithTrust(lf, wu, TrustPolicy{})

	if withTrust.MinTrustedCorroborators != 0 {
		t.Errorf("MinTrustedCorroborators = %d, want 0 (gate off)", withTrust.MinTrustedCorroborators)
	}
	// The redundancy numbers must be identical to the plain resolution.
	withTrust.MinTrustedCorroborators = plain.MinTrustedCorroborators
	withTrust.TrustFloor = plain.TrustFloor
	if withTrust != plain {
		t.Errorf("ResolvePolicyWithTrust diverged from ResolvePolicy:\n got %+v\nwant %+v", withTrust, plain)
	}
}

// TestResolvePolicyWithTrust_OverlaysGate: with the gate enabled the resolved policy carries
// the effective K (clamped to quorum) and floor.
func TestResolvePolicyWithTrust_OverlaysGate(t *testing.T) {
	lf := leafWith(leaf.ValidationConfig{RedundancyFactor: 3, MinQuorum: 3})
	tp := TrustPolicy{GateEnabled: true, DefaultMinCorroborators: 2, DefaultFloor: 25}
	p := ResolvePolicyWithTrust(lf, &workunit.WorkUnit{}, tp)
	if p.MinTrustedCorroborators != 2 {
		t.Errorf("MinTrustedCorroborators = %d, want 2", p.MinTrustedCorroborators)
	}
	if p.TrustFloor != 25 {
		t.Errorf("TrustFloor = %d, want 25", p.TrustFloor)
	}
}
