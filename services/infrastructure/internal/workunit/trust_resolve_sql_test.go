package workunit

import (
	"strings"
	"testing"
)

// These are plain (no-DB) unit tests of the trusted-corroborator SQL-fragment builders. They
// assert the STRING SHAPE — that each builder threads its alias and placeholder arguments
// into the expected expression. The behavioral equivalence to
// transition.TrustPolicy.ResolveTrust is pinned separately, against a live database, by the
// integration-tagged golden parity test (trust_resolve_parity_test.go).

func TestEffTrustFloorSQL_Construction(t *testing.T) {
	got := effTrustFloorSQL("lf", "$7")
	for _, want := range []string{
		// Reads the per-leaf override off the aliased leaf's validation_config...
		"lf.validation_config->>'trust_floor'",
		// ...uses it only when > 0...
		"> 0",
		// ...and otherwise falls back to the head-default floor placeholder as an int.
		"$7::int",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("effTrustFloorSQL missing %q in:\n%s", want, got)
		}
	}
}

func TestEffTrustKSQL_Construction(t *testing.T) {
	got := effTrustKSQL("wu", "l", "$5", "$6")
	for _, want := range []string{
		// Gate-off branch resolves a constant 0 (the reservation is then inert).
		"NOT $5::boolean THEN 0",
		// Leaf override key off the leaf alias...
		"l.validation_config->>'min_trusted_corroborators'",
		// ...else the head-default K placeholder as an int.
		"$6::int",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("effTrustKSQL missing %q in:\n%s", want, got)
		}
	}
	// The clamp target must be the EXACT effQuorumSQL fragment for the same aliases, so the K
	// clamp uses the very expression the redundancy SQL<->Go parity already pins to the Go
	// MinQuorum — a drift in effQuorumSQL is inherited here rather than silently diverging.
	if q := effQuorumSQL("wu", "l"); !strings.Contains(got, q) {
		t.Errorf("effTrustKSQL should clamp K to effQuorumSQL(\"wu\",\"l\"); fragment not found in:\n%s", got)
	}
}
