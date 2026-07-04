package trust

import (
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

func strptr(s string) *string { return &s }

func TestSubjectForVolunteer(t *testing.T) {
	id := types.NewID()
	sentinel := SubjectVolunteerPrefix + id.String()
	did := "did:plc:abc123"

	ok := volunteer.DIDBindingStatusOK
	stale := volunteer.DIDBindingStatusStale
	revoked := volunteer.DIDBindingStatusRevoked

	tests := []struct {
		name string
		vol  *volunteer.Volunteer
		want string
	}{
		{
			name: "unbound falls back to keypair sentinel",
			vol:  &volunteer.Volunteer{ID: id},
			want: sentinel,
		},
		{
			name: "OK binding uses the DID",
			vol:  &volunteer.Volunteer{ID: id, DID: strptr(did), DIDBindingStatus: &ok},
			want: did,
		},
		{
			name: "STALE binding still uses the DID (same principal; power suppressed elsewhere)",
			vol:  &volunteer.Volunteer{ID: id, DID: strptr(did), DIDBindingStatus: &stale},
			want: did,
		},
		{
			name: "REVOKED binding reverts to the keypair sentinel",
			vol:  &volunteer.Volunteer{ID: id, DID: strptr(did), DIDBindingStatus: &revoked},
			want: sentinel,
		},
		{
			name: "DID present but status nil (never verified) uses the sentinel",
			vol:  &volunteer.Volunteer{ID: id, DID: strptr(did)},
			want: sentinel,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SubjectForVolunteer(tt.vol); got != tt.want {
				t.Errorf("SubjectForVolunteer() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSubjectForVolunteerID(t *testing.T) {
	id := types.NewID()
	want := "vol:" + id.String()
	if got := SubjectForVolunteerID(id); got != want {
		t.Errorf("SubjectForVolunteerID() = %q, want %q", got, want)
	}
}

func TestQuorumPowerSuppressed(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	ok := volunteer.DIDBindingStatusOK
	stale := volunteer.DIDBindingStatusStale

	tests := []struct {
		name string
		vol  *volunteer.Volunteer
		want bool
	}{
		{
			name: "no binding, no freeze: not suppressed",
			vol:  &volunteer.Volunteer{},
			want: false,
		},
		{
			name: "OK binding, no freeze: not suppressed",
			vol:  &volunteer.Volunteer{DIDBindingStatus: &ok},
			want: false,
		},
		{
			name: "STALE binding: suppressed (fail closed on the privilege)",
			vol:  &volunteer.Volunteer{DIDBindingStatus: &stale},
			want: true,
		},
		{
			name: "freeze in the future: suppressed",
			vol:  &volunteer.Volunteer{DIDBindingStatus: &ok, DIDFrozenUntil: &future},
			want: true,
		},
		{
			name: "freeze in the past: not suppressed",
			vol:  &volunteer.Volunteer{DIDBindingStatus: &ok, DIDFrozenUntil: &past},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := QuorumPowerSuppressed(tt.vol, now); got != tt.want {
				t.Errorf("QuorumPowerSuppressed() = %v, want %v", got, tt.want)
			}
		})
	}
}
