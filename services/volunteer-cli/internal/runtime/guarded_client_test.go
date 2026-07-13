package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lettuce-compute/infrastructure/netguard"
)

// TestNewGuardedHTTPClientPosture pins the SSRF posture on the real production
// constructor so a later refactor cannot silently regress it: no env proxy (which
// would make the dial guard screen the proxy's IP, not the destination) and a
// bounded CheckRedirect.
func TestNewGuardedHTTPClientPosture(t *testing.T) {
	client := NewGuardedHTTPClient()
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", client.Transport)
	}
	if tr.Proxy != nil {
		t.Error("Transport.Proxy must be nil (an env proxy would let the dial guard screen the proxy IP, not the destination)")
	}
	if tr.DialContext == nil {
		t.Error("Transport.DialContext must be set (it carries the netguard dial screen)")
	}
	if client.CheckRedirect == nil {
		t.Error("CheckRedirect must be set (redirects are bounded)")
	}
}

// TestGuardedClientRefusesInternalAddresses is the BG-14 exit test (d): the REAL
// guarded client must fail closed with netguard.ErrDisallowedAddress for any URL
// whose connection would land on an internal address — a loopback/metadata IP
// literal, a public-looking hostname that RESOLVES to one, and a 302 that
// redirects to one after a first hop.
func TestGuardedClientRefusesInternalAddresses(t *testing.T) {
	client := NewGuardedHTTPClient()

	// (i) Literal internal addresses across the ranges netguard covers.
	literals := []string{
		"http://127.0.0.1/latest/meta-data/",           // loopback
		"http://169.254.169.254/latest/meta-data/",      // link-local / cloud metadata
		"http://[::1]/",                                  // IPv6 loopback
		"http://10.0.0.5/",                               // private
		"http://192.168.1.1/",                            // private
		"http://100.64.0.1/",                             // CGNAT
		"http://0.0.0.0/",                                // unspecified
	}
	for _, url := range literals {
		if err := doGet(client, url); !errors.Is(err, netguard.ErrDisallowedAddress) {
			t.Errorf("GET %s: err = %v, want netguard.ErrDisallowedAddress", url, err)
		}
	}

	// (ii) A hostname that resolves to loopback. localhost is the portable stand-in
	// for "attacker's hostname that A-records to an internal IP": the guard screens
	// the RESOLVED IP at connect time, so the URL host being a name does not help.
	if err := doGet(client, "http://localhost/"); !errors.Is(err, netguard.ErrDisallowedAddress) {
		t.Errorf("GET http://localhost/: err = %v, want netguard.ErrDisallowedAddress", err)
	}

	// (iii) An httptest server (which binds loopback, 127.0.0.1) is refused at the
	// dial regardless of what it would serve — the guard screens each connection,
	// so a redirect target is dialed through the same Control hook and screened on
	// its own hop. A public first hop cannot be stood up in a unit test, but this
	// confirms the connect-time screen fires for any server on an internal address.
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:9/blocked", http.StatusFound)
	}))
	defer redir.Close()
	if err := doGet(client, redir.URL); !errors.Is(err, netguard.ErrDisallowedAddress) {
		t.Errorf("GET %s (loopback server): err = %v, want netguard.ErrDisallowedAddress", redir.URL, err)
	}

	// A DownloadExternalData call (the container input path) must fail the same way,
	// proving finding #3 is closed: the default client it builds is guarded.
	if _, _, err := DownloadExternalData(context.Background(), "http://169.254.169.254/latest/meta-data/", 1024); !errors.Is(err, netguard.ErrDisallowedAddress) {
		t.Errorf("DownloadExternalData to metadata: err = %v, want netguard.ErrDisallowedAddress", err)
	}
}

func doGet(client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
