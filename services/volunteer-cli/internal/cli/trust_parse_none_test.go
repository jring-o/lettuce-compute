package cli

import "testing"

// PB-28: parseTrustRuntimes must return a NON-NIL (possibly empty) list — the
// empty list is how an explicit "none" stays distinguishable from a legacy
// config with no per-head trust at all, which the loader re-seeds from the
// global available_runtimes. A nil here collapsed the two cases and let the
// migration silently grant CONTAINER trust over a deliberate `--trust none`.
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
