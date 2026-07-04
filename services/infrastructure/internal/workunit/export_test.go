package workunit

// Test-only exports for the golden trust-resolution parity test.
//
// trust_resolve_parity_test.go must live in an EXTERNAL test package (workunit_test) so it
// can import internal/transition to call the real transition.TrustPolicy.ResolveTrust:
// transition imports workunit, so an in-package (package workunit) test that imported
// transition would form an import cycle and fail to compile. An external test package cannot
// see unexported identifiers, so these aliases expose the two unexported SQL-fragment
// builders the dispatch queries embed, letting the parity test build the exact same
// expressions and assert they evaluate identically to ResolveTrust across the input grid.
var (
	EffTrustKSQL     = effTrustKSQL
	EffTrustFloorSQL = effTrustFloorSQL
)
