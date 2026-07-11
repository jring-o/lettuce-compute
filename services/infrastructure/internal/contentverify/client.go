// Package contentverify implements external-output fetch-and-verify (design doc §10;
// BG-02b): a result submitted as an external reference (output_data_url) is held out
// of validation while the head fetches the URL and hashes the served bytes itself.
// The head-computed hash is the ONLY checksum a ref result may ever vote on; the
// volunteer's claimed checksum is recorded but never compared (a claim/served
// divergence is a mangling origin, not provable fraud — §10.7).
//
// This file is the guarded fetch client: the head's second outbound HTTP client and
// the first that fetches arbitrary volunteer-chosen URLs, so its posture is strict —
// no proxy (an environment proxy would make the dial guard screen the proxy's IP
// while the proxy fetches the attacker's URL from inside the network), the shared
// netguard dial screen on every connection (post-DNS, per-connection — DNS rebinding
// is screened at the socket), no redirects at all (allowlist/scheme policy can never
// be escaped via a hop), no content-encoding (the hash covers the served bytes), and
// stock TLS verification with no insecure escape hatch (tests inject an
// httptest-TLS client instead — the atproto NewClient precedent).
package contentverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/netguard"
)

// Machine reason codes stamped into results.content_fetch_last_error. The code is
// the stable prefix; free-form detail follows after ": ".
const (
	// CodeRedirectRefused: the origin answered with a redirect; the URL contract is
	// a direct link and redirects are refused outright (permanent).
	CodeRedirectRefused = "REDIRECT_REFUSED"
	// CodeHTTPStatus: a non-200 response. 5xx is transient (the origin may recover);
	// anything else non-200 is permanent.
	CodeHTTPStatus = "HTTP_STATUS"
	// CodeSizeExceeded: the body exceeded the effective byte cap (permanent).
	CodeSizeExceeded = "SIZE_EXCEEDED"
	// CodeDisallowedAddress: the dial guard refused the resolved address (permanent).
	CodeDisallowedAddress = "DISALLOWED_ADDRESS"
	// CodeNetworkError: DNS/dial/TLS/timeout/EOF failures (transient).
	CodeNetworkError = "NETWORK_ERROR"

	// Worker-side terminal codes (dispositions that never reach the client).
	// CodeHoldingExpired: the row sat unresolved past the holding lifetime.
	CodeHoldingExpired = "HOLDING_EXPIRED"
	// CodeFetchDisabled: the row expired while the content-fetch knob was off.
	CodeFetchDisabled = "FETCH_DISABLED"
	// CodeLeafOptedOut: the fetch-time re-check found the leaf no longer opted in.
	CodeLeafOptedOut = "LEAF_OPTED_OUT"
	// CodeURLDisallowed: the fetch-time re-check refused the URL against the
	// CURRENT leaf config (scheme/userinfo/port/allowlist).
	CodeURLDisallowed = "URL_DISALLOWED"
	// CodeFetchFailed: the transient-failure budget was exhausted.
	CodeFetchFailed = "FETCH_FAILED"
	// CodeUnitFinalized: the unit reached VALIDATED/FAILED while the row was held —
	// the seat is gone (the late-result mirror of submit's refusal).
	CodeUnitFinalized = "UNIT_FINALIZED"
)

// fetchTimeout bounds one fetch end to end (constant in v1; sized for the 100 MB
// default cap on slow origins). The deadline is PER ROW, never a shared batch
// budget — one slow origin cannot starve the batch (§10.6 fetches concurrently).
const fetchTimeout = 120 * time.Second

// userAgent identifies the head's verification fetches to origin operators.
const userAgent = "lettuce-head-content-verification"

// errRedirectRefused is the CheckRedirect sentinel (matched through the *url.Error
// wrapping http.Client applies).
var errRedirectRefused = errors.New("contentverify: redirect refused")

// FetchError is the classified failure of one fetch attempt. Code is the machine
// reason code above; Transient tells the worker whether to retry with backoff
// (attempts counts transient failures ONLY — §10.6) or terminate the row.
type FetchError struct {
	Code      string
	Transient bool
	Err       error
}

func (e *FetchError) Error() string {
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *FetchError) Unwrap() error { return e.Err }

// NewHTTPClient returns the production guarded fetch client. The worker takes an
// injected *http.Client (tests inject an httptest-TLS client — the ONLY test seam);
// the production constructor in main.go always uses this one.
func NewHTTPClient() *http.Client {
	return &http.Client{
		// Overall ceiling; the per-fetch context timeout is the operative deadline.
		Timeout: fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Refuse ALL redirects (S1): a cap of 0 is the safest cap. The URL
			// contract is a direct link, and refusing outright means allowlist and
			// scheme policy can never be escaped via a hop — the dial guard is
			// belt-and-suspenders rather than the only line.
			return errRedirectRefused
		},
		Transport: &http.Transport{
			// Explicit nil — NEVER ProxyFromEnvironment (see the package comment).
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
				Control:   netguard.DialControl,
			}).DialContext,
			// The hash covers the SERVED bytes; the URL contract is "serves the
			// exact output bytes, no content-encoding".
			DisableCompression:    true,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
		},
	}
}

// Fetch GETs rawURL through client and returns the lowercase-hex sha256 of the served
// bytes. The body is streamed through the hasher via an io.LimitReader set one byte
// past maxBytes (the ed25519 cap+1 overflow-detect idiom): O(1) memory, bytes never
// buffered or persisted. maxBytes is the effective cap — min(leaf cap, global knob) —
// composed by the caller. Every failure is a *FetchError classified per §10.5.
func Fetch(ctx context.Context, client *http.Client, rawURL string, maxBytes int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		// The URL was validated before we got here; a parse failure now is permanent.
		return "", &FetchError{Code: CodeURLDisallowed, Transient: false, Err: err}
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, errRedirectRefused):
			return "", &FetchError{Code: CodeRedirectRefused, Transient: false, Err: err}
		case errors.Is(err, netguard.ErrDisallowedAddress):
			return "", &FetchError{Code: CodeDisallowedAddress, Transient: false, Err: err}
		default:
			// DNS, dial, TLS, timeout: the origin may recover — retry with backoff.
			return "", &FetchError{Code: CodeNetworkError, Transient: true, Err: err}
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", &FetchError{
			Code: CodeHTTPStatus,
			// 5xx may recover; any other non-200 (4xx, stray 2xx/3xx) is the
			// origin's answer — permanent.
			Transient: resp.StatusCode >= 500,
			Err:       fmt.Errorf("origin answered %s", resp.Status),
		}
	}

	hasher := sha256.New()
	n, err := io.Copy(hasher, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		// Mid-body network failures (including EOF truncation) are transient.
		return "", &FetchError{Code: CodeNetworkError, Transient: true, Err: err}
	}
	if n > maxBytes {
		return "", &FetchError{
			Code:      CodeSizeExceeded,
			Transient: false,
			Err:       fmt.Errorf("body exceeds the %d-byte cap", maxBytes),
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
