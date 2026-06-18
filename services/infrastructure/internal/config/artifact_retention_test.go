package config

import "testing"

// TestEffectiveArtifactRetentionKeep covers the operator retention policy parsing
// (TODO #38): "all"/empty/unknown keep everything (0), "current+previous" keeps 2,
// "last:N" keeps N, and malformed N fails safe to keep-all.
func TestEffectiveArtifactRetentionKeep(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"all", 0},
		{"ALL", 0},
		{"  all  ", 0},
		{"current+previous", 2},
		{"Current+Previous", 2},
		{"last:5", 5},
		{"LAST:10", 10},
		{"last: 3", 3},
		{"last:1", 1},
		{"last:0", 0},
		{"last:-2", 0},
		{"last:abc", 0},
		{"bogus", 0},
	}
	for _, c := range cases {
		h := HeadConfig{ArtifactRetention: c.in}
		if got := h.EffectiveArtifactRetentionKeep(); got != c.want {
			t.Errorf("ArtifactRetention=%q: want keep=%d, got %d", c.in, c.want, got)
		}
	}
}
