package runtime

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// vizCacheDir returns the cache directory for viz bundles: {dataDir}/cache/viz/
func vizCacheDir(dataDir string) string {
	return filepath.Join(dataDir, "cache", "viz")
}

// vizBundleDir is the directory name within a work directory for extracted viz bundles.
const vizBundleDir = ".lettuce-viz"

// EnsureVizBundle downloads a viz bundle tarball if not cached, returning the cached path.
//
// SECURITY (C2): when expectedChecksum is set (lowercase hex SHA-256) the cache is
// keyed by that content digest (a cache hit can only be matching bytes) and a
// fresh download is verified before use, rejecting on mismatch. When absent, the
// bundle is keyed by URL and proceeds unverified — acceptable because viz bundles
// are static assets served in a sandboxed browser context — but a warning is
// logged. Follows the same pattern as ensureBinary/ensureModule.
func EnsureVizBundle(ctx context.Context, dataDir string, vizURL string, expectedChecksum string, httpClient *http.Client, logger *slog.Logger) (string, error) {
	cacheDir := vizCacheDir(dataDir)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create viz cache dir: %w", err)
	}

	var cacheKey string
	if expectedChecksum != "" {
		cacheKey = expectedChecksum + ".tar.gz"
	} else {
		h := sha256.Sum256([]byte(vizURL))
		cacheKey = hex.EncodeToString(h[:]) + ".tar.gz"
		logger.Warn("viz bundle has no checksum in execution spec; proceeding unverified (sandboxed)", "url", vizURL)
	}
	cachePath := filepath.Join(cacheDir, cacheKey)

	// Check cache.
	if _, err := os.Stat(cachePath); err == nil {
		logger.Debug("viz bundle cache hit", "url", vizURL, "path", cachePath)
		return cachePath, nil
	}

	logger.Info("downloading viz bundle", "url", vizURL, "expected_sha256", expectedChecksum)

	// Download to a temp file and verify BEFORE committing to the cache path so a
	// concurrent prepare for the same checksum cannot observe unverified bytes at the
	// cache path (matches the native runtime's verify-then-commit pattern).
	tmp, err := os.CreateTemp(cacheDir, ".viz-stage-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath) // no-op once renamed away

	if err := downloadVizBundle(ctx, httpClient, vizURL, tmpPath); err != nil {
		return "", err
	}

	// Verify SHA-256 against the expected digest when present.
	if expectedChecksum != "" {
		actual, err := fileChecksumSHA256(tmpPath)
		if err != nil {
			return "", fmt.Errorf("checksum viz bundle: %w", err)
		}
		if actual != expectedChecksum {
			logger.Warn("viz bundle checksum mismatch, rejecting", "url", vizURL,
				"expected_sha256", expectedChecksum, "actual_sha256", actual)
			return "", fmt.Errorf("viz bundle checksum mismatch: expected %s, got %s", expectedChecksum, actual)
		}
	}

	// Commit the verified bytes to the cache path.
	if err := os.Rename(tmpPath, cachePath); err != nil {
		return "", fmt.Errorf("rename to cache: %w", err)
	}

	return cachePath, nil
}

// downloadVizBundle downloads a URL to the given path using atomic write.
func downloadVizBundle(ctx context.Context, httpClient *http.Client, url, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download viz bundle: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download viz bundle returned status %d", resp.StatusCode)
	}

	dir := filepath.Dir(destPath)
	tmp, err := os.CreateTemp(dir, ".viz-download-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	const maxDownloadSize = 500 * 1024 * 1024 // 500 MB
	if _, err := io.Copy(tmp, io.LimitReader(resp.Body, maxDownloadSize)); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write viz bundle: %w", err)
	}
	tmp.Close()

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename to cache: %w", err)
	}

	return nil
}

// F2: extraction-time decompression caps. The viz tarball is fetched as
// COMPRESSED bytes (capped by maxDownloadSize); these constants bound the
// DECOMPRESSED side, so a small gzip bomb cannot expand into multi-GB on disk
// or in memory. Values mirror the dashboard route for consistency:
//   - maxVizExtractedTotal: sum of decompressed payload bytes across the
//     whole tarball (matches the 500 MB compressed-download cap).
//   - maxVizExtractedFile: per-entry payload cap, so a single oversized
//     entry is rejected even when it would fit under the total cap.
const (
	maxVizExtractedFile  = 100 * 1024 * 1024 // 100 MB per entry
	maxVizExtractedTotal = 500 * 1024 * 1024 // 500 MB across the bundle
)

// ExtractVizBundle extracts a viz tarball to {workDir}/.lettuce-viz/ and validates
// that index.html exists at the root. Returns the extracted directory path.
//
// SECURITY (F2): decompressed bytes are bounded at two levels — per-entry
// (maxVizExtractedFile) and per-bundle (maxVizExtractedTotal). On a cap
// breach the partially-extracted destDir is removed so the caller cannot
// observe (or later run from) a torn extraction.
func ExtractVizBundle(tarballPath string, workDir string) (string, error) {
	destDir := filepath.Join(workDir, vizBundleDir)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("create viz bundle dir: %w", err)
	}

	f, err := os.Open(tarballPath)
	if err != nil {
		return "", fmt.Errorf("open viz tarball: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	// F2: cleanup helper — on any extraction failure (cap breach, traversal,
	// I/O error) wipe the partial destDir so a caller cannot accidentally use
	// half-extracted bytes. Best-effort: a removal failure is not surfaced.
	var (
		ok           bool
		totalWritten int64
	)
	defer func() {
		if !ok {
			_ = os.RemoveAll(destDir)
		}
	}()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tarball: %w", err)
		}

		// Clean the path: normalize separators and remove leading ./
		name := filepath.Clean(header.Name)
		name = filepath.ToSlash(name)

		// Skip the bare root directory entry (e.g., "." or "").
		if name == "." || name == "" {
			continue
		}

		relPath := name

		// Security: reject path traversal.
		if strings.Contains(relPath, "..") {
			return "", fmt.Errorf("path traversal in tarball: %s", header.Name)
		}

		target := filepath.Join(destDir, filepath.FromSlash(relPath))

		// Verify the target is within destDir.
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return "", fmt.Errorf("resolve path: %w", err)
		}
		absDest, err := filepath.Abs(destDir)
		if err != nil {
			return "", fmt.Errorf("resolve dest: %w", err)
		}
		if !strings.HasPrefix(absTarget, absDest+string(filepath.Separator)) && absTarget != absDest {
			return "", fmt.Errorf("path escape in tarball: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", fmt.Errorf("create dir: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", fmt.Errorf("create parent dir: %w", err)
			}
			out, err := os.Create(target)
			if err != nil {
				return "", fmt.Errorf("create file: %w", err)
			}
			// F2: bound per-entry AND remaining-total decompressed bytes.
			// Read at most (smaller of perEntryCap, remainingTotalCap)+1 so
			// we can detect overflow by observing n > that smaller cap
			// (rather than silently truncating, which io.LimitReader alone
			// would do).
			remainingTotal := int64(maxVizExtractedTotal) - totalWritten
			limit := int64(maxVizExtractedFile)
			if remainingTotal < limit {
				limit = remainingTotal
			}
			n, copyErr := io.Copy(out, io.LimitReader(tr, limit+1))
			out.Close()
			if copyErr != nil {
				return "", fmt.Errorf("extract file: %w", copyErr)
			}
			if n > int64(maxVizExtractedFile) {
				return "", fmt.Errorf("viz bundle entry %q exceeds per-entry %d byte limit", header.Name, maxVizExtractedFile)
			}
			totalWritten += n
			if totalWritten > int64(maxVizExtractedTotal) {
				return "", fmt.Errorf("viz bundle exceeds %d byte total extraction limit", maxVizExtractedTotal)
			}
		}
	}

	// Resolve the bundle root: index.html must live at the extraction root, OR —
	// when the bundle wraps everything in a single top-level directory (the shape
	// assemble.sh produces and the dashboard's viz bundle route already strips) —
	// inside that one wrapper directory. Returning the wrapper as the root keeps
	// the extractor tolerant of both layouts without weakening the traversal/size
	// caps enforced above. See TODO #39.
	root, err := resolveVizRoot(destDir)
	if err != nil {
		return "", err
	}

	ok = true
	return root, nil
}

// resolveVizRoot returns the directory that contains index.html: destDir itself,
// or the single top-level subdirectory when the bundle is wrapped in exactly one.
// Returns an error (preserving the "missing required index.html" message) if no
// index.html can be found at either level.
func resolveVizRoot(destDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(destDir, "index.html")); err == nil {
		return destDir, nil
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		return "", fmt.Errorf("read viz bundle dir: %w", err)
	}
	var subDirs []string
	for _, e := range entries {
		if e.IsDir() {
			subDirs = append(subDirs, e.Name())
		}
	}
	if len(subDirs) == 1 {
		nested := filepath.Join(destDir, subDirs[0])
		if _, err := os.Stat(filepath.Join(nested, "index.html")); err == nil {
			return nested, nil
		}
	}
	return "", fmt.Errorf("viz bundle missing required index.html")
}

// PrepareVizBundle handles the viz bundle download and extraction for any runtime.
// If ExecutionSpec.Binaries["viz"] is set, downloads the tarball, extracts it to
// {workDir}/.lettuce-viz/, and returns the path. Returns empty string if no viz bundle.
func PrepareVizBundle(ctx context.Context, dataDir string, workDir string, spec *ExecutionSpec, httpClient *http.Client, logger *slog.Logger) (string, error) {
	if spec == nil || len(spec.Binaries) == 0 {
		logger.Debug("PrepareVizBundle: no spec or no binaries")
		return "", nil
	}
	vizURL, ok := spec.Binaries["viz"]
	if !ok || vizURL == "" {
		logger.Debug("PrepareVizBundle: no viz key in binaries")
		return "", nil
	}

	expectedChecksum := strings.ToLower(spec.BinaryChecksums["viz"])

	logger.Info("PrepareVizBundle: downloading", "url", vizURL)
	tarballPath, err := EnsureVizBundle(ctx, dataDir, vizURL, expectedChecksum, httpClient, logger)
	if err != nil {
		logger.Warn("PrepareVizBundle: download FAILED", "url", vizURL, "error", err)
		return "", fmt.Errorf("ensure viz bundle: %w", err)
	}
	logger.Debug("PrepareVizBundle: tarball ready", "path", tarballPath)

	logger.Debug("PrepareVizBundle: extracting", "tarball", tarballPath, "work_dir", workDir)
	vizPath, err := ExtractVizBundle(tarballPath, workDir)
	if err != nil {
		logger.Warn("PrepareVizBundle: extract FAILED", "tarball", tarballPath, "error", err)
		return "", fmt.Errorf("extract viz bundle: %w", err)
	}
	logger.Info("PrepareVizBundle: complete", "path", vizPath)

	return vizPath, nil
}
