package contentverify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNewHTTPClientPosture is the §10.11 (vi) client-posture pin: it asserts the SSRF
// posture on the REAL production constructor so a later refactor cannot silently
// regress it — no env proxy (which would make the dial guard screen the proxy's IP
// instead of the destination), compression disabled (the hash covers the served
// bytes), and a redirect-refusing CheckRedirect (allowlist/scheme policy can never be
// escaped via a hop).
func TestNewHTTPClientPosture(t *testing.T) {
	client := NewHTTPClient()

	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", client.Transport)
	}
	if tr.Proxy != nil {
		t.Error("Transport.Proxy must be nil (an env proxy would let the dial guard screen the proxy IP, not the destination)")
	}
	if !tr.DisableCompression {
		t.Error("Transport.DisableCompression must be true (the hash covers the served bytes)")
	}
	if client.CheckRedirect == nil {
		t.Fatal("CheckRedirect must be set (redirects are refused outright)")
	}
	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	if err := client.CheckRedirect(req, nil); err == nil {
		t.Error("CheckRedirect must return an error for any redirect")
	}
}

// serveTLS starts an httptest TLS server with the given handler. Its client (returned
// by srv.Client) trusts the test cert AND dials loopback — the guarded production
// client refuses loopback (see TestFetchGuardedClientRefusesLoopback), so behavior
// tests inject srv.Client instead, the atproto NewClient test-seam precedent.
func serveTLS(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchSuccess(t *testing.T) {
	body := []byte("hello external output bytes\n{\"result\":42}")
	srv := serveTLS(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})

	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	got, err := Fetch(context.Background(), srv.Client(), srv.URL, 1<<20)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != want {
		t.Errorf("hash = %s, want %s", got, want)
	}
}

func TestFetchRedirectRefused(t *testing.T) {
	srv := serveTLS(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/elsewhere", http.StatusFound)
	})
	// srv.Client() follows redirects by default; attach the production redirect refusal
	// so this exercises Fetch's classification of that refusal.
	client := srv.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return errRedirectRefused }

	_, err := Fetch(context.Background(), client, srv.URL, 1<<20)
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("error = %v, want *FetchError", err)
	}
	if fe.Code != CodeRedirectRefused {
		t.Errorf("code = %s, want %s", fe.Code, CodeRedirectRefused)
	}
	if fe.Transient {
		t.Error("a refused redirect must be permanent, not transient")
	}
}

func TestFetchHTTPStatusPermanent(t *testing.T) {
	srv := serveTLS(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	_, err := Fetch(context.Background(), srv.Client(), srv.URL, 1<<20)
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("error = %v, want *FetchError", err)
	}
	if fe.Code != CodeHTTPStatus {
		t.Errorf("code = %s, want %s", fe.Code, CodeHTTPStatus)
	}
	if fe.Transient {
		t.Error("a 404 is the origin's answer — permanent, not transient")
	}
}

func TestFetchHTTPStatusTransient(t *testing.T) {
	srv := serveTLS(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	_, err := Fetch(context.Background(), srv.Client(), srv.URL, 1<<20)
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("error = %v, want *FetchError", err)
	}
	if fe.Code != CodeHTTPStatus {
		t.Errorf("code = %s, want %s", fe.Code, CodeHTTPStatus)
	}
	if !fe.Transient {
		t.Error("a 503 may recover — transient")
	}
}

func TestFetchSizeExceeded(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 100)
	srv := serveTLS(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})

	got, err := Fetch(context.Background(), srv.Client(), srv.URL, 10)
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("error = %v, want *FetchError", err)
	}
	if fe.Code != CodeSizeExceeded {
		t.Errorf("code = %s, want %s", fe.Code, CodeSizeExceeded)
	}
	if fe.Transient {
		t.Error("an over-cap body is permanent")
	}
	if got != "" {
		t.Errorf("no hash may be returned on size overflow, got %q", got)
	}
}

// TestFetchGuardedClientRefusesLoopback proves the netguard wiring: the REAL guarded
// client refuses the loopback address an httptest server listens on at DIAL time
// (before TLS), so a volunteer URL that resolves to an internal address is refused.
func TestFetchGuardedClientRefusesLoopback(t *testing.T) {
	srv := serveTLS(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("unreachable"))
	})

	_, err := Fetch(context.Background(), NewHTTPClient(), srv.URL, 1<<20)
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("error = %v, want *FetchError", err)
	}
	if fe.Code != CodeDisallowedAddress {
		t.Errorf("code = %s, want %s", fe.Code, CodeDisallowedAddress)
	}
	if fe.Transient {
		t.Error("a disallowed address is permanent")
	}
}
