package atproto

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lettuce-compute/infrastructure/netguard"
)

// TestDefaultClientRefusesLoopback proves the shared netguard dial screen is actually
// wired into the client NewClient builds when httpClient is nil: a request to a
// loopback httptest server must fail the dial with netguard.ErrDisallowedAddress.
// The classifier itself is table-tested in internal/netguard; this is the thin
// wiring test kept behind after the guard's relocation (design doc §10.4).
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
	if !errors.Is(err, netguard.ErrDisallowedAddress) {
		t.Fatalf("want netguard.ErrDisallowedAddress, got %v", err)
	}
}
