//go:build windows

package attestation

// checkKeyFilePermissions is a no-op on Windows. Windows has ACLs, not POSIX
// mode bits, and there is no file-owner uid to compare against the process
// euid. The production head runs in a Linux container, where the Unix build
// enforces the key's permissions; this Windows no-op only keeps the dev build
// compiling, mirroring the daemon's process_windows.go house pattern.
func checkKeyFilePermissions(path string) error {
	return nil
}
