package admission

import (
	"strings"
	"testing"
)

// TestBucketForIP pins the bucket-derivation contract: IPv4 addresses bucket by the exact
// address, IPv4-mapped IPv6 is unmapped to its IPv4 form first, IPv6 collapses to its /64
// prefix (a single host trivially owns a whole /64), and anything that is not a bare IP
// literal is an error so callers fail closed while the gate is enabled.
func TestBucketForIP(t *testing.T) {
	valid := []struct {
		name string
		in   string
		want string
	}{
		{"ipv4 buckets as itself", "203.0.113.7", "203.0.113.7"},
		{"ipv4-mapped ipv6 is unmapped to ipv4", "::ffff:203.0.113.7", "203.0.113.7"},
		{"ipv6 collapses to its /64 prefix", "2001:db8:abcd:12ff:fe80::1", "2001:db8:abcd:12ff::/64"},
		{"ipv6 already on the /64 boundary", "2001:db8:abcd:12ff::", "2001:db8:abcd:12ff::/64"},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BucketForIP(tc.in)
			if err != nil {
				t.Fatalf("BucketForIP(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("BucketForIP(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	invalid := []struct {
		name string
		in   string
	}{
		{"empty string", ""},
		{"unknown gRPC no-peer sentinel", "unknown"},
		{"non-ip text", "not-an-ip"},
		{"host:port is not a bare ip", "1.2.3.4:5678"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := BucketForIP(tc.in); err == nil {
				t.Errorf("BucketForIP(%q) = %q, nil; want an error", tc.in, got)
			}
		})
	}
}

// TestCapExceededMessage_NeverMisreadAsVersionError pins the §2.3 contract: the client-facing
// cap-refusal text must never be classified by the volunteer CLI as a "your build is too old"
// error, or a rate-limited volunteer would be told to run an update instead of to wait.
//
// The classifier being defended against is the CLI's client.IsVolunteerTooOldError, which
// lives in a SEPARATE Go module (services/volunteer-cli). We deliberately do NOT import it —
// the two modules must stay decoupled — so this test re-derives its trigger logic locally.
// That classifier returns true when the (lower-cased) status message contains "too old" or
// "outdated" unconditionally, or, for an Unauthenticated/FailedPrecondition status, when it
// contains "version" together with any of mismatch/old/update/unsupported/incompatible. A cap
// refusal is sent as FailedPrecondition, so it is exactly a message subject to that branch.
func TestCapExceededMessage_NeverMisreadAsVersionError(t *testing.T) {
	msg := strings.ToLower(CapExceededMessage)

	for _, bad := range []string{"too old", "outdated"} {
		if strings.Contains(msg, bad) {
			t.Errorf("CapExceededMessage %q contains %q, which IsVolunteerTooOldError matches unconditionally", CapExceededMessage, bad)
		}
	}

	if strings.Contains(msg, "version") {
		for _, combo := range []string{"mismatch", "old", "update", "unsupported", "incompatible"} {
			if strings.Contains(msg, combo) {
				t.Errorf("CapExceededMessage %q pairs \"version\" with %q, which the FailedPrecondition branch of IsVolunteerTooOldError treats as a version problem", CapExceededMessage, combo)
			}
		}
	}
}

// TestCapPolicyZeroValueIsInert pins the deploy-safety default: an unconfigured CapPolicy has
// the gate off, so a head that never wires the knob behaves exactly as it did before this
// package existed.
func TestCapPolicyZeroValueIsInert(t *testing.T) {
	var p CapPolicy
	if p.Enabled {
		t.Error("zero-value CapPolicy.Enabled = true, want false (gate must default off)")
	}
}
