//go:build windows

package runtime

import "strings"

// nativeEnvWindowsAllowlist are the Windows host environment variables an opted-in
// native leaf may inherit. A process launched WITHOUT SystemRoot/PATH fails DLL
// resolution and won't start, so these platform vars are load-bearing (design
// finding #7); PATHEXT/ComSpec are needed to resolve and launch executables.
// Everything else in os.Environ() is dropped so the child never sees the
// volunteer's secrets (BG-12).
var nativeEnvWindowsAllowlist = []string{
	"SystemRoot", "windir", "SystemDrive", "PATH", "PATHEXT",
	"TEMP", "TMP", "ComSpec", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
}

// nativeEnvAllowed reports whether a host environment variable may be inherited by
// an opted-in native leaf on Windows. Matching is case-insensitive because Windows
// environment variable names are (PATH vs Path).
func nativeEnvAllowed(key string) bool {
	for _, allowed := range nativeEnvWindowsAllowlist {
		if strings.EqualFold(key, allowed) {
			return true
		}
	}
	return false
}
