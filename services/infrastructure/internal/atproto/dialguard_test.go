package atproto

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestScreenDialAddress(t *testing.T) {
	cases := []struct {
		name    string
		address string
		allow   bool
	}{
		// Allowed: ordinary public addresses.
		{"public ipv4", "93.184.216.34:443", true},
		{"public ipv6", "[2606:2800:220:1:248:1893:25c8:1946]:443", true},

		// Loopback.
		{"ipv4 loopback", "127.0.0.1:443", false},
		{"ipv4 loopback range", "127.9.9.9:8443", false},
		{"ipv6 loopback", "[::1]:443", false},
		{"ipv4-mapped loopback", "[::ffff:127.0.0.1]:443", false},

		// Private.
		{"private 10/8", "10.0.0.5:8443", false},
		{"private 172.16/12", "172.16.4.4:443", false},
		{"private 192.168/16", "192.168.1.10:443", false},
		{"private fc00::/7", "[fd00::1]:443", false},

		// Link-local.
		{"link-local ipv4", "169.254.169.254:80", false}, // cloud metadata endpoint
		{"link-local ipv6", "[fe80::1]:443", false},

		// Unspecified.
		{"unspecified ipv4", "0.0.0.0:443", false},
		{"unspecified ipv6", "[::]:443", false},

		// Multicast.
		{"multicast ipv4", "224.0.0.1:443", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := screenDialAddress("tcp", tc.address)
			if tc.allow && err != nil {
				t.Fatalf("screenDialAddress(%q) = %v, want allowed", tc.address, err)
			}
			if !tc.allow {
				if err == nil {
					t.Fatalf("screenDialAddress(%q) allowed, want blocked", tc.address)
				}
				if !errors.Is(err, ErrDisallowedAddress) {
					t.Fatalf("screenDialAddress(%q) error = %v, want ErrDisallowedAddress", tc.address, err)
				}
			}
		})
	}
}

func TestScreenDialAddressRejectsNonIP(t *testing.T) {
	// Control is handed a resolved IP literal; a bare hostname must be refused
	// rather than dialed unscreened.
	err := screenDialAddress("tcp", "internal.example.com:443")
	if !errors.Is(err, ErrDisallowedAddress) {
		t.Fatalf("want ErrDisallowedAddress for a non-IP host, got %v", err)
	}
}

// TestDefaultClientRefusesLoopback proves the guard is actually wired into the
// client NewClient builds when httpClient is nil: a request to a loopback
// httptest server must fail the dial with ErrDisallowedAddress.
func TestDefaultClientRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be reached: the dial should be refused before connect")
	}))
	defer srv.Close()

	// Default (guarded) client via nil httpClient.
	client := NewClient("", nil, nil)
	_, err := client.GetRecord(context.Background(), srv.URL, "did:plc:abc", "app.x", "self")
	if err == nil {
		t.Fatal("expected the loopback dial to be refused")
	}
	if !errors.Is(err, ErrDisallowedAddress) {
		t.Fatalf("want ErrDisallowedAddress, got %v", err)
	}
}
