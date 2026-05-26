package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// updateURL can be overridden via ldflags for custom release servers.
var updateURL = "https://api.github.com/repos/jring-o/lettuce-compute/releases/latest"

// githubRelease is the subset of the GitHub releases API response we use.
type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

// githubAsset is a single asset attached to a GitHub release.
type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func newUpdateCmd() *cobra.Command {
	var checkOnly bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update lettuce-volunteer to the latest version",
		Long:  "Checks for a newer version and downloads the binary, verifying its SHA-256 checksum before replacing the current binary.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(cmd, checkOnly)
		},
	}

	cmd.Flags().BoolVar(&checkOnly, "check", false, "only check for updates, do not download")

	return cmd
}

func runUpdate(cmd *cobra.Command, checkOnly bool) error {
	currentVersion := cmd.Root().Version

	// Fetch latest release metadata.
	release, err := fetchLatestRelease()
	if err != nil {
		return fmt.Errorf("checking for updates: %w", err)
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	currentClean := strings.TrimPrefix(currentVersion, "v")

	if !isNewer(currentClean, latestVersion) {
		fmt.Printf("Already up to date (current: %s, latest: %s)\n", currentVersion, release.TagName)
		return nil
	}

	fmt.Printf("Update available: %s → %s\n", currentVersion, release.TagName)

	if checkOnly {
		return nil
	}

	// Find the binary and checksum assets for this platform.
	binaryName := fmt.Sprintf("lettuce-volunteer-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	checksumName := binaryName + ".sha256"

	var binaryURL, checksumURL string
	for _, a := range release.Assets {
		if a.Name == binaryName {
			binaryURL = a.BrowserDownloadURL
		}
		if a.Name == checksumName {
			checksumURL = a.BrowserDownloadURL
		}
	}

	if binaryURL == "" {
		return fmt.Errorf("no binary found for %s/%s in release %s", runtime.GOOS, runtime.GOARCH, release.TagName)
	}

	// Download binary.
	fmt.Printf("Downloading %s...\n", binaryName)
	binaryData, err := downloadFile(binaryURL)
	if err != nil {
		return fmt.Errorf("downloading binary: %w", err)
	}

	// Verify checksum (mandatory).
	if checksumURL == "" {
		return fmt.Errorf("no checksum file found for %s in release %s — refusing to install unverified binary", binaryName, release.TagName)
	}
	checksumData, dlErr := downloadFile(checksumURL)
	if dlErr != nil {
		return fmt.Errorf("downloading checksum: %w", dlErr)
	}
	if err := verifyChecksum(binaryData, checksumData); err != nil {
		return fmt.Errorf("checksum verification failed: %w", err)
	}
	fmt.Println("Checksum verified.")

	// Replace the current binary.
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding current executable: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	if err := replaceBinary(execPath, binaryData); err != nil {
		return fmt.Errorf("replacing binary: %w", err)
	}

	fmt.Printf("Updated %s → %s\n", currentVersion, release.TagName)
	fmt.Println("Restart the daemon if it is running.")
	return nil
}

// fetchLatestRelease calls the GitHub releases API and returns the latest release.
func fetchLatestRelease() (*githubRelease, error) {
	req, err := http.NewRequest("GET", updateURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "lettuce-volunteer")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	return &release, nil
}

// maxDownloadSize is the maximum size for downloaded files (100 MB).
const maxDownloadSize = 100 * 1024 * 1024

// downloadFile downloads a URL and returns the full body as bytes.
func downloadFile(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned %d", resp.StatusCode)
	}

	return io.ReadAll(io.LimitReader(resp.Body, maxDownloadSize))
}

// verifyChecksum compares the SHA-256 of data against a checksum file.
// The checksum file is expected to contain "<hex_hash>  <filename>" or just "<hex_hash>".
func verifyChecksum(data, checksumFile []byte) error {
	actual := sha256.Sum256(data)
	actualHex := hex.EncodeToString(actual[:])

	checksumStr := strings.TrimSpace(string(checksumFile))
	// Handle both "<hash>  <filename>" and bare "<hash>" formats.
	fields := strings.Fields(checksumStr)
	if len(fields) == 0 {
		return fmt.Errorf("checksum file is empty")
	}
	expectedHex := fields[0]

	if actualHex != expectedHex {
		return fmt.Errorf("expected %s, got %s", expectedHex, actualHex)
	}
	return nil
}

// replaceBinary writes newBinary to the path of the current executable.
// On Windows, renames the old binary to .old first since the running binary can't be overwritten.
func replaceBinary(execPath string, newBinary []byte) error {
	dir := filepath.Dir(execPath)

	// Write new binary to a temp file in the same directory (same filesystem for atomic rename).
	tmpFile, err := os.CreateTemp(dir, "lettuce-volunteer-update-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(newBinary); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp file: %w", err)
	}
	tmpFile.Close()

	// Make executable.
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("setting permissions: %w", err)
	}

	if runtime.GOOS == "windows" {
		// Windows can't replace a running binary; rename old one first.
		oldPath := execPath + ".old"
		os.Remove(oldPath) // Clean up from any previous update.
		if err := os.Rename(execPath, oldPath); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("renaming old binary: %w", err)
		}
	}

	// Atomic rename (or as close as the OS allows).
	if err := os.Rename(tmpPath, execPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming new binary into place: %w", err)
	}

	return nil
}

// isNewer returns true if latest is newer than current using simple semver comparison.
// Handles versions like "0.4.0-dev", "0.6.0", "1.0.0".
func isNewer(current, latest string) bool {
	// Strip pre-release suffixes for comparison.
	cParts := parseSemver(current)
	lParts := parseSemver(latest)

	for i := 0; i < 3; i++ {
		if lParts[i] > cParts[i] {
			return true
		}
		if lParts[i] < cParts[i] {
			return false
		}
	}

	// Same version numbers. If current has a pre-release suffix and latest doesn't,
	// latest is newer (e.g., "0.6.0-dev" < "0.6.0").
	cHasPreRelease := strings.Contains(current, "-")
	lHasPreRelease := strings.Contains(latest, "-")
	if cHasPreRelease && !lHasPreRelease {
		return true
	}

	return false
}

// parseSemver extracts the major.minor.patch numbers from a version string.
func parseSemver(v string) [3]int {
	// Strip everything after a hyphen (pre-release suffix).
	if idx := strings.Index(v, "-"); idx != -1 {
		v = v[:idx]
	}

	var parts [3]int
	fields := strings.Split(v, ".")
	for i := 0; i < len(fields) && i < 3; i++ {
		fmt.Sscanf(fields[i], "%d", &parts[i])
	}
	return parts
}
