package cli

import "testing"

// PB-28: parseTrustRuntimes must return a NON-NIL (possibly empty) list — the
// empty list is how an explicit "none" stays distinguishable from a legacy
// config that never recorded a trust decision at all. A nil here collapsed the
// two cases; historically that let the loader's migration silently grant
// CONTAINER trust (seeded from the since-retired available_runtimes key) over
// a deliberate `--trust none`.
func TestParseTrustRuntimes_NoneIsExplicitEmpty(t *testing.T) {
	for _, in := range []string{"none", "", "wasm", "NONE", " none "} {
		got, err := parseTrustRuntimes(in)
		if err != nil {
			t.Fatalf("parseTrustRuntimes(%q): %v", in, err)
		}
		if got == nil {
			t.Errorf("parseTrustRuntimes(%q) = nil; an explicit no-opt-in choice must be a non-nil empty list (PB-28)", in)
		}
		if len(got) != 0 {
			t.Errorf("parseTrustRuntimes(%q) = %v, want empty", in, got)
		}
	}
}
