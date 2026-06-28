package cli

import "testing"

// TestProposeMemoryMB pins the hardware-derived memory default (#30): an 8 GB+ box
// must propose at least the standard leaf cap so a default volunteer is eligible for
// normal leafs, while a small box never advertises more RAM than it physically has.
func TestProposeMemoryMB(t *testing.T) {
	cases := []struct {
		name     string
		totalMB  int
		want     int
	}{
		{"8GB proposes at least the leaf cap", 8192, 4096},   // half = 4096, == floor
		{"16GB proposes half", 16384, 8192},                  // half = 8192 >= floor
		{"6GB floors up to the leaf cap", 6144, 4096},        // half = 3072 < 4096, has the RAM
		{"4GB floors up to the leaf cap", 4096, 4096},        // half = 2048 < 4096, exactly has it
		{"2GB cannot back the cap, advertises its RAM", 2048, 2048},
		{"detection failure falls back to the cap", 0, 4096},
		{"negative reading falls back to the cap", -1, 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proposeMemoryMB(tc.totalMB); got != tc.want {
				t.Errorf("proposeMemoryMB(%d) = %d, want %d", tc.totalMB, got, tc.want)
			}
			// Invariant: never propose more than physical RAM (when known).
			if tc.totalMB > 0 && proposeMemoryMB(tc.totalMB) > tc.totalMB {
				t.Errorf("proposeMemoryMB(%d) exceeds physical RAM", tc.totalMB)
			}
		})
	}
}

// TestProposeMemoryMB_CoversStandardLeaf is the direct regression for the reported
// bug: on any machine with at least the standard leaf cap of RAM, the proposed
// default must be eligible for a standard 4096 MB leaf.
func TestProposeMemoryMB_CoversStandardLeaf(t *testing.T) {
	for _, totalMB := range []int{4096, 8192, 16384, 32768, 65536} {
		if got := proposeMemoryMB(totalMB); got < standardLeafMemoryCapMB {
			t.Errorf("proposeMemoryMB(%d) = %d, want >= %d (eligible for a standard leaf)",
				totalMB, got, standardLeafMemoryCapMB)
		}
	}
}

func TestProposeDiskGB(t *testing.T) {
	cases := []struct {
		name      string
		availMB   int64
		want      int
	}{
		{"ample disk proposes half capped at 50", 200 * 1024, 50}, // half = 100 -> cap 50
		{"60GB free proposes half", 60 * 1024, 30},
		{"20GB free floors up to the static default", 20 * 1024, 10}, // half = 10
		{"15GB free floors up to the static default", 15 * 1024, 10}, // half = 7 -> 10
		{"8GB free advertises what the volume has", 8 * 1024, 8},
		{"detection failure falls back to the static default", 0, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proposeDiskGB(tc.availMB); got != tc.want {
				t.Errorf("proposeDiskGB(%d MB) = %d, want %d", tc.availMB, got, tc.want)
			}
		})
	}
}
