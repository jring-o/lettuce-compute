package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultMaxDownloadBytes is the default limit for external data downloads (100 MB).
const DefaultMaxDownloadBytes int64 = 100 * 1024 * 1024

// DefaultDownloadTimeout is the default timeout for external data downloads.
const DefaultDownloadTimeout = 5 * time.Minute

// maxRedirects is the maximum number of HTTP redirects to follow.
const maxRedirects = 5

// DownloadExternalData downloads data from an external URL.
// Returns the downloaded bytes and the SHA-256 hex checksum.
// Follows redirects (up to 5). Times out based on context deadline or 5 minutes.
// Validates response size against maxBytes.
func DownloadExternalData(ctx context.Context, url string, maxBytes int64) ([]byte, string, error) {
	if url == "" {
		return nil, "", errors.New("empty download URL")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxDownloadBytes
	}

	// Apply default timeout if context has no deadline.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultDownloadTimeout)
		defer cancel()
	}

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too many redirects (max %d)", maxRedirects)
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
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
