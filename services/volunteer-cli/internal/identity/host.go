package identity

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hostIDBytes is the length of a generated host key before hex-encoding. 16 bytes of
// CSPRNG output (128 bits) is more than enough to make per-machine collisions under one
// account negligible.
const hostIDBytes = 16

// GenerateHostID returns a fresh random host key: a stable, opaque per-MACHINE
// identifier the volunteer persists next to its keypair and reports to heads. The
// keypair is the ACCOUNT (run the SAME key on every machine); the host key distinguishes
// THIS machine under it so per-machine facts (advertised runtimes, in-flight cap,
// work-send floor, last-seen, attribution) are tracked per host while credit pools per
// account. It is opaque (no hardware fingerprint) — the head only needs stability, not
// meaning, and an owner spoofing their own host id buys nothing (credit is
// validated-output, quota is measured), so spoof-resistance is unnecessary.
func GenerateHostID() (string, error) {
	b := make([]byte, hostIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating host id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// SaveHostID writes the host key (0644) creating parent dirs as needed.
func SaveHostID(path, hostID string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating directory for host id: %w", err)
	}
	if err := os.WriteFile(path, []byte(hostID), 0644); err != nil {
		return fmt.Errorf("writing host id: %w", err)
	}
	return nil
}

// LoadHostID reads the persisted host key (trimming whitespace).
func LoadHostID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading host id: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// HostIDExists reports whether a non-empty host-id file is present.
func HostIDExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Size() > 0
}

// LoadOrCreateHostID returns the persisted host key, generating and persisting a fresh
// one on first use (or if the file is missing/empty). This lets an EXISTING install that
// predates the host split acquire a stable host id transparently on next start — no
// re-init required — while a fresh init writes it up front.
func LoadOrCreateHostID(path string) (string, error) {
	if HostIDExists(path) {
		if existing, err := LoadHostID(path); err == nil && existing != "" {
			return existing, nil
		}
		// File present but unreadable or empty-after-trim (e.g. truncated/whitespace):
		// fall through and write a fresh id.
	}
	hostID, err := GenerateHostID()
	if err != nil {
		return "", err
	}
	if err := SaveHostID(path, hostID); err != nil {
		return "", err
	}
	return hostID, nil
}
