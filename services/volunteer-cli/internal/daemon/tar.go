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

		// BG-15c: refuse any non-regular, non-directory entry EXPLICITLY. The
		// checkpoint dir is leaf-controlled; without this, a planted symlink was only
		// INCIDENTALLY safe — FileInfoHeader gives a symlink a Size:0 header, then the
		// os.Open below follows it and io.Copy of the (larger) target aborts the whole
		// archive with tar "write too long". Skipping it here means a symlink/device/
		// socket is never followed and never archived into the head-uploaded blob, and
		// a stray symlink no longer torpedoes an otherwise-valid checkpoint.
		// (filepath.Walk lstat's, so fi reports the link itself, and Walk never
		// descends through it.) A checkpoint HARDLINK would tar as a regular file, but
		// the confined runtimes cannot name a target outside their mount/WASI scope.
		if !fi.IsDir() && !fi.Mode().IsRegular() {
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
//
// Restored entries are normalized SANDBOX-WRITABLE — dirs 0o777, files 0o666 —
// the same contract makeSandboxWritable applies to the fresh bind dirs
// (PB-23). Checkpoint restore is how a unit reassigned ACROSS volunteers gets
// its state, and a hardened CPU container runs as nobody (65534:65534): files
// restored volunteer-owned 0644/0755 could not be overwritten in place, so the
// resumed leaf died with EACCES on its first checkpoint write (PB-29).
// World-writable is deliberate and contained exactly as in PB-23: every
// ancestor under the volunteer's 0o700 data dir keeps other host users out,
// the dirs are per-unit, and chown-to-sandbox-uid is not portable (rootless
// engines remap uids; Windows/macOS binds cross a VM share). Native-runtime
// checkpoints get the same modes harmlessly (the volunteer's own process
// writes them).
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
			if mkErr := os.MkdirAll(target, 0o777); mkErr != nil {
				return fmt.Errorf("creating dir %s: %w", target, mkErr)
			}
			// Explicit chmod after MkdirAll so the process umask cannot narrow the
			// sandbox-writable mode (same pattern as makeSandboxWritable).
			if chErr := os.Chmod(target, 0o777); chErr != nil {
				return fmt.Errorf("making dir %s sandbox-writable: %w", target, chErr)
			}
		case tar.TypeReg:
			parent := filepath.Dir(target)
			if mkErr := os.MkdirAll(parent, 0o777); mkErr != nil {
				return fmt.Errorf("creating parent dir for %s: %w", target, mkErr)
			}
			if chErr := os.Chmod(parent, 0o777); chErr != nil {
				return fmt.Errorf("making parent dir of %s sandbox-writable: %w", target, chErr)
			}
			f, createErr := os.Create(target)
			if createErr != nil {
				return fmt.Errorf("creating file %s: %w", target, createErr)
			}
			written = append(written, target)
			if chErr := f.Chmod(0o666); chErr != nil {
				f.Close()
				return fmt.Errorf("making file %s sandbox-writable: %w", target, chErr)
			}
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
