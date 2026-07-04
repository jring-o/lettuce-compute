package server

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// TestStampTrustSnapshot_Standing pins the effective-standing value the submit paths stamp
// on a result. trustRepo is nil here (score is always 0), isolating the standing decision:
// it is resolved from the volunteer row via volunteer.EffectiveStanding and is unconditional
// of trust suppression or any gate.
func TestStampTrustSnapshot_Standing(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour)
	past := now.Add(-24 * time.Hour)
	id := types.NewID()

	cases := []struct {
		name string
		vol  *volunteer.Volunteer
		want string
	}{
		{
			name: "OK",
			vol:  &volunteer.Volunteer{ID: id, Standing: volunteer.StandingOK},
			want: volunteer.StandingOK,
		},
		{
			name: "PROBATION",
			vol:  &volunteer.Volunteer{ID: id, Standing: volunteer.StandingProbation},
			want: volunteer.StandingProbation,
		},
		{
			name: "future bench stamps BENCHED",
			vol:  &volunteer.Volunteer{ID: id, Standing: volunteer.StandingBenched, BenchedUntil: &future},
			want: volunteer.StandingBenched,
		},
		{
			name: "expired bench stamps PROBATION",
			vol:  &volunteer.Volunteer{ID: id, Standing: volunteer.StandingBenched, BenchedUntil: &past},
			want: volunteer.StandingProbation,
		},
		{
			name: "indefinite bench (nil deadline) stamps BENCHED",
			vol:  &volunteer.Volunteer{ID: id, Standing: volunteer.StandingBenched},
			want: volunteer.StandingBenched,
		},
		{
			name: "nil volunteer stamps OK",
			vol:  nil,
			want: volunteer.StandingOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, score, standing := stampTrustSnapshot(context.Background(), nil, tc.vol, id, now, nil)
			if standing != tc.want {
				t.Errorf("standing = %q, want %q", standing, tc.want)
			}
			if score != 0 {
				t.Errorf("score = %d, want 0 (nil trust repo)", score)
			}
		})
	}
}

// TestStampTrustSnapshot_NilVolunteerSubject confirms the nil-volunteer fallback still yields
// the per-keypair sentinel subject alongside the OK standing.
func TestStampTrustSnapshot_NilVolunteerSubject(t *testing.T) {
	id := types.NewID()
	subject, score, standing := stampTrustSnapshot(context.Background(), nil, nil, id, time.Now(), nil)
	if subject == "" {
		t.Error("subject should fall back to the per-keypair sentinel, got empty")
	}
	if score != 0 {
		t.Errorf("score = %d, want 0", score)
	}
	if standing != volunteer.StandingOK {
		t.Errorf("standing = %q, want OK", standing)
	}
}
