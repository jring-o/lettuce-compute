package leaf

import (
	"fmt"
	"strings"
	"testing"
)

// §10.11 (iii): the leaf-config guardrails for external output (D10, §10.3). These run
// in ValidateValidationConfig, which covers BOTH create and update (the update handler
// re-merges the config and re-validates through this same function). The opt-in requires
// a non-empty allowlist and is incompatible with NUMERIC_TOLERANCE; an allowlist without
// the opt-in is vestigial; and every allowlist entry must be a bare lowercase FQDN.
//
// The "opt-in without hosts" and "opt-in with NUMERIC_TOLERANCE" rows both PASS on
// pre-slice-5 code (which had no external-output block) — they are the regression rows.
func TestValidateValidationConfig_ExternalOutput(t *testing.T) {
	base := func() *ValidationConfig {
		return &ValidationConfig{
			RedundancyFactor:   2,
			AgreementThreshold: 1.0,
			ComparisonMode:     ComparisonExact,
			MaxRetries:         3,
		}
	}

	// withHosts opts the config in and sets the allowlist, so a shape row isolates
	// exactly the per-entry check under test.
	withHosts := func(hosts ...string) func(*ValidationConfig) {
		return func(c *ValidationConfig) {
			c.AllowExternalOutput = true
			c.ExternalOutputHosts = hosts
		}
	}

	seventeenHosts := make([]string, 17)
	for i := range seventeenHosts {
		seventeenHosts[i] = fmt.Sprintf("h%02d.example.com", i)
	}
	tooLongHost := strings.Repeat("a", 250) + ".com" // 254 chars, > 253

	cases := []struct {
		name    string
		mutate  func(c *ValidationConfig)
		wantErr bool
		wantMsg string // asserted substring when wantErr and non-empty
	}{
		// Opt-in / mode / vestigial cross-checks.
		{
			"opt-in without hosts is rejected (regression: accepted pre-fix)",
			func(c *ValidationConfig) { c.AllowExternalOutput = true },
			true, "non-empty external_output_hosts",
		},
		{
			"opt-in with NUMERIC_TOLERANCE is rejected (regression: accepted pre-fix)",
			func(c *ValidationConfig) {
				c.AllowExternalOutput = true
				c.ExternalOutputHosts = []string{"storage.example.com"}
				c.ComparisonMode = ComparisonNumericTolerance
				tol := 0.1
				c.NumericTolerance = &tol
			},
			true, "NUMERIC_TOLERANCE",
		},
		{
			"hosts without opt-in is rejected",
			func(c *ValidationConfig) { c.ExternalOutputHosts = []string{"storage.example.com"} },
			true, "requires allow_external_output",
		},

		// Per-entry FQDN shape (all opted in).
		{"wildcard entry rejected", withHosts("*.storage.example.com"), true, ""},
		{"ipv4 literal entry rejected", withHosts("192.168.1.1"), true, ""},
		{"ipv6 literal entry rejected", withHosts("2001:db8::1"), true, ""},
		{"entry with port rejected", withHosts("storage.example.com:443"), true, ""},
		{"entry with scheme rejected", withHosts("https://storage.example.com"), true, ""},
		{"uppercase entry rejected", withHosts("Storage.Example.Com"), true, ""},
		{"single-label entry rejected", withHosts("localhost"), true, ""},
		{"over-long entry rejected", withHosts(tooLongHost), true, ""},
		{"more than 16 entries rejected", withHosts(seventeenHosts...), true, ""},

		// Accepted.
		{"valid two-label FQDN accepted", withHosts("storage.example.com"), false, ""},
		{"valid FQDN with hyphen accepted", withHosts("cdn-1.storage.example.com"), false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c)
			apiErr := ValidateValidationConfig(c)
			if tc.wantErr {
				if apiErr == nil {
					t.Fatalf("expected a validation error, got nil")
				}
				if tc.wantMsg != "" && !strings.Contains(apiErr.Message, tc.wantMsg) {
					t.Fatalf("error message %q does not contain %q", apiErr.Message, tc.wantMsg)
				}
				return
			}
			if apiErr != nil {
				t.Fatalf("unexpected validation error: %v", apiErr)
			}
		})
	}
}
