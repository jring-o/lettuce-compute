package config

import (
	"strings"
	"testing"
)

// Every way a volunteer might type a head must land on the same config entry
// (TB-51). The desktop wizard's placeholder invited "https://…", the head's
// name then came out as "https" and gRPC was handed a URL it cannot resolve.
func TestParseHeadAddress_AcceptedForms(t *testing.T) {
	cases := []struct {
		in       string
		host     string
		port     int
		insecure bool
		grpc     string
		http     string
	}{
		{"compute.example.org", "compute.example.org", 0, false, "compute.example.org:443", "https://compute.example.org"},
		{"  Compute.Example.ORG.  ", "compute.example.org", 0, false, "compute.example.org:443", "https://compute.example.org"},
		{"compute.example.org:8443", "compute.example.org", 8443, false, "compute.example.org:8443", "https://compute.example.org:8443"},
		{"https://compute.example.org", "compute.example.org", 0, false, "compute.example.org:443", "https://compute.example.org"},
		{"https://compute.example.org/", "compute.example.org", 0, false, "compute.example.org:443", "https://compute.example.org"},
		{"HTTPS://compute.example.org/some/path?x=1#frag", "compute.example.org", 0, false, "compute.example.org:443", "https://compute.example.org"},
		{"https://compute.example.org:443/", "compute.example.org", 443, false, "compute.example.org:443", "https://compute.example.org"},
		{"http://localhost:9090", "localhost", 9090, true, "localhost:9090", "http://localhost:9090"},
		{"http://localhost", "localhost", 0, true, "localhost:80", "http://localhost"},
		{"192.0.2.10:8443", "192.0.2.10", 8443, false, "192.0.2.10:8443", "https://192.0.2.10:8443"},
		{"[2001:db8::1]:8443", "2001:db8::1", 8443, false, "[2001:db8::1]:8443", "https://[2001:db8::1]:8443"},
		{"https://[2001:db8::1]/", "2001:db8::1", 0, false, "[2001:db8::1]:443", "https://[2001:db8::1]"},
	}
	for _, tc := range cases {
		got, err := ParseHeadAddress(tc.in)
		if err != nil {
			t.Errorf("%q: unexpected error: %v", tc.in, err)
			continue
		}
		if got.Host != tc.host || got.Port != tc.port || got.Insecure != tc.insecure {
			t.Errorf("%q: parsed %+v, want host=%q port=%d insecure=%v", tc.in, got, tc.host, tc.port, tc.insecure)
		}
		if g := got.GRPCAddress(); g != tc.grpc {
			t.Errorf("%q: GRPCAddress = %q, want %q", tc.in, g, tc.grpc)
		}
		if h := got.HTTPAddress(); h != tc.http {
			t.Errorf("%q: HTTPAddress = %q, want %q", tc.in, h, tc.http)
		}
	}
}

func TestParseHeadAddress_Refused(t *testing.T) {
	cases := []struct {
		in   string
		want string // substring of the error
	}{
		{"", "required"},
		{"   ", "required"},
		{"ftp://compute.example.org", "unsupported scheme"},
		{"grpc://compute.example.org", "unsupported scheme"},
		{"https://", "no host name"},
		{"https://user:pw@compute.example.org", "credentials"},
		{"compute.example.org:0", "port"},
		{"compute.example.org:70000", "port"},
		{"compute.example.org:abc", "port"},
	}
	for _, tc := range cases {
		_, err := ParseHeadAddress(tc.in)
		if err == nil {
			t.Errorf("%q: expected an error", tc.in)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: error %q does not mention %q", tc.in, err, tc.want)
		}
	}
}
