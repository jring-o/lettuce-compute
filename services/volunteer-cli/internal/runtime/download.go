package runtime

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

	"github.com/lettuce-compute/infrastructure/netguard"
)

// DefaultMaxDownloadBytes is the default limit for external data downloads (100 MB).
const DefaultMaxDownloadBytes int64 = 100 * 1024 * 1024

// DefaultMaxArtifactBytes bounds a native binary / wasm module download (BG-16d):
// generous for a real compute artifact, but finite so a malicious URL cannot stream
// unboundedly and fill the volunteer's disk during the download itself.
const DefaultMaxArtifactBytes int64 = 2 * 1024 * 1024 * 1024 // 2 GiB

// copyCapped copies src to dst but fails if more than maxBytes would be written,
// reading one byte past the cap to detect the overflow. Returns the byte count.
func copyCapped(dst io.Writer, src io.Reader, maxBytes int64) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(src, maxBytes+1))
	if err != nil {
		return n, err
	}
	if n > maxBytes {
		return n, fmt.Errorf("download exceeds maximum size of %d bytes", maxBytes)
	}
	return n, nil
}

// DefaultDownloadTimeout is the default timeout for external data downloads.
const DefaultDownloadTimeout = 5 * time.Minute

// maxRedirects is the maximum number of HTTP redirects to follow.
const maxRedirects = 5

// NewGuardedHTTPClient returns the production outbound fetch client for every
// leaf-influenced URL the daemon downloads — the leaf binary, the wasm module, the
// input-data reference, and the viz bundle (BG-14). It installs the shared netguard
// dial screen as a net.Dialer.Control hook, so EVERY connection attempt is screened
// against the concrete post-DNS IP: loopback, link-local/metadata (169.254/16),
// private, CGNAT (100.64/10), NAT64, and unspecified ranges are all refused with
// netguard.ErrDisallowedAddress. Because Control runs on each hop, a redirect to an
// internal address and DNS rebinding are both defeated by the same mechanism.
//
// Proxy is nil on purpose — never ProxyFromEnvironment: an env proxy would make the
// guard screen the proxy's IP instead of the destination. Redirects are followed but
// bounded, so a legitimate storage 302 (presigned URL -> CDN) still works while every
// hop is screened.
func NewGuardedHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       DefaultDownloadTimeout,
		CheckRedirect: boundedRedirect,
		Transport: &http.Transport{
			Proxy: nil, // NEVER ProxyFromEnvironment — a proxy bypasses the dial guard
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
				Control:   netguard.DialControl,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// boundedRedirect caps redirect chains; the per-hop dial screen (installed on the
// client's transport) screens every hop's resolved IP regardless of this count.
func boundedRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("too many redirects (max %d)", maxRedirects)
	}
	return nil
}

// DownloadExternalData downloads data from an external URL through the production
// netguard-guarded client, returning the downloaded bytes and their SHA-256 hex
// checksum. This is the container input-data path (container.go), so guarding it
// here is what closes the default-image SSRF surface (BG-14 / design finding #3).
// Follows redirects (bounded) with every hop screened; times out on the context
// deadline or 5 minutes; validates size against maxBytes.
func DownloadExternalData(ctx context.Context, url string, maxBytes int64) ([]byte, string, error) {
	return DownloadExternalDataWithClient(ctx, NewGuardedHTTPClient(), url, maxBytes)
}

// DownloadExternalDataWithClient is DownloadExternalData with an injected client —
// the sole test seam (tests inject a loopback-dialing client; production callers use
// the netguard-guarded client). It enforces its own redirect and size caps
// regardless of the injected client's policy, so injection can never drop those
// bounds.
func DownloadExternalDataWithClient(ctx context.Context, client *http.Client, url string, maxBytes int64) ([]byte, string, error) {
	if url == "" {
		return nil, "", errors.New("empty download URL")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxDownloadBytes
	}
	if client == nil {
		client = NewGuardedHTTPClient()
	}

	// Apply default timeout if context has no deadline.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultDownloadTimeout)
		defer cancel()
	}

	// Copy the client so the redirect cap is enforced here regardless of the
	// injected client's own CheckRedirect; the copy shares the (guarded) transport.
	c := *client
	c.CheckRedirect = boundedRedirect

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}

	resp, err := c.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	// Check Content-Length if available.
	if resp.ContentLength > maxBytes {
		return nil, "", fmt.Errorf("content-length %d exceeds max %d bytes", resp.ContentLength, maxBytes)
	}

	// Read up to maxBytes + 1 to detect oversized responses.
	limitedReader := io.LimitReader(resp.Body, maxBytes+1)
	data, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, "", fmt.Errorf("read response: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("response body exceeds max %d bytes", maxBytes)
	}

	hash := sha256.Sum256(data)
	checksum := hex.EncodeToString(hash[:])

	return data, checksum, nil
}
