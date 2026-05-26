package daemon

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// tarDirectory archives all files in dir into a tar blob.
// Returns nil, nil if dir doesn't exist or is empty.
func tarDirectory(dir string) ([]byte, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat checkpoint dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("checkpoint path is not a directory: %s", dir)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	fileCount := 0

	err = filepath.Walk(dir, func(path string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		// Get path relative to the checkpoint dir.
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		// Skip the root directory itself.
		if rel == "." {
			return nil
		}

		header, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return fmt.Errorf("creating tar header for %s: %w", rel, err)
		}
		// Use forward slashes and the relative path.
		header.Name = filepath.ToSlash(rel)

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("writing tar header for %s: %w", rel, err)
		}

		if fi.IsDir() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("opening %s: %w", rel, err)
		}
		defer f.Close()

		if _, err := io.Copy(tw, f); err != nil {
			return fmt.Errorf("copying %s: %w", rel, err)
		}

		fileCount++
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking checkpoint dir: %w", err)
	}

	if fileCount == 0 {
		return nil, nil
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("closing tar writer: %w", err)
	}
	return buf.Bytes(), nil
}

// F2: checkpoint extraction caps. The checkpoint blob arrives over the gRPC
// stream (authenticated head <-> volunteer) but is not yet bounded at
// decompression time. Cap per-entry and per-bundle decompressed bytes so a
// hostile/buggy head cannot push a tarball that explodes into multi-GB on
// disk. Values mirror the viz extractor for consistency:
//   - maxCheckpointEntry: per-file payload cap.
//   - maxCheckpointTotal: sum across all files in the checkpoint.
//
// On a cap breach the partially-extracted dir is wiped so the caller cannot
// resume from a torn checkpoint.
const (
	maxCheckpointEntry = 100 * 1024 * 1024 // 100 MB per file
	maxCheckpointTotal = 500 * 1024 * 1024 // 500 MB across the checkpoint
)

// extractTar extracts a tar blob into dir.
//
// SECURITY (F2): bounds decompressed bytes per-entry and per-bundle. On any
// extraction failure (cap breach, traversal, I/O error) the partial contents
// written under dir are removed best-effort so the caller does not resume
// from a half-extracted checkpoint.
func extractTar(data []byte, dir string) (err error) {
	// F2: track files we created so a cap breach mid-extraction wipes them.
	// We only remove what we wrote (best-effort), leaving any pre-existing
	// contents under dir untouched.
	var written []string
	defer func() {
		if err != nil {
			for _, p := range written {
				_ = os.Remove(p)
			}
		}
	}()

	tr := tar.NewReader(bytes.NewReader(data))
	var totalWritten int64

	for {
		header, hdrErr := tr.Next()
		if hdrErr == io.EOF {
			break
		}
		if hdrErr != nil {
			return fmt.Errorf("reading tar entry: %w", hdrErr)
		}

		// Sanitize: prevent path traversal.
		clean := filepath.FromSlash(header.Name)
		if strings.Contains(clean, "..") {
			return fmt.Errorf("tar entry has path traversal: %s", header.Name)
		}

		target := filepath.Join(dir, clean)

		switch header.Typeflag {
		case tar.TypeDir:
			if mkErr := os.MkdirAll(target, 0755); mkErr != nil {
				return fmt.Errorf("creating dir %s: %w", target, mkErr)
			}
		case tar.TypeReg:
			if mkErr := os.MkdirAll(filepath.Dir(target), 0755); mkErr != nil {
				return fmt.Errorf("creating parent dir for %s: %w", target, mkErr)
			}
			f, createErr := os.Create(target)
			if createErr != nil {
				return fmt.Errorf("creating file %s: %w", target, createErr)
			}
			written = append(written, target)
			// F2: per-entry and remaining-total cap. Read at most limit+1
			// bytes so we can detect overflow by observing n > limit rather
			// than silently truncating (which io.LimitReader alone would do).
			remainingTotal := int64(maxCheckpointTotal) - totalWritten
			limit := int64(maxCheckpointEntry)
			if remainingTotal < limit {
				limit = remainingTotal
			}
			n, copyErr := io.Copy(f, io.LimitReader(tr, limit+1))
			f.Close()
			if copyErr != nil {
				return fmt.Errorf("extracting %s: %w", target, copyErr)
			}
			if n > int64(maxCheckpointEntry) {
				return fmt.Errorf("checkpoint entry %q exceeds per-entry %d byte limit", header.Name, maxCheckpointEntry)
			}
			totalWritten += n
			if totalWritten > int64(maxCheckpointTotal) {
				return fmt.Errorf("checkpoint exceeds %d byte total extraction limit", maxCheckpointTotal)
			}
		}
	}
	return nil
}
