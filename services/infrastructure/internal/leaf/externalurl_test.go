package leaf

import "testing"

// §10.11 (ii): the one external-output URL rule (ValidateExternalOutputURL), called by
// both the gRPC submit gate and the fetch worker. A URL is accepted only when it is
// https, carries no userinfo, has port empty or 443, and its lowercased host is an
// EXACT member of the leaf's allowlist. An empty allowlist matches nothing, so a
// pre-slice-5 opted-in leaf with no allowlist fails closed. Scheme/host casing is
// normalized; fragment and query are ignored.
func TestValidateExternalOutputURL(t *testing.T) {
	allow := []string{"allowed.example.com", "storage.example.com"}

	cases := []struct {
		name    string
		raw     string
		hosts   []string
		wantErr bool
	}{
		// Accepted shapes.
		{"https allowlisted host", "https://allowed.example.com/results/wu.json", allow, false},
		{"https second allowlisted host", "https://storage.example.com/x", allow, false},
		{"explicit port 443 allowed", "https://allowed.example.com:443/x", allow, false},
		{"uppercase URL host matches lowercase allowlist", "HTTPS://ALLOWED.EXAMPLE.COM/x", allow, false},
		{"fragment and query allowed", "https://allowed.example.com/x?a=1&b=2#frag", allow, false},

		// Refused shapes.
		{"http scheme refused", "http://allowed.example.com/x", allow, true},
		{"ftp scheme refused", "ftp://allowed.example.com/x", allow, true},
		{"userinfo refused", "https://user:pass@allowed.example.com/x", allow, true},
		{"non-443 port refused", "https://allowed.example.com:8443/x", allow, true},
		{"host not in allowlist refused", "https://evil.example.com/x", allow, true},
		{"ip-literal host not in allowlist refused", "https://192.168.1.10/x", allow, true},
		{"empty allowlist matches nothing", "https://allowed.example.com/x", nil, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateExternalOutputURL(tc.raw, tc.hosts)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateExternalOutputURL(%q, %v): expected error, got nil", tc.raw, tc.hosts)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateExternalOutputURL(%q, %v): unexpected error: %v", tc.raw, tc.hosts, err)
			}
		})
	}
}
