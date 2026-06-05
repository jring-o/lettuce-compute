package main

import (
	"reflect"
	"testing"
)

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

// TestHeadGRPCTargets asserts --head-grpc parses a single address (the default,
// unchanged single-head behavior) and a comma-separated list (the multi-head
// scale-out run), trimming whitespace and dropping empty entries, and that an
// effectively-empty value is rejected by parseFlags.
func TestHeadGRPCTargets(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"single", "127.0.0.1:9090", []string{"127.0.0.1:9090"}},
		{"two heads", "h1:9090,h2:9090", []string{"h1:9090", "h2:9090"}},
		{"whitespace + trailing comma", " h1:9090 , h2:9090 ,", []string{"h1:9090", "h2:9090"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &options{headGRPC: tt.in}
			got := o.headGRPCTargets()
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("headGRPCTargets(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}

	// An all-empty value must be rejected at parse time.
	if _, err := parseFlags([]string{"--head-grpc", " , "}); err == nil {
		t.Fatalf("parseFlags with empty --head-grpc = nil err, want error")
	}
}
