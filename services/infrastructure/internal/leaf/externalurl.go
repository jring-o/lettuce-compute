package leaf

// externalurl.go is the ONE URL rule for external output references (design doc §10.2
// step 3 / §10.6 step 3): the gRPC submit gate and the content-verification fetch
// worker both call ValidateExternalOutputURL, so a URL can never pass one seam and
// fail the other except through a deliberate leaf-config change in between (the D10
// fetch-time re-check against the CURRENT allowlist).

import (
	"fmt"
	"net/url"
	"strings"
)

// ValidateExternalOutputURL checks a submitted output_data_url against the D10
// contract: https scheme, non-empty host, no userinfo, port empty or 443, and the
// lowercased host an EXACT member of the leaf's external_output_hosts allowlist.
// An empty allowlist matches nothing — a pre-slice-5 opted-in leaf with no allowlist
// fails closed here. The error message is reason-coded for the refusal message;
// callers wrap it (InvalidArgument at submit, URL_DISALLOWED at fetch).
func ValidateExternalOutputURL(raw string, allowedHosts []string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("output_data_url does not parse: %v", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("output_data_url scheme must be https, got %q", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("output_data_url must not carry userinfo")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("output_data_url has no host")
	}
	if p := u.Port(); p != "" && p != "443" {
		return fmt.Errorf("output_data_url port must be 443 (or omitted), got %q", p)
	}
	for _, allowed := range allowedHosts {
		if host == allowed {
			return nil
		}
	}
	return fmt.Errorf("output_data_url host %q is not in the leaf's external_output_hosts allowlist", host)
}
