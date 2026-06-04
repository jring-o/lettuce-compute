package main

import "testing"

// TestParseFlagsProfiles asserts the supported profiles parse and that an unknown
// profile is rejected. The overload profile is the Layer 2 saturation driver and
// request-only is its HandOut-isolation probe; both must be accepted alongside
// naive and buffered.
func TestParseFlagsProfiles(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"naive ok", []string{"--profile", "naive"}, false},
		{"buffered ok", []string{"--profile", "buffered"}, false},
		{"overload ok", []string{"--profile", "overload"}, false},
		{"request-only ok", []string{"--profile", "request-only"}, false},
		{"unknown rejected", []string{"--profile", "bogus"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseFlags(tt.args)
			if tt.wantErr && err == nil {
				t.Fatalf("parseFlags(%v) = nil err, want error", tt.args)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("parseFlags(%v) = %v, want nil err", tt.args, err)
			}
		})
	}
}

// TestParseFlagsHonorShedDefault asserts --honor-shed defaults off (the maximal
// ceiling-run pressure) and parses when set.
func TestParseFlagsHonorShedDefault(t *testing.T) {
	o, err := parseFlags([]string{"--profile", "overload"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if o.honorShed {
		t.Fatalf("honorShed = true by default, want false")
	}

	o, err = parseFlags([]string{"--profile", "overload", "--honor-shed"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !o.honorShed {
		t.Fatalf("honorShed = false with --honor-shed, want true")
	}
}
