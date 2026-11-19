package workunit

import (
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TestParseLiveContributorTexts: the subject is everything after the second separator, so
// a subject that itself contains one survives; a malformed entry is dropped.
func TestParseLiveContributorTexts(t *testing.T) {
	id := types.NewID()
	got := parseLiveContributorTexts([]string{
		id.String() + "|1|did:web:example.org|x",
		id.String() + "|0|vol:" + id.String(),
		"not-a-uuid|1|vol:x",
		id.String() + "|2|vol:x",
		id.String() + "|1|",
		id.String(),
	})
	want := []LiveContributor{
		{VolunteerID: id, Subject: "did:web:example.org|x", Countable: true},
		{VolunteerID: id, Subject: "vol:" + id.String(), Countable: false},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parseLiveContributorTexts = %+v, want %+v", got, want)
	}
}
